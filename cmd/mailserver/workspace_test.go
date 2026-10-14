package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// realDaemon serves the real REST routes over a fresh database, and returns
// the configuration of a command line holding an unrestricted instance admin
// key for it: the operator.
func realDaemon(t *testing.T) (config.Config, *store.Store) {
	t.Helper()
	db := storetest.New(t)
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, daemonKeyring(t)),
		account.RegistryOptions{SpoolDir: t.TempDir()})
	t.Cleanup(func() { _ = registry.Close() })
	logger := obs.NewLogger("error", "text")
	h := &api.Handler{
		Service: service.New(service.Deps{
			Accounts: registry, Keys: auth.NewKeys(db), Users: auth.NewUsers(db), Store: db, Log: logger,
			PublicURL: "https://console.example",
		}),
		Limits: ratelimit.DefaultAuth(nil), SignInLimits: ratelimit.DefaultSignIn(nil), Log: logger,
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return config.Config{
		HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: authtest.NewKey(t, db, auth.ScopeAdmin),
	}, db
}

// daemonKeyring is the keyring realDaemon seals credentials with, for a test
// that stores one the daemon then holds.
func daemonKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0x7e
	}
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func TestTheOperatorAdministersTeamsFromTheCommandLineAndGrantsOnlyManage(t *testing.T) {
	cfg, db := realDaemon(t)
	ctx := t.Context()
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	run := func(command func() error, what string) {
		t.Helper()
		if err := command(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	run(func() error {
		return workspaceCommand(ctx, cfg, []string{"create", "--name", "Support", "--owner", "ana@example.com"})
	}, "workspace create")
	mine, err := ws.ForPerson(ctx, ana.ID)
	if err != nil || len(mine) != 2 || mine[1].Name != "Support" || mine[1].Role != workspace.RoleOwner {
		t.Fatalf("Ana's workspaces: %+v (%v)", mine, err)
	}
	team := mine[1].ID
	run(func() error {
		return workspaceCommand(ctx, cfg, []string{"rename", "--workspace", team, "--name", "Help desk"})
	},
		"workspace rename")
	run(func() error { return workspaceCommand(ctx, cfg, []string{"list"}) }, "workspace list")

	// A team invite, with a role in the team.
	run(func() error {
		return userCommand(ctx, cfg, []string{"invite", "--email", "carol@example.com", "--workspace", team, "--role", "admin"})
	}, "user invite --workspace")
	var role, workspaceID string
	if err := db.Reader().QueryRowContext(ctx, `SELECT workspace_role, coalesce(workspace_id, '') FROM invites
		WHERE email = 'carol@example.com'`).Scan(&role, &workspaceID); err != nil || role != "admin" || workspaceID != team {
		t.Errorf("the invite is %q into %q (%v)", role, workspaceID, err)
	}

	// Bea joins, and Ana links a mailbox into the team and lets her read it.
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		return ws.AddMemberTx(ctx, tx, team, bea.ID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	const shared = "acc_00000000000000aa"
	if _, err := account.NewRepository(db, nil).Create(ctx, account.Account{
		ID: shared, WorkspaceID: team, Email: "support@mail.example", Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "support@mail.example",
		State: account.StateActive,
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetGrant(ctx, shared, bea.ID, workspace.Flags{Read: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	grantOf := func() workspace.Flags {
		t.Helper()
		g, err := ws.Grant(ctx, shared, bea.ID)
		if err != nil {
			t.Fatal(err)
		}
		return g.Flags
	}

	if err := accessCommand(ctx, cfg, []string{"grant", "--account", shared, "--email", "bea@example.com"}); err == nil ||
		!strings.Contains(err.Error(), "--manage") {
		t.Errorf("a grant without --manage: %v", err)
	}
	run(func() error {
		return accessCommand(ctx, cfg, []string{"grant", "--account", shared, "--email", "bea@example.com", "--manage"})
	}, "access grant --manage")
	if got := grantOf(); got != (workspace.Flags{Read: true, Manage: true}) {
		t.Errorf("after the operator's grant Bea holds %+v: manage joins what she held", got)
	}
	run(func() error { return accessCommand(ctx, cfg, []string{"list", "--workspace", team}) }, "access list")
	run(func() error {
		return accessCommand(ctx, cfg, []string{"revoke", "--account", shared, "--email", "bea@example.com", "--read"})
	}, "access revoke --read")
	if got := grantOf(); got != (workspace.Flags{Manage: true}) {
		t.Errorf("after revoking read Bea holds %+v", got)
	}

	run(func() error {
		return memberCommand(ctx, cfg, []string{"role", "--workspace", team, "--email", "bea@example.com", "--role", "admin"})
	}, "member role")
	if m, err := ws.Member(ctx, team, bea.ID); err != nil || m.Role != workspace.RoleAdmin {
		t.Errorf("Bea's membership: %+v (%v)", m, err)
	}
	run(func() error { return memberCommand(ctx, cfg, []string{"list", "--workspace", team}) }, "member list")
	// Ana linked the mailbox: she stays while it is linked, and the
	// daemon's refusal says why.
	if err := memberCommand(ctx, cfg, []string{"remove", "--workspace", team, "--email", "ana@example.com"}); err == nil ||
		!strings.Contains(err.Error(), "conflict") {
		t.Errorf("removing the linker: %v", err)
	}
	run(func() error {
		return memberCommand(ctx, cfg, []string{"remove", "--workspace", team, "--email", "bea@example.com"})
	},
		"member remove")
	if _, err := ws.Grant(ctx, shared, bea.ID); err == nil {
		t.Error("Bea's grant survived her removal")
	}
}

func TestTheCommandLineClosesOnlyALocalTeam(t *testing.T) {
	cfg, db := realDaemon(t)
	ctx := t.Context()
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	team, err := ws.CreateTeam(ctx, "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A team whose source is not local, which closes where it is managed.
	const elsewhere = "0b4a3c5e-0000-4000-8000-000000000001"
	if _, err := db.Writer().ExecContext(ctx, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES (?, 'team', 'platform', 'Elsewhere', 500, 500)`, elsewhere); err != nil {
		t.Fatal(err)
	}
	closedAt := func(id string) time.Time {
		t.Helper()
		w, err := ws.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w.ClosedAt
	}
	refused := func(what string, err error, code string) {
		t.Helper()
		var failure *apiError
		if !errors.As(err, &failure) || failure.Code != code {
			t.Errorf("%s: %v, want %s", what, err, code)
		}
	}

	if err := workspaceCommand(ctx, cfg, []string{"close", "--workspace", team.ID}); err != nil {
		t.Fatalf("workspace close: %v", err)
	}
	if closedAt(team.ID).IsZero() {
		t.Error("the command line did not close the local team")
	}
	// Closing it again changes nothing, and says it is closed.
	if err := workspaceCommand(ctx, cfg, []string{"close", "--workspace", team.ID}); err != nil {
		t.Errorf("closing a closed team: %v", err)
	}
	// The list says since when it is closed.
	row := func() string {
		t.Helper()
		out, err := captureStdout(t, func() error { return workspaceCommand(ctx, cfg, []string{"list"}) })
		if err != nil {
			t.Fatalf("workspace list: %v", err)
		}
		for line := range strings.Lines(out) {
			if strings.HasPrefix(line, team.ID+" ") {
				return strings.TrimSpace(line)
			}
		}
		t.Fatalf("workspace list leaves the team out:\n%s", out)
		return ""
	}
	if got, want := row(), "closed since "+closedAt(team.ID).UTC().Format(time.RFC3339); !strings.HasSuffix(got, want) {
		t.Errorf("workspace list shows the closed team as %q, want it to end %q", got, want)
	}
	if err := workspaceCommand(ctx, cfg, []string{"reopen", "--workspace", team.ID}); err != nil {
		t.Fatalf("workspace reopen: %v", err)
	}
	if !closedAt(team.ID).IsZero() {
		t.Error("the command line did not reopen the team")
	}
	if fields := strings.Fields(row()); fields[len(fields)-1] != "-" || fields[len(fields)-2] != "local" {
		t.Errorf("workspace list shows the reopened team as %q, with a note", fields)
	}

	refused("closing a team from another source",
		workspaceCommand(ctx, cfg, []string{"close", "--workspace", elsewhere}), "conflict")
	if !closedAt(elsewhere).IsZero() {
		t.Error("the command line closed a team from another source")
	}
	refused("closing a personal workspace",
		workspaceCommand(ctx, cfg, []string{"close", "--workspace", authtest.Personal(t, db, ana.ID)}), "bad_request")
	refused("closing the operator workspace",
		workspaceCommand(ctx, cfg, []string{"close", "--workspace", workspace.OperatorID}), "bad_request")
	refused("closing a workspace nobody knows",
		workspaceCommand(ctx, cfg, []string{"close", "--workspace", "wsp_0000000000000000"}), "not_found")
	if err := workspaceCommand(ctx, cfg, []string{"close"}); err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("close without --workspace: %v", err)
	}
}
