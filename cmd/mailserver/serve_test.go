package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestServeWithMCPStdioRefusesToStartWithoutAKeyThatWorks(t *testing.T) {
	// A client that launches the daemon has no header to send its key in;
	// a daemon that started without one would fail only at its first call,
	// after the client had already shown the person a working server.
	logger := obs.NewLoggerTo(io.Discard, "error", "text")
	keyless := localConfig(t)
	err := serve(t.Context(), keyless, logger, []string{"--mcp-stdio"})
	if err == nil || !strings.Contains(err.Error(), "MAIL_MCP_KEY") {
		t.Fatalf("serve --mcp-stdio without a key: %v", err)
	}
	if _, err := os.Stat(keyless.DatabasePath()); err == nil {
		t.Error("the daemon opened its database before refusing to start without a key")
	}

	cfg := localConfig(t)
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.MCPKey = "deadbeef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	err = serve(t.Context(), cfg, logger, []string{"--mcp-stdio"})
	if err == nil || !strings.Contains(err.Error(), "MAIL_MCP_KEY") {
		t.Fatalf("serve --mcp-stdio with a key that matches nothing: %v", err)
	}
	if strings.Contains(err.Error(), "AAAAAAAAAAAA") {
		t.Errorf("the refusal repeats the key: %v", err)
	}
}

// apiNotFound is the API's own 404 body.
const apiNotFound = `{"code":"not_found","message":"no such endpoint"}`

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

// runDaemon runs serve on a free loopback port until the test ends, and
// returns its base URL and its log.
func runDaemon(t *testing.T, cfg config.Config) (string, *lockedBuffer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr = ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, obs.NewLoggerTo(logs, "info", "text"), nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
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
				return base, logs
			}
		}
		select {
		case err := <-done:
			t.Fatalf("serve returned before answering: %v\n%s", err, logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not answer within 10s:\n%s", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func call(t *testing.T, method, url string) (int, string, string) {
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
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func TestADaemonWithMCPOverHTTPOffSaysSoAndAnswersMCPWithTheAPIs404(t *testing.T) {
	cfg := localConfig(t)
	cfg.MCPHTTP = false
	key := readKey(t, cfg)
	base, logs := runDaemon(t, cfg)
	if served := mcpReported(t, base, key); served {
		t.Error("GET /v1/me/mcp says MCP is served over HTTP, and the console would show an address that answers 404")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if status, kind, body := call(t, method, base+"/mcp"); status != http.StatusNotFound ||
			kind != "application/json" || body != apiNotFound {
			t.Errorf("%s /mcp answered %d %q %q", method, status, kind, body)
		}
	}
	starting := logLine(t, logs.String(), "msg=starting")
	if !strings.Contains(starting, "mcp_http=off") {
		t.Errorf("the startup log does not say MCP over HTTP is off:\n%s", starting)
	}
	if strings.Contains(logs.String(), "mcp_event_store") {
		t.Errorf("the log reports the bounds of an event store nothing uses:\n%s", logs)
	}
}

func TestADaemonWithMCPOverHTTPOnServesItAsBefore(t *testing.T) {
	cfg := localConfig(t)
	cfg.MCPHTTP = true
	key := readKey(t, cfg)
	base, logs := runDaemon(t, cfg)
	if served := mcpReported(t, base, key); !served {
		t.Error("GET /v1/me/mcp says MCP is not served over HTTP")
	}
	// The MCP handler is there: it asks for a key rather than not existing.
	if status, _, body := call(t, http.MethodPost, base+"/mcp"); status != http.StatusUnauthorized {
		t.Errorf("POST /mcp without a key answered %d %q, want the MCP endpoint's 401", status, body)
	}
	starting := logLine(t, logs.String(), "msg=starting")
	if !strings.Contains(starting, "mcp_event_store_bytes=") || strings.Contains(starting, "mcp_http=off") {
		t.Errorf("the startup log = %s", starting)
	}
}

// readKey issues a read key in cfg's database, as apikey create --bootstrap
// does, before the daemon takes the lock.
func readKey(t *testing.T, cfg config.Config) string {
	t.Helper()
	db, release, err := openExclusively(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	secret, _, err := auth.NewKeys(db).Issue(t.Context(), auth.NewKeyRequest{Name: "test", Scope: auth.ScopeRead, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// mcpReported is what GET /v1/me/mcp says about MCP over HTTP.
func mcpReported(t *testing.T, base, key string) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/me/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		HTTP *bool `json:"http"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.HTTP == nil {
		t.Fatalf("GET /v1/me/mcp answered %d, %+v, %v", resp.StatusCode, body, err)
	}
	return *body.HTTP
}

// logLine is the first line of the log that contains want.
func logLine(t *testing.T, log, want string) string {
	t.Helper()
	for line := range strings.Lines(log) {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no log line with %q in:\n%s", want, log)
	return ""
}

func TestServeNeverWithholdsTeams(t *testing.T) {
	// Withholding teams is a choice of a binary that embeds the daemon
	// (app.Options.TeamsWithheld), with no environment variable: the
	// self-hosted server offers them, whatever its configuration.
	cfg := localConfig(t)
	db, release, err := openExclusively(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	session := authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	ana, err := auth.NewUsers(db).GetByEmail(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	operator, _, err := auth.NewKeys(db).Issue(t.Context(), auth.NewKeyRequest{Name: "cli", Scope: auth.ScopeAdmin, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	release()
	base, _ := runDaemon(t, cfg)

	get := func(path, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	for name, token := range map[string]string{"the operator": operator, "Ana, its owner": session} {
		status, body := get("/v1/workspaces", token)
		var listed []map[string]any
		if err := json.Unmarshal([]byte(body), &listed); err != nil || status != http.StatusOK {
			t.Fatalf("%s listing workspaces: %d %s (%v)", name, status, body, err)
		}
		found := false
		for _, w := range listed {
			if w["id"] == team.ID {
				found = true
			}
			if _, withheld := w["withheld"]; withheld {
				t.Errorf("%s lists %v withheld", name, w)
			}
		}
		if !found {
			t.Errorf("%s does not list the team: %s", name, body)
		}
	}
	if status, body := get("/v1/workspaces/"+team.ID+"/members", session); status != http.StatusOK {
		t.Errorf("Ana listing her team's members: %d %s", status, body)
	}
	if status, body := get("/v1/accounts?workspace="+team.ID, session); status != http.StatusOK {
		t.Errorf("Ana listing her team's mailboxes: %d %s", status, body)
	}
}

func TestServeNeverKeepsKeysToPersonalWorkspaces(t *testing.T) {
	// Keeping keys to personal workspaces is a choice of a binary that
	// embeds the daemon (app.Options.KeysInPersonalWorkspaceOnly), with no
	// environment variable: a team's owners create its keys on the
	// self-hosted server, whatever its configuration.
	cfg := localConfig(t)
	db, release, err := openExclusively(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	session := authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	ana, err := auth.NewUsers(db).GetByEmail(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	base, logs := runDaemon(t, cfg)

	body := `{"name":"assistant","scope":"read","terms_version":"` + cfg.Consent.OrDefaults().Keys + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/workspaces/"+team.ID+"/apikeys",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	said, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Ana creating her team's key: %d %s", resp.StatusCode, said)
	}
	if strings.Contains(logs.String(), "API keys belong to personal workspaces only") {
		t.Errorf("serve says it keeps keys to personal workspaces:\n%s", logs)
	}
}

func TestTheDaemonSaysAtStartWhichTeamsAreClosed(t *testing.T) {
	cfg := localConfig(t)
	db, release, err := openExclusively(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	var teams []string
	for i, name := range []string{"Support", "Sales", "Billing"} {
		team, err := ws.CreateTeam(t.Context(), name, ana.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		teams = append(teams, team.ID)
		// Created in this order, whatever the clock's second.
		if _, err := db.Writer().ExecContext(t.Context(), `UPDATE workspaces SET created_at = ? WHERE id = ?`,
			1700000000+i, team.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Support and Billing closed, Sales open.
	for _, id := range []string{teams[0], teams[2]} {
		if _, _, err := ws.SetClosed(t.Context(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	release()

	_, logs := runDaemon(t, cfg)
	line := logLine(t, logs.String(), `msg="teams closed`)
	if !strings.Contains(line, "count=2") || !strings.Contains(line, "["+teams[0]+" "+teams[2]+"]") ||
		strings.Contains(line, teams[1]) {
		t.Errorf("the start says %s, want the two closed teams, oldest first, and not the open one", line)
	}
}

func TestTheDaemonSaysNothingOfClosedTeamsWhereNoneIs(t *testing.T) {
	cfg := localConfig(t)
	_, logs := runDaemon(t, cfg)
	if strings.Contains(logs.String(), "teams closed") {
		t.Errorf("the start speaks of closed teams where none is:\n%s", logs)
	}
}
