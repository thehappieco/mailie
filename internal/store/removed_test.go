package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A removed mailbox, and one whose sync a recovery holds, as the rules this
// package decides see them (migration 0020).

func TestARemovedOrHeldMailboxIsNeverEligibleToSync(t *testing.T) {
	// The seed at schema 20: acc_0000000000000003 is Bea's own, under her
	// consent; acc_t1 a mailbox of the team wsp_t1, under its consent, which
	// Bea and Fay read; acc_0000000000000005 an operator mailbox, switched
	// on here.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	f.accepted("the operator's switched on", f.exec(`UPDATE accounts SET sync_enabled_at = 800, sync_enabled_by = 'cli'
		WHERE id = 'acc_0000000000000005'`))
	eligible := func(id string) bool {
		t.Helper()
		ok, err := f.s.SyncEligible(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		all, err := f.s.SyncEligibleAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		permitted, err := f.s.SyncPermitted(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		inTx := f.s.Write(ctx, func(tx *sql.Tx) error { return f.s.RequireSyncEligibleTx(ctx, tx, id) })
		if slices.Contains(all, id) != ok || permitted[id] != ok || (inTx == nil) != ok {
			t.Fatalf("%s: eligible %t, listed %t, permitted %t, in a transaction %v: the rules disagree",
				id, ok, slices.Contains(all, id), permitted[id], inTx)
		}
		if inTx != nil && !errors.Is(inTx, ErrNotEligible) {
			t.Fatalf("%s: %v, want ErrNotEligible", id, inTx)
		}
		return ok
	}
	for _, id := range []string{"acc_0000000000000003", "acc_t1", "acc_0000000000000005"} {
		if !eligible(id) {
			t.Fatalf("%s does not sync before anything happened to it", id)
		}
		f.remove(id, "usr_0000000000000001", 900)
		if eligible(id) {
			t.Errorf("%s syncs while it is removed", id)
		}
		// Recovered, it is held until someone turns it on, which keeps
		// its consent as it was.
		f.accepted("recovered", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
			sync_held_at = 950 WHERE id = ?`, id))
		if eligible(id) {
			t.Errorf("%s syncs while a recovery holds it", id)
		}
		// What the engine writes of a sync it is not allowed is dropped.
		if err := f.s.RecordSyncOK(ctx, id, time.Unix(5000, 0)); err != nil {
			t.Fatal(err)
		}
		if n := queryInt(t, f.s, `SELECT last_ok_at FROM accounts WHERE id = ?`, id); n == 5000 {
			t.Errorf("%s recorded a sync pass while held", id)
		}
		f.accepted("turned on", f.exec(`UPDATE accounts SET sync_held_at = 0 WHERE id = ?`, id))
		if !eligible(id) {
			t.Errorf("%s does not sync once turned on again", id)
		}
	}
}

func TestARemovedMailboxHasNoReader(t *testing.T) {
	// The seed at schema 20: Bea (usr_2) and Fay (usr_6) read acc_t1, with
	// no key; Fay is suspended here, so she would read it once resumed.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	f.accepted("Fay suspended", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 800
		WHERE id = 'usr_0000000000000006'`))
	count := func(rule string) int64 {
		t.Helper()
		return queryInt(t, f.s, `SELECT count(*) FROM mailbox_access g WHERE g.account_id = 'acc_t1' AND `+rule)
	}
	readers := func() []string {
		t.Helper()
		var out []string
		err := f.s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = MailboxReadersTx(ctx, tx, "acc_t1")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if count(ReaderSQL("g")) != 1 || count(FlagHolderSQL("g")) != 1 || count(SuspendedReaderSQL("g")) != 1 ||
		!slices.Equal(readers(), []string{"usr_0000000000000002"}) {
		t.Fatalf("before the removal: readers %v, %d holding the flag, %d suspended", readers(),
			count(FlagHolderSQL("g")), count(SuspendedReaderSQL("g")))
	}
	f.remove("acc_t1", "usr_0000000000000001", 900)
	for name, rule := range map[string]string{
		"reads": ReaderSQL("g"), "holds the flag": FlagHolderSQL("g"),
		"would read once resumed": SuspendedReaderSQL("g"), "waits for the key": WaitingSQL("g"),
	} {
		if n := count(rule); n != 0 {
			t.Errorf("%d people %s on the removed mailbox", n, name)
		}
	}
	if got := readers(); len(got) != 0 {
		t.Errorf("the removed mailbox's readers are %v", got)
	}
	// Who would read it once recovered is still known: Bea, and not Fay,
	// who is suspended.
	if n := count(KeptReaderSQL("g")); n != 1 {
		t.Errorf("%d people would read the removed mailbox once recovered, want Bea", n)
	}
	// Its grants are kept for a recovery, which gives them back.
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_access WHERE account_id = 'acc_t1' AND read = 1`); n != 2 {
		t.Errorf("the removed mailbox keeps %d read flags, want the 2 it had", n)
	}
	f.accepted("recovered", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
		sync_held_at = 950 WHERE id = 'acc_t1'`))
	if got := readers(); !slices.Equal(got, []string{"usr_0000000000000002"}) {
		t.Errorf("recovered, its readers are %v", got)
	}
}

func TestWithdrawingConsentDeletesTheIndexOfRemovedMailboxesToo(t *testing.T) {
	// The seed at schema 20: acc_0000000000000001 is Owner's (usr_1) own,
	// with two messages; acc_t2's consent is still the one migration 0011
	// copied from Eve's (usr_5), and it holds one message.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	f.remove("acc_0000000000000001", "usr_0000000000000001", 900)
	f.remove("acc_t2", "usr_0000000000000001", 900)
	index := func(id string) int64 {
		t.Helper()
		return queryInt(t, f.s, `SELECT (SELECT count(*) FROM messages WHERE account_id = ?1)
			+ (SELECT count(*) FROM folders WHERE account_id = ?1)`, id)
	}
	if index("acc_0000000000000001") == 0 || index("acc_t2") == 0 {
		t.Fatal("the seed's removed mailboxes hold no index")
	}

	stopped, err := f.s.WithdrawSyncConsent(ctx, "usr_0000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stopped, "acc_0000000000000001") {
		t.Errorf("withdrawing stopped %v, not the removed mailbox of her own", stopped)
	}
	if n := index("acc_0000000000000001"); n != 0 {
		t.Errorf("her removed mailbox keeps %d rows of its index after she withdrew", n)
	}
	// The consent bound to Eve stops with her own withdrawal, removed or not.
	stopped, err = f.s.WithdrawSyncConsent(ctx, "usr_0000000000000005")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stopped, "acc_t2") {
		t.Errorf("Eve's withdrawal stopped %v, not the removed team mailbox bound to her", stopped)
	}
	if n := index("acc_t2"); n != 0 {
		t.Errorf("the removed team mailbox bound to her keeps %d rows of its index", n)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM accounts WHERE id IN ('acc_0000000000000001', 'acc_t2')
		AND removed_at = 900`); n != 2 {
		t.Errorf("%d of the mailboxes are still removed, want both: a withdrawal deletes the index, not the removal", n)
	}
}

func TestTheMailboxJournalNeverHoldsAnAddress(t *testing.T) {
	for email, want := range map[string]string{
		"support@example.org": "s…@example.org", "Élodie@exemple.fr": "É…@exemple.fr", "a@b": "a…@b",
		"odd@local@example.org": "o…@example.org", "": "", "nobody": "", "@example.org": "", "ana@": "",
	} {
		if got := MailboxHint(email); got != want {
			t.Errorf("MailboxHint(%q) = %q, want %q", email, got, want)
		}
	}

	// The seed at schema 20: acc_t1 (support@example.org) and acc_t3
	// (ops@team.example.org), whose display names are "Name of …"; Bea and
	// Fay read acc_t1, Dan and Bea acc_t3.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	f.accepted("a display name that is the address", f.exec(`UPDATE accounts SET display_name = 'Support <support@example.org>'
		WHERE id = 'acc_t1'`))
	journal := func(kind MailboxEventKind, account, actor string, notify bool) string {
		t.Helper()
		var id string
		err := f.s.Write(ctx, func(tx *sql.Tx) error {
			readers, err := MailboxReadersTx(ctx, tx, account)
			if err != nil {
				return err
			}
			_, id, err = JournalMailboxTx(ctx, tx, MailboxEvent{Kind: kind, AccountID: account, Actor: actor, At: 900},
				readers, notify)
			return err
		})
		if err != nil {
			t.Fatalf("journal %s of %s: %v", kind, account, err)
		}
		return id
	}
	journal(MailboxRemoved, "acc_t1", "usr_0000000000000001", true)
	journal(MailboxSyncPaused, "acc_t3", "usr_0000000000000004", true)
	rows := func() []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT kind || '|' || workspace_id || '|' || account_id || '|' || label || '|' || hint
			|| '|' || actor FROM mailbox_events ORDER BY seq`)
	}
	if got, want := rows(), []string{
		"removed|wsp_t1|acc_t1||s…@example.org|usr_0000000000000001",
		"sync_paused|wsp_t2|acc_t3|Name of acc_t3|o…@team.example.org|usr_0000000000000004",
	}; !slices.Equal(got, want) {
		t.Errorf("the journal holds\n%v\nwant\n%v", got, want)
	}
	// Who read them, and who was told: everyone but whoever acted.
	if got := queryStrings(t, f.s, `SELECT e.account_id || ' ' || r.user_id || ' ' || (n.user_id IS NOT NULL)
		FROM mailbox_event_readers r JOIN mailbox_events e ON e.seq = r.event_seq
		LEFT JOIN notices n ON n.event_seq = r.event_seq AND n.user_id = r.user_id ORDER BY e.seq, r.user_id`); !slices.Equal(got, []string{
		"acc_t1 usr_0000000000000002 1", "acc_t1 usr_0000000000000006 1",
		"acc_t3 usr_0000000000000002 1", "acc_t3 usr_0000000000000004 0",
	}) {
		t.Errorf("readers and notices: %v", got)
	}

	// Deleted for good: its earlier entries forget its names, but for the
	// hint a notice still needs, and the entry that says so holds none.
	err := f.s.Write(ctx, func(tx *sql.Tx) error { return ForgetMailboxNamesTx(ctx, tx, []string{"acc_t3"}) })
	if err != nil {
		t.Fatal(err)
	}
	journal(MailboxPurged, "acc_t3", ActorSweep, false)
	// The entry Bea is still to read keeps the hint alone, to name it in her
	// notice.
	if got := rows(); !slices.Equal(got[1:], []string{
		"sync_paused|wsp_t2|acc_t3||o…@team.example.org|usr_0000000000000004", "purged|wsp_t2|acc_t3|||sweep",
	}) {
		t.Errorf("after the deletion for good the journal holds %v", got)
	}
	// Nothing in the journal is an address, nor holds the local part of
	// one.
	for _, table := range []string{"mailbox_events", "mailbox_event_readers", "notices"} {
		for _, row := range dumpTable(t, f.s, table, "rowid") {
			for col, value := range row {
				for _, local := range []string{"support@", "ops@"} {
					if strings.Contains(value, local) {
						t.Errorf("%s.%s holds %s", table, col, value)
					}
				}
			}
		}
	}
	// A recovery and a deletion for good tell nobody.
	err = f.s.Write(ctx, func(tx *sql.Tx) error {
		_, _, err := JournalMailboxTx(ctx, tx, MailboxEvent{Kind: MailboxRecovered, AccountID: "acc_t1", At: 950},
			[]string{"usr_0000000000000002"}, true)
		return err
	})
	if !errors.Is(err, ErrNotNotifying) {
		t.Errorf("a recovery told its readers: %v", err)
	}
}

func TestTeamSyncNoticesLeaveRemovedMailboxesOut(t *testing.T) {
	// The seed at schema 20: acc_t2 is kept stopped, bound to Eve, who is
	// disabled, and read by Owner; acc_t9, linked here, nobody reads.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	f.accepted("a team mailbox nobody reads", f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind,
		imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
		created_at, updated_at, linked_by) VALUES ('acc_t9', 'wsp_t2', 'nobody@example.org', 'imap', 'password',
		'imap.example.org', 993, 'smtp.example.org', 587, 'starttls', 'nobody@example.org', 1, 'active', 800, 800, 800,
		'usr_0000000000000004')`))
	kept, unread, err := f.s.TeamSyncNotices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"acc_t2"}) || !slices.Equal(unread, []string{"acc_t9"}) {
		t.Fatalf("before any removal: kept stopped %v, read by nobody %v", kept, unread)
	}
	f.remove("acc_t2", "usr_0000000000000001", 900)
	f.remove("acc_t9", "usr_0000000000000004", 900)
	if kept, unread, err := f.s.TeamSyncNotices(ctx); err != nil || len(kept) != 0 || len(unread) != 0 {
		t.Errorf("with both removed: kept stopped %v, read by nobody %v (%v)", kept, unread, err)
	}
}

func TestANoticeShowsTheMailboxAsItStandsUntilItsReaderDismissesIt(t *testing.T) {
	// The seed at schema 20: Bea (usr_2) and Fay (usr_6) read acc_t1
	// (support@example.org), of the team Support; Owner (usr_1) removes it.
	f := newSchemaTwenty(t)
	ctx := context.Background()
	const owner, bea, fay = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006"
	var eventID string
	err := f.s.Write(ctx, func(tx *sql.Tx) error {
		readers, err := MailboxReadersTx(ctx, tx, "acc_t1")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = 900, removed_by = ?, purge_after = ?
			WHERE id = 'acc_t1'`, owner, 900+thirtyDays); err != nil {
			return err
		}
		_, eventID, err = JournalMailboxTx(ctx, tx, MailboxEvent{Kind: MailboxRemoved, AccountID: "acc_t1", Actor: owner,
			At: 900}, readers, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	notices := func(user string) []Notice {
		t.Helper()
		got, err := f.s.Notices(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := notices(bea)
	if len(got) != 1 || got[0].Event.ID != eventID || got[0].Event.Kind != MailboxRemoved || got[0].Event.Actor != owner ||
		got[0].WorkspaceName != "Support" || !got[0].Kept || got[0].Email != "support@example.org" ||
		got[0].RemovedAt != 900 || got[0].PurgeAfter != 900+thirtyDays || got[0].CreatedAt != 900 {
		t.Fatalf("Bea's notices while the mailbox is kept: %+v", got)
	}
	if got := notices(owner); len(got) != 0 {
		t.Errorf("Owner, who removed it, has the notices %+v", got)
	}

	// Deleted for good, it is named by what the journal kept of it.
	if err := f.exec(`DELETE FROM accounts WHERE id = 'acc_t1'`); err != nil {
		t.Fatal(err)
	}
	if got := notices(fay); len(got) != 1 || got[0].Kept || got[0].Email != "" || got[0].RemovedAt != 0 ||
		got[0].Event.Hint != "s…@example.org" {
		t.Errorf("Fay's notice once the mailbox is gone: %+v", got)
	}

	// Each reader dismisses their own, once.
	if err := f.s.DismissNotice(ctx, owner, eventID); !errors.Is(err, ErrNoNotice) {
		t.Errorf("Owner dismissed a notice she never had: %v", err)
	}
	if err := f.s.DismissNotice(ctx, bea, eventID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DismissNotice(ctx, bea, eventID); !errors.Is(err, ErrNoNotice) {
		t.Errorf("Bea dismissed hers twice: %v", err)
	}
	if len(notices(bea)) != 0 || len(notices(fay)) != 1 {
		t.Errorf("after Bea dismissed hers: Bea %+v, Fay %+v", notices(bea), notices(fay))
	}

	// The journal goes after its retention, with what it gave.
	if n, err := f.s.SweepMailboxEvents(ctx, 900); err != nil || n != 0 {
		t.Errorf("a sweep of what came before it deleted %d (%v)", n, err)
	}
	if n, err := f.s.SweepMailboxEvents(ctx, 901); err != nil || n != 1 {
		t.Errorf("a sweep past it deleted %d (%v), want its 1 entry", n, err)
	}
	if len(notices(fay)) != 0 || queryInt(t, f.s, `SELECT count(*) FROM mailbox_event_readers`) != 0 {
		t.Error("the swept entry left its notices or its readers")
	}
}
