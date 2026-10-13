package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// What a recovery's hold and the notices of a sync turned off do to the
// rules this package decides (migration 0020).

func TestSwitchingATeamMailboxOffTellsItsOtherReadersAndEitherWayLiftsAHold(t *testing.T) {
	// The seed at schema 20: acc_t1 is a mailbox of the team wsp_t1, under
	// its consent, which Bea (usr_2) and Fay (usr_6) read;
	// acc_0000000000000005 is an operator mailbox.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	const owner, bea, fay = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006"
	held := func(id string) bool {
		t.Helper()
		return queryInt(t, f.s, `SELECT sync_held_at FROM accounts WHERE id = ?`, id) != 0
	}
	hold := func(id string) {
		t.Helper()
		f.accepted("held", f.exec(`UPDATE accounts SET sync_held_at = 950 WHERE id = ?`, id))
	}

	// On, at the revision it already has, a held mailbox is let go.
	version := queryStrings(t, f.s, `SELECT sync_consent_version FROM accounts WHERE id = 'acc_t1'`)[0]
	hold("acc_t1")
	switched, err := f.s.SetMailboxSync(ctx, "acc_t1", true, owner, version, nil)
	if err != nil || !switched.Changed || len(switched.Told) != 0 || held("acc_t1") {
		t.Fatalf("turning the held mailbox on: %+v (%v), held %v", switched, err, held("acc_t1"))
	}
	if switched, err := f.s.SetMailboxSync(ctx, "acc_t1", true, owner, version, nil); err != nil || switched.Changed {
		t.Errorf("turning it on again: %+v (%v), want nothing changed", switched, err)
	}

	// Off, by Bea: Fay is told, Bea is not, and a hold goes too.
	hold("acc_t1")
	switched, err = f.s.SetMailboxSync(ctx, "acc_t1", false, bea, "", nil)
	if err != nil || !switched.Changed || !slices.Equal(switched.Told, []string{fay}) || held("acc_t1") {
		t.Fatalf("Bea turning it off: %+v (%v), held %v", switched, err, held("acc_t1"))
	}
	if got := queryStrings(t, f.s, `SELECT e.kind || ' ' || e.actor || ' ' || r.user_id || ' ' || (n.user_id IS NOT NULL)
		FROM mailbox_events e JOIN mailbox_event_readers r ON r.event_seq = e.seq
		LEFT JOIN notices n ON n.event_seq = e.seq AND n.user_id = r.user_id
		WHERE e.account_id = 'acc_t1' ORDER BY r.user_id`); !slices.Equal(got, []string{
		"sync_paused " + bea + " " + bea + " 0", "sync_paused " + bea + " " + fay + " 1",
	}) {
		t.Errorf("the journal of Bea turning it off: %v", got)
	}
	// Off already, nothing is journaled again.
	if switched, err := f.s.SetMailboxSync(ctx, "acc_t1", false, owner, "", nil); err != nil || switched.Changed ||
		len(switched.Told) != 0 {
		t.Errorf("turning it off again: %+v (%v)", switched, err)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`); n != 1 {
		t.Errorf("%d entries in the journal, want Bea's alone", n)
	}

	// An operator mailbox has no readers to tell, and is journaled by
	// nothing here.
	f.accepted("the operator's switched on", f.exec(`UPDATE accounts SET sync_enabled_at = 800, sync_enabled_by = 'cli'
		WHERE id = 'acc_0000000000000005'`))
	hold("acc_0000000000000005")
	if switched, err := f.s.SetMailboxSync(ctx, "acc_0000000000000005", false, "cli", "", nil); err != nil ||
		!switched.Changed || len(switched.Told) != 0 || held("acc_0000000000000005") {
		t.Errorf("the operator turning its mailbox off: %+v (%v)", switched, err)
	}

	// A removed mailbox is turned off, and never on.
	f.remove("acc_t1", owner, 1000)
	if _, err := f.s.SetMailboxSync(ctx, "acc_t1", true, owner, version, nil); !errors.Is(err, ErrNoMailboxConsent) {
		t.Errorf("turning a removed mailbox on: %v, want ErrNoMailboxConsent", err)
	}
	if _, err := f.s.SetMailboxSync(ctx, "acc_t1", false, owner, "", nil); err != nil {
		t.Errorf("turning a removed mailbox off: %v", err)
	}
}

func TestOnlyAPersonalMailboxsHoldIsLiftedAlone(t *testing.T) {
	// The seed at schema 20: acc_0000000000000003 is Bea's own, under her
	// consent.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	const own = "acc_0000000000000003"
	f.accepted("held", f.exec(`UPDATE accounts SET sync_held_at = 950 WHERE id = ?`, own))
	refused := errors.New("not theirs")
	if _, err := f.s.LiftSyncHold(ctx, own, func(*sql.Tx) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused lift: %v", err)
	}
	if lifted, err := f.s.LiftSyncHold(ctx, own, nil); err != nil || !lifted {
		t.Fatalf("lifting the hold: %v, %v", lifted, err)
	}
	if lifted, err := f.s.LiftSyncHold(ctx, own, nil); err != nil || lifted {
		t.Errorf("lifting it again: %v, %v, want nothing to lift", lifted, err)
	}
	ok, err := f.s.SyncEligible(ctx, own)
	if err != nil || !ok {
		t.Errorf("Bea's own mailbox once let go: eligible %v (%v)", ok, err)
	}
	f.remove(own, "usr_0000000000000002", 1000)
	if _, err := f.s.LiftSyncHold(ctx, own, nil); !errors.Is(err, ErrNoSuchMailbox) {
		t.Errorf("lifting the hold of a removed mailbox: %v, want ErrNoSuchMailbox", err)
	}
}

func TestAConsentStoppedByItsPersonTellsTheMailboxsOtherReaders(t *testing.T) {
	// The seed at schema 20: Bea (usr_2) and Fay (usr_6) read acc_t1, a
	// mailbox of the team wsp_t1; its consent is bound to Bea here, as
	// migration 0011 left it.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	const bea, fay = "usr_0000000000000002", "usr_0000000000000006"
	f.accepted("bound to Bea", f.exec(`UPDATE accounts SET sync_enabled_via = 'migration', sync_enabled_by = ?
		WHERE id = 'acc_t1'`, bea))
	stopped, err := f.s.WithdrawSyncConsent(ctx, bea)
	if err != nil || !slices.Contains(stopped, "acc_t1") {
		t.Fatalf("Bea withdrawing her consent stopped %v (%v)", stopped, err)
	}
	got, err := f.s.Notices(ctx, fay)
	if err != nil || len(got) != 1 || got[0].Event.Kind != MailboxSyncPaused || got[0].Event.Actor != bea ||
		got[0].Event.AccountID != "acc_t1" {
		t.Errorf("Fay's notices: %+v (%v)", got, err)
	}
	if got, err := f.s.Notices(ctx, bea); err != nil || len(got) != 0 {
		t.Errorf("Bea is told of her own withdrawal: %+v (%v)", got, err)
	}
	if seq, err := f.s.LatestNotice(ctx, fay); err != nil || len(got) == 0 || seq != got[0].Event.Seq {
		t.Errorf("Fay's newest notice is %d (%v)", seq, err)
	}
	if seq, err := f.s.LatestNotice(ctx, bea); err != nil || seq != 0 {
		t.Errorf("Bea's newest notice is %d (%v), want none", seq, err)
	}
}

func TestADeletedMailboxIsNamedByItsHintOnlyWhileANoticeNeedsIt(t *testing.T) {
	// The seed at schema 20: Bea (usr_2) and Fay (usr_6) read acc_t1
	// (support@example.org), whose display name is "Name of acc_t1"; Owner
	// (usr_1) removes it, recovers it, removes it again and deletes it for
	// good.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	const owner, bea, fay = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006"
	journal := func(kind MailboxEventKind, notify bool) string {
		t.Helper()
		var id string
		err := f.s.Write(ctx, func(tx *sql.Tx) error {
			readers, err := MailboxReadersTx(ctx, tx, "acc_t1")
			if err != nil {
				return err
			}
			_, id, err = JournalMailboxTx(ctx, tx, MailboxEvent{Kind: kind, AccountID: "acc_t1", Actor: owner, At: 900},
				readers, notify)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	removed := journal(MailboxRemoved, true)
	journal(MailboxRecovered, false)
	err := f.s.Write(ctx, func(tx *sql.Tx) error {
		if err := ForgetMailboxNamesTx(ctx, tx, []string{"acc_t1"}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = 'acc_t1'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT kind || '|' || label || '|' || hint FROM mailbox_events ORDER BY seq`)
	}
	// The removal Bea and Fay are still to read keeps the hint alone; the
	// recovery, which told nobody, keeps nothing.
	if got := names(); !slices.Equal(got, []string{"removed||s…@example.org", "recovered||"}) {
		t.Fatalf("deleted for good, the journal names it %v", got)
	}
	if err := f.s.DismissNotice(ctx, bea, removed); err != nil {
		t.Fatal(err)
	}
	if got := names(); got[0] != "removed||s…@example.org" {
		t.Errorf("with Fay's notice still to read, the removal is named %q", got[0])
	}
	if err := f.s.DismissNotice(ctx, fay, removed); err != nil {
		t.Fatal(err)
	}
	if got := names(); got[0] != "removed||" {
		t.Errorf("with every notice read, the removal is named %q", got[0])
	}

	// A notice that goes with its person is followed by the next sweep.
	f.accepted("a mailbox", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, hint, actor, created_at)
		VALUES ('mbe_00000000000000f1', 'removed', 'wsp_t1', 'acc_gone', 'g…@example.org', ?, 950)`, owner))
	seq := queryInt(t, f.s, `SELECT seq FROM mailbox_events WHERE id = 'mbe_00000000000000f1'`)
	f.accepted("its reader", f.exec(`INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES (?, ?)`, seq, fay))
	f.accepted("told", f.exec(`INSERT INTO notices(user_id, event_seq, created_at) VALUES (?, ?, 950)`, fay, seq))
	if _, err := f.s.SweepMailboxEvents(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE hint <> ''`); n != 1 {
		t.Fatalf("%d hints after a sweep with Fay's notice still to read, want hers", n)
	}
	f.accepted("Fay's notice gone with her", f.exec(`DELETE FROM notices WHERE user_id = ?`, fay))
	if _, err := f.s.SweepMailboxEvents(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE hint <> ''`); n != 0 {
		t.Errorf("%d hints no notice needs outlive the sweep", n)
	}
}
