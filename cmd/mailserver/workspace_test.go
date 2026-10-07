package main

import (
	"database/sql"
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
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0x7e
	}
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, keyring), account.RegistryOptions{SpoolDir: t.TempDir()})
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
		HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: authtest.NewKey(t, db, auth.ScopeAdmin, ""),
	}, db
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
