package service_test

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

// An edition whose API keys belong to personal workspaces alone
// (Deps.KeysInPersonalWorkspaceOnly): a team gets no key and none gets more
// of a team, while what a database already holds is still listed, taken
// away and revoked. Unset, a team's owners and admins create its keys
// (TestOnlyAnOwnerOrAdminSignedInCreatesAWorkspacesKeys).

// keepKeysPersonal restarts the fixture's daemon, over the same database, as
// an edition whose keys are personal workspaces' alone: what an edition that
// gave teams keys made is still there.
func (f *fixture) keepKeysPersonal(t *testing.T) {
	t.Helper()
	f.opts.keysPersonalOnly = true
	f.svc, f.registry = f.build(t, f.opts.registry)
}

// wantTeamKeysRefused fails the test unless err is the refusal of a team's
// key where keys are personal workspaces' alone.
func wantTeamKeysRefused(t *testing.T, what string, err error) {
	t.Helper()
	if service.CodeOf(err) != service.CodeNotAuthorized ||
		!strings.Contains(service.MessageOf(err), "belong to a personal workspace only") {
		t.Errorf("%s = %v, want the not_authorized of a server whose keys are personal workspaces' alone", what, err)
	}
}

func TestAnEditionWhoseKeysArePersonalOnlyCreatesNoKeyInATeamAndGivesNoneMoreThere(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	// Where the option is unset, Ana, the team's owner, gives it a key.
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "write", reads(tm.shared)))
	if err != nil {
		t.Fatalf("creating a team's key where keys are a team's too: %v", err)
	}
	keys := `SELECT count(*) FROM api_keys WHERE workspace_id = ?`
	before := f.count(t, keys, tm.id)

	f.keepKeysPersonal(t)

	// No key is created in the team, by its owner or its admin.
	for name, p := range map[string]service.Principal{"Ana, its owner": tm.ana, "Carol, an admin": tm.carol} {
		_, err := f.svc.CreateWorkspaceKey(ctx, p, tm.id, keyRequest(name, "read"))
		wantTeamKeysRefused(t, name+" creating a team's key", err)
	}
	// A member and someone outside it are refused as before.
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.bea, tm.id, keyRequest("bea's", "read"))
	wantCode(t, "a member creating a team's key", err, service.CodeNotAuthorized)
	outsider := f.person(t, "out@example.com", auth.RoleOwner)
	_, err = f.svc.CreateWorkspaceKey(ctx, outsider, tm.id, keyRequest("out", "read"))
	wantCode(t, "someone outside the team", err, service.CodeNotFound)
	if n := f.count(t, keys, tm.id); n != before {
		t.Errorf("the team holds %d keys, want the %d it had", n, before)
	}

	// The key the team had gets nothing more on its mailbox.
	_, err = f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, true, false))
	wantTeamKeysRefused(t, "raising what the team's key holds", err)
	if n := f.count(t, `SELECT act FROM key_access WHERE key_prefix = ? AND account_id = ?`, created.Prefix, tm.shared); n != 0 {
		t.Error("the team's key was given act")
	}
	// What it holds is listed, set to what it already holds, taken away, and
	// the key revoked.
	listed, err := f.svc.ListWorkspaceKeys(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 1 || listed[0].Prefix != created.Prefix {
		t.Fatalf("listing the team's keys: %+v, %v", listed, err)
	}
	if _, err := f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, false, false)); err != nil {
		t.Errorf("setting what the team's key already holds: %v", err)
	}
	if err := f.svc.RevokeKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared); err != nil {
		t.Errorf("taking the mailbox out of the team's key: %v", err)
	}
	if err := f.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, created.Prefix); err != nil {
		t.Errorf("revoking the team's key: %v", err)
	}

	// A personal workspace's keys are as they were.
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	mine, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, authtest.Personal(t, f.db, tm.ana.UserID),
		keyRequest("ana's own", "write", reads(own)))
	if err != nil {
		t.Fatalf("Ana creating a key in her personal workspace: %v", err)
	}
	if _, err := f.svc.SetKeyAccess(ctx, tm.ana, authtest.Personal(t, f.db, tm.ana.UserID), mine.Prefix, own,
		keyAccess(true, true, false)); err != nil {
		t.Errorf("Ana giving her own key act on her own mailbox: %v", err)
	}
}
