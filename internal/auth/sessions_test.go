package auth_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

// A sign-out everywhere is recorded (users.signed_out_everywhere_at,
// migration 0027): the person's own act, which an extension reports.

// signedOutEverywhereAt is what the users row records of a person's last
// sign-out everywhere, unix seconds.
func signedOutEverywhereAt(t *testing.T, db *store.Store, userID string) int64 {
	t.Helper()
	var at int64
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, userID).
		Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestSigningOutEverywhereRecordsItsMomentInTheSameWrite(t *testing.T) {
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	if at := signedOutEverywhereAt(t, db, ana.ID); at != 0 {
		t.Fatalf("a person who never signed out everywhere has it recorded at %d", at)
	}
	*clock = clock.Add(time.Hour)
	if err := users.EndAllSessions(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	// The moment its sessions were revoked at, read back with them.
	var revoked int64
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT revoked_at FROM sessions WHERE user_id = ?`, ana.ID).
		Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if at := signedOutEverywhereAt(t, db, ana.ID); at != clock.Unix() || at != revoked {
		t.Errorf("the sign-out everywhere is recorded at %d, its sessions revoked at %d, want both %d", at, revoked,
			clock.Unix())
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); err == nil {
		t.Error("a session survived signing out everywhere")
	}
	// A write that fails records nothing: the moment and the sessions go
	// together.
	*clock = clock.Add(time.Hour)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	authtest.SignIn(t, users, "bea@example.com")
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TEMP TRIGGER refuse_sign_out BEFORE UPDATE OF signed_out_everywhere_at
			ON users BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := users.EndAllSessions(t.Context(), bea.ID); err == nil {
		t.Fatal("a sign-out everywhere whose record was refused succeeded")
	}
	if n := countRows(t, db, `SELECT count(*) FROM sessions WHERE user_id = ? AND revoked_at <> 0`, bea.ID); n != 0 {
		t.Errorf("a sign-out everywhere whose record was refused revoked %d sessions", n)
	}
}

func TestSigningOutOfOneSessionOrChangingThePasswordRecordsNoSignOutEverywhere(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	phone := authtest.SignIn(t, users, "ana@example.com")
	*clock = clock.Add(time.Minute)

	if err := users.EndSession(t.Context(), sessionOf(t, users, phone)); err != nil {
		t.Fatal(err)
	}
	sid := sessionOf(t, users, laptop)
	begun, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, authtest.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, auth.NewPassword{
		Ticket: begun.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: secretOf("brand new"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t),
	}, "test")
	if err != nil || !changed.Rotated {
		t.Fatalf("the password change: %+v, %v", changed, err)
	}
	// A directory's revocation ends sessions too, and is not the person's.
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.RevokeSessionsStartedBeforeTx(t.Context(), tx, ana.ID, clock.Add(time.Hour).UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM sessions WHERE user_id = ? AND revoked_at = 0`, ana.ID); n != 0 {
		t.Fatalf("%d sessions are still live", n)
	}
	if at := signedOutEverywhereAt(t, db, ana.ID); at != 0 {
		t.Errorf("ending sessions otherwise recorded a sign-out everywhere at %d", at)
	}
}

// countRows is a count a query answers.
func countRows(t *testing.T, db *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
