// Package webui serves the built web console.
//
// From a directory rather than from the binary. Embedding the built assets
// would make `go build` and `make check` depend on a Node toolchain, and would
// mean rebuilding the daemon to change a stylesheet. The cost is that a
// deployment ships two things, the binary and a console's build (web/dist,
// the open console, on a self-hosted server; the cloud app's on the hosted
// one), which is what a release directory is for.
//
// The console holds a session token that can remove mailboxes, so the headers
// below are load-bearing rather than decoration: anything that can run script
// in this origin, or frame it, can act as whoever is signed in.
package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/thehappieco/mailie/internal/service"
)

// contentSecurityPolicy is the policy web/index.html also carries in a meta
// tag. Set in both places on purpose: the meta tag survives being served by
// some other static server, and the header cannot be stripped by rewriting the
// HTML and is the only one that applies to responses that are not documents.
// The meta copy leaves out frame-ancestors, which browsers ignore there.
//
// connect-src 'self' is the property the design rests on: the console and the
// API share one origin, so the page never needs to talk anywhere else, and a
// script injected into it cannot send what it reads anywhere else either. A
// deployment can add origins to it (MAIL_CONNECT_SRC), and then a page may
// send what it holds there too; with none, the policy is exactly the one
// below. A browser enforces every policy a document carries, so a page also
// reaches an added origin only if its own meta tag allows it.
//
// style-src allows inline styles for one reader: a message's own HTML, which
// an edition of the console that reads mail (the hosted service's app) shows
// in a sandboxed srcdoc frame. A
// srcdoc document inherits this policy on top of its own, so without
// 'unsafe-inline' here every style in an email is refused, whatever the
// frame's policy says. Scripts stay 'self' only, and with img-src, font-src and
// connect-src at 'self', a style has nowhere to send what it could see.
//
// worker-src 'self' is the one worker the console starts: the key scheme's
// Argon2id (docs/key-scheme.md section 5.4), the kit's kdf.worker.js, a module
// script the build emits beside the page's own. Said outright rather than
// left to fall back on script-src, so that no blob: or data: worker is ever
// allowed by a later change to that directive. The derivation is JavaScript,
// not WebAssembly, so nothing here allows 'wasm-unsafe-eval'.
func contentSecurityPolicy(connectSrc []string) string {
	return "default-src 'self'; " +
		"script-src 'self'; " +
		"worker-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src " + strings.Join(append([]string{"'self'"}, connectSrc...), " ") + "; " +
		"frame-src 'none'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"object-src 'none'; " +
		"form-action 'self'"
}

// ErrNotBuilt says the directory holds no console.
var ErrNotBuilt = errors.New("webui: no index.html in that directory")

// Handler serves a built single-page console out of a directory.
type Handler struct {
	dir   string
	root  *os.Root
	fsys  fs.FS
	files http.Handler
	csp   string
}

// New opens the directory, or reports why it cannot be served.
//
// The caller is expected to carry on without a console rather than refuse to
// start: a headless daemon is a perfectly good deployment, and failing at boot
// because a frontend was not built would be a poor trade.
//
// The directory is opened as an os.Root, so no request path — "..", an
// encoded "%2e%2e", or a symlink inside the build pointing elsewhere — can
// name a file outside it.
//
// connectSrc are origins the console's pages may connect to besides their
// own, added to the policy's connect-src: MAIL_CONNECT_SRC, as config.Load
// validated it. None leaves the policy as it always was.
func New(dir string, connectSrc ...string) (*Handler, error) {
	if dir == "" {
		return nil, ErrNotBuilt
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Join(ErrNotBuilt, err)
	}
	fsys := root.FS()
	info, err := fs.Stat(fsys, "index.html")
	if err != nil || info.IsDir() {
		//nolint:errcheck // unwinding a failed open; ErrNotBuilt is the error to report
		_ = root.Close()
		return nil, errors.Join(ErrNotBuilt, err)
	}
	return &Handler{
		dir: dir, root: root, fsys: fsys, files: http.FileServerFS(fsys), csp: contentSecurityPolicy(connectSrc),
	}, nil
}

// Dir reports where the console is being served from, for the boot log.
func (h *Handler) Dir() string { return h.dir }

// Close releases the directory.
func (h *Handler) Close() error { return h.root.Close() }

// ServeHTTP answers everything the API mux did not claim.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path)
	h.headers(w)

	// Paths that belong to the API or to MCP never fall back to the page. An
	// unknown endpoint answered with 200 and HTML is a client that parses the
	// console as JSON and reports a syntax error about a route that does not
	// exist.
	if isAPIPath(clean) {
		notFoundJSON(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if clean != "/" && h.hasFile(clean) {
		w.Header().Set("Cache-Control", cacheControl(clean))
		h.files.ServeHTTP(w, r)
		return
	}
	// A missing file that looks like a file must 404. Falling back to the page
	// for everything answers a stale script URL with HTML, and the browser
	// reports that as a syntax error in a file that does not exist.
	if clean != "/" && path.Ext(clean) != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}

	// Everything else is a route inside the console — /, /oauth/return — and
	// gets the page, which is only ever revalidated: a cached shell after a
	// deploy is a client speaking an older protocol.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, h.fsys, "index.html")
}

// headers sets what every response from here carries, whatever it turns out
// to be.
func (h *Handler) headers(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", h.csp)
	header.Set("X-Content-Type-Options", "nosniff")
	// The OAuth return lands on /oauth/return with the code in its query;
	// no-referrer keeps it out of any request the page goes on to make.
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
}

// cacheControl is how long a real file may be kept. The build puts a content
// hash in everything under /assets, so what sits behind one of those names
// never changes; anything else, such as a favicon, keeps its name across
// deploys and is only cached for an hour.
func cacheControl(clean string) string {
	switch {
	case clean == "/index.html":
		// The file server redirects this to "/", which is the page itself.
		return "no-cache"
	case strings.HasPrefix(clean, "/assets/"):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=3600"
	}
}

func isAPIPath(clean string) bool {
	for _, prefix := range []string{"/v1", "/mcp", "/.well-known"} {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

// NotFound answers every request with the API's own 404, the one an unknown
// path under /v1, /mcp or /.well-known gets here: for a route the daemon
// leaves unmounted, so it answers the same with or without a console behind
// it.
func NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFoundJSON(w) })
}

// notFoundJSON is the API's own 404, in the API's own shape.
func notFoundJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	//nolint:errcheck // the client hung up; there is nothing left to say to it
	_, _ = w.Write([]byte(`{"code":"` + string(service.CodeNotFound) + `","message":"no such endpoint"}`))
}

// hasFile reports whether a regular file sits at that path inside the root.
func (h *Handler) hasFile(clean string) bool {
	info, err := fs.Stat(h.fsys, strings.TrimPrefix(clean, "/"))
	return err == nil && info.Mode().IsRegular()
}
