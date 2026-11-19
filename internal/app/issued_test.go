package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Keys an issuer makes, as the daemon a binary embeds runs them.

func TestAKeyIssuerNeedsExternalSignInOnly(t *testing.T) {
	cfg := localConfig(t)
	err := Run(context.Background(), cfg, obs.NewLoggerTo(io.Discard, "error", "text"),
		Options{KeyIssuer: issuertest.New()})
	if !errors.Is(err, ErrKeyIssuer) {
		t.Fatalf("a key issuer beside passwords started: %v", err)
	}
	// Nothing started: no database was even made.
	if _, err := os.Stat(cfg.DatabasePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused daemon made its database: %v", err)
	}
}

func TestRESTAndMCPTakeTheSameIssuedKeys(t *testing.T) {
	cfg := localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	ws := authtest.Personal(t, db, ana.ID)
	const box = "acc_anas0000000001"
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO accounts(id, workspace_id, owner_user_id, email,
		provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state,
		state_changed_at, created_at, updated_at) VALUES (?, ?, ?, 'ana@mail.example.com', 'imap', 'password',
		'imap.example.com', 993, 'smtp.example.com', 587, 'starttls', 'ana', 1, 'active', 100, 100, 100)`,
		box, ws, ana.ID); err != nil {
		t.Fatal(err)
	}
	reads := []workspace.KeyGrant{{AccountID: box, Flags: workspace.Flags{Read: true}}}
	good, _ := issuertest.Store(t, db, issuertest.Key{WorkspaceID: ws, CreatorID: ana.ID, Grants: reads})
	revoked, _ := issuertest.Store(t, db, issuertest.Key{WorkspaceID: ws, CreatorID: ana.ID, Grants: reads, Revoked: true})
	own := authtest.NewWorkspaceKey(t, db, auth.ScopeRead, ws, ana.ID, reads...)
	operator := authtest.NewKey(t, db, auth.ScopeAdmin)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	base, logs := runDaemonLogged(t, cfg, Options{ExternalSignInOnly: true, KeyIssuer: issuertest.New(),
		KeysIssuedElsewhere: true})
	mcp := func(token string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
				`"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if a := request(t, http.MethodGet, base+"/v1/accounts", good, ""); a.status != http.StatusOK ||
		!strings.Contains(a.body, box) {
		t.Errorf("REST with the issued key: %d %s", a.status, a.body)
	}
	if status := mcp(good); status != http.StatusOK {
		t.Errorf("MCP with the issued key: %d", status)
	}
	for what, token := range map[string]string{"the revoked issued key": revoked, "her own key": own} {
		if a := request(t, http.MethodGet, base+"/v1/accounts", token, ""); a.status != http.StatusUnauthorized ||
			!strings.Contains(a.body, "invalid or expired api key") {
			t.Errorf("REST with %s: %d %s", what, a.status, a.body)
		}
		if status := mcp(token); status != http.StatusUnauthorized {
			t.Errorf("MCP with %s: %d", what, status)
		}
	}
	if a := request(t, http.MethodGet, base+"/v1/apikeys", operator, ""); a.status == http.StatusUnauthorized {
		t.Errorf("the operator's key was refused: %d %s", a.status, a.body)
	}
	var said []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "workspace keys revoked: this edition takes keys from their issuer") {
			said = append(said, l)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "revoked=1") || !strings.Contains(said[0], "unexpired=1") ||
		!strings.Contains(said[0], "at=") {
		t.Errorf("the start's revocation line: %q", said)
	}
}
