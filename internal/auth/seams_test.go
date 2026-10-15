package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

// What an extension that follows an external directory needs of people:
// forms of the closures that run inside its one write, the sign-in's step for
// a new person, and the strandings a forced closure journals.

// within bounds a call that would hang if it opened a write inside another
// one: the store has one writer.
func within(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// strandings lists the journal's strandings as "account cause actor lost".
func strandings(t *testing.T, db *store.Store) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT account_id || ' ' || cause || ' ' || actor || ' ' || lost_user
		FROM mailbox_events WHERE kind = 'stranded' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRevokingSessionsStartedBeforeAMomentSparesLaterSeconds(t *testing.T) {
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	base := clock.Truncate(time.Second)
	*clock = base
	before := authtest.SignIn(t, users, ana.Email)
	// Within the moment's second, but after it: a session starts at a whole
	// second, so it ends too.
	*clock = base.Add(700 * time.Millisecond)
	sameSecond := authtest.SignIn(t, users, ana.Email)
	*clock = base.Add(time.Second)
	later := authtest.SignIn(t, users, ana.Email)
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO auth_tickets(hash, user_id, session_id, purpose,
		kdf_salt, kdf_m, kdf_t, kdf_p, proof, created_at, expires_at)
		VALUES (randomblob(32), ?, NULL, 'recover', randomblob(16), 1, 1, 1, randomblob(32), 0, 99999999999)`, ana.ID); err != nil {
		t.Fatal(err)
	}

	moment := base.Add(250 * time.Millisecond)
	var ended int
	if err := db.Write(within(t), func(tx *sql.Tx) error {
		var err error
		ended, err = users.RevokeSessionsStartedBeforeTx(t.Context(), tx, ana.ID, moment.UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ended != 2 {
		t.Errorf("%d sessions ended, want the 2 that started in the moment's second", ended)
	}
	for name, token := range map[string]string{"before": before, "within the second": sameSecond} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("the session started %s still works: %v", name, err)
		}
	}
	if _, err := users.AuthenticateSession(t.Context(), later); err != nil {
		t.Errorf("the session started a second later ended: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM auth_tickets WHERE user_id = ?`, ana.ID); n != 0 {
		t.Errorf("%d tickets of a ceremony outlived the revocation", n)
	}
}

func TestANewPersonsCallbackRunsInTheTransactionThatCreatesThem(t *testing.T) {
	users, db, _ := newUsers(t)
	ctx := within(t)
	var created []string
	// The step an extension adds: it sees the person and their identity, and
	// what it writes commits with them.
	step := func(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error {
		var linked string
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?`,
			issuer, "subject-of-cy").Scan(&linked); err != nil || linked != userID {
			return errors.New("the identity is not linked to the new person yet")
		}
		created = append(created, userID)
		_, err := tx.ExecContext(ctx, `INSERT INTO extension_state(extension, key, value, updated_at)
			VALUES ('test', ?, x'01', ?)`, userID, now.Unix())
		return err
	}
	in := external("subject-of-cy", "cy@example.com")
	authtest.PinProductKey(t, users, issuer, in.Subject)
	in.WantsKey, in.ProductKeyID, in.PersonCreatedTx = true, authtest.ProductKeyID, step
	signed, err := users.SignInExternal(ctx, in)
	if err != nil || signed.Ticket == "" {
		t.Fatalf("Cy's first sign-in: %+v, %v", signed, err)
	}
	if !slices.Equal(created, []string{signed.User.ID}) {
		t.Fatalf("the step ran for %v, want Cy alone", created)
	}
	if n := count(t, db, `SELECT count(*) FROM extension_state WHERE key = ?`, signed.User.ID); n != 1 {
		t.Errorf("what the step wrote did not commit with Cy: %d rows", n)
	}

	// A step that refuses rolls the whole sign-in back, the person included.
	refusal := errors.New("not this one")
	dan := external("subject-of-dan", "dan@example.com")
	dan.PersonCreatedTx = func(context.Context, *sql.Tx, string, time.Time) error { return refusal }
	if _, err := users.SignInExternal(ctx, dan); !errors.Is(err, refusal) {
		t.Fatalf("a sign-in whose step refuses: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM users WHERE email = 'dan@example.com'`) +
		count(t, db, `SELECT count(*) FROM user_identities WHERE subject = 'subject-of-dan'`); n != 0 {
		t.Errorf("a refused step left %d rows of Dan", n)
	}

	// A linked identity creates nobody, and the step does not run.
	created = nil
	if _, err := users.SignInExternal(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Errorf("the step ran for a linked identity: %v", created)
	}
}

// strandedTeams is Ana's local team, with Bea and Cy, and a team from
// another source with Ana and Bea: Bea alone reads one mailbox of each, and
// a third with Cy.
func strandedTeams(t *testing.T, db *store.Store) (ana, bea auth.User, alone, elsewhere, shared string) {
	t.Helper()
	ana = authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea = authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	cy := authtest.NewUser(t, db, "cy@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	addMember(t, db, team.ID, cy.ID)
	const other = "0b4a3c5e-0000-4000-8000-0000000000f1"
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES (?, 'team', 'platform', 'Elsewhere', 100, 100)`, other); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ana.ID, bea.ID} {
		if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO workspace_members(workspace_id, user_id, role, status,
			created_at, updated_at) VALUES (?, ?, 'member', 'active', 100, 100)`, other, id); err != nil {
			t.Fatal(err)
		}
	}
	alone, shared, elsewhere = teamMailbox(t, db, team.ID, ana.ID), teamMailbox(t, db, team.ID, ana.ID),
		teamMailbox(t, db, other, ana.ID)
	for _, box := range []string{alone, elsewhere} {
		grantRead(t, db, box, bea.ID)
		revokeRead(t, db, box, ana.ID)
	}
	grantRead(t, db, shared, cy.ID)
	revokeRead(t, db, shared, ana.ID)
	grantRead(t, db, shared, bea.ID)
	return ana, bea, alone, elsewhere, shared
}

func TestAForcedDisableJournalsEveryMailboxItStrands(t *testing.T) {
	users, db, _ := newUsers(t)
	ana, bea, alone, elsewhere, _ := strandedTeams(t, db)

	var blocked *auth.BlockedError
	if _, err := users.DisableBy(t.Context(), bea.ID, false, ana.ID); !errors.As(err, &blocked) {
		t.Fatalf("disabling the last reader without force: %v", err)
	}
	if got := strandings(t, db); len(got) != 0 {
		t.Fatalf("a refused disable journaled %v", got)
	}
	ended, err := users.DisableBy(t.Context(), bea.ID, true, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range ended.Stranded {
		got = append(got, s.AccountID)
	}
	want := []string{alone, elsewhere}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the forced disable stranded %v, want %v", got, want)
	}
	rows := strandings(t, db)
	slices.Sort(rows)
	wantRows := []string{alone + " disabled " + ana.ID + " " + bea.ID, elsewhere + " disabled " + ana.ID + " " + bea.ID}
	slices.Sort(wantRows)
	if !slices.Equal(rows, wantRows) {
		t.Errorf("the journal holds %v, want %v", rows, wantRows)
	}
	if n := count(t, db, `SELECT count(*) FROM notices`); n != 0 {
		t.Errorf("a stranding gave %d notices", n)
	}
}

func TestAForcedResetJournalsEveryKeyedMailboxItStrandsWithItsIssuer(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	keyed, keyless := teamMailbox(t, db, team.ID, ana.ID), teamMailbox(t, db, team.ID, ana.ID)
	authtest.KeyMailbox(t, db, keyed, ana.ID)
	for _, box := range []string{keyed, keyless} {
		grantRead(t, db, box, bea.ID)
		revokeRead(t, db, box, ana.ID)
	}

	code, _, err := users.CreateReset(t.Context(), bea.ID, true, "key:abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, bea.Email, authtest.Enrolment(t), "test"); err != nil {
		t.Fatal(err)
	}
	// The keyless mailbox she reads by the flag, which a reset keeps.
	if got := strandings(t, db); !slices.Equal(got, []string{keyed + " reset key:abcd1234 " + bea.ID}) {
		t.Errorf("the journal holds %v, want the keyed mailbox alone, by whoever issued the reset", got)
	}
}

func TestASuspensionStrandsNothing(t *testing.T) {
	users, db, _ := newUsers(t)
	_, bea, _, _, _ := strandedTeams(t, db)
	if _, err := users.Suspend(t.Context(), bea.ID); err != nil {
		t.Fatal(err)
	}
	if got := strandings(t, db); len(got) != 0 {
		t.Errorf("a suspension journaled %v: what they read waits for them", got)
	}
}

func TestAResumeInsideAnotherWriteNeverNestsOne(t *testing.T) {
	users, db, _ := newUsers(t)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	if _, err := users.Suspend(t.Context(), bea.ID); err != nil {
		t.Fatal(err)
	}
	ctx := within(t)
	var active bool
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if active, err = users.ResumeTx(ctx, tx, bea.ID); err != nil {
			return err
		}
		// And the rest of the write goes on in the same transaction.
		_, err = tx.ExecContext(ctx, `UPDATE users SET name = 'Bea' WHERE id = ?`, bea.ID)
		return err
	}); err != nil {
		t.Fatalf("resuming inside a write: %v", err)
	}
	if disabled, suspended := personState(t, users, bea.ID); !active || disabled || suspended {
		t.Errorf("resumed inside a write: active %v, disabled %v, suspended %v", active, disabled, suspended)
	}
	// A person disabled here is not resumed.
	if _, err := users.Disable(t.Context(), bea.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		active, err = users.ResumeTx(ctx, tx, bea.ID)
		return err
	}); err != nil || active {
		t.Errorf("resuming a person disabled here: active %v (%v)", active, err)
	}
}
