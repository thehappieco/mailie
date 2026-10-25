package service_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The operator's turn-off (StopMailboxSync): any workspace's mailbox, live or
// removed, its sync turned off and its index deleted, its readers told as of
// any turn-off, and the journal naming the operator; whoever may turns it on
// again afterwards.

// indexed counts what a mailbox's index holds: its messages and its folders.
func (f *fixture) indexed(t *testing.T, accountID string) int {
	t.Helper()
	return f.count(t, `SELECT (SELECT count(*) FROM messages WHERE account_id = ?) + (SELECT count(*) FROM folders WHERE account_id = ?)`,
		accountID, accountID)
}

// operatorStopped turns a mailbox's sync off as the operator, and fails the test
// unless its index is gone.
func (f *fixture) operatorStopped(t *testing.T, accountID string) service.AccountSync {
	t.Helper()
	st, err := f.svc.StopMailboxSync(t.Context(), admin(), accountID)
	if err != nil {
		t.Fatalf("the operator stopping %s: %v", accountID, err)
	}
	if n := f.indexed(t, accountID); n != 0 {
		t.Errorf("%s keeps %d rows of its index", accountID, n)
	}
	return st
}

// operatorPaused reports whether the journal holds a sync turned off by the
// operator for the mailbox.
func operatorPaused(t *testing.T, f *fixture, accountID string) bool {
	t.Helper()
	return slices.Contains(f.journalOf(t, accountID), "sync_paused "+store.ActorOperator+" ")
}

func TestTheOperatorTurnsATeamMailboxsSyncOffAndItsOwnersMayTurnItOnAgain(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	tm.syncOn(t, f, tm.shared)
	f.seedIndex(t, tm.shared, "Quartz")

	st := f.operatorStopped(t, tm.shared)
	if st.Enabled || st.Held {
		t.Errorf("the mailbox's sync after the operator's turn-off: %+v", st)
	}
	if n := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared); n != 0 {
		t.Error("the team's consent to sync stands")
	}
	if f.eligible(t, tm.shared) {
		t.Error("the mailbox still syncs")
	}
	// Journaled as the operator's, and both readers told: the operator is
	// neither.
	if !operatorPaused(t, f, tm.shared) {
		t.Errorf("the journal holds %q, want the operator's sync_paused", f.journalOf(t, tm.shared))
	}
	for name, p := range map[string]service.Principal{"Ana": tm.ana, "Bea": tm.bea} {
		if kinds := noticeKinds(f.notices(t, p)); !slices.Equal(kinds, []string{"sync_off"}) {
			t.Errorf("%s is told %v, want the sync turned off", name, kinds)
		}
	}

	// Its owner turns it on again, to the current text, as after any
	// turn-off.
	tm.syncOn(t, f, tm.shared)
	if !f.eligible(t, tm.shared) {
		t.Error("the team's consent given again does not sync it")
	}
}

func TestTheOperatorHoldsAPersonsMailboxUntilTheyTurnItOnAgainAndKeepsTheirConsent(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	mine := f.mailbox(t, ana, "ana@mail.example")
	other := f.mailbox(t, ana, "ana.other@mail.example")
	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	f.seedIndex(t, mine, "Quartz")
	f.seedIndex(t, other, "Basalt")

	if st := f.operatorStopped(t, mine); !st.Held {
		t.Errorf("Ana's mailbox after the operator's turn-off: %+v, want it held", st)
	}
	if f.eligible(t, mine) {
		t.Error("the mailbox still syncs")
	}
	// Her consent is hers: it stands, and her other mailbox keeps its index
	// and its sync.
	if c, err := f.svc.SyncConsent(t.Context(), ana); err != nil || !c.Consented {
		t.Errorf("Ana's consent after the operator's turn-off: %+v, %v", c, err)
	}
	if f.indexed(t, other) == 0 || !f.eligible(t, other) {
		t.Error("Ana's other mailbox lost its index or its sync")
	}
	if !operatorPaused(t, f, mine) {
		t.Errorf("the journal holds %q, want the operator's sync_paused", f.journalOf(t, mine))
	}
	if kinds := noticeKinds(f.notices(t, ana)); !slices.Equal(kinds, []string{"sync_off"}) {
		t.Errorf("Ana is told %v, want the sync turned off", kinds)
	}

	// She turns it on again.
	on := true
	if _, err := f.svc.SetMailboxSync(t.Context(), ana, mine, service.MailboxSyncRequest{Enabled: &on}); err != nil {
		t.Fatalf("Ana turning her mailbox on again: %v", err)
	}
	if !f.eligible(t, mine) {
		t.Error("Ana's mailbox does not sync once she turned it on")
	}
}

func TestTheOperatorTellsAPersonWhoseRecoveredMailboxKeptItsIndexOnce(t *testing.T) {
	// A recovered mailbox is held and keeps its index until its person
	// turns it on: the operator's turn-off deletes that index, and says so,
	// once.
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	mine := f.mailbox(t, ana, "ana@mail.example")
	f.connect(t, mine)
	if _, err := f.svc.GrantSyncConsent(ctx, ana, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	f.seedIndex(t, mine, "Quartz")
	f.removed(t, ana, mine)
	if _, err := f.svc.RecoverAccount(ctx, ana, mine); err != nil {
		t.Fatal(err)
	}
	if f.indexed(t, mine) == 0 {
		t.Fatal("the recovered mailbox did not keep its index")
	}

	if st := f.operatorStopped(t, mine); !st.Held {
		t.Errorf("Ana's recovered mailbox after the operator's turn-off: %+v, want it held", st)
	}
	if !operatorPaused(t, f, mine) {
		t.Errorf("the journal holds %q, want the operator's sync_paused", f.journalOf(t, mine))
	}
	if kinds := noticeKinds(f.notices(t, ana)); !slices.Equal(kinds, []string{"sync_off"}) {
		t.Errorf("Ana is told %v, want the sync turned off", kinds)
	}

	// Again, with nothing left to delete: nothing more is journaled or told.
	f.operatorStopped(t, mine)
	paused := 0
	for _, entry := range f.journalOf(t, mine) {
		if strings.HasPrefix(entry, "sync_paused ") {
			paused++
		}
	}
	if paused != 1 {
		t.Errorf("the journal holds %q, want one sync_paused", f.journalOf(t, mine))
	}
	if kinds := noticeKinds(f.notices(t, ana)); !slices.Equal(kinds, []string{"sync_off"}) {
		t.Errorf("Ana is told %v after a second turn-off, want the one notice", kinds)
	}
}

func TestTheOperatorTurnsOffARemovedOrAClosedTeamsMailboxAndAWithheldTeamsToo(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	const removed, closed = "acc_00000000000000r1", "acc_00000000000000c1"
	for _, id := range []string{removed, closed} {
		tm.link(t, f, id, id+"@mail.example")
		f.connect(t, id)
		tm.syncOn(t, f, id)
		f.seedIndex(t, id, "Kept")
	}
	f.removed(t, tm.ana, removed)
	f.operatorStopped(t, removed)
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, removed); n != 1 {
		t.Error("the removed mailbox is no longer kept")
	}
	listed, err := f.svc.ListRemovedMailboxes(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 1 || listed[0].IndexKept {
		t.Errorf("the removed mailboxes after the operator's turn-off: %+v, %v", listed, err)
	}
	if !operatorPaused(t, f, removed) {
		t.Errorf("the journal holds %q, want the operator's sync_paused", f.journalOf(t, removed))
	}

	f.closeTeam(t, tm.id)
	f.operatorStopped(t, closed)
	if n := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, closed); n != 0 {
		t.Error("the closed team's consent to sync stands")
	}
	f.reopenTeam(t, tm.id)

	// Where the edition withholds teams, it is the operator's erasure all
	// the same.
	tm.syncOn(t, f, closed)
	f.seedIndex(t, closed, "Again")
	f.withholdTeams(t)
	f.operatorStopped(t, closed)
}

func TestOnlyTheOperatorTurnsAnyMailboxsSyncOff(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	tm.syncOn(t, f, tm.shared)
	f.seedIndex(t, tm.shared, "Quartz")
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "read", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	restricted := admin()
	restricted.AccountIDs = []string{tm.shared}
	for name, p := range map[string]service.Principal{
		"Ana, the team's owner": tm.ana, "the team's key": f.authenticate(t, created.Key),
		"an instance key of the read scope": reader(), "an instance key restricted to the mailbox": restricted,
	} {
		_, err := f.svc.StopMailboxSync(ctx, p, tm.shared)
		wantCode(t, name+" stopping the mailbox's sync", err, service.CodeNotAuthorized)
	}
	if f.indexed(t, tm.shared) == 0 || !f.eligible(t, tm.shared) {
		t.Error("a refused turn-off changed the mailbox's sync")
	}
	_, err = f.svc.StopMailboxSync(ctx, admin(), "acc_00000000000000zz")
	wantCode(t, "the operator stopping a mailbox nobody knows", err, service.CodeNotFound)

	// An operator mailbox's, which nobody else reads, is journaled for nobody.
	ops := f.mailbox(t, admin(), "ops@mail.example")
	if _, err := f.svc.SetMailboxSync(ctx, admin(), ops, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	f.seedIndex(t, ops, "Ops")
	f.operatorStopped(t, ops)
	if got := f.journalOf(t, ops); len(got) != 0 {
		t.Errorf("the operator's own mailbox is journaled %q", got)
	}
}
