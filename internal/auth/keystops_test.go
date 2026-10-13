package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestRevokingKeysCreatedBeforeATimeLeavesTheLaterOnes(t *testing.T) {
	keys, db, clock := newKeys(t)
	ctx := t.Context()
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	own := authtest.Personal(t, db, ana.ID)
	start := *clock
	at := func(after time.Duration, workspaceID, createdBy string) string {
		t.Helper()
		*clock = start.Add(after)
		_, key := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeRead, WorkspaceID: workspaceID, CreatedBy: createdBy,
			TermsVersion: "terms"})
		return key.Prefix
	}
	// Before the moment: two of Ana's keys, in two workspaces, and one she
	// revoked herself; Bea's; and an instance key. At and after it: two
	// more of Ana's.
	ownBefore := at(0, own, ana.ID)
	teamBefore := at(time.Minute, team.ID, ana.ID)
	revoked := at(2*time.Minute, own, ana.ID)
	if err := keys.RevokeCreatedBy(ctx, revoked, ana.ID); err != nil {
		t.Fatal(err)
	}
	beas := at(3*time.Minute, authtest.Personal(t, db, bea.ID), bea.ID)
	_, instance := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeAdmin, CreatedBy: "cli"})
	moment := start.Add(10 * time.Minute)
	atMoment := at(10*time.Minute, team.ID, ana.ID)
	after := at(20*time.Minute, own, ana.ID)
	*clock = start.Add(30 * time.Minute)

	for _, reason := range []auth.KeyStopReason{"", "expired", "MANUAL"} {
		if n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, reason); !errors.Is(err, auth.ErrStopReason) || n != 0 {
			t.Errorf("the reason %q: %d keys, %v", reason, n, err)
		}
	}
	if n, err := keys.RevokeCreatedBefore(ctx, "", moment, workspace.KeyStopAccountRecovered); err != nil || n != 0 {
		t.Errorf("nobody's keys: %d keys, %v", n, err)
	}
	n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, workspace.KeyStopAccountRecovered)
	if err != nil || n != 2 {
		t.Fatalf("revoked %d keys (%v), want Ana's two live ones from before the moment", n, err)
	}

	want := map[string]struct {
		live   bool
		reason workspace.KeyStopReason
	}{
		ownBefore:       {false, workspace.KeyStopAccountRecovered},
		teamBefore:      {false, workspace.KeyStopAccountRecovered},
		revoked:         {false, workspace.KeyStopManual},
		beas:            {true, ""},
		instance.Prefix: {true, ""},
		atMoment:        {true, ""},
		after:           {true, ""},
	}
	for prefix, w := range want {
		key, err := keys.Get(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if key.Live(*clock) != w.live || key.StopReason != w.reason {
			t.Errorf("key %s created %s: live %v for %q, want live %v for %q", prefix,
				key.CreatedAt.Sub(start), key.Live(*clock), key.StopReason, w.live, w.reason)
		}
		if !w.live && prefix != revoked && !key.RevokedAt.Equal(*clock) {
			t.Errorf("key %s was revoked at %s, want now", prefix, key.RevokedAt)
		}
	}
	// Again: nothing left to stop.
	if n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, workspace.KeyStopAccountRecovered); err != nil || n != 0 {
		t.Errorf("again: %d keys, %v", n, err)
	}
}

// personsKeys are the keys a ceremony of Ana's may stop: two live keys she
// created, in her personal workspace and in a team, and one she revoked
// herself; and two that nothing of hers stops, Bea's and an instance key.
type personsKeys struct {
	db      *store.Store
	hers    []string
	revoked string
	others  []string
}

func newPersonsKeys(t *testing.T, db *store.Store, ana string) personsKeys {
	t.Helper()
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	k := personsKeys{db: db}
	k.hers = []string{
		authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, ana), ana),
		authtest.NewWorkspaceKey(t, db, auth.ScopeRead, teamOf(t, db, ana).ID, ana),
	}
	k.revoked = authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, ana), ana)
	if err := auth.NewKeys(db).RevokeCreatedBy(t.Context(), authtest.Prefix(k.revoked), ana); err != nil {
		t.Fatal(err)
	}
	k.others = []string{
		authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, bea.ID), bea.ID),
		authtest.NewKey(t, db, auth.ScopeAdmin),
	}
	return k
}

// check holds Ana's live keys to having stopped for reason, at the moment
// given ("" and the zero time: still live), and every other key to what it
// was.
func (k personsKeys) check(t *testing.T, when string, reason workspace.KeyStopReason, at time.Time) {
	t.Helper()
	for _, key := range k.hers {
		if got := stopReason(t, k.db, key); got != string(reason) {
			t.Errorf("%s, a key Ana created says it stopped for %q, want %q", when, got, reason)
		}
		var revokedAt int64
		if err := k.db.Reader().QueryRowContext(t.Context(), `SELECT revoked_at FROM api_keys WHERE prefix = ?`,
			authtest.Prefix(key)).Scan(&revokedAt); err != nil {
			t.Fatal(err)
		}
		if want := unixOrZero(at); revokedAt != want {
			t.Errorf("%s, a key Ana created was revoked at %d, want %d", when, revokedAt, want)
		}
	}
	if got := stopReason(t, k.db, k.revoked); got != string(workspace.KeyStopManual) {
		t.Errorf("%s, the key Ana revoked herself says it stopped for %q", when, got)
	}
	for _, key := range k.others {
		if got := stopReason(t, k.db, key); got != "" {
			t.Errorf("%s, a key Ana did not create stopped for %q", when, got)
		}
	}
}

func unixOrZero(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()
}

func TestARecoveryStopsTheKeysThePersonCreated(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	keys := newPersonsKeys(t, db, ana.ID)

	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	finish := auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: secretOf("not the code"), AuthKey: secretOf("after recovery"),
		KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("new code"),
	}
	if err := users.FinishRecovery(t.Context(), finish); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("a recovery finished with another proof: %v", err)
	}
	keys.check(t, "opened, and refused once", "", time.Time{})

	*clock = clock.Add(time.Minute)
	finish.CurrentRecoveryProof = authtest.RecoveryProof
	if err := users.FinishRecovery(t.Context(), finish); err != nil {
		t.Fatal(err)
	}
	keys.check(t, "recovered", workspace.KeyStopAccountRecovered, *clock)

	// Signed in with the new password, she creates keys again; a second
	// recovery stops those too, and leaves why the first ones stopped.
	if _, err := users.Login(t.Context(), "ana@example.com", finish.AuthKey, "test"); err != nil {
		t.Fatal(err)
	}
	again := authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, ana.ID), ana.ID)
	opened, err = users.OpenRecovery(t.Context(), "ana@example.com", finish.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	if err := users.FinishRecovery(t.Context(), auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: finish.RecoveryProof, AuthKey: secretOf("again"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("another code"),
	}); err != nil {
		t.Fatal(err)
	}
	keys.check(t, "recovered twice", workspace.KeyStopAccountRecovered, clock.Add(-time.Minute))
	if got := stopReason(t, db, again); got != string(workspace.KeyStopAccountRecovered) {
		t.Errorf("a key created between two recoveries says it stopped for %q", got)
	}
}

func TestAResetStopsTheKeysThePersonCreated(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	keys := newPersonsKeys(t, db, ana.ID)

	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	keys.check(t, "with a reset invitation made", "", time.Time{})
	in := authtest.EnrolmentWith(t, secretOf("after reset"), secretOf("new code"))
	if _, _, _, err := users.CompleteReset(t.Context(), code, "bea@example.com", in, "test"); !errors.Is(err, auth.ErrResetInvalid) {
		t.Fatalf("a reset for another address: %v", err)
	}
	keys.check(t, "with a reset refused", "", time.Time{})

	*clock = clock.Add(time.Hour)
	token, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", in, "test")
	if err != nil {
		t.Fatal(err)
	}
	keys.check(t, "reset", workspace.KeyStopAccountRecovered, *clock)
	// The session the reset opened is the way to create keys again.
	if _, err := users.AuthenticateSession(t.Context(), token); err != nil {
		t.Errorf("the reset's own session does not work: %v", err)
	}
}

func TestAPasswordChangeKeepsThePersonsKeys(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	keys := newPersonsKeys(t, db, ana.ID)
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))

	begun, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, authtest.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	newKey := secretOf("brand new")
	changed, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, auth.NewPassword{
		Ticket: begun.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: newKey, KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t),
	}, "test")
	if err != nil || !changed.Rotated {
		t.Fatalf("finish = %+v, %v", changed, err)
	}
	keys.check(t, "after a password change", "", time.Time{})

	// Nor does replacing the recovery code, or signing out everywhere: each
	// ends what it ends, and the account was never recovered.
	if err := users.ReplaceRecovery(t.Context(), ana.ID, changed.Session.ID, newKey, authtest.Wrap(t), secretOf("new code")); err != nil {
		t.Fatal(err)
	}
	keys.check(t, "after the recovery code was replaced", "", time.Time{})
	if err := users.EndAllSessions(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	keys.check(t, "after signing out everywhere", "", time.Time{})
}
