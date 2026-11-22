package service_test

import (
	"context"
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

// removalsMovedBack moves every removal back by d, as if each mailbox had
// been removed that much earlier: a removal never moves in place
// (accounts_removal_fixed), so each is lifted and recorded again, earlier,
// in one write.
func (f *fixture) removalsMovedBack(t *testing.T, d time.Duration) {
	t.Helper()
	ctx := t.Context()
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		type removal struct {
			id, by    string
			at, purge int64
		}
		var removals []removal
		rows, err := tx.QueryContext(ctx, `SELECT id, removed_by, removed_at, purge_after FROM accounts WHERE removed_at <> 0`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r removal
			if err := rows.Scan(&r.id, &r.by, &r.at, &r.purge); err != nil {
				return err
			}
			removals = append(removals, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		back := int64(d / time.Second)
		for _, r := range removals {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0 WHERE id = ?`,
				r.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = ?, removed_by = ?, purge_after = ? WHERE id = ?`,
				r.at-back, r.by, r.purge-back, r.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// removeIn removes a team mailbox inside tx, by who, as the repository
// does (removeBySQL).
func removeIn(ctx context.Context, t *testing.T, tx *sql.Tx, accountID, who string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = unixepoch(), removed_by = ?,
		purge_after = unixepoch() + 2592000 WHERE id = ? AND removed_at = 0`, who, accountID); err != nil {
		t.Fatal(err)
	}
}

// recoverIn recovers a removed mailbox inside tx, by who, as the repository
// does (recoverRemoved): live again, paused under its consent, journaled
// with who reads it then.
func recoverIn(ctx context.Context, t *testing.T, tx *sql.Tx, accountID, who string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
		sync_paused_at = unixepoch(), sync_paused_by = ? WHERE id = ? AND removed_at <> 0`, who, accountID); err != nil {
		t.Fatal(err)
	}
	readers, err := store.MailboxReadersTx(ctx, tx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
		Kind: store.MailboxRecovered, AccountID: accountID, Actor: who, At: time.Now().Unix(),
	}, readers, false); err != nil {
		t.Fatal(err)
	}
}

// purgeIn deletes a removed mailbox for good inside tx, by who, journaled
// as the repository does (purgeRemoved): its journal's names forgotten, its
// deletion journaled, the mailbox gone.
func purgeIn(ctx context.Context, t *testing.T, tx *sql.Tx, accountID, who string) {
	t.Helper()
	if err := store.ForgetMailboxNamesTx(ctx, tx, []string{accountID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
		Kind: store.MailboxPurged, AccountID: accountID, Actor: who, At: time.Now().Unix(),
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`DELETE FROM events WHERE account_id = ?`, `DELETE FROM accounts WHERE id = ?`} {
		if _, err := tx.ExecContext(ctx, query, accountID); err != nil {
			t.Fatal(err)
		}
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
				removeIn(t.Context(), t, tx, tm.shared, tm.ana.UserID)
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
	// Removed before anyone asks to delete what they keep.
	f.removalsMovedBack(t, 10*time.Minute)
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
	f.removalsMovedBack(t, 10*time.Minute)
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

func TestALateRemovedIndexOffNeverWithdrawsATeamsAgreementGivenSinceItWasAsked(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.seedIndex(t, tm.shared, "Zebra")
	f.removed(t, tm.ana, tm.shared)
	// Agreed well before Ana asks the console to delete what it keeps.
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE id = ?`, tm.shared)
	asked := ago(time.Minute)

	// Since she asked, in Mailie's own screens: recovered, resumed (the
	// team agreeing again), indexed again and removed again.
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	f.seedIndex(t, tm.shared, "Okapi")
	f.removed(t, tm.ana, tm.shared)
	agreed := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)
	indexed := f.indexed(t, tm.shared)
	if int64(agreed) < asked.Unix() || indexed == 0 {
		t.Fatalf("the team agreed at %d, %d rows kept; want an agreement since %d and an index", agreed, indexed, asked.Unix())
	}

	_, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, asked)).RemovedIndexOff(ctx, tm.shared)
	wantRefusal(t, "the kept index turned off by a request asked before the team agreed again", err, service.RefusalSetSince)
	if n := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared); n != agreed {
		t.Errorf("the team's agreement was %d and is %d", agreed, n)
	}
	if n := f.indexed(t, tm.shared); n != indexed {
		t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", indexed, n)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'sync_paused'`, tm.shared); n != 0 {
		t.Errorf("%d turn-offs journaled", n)
	}
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); ran {
		t.Error("the refused request was recorded")
	}

	// Agreed and removed before the request was asked: it runs.
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE id = ?`, tm.shared)
	f.removalsMovedBack(t, 10*time.Minute)
	out, err := f.actsFor(t, delegated(tm.ana, "r2", tm.id, asked)).RemovedIndexOff(ctx, tm.shared)
	if err != nil || !out.Changed {
		t.Fatalf("the kept index turned off by a request asked after the team agreed: %+v, %v", out, err)
	}
	if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) != 0 || f.indexed(t, tm.shared) != 0 {
		t.Error("the team's agreement or the kept index stands")
	}
}

func TestARemovedIndexOffMeetsATeamsAgreementGivenBetweenTheReadAndTheWrite(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.seedIndex(t, tm.shared, "Zebra")
	f.removed(t, tm.ana, tm.shared)
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE id = ?`, tm.shared)
	indexed := f.indexed(t, tm.shared)

	// Read removed under the old agreement; before the write, recovered,
	// agreed to again and removed again, in Mailie's own screens.
	var agreed int64
	err := f.raceTheWrite(t, func() error {
		_, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, ago(time.Minute))).RemovedIndexOff(t.Context(), tm.shared)
		return err
	}, func(tx *sql.Tx) {
		recoverIn(ctx, t, tx, tm.shared, tm.ana.UserID)
		if err := tx.QueryRowContext(ctx, `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '',
			sync_enabled_at = unixepoch(), sync_enabled_by = ? WHERE id = ? RETURNING sync_enabled_at`,
			tm.ana.UserID, tm.shared).Scan(&agreed); err != nil {
			t.Fatal(err)
		}
		removeIn(ctx, t, tx, tm.shared, tm.ana.UserID)
	})
	wantRefusal(t, "the kept index turned off as the team agreed again", err, service.RefusalSetSince)
	if n := int64(f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)); n != agreed {
		t.Errorf("the team's agreement was %d and is %d", agreed, n)
	}
	if n := f.indexed(t, tm.shared); n != indexed {
		t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", indexed, n)
	}
}

// removedAgainOfAna is a mailbox of Ana's removed before she asks the
// console to delete what it keeps, and then recovered, used and removed
// again in Mailie's own screens: the team's, recovered and removed with no
// agreement given since, or her personal one, resumed in between.
type removedAgainOfAna struct {
	what string
	// mailbox removes it, its agreement and removal well before now, and
	// names it and its workspace.
	mailbox func(t *testing.T, f *fixture, tm keysTeam) (accountID, workspaceID string)
	// used is what Ana does with it once recovered, before she removes it
	// again.
	used func(t *testing.T, f *fixture, tm keysTeam, accountID string)
	// kept reads what a removed mailbox keeps of its person's or its team's
	// beside its index: the consent or the agreement.
	kept func(t *testing.T, f *fixture, tm keysTeam, accountID string) int
}

var removedAgainOfAnas = []removedAgainOfAna{
	{
		what: "the team's",
		mailbox: func(t *testing.T, f *fixture, tm keysTeam) (string, string) {
			f.seedIndex(t, tm.shared, "Zebra")
			f.removed(t, tm.ana, tm.shared)
			f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE id = ?`, tm.shared)
			f.removalsMovedBack(t, 10*time.Minute)
			return tm.shared, tm.id
		},
		used: func(t *testing.T, f *fixture, _ keysTeam, id string) {
			// Paused, as a recovery brings it, under the team's agreement as
			// it was.
			f.seedIndex(t, id, "Okapi")
		},
		kept: func(t *testing.T, f *fixture, _ keysTeam, id string) int {
			return f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, id)
		},
	},
	{
		what: "her personal one",
		mailbox: func(t *testing.T, f *fixture, tm keysTeam) (string, string) {
			own := f.ownSynced(t, tm.ana, "ana@mail.example")
			f.connect(t, own)
			f.seedIndex(t, own, "Zebra")
			f.removed(t, tm.ana, own)
			f.exec(t, `UPDATE users SET sync_consent_at = sync_consent_at - 600 WHERE id = ?`, tm.ana.UserID)
			f.removalsMovedBack(t, 10*time.Minute)
			return own, authtest.Personal(t, f.db, tm.ana.UserID)
		},
		used: func(t *testing.T, f *fixture, tm keysTeam, id string) {
			if _, err := f.svc.ResumeMailboxSync(t.Context(), tm.ana, id, service.SyncResumeRequest{}); err != nil {
				t.Fatal(err)
			}
			f.seedIndex(t, id, "Okapi")
		},
		kept: func(t *testing.T, f *fixture, tm keysTeam, _ string) int {
			return f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, tm.ana.UserID)
		},
	},
}

func TestALateRemovedIndexOffNeverDeletesWhatAMailboxRemovedAgainSinceItWasAskedKeeps(t *testing.T) {
	for _, c := range removedAgainOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)
			asked := ago(time.Minute)

			// Since Ana asked, in Mailie's own screens: recovered, used and
			// removed again.
			if _, err := f.svc.RecoverAccount(ctx, tm.ana, id); err != nil {
				t.Fatal(err)
			}
			c.used(t, f, tm, id)
			f.removed(t, tm.ana, id)
			kept, indexed := c.kept(t, f, tm, id), f.indexed(t, id)
			if kept == 0 || int64(kept) >= asked.Unix() || indexed == 0 {
				t.Fatalf("kept %d (asked at %d), %d rows indexed; want an older consent or agreement, and an index",
					kept, asked.Unix(), indexed)
			}

			_, err := f.actsFor(t, delegated(tm.ana, "r1", ws, asked)).RemovedIndexOff(ctx, id)
			wantRefusal(t, "the kept index of a mailbox removed again since the request was asked", err,
				service.RefusalSetSince)
			if n := f.indexed(t, id); n != indexed {
				t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", indexed, n)
			}
			if n := c.kept(t, f, tm, id); n != kept {
				t.Errorf("what it kept beside its index was %d and is %d", kept, n)
			}
			if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'sync_paused'`, id); n != 0 {
				t.Errorf("%d turn-offs journaled", n)
			}
			if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); ran {
				t.Error("the refused request was recorded")
			}

			// Removed again before the request was asked: it runs.
			f.removalsMovedBack(t, 10*time.Minute)
			out, err := f.actsFor(t, delegated(tm.ana, "r2", ws, asked)).RemovedIndexOff(ctx, id)
			if err != nil || !out.Changed {
				t.Fatalf("the kept index of a mailbox removed before the request was asked: %+v, %v", out, err)
			}
			if n := f.indexed(t, id); n != 0 {
				t.Errorf("the removed mailbox keeps %d rows of its index", n)
			}
		})
	}
}

func TestARemovedIndexOffMeetsARemovalMadeAgainBetweenTheReadAndTheWrite(t *testing.T) {
	for _, c := range removedAgainOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)
			kept, indexed := c.kept(t, f, tm, id), f.indexed(t, id)

			// Read under its first removal; recovered and removed again, in
			// Mailie's own screens, before the write.
			err := f.raceTheWrite(t, func() error {
				_, err := f.actsFor(t, delegated(tm.ana, "r1", ws, ago(time.Minute))).RemovedIndexOff(t.Context(), id)
				return err
			}, func(tx *sql.Tx) {
				recoverIn(t.Context(), t, tx, id, tm.ana.UserID)
				if _, err := tx.ExecContext(t.Context(), `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = ?`,
					id); err != nil {
					t.Fatal(err)
				}
				removeIn(t.Context(), t, tx, id, tm.ana.UserID)
			})
			wantRefusal(t, "the kept index of a mailbox removed again meanwhile", err, service.RefusalSetSince)
			if n := f.indexed(t, id); n != indexed {
				t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", indexed, n)
			}
			if n := c.kept(t, f, tm, id); n != kept {
				t.Errorf("what it kept beside its index was %d and is %d", kept, n)
			}
		})
	}
}

// keptOfRemoved reads what deleting a removed mailbox for good would take:
// whether it is still there, removed, the rows of its index, its team's
// agreement and what Bea holds on it.
func (f *fixture) keptOfRemoved(t *testing.T, tm keysTeam, accountID string) string {
	t.Helper()
	return fmt.Sprintf("removed %d, index %d, agreed at %d, Bea holds %d",
		f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, accountID),
		f.indexed(t, accountID),
		f.count(t, `SELECT coalesce(max(sync_enabled_at), -1) FROM accounts WHERE id = ?`, accountID),
		f.count(t, `SELECT coalesce(max(read * 1000 + act * 100 + send * 10 + manage), -1)
			FROM mailbox_access WHERE account_id = ? AND user_id = ?`, accountID, tm.bea.UserID))
}

func TestALatePurgeNeverDeletesATeamsAgreementOrAGrantGivenSinceItWasAsked(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.seedIndex(t, tm.shared, "Zebra")
	f.removed(t, tm.ana, tm.shared)
	// Agreed, given and removed well before Ana asks the console to delete
	// it for good.
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE id = ?`, tm.shared)
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE account_id = ?`, tm.shared)
	f.removalsMovedBack(t, 10*time.Minute)
	asked := ago(time.Minute)

	// Since she asked, in Mailie's own screens: recovered, resumed (the team
	// agreeing again), Bea given act, indexed again and removed again, to be
	// kept 30 days more.
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Act: true})
	f.seedIndex(t, tm.shared, "Okapi")
	f.removed(t, tm.ana, tm.shared)
	agreed := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)
	raised := f.count(t, `SELECT raised_at FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.bea.UserID)
	if int64(agreed) < asked.Unix() || int64(raised) < asked.Unix() {
		t.Fatalf("the team agreed at %d and Bea was given act at %d; want both since %d", agreed, raised, asked.Unix())
	}
	kept := f.keptOfRemoved(t, tm, tm.shared)

	_, err := f.actsFor(t, delegated(tm.ana, "p1", tm.id, asked)).Purge(ctx, tm.shared)
	wantRefusal(t, "a deletion for good asked before the team agreed again", err, service.RefusalSetSince)
	if now := f.keptOfRemoved(t, tm, tm.shared); now != kept {
		t.Errorf("the removed mailbox kept %s, and keeps %s", kept, now)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'purged'`, tm.shared); n != 0 {
		t.Errorf("%d deletions for good journaled", n)
	}
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "p1"); ran {
		t.Error("the refused request was recorded")
	}
}

func TestALatePurgeNeverDeletesAMailboxRemovedAgainSinceItWasAsked(t *testing.T) {
	for _, c := range removedAgainOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)
			asked := ago(time.Minute)

			// Since Ana asked to delete it for good, in Mailie's own
			// screens: recovered, used and removed again.
			if _, err := f.svc.RecoverAccount(ctx, tm.ana, id); err != nil {
				t.Fatal(err)
			}
			c.used(t, f, tm, id)
			f.removed(t, tm.ana, id)
			kept, held := c.kept(t, f, tm, id), f.keptOfRemoved(t, tm, id)

			_, err := f.actsFor(t, delegated(tm.ana, "p1", ws, asked)).Purge(ctx, id)
			wantRefusal(t, "a deletion for good of a mailbox removed again since it was asked", err, service.RefusalSetSince)
			if now := f.keptOfRemoved(t, tm, id); now != held {
				t.Errorf("the removed mailbox kept %s, and keeps %s", held, now)
			}
			if n := c.kept(t, f, tm, id); n != kept {
				t.Errorf("what it kept beside its index was %d and is %d", kept, n)
			}
			if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "p1"); ran {
				t.Error("the refused request was recorded")
			}

			// Removed again before the request was asked: it runs.
			f.removalsMovedBack(t, 10*time.Minute)
			out, err := f.actsFor(t, delegated(tm.ana, "p2", ws, asked)).Purge(ctx, id)
			if err != nil || !out.Changed {
				t.Fatalf("deleting for good a mailbox removed before the request was asked: %+v, %v", out, err)
			}
			if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, id); n != 0 {
				t.Error("the mailbox is still there")
			}
		})
	}
}

func TestAPurgeMeetsARemovalMadeAgainBetweenTheReadAndTheWrite(t *testing.T) {
	for _, c := range removedAgainOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)
			kept, held := c.kept(t, f, tm, id), f.keptOfRemoved(t, tm, id)

			// Read under its first removal; recovered and removed again, in
			// Mailie's own screens, before the write.
			err := f.raceTheWrite(t, func() error {
				_, err := f.actsFor(t, delegated(tm.ana, "p1", ws, ago(time.Minute))).Purge(t.Context(), id)
				return err
			}, func(tx *sql.Tx) {
				recoverIn(ctx, t, tx, id, tm.ana.UserID)
				if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = ?`,
					id); err != nil {
					t.Fatal(err)
				}
				removeIn(ctx, t, tx, id, tm.ana.UserID)
			})
			wantRefusal(t, "a deletion for good of a mailbox removed again meanwhile", err, service.RefusalSetSince)
			if now := f.keptOfRemoved(t, tm, id); now != held {
				t.Errorf("the removed mailbox kept %s, and keeps %s", held, now)
			}
			if n := c.kept(t, f, tm, id); n != kept {
				t.Errorf("what it kept beside its index was %d and is %d", kept, n)
			}
		})
	}
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

func TestTurningOffARemovedPersonalMailboxRecoveredBeforeTheWriteIsAConflictThatKeepsItsIndex(t *testing.T) {
	for name, removedOnly := range map[string]bool{"plain": false, "removed_only": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			ana := f.person(t, "ana@example.com", auth.RoleMember)
			own := f.ownSynced(t, ana, "ana@mail.example")
			f.connect(t, own)
			f.seedIndex(t, own, "Okapi")
			f.removed(t, ana, own)
			indexed := f.indexed(t, own)
			if indexed == 0 {
				t.Fatal("the removed mailbox keeps no index")
			}
			req := switchSync(false)
			req.RemovedOnly = removedOnly

			// Read removed; recovered by Ana, in another tab, before the
			// write: back paused, as a recovery brings it.
			err := f.raceTheWrite(t, func() error {
				_, err := f.svc.SetMailboxSync(ctx, ana, own, req)
				return err
			}, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
					sync_paused_at = unixepoch(), sync_paused_by = ? WHERE id = ? AND removed_at <> 0`, ana.UserID, own); err != nil {
					t.Fatal(err)
				}
			})
			if !errors.Is(err, service.ErrNotRemoved) || service.CodeOf(err) != service.CodeConflict {
				t.Errorf("turning off the mailbox recovered meanwhile: %v, want conflict (ErrNotRemoved)", err)
			}
			if n := f.indexed(t, own); n != indexed {
				t.Errorf("the recovered mailbox keeps %d rows of its index, and kept %d", n, indexed)
			}
			if !f.paused(t, own) || f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, ana.UserID) == 0 {
				t.Errorf("recovered: paused %v, her consent %d; want paused under her consent", f.paused(t, own),
					f.count(t, `SELECT sync_consent_at FROM users WHERE id = ?`, ana.UserID))
			}
		})
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

// removedOfAna is a removed mailbox Ana recovers: the team's she owns, or
// her personal one.
type removedOfAna struct {
	what string
	// mailbox removes it and names it and its workspace.
	mailbox func(t *testing.T, f *fixture, tm keysTeam) (accountID, workspaceID string)
}

var removedOfAnas = []removedOfAna{
	{"the team's", func(t *testing.T, f *fixture, tm keysTeam) (string, string) {
		f.removed(t, tm.ana, tm.shared)
		return tm.shared, tm.id
	}},
	{"her personal one", func(t *testing.T, f *fixture, tm keysTeam) (string, string) {
		own := f.ownSynced(t, tm.ana, "ana@mail.example")
		f.connect(t, own)
		f.removed(t, tm.ana, own)
		return own, authtest.Personal(t, f.db, tm.ana.UserID)
	}},
}

func TestRecoveringAMailboxRecoveredBetweenTheReadAndTheWriteIsDone(t *testing.T) {
	for _, c := range removedOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)

			// Read removed; recovered by Ana, in Mailie's own screen, before
			// the write.
			var out service.ActOutcome
			err := f.raceTheWrite(t, func() (err error) {
				out, err = f.actsFor(t, delegated(tm.ana, "r1", ws, time.Now())).Recover(t.Context(), id)
				return err
			}, func(tx *sql.Tx) {
				recoverIn(ctx, t, tx, id, tm.ana.UserID)
			})
			if err != nil || out.Changed || out.Ran {
				t.Fatalf("recovering the mailbox recovered meanwhile: %+v, %v; want done, nothing changed", out, err)
			}
			if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); !ran {
				t.Error("the request was not recorded")
			}
			if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'recovered'`, id); n != 1 {
				t.Errorf("%d recoveries journaled, want the one that ran meanwhile", n)
			}
			if f.count(t, `SELECT removed_at FROM accounts WHERE id = ?`, id) != 0 {
				t.Error("the mailbox is removed")
			}
		})
	}
}

func TestRecoveringAMailboxRecoveredMeanwhileIsNotFoundToWhoNoLongerOwnsIt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.removed(t, tm.ana, tm.shared)

	// Read removed by Ana, its owner; recovered, and Ana made an admin,
	// before the write.
	err := f.raceTheWrite(t, func() error {
		_, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).Recover(t.Context(), tm.shared)
		return err
	}, func(tx *sql.Tx) {
		recoverIn(ctx, t, tx, tm.shared, tm.ana.UserID)
		if _, err := tx.ExecContext(ctx, `UPDATE workspace_members SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`,
			tm.id, tm.ana.UserID); err != nil {
			t.Fatal(err)
		}
	})
	wantRefusal(t, "Ana, an admin now, recovering the mailbox recovered meanwhile", err, service.RefusalNotFound)
	if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); ran {
		t.Error("the refused request was recorded")
	}
}

func TestRecoveringAMailboxDeletedForGoodBetweenTheReadAndTheWriteIsRecoveryClosed(t *testing.T) {
	for _, c := range removedOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)

			// Read removed; deleted for good by Ana, in Mailie's own screen,
			// before the write.
			err := f.raceTheWrite(t, func() error {
				_, err := f.actsFor(t, delegated(tm.ana, "r1", ws, time.Now())).Recover(t.Context(), id)
				return err
			}, func(tx *sql.Tx) {
				purgeIn(ctx, t, tx, id, tm.ana.UserID)
			})
			wantRefusal(t, "recovering the mailbox deleted for good meanwhile", err, service.RefusalRecoveryOver)
			wantCode(t, "recovering the mailbox deleted for good meanwhile", err, service.CodeConflict)
			if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); ran {
				t.Error("the refused request was recorded")
			}
		})
	}
}

func TestRemovedIndexOffOnAMailboxRecoveredBetweenTheReadAndTheWriteIsDoneAndTouchesNothing(t *testing.T) {
	for _, c := range removedOfAnas {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			id, ws := c.mailbox(t, f, tm)
			f.seedIndex(t, id, "Zebra")
			indexed := f.indexed(t, id)
			agreed := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, id)

			// Read removed; recovered by Ana before the write.
			var out service.ActOutcome
			err := f.raceTheWrite(t, func() (err error) {
				out, err = f.actsFor(t, delegated(tm.ana, "r1", ws, time.Now())).RemovedIndexOff(t.Context(), id)
				return err
			}, func(tx *sql.Tx) {
				recoverIn(ctx, t, tx, id, tm.ana.UserID)
			})
			if err != nil || out.Changed || out.Ran {
				t.Fatalf("turning off the kept index of the mailbox recovered meanwhile: %+v, %v; want done, nothing changed",
					out, err)
			}
			if _, ran, _ := f.svc.RequestOutcome(ctx, "ext", "r1"); !ran {
				t.Error("the request was not recorded")
			}
			if n := f.indexed(t, id); n != indexed {
				t.Errorf("the recovered mailbox keeps %d rows of its index, and kept %d", n, indexed)
			}
			if n := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, id); n != agreed {
				t.Errorf("its own agreement was %d and is %d", agreed, n)
			}
			if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'sync_paused'`, id); n != 0 {
				t.Errorf("%d turn-offs journaled", n)
			}
		})
	}
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

func TestOnlyAnOwnerTurnsSyncOffOnAMailboxRemovedBetweenTheReadAndTheWrite(t *testing.T) {
	// What a turn-off answers: the switch's sync over REST, and whether the
	// act changed anything through an extension.
	type answer struct {
		state   string
		changed bool
	}
	type turnOff struct {
		who     string
		allowed bool
		act     func(f *fixture, tm keysTeam) (answer, error)
	}
	rest := func(p func(keysTeam) service.Principal) func(*fixture, keysTeam) (answer, error) {
		return func(f *fixture, tm keysTeam) (answer, error) {
			st, err := f.svc.SetMailboxSync(t.Context(), p(tm), tm.shared, switchSync(false))
			return answer{state: st.State}, err
		}
	}
	extension := func(p func(keysTeam) service.Principal) func(*fixture, keysTeam) (answer, error) {
		return func(f *fixture, tm keysTeam) (answer, error) {
			out, err := f.actsFor(t, delegated(p(tm), "off", tm.id, ago(time.Minute))).TeamSync(t.Context(), tm.shared, false, "")
			return answer{changed: out.Changed}, err
		}
	}
	carol := func(tm keysTeam) service.Principal { return tm.carol }
	ana := func(tm keysTeam) service.Principal { return tm.ana }
	for _, c := range []turnOff{
		{"Carol, an admin", false, rest(carol)},
		{"Carol, an admin, through an extension", false, extension(carol)},
		{"Ana, its owner", true, rest(ana)},
		{"Ana, its owner, through an extension", true, extension(ana)},
	} {
		t.Run(c.who, func(t *testing.T) {
			f := newFixture(t)
			tm := syncedTeam(t, f)
			f.seedIndex(t, tm.shared, "Zebra")
			// Agreed before the extension's request was first asked.
			f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 300 WHERE id = ?`, tm.shared)
			kept := f.indexed(t, tm.shared)

			// Read live; removed by Ana before the write.
			var got answer
			err := f.raceTheWrite(t, func() (err error) {
				got, err = c.act(f, tm)
				return err
			}, func(tx *sql.Tx) {
				removeIn(t.Context(), t, tx, tm.shared, tm.ana.UserID)
			})
			if f.count(t, `SELECT removed_at <> 0 FROM accounts WHERE id = ?`, tm.shared) != 1 {
				t.Fatal("the mailbox is no longer removed")
			}
			turnedOff := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'sync_paused'`, tm.shared)
			if !c.allowed {
				wantCode(t, c.who+" turning off the mailbox removed meanwhile", err, service.CodeNotFound)
				if n := f.indexed(t, tm.shared); n != kept {
					t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", kept, n)
				}
				if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) == 0 {
					t.Error("the team's agreement was withdrawn")
				}
				if turnedOff != 0 {
					t.Errorf("%d turn-offs journaled", turnedOff)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s turning off the mailbox removed meanwhile: %v", c.who, err)
			}
			if got.state != "" && got.state != "off" || got.state == "" && !got.changed {
				t.Errorf("%s turning off the mailbox removed meanwhile is answered %+v", c.who, got)
			}
			if n := f.indexed(t, tm.shared); n != 0 {
				t.Errorf("the removed mailbox keeps %d rows of its index", n)
			}
			if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) != 0 {
				t.Error("the team's agreement stands")
			}
			if turnedOff != 1 {
				t.Errorf("%d turn-offs journaled, want one", turnedOff)
			}
		})
	}
}

func TestTheOperatorsTurnOffOfAnOperatorMailboxRemovedMeanwhileChangesNothing(t *testing.T) {
	f := newFixture(t)
	ops := f.opsSynced(t, "ops@mail.example")
	f.seedIndex(t, ops, "Zebra")
	kept := f.indexed(t, ops)

	// Read live; removed by the operator before the write. Removed, it is
	// not found on this path, as it is when asked after the removal: its
	// kept index is the operator's to delete with StopMailboxSync.
	err := f.raceTheWrite(t, func() error {
		_, err := f.svc.SetMailboxSync(t.Context(), admin(), ops, switchSync(false))
		return err
	}, func(tx *sql.Tx) {
		removeIn(t.Context(), t, tx, ops, "cli")
	})
	wantCode(t, "the operator turning off the mailbox removed meanwhile", err, service.CodeNotFound)
	_, err = f.svc.SetMailboxSync(t.Context(), admin(), ops, switchSync(false))
	wantCode(t, "the operator turning off the removed mailbox", err, service.CodeNotFound)
	if n := f.indexed(t, ops); n != kept {
		t.Errorf("the removed mailbox kept %d rows of its index, and keeps %d", kept, n)
	}
	if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, ops) == 0 {
		t.Error("the operator's switch was turned off")
	}
}

func TestTurningOnOrResumingATeamMailboxRemovedBetweenTheReadAndTheWriteIsNotFound(t *testing.T) {
	type turnOn struct {
		what string
		// paused: the act resumes a paused mailbox; otherwise it turns on
		// one switched off.
		paused bool
		act    func(f *fixture, tm keysTeam) error
	}
	on := true
	for _, c := range []turnOn{
		{"Carol, an admin, turning it on", false, func(f *fixture, tm keysTeam) error {
			_, err := f.svc.SetMailboxSync(t.Context(), tm.carol, tm.shared,
				service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync})
			return err
		}},
		{"Ana, its owner, turning it on", false, func(f *fixture, tm keysTeam) error {
			_, err := f.svc.SetMailboxSync(t.Context(), tm.ana, tm.shared,
				service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync})
			return err
		}},
		{"Carol, an admin, turning it on through an extension", false, func(f *fixture, tm keysTeam) error {
			_, err := f.actsFor(t, delegated(tm.carol, "on", tm.id, time.Now())).TeamSync(t.Context(), tm.shared, true,
				f.consent().Sync)
			return err
		}},
		{"Carol, an admin, resuming it", true, func(f *fixture, tm keysTeam) error {
			_, err := f.svc.ResumeMailboxSync(t.Context(), tm.carol, tm.shared,
				service.SyncResumeRequest{Version: f.consent().Sync})
			return err
		}},
		{"Carol, an admin, resuming it through an extension", true, func(f *fixture, tm keysTeam) error {
			_, err := f.actsFor(t, delegated(tm.carol, "resume", tm.id, time.Now())).ResumeSync(t.Context(), tm.shared,
				f.consent().Sync)
			return err
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := newFixture(t)
			tm := syncedTeam(t, f)
			if c.paused {
				f.pause(t, tm.ana, tm.shared)
			} else if _, err := f.svc.SetMailboxSync(t.Context(), tm.ana, tm.shared, switchSync(false)); err != nil {
				t.Fatal(err)
			}
			agreed := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)

			// Read live; removed by Ana before the write, its pause ended
			// first, as a removal does.
			err := f.raceTheWrite(t, func() error { return c.act(f, tm) }, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(t.Context(), `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = ''
					WHERE id = ?`, tm.shared); err != nil {
					t.Fatal(err)
				}
				removeIn(t.Context(), t, tx, tm.shared, tm.ana.UserID)
			})
			wantCode(t, c.what+" once removed meanwhile", err, service.CodeNotFound)
			if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) != agreed {
				t.Error("the team's agreement changed")
			}
		})
	}
}
