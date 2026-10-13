package auth_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Suspending a person, for an extension of the daemon, and how a disable
// here stands over it (docs/key-scheme.md section 12.13).

// personState is what the users row says of a person: their status, and
// whether they are suspended.
func personState(t *testing.T, users *auth.Users, id string) (disabled, suspended bool) {
	t.Helper()
	user, err := users.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return user.Disabled, user.Suspended
}

// stopReason reads why a key stopped: "" while it works.
func stopReason(t *testing.T, db *store.Store, presented string) string {
	t.Helper()
	var reason string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT CASE revoked_at WHEN 0 THEN '' ELSE stop_reason END
		FROM api_keys WHERE prefix = ?`, authtest.Prefix(presented)).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestADisableHereStandsOverASuspension(t *testing.T) {
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)
	authtest.KeyMailbox(t, db, mailbox, ana.ID)
	grantRead(t, db, mailbox, bea.ID)

	suspended, err := users.Suspend(t.Context(), bea.ID)
	if err != nil || !suspended.Suspended {
		t.Fatalf("suspending Bea: %+v, %v", suspended, err)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, bea.ID); n != 1 {
		t.Fatalf("suspended, Bea holds %d grants, want hers kept", n)
	}
	// Switching her on here is not a resume.
	if err := users.SetDisabled(t.Context(), bea.ID, false); !errors.Is(err, auth.ErrUserSuspended) {
		t.Errorf("switching a suspended person on here: %v, want ErrUserSuspended", err)
	}
	if disabled, isSuspended := personState(t, users, bea.ID); !disabled || !isSuspended {
		t.Errorf("after the refused switch Bea is disabled %v, suspended %v", disabled, isSuspended)
	}

	// Disabled here while suspended: her grants go, and so does the mark.
	if _, err := users.Disable(t.Context(), bea.ID, false); err != nil {
		t.Fatalf("disabling a suspended person: %v", err)
	}
	if disabled, isSuspended := personState(t, users, bea.ID); !disabled || isSuspended {
		t.Errorf("disabled here, Bea is disabled %v, suspended %v", disabled, isSuspended)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("disabled here, Bea keeps %d grants", n)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_access WHERE user_id = ? AND read = 1`, bea.ID); n != 1 {
		t.Errorf("disabled here, Bea holds read on %d mailboxes, want the flag kept", n)
	}

	// A resume, whenever the extension sends it, lifts nothing.
	active, err := users.Resume(t.Context(), bea.ID)
	if err != nil || active {
		t.Fatalf("resuming someone disabled here: active %v, %v", active, err)
	}
	if disabled, isSuspended := personState(t, users, bea.ID); !disabled || isSuspended {
		t.Errorf("after the resume Bea is disabled %v, suspended %v", disabled, isSuspended)
	}
	// Only a switch here brings her back, waiting for the key.
	if err := users.SetDisabled(t.Context(), bea.ID, false); err != nil {
		t.Fatal(err)
	}
	if readsBy(t, db, mailbox, bea.ID) {
		t.Error("switched back on here, Bea reads without a grant")
	}
}

func TestSuspendingSomeoneDisabledHereChangesNothing(t *testing.T) {
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	if _, err := users.Disable(t.Context(), bea.ID, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Hour)
	row := func() string {
		t.Helper()
		var status string
		var suspended, updated int64
		if err := db.Reader().QueryRowContext(t.Context(), `SELECT status, suspended_at, updated_at FROM users WHERE id = ?`,
			bea.ID).Scan(&status, &suspended, &updated); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s %d %d %s", status, suspended, updated, userRow(t, db, bea.ID))
	}
	before := row()

	suspended, err := users.Suspend(t.Context(), bea.ID)
	if err != nil {
		t.Fatalf("suspending someone disabled here: %v", err)
	}
	if suspended.Suspended || suspended.Sessions != 0 || suspended.Keys != 0 || suspended.Invites != 0 {
		t.Errorf("suspending someone disabled here did %+v", suspended)
	}
	if after := row(); after != before {
		t.Errorf("suspending someone disabled here changed her row:\n%s\nis now\n%s", before, after)
	}
	if active, err := users.Resume(t.Context(), bea.ID); err != nil || active {
		t.Errorf("resuming her: active %v, %v", active, err)
	}
	if disabled, isSuspended := personState(t, users, bea.ID); !disabled || isSuspended {
		t.Errorf("Bea is disabled %v, suspended %v, want disabled here", disabled, isSuspended)
	}
	if _, err := users.Suspend(t.Context(), "usr_0000000000000000"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("suspending nobody: %v", err)
	}
	if _, err := users.Resume(t.Context(), "usr_0000000000000000"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("resuming nobody: %v", err)
	}
}

func TestAKeyStoppedBySuspensionSaysSo(t *testing.T) {
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, bea.ID)
	mine := authtest.NewWorkspaceKey(t, db, auth.ScopeRead, authtest.Personal(t, db, bea.ID), bea.ID)
	teams := authtest.NewWorkspaceKey(t, db, auth.ScopeRead, team.ID, bea.ID)
	stoppedBefore := authtest.NewWorkspaceKey(t, db, auth.ScopeRead, team.ID, bea.ID)
	if err := auth.NewKeysWithClock(db, func() time.Time { return *clock }).RevokeCreatedBy(t.Context(),
		authtest.Prefix(stoppedBefore), bea.ID); err != nil {
		t.Fatal(err)
	}

	suspended, err := users.Suspend(t.Context(), bea.ID)
	if err != nil || suspended.Keys != 2 {
		t.Fatalf("suspending Bea stopped %d keys (%v), want her two live ones", suspended.Keys, err)
	}
	want := map[string]string{mine: "creator_suspended", teams: "creator_suspended", stoppedBefore: "manual"}
	check := func(when string) {
		t.Helper()
		for key, reason := range want {
			if got := stopReason(t, db, key); got != reason {
				t.Errorf("%s, a key says it stopped for %q, want %q", when, got, reason)
			}
		}
	}
	check("suspended")
	// Suspended again, resumed, then disabled here: why each stopped stays.
	if again, err := users.Suspend(t.Context(), bea.ID); err != nil || again.Keys != 0 {
		t.Errorf("suspending her again stopped %d keys (%v)", again.Keys, err)
	}
	if active, err := users.Resume(t.Context(), bea.ID); err != nil || !active {
		t.Fatalf("resuming Bea: active %v, %v", active, err)
	}
	check("resumed")
	if _, err := users.Disable(t.Context(), bea.ID, true); err != nil {
		t.Fatal(err)
	}
	check("disabled here")
	if n := count(t, db, `SELECT count(*) FROM api_keys WHERE created_by = ? AND stop_reason = ?`,
		bea.ID, string(workspace.KeyStopCreatorDisabled)); n != 0 {
		t.Errorf("%d keys stopped by her suspension now say she was disabled", n)
	}
}
