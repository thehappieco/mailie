package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
)

// consolePage is the built console the daemons below serve.
const consolePage = `<!doctype html><title>Console</title>`

// localConfig is a daemon's configuration over a fresh data directory, with a
// credential key, a built console, MCP over HTTP, and a free loopback port.
func localConfig(t *testing.T) config.Config {
	t.Helper()
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0x5c
	}
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte(consolePage), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		DataDir:     t.TempDir(),
		Credentials: config.Credentials{ActiveKeyID: 1, Keys: map[uint8][]byte{1: key}},
		HTTPAddr:    addr,
		WebDir:      web,
		MCPHTTP:     true,
	}
}

// lockedBuffer is a log the daemon writes from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runDaemon runs the daemon with opts until the test ends, and returns its
// base URL.
func runDaemon(t *testing.T, cfg config.Config, opts Options) string {
	t.Helper()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, obs.NewLoggerTo(logs, "info", "text"), opts) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	base := "http://" + cfg.HTTPAddr
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		select {
		case err := <-done:
			t.Fatalf("Run returned before answering: %v\n%s", err, logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not answer within 10s:\n%s", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// answer is a response's status, headers and body.
type answer struct {
	status int
	header http.Header
	body   string
}

func fetch(t *testing.T, method, url string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{resp.StatusCode, resp.Header, string(body)}
}

// says answers every request with text.
func says(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, text)
	}
}

func TestAnExtensionRouteIsServedBesideTheCoreRoutesWhichAnswerAsBefore(t *testing.T) {
	cfg := localConfig(t)
	type given struct {
		ctx  context.Context
		deps Deps
	}
	seen := make(chan given, 1)
	extension := func(ctx context.Context, r *Router, d Deps) error {
		seen <- given{ctx, d}
		r.Handle("GET /extension/{name}", says("from the extension"))
		r.HandleFunc("POST /v1/auth/another-way", says("signed in another way"))
		return nil
	}
	base := runDaemon(t, cfg, Options{Version: "test-version", Extensions: []Extension{extension}})

	g := <-seen
	got, hookCtx := g.deps, g.ctx
	if got.Service == nil || got.Log == nil || got.Limits == nil || got.SignInLimits == nil ||
		got.Config.DataDir != cfg.DataDir || got.Config.HTTPAddr != cfg.HTTPAddr {
		t.Errorf("the extension was given %+v", got)
	}
	if hookCtx == nil || hookCtx.Err() != nil {
		t.Errorf("the extension ran without the daemon's live context: %v", hookCtx)
	}

	for path, want := range map[string]string{
		"GET /extension/hello":      "from the extension",
		"POST /v1/auth/another-way": "signed in another way",
	} {
		method, p, _ := strings.Cut(path, " ")
		if a := fetch(t, method, base+p); a.status != http.StatusOK || a.body != want {
			t.Errorf("%s answered %d %q, want the extension's %q", path, a.status, a.body, want)
		}
	}

	// The core answers as it does without the extension: the health probe,
	// MCP asking for a key, the API's 404 for an endpoint that does not
	// exist, and the console for everything else, including a path beside
	// the extension's that it did not claim.
	if a := fetch(t, http.MethodGet, base+"/v1/healthz"); a.status != http.StatusOK ||
		!strings.Contains(a.body, `"status":"ok"`) || !strings.Contains(a.body, `"version":"test-version"`) {
		t.Errorf("GET /v1/healthz answered %d %q", a.status, a.body)
	}
	if a := fetch(t, http.MethodPost, base+"/mcp"); a.status != http.StatusUnauthorized {
		t.Errorf("POST /mcp without a key answered %d %q, want the MCP endpoint's 401", a.status, a.body)
	}
	for _, p := range []string{"/v1/no-such-endpoint", "/v1/auth/no-such-way", "/mcp/extension"} {
		if a := fetch(t, http.MethodGet, base+p); a.status != http.StatusNotFound || a.body != apiNotFound {
			t.Errorf("GET %s answered %d %q, want the API's 404", p, a.status, a.body)
		}
	}
	for _, p := range []string{"/", "/oauth/return", "/extension/hello/more"} {
		if a := fetch(t, http.MethodGet, base+p); a.status != http.StatusOK || a.body != consolePage {
			t.Errorf("GET %s answered %d %q, want the console", p, a.status, a.body)
		}
	}
}

// coreRequests are requests every core route answers one of, the console's
// catch-all included.
var coreRequests = []string{
	"GET /v1/healthz", "POST /v1/auth/login", "POST /v1/auth/signup", "GET /v1/auth/me", "POST /v1/auth/logout",
	"GET /v1/accounts", "POST /v1/accounts", "GET /v1/accounts/acc_1", "DELETE /v1/accounts/acc_1",
	"GET /v1/accounts/acc_1/folders", "GET /v1/messages", "GET /v1/messages/m_1", "GET /v1/messages/m_1/raw",
	"GET /v1/messages/m_1/attachments/1.2", "PATCH /v1/messages/m_1", "POST /v1/messages/send",
	"GET /v1/sends/k", "GET /v1/me/apikeys", "GET /v1/events", "GET /v1/events/wait", "GET /v1/apikeys",
	"GET /mcp", "POST /mcp", "DELETE /mcp", "GET /mcp/sessions/1", "GET /metrics",
	"GET /.well-known/oauth-protected-resource", "GET /.well-known/oauth-authorization-server",
	"GET /", "GET /oauth/return", "GET /assets/main.js", "GET /v1/no-such-endpoint",
}

// matched is the pattern mux routes the request "METHOD /path" to.
func matched(t *testing.T, mux *http.ServeMux, request string) string {
	t.Helper()
	method, path, _ := strings.Cut(request, " ")
	_, pattern := mux.Handler(httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return pattern
}

func TestTheCoreRoutesAnswerTheSameRequestsWithAnExtensionMounted(t *testing.T) {
	never := http.NotFoundHandler()
	extra := []route{
		{"GET /extension/{name}", says("extension")},
		{"POST /v1/auth/another-way", says("extension")},
		{"/.well-known/extension", says("extension")},
	}
	for name, deployment := range map[string]struct {
		rest             *api.Handler
		mcpHTTP, metrics http.Handler
	}{
		"everything on":            {&api.Handler{AdminAPI: true}, never, never},
		"MCP and admin routes off": {&api.Handler{}, nil, nil},
	} {
		without, err := routes(deployment.rest, deployment.mcpHTTP, deployment.metrics, never)
		if err != nil {
			t.Fatal(err)
		}
		with, err := routes(deployment.rest, deployment.mcpHTTP, deployment.metrics, never, extra...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, request := range coreRequests {
			if before, after := matched(t, without, request), matched(t, with, request); before != after {
				t.Errorf("%s: %s went to %q, and with the extension to %q", name, request, before, after)
			}
		}
		for request, want := range map[string]string{
			"GET /extension/hello":          "GET /extension/{name}",
			"POST /v1/auth/another-way":     "POST /v1/auth/another-way",
			"DELETE /.well-known/extension": "/.well-known/extension",
		} {
			if got := matched(t, with, request); got != want {
				t.Errorf("%s: %s went to %q, not the extension's %q", name, request, got, want)
			}
		}
	}
}

func TestAnExtensionCannotTakeARouteFromTheCoreWhateverIsSwitchedOn(t *testing.T) {
	// MCP over HTTP on, and the instance-key routes, /metrics and the console
	// off: every refusal below holds all the same, so a deployment that
	// switches one on never finds an extension's route refused, or worse,
	// taking requests from it.
	never := http.NotFoundHandler()
	for pattern, why := range map[string]string{
		"GET /v1/healthz":           "the same pattern as a core route",
		"GET /v1/accounts/{name}":   "a core route's pattern with another wildcard name",
		"/mcp":                      "the MCP endpoint",
		"GET /v1/apikeys":           "an instance-key route, though they are off here",
		"GET /metrics":              "the metrics route, though it is off here",
		"GET /v1/accounts/mine":     "a path a core route's wildcard answers",
		"GET /mcp":                  "a method of the MCP endpoint",
		"/mcp/extension":            "under /mcp/, which answers the API's 404 when MCP is off",
		"/v1/accounts/x":            "a pattern that conflicts with a core route",
		"example.com/v1/healthz":    "a core route, on one host",
		"example.com/v1/":           "the whole API, on one host",
		"GET example.com/v1/{a...}": "the whole API for one method, on one host",
		"example.com/{path...}":     "every route, on one host",
		"GET example.com/{path...}": "every route for one method, on one host",
		"example.com/.well-known/":  "the discovery documents, on one host",
		"example.com/extension":     "a path no core route answers, on one host",
		"/":                         "the console's catch-all",
		"GET /":                     "the console's catch-all, for one method",
		"GET /{$}":                  "the console's page",
		"example.com/":              "the console's catch-all, on one host",
		"no slash":                  "not a pattern",
		"GET /extension/{a}/{a}":    "a pattern the mux refuses",
		"GET /extension/{bad name}": "a wildcard the mux refuses",
	} {
		_, err := routes(&api.Handler{}, never, nil, nil, route{pattern, says("extension")})
		if !errors.Is(err, ErrRoute) {
			t.Errorf("%q (%s) was not refused: %v", pattern, why, err)
		}
	}

	// A handler that is not there, and two extensions that want the same
	// pattern, are refused too.
	var r Router
	r.HandleFunc("GET /extension/nil", nil)
	if _, err := routes(&api.Handler{}, never, nil, nil, r.routes...); !errors.Is(err, ErrRoute) {
		t.Errorf("a nil handler was not refused: %v", err)
	}
	twice := []route{{"GET /extension/x", says("one")}, {"GET /extension/x", says("other")}}
	if _, err := routes(&api.Handler{}, never, nil, nil, twice...); !errors.Is(err, ErrRoute) {
		t.Errorf("the same pattern from two extensions was not refused: %v", err)
	}
}

func TestAnExtensionRouteCannotNameAHostWhichWouldOutrankEveryCoreRoute(t *testing.T) {
	// The mux tries a pattern with a host before any without one and never
	// calls the pair a conflict: admitted, "example.com/{path...}" would
	// answer /v1/..., /mcp, /metrics and /.well-known/... for that Host.
	never := http.NotFoundHandler()
	for name, deployment := range map[string]struct {
		rest             *api.Handler
		mcpHTTP, metrics http.Handler
	}{
		"everything on":            {&api.Handler{AdminAPI: true}, never, never},
		"MCP and admin routes off": {&api.Handler{}, nil, nil},
	} {
		for _, pattern := range []string{
			"example.com/v1/", "example.com/{path...}", "GET example.com/{path...}",
			"example.com/.well-known/", "GET example.com/v1/{rest...}", "POST example.com/mcp",
			"example.com/extension/{name}", "GET 127.0.0.1/extension",
		} {
			_, err := routes(deployment.rest, deployment.mcpHTTP, deployment.metrics, never, route{pattern, says("extension")})
			if !errors.Is(err, ErrRoute) || !strings.Contains(err.Error(), "host") {
				t.Errorf("%s: %q was not refused for its host: %v", name, pattern, err)
			}
		}
	}

	// The daemon does not start with one.
	cfg := localConfig(t)
	hosted := func(_ context.Context, r *Router, _ Deps) error {
		r.Handle("example.com/{path...}", says("from the extension"))
		return nil
	}
	// Bounded: a daemon that started would otherwise serve until the test
	// ends.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := Run(ctx, cfg, obs.NewLoggerTo(io.Discard, "info", "text"), Options{Extensions: []Extension{hosted}})
	if !errors.Is(err, ErrRoute) || !strings.Contains(err.Error(), "example.com/{path...}") {
		t.Fatalf("Run with a route that names a host: %v", err)
	}
}

func TestADuplicatePatternIsRefusedAtStartupBeforeAnythingListens(t *testing.T) {
	cfg := localConfig(t)
	var extensionCtx context.Context
	duplicate := func(ctx context.Context, r *Router, _ Deps) error {
		extensionCtx = ctx
		r.Handle("GET /v1/healthz", says("not the health probe"))
		return nil
	}
	logs := &lockedBuffer{}
	err := Run(t.Context(), cfg, obs.NewLoggerTo(logs, "info", "text"), Options{Extensions: []Extension{duplicate}})
	if !errors.Is(err, ErrRoute) || !strings.Contains(err.Error(), "GET /v1/healthz") {
		t.Fatalf("Run with a duplicate of a core route: %v", err)
	}
	for _, line := range []string{"msg=starting", "msg=listening"} {
		if strings.Contains(logs.String(), line) {
			t.Errorf("the daemon got as far as %s before refusing:\n%s", line, logs)
		}
	}
	if conn, err := net.DialTimeout("tcp", cfg.HTTPAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("something listens on %s after the refusal", cfg.HTTPAddr)
	}
	// The extension is told the daemon stopped, though its caller's context
	// goes on.
	if extensionCtx == nil || extensionCtx.Err() == nil || t.Context().Err() != nil {
		t.Errorf("the extension's context outlived the daemon: %v", extensionCtx)
	}
	// Refusing let go of the data directory: the next start can take it.
	lock, err := lockfile.Acquire(cfg.LockPath())
	if err != nil {
		t.Fatalf("the refusal kept the data directory locked: %v", err)
	}
	_ = lock.Release()

	// An extension that fails stops the start the same way, with its error.
	failure := errors.New("the extension could not start")
	failing := func(context.Context, *Router, Deps) error { return failure }
	if err := Run(t.Context(), cfg, obs.NewLoggerTo(io.Discard, "info", "text"), Options{Extensions: []Extension{failing}}); !errors.Is(err, failure) {
		t.Errorf("Run with an extension that fails: %v", err)
	}
}

func TestTheConsoleCarriesTheConfiguredOriginsInItsPolicy(t *testing.T) {
	cfg := localConfig(t)
	cfg.ConnectSrc = []string{"https://id.example.com", "http://localhost:9000"}
	base := runDaemon(t, cfg, Options{})
	a := fetch(t, http.MethodGet, base+"/")
	if want := "connect-src 'self' https://id.example.com http://localhost:9000;"; !strings.Contains(a.header.Get("Content-Security-Policy"), want) {
		t.Errorf("the console's policy is %q, want it to say %q", a.header.Get("Content-Security-Policy"), want)
	}
}
