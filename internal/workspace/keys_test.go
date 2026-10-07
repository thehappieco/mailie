package workspace_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// key makes a key of a workspace, created by createdBy, holding what grants
// name, and returns its prefix.
func (f *fixture) key(scope auth.Scope, workspaceID, createdBy string, grants ...workspace.KeyGrant) string {
	f.t.Helper()
	return authtest.Prefix(authtest.NewWorkspaceKey(f.t, f.db, scope, workspaceID, createdBy, grants...))
}

func (f *fixture) revokedAt(prefix string) int {
	f.t.Helper()
	return f.count(`SELECT revoked_at FROM api_keys WHERE prefix = ?`, prefix)
}

func (f *fixture) holds(prefix, accountID string) workspace.Flags {
	f.t.Helper()
	held, err := f.ws.KeyAccessOf(f.t.Context(), prefix, []string{accountID})
	if err != nil {
		f.t.Fatal(err)
	}
	return held[accountID]
}

func TestRemovingAMemberRevokesTheKeysTheyCreatedThereAndNoOthers(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(shared.ID, bea.ID, readAct())
	ctx := t.Context()

	beasTeamKey := f.key(auth.ScopeRead, team.ID, bea.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()})
	beasOwnKey := f.key(auth.ScopeRead, f.personal(bea.ID), bea.ID)
	anasTeamKey := f.key(auth.ScopeRead, team.ID, ana.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()})

	if err := f.ws.RemoveMember(ctx, team.ID, bea.ID, nil); err != nil {
		t.Fatal(err)
	}
	if f.revokedAt(beasTeamKey) == 0 {
		t.Error("the key Bea created in the team she left still works")
	}
	if f.revokedAt(beasOwnKey) != 0 {
		t.Error("leaving a team revoked Bea's key of her personal workspace")
	}
	if f.revokedAt(anasTeamKey) != 0 || !f.holds(anasTeamKey, shared.ID).Read {
		t.Error("Bea leaving took something from a key Ana created")
	}
}

func TestDemotingOrDisablingAMembershipKeepsTheKeysThePersonCreated(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(shared.ID, bea.ID, readAct())
	ctx := t.Context()
	k := f.key(auth.ScopeRead, team.ID, bea.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()})

	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil); err != nil {
		t.Fatal(err)
	}
	if f.revokedAt(k) != 0 || !f.holds(k, shared.ID).Read {
		t.Error("a demotion or a disabled membership took the key its person created")
	}
}

func TestAKeyKeepsItsMailboxWhenWhoeverGaveItLosesRead(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(shared.ID, bea.ID, readAct())
	k := f.key(auth.ScopeWrite, team.ID, bea.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readAct()})

	if _, err := f.ws.Revoke(t.Context(), shared.ID, bea.ID, workspace.Flags{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.holds(k, shared.ID); got != readAct() {
		t.Errorf("the key holds %+v after its creator lost read, want read and act still", got)
	}
}

func TestWhatAKeyHoldsStaysInItsWorkspaceAndWithinItsScope(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	team := f.team("Support", ana, nil)
	other := f.team("Ops", ana, nil)
	shared := f.link(team.ID, ana.ID, "support@example.org")
	elsewhere := f.link(other.ID, ana.ID, "ops@example.org")
	ctx := t.Context()
	reader := f.key(auth.ScopeRead, team.ID, ana.ID)
	sender := f.key(auth.ScopeSend, team.ID, ana.ID)

	set := func(prefix, accountID string, flags workspace.Flags) error {
		_, err := f.ws.SetKeyAccess(ctx, prefix, accountID, flags, ana.ID, nil)
		return err
	}
	want(t, "a mailbox of another workspace", set(reader, elsewhere.ID, readOnly()), workspace.ErrNoMailbox)
	want(t, "act on a read key", set(reader, shared.ID, readAct()), workspace.ErrKeyScope)
	want(t, "send on a read key", set(reader, shared.ID, workspace.Flags{Read: true, Send: true}), workspace.ErrKeyScope)
	want(t, "act without read", set(sender, shared.ID, workspace.Flags{Act: true}), workspace.ErrActWithoutRead)
	want(t, "nothing", set(sender, shared.ID, workspace.Flags{}), workspace.ErrNoFlags)
	want(t, "manage", set(sender, shared.ID, workspace.Flags{Read: true, Manage: true}), workspace.ErrManageByRole)
	want(t, "an operator key", set(authtest.Prefix(authtest.NewKey(t, f.db, auth.ScopeSend)), shared.ID, readOnly()),
		workspace.ErrNoKey)
	if err := set(sender, shared.ID, workspace.Flags{Send: true}); err != nil {
		t.Errorf("send alone on a send key: %v", err)
	}
	if err := set(sender, shared.ID, workspace.Flags{Read: true, Act: true, Send: true}); err != nil {
		t.Errorf("every flag on a send key: %v", err)
	}
	if err := f.ws.DropKeyAccess(ctx, sender, shared.ID, nil); err != nil {
		t.Fatal(err)
	}
	want(t, "taking out a mailbox the key no longer holds", f.ws.DropKeyAccess(ctx, sender, shared.ID, nil),
		workspace.ErrNoKeyAccess)

	// A key revoked, or expired, is given nothing more.
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE api_keys SET revoked_at = 1 WHERE prefix = ?`, reader); err != nil {
		t.Fatal(err)
	}
	want(t, "a revoked key", set(reader, shared.ID, readOnly()), workspace.ErrKeyNotLive)
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE prefix = ?`,
		time.Now().Add(-time.Hour).Unix(), sender); err != nil {
		t.Fatal(err)
	}
	want(t, "an expired key", set(sender, shared.ID, readOnly()), workspace.ErrKeyNotLive)
}

func TestACarriedOverKeyGainsNothingAndIsRevokedWithItsLastMailbox(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	team := f.team("Support", ana, nil)
	shared := f.link(team.ID, ana.ID, "support@example.org")
	billing := f.link(team.ID, ana.ID, "billing@example.org")
	own := f.link(f.personal(ana.ID), ana.ID, "ana@gmail.com")
	ctx := t.Context()
	// As migration 0012 leaves one: no workspace, mailboxes of two. The
	// schema refuses both moving a key and a carried-over key gaining a
	// mailbox, so the scene is set past those two triggers, which come back
	// as 0012 made them.
	k := f.key(auth.ScopeWrite, team.ID, ana.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readAct()})
	err := f.db.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TRIGGER api_keys_workspace_fixed`,
			`DROP TRIGGER key_access_own_workspace`,
			`UPDATE api_keys SET workspace_id = NULL, origin = 'person-all' WHERE prefix = '` + k + `'`,
			`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, created_at, updated_at)
			 SELECT '` + k + `', id, workspace_id, 1, 1, 1 FROM accounts WHERE id = '` + own.ID + `'`,
			`CREATE TRIGGER api_keys_workspace_fixed BEFORE UPDATE OF workspace_id ON api_keys
			 WHEN NEW.workspace_id IS NOT OLD.workspace_id
			 BEGIN SELECT RAISE(ABORT, 'a key never moves to another workspace'); END`,
			`CREATE TRIGGER key_access_own_workspace BEFORE INSERT ON key_access
			 WHEN NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.prefix = NEW.key_prefix AND k.workspace_id = NEW.workspace_id
			                    AND k.workspace_id <> 'wsp_operator')
			 BEGIN SELECT RAISE(ABORT, 'a key holds mailboxes of its own workspace only'); END`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("setting the scene: %v", err)
	}
	_, err = f.ws.SetKeyAccess(ctx, k, billing.ID, readOnly(), ana.ID, nil)
	want(t, "a new mailbox", err, workspace.ErrCarriedOver)
	_, err = f.ws.SetKeyAccess(ctx, k, own.ID, readAct(), ana.ID, nil)
	want(t, "a new flag", err, workspace.ErrCarriedOver)
	if _, err := f.ws.SetKeyAccess(ctx, k, shared.ID, readOnly(), ana.ID, nil); err != nil {
		t.Errorf("taking act away: %v", err)
	}
	if err := f.ws.DropKeyAccess(ctx, k, shared.ID, nil); err != nil {
		t.Fatal(err)
	}
	if f.revokedAt(k) != 0 {
		t.Error("revoked with a mailbox left")
	}
	if err := f.ws.DropKeyAccess(ctx, k, own.ID, nil); err != nil {
		t.Fatal(err)
	}
	if f.revokedAt(k) == 0 {
		t.Error("a carried-over key left with no mailbox still works")
	}
}
