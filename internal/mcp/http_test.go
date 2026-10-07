package mcp_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/service"
)

// serve mounts the HTTP transport at /mcp, as the daemon does.
func (h *harness) serve(o mcp.HTTPOptions) *httptest.Server {
	h.t.Helper()
	srv, _ := h.serveHandler(o)
	return srv
}

// serveHandler is serve, with the transport it mounted.
func (h *harness) serveHandler(o mcp.HTTPOptions) (*httptest.Server, *mcp.HTTP) {
	h.t.Helper()
	if o.PublicURL == "" {
		o.PublicURL = "https://console.mailie.example"
	}
	handler := h.mcp.HTTPHandler(o)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	srv := httptest.NewServer(mux)
	h.t.Cleanup(srv.Close)
	return srv, handler
}

// bearer adds the key to every request, as `claude mcp add --header` does.
type bearer struct {
	key  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(r)
}

// connectHTTP opens a session over HTTP with the SDK's own client.
func connectHTTP(t *testing.T, srv *httptest.Server, key, protocol string) (*sdk.ClientSession, error) {
	t.Helper()
	transport := &sdk.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", MaxRetries: -1,
		HTTPClient: &http.Client{Transport: bearer{key, http.DefaultTransport}},
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), transport, &sdk.ClientSessionOptions{ProtocolVersion: protocol})
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
	`"capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}`

// post sends one JSON-RPC message to /mcp as a client would.
func post(t *testing.T, srv *httptest.Server, token, session, origin, body string) *http.Response {
	t.Helper()
	header := http.Header{}
	if session != "" {
		header.Set("Mcp-Session-Id", session)
		header.Set("Mcp-Protocol-Version", "2025-11-25")
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return send(t, srv, http.MethodPost, token, header, body)
}

// send makes one request to /mcp with the key and the headers given.
func send(t *testing.T, srv *httptest.Server, method, token string, header http.Header, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func wireError(t *testing.T, resp *http.Response) (service.Code, string) {
	t.Helper()
	var e struct {
		Code    service.Code `json:"code"`
		Message string       `json:"message"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("not the error shape: %s", raw)
	}
	return e.Code, e.Message
}

func TestTheMCPEndpointServesAKeysToolsOverStreamableHTTP(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", ""} {
		t.Run("protocol "+protocol, func(t *testing.T) {
			h := newHarness(t)
			ana := h.person("ana@example.com", auth.RoleMember)
			const acc = "acc_00000000000000a1"
			box := h.mailbox(acc, ana.user.ID, "ana@work.example")
			id := h.deliver(acc, box, letter("Hello", "Bea <bea@example.org>", "Hi there", "a.pdf", "%PDF"))
			srv := h.serve(mcp.HTTPOptions{})
			cs, err := connectHTTP(t, srv, h.key(ana, "read"), protocol)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			m := ok[service.Message](t, cs, "get_message", map[string]any{"id": id})
			if m.Body.Text == nil || !strings.Contains(*m.Body.Text, "Hi there") {
				t.Fatalf("get_message over HTTP read %+v", m.Body)
			}
		})
	}
}

func TestAConsoleSessionTokenIsNotAnMCPCredential(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	resp := post(t, srv, ana.token, "", "", initialize)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a console session token answered %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer") || strings.Contains(challenge, "resource_metadata") {
		t.Errorf("WWW-Authenticate = %q; a static-key client must not be sent to OAuth discovery", challenge)
	}
	if code, _ := wireError(t, resp); code != service.CodeUnauthorized {
		t.Errorf("code = %s", code)
	}
	if _, err := connectHTTP(t, srv, ana.token, "2025-11-25"); err == nil {
		t.Error("an MCP client connected with a console session token")
	}
	// Nor without any credential at all.
	if resp := post(t, srv, "", "", "", initialize); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential answered %d", resp.StatusCode)
	}
}

func TestAKeyNobodyAgreedToTheTermsOfCannotOpenAnMCPSession(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	resp := post(t, srv, h.unagreedKey(ana), "", "", initialize)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a key nobody agreed to the terms of answered %d, want 403", resp.StatusCode)
	}
}

func TestAnMCPSessionBelongsToTheKeyThatOpenedIt(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	bea := h.person("bea@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	anas := h.key(ana, "read")
	resp := post(t, srv, anas, "", "", initialize)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize answered %d", resp.StatusCode)
	}
	session := resp.Header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatal("no session id: the transport must be stateful")
	}
	post(t, srv, anas, session, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	for _, other := range []string{h.key(bea, "read"), h.key(ana, "read")} {
		if resp := post(t, srv, other, session, "", list); resp.StatusCode != http.StatusForbidden {
			t.Errorf("another key used the session: %d", resp.StatusCode)
		}
	}
	if resp := post(t, srv, anas, session, "", list); resp.StatusCode != http.StatusOK {
		t.Errorf("the key that opened the session got %d", resp.StatusCode)
	}
}

func TestAWebPageFromAnotherOriginCannotCallMCP(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{PublicURL: "https://console.mailie.example"})
	key := h.key(ana, "read")
	for origin, want := range map[string]int{
		"https://evil.example":           http.StatusForbidden,
		"null":                           http.StatusForbidden,
		"http://localhost:6274":          http.StatusOK,
		"https://console.mailie.example": http.StatusOK,
		"":                               http.StatusOK,
	} {
		if resp := post(t, srv, key, "", origin, initialize); resp.StatusCode != want {
			t.Errorf("Origin %q answered %d, want %d", origin, resp.StatusCode, want)
		}
	}
}

func TestTheMCPEndpointRateLimitsAKeysFailuresAsRESTDoes(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	limits := &ratelimit.Auth{PerIP: ratelimit.New(600, 60), PerSubject: ratelimit.New(1, 2)}
	srv := h.serve(mcp.HTTPOptions{Limits: limits})
	key := h.key(ana, "read")
	prefix := prefixOf(key)
	wrong := prefix + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for range 2 {
		if resp := post(t, srv, wrong, "", "", initialize); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a wrong secret answered %d", resp.StatusCode)
		}
	}
	resp := post(t, srv, wrong, "", "", initialize)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the third wrong secret answered %d (Retry-After %q), want 429 with a wait",
			resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestARevokedKeyLosesItsMCPSessionAtTheNextCallOverHTTP(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	key := h.key(ana, "read")
	cs, err := connectHTTP(t, srv, key, "2025-11-25")
	if err != nil {
		t.Fatal(err)
	}
	ok[map[string]any](t, cs, "list_accounts", nil)
	if err := h.svc.RevokeMyAPIKey(t.Context(), ana.session, prefixOf(key)); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_accounts",
		Arguments: map[string]any{}}); err == nil {
		t.Fatal("a revoked key's session still answered a call")
	}
}

func TestAnIdleMCPSessionEndsAfterItsTimeout(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{SessionTimeout: 300 * time.Millisecond})
	key := h.key(ana, "read")
	resp := post(t, srv, key, "", "", initialize)
	session := resp.Header.Get("Mcp-Session-Id")
	time.Sleep(time.Second)
	if resp := post(t, srv, key, session, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an idle session answered %d after its timeout, want 404", resp.StatusCode)
	}
}

func TestAnMCPRequestLargerThan16MiBIsRefused(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	key := h.key(ana, "read")
	huge := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{},"clientInfo":{"name":"` + strings.Repeat("x", 16<<20) + `","version":"1"}}}`
	resp := post(t, srv, key, "", "", huge)
	if resp.StatusCode < 400 {
		t.Errorf("a request over 16 MiB answered %d", resp.StatusCode)
	}
	if resp := post(t, srv, key, "", "", initialize); resp.StatusCode != http.StatusOK {
		t.Errorf("a small one after it answered %d", resp.StatusCode)
	}
}

func TestABatchOfToolCallsIsRefused(t *testing.T) {
	// One request, one rate-limit token and one check of the key would
	// otherwise carry as many calls as 16 MiB holds, all run at once. The
	// SDK takes a request without Mcp-Protocol-Version as 2025-03-26, which
	// had batches, whatever its session negotiated.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv := h.serve(mcp.HTTPOptions{})
	key := h.key(ana, "read")
	session := post(t, srv, key, "", "", initialize).Header.Get("Mcp-Session-Id")
	post(t, srv, key, session, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	listAccounts := `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"list_accounts","arguments":{}}}`

	for name, c := range map[string]struct{ session, body string }{
		"tool calls in a session": {session, "[" + fmt.Sprintf(listAccounts, 2) + "," + fmt.Sprintf(listAccounts, 3) + "]"},
		"after blank space":       {session, " \r\n\t[" + fmt.Sprintf(listAccounts, 4) + "]"},
		"an initialize":           {"", "[" + initialize + "]"},
	} {
		header := http.Header{}
		if c.session != "" {
			header.Set("Mcp-Session-Id", c.session)
		}
		resp := send(t, srv, http.MethodPost, key, header, c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: a batch answered %d, want 400", name, resp.StatusCode)
			continue
		}
		if code, _ := wireError(t, resp); code != service.CodeBadRequest {
			t.Errorf("%s: code %s", name, code)
		}
	}
	if logs := h.logs.String(); strings.Contains(logs, `"tool":"list_accounts"`) {
		t.Errorf("a tool in a refused batch ran:\n%s", logs)
	}
	// One message per request goes through as before.
	if resp := post(t, srv, key, session, "", fmt.Sprintf(listAccounts, 5)); resp.StatusCode != http.StatusOK {
		t.Errorf("a single call after the batches answered %d", resp.StatusCode)
	}
}

func TestAKeyHoldsAtMostItsShareOfMCPSessionsAndLosesTheOneItUsedLeast(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	srv, handler := h.serveHandler(mcp.HTTPOptions{MaxSessionsPerKey: 3})
	key := h.key(ana, "read")
	open := func() string {
		t.Helper()
		resp := post(t, srv, key, "", "", initialize)
		session := resp.Header.Get("Mcp-Session-Id")
		if resp.StatusCode != http.StatusOK || session == "" {
			t.Fatalf("initialize answered %d, session %q", resp.StatusCode, session)
		}
		post(t, srv, key, session, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		return session
	}
	list := func(session string) int {
		t.Helper()
		return post(t, srv, key, session, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).StatusCode
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	first, second, third := open(), open(), open()
	// The first is used again: the second is now the one used least.
	if status := list(first); status != http.StatusOK {
		t.Fatalf("tools/list on the first session answered %d", status)
	}
	fourth := open()
	eventually("the session used least is closed when a fourth opens", func() bool {
		return list(second) == http.StatusNotFound
	})
	for _, session := range []string{first, third, fourth} {
		if status := list(session); status != http.StatusOK {
			t.Errorf("session %s answered %d; only the one used least should go", session, status)
		}
	}
	eventually("the key holds three sessions", func() bool { return mcp.SessionsOpen(handler, prefixOf(key)) == 3 })

	// A session the client closes is no longer counted.
	header := http.Header{}
	header.Set("Mcp-Session-Id", first)
	header.Set("Mcp-Protocol-Version", "2025-11-25")
	if resp := send(t, srv, http.MethodDelete, key, header, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE answered %d", resp.StatusCode)
	}
	eventually("a deleted session is no longer counted", func() bool {
		return mcp.SessionsOpen(handler, prefixOf(key)) == 2
	})
	// Another key has its own share.
	other := h.key(ana, "read")
	if resp := post(t, srv, other, "", "", initialize); resp.StatusCode != http.StatusOK {
		t.Errorf("another key's first session answered %d", resp.StatusCode)
	}
	if status := list(third); status != http.StatusOK {
		t.Errorf("another key's session closed one of this key's: %d", status)
	}
}
