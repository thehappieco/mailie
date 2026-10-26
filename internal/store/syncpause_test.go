package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

// Pausing one mailbox's sync and resuming it, as the store writes them
// (migration 0025). The seed, at the latest schema: acc_t1 is a mailbox of
// the team wsp_t1 under a consent migration 0011 bound to Bea (usr_2), which
// Bea and Fay (usr_6) read and Owner (usr_1), its team's owner, manages;
// acc_t3 one of wsp_t2 never agreed to; acc_t2 one whose bound consent
// stopped; acc_5 an operator mailbox switched on, acc_6 another; acc_1 and
// acc_2 Owner's own, acc_3 Bea's own, both of them consenting; acc_4 Cid's
// own, who never consented.

const (
	pOwner = "usr_0000000000000001"
	pBea   = "usr_0000000000000002"
	pFay   = "usr_0000000000000006"
	pOwn1  = "acc_0000000000000001"
	pOwn2  = "acc_0000000000000002"
	pBeas  = "acc_0000000000000003"
	pCids  = "acc_0000000000000004"
	pOps   = "acc_0000000000000005"
	pOps2  = "acc_0000000000000006"
)

func (f seededSchema) paused(id string) bool {
	f.t.Helper()
	return queryInt(f.t, f.s, `SELECT sync_paused_at FROM accounts WHERE id = ?`, id) != 0
}

func (f seededSchema) pauseSync(id, actor string) MailboxSyncSwitch {
	f.t.Helper()
	switched, err := f.s.PauseSync(context.Background(), id, actor, nil)
	if err != nil {
		f.t.Fatalf("pausing %s: %v", id, err)
	}
	return switched
}

func (f seededSchema) noticeKinds(user string) []string {
	f.t.Helper()
	got, err := f.s.Notices(context.Background(), user)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, n := range got {
		out = append(out, string(n.Event.Kind)+" "+n.Event.AccountID)
	}
	return out
}

func TestPausingKeepsTheIndexTheConsentAndEveryReader(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	readers := func() []string {
		t.Helper()
		var out []string
		if err := f.s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = MailboxReadersTx(ctx, tx, "acc_t1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	kept := func() []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT (SELECT count(*) FROM messages WHERE account_id = 'acc_t1') || ' ' ||
			(SELECT count(*) FROM folders WHERE account_id = 'acc_t1') || ' ' ||
			(SELECT count(*) FROM events WHERE account_id = 'acc_t1') || ' ' ||
			sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_consent_version || ' ' || sync_enabled_via || ' ' ||
			(SELECT count(*) FROM mailbox_access g WHERE g.account_id = 'acc_t1' AND `+KeptReaderSQL("g")+`)
			FROM accounts WHERE id = 'acc_t1'`)
	}
	state := func() []string {
		t.Helper()
		return append(kept(), readers()...)
	}
	before := state()
	if switched := f.pauseSync("acc_t1", pOwner); !switched.Changed {
		t.Fatal("the pause changed nothing")
	}
	if after := state(); !slices.Equal(after, before) {
		t.Errorf("pausing changed the index, the consent or the readers: %v, was %v", after, before)
	}
	if !f.paused("acc_t1") {
		t.Error("the mailbox is not paused")
	}
}

func TestPausingAPausedMailboxAndResumingAnUnpausedOneWriteNothing(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	row := func() []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT updated_at || ' ' || sync_enabled_at || ' ' || sync_enabled_by || ' ' ||
			sync_consent_version || ' ' || sync_enabled_via || ' ' || sync_paused_at || ' ' || sync_paused_by || ' ' ||
			(SELECT count(*) FROM mailbox_events) FROM accounts WHERE id = 'acc_t1'`)
	}
	before := row()
	if switched, err := f.s.ResumeSync(ctx, "acc_t1", pOwner, "sync-2", nil); err != nil || switched.Changed {
		t.Errorf("resuming a mailbox that is not paused: %+v (%v)", switched, err)
	}
	if got := row(); !slices.Equal(got, before) {
		t.Errorf("resuming a mailbox that is not paused wrote %v, was %v", got, before)
	}
	f.s.now = func() time.Time { return time.Unix(2000, 0) }
	f.pauseSync("acc_t1", pOwner)
	paused := row()
	f.s.now = func() time.Time { return time.Unix(3000, 0) }
	if switched, err := f.s.PauseSync(ctx, "acc_t1", pBea, nil); err != nil || switched.Changed || len(switched.Told) != 0 {
		t.Errorf("pausing a paused mailbox: %+v (%v)", switched, err)
	}
	if got := row(); !slices.Equal(got, paused) {
		t.Errorf("pausing a paused mailbox wrote %v, was %v", got, paused)
	}
}

func TestATeamPauseTellsItsOtherReadersAndItsResumeWithdrawsTheNotices(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	switched := f.pauseSync("acc_t1", pBea)
	if !switched.Changed || !slices.Equal(switched.Told, []string{pFay}) {
		t.Fatalf("Bea's pause: %+v, want Fay told", switched)
	}
	if got := f.noticeKinds(pFay); !slices.Equal(got, []string{"paused acc_t1"}) {
		t.Errorf("Fay's notices: %v", got)
	}
	if got := f.noticeKinds(pBea); len(got) != 0 {
		t.Errorf("Bea is told of her own pause: %v", got)
	}
	resumed, err := f.s.ResumeSync(ctx, "acc_t1", pOwner, "sync-2", nil)
	if err != nil || !resumed.Changed || len(resumed.Told) != 0 {
		t.Fatalf("Owner's resume: %+v (%v)", resumed, err)
	}
	if got := f.noticeKinds(pFay); len(got) != 0 {
		t.Errorf("Fay's notices once resumed: %v", got)
	}
	if got := queryStrings(t, f.s, `SELECT e.kind || ' ' || e.actor || ' ' || e.cause || ' ' ||
		(SELECT group_concat(r.user_id) FROM (SELECT user_id FROM mailbox_event_readers WHERE event_seq = e.seq ORDER BY user_id) r)
		|| ' ' || (SELECT count(*) FROM notices n WHERE n.event_seq = e.seq) FROM mailbox_events e ORDER BY e.seq`); !slices.Equal(got, []string{
		"paused " + pBea + "  " + pBea + "," + pFay + " 0",
		"resumed " + pOwner + "  " + pBea + "," + pFay + " 0",
	}) {
		t.Errorf("the journal: %v", got)
	}
	if f.paused("acc_t1") {
		t.Error("the mailbox is still paused")
	}
}

func TestAPersonalOrOperatorPauseIsNotJournaled(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	for id, actor := range map[string]string{pBeas: pBea, pOps: "cli"} {
		if switched := f.pauseSync(id, actor); !switched.Changed || len(switched.Told) != 0 {
			t.Errorf("pausing %s: %+v", id, switched)
		}
		if switched, err := f.s.ResumeSync(ctx, id, actor, "", nil); err != nil || !switched.Changed {
			t.Errorf("resuming %s: %+v (%v)", id, switched, err)
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`); n != 0 {
		t.Errorf("%d entries journaled for a personal and an operator mailbox's pause and resume", n)
	}
}

func TestATeamResumeRecordsTheTeamsAgreementByTheResumer(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	version := queryStrings(t, f.s, `SELECT sync_consent_version FROM accounts WHERE id = 'acc_t1'`)[0]
	f.pauseSync("acc_t1", pOwner)
	f.s.now = func() time.Time { return time.Unix(5000, 0) }
	// At the revision it already had, bound to Bea by 0011: the resumer
	// agrees again, which confirms it.
	if switched, err := f.s.ResumeSync(ctx, "acc_t1", pOwner, version, nil); err != nil || !switched.Changed {
		t.Fatalf("resuming: %+v (%v)", switched, err)
	}
	if got := queryStrings(t, f.s, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_consent_version || ' ' ||
		sync_enabled_via || ' ' || sync_paused_at FROM accounts WHERE id = 'acc_t1'`); !slices.Equal(got, []string{
		"5000 " + pOwner + " " + version + "  0",
	}) {
		t.Errorf("the agreement recorded: %v", got)
	}
	if got := queryStrings(t, f.s, `SELECT kind || ' ' || actor || ' ' || created_at FROM mailbox_events
		WHERE kind = 'resumed'`); !slices.Equal(got, []string{"resumed " + pOwner + " 5000"}) {
		t.Errorf("the resume journaled: %v", got)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices`); n != 0 {
		t.Errorf("a resume told %d people", n)
	}
}

func TestAPersonalOrOperatorResumeRecordsNothing(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	consent := func() []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT id || ' ' || sync_enabled_at || ' ' || sync_enabled_by || ' ' ||
			sync_consent_version || ' ' || (SELECT sync_consent_at || sync_consent_version FROM users u
			WHERE u.id = accounts.owner_user_id) FROM accounts WHERE id IN (?, ?) ORDER BY id`, pBeas, pOps)
	}
	before := consent()
	for id, actor := range map[string]string{pBeas: pBea, pOps: "cli"} {
		f.pauseSync(id, actor)
		if switched, err := f.s.ResumeSync(ctx, id, actor, "", nil); err != nil || !switched.Changed || f.paused(id) {
			t.Errorf("resuming %s: %+v (%v)", id, switched, err)
		}
	}
	if got := consent(); !slices.Equal(got, before) {
		t.Errorf("a resume recorded a consent: %v, was %v", got, before)
	}
}

func TestAResumeThatDoesNotMatchItsMailboxIsRefused(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.pauseSync("acc_t1", pOwner)
	f.pauseSync(pBeas, pBea)
	for id, version := range map[string]string{"acc_t1": "", pBeas: "sync-2"} {
		if _, err := f.s.ResumeSync(ctx, id, pOwner, version, nil); !errors.Is(err, ErrResumeAgreement) {
			t.Errorf("resuming %s naming %q: %v, want ErrResumeAgreement", id, version, err)
		}
		if !f.paused(id) {
			t.Errorf("%s was resumed all the same", id)
		}
	}
}

func TestPausedAccountsListsThePausedMailboxesWhoseIndexStillFollows(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	list := func() []string {
		t.Helper()
		got, err := f.s.PausedAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	f.pauseSync("acc_t1", pOwner)
	f.pauseSync(pBeas, pBea)
	f.pauseSync(pOps, "cli")
	if got := list(); !slices.Equal(got, []string{"acc_t1", pBeas, pOps}) {
		t.Errorf("the paused accounts: %v", got)
	}
	f.accepted("its team closed", f.exec(`UPDATE workspaces SET closed_at = 900 WHERE id = 'wsp_t1'`))
	f.accepted("Bea suspended", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?`, pBea))
	if got := list(); !slices.Equal(got, []string{pOps}) {
		t.Errorf("the paused accounts whose index still follows: %v", got)
	}
	if ok, err := f.s.SyncEligible(ctx, pOwn1); err != nil || !ok || slices.Contains(list(), pOwn1) {
		t.Errorf("a syncing mailbox is listed paused, or does not sync: %v (%v)", ok, err)
	}
}

func TestOnlyAMailboxWhoseConsentStandsAndWhoseSyncIsNotHeldIsPaused(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.accepted("the operator's second switched off", f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ''
		WHERE id = ?`, pOps2))
	f.accepted("Owner's first held", f.exec(`UPDATE accounts SET sync_held_at = 900 WHERE id = ?`, pOwn1))
	f.remove(pOwn2, pOwner, 900)
	for _, c := range []struct {
		id, what string
		want     error
	}{
		{pCids, "a person's who never consented", ErrPauseSyncOff},
		{"acc_t3", "a team's never agreed to", ErrPauseSyncOff},
		{pOps2, "an operator mailbox switched off", ErrPauseSyncOff},
		{"acc_t2", "a team's whose bound consent stopped", ErrPauseSyncOff},
		{pOwn1, "a person's the operator turned off", ErrPauseHeld},
		{pOwn2, "a removed one", ErrNoSuchMailbox},
		{"acc_nobody", "one nobody knows", ErrNoSuchMailbox},
	} {
		if _, err := f.s.PauseSync(ctx, c.id, pOwner, nil); !errors.Is(err, c.want) {
			t.Errorf("pausing %s: %v, want %v", c.what, err, c.want)
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM accounts WHERE sync_paused_at <> 0`); n != 0 {
		t.Errorf("%d mailboxes paused", n)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`); n != 0 {
		t.Errorf("%d entries journaled", n)
	}
}

func TestEveryEndOfAConsentEndsThePauseAndWithdrawsItsNotices(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	// Bea withdraws: her own mailbox and the team mailbox bound to her stop,
	// which the schema would refuse were their pauses not ended first. Fay
	// keeps the turn-off's notice alone.
	f.pauseSync(pBeas, pBea)
	f.pauseSync("acc_t1", pOwner)
	if _, err := f.s.WithdrawSyncConsent(ctx, pBea); err != nil {
		t.Fatalf("Bea's withdrawal: %v", err)
	}
	for _, id := range []string{pBeas, "acc_t1"} {
		if f.paused(id) {
			t.Errorf("%s is still paused after Bea's withdrawal", id)
		}
	}
	if got := f.noticeKinds(pFay); !slices.Equal(got, []string{"sync_paused acc_t1"}) {
		t.Errorf("Fay's notices after Bea's withdrawal: %v", got)
	}

	// The team turns it on again; turned off while paused, the pause ends.
	if _, err := f.s.SetMailboxSync(ctx, "acc_t1", true, pOwner, "sync-2", nil); err != nil {
		t.Fatal(err)
	}
	f.pauseSync("acc_t1", pOwner)
	if switched, err := f.s.SetMailboxSync(ctx, "acc_t1", false, pOwner, "", nil); err != nil || !switched.Changed ||
		f.paused("acc_t1") {
		t.Errorf("turning the paused team mailbox off: %+v (%v), paused %v", switched, err, f.paused("acc_t1"))
	}
	if got := f.noticeKinds(pFay); !slices.Equal(got, []string{"sync_paused acc_t1", "sync_paused acc_t1"}) {
		t.Errorf("Fay's notices after the team's turn-off: %v", got)
	}

	// The operator's turn-off, of a team mailbox and of a person's.
	if _, err := f.s.SetMailboxSync(ctx, "acc_t1", true, pOwner, "sync-2", nil); err != nil {
		t.Fatal(err)
	}
	f.pauseSync("acc_t1", pOwner)
	f.pauseSync(pOwn1, pOwner)
	for _, id := range []string{"acc_t1", pOwn1} {
		if _, err := f.s.StopSync(ctx, id, ActorOperator); err != nil {
			t.Fatalf("the operator's turn-off of %s: %v", id, err)
		}
		if f.paused(id) {
			t.Errorf("%s is still paused after the operator's turn-off", id)
		}
	}
	if got := queryStrings(t, f.s, `SELECT id || ' ' || sync_held_at || ' ' || sync_enabled_at FROM accounts
		WHERE id IN ('acc_t1', ?) ORDER BY id`, pOwn1); len(got) != 2 || got[0] == pOwn1+" 0 0" || got[1] != "acc_t1 0 0" {
		t.Errorf("after the operator's turn-off: %v, want the person's held and the team's off", got)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq
		WHERE e.kind = 'paused'`); n != 0 {
		t.Errorf("%d notices of a pause outlive its end", n)
	}
	// And the operator mailbox's own switch.
	f.pauseSync(pOps, "cli")
	if switched, err := f.s.SetMailboxSync(ctx, pOps, false, "cli", "", nil); err != nil || !switched.Changed || f.paused(pOps) {
		t.Errorf("switching the paused operator mailbox off: %+v (%v)", switched, err)
	}
}

func TestSwitchingATeamMailboxOnAgainLeavesItsPause(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.pauseSync("acc_t1", pOwner)
	switched, err := f.s.SetMailboxSync(ctx, "acc_t1", true, pOwner, "sync-2", nil)
	if err != nil || !switched.Changed {
		t.Fatalf("a new revision agreed to: %+v (%v)", switched, err)
	}
	if got := queryStrings(t, f.s, `SELECT sync_consent_version || ' ' || sync_enabled_by || ' ' || (sync_paused_at <> 0)
		FROM accounts WHERE id = 'acc_t1'`); !slices.Equal(got, []string{"sync-2 " + pOwner + " 1"}) {
		t.Errorf("after the new revision: %v, want it recorded and still paused", got)
	}
}

func TestTheOperatorsTurnOffHoldsAPersonalMailboxLiveOrRemoved(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.remove(pOwn2, pOwner, 900)
	first, err := f.s.StopSync(ctx, pOwn2, ActorOperator)
	if err != nil || !first.Changed {
		t.Fatalf("the operator's turn-off of a removed person's mailbox: %+v (%v)", first, err)
	}
	if held := queryInt(t, f.s, `SELECT sync_held_at FROM accounts WHERE id = ?`, pOwn2); held == 0 {
		t.Error("the removed personal mailbox is not held")
	}
	journaled := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, pOwn2)
	if journaled != 1 {
		t.Errorf("the turn-off journaled %d entries, want one: its index was deleted", journaled)
	}
	if second, err := f.s.StopSync(ctx, pOwn2, ActorOperator); err != nil || second.Changed {
		t.Errorf("a second turn-off: %+v (%v), want nothing changed", second, err)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, pOwn2); n != journaled {
		t.Errorf("a second turn-off journaled again: %d entries", n)
	}
	if _, err := f.s.StopSync(ctx, "acc_t1", ActorOperator); err != nil {
		t.Fatal(err)
	}
	if held := queryInt(t, f.s, `SELECT sync_held_at FROM accounts WHERE id = 'acc_t1'`); held != 0 {
		t.Error("a team mailbox is held")
	}
}

func TestACheckThatRefusesAPauseOrAResumeWritesNothing(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	refused := errors.New("not yours")
	no := func(*sql.Tx) error { return refused }
	if _, err := f.s.PauseSync(ctx, "acc_t1", pOwner, no); !errors.Is(err, refused) {
		t.Errorf("a refused pause: %v", err)
	}
	if f.paused("acc_t1") || queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`) != 0 {
		t.Error("a refused pause wrote something")
	}
	f.pauseSync("acc_t1", pOwner)
	agreement := queryStrings(t, f.s, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_enabled_via
		FROM accounts WHERE id = 'acc_t1'`)
	if _, err := f.s.ResumeSync(ctx, "acc_t1", pOwner, "sync-2", no); !errors.Is(err, refused) {
		t.Errorf("a refused resume: %v", err)
	}
	if !f.paused("acc_t1") || queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE kind = 'resumed'`) != 0 {
		t.Error("a refused resume wrote something")
	}
	if got := queryStrings(t, f.s, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_enabled_via
		FROM accounts WHERE id = 'acc_t1'`); !slices.Equal(got, agreement) {
		t.Errorf("a refused resume recorded an agreement: %v, was %v", got, agreement)
	}
}
