package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/mcpbridge"
)

// call calls a tool and fails the test on a protocol error.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// text is a result's text content.
func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// secretOf is the part of a key that authenticates it.
func secretOf(key string) string {
	_, secret, _ := strings.Cut(key, ".")
	return secret
}

func TestToolsThroughTheBridgeAnswerAsTheServerDoesDirectly(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	const ops = "acc_00000000000000c1"
	box := m.mailbox(ops, "ops@example.com")
	m.deliver(ops, box, "Disk almost full", "Monitor <mon@example.com>", "Disk at 91%")
	m.deliver(ops, box, "Backup done", "Cron <cron@example.com>", "All good")
	key := m.key(auth.ScopeRead)

	via, err := bridge(t, m.endpoint(), key, nil)
	if err != nil {
		t.Fatalf("connecting through the bridge: %v", err)
	}
	straight := direct(t, m.endpoint(), key, nil)
	negotiated := via.cs.InitializeResult().ProtocolVersion
	if want := straight.InitializeResult().ProtocolVersion; negotiated != want {
		t.Errorf("through the bridge the session speaks %s, directly %s", negotiated, want)
	}

	// The tools a client is offered, their schemas and annotations included.
	viaTools, err := via.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	straightTools, err := straight.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := asJSON(t, viaTools), asJSON(t, straightTools); got != want || len(viaTools.Tools) == 0 {
		t.Errorf("tools/list through the bridge differs:\n%s\ndirectly:\n%s", got, want)
	}

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"list_accounts", nil},
		{"search_messages", map[string]any{"q": "disk"}},
		{"search_messages", map[string]any{"account": ops, "limit": 1}},
		{"list_folders", map[string]any{"account": ops}},
		// A refusal crosses the bridge as the same tool error.
		{"search_messages", map[string]any{"account": "acc_ffffffffffffffff"}},
	} {
		got, want := call(t, via.cs, c.name, c.args), call(t, straight, c.name, c.args)
		if got.IsError != want.IsError || asJSON(t, got.StructuredContent) != asJSON(t, want.StructuredContent) ||
			text(got) != text(want) {
			t.Errorf("%s %v through the bridge:\n%v %s %q\ndirectly:\n%v %s %q", c.name, c.args,
				got.IsError, asJSON(t, got.StructuredContent), text(got),
				want.IsError, asJSON(t, want.StructuredContent), text(want))
		}
	}
	if found := text(call(t, via.cs, "search_messages", map[string]any{"q": "disk"})); !strings.Contains(found, "Disk almost full") {
		t.Errorf("search_messages through the bridge found %q", found)
	}

	// Resources too.
	viaRes, err := via.cs.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: "mail://accounts"})
	if err != nil {
		t.Fatal(err)
	}
	straightRes, err := straight.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: "mail://accounts"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := asJSON(t, viaRes), asJSON(t, straightRes); got != want {
		t.Errorf("mail://accounts through the bridge:\n%s\ndirectly:\n%s", got, want)
	}

	// Every request in a session names the version it negotiated, as the
	// protocol asks of a client: the bridge's as the SDK's own client's.
	for _, v := range m.sessionVersions() {
		if v != negotiated {
			t.Errorf("a request in a session named protocol %q; the session negotiated %s", v, negotiated)
		}
	}
}

func TestTheBridgeEndsCleanlyAndClosesItsSessionWhenTheClientCloses(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	key := m.key(auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")
	via, err := bridge(t, m.endpoint(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	call(t, via.cs, "list_accounts", nil)
	if err := via.cs.Close(); err != nil {
		t.Fatal(err)
	}
	via.cs = nil
	if err := via.result(t); err != nil {
		t.Errorf("the bridge ended with %v when its client closed; want a clean end", err)
	}
	if !slices.Contains(m.seen(), http.MethodDelete+" "+prefix) {
		t.Errorf("the bridge left its session open on the server: %v", m.seen())
	}
}

func TestABridgeWithoutAKeyRefusesToStartAndSendsNothing(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	_, local := sdk.NewInMemoryTransports()
	err := mcpbridge.Run(t.Context(), local, mcpbridge.Options{Endpoint: m.endpoint()})
	if !errors.Is(err, mcpbridge.ErrNoKey) {
		t.Errorf("a bridge without a key: %v", err)
	}
	if seen := m.seen(); len(seen) != 0 {
		t.Errorf("a bridge without a key reached the server: %v", seen)
	}
}

func TestAWrongKeyEndsTheBridgeWithTheServersRefusal(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	prefix, _, _ := strings.Cut(m.key(auth.ScopeRead), ".")
	wrong := prefix + ".notTheSecretOfThisKeyAtAllxxxxxxxxxxxxxxxxx"
	via, err := bridge(t, m.endpoint(), wrong, nil)
	if err == nil {
		t.Fatal("a client connected through a bridge holding a wrong key")
	}
	end := via.result(t)
	var refusal *mcpbridge.Refusal
	if !errors.Is(end, mcpbridge.ErrKeyRefused) || !errors.As(end, &refusal) || refusal.Status != http.StatusUnauthorized {
		t.Fatalf("the bridge ended with %v; want the server's 401", end)
	}
	for what, s := range map[string]string{"the bridge's error": end.Error(), "the client's error": err.Error(), "the bridge's log": via.logs.String()} {
		if strings.Contains(s, secretOf(wrong)) {
			t.Errorf("%s repeats the key: %s", what, s)
		}
	}
}

func TestAWrongKeyEndsTheBridgeWithTheRefusalWhenTheClientLeftBeforeTheAnswer(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	prefix, _, _ := strings.Cut(m.key(auth.ScopeRead), ".")
	wrong := prefix + ".notTheSecretOfThisKeyAtAllxxxxxxxxxxxxxxxxx"
	client, local := sdk.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- mcpbridge.Run(t.Context(), local, mcpbridge.Options{Endpoint: m.endpoint(), Key: wrong})
	}()
	conn, err := client.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id, err := jsonrpc.MakeID("init")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), &jsonrpc.Request{ID: id, Method: "initialize", Params: json.RawMessage(
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`)}); err != nil {
		t.Fatal(err)
	}
	// A client that gives up on its initialize closes the bridge's standard
	// input before the server's answer, which the bridge then cannot tell it.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case end := <-done:
		var refusal *mcpbridge.Refusal
		if !errors.Is(end, mcpbridge.ErrKeyRefused) || !errors.As(end, &refusal) || refusal.Status != http.StatusUnauthorized {
			t.Errorf("the bridge ended with %v; want the server's 401", end)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the bridge did not end within 15 s")
	}
}

func TestAServerWithMCPOffEndsTheBridgeWithItsNotFound(t *testing.T) {
	// A Mailie server with MAIL_MCP_HTTP=false answers /mcp with the API's
	// own 404.
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"no such endpoint"}`))
	}))
	t.Cleanup(off.Close)
	via, err := bridge(t, off.URL, "0123abcd.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil)
	if err == nil {
		t.Fatal("a client connected to a server that does not serve MCP")
	}
	end := via.result(t)
	if !errors.Is(end, mcpbridge.ErrNoMCP) {
		t.Fatalf("the bridge ended with %v; want ErrNoMCP", end)
	}
	if !strings.Contains(end.Error(), "MAIL_MCP_HTTP=false") || !strings.Contains(end.Error(), "not_found: no such endpoint") {
		t.Errorf("the refusal does not say what to look at: %v", end)
	}
}

func TestPlainHTTPToAnotherMachineIsRefusedBeforeAnythingIsSent(t *testing.T) {
	for _, addr := range []string{"http://mail.example.com", "http://192.168.1.20:8080/mcp", "http://10.0.0.1", "http://[fd00::1]/"} {
		_, local := sdk.NewInMemoryTransports()
		err := mcpbridge.Run(t.Context(), local, mcpbridge.Options{
			Endpoint: addr, Key: "0123abcd.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Errorf("a request went to %s", r.URL)
				return nil, errors.New("no network in this test")
			}),
		})
		if !errors.Is(err, mcpbridge.ErrInsecureURL) {
			t.Errorf("%s: %v; want ErrInsecureURL", addr, err)
		}
	}
}

func TestAnAddressResolvesToTheMCPEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://mail.example.com":         "https://mail.example.com/mcp",
		"https://mail.example.com/":        "https://mail.example.com/mcp",
		"https://mail.example.com/mcp":     "https://mail.example.com/mcp",
		"https://mail.example.com/mcp/":    "https://mail.example.com/mcp",
		"https://example.com/mailie":       "https://example.com/mailie/mcp",
		" https://mail.example.com:8443 ":  "https://mail.example.com:8443/mcp",
		"http://localhost:8080":            "http://localhost:8080/mcp",
		"http://127.0.0.1:8080/mcp":        "http://127.0.0.1:8080/mcp",
		"http://[::1]:8080":                "http://[::1]:8080/mcp",
		"http://LOCALHOST.:8080":           "http://LOCALHOST.:8080/mcp",
		"https://mail.example.com/x%2Fy/z": "https://mail.example.com/x/y/z/mcp",
	} {
		if got, err := mcpbridge.Endpoint(in); err != nil || got != want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]error{
		"":                                      mcpbridge.ErrBadURL,
		"mail.example.com":                      mcpbridge.ErrBadURL,
		"ftp://mail.example.com":                mcpbridge.ErrBadURL,
		"https://":                              mcpbridge.ErrBadURL,
		"https://ana:s3cr3tpw@mail.example.com": mcpbridge.ErrBadURL,
		"https://mail.example.com/?key=x":       mcpbridge.ErrBadURL,
		"https://mail.example.com/#x":           mcpbridge.ErrBadURL,
		"http://mail.example.com":               mcpbridge.ErrInsecureURL,
		"http://localhost.example.com":          mcpbridge.ErrInsecureURL,
	} {
		_, err := mcpbridge.Endpoint(in)
		if !errors.Is(err, want) {
			t.Errorf("Endpoint(%q): %v; want %v", in, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3tpw") {
			t.Errorf("the refusal of %q repeats the password: %v", in, err)
		}
	}
}

func TestASubscriptionsUpdatesReachTheClientThroughTheBridge(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	const ops = "acc_00000000000000c1"
	box := m.mailbox(ops, "ops@example.com")
	id := m.deliver(ops, box, "Disk almost full", "Monitor <mon@example.com>", "Disk at 91%")
	updates := make(chan string, 8)
	key := m.key(auth.ScopeRead)
	via, err := bridge(t, m.endpoint(), key, &sdk.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox := "mail://" + ops + "/folder/inbox"
	if err := via.cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: inbox}); err != nil {
		t.Fatal(err)
	}
	m.waitForLog(`"msg":"mcp subscription"`, `"account":"`+ops+`"`, `"outcome":"ok"`)
	waitForStreams(t, m, key, 1)
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)
}

// waitForStreams waits until the server has received n standalone stream
// requests with key.
func waitForStreams(t *testing.T, m *mailie, key string, n int) {
	t.Helper()
	prefix, _, _ := strings.Cut(key, ".")
	waitFor(t, "the bridge's stream of notifications", func() bool {
		count := 0
		for _, r := range m.seen() {
			if r == http.MethodGet+" "+prefix {
				count++
			}
		}
		return count >= n
	})
	// The request reached the handler; give it the moment it takes to be
	// the session's stream.
	time.Sleep(200 * time.Millisecond)
}

func expectUpdate(t *testing.T, updates <-chan string, uri string) {
	t.Helper()
	select {
	case got := <-updates:
		if got != uri {
			t.Errorf("notified of %s; subscribed to %s", got, uri)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notifications/resources/updated reached the client within 10 s")
	}
}

func TestAfterTheServerForgetsTheSessionTheBridgeOpensAnotherWithTheSubscriptions(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	const ops = "acc_00000000000000c1"
	box := m.mailbox(ops, "ops@example.com")
	id := m.deliver(ops, box, "Disk almost full", "Monitor <mon@example.com>", "Disk at 91%")
	updates := make(chan string, 8)
	key := m.key(auth.ScopeRead)
	via, err := bridge(t, m.endpoint(), key, &sdk.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox := "mail://" + ops + "/folder/inbox"
	if err := via.cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: inbox}); err != nil {
		t.Fatal(err)
	}
	waitForStreams(t, m, key, 1)

	// The daemon restarts: the session, and its subscription, are gone.
	m.restart()
	// The stream of notifications finds the session gone, and the bridge
	// opens another at once, with the client's subscription: the old
	// stream, the attempt that found it gone, and the new session's.
	waitForStreams(t, m, key, 3)
	waitFor(t, "the subscription made again", func() bool {
		return strings.Count(m.logs.String(), `"msg":"mcp subscription"`) >= 2
	})
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)

	// And calls go on in the new session, the client none the wiser.
	if res := call(t, via.cs, "list_accounts", nil); res.IsError || !strings.Contains(text(res), ops) {
		t.Errorf("list_accounts after the restart: %v %q", res.IsError, text(res))
	}
	if err := via.cs.Ping(t.Context(), nil); err != nil {
		t.Errorf("ping after the restart: %v", err)
	}
}

func TestAnIdleSessionTheServerEndedIsReopenedAtTheNextCall(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{SessionTimeout: 300 * time.Millisecond})
	const ops = "acc_00000000000000c1"
	m.mailbox(ops, "ops@example.com")
	via, err := bridge(t, m.endpoint(), m.key(auth.ScopeRead), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := call(t, via.cs, "list_accounts", nil)
	time.Sleep(1500 * time.Millisecond) // idle past the server's timeout
	again := call(t, via.cs, "list_accounts", nil)
	if again.IsError || text(again) != text(first) {
		t.Errorf("list_accounts after the session expired: %v %q; before: %q", again.IsError, text(again), text(first))
	}
	if !strings.Contains(via.logs.String(), "a new one is open") {
		t.Errorf("the bridge did not open a new session:\n%s", via.logs)
	}
}

func TestACallInFlightWhenTheServerForgetsTheSessionIsAnsweredAndTheNextOneWorks(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	const ops = "acc_00000000000000c1"
	m.mailbox(ops, "ops@example.com")
	via, err := bridge(t, m.endpoint(), m.key(auth.ScopeRead), nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	posts := m.count(http.MethodPost)
	go func() {
		res, err := via.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "wait_for_new_mail",
			Arguments: map[string]any{"timeout_seconds": 120}})
		if err == nil && !res.IsError {
			err = errors.New("the wait answered as if nothing had happened: " + text(res))
		} else if err == nil {
			err = errors.New(text(res))
		}
		answered <- err
	}()
	m.waitForRequests(http.MethodPost, posts+1)
	m.restart()
	select {
	case err := <-answered:
		if err == nil {
			t.Error("the wait in flight was not told its session ended")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the call in flight when the session ended was never answered")
	}
	if res := call(t, via.cs, "list_accounts", nil); res.IsError || !strings.Contains(text(res), ops) {
		t.Errorf("list_accounts after the restart: %v %q", res.IsError, text(res))
	}
}

func TestALongCallDoesNotHoldUpTheOthers(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	const ops = "acc_00000000000000c1"
	m.mailbox(ops, "ops@example.com")
	via, err := bridge(t, m.endpoint(), m.key(auth.ScopeRead), nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	posts := m.count(http.MethodPost)
	go func() {
		defer close(waited)
		_, _ = via.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "wait_for_new_mail",
			Arguments: map[string]any{"timeout_seconds": 4}})
	}()
	m.waitForRequests(http.MethodPost, posts+1)
	started := time.Now()
	if res := call(t, via.cs, "list_accounts", nil); res.IsError {
		t.Fatalf("list_accounts: %q", text(res))
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("list_accounts took %v behind a wait in flight", took)
	}
	<-waited
}

func TestARevokedKeyEndsTheBridgeAtItsNextCall(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	key := m.key(auth.ScopeRead)
	via, err := bridge(t, m.endpoint(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	call(t, via.cs, "list_accounts", nil)
	prefix, _, _ := strings.Cut(key, ".")
	if err := auth.NewKeys(m.store).Revoke(t.Context(), prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := via.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_accounts", Arguments: map[string]any{}}); err == nil {
		t.Error("a call with a revoked key was answered")
	}
	if end := via.result(t); !errors.Is(end, mcpbridge.ErrKeyRefused) {
		t.Errorf("the bridge ended with %v; want ErrKeyRefused", end)
	}
}

func TestARedirectIsNotFollowedSoTheKeyNeverReachesWhereItPoints(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(elsewhere.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	via, err := bridge(t, redirector.URL, "0123abcd.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil)
	if err == nil {
		t.Fatal("a client connected through a redirect")
	}
	if end := via.result(t); !errors.Is(end, mcpbridge.ErrRedirected) {
		t.Errorf("the bridge ended with %v; want ErrRedirected", end)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the address the redirect named received %d requests", n)
	}
}

// toy is an MCP server of the SDK's own behind a bearer key, for what a
// Mailie server does not do yet: progress on demand, and requests of its own
// to the client.
func toy(t *testing.T, key string) *httptest.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "toy", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "count"}, func(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		for i := 1; i <= 3; i++ {
			if err := req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
				ProgressToken: req.Params.GetProgressToken(), Progress: float64(i), Total: 3,
			}); err != nil {
				return nil, nil, err
			}
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "counted"}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "ask"}, func(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{
			Mode: "form", Message: "Which folder?",
			RequestedSchema: map[string]any{
				"type": "object", "properties": map[string]any{"folder": map[string]any{"type": "string"}},
			},
		})
		if err != nil {
			return nil, nil, err
		}
		folder, _ := res.Content["folder"].(string)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: res.Action + ":" + folder}}}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

func TestProgressOfACallReachesTheClientThroughTheBridge(t *testing.T) {
	const key = "0123abcd.toyKeyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	srv := toy(t, key)
	var mu sync.Mutex
	var progress []float64
	via, err := bridge(t, srv.URL, key, &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Progress)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := &sdk.CallToolParams{Name: "count", Arguments: map[string]any{}}
	params.SetProgressToken("count-1")
	res, err := via.cs.CallTool(t.Context(), params)
	if err != nil || text(res) != "counted" {
		t.Fatalf("count: %v %v", err, res)
	}
	waitFor(t, "three progress notifications", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(progress) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(progress, []float64{1, 2, 3}) {
		t.Errorf("progress %v; want 1, 2, 3", progress)
	}
}

func TestARequestTheServerMakesOfTheClientIsRelayedBothWays(t *testing.T) {
	const key = "0123abcd.toyKeyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	srv := toy(t, key)
	via, err := bridge(t, srv.URL, key, &sdk.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			if req.Params.Message != "Which folder?" {
				return nil, errors.New("asked " + req.Params.Message)
			}
			return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"folder": "Receipts"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := call(t, via.cs, "ask", nil); res.IsError || text(res) != "accept:Receipts" {
		t.Errorf("the server asked the client a question and got %v %q", res.IsError, text(res))
	}
}

func TestACheckOpensASessionWithTheKeyAndClosesIt(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	key := m.key(auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")
	if err := mcpbridge.Check(t.Context(), mcpbridge.Options{Endpoint: m.endpoint(), Key: key}); err != nil {
		t.Fatalf("a good key: %v", err)
	}
	if !slices.Contains(m.seen(), http.MethodDelete+" "+prefix) {
		t.Errorf("the check left its session open: %v", m.seen())
	}
	err := mcpbridge.Check(t.Context(), mcpbridge.Options{Endpoint: m.endpoint(), Key: prefix + ".wrongwrongwrongwrongwrongwrongwrongwrongwro"})
	if !errors.Is(err, mcpbridge.ErrKeyRefused) || strings.Contains(err.Error(), "wrongwrong") {
		t.Errorf("a wrong key: %v", err)
	}
	if err := mcpbridge.Check(t.Context(), mcpbridge.Options{Endpoint: m.endpoint()}); !errors.Is(err, mcpbridge.ErrNoKey) {
		t.Errorf("no key: %v", err)
	}

	// A key nobody agreed to the key terms through is not a tool's
	// credential.
	ana := authtest.NewUser(t, m.store, "ana@example.com", auth.RoleMember)
	unagreed := authtest.NewWorkspaceKey(t, m.store, auth.ScopeRead, authtest.Personal(t, m.store, ana.ID), ana.ID)
	if _, err := m.store.Writer().ExecContext(t.Context(), `UPDATE api_keys SET terms_version = '' WHERE prefix = ?`,
		authtest.Prefix(unagreed)); err != nil {
		t.Fatal(err)
	}
	if err := mcpbridge.Check(t.Context(), mcpbridge.Options{Endpoint: m.endpoint(), Key: unagreed}); !errors.Is(err, mcpbridge.ErrKeyNotAllowed) {
		t.Errorf("a key nobody agreed to the terms of: %v", err)
	}
}

func TestReachableTellsAWrongAddressFromOneThatWantsAKey(t *testing.T) {
	m := newMailie(t, mcp.HTTPOptions{})
	if err := mcpbridge.Reachable(t.Context(), m.endpoint(), nil); err != nil {
		t.Errorf("a Mailie server asking for a key: %v", err)
	}
	off := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(off.Close)
	if err := mcpbridge.Reachable(t.Context(), off.URL, nil); !errors.Is(err, mcpbridge.ErrNoMCP) {
		t.Errorf("a server with no MCP: %v", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := mcpbridge.Reachable(t.Context(), gone.URL, nil); !errors.Is(err, mcpbridge.ErrUnreachable) {
		t.Errorf("a server that is not there: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// subscribedThrough connects a client through a bridge to endpoint, subscribes
// it to the inbox of a mailbox with one message, waits for the bridge's
// stream of notifications, and returns the client, the inbox's URI, the
// message to announce and where the client's updates arrive.
func subscribedThrough(t *testing.T, m *mailie, endpoint, key string) (*bridged, string, int64, <-chan string) {
	t.Helper()
	const ops = "acc_00000000000000c1"
	box := m.mailbox(ops, "ops@example.com")
	id := m.deliver(ops, box, "Disk almost full", "Monitor <mon@example.com>", "Disk at 91%")
	updates := make(chan string, 8)
	via, err := bridge(t, endpoint, key, &sdk.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox := "mail://" + ops + "/folder/inbox"
	if err := via.cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: inbox}); err != nil {
		t.Fatal(err)
	}
	m.waitForLog(`"msg":"mcp subscription"`, `"account":"`+ops+`"`, `"outcome":"ok"`)
	waitForStreams(t, m, key, 1)
	return via, inbox, id, updates
}

// expectNoUpdate fails when an update reaches the client within a moment.
func expectNoUpdate(t *testing.T, updates <-chan string, why string) {
	t.Helper()
	select {
	case got := <-updates:
		t.Errorf("%s: an update for %s reached the client", why, got)
	case <-time.After(500 * time.Millisecond):
	}
}

// waitForSubscriptions waits until the server has logged n subscriptions,
// and a moment more for the last one's watch to start: an announcement
// before it begins waiting on the journal is not news to it.
func (m *mailie) waitForSubscriptions(n int) {
	m.t.Helper()
	waitFor(m.t, fmt.Sprintf("%d subscriptions", n), func() bool {
		return strings.Count(m.logs.String(), `"msg":"mcp subscription"`) >= n
	})
	time.Sleep(300 * time.Millisecond)
}

func TestAStreamCutOnTheClientsSideIsAskedForUntilTheServerLetsGoAndMissesNothing(t *testing.T) {
	mcpbridge.SetListenTimings(t, 100*time.Millisecond, 400*time.Millisecond, time.Minute)
	m := newMailie(t, mcp.HTTPOptions{})
	p := newHalfOpen(t, m)
	key := m.key(auth.ScopeRead)
	via, inbox, id, updates := subscribedThrough(t, m, p.endpoint(), key)
	const ops = "acc_00000000000000c1"
	// One update read before the cut: the bridge holds its event's id.
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)

	// The client's side drops; the server still holds the stream for the
	// connection it believes is there, and refuses the bridge's (409).
	p.cut()
	waitFor(t, "the server refusing a stream it still holds", func() bool {
		return strings.Contains(via.logs.String(), "the server still holds the stream of notifications")
	})
	// Meanwhile the server sends an update on the stream nobody reads.
	m.announce(ops, id, "Disk almost full")
	expectNoUpdate(t, updates, "while the stream was held")

	// The server notices; the bridge's next request gets the stream with
	// Last-Event-ID, and the update sent meanwhile is replayed, once.
	p.release()
	expectUpdate(t, updates, inbox)
	expectNoUpdate(t, updates, "after the replay")
	if strings.Contains(via.logs.String(), "a new one is open") {
		t.Errorf("the bridge opened a new session for a stream it got back:\n%s", via.logs)
	}

	// And the session goes on.
	if res := call(t, via.cs, "list_accounts", nil); res.IsError {
		t.Errorf("list_accounts after the stream came back: %q", text(res))
	}
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)
}

func TestAStreamTheServerCanNoLongerResumeIsHadAgainInANewSessionWithTheSubscriptions(t *testing.T) {
	mcpbridge.SetListenTimings(t, 100*time.Millisecond, 400*time.Millisecond, time.Minute)
	// The server keeps what it sends for a second only.
	m := newMailie(t, mcp.HTTPOptions{EventStore: mcp.NewEventStore(0, time.Second)})
	p := newHalfOpen(t, m)
	key := m.key(auth.ScopeRead)
	via, inbox, id, updates := subscribedThrough(t, m, p.endpoint(), key)
	const ops = "acc_00000000000000c1"
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)

	p.cut()
	waitFor(t, "the server refusing a stream it still holds", func() bool {
		return strings.Contains(via.logs.String(), "the server still holds the stream of notifications")
	})
	// An update on the stream nobody reads, which the server no longer keeps
	// by the time it lets the stream go.
	m.announce(ops, id, "Disk almost full")
	time.Sleep(1500 * time.Millisecond)
	p.release()

	// Resuming fails (400): the bridge opens a new session with the
	// subscription, and the updates after it arrive.
	m.waitForSubscriptions(2) // made again, in a new session
	if !strings.Contains(via.logs.String(), "no longer keeps what it sent") {
		t.Errorf("the bridge did not say that updates were lost:\n%s", via.logs)
	}
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)
	if res := call(t, via.cs, "list_accounts", nil); res.IsError {
		t.Errorf("list_accounts in the new session: %q", text(res))
	}
}

func TestAStreamTheServerKeepsHoldingIsGivenUpWithItsSession(t *testing.T) {
	mcpbridge.SetListenTimings(t, 100*time.Millisecond, 400*time.Millisecond, 1500*time.Millisecond)
	m := newMailie(t, mcp.HTTPOptions{})
	p := newHalfOpen(t, m)
	key := m.key(auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")
	via, inbox, id, updates := subscribedThrough(t, m, p.endpoint(), key)
	const ops = "acc_00000000000000c1"

	// The server never notices: the proxy holds its side for good.
	p.cut()
	m.waitForSubscriptions(2) // made again, in a new session
	if !strings.Contains(via.logs.String(), "still holds the stream of notifications for a connection the bridge lost; the session") {
		t.Errorf("the bridge did not say why it opened a new session:\n%s", via.logs)
	}
	// The old session was ended on the server, which let go of its stream.
	if !slices.Contains(m.seen(), http.MethodDelete+" "+prefix) {
		t.Errorf("the session given up was left open on the server: %v", m.seen())
	}
	m.announce(ops, id, "Disk almost full")
	expectUpdate(t, updates, inbox)
	if res := call(t, via.cs, "list_accounts", nil); res.IsError {
		t.Errorf("list_accounts in the new session: %q", text(res))
	}
}
