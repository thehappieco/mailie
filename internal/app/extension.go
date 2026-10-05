package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/service"
)

// Extension adds routes to the daemon's listener. It runs once, at start,
// after the service is built and before anything listens, and mounts its
// handlers on r. An error stops the daemon from starting, as a route that is
// refused does. Its context ends when the daemon stops, whatever stops it:
// anything the extension starts must stop then.
//
// Its routes are mounted beside the core's and before the console's
// catch-all, which they take precedence over. A route is refused when the
// mux refuses its pattern (malformed, or a nil handler), when it duplicates
// or conflicts with a route already mounted, when it would take a request a
// core route answers (a pattern more specific than a core one, such as
// "GET /v1/accounts/mine" beside "GET /v1/accounts/{id}", or "GET /mcp"
// beside "/mcp"), when it claims the root ("/", "/{$}", with or without a
// method), which is the console's, and when it names a host. The mux tries a
// pattern with a host before every pattern without one and never reports the
// two as a conflict, so "example.com/{path...}" would take the whole API and
// MCP for that Host; the core serves every host alike, and so do its
// extensions. The core's routes are judged as the daemon serves them with
// everything switched on (MCP over HTTP and the 404 it answers under /mcp/
// when off, the instance-key routes, /metrics), so whether a route is
// accepted never depends on MAIL_MCP_HTTP, MAIL_ADMIN_API, MAIL_METRICS_ADDR
// or MAIL_WEB_DIR.
//
// An extension's handlers are its own: nothing of the core's middleware (the
// route timeouts, the body limits, the authentication, the metrics) wraps
// them.
type Extension func(ctx context.Context, r *Router, d Deps) error

// Deps is what an extension may use. It holds the service every core route
// goes through, and never the store, the sync engine or the account registry
// behind it: an extension decides nothing the service does not, authorization
// and account ownership included.
type Deps struct {
	// Config is the daemon's configuration, as loaded.
	Config config.Config
	// Service is the use cases and all authorization.
	Service *service.Service
	// Log is the daemon's logger (standard error under MCPStdio).
	Log *slog.Logger
	// Limits meters bearer traffic, the same buckets REST and MCP spend
	// from: a credential has one budget whichever way it comes in.
	Limits *ratelimit.Auth
	// SignInLimits meters sign-in, sign-up and password changes, the same
	// buckets the core's spend from: another way to sign in is not another
	// budget of guesses.
	SignInLimits *ratelimit.Auth
}

// Router collects an extension's routes, in http.ServeMux's patterns. They
// are checked and mounted once every extension has run.
type Router struct{ routes []route }

// route is one pattern and its handler.
type route struct {
	pattern string
	handler http.Handler
}

// Handle adds a route.
func (r *Router) Handle(pattern string, handler http.Handler) {
	r.routes = append(r.routes, route{pattern: pattern, handler: handler})
}

// HandleFunc adds a route served by a function.
func (r *Router) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	var h http.Handler
	if handler != nil {
		h = http.HandlerFunc(handler)
	}
	// A nil function stays a nil handler, which the mux refuses.
	r.Handle(pattern, h)
}

// ErrRoute says an extension's route was refused, and the daemon did not
// start (see Extension).
var ErrRoute = errors.New("app: extension route refused")

// collect runs the extensions, in order, and returns the routes they added.
func collect(ctx context.Context, extensions []Extension, d Deps) ([]route, error) {
	var r Router
	for _, ext := range extensions {
		if err := ext(ctx, &r, d); err != nil {
			return nil, err
		}
	}
	return r.routes, nil
}

// mountExtensions mounts extra on mux, which holds the core's routes and not
// yet the console's catch-all, once each route has been admitted against
// every route the core can serve.
func mountExtensions(mux *http.ServeMux, extra []route) error {
	if len(extra) == 0 {
		return nil
	}
	core := everyCoreRoute()
	admitted := map[string]bool{}
	for _, rt := range extra {
		if err := admit(core, admitted, rt); err != nil {
			return err
		}
		if err := handle(mux, rt); err != nil {
			return err
		}
	}
	return nil
}

// consoleCatchAll is the console's pattern, the one route extensions are
// meant to take requests from.
const consoleCatchAll = "/"

// everyCoreRoute is a mux holding every route the core can serve, whatever
// this deployment switched on, and the console's catch-all, with handlers
// that are never called: what an extension's routes are judged against.
func everyCoreRoute() *http.ServeMux {
	mux := http.NewServeMux()
	never := http.NotFoundHandler()
	// MCP switched off covers /mcp and /mcp/; switched on is /mcp alone.
	coreRoutes(mux, &api.Handler{AdminAPI: true}, nil, never)
	mux.Handle(consoleCatchAll, never)
	return mux
}

// admit registers rt on core, which holds the core's routes and the ones
// already admitted, or says why it may not be mounted.
//
// A pattern with a host is refused outright (see Extension). A duplicate or a
// conflict is the mux's own refusal. A pattern that would take requests from
// a core route is one more specific than it: every request the pattern
// matches, the core route matched before, so one request the pattern matches
// is enough to tell, asked of the mux before and after the pattern is added.
func admit(core *http.ServeMux, admitted map[string]bool, rt route) error {
	_, host, path, parsed := split(rt.pattern)
	switch {
	case parsed && host != "":
		// It outranks every core route for that Host, which the mux never
		// calls a conflict and the sample below would not see.
		return fmt.Errorf("%w: %q: a route may not name a host; the core serves every host alike", ErrRoute, rt.pattern)
	case parsed && (path == "/" || path == "/{$}"):
		return fmt.Errorf("%w: %q: the root is the console's", ErrRoute, rt.pattern)
	}
	probe, probed := sample(rt.pattern)
	before := ""
	if probed {
		_, before = core.Handler(probe)
	}
	if err := handle(core, rt); err != nil {
		return err
	}
	if probed {
		_, after := core.Handler(probe)
		if after == rt.pattern && before != "" && before != consoleCatchAll && !admitted[before] {
			return fmt.Errorf("%w: %q would take requests the core route %q answers", ErrRoute, rt.pattern, before)
		}
	}
	admitted[rt.pattern] = true
	return nil
}

// handle registers rt on mux, and turns the mux's panic over a malformed,
// duplicate or conflicting pattern, or a nil handler, into an error.
func handle(mux *http.ServeMux, rt route) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: %q: %v", ErrRoute, rt.pattern, p)
		}
	}()
	mux.Handle(rt.pattern, rt.handler)
	return nil
}

// split is a pattern's method, host and path, read as http.ServeMux reads
// them. It reports false for what is not a pattern, which the mux refuses on
// its own.
func split(pattern string) (method, host, path string, ok bool) {
	rest := pattern
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		method, rest = pattern[:i], strings.TrimLeft(pattern[i+1:], " \t")
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", "", false
	}
	return method, rest[:i], rest[i:], true
}

// probeSegment stands for a wildcard in a sample request: a value no core
// route spells out, so the request reaches the core route the wildcard
// overlaps rather than a literal one beside it.
const probeSegment = "extension-probe"

// sample is a request the pattern matches: its method (GET when it has none),
// its host, and its path with every wildcard filled in.
func sample(pattern string) (*http.Request, bool) {
	method, host, path, ok := split(pattern)
	if !ok {
		return nil, false
	}
	segments := strings.Split(path[1:], "/")
	for i, s := range segments {
		switch {
		case s == "{$}":
			segments[i] = ""
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segments[i] = probeSegment
		}
	}
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.ParseRequestURI("/" + strings.Join(segments, "/"))
	if err != nil {
		return nil, false
	}
	// Only routed, never served: the mux reads its method, host and path.
	return &http.Request{Method: method, Host: host, URL: u, Header: http.Header{}}, true
}
