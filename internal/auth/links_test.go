package auth_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

// A link at the provider (migration 0026) ends with whatever ends the
// sessions it was started with.

// seedLink writes a link at the provider a person started at the moment
// given, into their personal workspace, waiting for its return.
func seedLink(t *testing.T, db *store.Store, id, owner string, at time.Time) {
	t.Helper()
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO oauth_links(id, state, owner_user_id, provider, oauth_client,
			pkce_verifier, redirect_uri, created_at, expires_at) VALUES (?, 'state-' || ?, ?, 'gmail', 'web', 'verifier',
			'https://mail.example.test/oauth/return', ?, ?)`, id, id, owner, at.Unix(), at.Add(10*time.Minute).Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// links lists the links left, by id.
func links(t *testing.T, db *store.Store) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT id FROM oauth_links ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestARecoveryAResetAndSigningOutEverywhereEndThePersonsLinks(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	seedLink(t, db, "lnk_bea", bea.ID, *clock)
	again := func(what string) {
		t.Helper()
		seedLink(t, db, "lnk_ana_"+what, ana.ID, *clock)
	}

	again("signout")
	if err := users.EndAllSessions(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	if got := links(t, db); !slices.Equal(got, []string{"lnk_bea"}) {
		t.Errorf("after signing out everywhere: %v", got)
	}

	again("recovery")
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.FinishRecovery(t.Context(), auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: secretOf("after recovery"),
		KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("new code"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := links(t, db); !slices.Equal(got, []string{"lnk_bea"}) {
		t.Errorf("after a recovery: %v", got)
	}

	again("reset")
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	in := authtest.EnrolmentWith(t, secretOf("after reset"), secretOf("another code"))
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", in, "test"); err != nil {
		t.Fatal(err)
	}
	if got := links(t, db); !slices.Equal(got, []string{"lnk_bea"}) {
		t.Errorf("after a reset: %v", got)
	}

	// A password change keeps them: the person proved the password, and
	// signs in on with the session it opens.
	again("change")
	login, err := users.Login(t.Context(), "ana@example.com", secretOf("after reset"), "test")
	if err != nil {
		t.Fatal(err)
	}
	sid := sessionOf(t, users, login.Token)
	begun, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, secretOf("after reset"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, auth.NewPassword{
		Ticket: begun.Ticket, CurrentAuthKey: secretOf("after reset"), AuthKey: secretOf("changed"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t),
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if got := links(t, db); !slices.Equal(got, []string{"lnk_ana_change", "lnk_bea"}) {
		t.Errorf("after a password change: %v", got)
	}
}

func TestADirectorysRevocationEndsTheLinksStartedByItsMoment(t *testing.T) {
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	moment := clock.Truncate(time.Second)
	seedLink(t, db, "lnk_before", ana.ID, moment.Add(-time.Minute))
	// Within the moment's second, as a session started then is ended.
	seedLink(t, db, "lnk_at", ana.ID, moment)
	seedLink(t, db, "lnk_after", ana.ID, moment.Add(time.Second))
	seedLink(t, db, "lnk_bea", bea.ID, moment.Add(-time.Minute))
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.RevokeSessionsStartedBeforeTx(t.Context(), tx, ana.ID, moment.Add(300*time.Millisecond).UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := links(t, db); !slices.Equal(got, []string{"lnk_after", "lnk_bea"}) {
		t.Errorf("after the directory's revocation the links are %v", got)
	}
}

func TestAStepUpThatLapsesWithinTheMarginIsNotFreshForAnActThatEndsThen(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	const margin = 45 * time.Second

	*clock = clock.Add(auth.StepUpWindow - margin)
	if err := users.RequireStepUpFor(t.Context(), ana.ID, sid, margin); err != nil {
		t.Errorf("fresh until exactly the margin's end: %v", err)
	}
	*clock = clock.Add(time.Second)
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); err != nil {
		t.Errorf("fresh now: %v", err)
	}
	if err := users.RequireStepUpFor(t.Context(), ana.ID, sid, margin); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Errorf("lapsing within the margin: %v, want ErrStepUpNeeded", err)
	}
	if err := users.RequireStepUpFor(t.Context(), ana.ID, sid, 0); err != nil {
		t.Errorf("with no margin, as RequireStepUp: %v", err)
	}
	// It proves the session's own person, or nobody.
	bea, err := users.GetByEmail(t.Context(), "bea@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.RequireStepUpFor(t.Context(), bea.ID, sid, margin); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("another person's session: %v, want ErrInvalidSession", err)
	}
	if err := users.EndAllSessions(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	if err := users.RequireStepUpFor(t.Context(), ana.ID, sid, margin); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a session ended: %v, want ErrInvalidSession", err)
	}
}
