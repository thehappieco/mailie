package auth_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func invitesFor(t *testing.T, db *store.Store, email string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM invites WHERE email = ?`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnUnusedInviteIsDeletedThirtyDaysAfterItExpires(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	start := *clock
	invite(t, users, "late@example.com", auth.RoleMember)
	used := invite(t, users, "used@example.com", auth.RoleMember)
	if _, _, _, err := users.SignUp(t.Context(), signUpRequest(t, used, "used@example.com")); err != nil {
		t.Fatal(err)
	}
	expired := start.Add(auth.InviteTTL)

	sweep := func(at time.Time, ahead time.Duration) int {
		t.Helper()
		*clock = at
		n, err := users.SweepInvites(t.Context(), ahead)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Expired, but not yet for thirty days: kept, so "my invite did not
	// work" still has an answer.
	if n := sweep(expired.Add(auth.InviteRetention), 0); n != 0 {
		t.Fatalf("an invite expired exactly thirty days ago was swept (%d)", n)
	}
	invite(t, users, "fresh@example.com", auth.RoleMember)
	if n := sweep(expired.Add(auth.InviteRetention+time.Second), 0); n != 1 {
		t.Fatalf("swept %d invites a second past the retention, want the one", n)
	}
	for email, want := range map[string]int{"late@example.com": 0, "used@example.com": 1, "fresh@example.com": 1} {
		if got := invitesFor(t, db, email); got != want {
			t.Errorf("%s: %d invites left, want %d", email, got, want)
		}
	}

	// A sweep that will not run again for an hour takes what would pass the
	// retention before then, so nothing outlives it by waiting.
	freshExpired := expired.Add(auth.InviteRetention).Add(auth.InviteTTL)
	if n := sweep(freshExpired.Add(auth.InviteRetention-30*time.Minute), time.Hour); n != 1 {
		t.Errorf("a sweep with an hour to its next run left an invite thirty minutes from its limit (%d)", n)
	}
}

func TestAPersonIsNotDeletedWhileAnAccountStillNamesThem(t *testing.T) {
	// Account ownership has no ON DELETE action on purpose: a deletion that
	// quietly turned someone's mailboxes into the instance's would hand them
	// to every owner. The accounts must go first, in the same transaction.
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	if _, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls,
		 login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		 VALUES ('acc_0000000000000001', ?, 'ana@mail.example', 'imap', 'password', 'h', 993, 'h', 465, 'implicit',
		 'ana', 1, 'active', 0, 0, 0, ?)`, authtest.Personal(t, db, ana.ID), ana.ID); err != nil {
		t.Fatal(err)
	}

	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, ana.ID, false)
		return err
	})
	if !errors.Is(err, auth.ErrOwnsAccounts) {
		t.Fatalf("DeleteTx with an account left: %v, want ErrOwnsAccounts", err)
	}
	if _, err := users.Get(t.Context(), ana.ID); err != nil {
		t.Fatalf("the refused deletion was not rolled back: %v", err)
	}
}

func TestAnotherPersonsUsedTeamInviteOutlivesTheTeamDeletedWithItsLastMember(t *testing.T) {
	// Ana's team brought Bea onto the server; Bea left it, and Ana, its last
	// member, is deleted, the team with her. Bea's invite is the record of
	// how she arrived, and stays until she goes; the one still waiting for
	// Cid goes with the team, and never becomes an instance invite.
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleOwner)
	team := teamOf(t, db, ana.ID)
	code, _ := teamInviteBy(t, users, ana.ID, "bea@example.org", team.ID, workspace.RoleMember)
	bea := signUp(t, users, code, "bea@example.org")
	teamInviteBy(t, users, ana.ID, "cid@example.org", team.ID, workspace.RoleMember)
	if err := workspace.NewRepository(db, nil).RemoveMember(t.Context(), team.ID, bea.ID, nil); err != nil {
		t.Fatal(err)
	}

	var removed auth.Removed
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		removed, err = users.DeleteTx(t.Context(), tx, ana.ID, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if removed.Teams != 1 {
		t.Fatalf("deleted %+v, want the team with its last member", removed)
	}
	var used, workspaceRole string
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*), coalesce(max(used_by), ''), coalesce(max(workspace_role), '')
		FROM invites WHERE email = 'bea@example.org' AND workspace_id IS NULL`).Scan(&n, &used, &workspaceRole); err != nil {
		t.Fatal(err)
	}
	if n != 1 || used != bea.ID || workspaceRole != string(workspace.RoleMember) {
		t.Errorf("Bea's used invite after the team went: %d row(s), used by %q, role %q", n, used, workspaceRole)
	}
	if n := invitesFor(t, db, "cid@example.org"); n != 0 {
		t.Errorf("%d invites still wait for Cid after the team went", n)
	}
	if pending, err := users.PendingInvites(t.Context(), ""); err != nil || len(pending) != 0 {
		t.Errorf("the instance's pending invites: %+v, %v", pending, err)
	}
	// And it goes with Bea, as every used invite goes with its person.
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, bea.ID, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := invitesFor(t, db, "bea@example.org"); n != 0 {
		t.Errorf("%d invites for Bea after she was deleted", n)
	}
}

func TestATeamDeletedByAnyWayTakesTheInvitesStillWaitingToJoinIt(t *testing.T) {
	// Whatever deletes a team, the schema does not let an unused invite to
	// it outlive it, where it would read as an instance invite.
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleOwner)
	team := teamOf(t, db, ana.ID)
	teamInvite(t, users, "cid@example.org", team.ID, workspace.RoleAdmin)
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM workspaces WHERE id = ?`, team.ID); err != nil {
		t.Fatal(err)
	}
	if n := invitesFor(t, db, "cid@example.org"); n != 0 {
		t.Errorf("%d unused invites outlived their team", n)
	}
}

func TestDeletingAPersonTakesTheirWorkspacesKeysAndLeavesATeamsRevokedWithoutTheirName(t *testing.T) {
	// The keys of Ana's personal workspace go with it; the one she created
	// in a team that stays is the team's record, revoked, without her name;
	// what she gave a key someone else created stays, without her name.
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, bea.ID)
	ws := workspace.NewRepository(db, nil)
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return ws.AddMemberTx(t.Context(), tx, team.ID, ana.ID, workspace.RoleAdmin, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	const shared = "acc_00000000000000aa"
	linkMailbox(t, db, shared, team.ID, bea.ID, "support@mail.example")
	if _, err := ws.SetGrant(t.Context(), shared, ana.ID, workspace.Flags{Read: true}, bea.ID, nil); err != nil {
		t.Fatal(err)
	}
	own := authtest.Prefix(authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, ana.ID), ana.ID))
	teams := authtest.Prefix(authtest.NewWorkspaceKey(t, db, auth.ScopeRead, team.ID, ana.ID))
	beas := authtest.Prefix(authtest.NewWorkspaceKey(t, db, auth.ScopeRead, team.ID, bea.ID))
	if _, err := ws.SetKeyAccess(t.Context(), beas, shared, workspace.Flags{Read: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}

	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, ana.ID, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM api_keys WHERE prefix = ?`, own).Scan(&n); err != nil || n != 0 {
		t.Errorf("the key of Ana's personal workspace is still there (%d, %v)", n, err)
	}
	var revoked int64
	var by string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT revoked_at, created_by FROM api_keys WHERE prefix = ?`,
		teams).Scan(&revoked, &by); err != nil || revoked == 0 || by != "" {
		t.Errorf("the key Ana created in the team: revoked at %d, created by %q (%v)", revoked, by, err)
	}
	var given string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT granted_by FROM key_access WHERE key_prefix = ?`,
		beas).Scan(&given); err != nil || given != "" {
		t.Errorf("what Ana gave Bea's key names %q (%v)", given, err)
	}
}
