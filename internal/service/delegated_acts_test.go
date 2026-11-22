package service_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The acts an extension runs for a person that a session's screens did not
// have, or had narrower (delegated.go; docs/workspaces.md, "Acts an
// extension runs for a person"): a sign-out everywhere before a moment,
// notices dismissed in a batch, a removed mailbox's kept index turned off by
// its person or its team's owners, access taken away on a removed team
// mailbox, a recovery of a mailbox recovered or deleted for good since, and
// a team's turn-off of a mailbox removed since.

func TestASuspendedPersonsOwnWithdrawalAndSignOutStillRun(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	if _, err := f.svc.GrantActionsConsent(ctx, ana, f.consent().Actions); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET actions_consent_at = actions_consent_at - 100 WHERE id = ?`, ana.UserID)
	asked := time.Now()
	f.suspend(t, ana.UserID)
	out, err := f.actsFor(t, delegated(ana, "r1", "", ago(time.Minute))).WithdrawConsent(ctx, service.ConsentActions)
	if err != nil || !out.Changed {
		t.Fatalf("a suspended person withdrawing their consent: %+v, %v", out, err)
	}
	if f.count(t, `SELECT actions_consent_at FROM users WHERE id = ?`, ana.UserID) != 0 {
		t.Error("the withdrawal left the consent standing")
	}
	out, err = f.actsFor(t, delegated(ana, "r2", "", asked)).SignOutEverywhere(ctx, asked)
	if err != nil || !out.Changed {
		t.Fatalf("a suspended person signing out everywhere: %+v, %v", out, err)
	}
	if got := int64(f.count(t, `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, ana.UserID)); got != asked.Unix() {
		t.Errorf("the sign-out everywhere is recorded at %d, want %d", got, asked.Unix())
	}
	// Nothing else: their notices are not theirs to dismiss while suspended.
	_, err = f.actsFor(t, delegated(ana, "r3", "", time.Now())).DismissNotices(ctx, []string{"mbe_0000000000000001"})
	wantRefusal(t, "a suspended person dismissing notices", err, service.RefusalActorInactive)

	// Disabled here: not even their own.
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ActFor(ctx, delegated(ana, "r4", "", time.Now()))
	wantRefusal(t, "a person disabled here withdrawing or signing out", err, service.RefusalActorInactive)
	// Deleted: nobody acts.
	bea := f.person(t, "bea@example.com", auth.RoleMember)
	acts := f.actsFor(t, delegated(bea, "r5", "", time.Now()))
	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	_, err = acts.SignOutEverywhere(ctx, time.Now())
	wantRefusal(t, "a person deleted since signing out everywhere", err, service.RefusalActorInactive)
}

func TestSigningOutEverywhereBeforeAMomentEndsOnlyLiveSessionsStartedByThenAndRecordsIt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	key := authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, authtest.Personal(t, f.db, ana.UserID), ana.UserID)
	asked := time.Now().Add(-5 * time.Minute)
	session := func(started time.Time, expired, revoked bool) service.Principal {
		t.Helper()
		p := f.signInAgain(t, "ana@example.com")
		f.exec(t, `UPDATE sessions SET created_at = ? WHERE id = ?`, started.Unix(), p.SessionID)
		if expired {
			f.exec(t, `UPDATE sessions SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), p.SessionID)
		}
		if revoked {
			f.exec(t, `UPDATE sessions SET revoked_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), p.SessionID)
		}
		return p
	}
	before := session(asked.Add(-time.Hour), false, false)
	within := session(asked, false, false)
	session(asked.Add(-2*time.Hour), true, false)
	session(asked.Add(-3*time.Hour), false, true)
	after := session(asked.Add(time.Minute), false, false)
	// The fixture's own session started now, after the moment too.
	f.exec(t, `UPDATE sessions SET created_at = ? WHERE id = ?`, time.Now().Unix(), ana.SessionID)

	out, err := f.actsFor(t, delegated(ana, "r1", "", asked)).SignOutEverywhere(ctx, asked)
	if err != nil || !out.Changed || out.SessionsEnded != 2 {
		t.Fatalf("signing out everywhere: %+v, %v; want the two live sessions started by then ended", out, err)
	}
	for what, p := range map[string]service.Principal{"started before": before, "started in its second": within} {
		if err := f.svc.Recheck(ctx, p); err == nil {
			t.Errorf("the session %s the moment still works", what)
		}
	}
	for what, p := range map[string]service.Principal{"started after": after, "the fixture's, started now": ana} {
		if err := f.svc.Recheck(ctx, p); err != nil {
			t.Errorf("the session %s the moment ended: %v", what, err)
		}
	}
	if _, err := f.svc.Authenticate(ctx, key, nil); err != nil {
		t.Errorf("the person's key stopped: %v", err)
	}
	if got := int64(f.count(t, `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, ana.UserID)); got != asked.Unix() {
		t.Errorf("the sign-out everywhere is recorded at %d, want the moment asked, %d", got, asked.Unix())
	}
	// Again: its first outcome.
	if again, err := f.actsFor(t, delegated(ana, "r1", "", asked)).SignOutEverywhere(ctx, asked); err != nil ||
		!again.Ran || again.SessionsEnded != 2 {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	// An older moment ends nothing and never moves the record back; still
	// recorded, done.
	out, err = f.actsFor(t, delegated(ana, "r2", "", asked.Add(-24*time.Hour))).SignOutEverywhere(ctx, asked.Add(-24*time.Hour))
	if err != nil || out.Changed || out.SessionsEnded != 0 {
		t.Errorf("an older sign-out everywhere: %+v, %v", out, err)
	}
	if got := int64(f.count(t, `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, ana.UserID)); got != asked.Unix() {
		t.Errorf("an older sign-out moved the record to %d", got)
	}
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r2"); !ran {
		t.Error("the sign-out that ended nothing was not recorded")
	}
	// A moment ahead of now is now.
	out, err = f.actsFor(t, delegated(ana, "r3", "", time.Now())).SignOutEverywhere(ctx, time.Now().Add(time.Hour))
	if err != nil || out.SessionsEnded != 2 {
		t.Errorf("a sign-out everywhere asked ahead of now: %+v, %v; want the two sessions left", out, err)
	}
	if got := int64(f.count(t, `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, ana.UserID)); got > time.Now().Unix() {
		t.Errorf("the record is ahead of now: %d", got)
	}
	_, err = f.actsFor(t, delegated(ana, "r4", "", time.Now())).SignOutEverywhere(ctx, time.Time{})
	wantCode(t, "a sign-out everywhere naming no moment", err, service.CodeBadRequest)
}

// noticeOf gives each of userIDs a notice of accountID's sync turned off,
// as a journal entry the actor wrote, and returns its id.
func (f *fixture) noticeOf(t *testing.T, accountID, actor string, userIDs ...string) string {
	t.Helper()
	var id string
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		_, id, err = store.JournalMailboxTx(t.Context(), tx, store.MailboxEvent{
			Kind: store.MailboxSyncPaused, AccountID: accountID, Actor: actor, At: time.Now().Unix(),
		}, userIDs, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDismissingNoticesInABatchCountsGoneOnesAsDone(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	first := f.noticeOf(t, tm.shared, tm.ana.UserID, tm.bea.UserID)
	second := f.noticeOf(t, tm.shared, tm.ana.UserID, tm.bea.UserID, tm.carol.UserID)
	carols := f.noticeOf(t, tm.shared, tm.ana.UserID, tm.carol.UserID)
	if n := len(f.notices(t, tm.bea)); n != 2 {
		t.Fatalf("Bea holds %d notices", n)
	}
	out, err := f.actsFor(t, delegated(tm.bea, "d1", "", time.Now())).DismissNotices(ctx,
		[]string{first, second, carols, "mbe_00000000000000ff", first})
	if err != nil || !out.Changed || out.Dismissed != 2 {
		t.Fatalf("dismissing Bea's two notices, Carol's, one gone and one twice: %+v, %v", out, err)
	}
	if n := len(f.notices(t, tm.bea)); n != 0 {
		t.Errorf("Bea still holds %d notices", n)
	}
	if n := len(f.notices(t, tm.carol)); n != 2 {
		t.Errorf("Carol holds %d notices, want her two: a dismissal is the person's own", n)
	}
	if again, err := f.actsFor(t, delegated(tm.bea, "d1", "", time.Now())).DismissNotices(ctx, []string{first}); err != nil ||
		!again.Ran || again.Dismissed != 2 {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	// Every one gone already: done, with nothing changed.
	out, err = f.actsFor(t, delegated(tm.bea, "d2", "", time.Now())).DismissNotices(ctx, []string{first, second})
	if err != nil || out.Changed || out.Dismissed != 0 {
		t.Errorf("dismissing notices dismissed already: %+v, %v", out, err)
	}
	many := make([]string, store.MaxNoticesDismissed+1)
	for i := range many {
		many[i] = fmt.Sprintf("mbe_%016x", i)
	}
	for what, ids := range map[string][]string{"none": nil, "301": many, "an empty id": {""}} {
		_, err := f.actsFor(t, delegated(tm.bea, "d3", "", time.Now())).DismissNotices(ctx, ids)
		wantCode(t, "a dismissal of "+what, err, service.CodeBadRequest)
	}
	if out, err := f.actsFor(t, delegated(tm.bea, "d4", "", time.Now())).DismissNotices(ctx, many[:300]); err != nil || out.Dismissed != 0 {
		t.Errorf("a dismissal of 300 notices: %+v, %v", out, err)
	}
}

// keptReaderTeam is the Support team's mailbox, synced, removed by Ana: Ana,
// who linked it, and Bea, given read, send and manage, would read it once
// recovered.
func keptReaderTeam(t *testing.T, f *fixture) keysTeam {
	t.Helper()
	tm := syncedTeam(t, f)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Send: true, Manage: true})
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE account_id = ?`, tm.shared)
	f.removed(t, tm.ana, tm.shared)
	return tm
}

func TestAnOwnerTakesAccessAwayOnARemovedMailboxButNeverFromItsLastKeptReader(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := keptReaderTeam(t, f)
	flags := func(userID string) string {
		t.Helper()
		var read, act, send, manage bool
		err := f.db.Reader().QueryRowContext(ctx, `SELECT read, act, send, manage FROM mailbox_access
			WHERE account_id = ? AND user_id = ?`, tm.shared, userID).Scan(&read, &act, &send, &manage)
		if err == sql.ErrNoRows {
			return "none"
		}
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(read, act, send, manage)
	}

	// Carol, an admin, sees no removed mailbox: not found, either way.
	err := f.svc.RevokeAccess(ctx, tm.carol, tm.shared, tm.bea.UserID, workspace.Flags{Send: true})
	wantCode(t, "Carol, an admin, taking Bea's send", err, service.CodeNotFound)
	_, err = f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).DropFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Send: true})
	wantRefusal(t, "Carol, an admin, taking Bea's send through an extension", err, service.RefusalNotFound)

	// Ana, its owner, signed in: Bea's send goes.
	if err := f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{Send: true}); err != nil {
		t.Fatalf("Ana taking Bea's send on the removed mailbox: %v", err)
	}
	// Through an extension: Bea's access goes.
	out, err := f.actsFor(t, delegated(tm.ana, "a1", tm.id, time.Now())).RemoveAccess(ctx, tm.shared, tm.bea.UserID)
	if err != nil || !out.Changed {
		t.Fatalf("Ana removing Bea's access on the removed mailbox through an extension: %+v, %v", out, err)
	}
	if got := flags(tm.bea.UserID); got != "none" {
		t.Errorf("Bea still holds %s", got)
	}
	// Ana is now the one who would read it once recovered: she keeps read,
	// either way.
	err = f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.ana.UserID, workspace.Flags{Read: true})
	wantRefusal(t, "Ana taking her own read, the last kept", err, service.RefusalLastReader)
	_, err = f.actsFor(t, delegated(tm.ana, "a2", tm.id, time.Now())).RemoveAccess(ctx, tm.shared, tm.ana.UserID)
	wantRefusal(t, "Ana removing her own access through an extension, the last kept", err, service.RefusalLastReader)
	if got := flags(tm.ana.UserID); got != "true true true false" {
		t.Errorf("Ana holds %s after the refusals", got)
	}
	// Taking act away is no read taken: it goes.
	if out, err := f.actsFor(t, delegated(tm.ana, "a3", tm.id, time.Now())).DropFlags(ctx, tm.shared, tm.ana.UserID,
		service.GrantFlags{Act: true}); err != nil || !out.Changed {
		t.Errorf("Ana dropping her own act: %+v, %v", out, err)
	}
	// Recovered, it comes back with who still holds read.
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared); err != nil {
		t.Fatal(err)
	}
	if got := flags(tm.bea.UserID); got != "none" {
		t.Errorf("the recovery gave Bea %s back", got)
	}
}

// raceTheWrite runs act while the writer's one connection is held and, once
// act waits for it, runs meanwhile in the held transaction and commits it:
// what act read before its write saw the database before meanwhile, and its
// write sees it after. It returns what act returned.
func (f *fixture) raceTheWrite(t *testing.T, act func() error, meanwhile func(tx *sql.Tx)) error {
	t.Helper()
	writer := f.db.Writer()
	tx, err := writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	waited := writer.Stats().WaitCount
	done := make(chan error, 1)
	go func() { done <- act() }()
	for deadline := time.Now().Add(10 * time.Second); writer.Stats().WaitCount == waited; time.Sleep(time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("the act ended before it waited for the writer: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the act never waited for the writer")
		}
	}
	meanwhile(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// removeIn removes a team mailbox inside tx, by who, as the repository
// does (removeBySQL).
func removeIn(t *testing.T, tx *sql.Tx, accountID, who string) {
	t.Helper()
	if _, err := tx.ExecContext(t.Context(), `UPDATE accounts SET removed_at = unixepoch(), removed_by = ?,
		purge_after = unixepoch() + 2592000 WHERE id = ? AND removed_at = 0`, who, accountID); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyAnOwnerTakesAccessAwayOnAMailboxRemovedBetweenTheReadAndTheWrite(t *testing.T) {
	type taker struct {
		who     string
		allowed bool
		take    func(f *fixture, tm keysTeam) error
	}
	takers := []taker{
		{"Carol, an admin", false, func(f *fixture, tm keysTeam) error {
			return f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, tm.bea.UserID, workspace.Flags{Read: true})
		}},
		{"the operator", false, func(f *fixture, tm keysTeam) error {
			return f.svc.RevokeAccess(t.Context(), admin(), tm.shared, tm.bea.UserID, workspace.Flags{Read: true})
		}},
		{"Carol, through an extension, dropping read", false, func(f *fixture, tm keysTeam) error {
			_, err := f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).DropFlags(t.Context(), tm.shared,
				tm.bea.UserID, service.GrantFlags{Read: true})
			return err
		}},
		{"Carol, through an extension, removing the access", false, func(f *fixture, tm keysTeam) error {
			_, err := f.actsFor(t, delegated(tm.carol, "c2", tm.id, time.Now())).RemoveAccess(t.Context(), tm.shared,
				tm.bea.UserID)
			return err
		}},
		{"Ana, its owner", true, func(f *fixture, tm keysTeam) error {
			return f.svc.RevokeAccess(t.Context(), tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{Read: true})
		}},
	}
	for _, c := range takers {
		t.Run(c.who, func(t *testing.T) {
			f := newFixture(t)
			tm := syncedTeam(t, f)
			tm.grant(t, tm.shared, workspace.Flags{Read: true, Send: true, Manage: true})
			f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE account_id = ?`, tm.shared)
			held := func() string {
				t.Helper()
				return fmt.Sprint(f.count(t, `SELECT coalesce(max(read * 1000 + act * 100 + send * 10 + manage), -1)
					FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.bea.UserID))
			}
			before := held()
			if before != "1011" {
				t.Fatalf("Bea holds %s, want read, send and manage", before)
			}

			// Read live; removed by Ana before the write.
			err := f.raceTheWrite(t, func() error { return c.take(f, tm) }, func(tx *sql.Tx) {
				removeIn(t, tx, tm.shared, tm.ana.UserID)
			})
			if c.allowed {
				if err != nil {
					t.Fatalf("taking Bea's read on the mailbox removed meanwhile: %v", err)
				}
				if after := held(); after != "11" {
					t.Errorf("Bea held %s, and holds %s: want her read gone", before, after)
				}
				return
			}
			wantCode(t, "taking Bea's read on the mailbox removed meanwhile", err, service.CodeNotFound)
			if after := held(); after != before {
				t.Errorf("Bea held %s, and holds %s", before, after)
			}
		})
	}
}

func TestTakingAwayReachesAMemberInAnyStanding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Send: true})
	if _, err := tm.ws.SetGrant(ctx, tm.shared, dan.UserID, workspace.Flags{Read: true, Send: true}, tm.ana.UserID, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE account_id = ?`, tm.shared)

	// Bea suspended by an extension, Dan's membership disabled: their flags
	// go all the same.
	f.suspend(t, tm.bea.UserID)
	f.exec(t, `UPDATE workspace_members SET status = 'disabled' WHERE workspace_id = ? AND user_id = ?`, tm.id, dan.UserID)
	for name, userID := range map[string]string{"Bea, suspended": tm.bea.UserID, "Dan, disabled in the team": dan.UserID} {
		out, err := f.actsFor(t, delegated(tm.carol, "s-"+userID, tm.id, time.Now())).DropFlags(ctx, tm.shared, userID,
			service.GrantFlags{Send: true})
		if err != nil || !out.Changed {
			t.Errorf("Carol dropping %s's send: %+v, %v", name, out, err)
		}
		out, err = f.actsFor(t, delegated(tm.carol, "r-"+userID, tm.id, time.Now())).RemoveAccess(ctx, tm.shared, userID)
		if err != nil || !out.Changed {
			t.Errorf("Carol removing %s's access: %+v, %v", name, out, err)
		}
		if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, userID); n != 0 {
			t.Errorf("%s still holds a grant", name)
		}
	}
	// A former member holds nothing: done.
	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, dan.UserID); err != nil {
		t.Fatal(err)
	}
	if out, err := f.actsFor(t, delegated(tm.carol, "former", tm.id, time.Now())).RemoveAccess(ctx, tm.shared, dan.UserID); err != nil ||
		out.Changed {
		t.Errorf("removing a former member's access: %+v, %v", out, err)
	}
}

func TestAPersonTurnsOffTheKeptIndexOfTheirOwnRemovedMailbox(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bea := f.person(t, "bea@example.com", auth.RoleOwner)
	own := f.ownSynced(t, ana, "ana@mail.example")
	other := f.mailbox(t, ana, "ana2@mail.example")
	for _, id := range []string{own, other} {
		f.connect(t, id)
		f.seedIndex(t, id, "Okapi")
		f.removed(t, ana, id)
	}
	journal := f.count(t, `SELECT count(*) FROM mailbox_events`)
	hook.take()

	// Nobody else: Bea, an instance owner, and the operator.
	for name, p := range map[string]service.Principal{"Bea": bea, "the operator": admin()} {
		_, err := f.svc.SetMailboxSync(ctx, p, own, switchSync(false))
		wantCode(t, name+" turning off Ana's removed mailbox", err, service.CodeNotFound)
	}
	// Ana, signed in, by the route a team's owner uses.
	st, err := f.svc.SetMailboxSync(ctx, ana, own, switchSync(false))
	if err != nil || st.State != "off" {
		t.Fatalf("Ana turning off her removed mailbox: %+v, %v", st, err)
	}
	// Through an extension, her other one.
	out, err := f.actsFor(t, delegated(ana, "r1", authtest.Personal(t, f.db, ana.UserID), time.Now())).RemovedIndexOff(ctx, other)
	if err != nil || !out.Changed {
		t.Fatalf("Ana turning off her other removed mailbox through an extension: %+v, %v", out, err)
	}
	for _, id := range []string{own, other} {
		if n := f.indexed(t, id); n != 0 {
			t.Errorf("%s keeps %d rows of its index", id, n)
		}
		if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, id); n != 1 {
			t.Errorf("%s is no longer removed", id)
		}
	}
	if f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, ana.UserID) == 0 {
		t.Error("her consent went with the index")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events`); n != journal {
		t.Errorf("the journal holds %d entries, and held %d: her own act on what nobody else reads is told nowhere", n, journal)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("a personal mailbox's kept index asked the freshness hook %+v", asked)
	}
	// Nothing kept: done.
	if out, err := f.actsFor(t, delegated(ana, "r2", authtest.Personal(t, f.db, ana.UserID), time.Now())).RemovedIndexOff(ctx, other); err != nil ||
		out.Changed {
		t.Errorf("turning off what keeps nothing: %+v, %v", out, err)
	}
}

func TestARecoveryAfterThatComesBackPausedWithNothingKept(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	f.connect(t, own)
	f.seedIndex(t, own, "Okapi")
	f.removed(t, ana, own)
	if _, err := f.actsFor(t, delegated(ana, "r1", authtest.Personal(t, f.db, ana.UserID), time.Now())).RemovedIndexOff(ctx, own); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RecoverAccount(ctx, ana, own); err != nil {
		t.Fatal(err)
	}
	if !f.paused(t, own) || f.indexed(t, own) != 0 {
		t.Errorf("recovered: paused %v, %d rows indexed; want paused with nothing kept", f.paused(t, own), f.indexed(t, own))
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, ana, own, service.SyncResumeRequest{}); err != nil {
		t.Fatal(err)
	}
	if !f.eligible(t, own) {
		t.Error("resumed, it does not sync")
	}
}

func TestRemovedIndexOffOnAMailboxRecoveredSinceTouchesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	personal := authtest.Personal(t, f.db, tm.ana.UserID)
	for _, id := range []string{tm.shared, own} {
		f.connect(t, id)
		f.seedIndex(t, id, "Zebra")
		f.removed(t, tm.ana, id)
		if _, err := f.svc.RecoverAccount(ctx, tm.ana, id); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) string {
		t.Helper()
		return fmt.Sprint(f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, id),
			f.count(t, `SELECT sync_paused_at FROM accounts WHERE id = ?`, id),
			f.count(t, `SELECT sync_held_at FROM accounts WHERE id = ?`, id), f.indexed(t, id),
			f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, id),
			f.count(t, `SELECT count(*) FROM notices`), f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, tm.ana.UserID) != 0)
	}
	for id, ws := range map[string]string{tm.shared: tm.id, own: personal} {
		before := state(id)
		out, err := f.actsFor(t, delegated(tm.ana, "r-"+id, ws, time.Now())).RemovedIndexOff(ctx, id)
		if err != nil || out.Changed || out.Ran {
			t.Errorf("the kept index of %s, recovered since: %+v, %v; want done, nothing changed", id, out, err)
		}
		if after := state(id); after != before {
			t.Errorf("%s was %s and is %s", id, before, after)
		}
		if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r-"+id); !ran {
			t.Errorf("the request on %s was not recorded", id)
		}
	}
	// Carol, an admin, may never: not found.
	_, err := f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).RemovedIndexOff(ctx, tm.shared)
	wantRefusal(t, "Carol, an admin, on the team's mailbox recovered since", err, service.RefusalNotFound)
}

func TestTurningOffOnlyARemovedMailboxsKeptIndexNeverTurnsOffOneRecoveredSince(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	removedOnly := func(on bool) service.MailboxSyncRequest {
		req := switchSync(on)
		req.RemovedOnly = true
		return req
	}
	for _, id := range []string{tm.shared, own} {
		f.connect(t, id)
		f.seedIndex(t, id, "Zebra")
		f.removed(t, tm.ana, id)
		if _, err := f.svc.RecoverAccount(ctx, tm.ana, id); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) string {
		t.Helper()
		return fmt.Sprint(f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, id),
			f.count(t, `SELECT sync_paused_at FROM accounts WHERE id = ?`, id),
			f.count(t, `SELECT sync_held_at FROM accounts WHERE id = ?`, id), f.indexed(t, id),
			f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, id),
			f.count(t, `SELECT count(*) FROM notices`), f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, tm.ana.UserID) != 0)
	}
	// Recovered since the page drew it: refused, and nothing of the live
	// mailbox is turned off.
	for _, id := range []string{tm.shared, own} {
		before := state(id)
		_, err := f.svc.SetMailboxSync(ctx, tm.ana, id, removedOnly(false))
		if !errors.Is(err, service.ErrNotRemoved) || service.CodeOf(err) != service.CodeConflict {
			t.Errorf("the kept index of %s, recovered since: %v, want conflict (ErrNotRemoved)", id, err)
		}
		if after := state(id); after != before {
			t.Errorf("%s was %s and is %s", id, before, after)
		}
	}
	_, err := f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, removedOnly(true))
	wantCode(t, "switching a removed mailbox on", err, service.CodeBadRequest)
	// Removed: its kept index goes, by its owners alone.
	for _, id := range []string{tm.shared, own} {
		f.removed(t, tm.ana, id)
	}
	_, err = f.svc.SetMailboxSync(ctx, tm.bea, tm.shared, removedOnly(false))
	wantCode(t, "Bea, a member, on the removed team mailbox", err, service.CodeNotFound)
	for _, id := range []string{tm.shared, own} {
		st, err := f.svc.SetMailboxSync(ctx, tm.ana, id, removedOnly(false))
		if err != nil || st.State != "off" {
			t.Errorf("the kept index of %s, removed: %+v, %v", id, st, err)
		}
		if n := f.indexed(t, id); n != 0 {
			t.Errorf("%s keeps %d rows of its index", id, n)
		}
	}
}

func TestRecoveringAMailboxRecoveredSinceIsDone(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.removed(t, tm.ana, tm.shared)
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared); err != nil {
		t.Fatal(err)
	}
	journal := f.count(t, `SELECT count(*) FROM mailbox_events`)
	out, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).Recover(ctx, tm.shared)
	if err != nil || out.Changed || out.Ran {
		t.Fatalf("recovering a mailbox recovered since: %+v, %v; want done, nothing changed", out, err)
	}
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); !ran {
		t.Error("the request was not recorded")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events`); n != journal {
		t.Errorf("the journal grew from %d to %d", journal, n)
	}
	// Who could never recover it is told it does not exist.
	_, err = f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).Recover(ctx, tm.shared)
	wantRefusal(t, "Carol, an admin, recovering it", err, service.RefusalNotFound)
}

func TestRecoveringAPurgedMailboxIsRecoveryClosed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.removed(t, tm.ana, tm.shared)
	if err := f.svc.PurgeAccount(ctx, tm.ana, tm.shared, service.PurgeAccountRequest{Confirm: tm.shared}); err != nil {
		t.Fatal(err)
	}
	_, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).Recover(ctx, tm.shared)
	wantRefusal(t, "Ana recovering the mailbox deleted for good", err, service.RefusalRecoveryOver)
	wantCode(t, "Ana recovering the mailbox deleted for good", err, service.CodeConflict)
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); ran {
		t.Error("the refused request was recorded")
	}
	// Whoever could never recover it, and another workspace, are not told.
	_, err = f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).Recover(ctx, tm.shared)
	wantRefusal(t, "Carol, an admin", err, service.RefusalNotFound)
	_, err = f.actsFor(t, delegated(tm.ana, "r2", authtest.Personal(t, f.db, tm.ana.UserID), time.Now())).Recover(ctx, tm.shared)
	wantRefusal(t, "Ana, naming her personal workspace", err, service.RefusalNotFound)
	_, err = f.actsFor(t, delegated(tm.ana, "r3", tm.id, time.Now())).Recover(ctx, "acc_00000000000000ff")
	wantRefusal(t, "Ana, a mailbox nobody knows", err, service.RefusalNotFound)
}

func TestTurningSyncOffOnAMailboxRemovedSinceDeletesItsKeptIndexForAnOwner(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.seedIndex(t, tm.shared, "Zebra")
	f.removed(t, tm.ana, tm.shared)
	hook.take()

	// Carol, an admin, sees no removed mailbox.
	_, err := f.actsFor(t, delegated(tm.carol, "c1", tm.id, time.Now())).TeamSync(ctx, tm.shared, false, "")
	wantRefusal(t, "Carol, an admin, turning off the mailbox removed since", err, service.RefusalNotFound)
	// The team agreed at or after Ana asked: nothing is taken away.
	_, err = f.actsFor(t, delegated(tm.ana, "a1", tm.id, ago(time.Minute))).TeamSync(ctx, tm.shared, false, "")
	wantRefusal(t, "Ana's turn-off asked before the team's agreement", err, service.RefusalSetSince)
	if f.indexed(t, tm.shared) == 0 {
		t.Fatal("the refused turn-off deleted the kept index")
	}
	hook.take()
	// Asked after it: it runs.
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 300 WHERE id = ?`, tm.shared)
	out, err := f.actsFor(t, delegated(tm.ana, "a2", tm.id, ago(time.Minute))).TeamSync(ctx, tm.shared, false, "")
	if err != nil || !out.Changed {
		t.Fatalf("Ana turning off the mailbox removed since: %+v, %v", out, err)
	}
	if n := f.indexed(t, tm.shared); n != 0 {
		t.Errorf("the removed mailbox keeps %d rows of its index", n)
	}
	if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) != 0 {
		t.Error("the team's agreement stands")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'sync_paused'`, tm.shared); n != 1 {
		t.Errorf("%d turn-offs journaled, want one", n)
	}
	asked := hook.take()
	if len(asked) != 1 || asked[0].Kind != service.FreshRemovedSyncOff || asked[0].ActorID != tm.ana.UserID {
		t.Errorf("the turn-off asked the freshness hook %+v", asked)
	}
}
