package webui_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/webui"
)

// What a binary that embeds the daemon adds to how the console is served
// (webui.Pages): headers of its own on the page documents it chooses, and
// small files beside them. Its paths here are an edition's own, named
// nothing in particular.

// buildDir lays out a small build: the page, an asset and a favicon.
func buildDir(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for name, content := range map[string]string{
		"index.html": page, "assets/main-1a2b.js": "console.log('mailie')", "favicon-light.svg": "<svg/>",
	} {
		path := filepath.Join(dist, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

// withPages serves a build with pages.
func withPages(t *testing.T, pages webui.Pages) *webui.Handler {
	t.Helper()
	h, err := webui.NewWith(buildDir(t), webui.Options{Pages: pages})
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// ask sends method to target with headers, and records the answer.
func ask(t *testing.T, h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for name, value := range header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// neighbour is the origin whose requests a document below answers with an
// opt-in of its own.
const neighbour = "https://other.example"

// screens is a binary's document: its own headers on the pages under
// /screens/ only, one of them only for a request whose one Referer is
// exactly neighbour's, and those pages answered whole.
func screens(asked *[]string) func(*http.Request) webui.Document {
	return func(r *http.Request) webui.Document {
		*asked = append(*asked, r.Method+" "+r.URL.Path)
		if !strings.HasPrefix(r.URL.Path, "/screens/") {
			return webui.Document{}
		}
		header := http.Header{"Speculation-Rules": {`"/rules.json"`}}
		if refs := r.Header.Values("Referer"); len(refs) == 1 {
			if u, err := url.Parse(refs[0]); err == nil && u.Scheme+"://"+u.Host == neighbour {
				header.Set("Supports-Loading-Mode", "credentialed-prerender")
			}
		}
		return webui.Document{Header: header, Whole: true}
	}
}

func TestABinarysDocumentCarriesItsHeadersOnThePagesItChooses(t *testing.T) {
	var asked []string
	h := withPages(t, webui.Pages{Document: screens(&asked)})

	rec := ask(t, h, http.MethodGet, "/screens/one?workspace=w", map[string]string{"Referer": neighbour + "/somewhere?x=1"})
	if rec.Code != http.StatusOK || rec.Body.String() != page {
		t.Fatalf("/screens/one: %d %q", rec.Code, rec.Body.String())
	}
	header := rec.Header()
	if header.Get("Speculation-Rules") != `"/rules.json"` || header.Get("Supports-Loading-Mode") != "credentialed-prerender" {
		t.Errorf("a page the binary chose, asked from its neighbour: %v", header)
	}
	// The handler's own headers are all there, as on any page.
	if header.Get("Content-Security-Policy") != alwaysPolicy || header.Get("X-Frame-Options") != "DENY" ||
		header.Get("Referrer-Policy") != "no-referrer" || header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the handler's own headers: %v", header)
	}

	// Asked by anyone else, or by two at once: no opt-in.
	for name, referer := range map[string]string{"nobody": "", "another origin": "https://elsewhere.example/",
		"a subdomain": "https://a.other.example/"} {
		headers := map[string]string{}
		if referer != "" {
			headers["Referer"] = referer
		}
		rec := ask(t, h, http.MethodGet, "/screens/one", headers)
		if got := rec.Header().Get("Supports-Loading-Mode"); got != "" || rec.Header().Get("Speculation-Rules") == "" {
			t.Errorf("%s: opt-in %q, rules %q", name, got, rec.Header().Get("Speculation-Rules"))
		}
	}

	// A page the binary did not choose is the page as it always was.
	rec = ask(t, h, http.MethodGet, "/oauth/return?code=x", map[string]string{"Referer": neighbour + "/"})
	if rec.Code != http.StatusOK || rec.Header().Get("Speculation-Rules") != "" ||
		rec.Header().Get("Supports-Loading-Mode") != "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("/oauth/return: %d %v", rec.Code, rec.Header())
	}
}

func TestADocumentIsAskedAboutPageDocumentsOnly(t *testing.T) {
	var asked []string
	h := withPages(t, webui.Pages{Document: screens(&asked)})
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/"}, {http.MethodHead, "/screens/one"}, {http.MethodGet, "/a/b?c=d"},
		// Never a file of the build, a missing file, the API's, a refusal.
		{http.MethodGet, "/assets/main-1a2b.js"}, {http.MethodGet, "/favicon-light.svg"},
		{http.MethodGet, "/gone.js"}, {http.MethodGet, "/v1/nope"}, {http.MethodPost, "/screens/one"},
	} {
		ask(t, h, c.method, c.target, nil)
	}
	if got := strings.Join(asked, ", "); got != "GET /, HEAD /screens/one, GET /a/b" {
		t.Errorf("asked about %q, want the three page documents", got)
	}
}

func TestADocumentNeverReplacesWhatProtectsThePage(t *testing.T) {
	hostile := http.Header{
		"Content-Security-Policy":      {"default-src *"},
		"X-Frame-Options":              {"ALLOWALL"},
		"X-Content-Type-Options":       {"sniff"},
		"Cross-Origin-Opener-Policy":   {"unsafe-none"},
		"Cross-Origin-Resource-Policy": {"cross-origin"},
		"Permissions-Policy":           {"camera=*"},
		"Cache-Control":                {"public, max-age=31536000"},
		"Content-Type":                 {"text/plain"},
		"Content-Disposition":          {"attachment"},
		"Set-Cookie":                   {"session=x"},
		"etag":                         {`"forever"`},
		// A policy that sends another origin the page's path and query,
		// where a provider's return carries its code.
		"Referrer-Policy": {"unsafe-url"},
	}
	h := withPages(t, webui.Pages{Document: func(*http.Request) webui.Document { return webui.Document{Header: hostile} }})
	rec := ask(t, h, http.MethodGet, "/oauth/return?code=secret", nil)
	header := rec.Header()
	for name, want := range map[string]string{
		"Content-Security-Policy": alwaysPolicy, "X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff",
		"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy": "camera=(), microphone=(), geolocation=(), payment=()",
		"Cache-Control":      "no-cache", "Content-Type": "text/html; charset=utf-8", "Content-Disposition": "",
		"Set-Cookie": "", "Etag": "", "Referrer-Policy": "no-referrer",
	} {
		if got := header.Values(name); strings.Join(got, ", ") != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, policy := range []string{"no-referrer-when-downgrade", "strict-origin, unsafe-url", ""} {
		h := withPages(t, webui.Pages{Document: func(*http.Request) webui.Document {
			return webui.Document{Header: http.Header{"Referrer-Policy": {policy}}}
		}})
		if got := ask(t, h, http.MethodGet, "/", nil).Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("a document's %q: Referrer-Policy = %q, want the handler's no-referrer", policy, got)
		}
	}
	// One that keeps the path and query home is the document's to choose.
	h = withPages(t, webui.Pages{Document: func(*http.Request) webui.Document {
		return webui.Document{Header: http.Header{"referrer-policy": {"strict-origin"}}}
	}})
	if got := ask(t, h, http.MethodGet, "/", nil).Header().Values("Referrer-Policy"); len(got) != 1 || got[0] != "strict-origin" {
		t.Errorf("a document's strict-origin: Referrer-Policy = %q", got)
	}
}

func TestADocumentAnsweredWholeIsNeverStoredNorAnsweredInPart(t *testing.T) {
	var asked []string
	h := withPages(t, webui.Pages{Document: screens(&asked)})
	// What a browser holding a stored copy sends, and a range.
	conditional := map[string]string{
		"If-Modified-Since": "Mon, 01 Jan 2125 00:00:00 GMT", "If-None-Match": `"*"`, "Range": "bytes=0-3",
	}
	rec := ask(t, h, http.MethodGet, "/screens/one", conditional)
	if rec.Code != http.StatusOK || rec.Body.String() != page || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a page answered whole: %d %q, Cache-Control %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	// Any other page is revalidated as ever: the same request is a 304.
	if rec := ask(t, h, http.MethodGet, "/", map[string]string{"If-Modified-Since": "Mon, 01 Jan 2125 00:00:00 GMT"}); rec.Code != http.StatusNotModified ||
		rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("another page: %d, Cache-Control %q, want 304 and no-cache", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestAFileBesideThePagesIsServedWholeWithItsTypeAndNeverStored(t *testing.T) {
	rules := []byte(`{"prerender":[]}`)
	files := map[string]webui.File{
		"/rules.json":        {ContentType: "application/speculationrules+json", Body: rules},
		"/favicon-light.svg": {ContentType: "image/svg+xml", Body: []byte("<svg id=binary/>")},
	}
	h := withPages(t, webui.Pages{Files: files})
	// The caller's map and bytes are its own once served.
	files["/rules.json"].Body[0] = 'X'
	delete(files, "/favicon-light.svg")

	rec := ask(t, h, http.MethodGet, "/rules.json", map[string]string{"If-None-Match": `"*"`, "Range": "bytes=0-1"})
	header := rec.Header()
	if rec.Code != http.StatusOK || rec.Body.String() != `{"prerender":[]}` {
		t.Fatalf("/rules.json: %d %q", rec.Code, rec.Body.String())
	}
	if header.Get("Content-Type") != "application/speculationrules+json" || header.Get("Cache-Control") != "no-store" ||
		header.Get("Content-Length") != "16" || header.Get("Content-Security-Policy") != alwaysPolicy ||
		header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("/rules.json: %v", header)
	}
	if rec := ask(t, h, http.MethodHead, "/rules.json", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 ||
		rec.Header().Get("Content-Length") != "16" {
		t.Errorf("HEAD /rules.json: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec := ask(t, h, http.MethodPost, "/rules.json", nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /rules.json: %d %v", rec.Code, rec.Header())
	}
	// A path the build also has is the binary's.
	if rec := ask(t, h, http.MethodGet, "/favicon-light.svg", nil); rec.Body.String() != "<svg id=binary/>" ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("/favicon-light.svg: %q %v", rec.Body.String(), rec.Header())
	}
}

func TestPagesItWouldNotServeAreRefusedBeforeTheDirectoryIsRead(t *testing.T) {
	json := "application/json"
	for name, files := range map[string]map[string]webui.File{
		"the root":              {"/": {ContentType: json}},
		"a relative path":       {"rules.json": {ContentType: json}},
		"a path not clean":      {"/a/../rules.json": {ContentType: json}},
		"a trailing slash":      {"/rules.json/": {ContentType: json}},
		"no extension":          {"/rules": {ContentType: json}},
		"the console's page":    {"/index.html": {ContentType: "text/html"}},
		"the build's assets":    {"/assets/rules.json": {ContentType: json}},
		"the API's":             {"/v1/rules.json": {ContentType: json}},
		"MCP's":                 {"/mcp/rules.json": {ContentType: json}},
		"well-known":            {"/.well-known/rules.json": {ContentType: json}},
		"a space":               {"/my rules.json": {ContentType: json}},
		"a percent":             {"/rules%2e.json": {ContentType: json}},
		"no content type":       {"/rules.json": {}},
		"no media type at all":  {"/rules.json": {ContentType: "rules"}},
		"one of two is refused": {"/rules.json": {ContentType: json}, "/v1/x.json": {ContentType: json}},
	} {
		pages := webui.Pages{Files: files}
		if err := pages.Check(); !errors.Is(err, webui.ErrPages) {
			t.Errorf("%s: Check = %v, want ErrPages", name, err)
		}
		// Before the directory: a missing build is not what is wrong.
		if _, err := webui.NewWith("", webui.Options{Pages: pages}); !errors.Is(err, webui.ErrPages) || errors.Is(err, webui.ErrNotBuilt) {
			t.Errorf("%s: NewWith = %v, want ErrPages", name, err)
		}
	}
	ok := webui.Pages{Files: map[string]webui.File{"/a/rules-1.json": {ContentType: "application/speculationrules+json"}}}
	if err := ok.Check(); err != nil {
		t.Errorf("a file it serves: %v", err)
	}
	if err := (webui.Pages{}).Check(); err != nil {
		t.Errorf("no pages: %v", err)
	}
}
