package webui_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/webui"
)

const page = `<!doctype html><title>Mailie Console</title><div id="app"></div>`

// newConsole lays out a small build next to a file that must never be
// reachable, and serves the build.
func newConsole(t *testing.T) (*webui.Handler, string) {
	t.Helper()
	base := t.TempDir()
	dist := filepath.Join(base, "dist")
	for name, content := range map[string]string{
		"index.html":          page,
		"assets/main-1a2b.js": "console.log('mailie')",
		"favicon-light.svg":   "<svg/>",
	} {
		path := filepath.Join(dist, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "secret.env"), []byte("MAIL_CREDENTIAL_KEY_HEX=deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := webui.New(dist)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, base
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), method, "/", nil)
	// Set the path raw, as it arrives on the wire, so nothing between the
	// test and the handler cleans it first.
	req.URL.Path = target
	req.RequestURI = target
	h.ServeHTTP(rec, req)
	return rec
}

func TestTheConsoleIsServedWithItsSecurityHeaders(t *testing.T) {
	h, _ := newConsole(t)
	for _, target := range []string{"/", "/oauth/return", "/assets/main-1a2b.js", "/assets/gone-9z.js", "/v1/nope"} {
		rec := get(t, h, http.MethodGet, target)
		header := rec.Header()
		csp := header.Get("Content-Security-Policy")
		// Exact values, not substrings: "script-src 'self'" is also a prefix
		// of a policy that allows inline script. Inline style is allowed for
		// the sandboxed frame a message's HTML is shown in; script never is.
		got := map[string]string{}
		for _, directive := range strings.Split(csp, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
			got[name] = value
		}
		for name, want := range map[string]string{
			"default-src": "'self'", "script-src": "'self'", "style-src": "'self' 'unsafe-inline'", "img-src": "'self' data:",
			"font-src": "'self'", "connect-src": "'self'", "frame-src": "'none'", "frame-ancestors": "'none'",
			"base-uri": "'none'", "object-src": "'none'", "form-action": "'self'",
		} {
			if got[name] != want {
				t.Errorf("%s: CSP %s = %q, want %q (%q)", target, name, got[name], want, csp)
			}
		}
		if len(got) != 11 {
			t.Errorf("%s: CSP has %d directives, want exactly the 11 above: %q", target, len(got), csp)
		}
		for name, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "no-referrer",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
			"X-Frame-Options":              "DENY",
			"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=()",
		} {
			if got := header.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", target, name, got, want)
			}
		}
		if header.Get("Set-Cookie") != "" {
			t.Errorf("%s set a cookie", target)
		}
	}
}

func TestEveryConsoleRouteGetsThePageAndIsOnlyRevalidated(t *testing.T) {
	// The OAuth return is a console route, not a server one: the page reads
	// the code out of its own address and posts it with the session token.
	h, _ := newConsole(t)
	for _, target := range []string{"/", "/oauth/return", "/accounts/acc_123"} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK || rec.Body.String() != page {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", target, got)
		}
	}
	if rec := get(t, h, http.MethodHead, "/oauth/return"); rec.Code != http.StatusOK {
		t.Errorf("HEAD: %d", rec.Code)
	}
}

func TestHashedAssetsAreCachedForeverAndOtherFilesForAnHour(t *testing.T) {
	h, _ := newConsole(t)
	rec := get(t, h, http.MethodGet, "/assets/main-1a2b.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('mailie')" {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", got)
	}
	rec = get(t, h, http.MethodGet, "/favicon-light.svg")
	if got := rec.Header().Get("Cache-Control"); rec.Code != http.StatusOK || got != "public, max-age=3600" {
		t.Errorf("favicon: %d, Cache-Control = %q", rec.Code, got)
	}
}

func TestAMissingFileIs404NotThePage(t *testing.T) {
	// A stale script URL answered with HTML shows up in the browser as a
	// syntax error in a file that does not exist.
	h, _ := newConsole(t)
	rec := get(t, h, http.MethodGet, "/assets/main-old.js")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<!doctype") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") || strings.Contains(got, "max-age") {
		t.Errorf("a 404 is cacheable: %q", got)
	}
}

func TestAnUnknownAPIPathIsJSONNotTheConsole(t *testing.T) {
	h, _ := newConsole(t)
	for _, target := range []string{"/v1", "/v1/does-not-exist", "/v1/accounts/acc_1/nope", "/mcp", "/mcp/sse", "/.well-known/security.txt"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			rec := get(t, h, method, target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", method, target, rec.Code)
				continue
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("%s %s: Content-Type = %q", method, target, got)
			}
			var body struct{ Code, Message string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "not_found" || body.Message != "no such endpoint" {
				t.Errorf("%s %s: body %q", method, target, rec.Body.String())
			}
		}
	}
}

func TestTheConsoleOnlyAnswersGetAndHead(t *testing.T) {
	h, _ := newConsole(t)
	rec := get(t, h, http.MethodPost, "/oauth/return")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestTheConsoleCannotBeEscapedByPathTraversal(t *testing.T) {
	h, base := newConsole(t)
	// A link inside the build pointing out of it is the traversal a path
	// filter cannot see; os.Root refuses to follow it.
	if err := os.Symlink(filepath.Join(base, "secret.env"), filepath.Join(base, "dist", "leak.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(base, "dist", "up")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/../secret.env", "/assets/../../secret.env", "/%2e%2e/secret.env", "/..%2fsecret.env",
		"/leak.env", "/up/secret.env", "//../secret.env", "/assets/..\\..\\secret.env",
	} {
		rec := get(t, h, http.MethodGet, target)
		if strings.Contains(rec.Body.String(), "MAIL_CREDENTIAL_KEY_HEX") {
			t.Errorf("%s leaked a file outside the build (%d)", target, rec.Code)
		}
	}
	// The same requests through a real server, which decodes and cleans the
	// way a client's request actually arrives.
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	for _, target := range []string{"/../secret.env", "/%2e%2e/secret.env", "/leak.env", "/up/secret.env"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "MAIL_CREDENTIAL_KEY_HEX") {
			t.Errorf("%s leaked through a real server (%d)", target, resp.StatusCode)
		}
	}
}

func TestADirectoryWithoutAConsoleIsNotServed(t *testing.T) {
	// The daemon logs this and serves the API only, rather than refusing to
	// start because a frontend was not built.
	for name, dir := range map[string]string{"unset": "", "empty": t.TempDir(), "missing": filepath.Join(t.TempDir(), "nope")} {
		if _, err := webui.New(dir); !errors.Is(err, webui.ErrNotBuilt) {
			t.Errorf("%s: err = %v, want ErrNotBuilt", name, err)
		}
	}
}
