package sync_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

// live starts a harness and waits for its initial sync and its idle
// connection.
func (h *harness) live() *events.Subscription {
	h.t.Helper()
	h.start()
	h.waitLive()
	h.waitFor("the idle connection", 5*time.Second, func() bool { return h.box.CallCount(providertest.MethodIdle) > 0 })
	return h.subscribe()
}

// settled waits for the inbox pass that every (re)connection of the idle
// connection asks for — it reads every flag window and diffs — so a test's
// next change is not raced by it. The diff stamps last_full_uid_scan_at.
func (h *harness) settled() {
	h.t.Helper()
	h.waitFor("the pass after the idle connection came up", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM folders WHERE name = 'INBOX' AND last_full_uid_scan_at <> 0`) == 1
	})
}

func (h *harness) rowID(folder string, uid imap.UID) int64 {
	h.t.Helper()
	var id int64
	err := h.db.Reader().QueryRowContext(context.Background(),
		`SELECT id FROM messages WHERE folder_id = ? AND uid = ?`, h.folderID(folder), uint32(uid)).Scan(&id)
	if err != nil {
		h.t.Fatalf("row %s/%d: %v", folder, uid, err)
	}
	return id
}

func TestKillingTheIdleConnectionLosesNoMessages(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.Deliver("INBOX", mail("first", time.Now().Add(-time.Hour)))
	sub := h.live()

	const n = 12
	for i := range n {
		if i%3 == 0 {
			h.box.KillSessions(provider.RoleIdle)
		}
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%02d", i), time.Now()))
		time.Sleep(10 * time.Millisecond)
	}
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < n {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.TypeMessageNew {
				subject := payload[store.MessageNew](t, ev).Subject
				if seen[subject] {
					t.Errorf("%q announced twice", subject)
				}
				seen[subject] = true
			}
		case <-deadline:
			t.Fatalf("%d of %d messages announced after the idle connection was killed", len(seen), n)
		}
	}
	if got := h.messages(); got != n+1 {
		t.Errorf("%d messages indexed, want %d", got, n+1)
	}
}

func TestAnExpungedUIDIsDeletedAfterTwoConsecutiveScans(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(),
		opts: syncengine.Options{ConfirmDiffDelay: 150 * time.Millisecond}})
	var uids []imap.UID
	for i := range 3 {
		uids = append(uids, h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour))))
	}
	sub := h.live()
	gone := h.rowID("INBOX", uids[1])

	h.box.Expunge("INBOX", uids[1])
	h.waitFor("the tombstone", 2*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM messages WHERE id = ? AND vanished_at <> 0`, gone) == 1
	})
	tombstoned := time.Now()
	ev := next(t, sub, events.TypeMessageDeleted, 3*time.Second)
	if took := time.Since(tombstoned); took < 100*time.Millisecond {
		t.Errorf("the row was deleted %s after its first absence", took)
	}
	if got := payload[store.MessageDeleted](t, ev); got.MessageID != gone {
		t.Errorf("message.deleted = %s, want row %d", describe(got), gone)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE id = ?`, gone); n != 0 {
		t.Error("the expunged row is still indexed")
	}
	if got := h.messages(); got != 2 {
		t.Errorf("%d messages left, want 2", got)
	}
}

func TestDeletingTheNewestMessageDoesNotReplayNewMail(t *testing.T) {
	// max_seen_uid is the highest UID ever seen, not the highest present:
	// otherwise the next pass would fetch what is below it again as new.
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft,
		opts: syncengine.Options{ConfirmDiffDelay: 50 * time.Millisecond}})
	var uids []imap.UID
	for i := range 4 {
		uids = append(uids, h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour))))
	}
	sub := h.live()
	h.box.Expunge("INBOX", uids[3])
	next(t, sub, events.TypeMessageDeleted, 3*time.Second)
	fresh := h.box.Deliver("INBOX", mail("fresh", time.Now()))
	ev := next(t, sub, events.TypeMessageNew, 2*time.Second)
	if got := payload[store.MessageNew](t, ev); got.Subject != "Subject fresh" {
		t.Errorf("message.new for %q after deleting the newest", got.Subject)
	}
	if evs := h.journaled(events.TypeMessageNew); len(evs) != 1 {
		t.Errorf("%d message.new journaled, want only UID %d's", len(evs), fresh)
	}
}

func TestAUIDValidityChangeKeepsIdsAndEmitsNothingForKnownMessages(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft})
	var keys []string
	for i := range 5 {
		key := fmt.Sprintf("m%d", i)
		keys = append(keys, key)
		h.box.Deliver("INBOX", mail(key, time.Now().Add(-time.Duration(5-i)*time.Hour)))
	}
	sub := h.live()
	before := map[string]int64{}
	for _, key := range keys {
		before[key] = int64(h.count(`SELECT id FROM messages WHERE message_id = ?`, key+"@mail.example"))
	}
	journaledBefore := h.count(`SELECT count(*) FROM events WHERE type IN ('message.new', 'message.deleted', 'message.moved')`)

	uidvalidity := h.box.ChangeUIDValidity("INBOX")
	// Nothing notifies a UIDVALIDITY change; the next pass finds it.
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	ev := next(t, sub, events.TypeFolderChanged, 5*time.Second)
	for payload[store.FolderChanged](t, ev).Change != store.FolderResyncDone {
		ev = next(t, sub, events.TypeFolderChanged, 5*time.Second)
	}
	for _, key := range keys {
		var (
			id int64
			uv int
		)
		err := h.db.Reader().QueryRowContext(context.Background(),
			`SELECT id, uidvalidity FROM messages WHERE message_id = ? AND stale = 0`, key+"@mail.example").Scan(&id, &uv)
		if err != nil {
			t.Fatalf("%s after the resync: %v", key, err)
		}
		if id != before[key] || uint32(uv) != uidvalidity {
			t.Errorf("%s: id %d → %d, uidvalidity %d, want the same id under %d", key, before[key], id, uv, uidvalidity)
		}
	}
	if n := h.count(`SELECT count(*) FROM events WHERE type IN ('message.new', 'message.deleted', 'message.moved')`); n != journaledBefore {
		t.Errorf("the resync announced %d message events for messages the index already knew", n-journaledBefore)
	}
}

func TestMicrosoftReassignedUIDIsRewrittenInPlace(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft})
	uid := h.box.Deliver("INBOX", mail("moved-db", time.Now().Add(-time.Hour)))
	sub := h.live()
	id := h.rowID("INBOX", uid)

	fresh := h.box.ReassignUID("INBOX", uid)
	h.waitFor("the row to follow its new UID", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM messages WHERE id = ? AND uid = ?`, id, uint32(fresh)) == 1
	})
	if got := h.messages(); got != 1 {
		t.Errorf("%d messages after the reassignment, want the one", got)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type == events.TypeMessageNew || ev.Type == events.TypeMessageDeleted {
			t.Errorf("a reassigned UID announced %s", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestGmailLabelCopiesDoNotEmitNewMail(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(), sharedFlags: true})
	h.box.CreateFolder("Receipts")
	sub := h.live()

	uid := h.box.Deliver("INBOX", mail("invoice", time.Now()))
	first := next(t, sub, events.TypeMessageNew, 2*time.Second)
	h.box.CopyTo("INBOX", uid, "Receipts") // the label is applied
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	moved := next(t, sub, events.TypeMessageMoved, 2*time.Second)
	if got := payload[store.MessageMoved](t, moved); !got.NewCopy || got.FolderID != h.folderID("Receipts") ||
		got.PrimaryID != payload[store.MessageNew](t, first).MessageID {
		t.Errorf("message.moved for the label = %s", describe(got))
	}
	if evs := h.journaled(events.TypeMessageNew); len(evs) != 1 {
		t.Errorf("%d message.new for one message and its label", len(evs))
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE dup_of IS NULL AND vanished_at = 0`); n != 1 {
		t.Errorf("%d primary rows for one message", n)
	}
}

func TestAGmailLabelSyncedBeforeInboxStillReportsNewInboxMail(t *testing.T) {
	// A filter labels mail as it arrives, and the label's folder can be
	// synced before the inbox: the arrival in the inbox is still new mail.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), sharedFlags: true})
	h.box.CreateFolder("Receipts")
	sub := h.live()

	uid := h.box.Deliver("Receipts", mail("filtered", time.Now()))
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	labelled := payload[store.MessageNew](t, next(t, sub, events.TypeMessageNew, 2*time.Second))
	if labelled.FolderRole != "" || labelled.FirstInboxCopy {
		t.Errorf("the label copy = %s", describe(labelled))
	}
	h.box.CopyTo("Receipts", uid, "INBOX")
	inbox := payload[store.MessageNew](t, next(t, sub, events.TypeMessageNew, 2*time.Second))
	if inbox.FolderRole != "inbox" || !inbox.FirstInboxCopy || inbox.FirstCopy {
		t.Errorf("the inbox arrival = %s", describe(inbox))
	}
}

func TestAPartiallyBackfilledFolderDoesNotFullScanEveryPass(t *testing.T) {
	// The window leaves old mail below the floor. Comparing the server's
	// message count with the index's would say "something vanished" on every
	// pass and run the full UID diff each time.
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft, initialDays: 30,
		opts: syncengine.Options{InboxPollInterval: 20 * time.Millisecond}})
	for i := range 10 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("old%d", i), time.Now().AddDate(0, 0, -200)))
	}
	for i := range 5 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("new%d", i), time.Now().Add(-time.Hour)))
	}
	h.live()
	floor := h.count(`SELECT backfill_floor FROM folders WHERE name = 'INBOX'`)
	if floor <= 1 {
		t.Fatalf("floor %d: the window should have left the old mail out", floor)
	}
	h.box.ResetCalls()
	time.Sleep(300 * time.Millisecond) // a dozen poll passes
	passes := h.box.CallCount(providertest.MethodSelect)
	if passes < 5 {
		t.Fatalf("only %d passes ran", passes)
	}
	diffs := 0
	for _, c := range h.box.Calls() {
		if c.Method == providertest.MethodUIDs && strings.HasSuffix(c.Set, ":*") && c.Since.IsZero() &&
			c.Set == fmt.Sprintf("%d:*", floor) {
			diffs++
		}
	}
	if diffs > 1 {
		t.Errorf("%d full UID diffs in %d quiet passes", diffs, passes)
	}
}

func TestRemovingAGmailLabelIsACopyRemovedNotADeletion(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(), sharedFlags: true,
		opts: syncengine.Options{ConfirmDiffDelay: 50 * time.Millisecond}})
	h.box.CreateFolder("Receipts")
	uid := h.box.Deliver("INBOX", mail("invoice", time.Now().Add(-time.Hour)))
	copied := h.box.CopyTo("INBOX", uid, "Receipts")
	sub := h.live()
	primary := h.rowID("INBOX", uid)
	label := h.rowID("Receipts", copied)

	h.box.Expunge("Receipts", copied) // the label is removed
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	ev := next(t, sub, events.TypeMessageMoved, 3*time.Second)
	got := payload[store.MessageMoved](t, ev)
	if got.NewCopy || got.To != nil || got.MessageID != label || got.PrimaryID != primary {
		t.Errorf("message.moved = %s, want the label's row %d gone with %d still standing", describe(got), label, primary)
	}
	if evs := h.journaled(events.TypeMessageDeleted); len(evs) != 0 {
		t.Errorf("removing a label announced %d deletions", len(evs))
	}
	if n := h.messages(); n != 1 {
		t.Errorf("%d rows left, want the inbox copy", n)
	}
}

func TestMailOlderThanTheWindowAmongItsUIDsIsNeverIndexed(t *testing.T) {
	// A person consented to the last 90 days. Old mail can sit among the
	// window's UIDs — imported newest first, labelled or moved into the
	// folder before sync began — and every diff and flag fetch sees it as a
	// UID the index lacks. It stays out; mail put back in the folder after
	// sync started is a different matter, and is indexed whatever its date.
	for _, tc := range []struct {
		tier string
		caps provider.Caps
		kind provider.Kind
	}{
		{syncengine.TierCondStore, providertest.GmailCaps(), provider.KindGmail},
		{syncengine.TierUIDPoll, providertest.ExchangeCaps(), provider.KindMicrosoft},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			short := 100 * time.Millisecond
			h := newHarness(t, setup{caps: tc.caps, kind: tc.kind, opts: syncengine.Options{
				InboxInterval: short, InboxPollInterval: short, OtherInterval: short,
				InboxDiffInterval: short, OtherDiffInterval: short, InboxFlagSweep: short, OtherFlagSweep: short,
			}})
			h.box.CreateFolder("Projects")
			now := time.Now()
			for _, folder := range []string{"INBOX", "Projects"} {
				h.box.Deliver(folder, mail(folder+"-recent", now.AddDate(0, 0, -10)))
				for i := range 4 {
					h.box.Deliver(folder, mail(fmt.Sprintf("%s-old%d", folder, i), now.AddDate(-3, 0, -i)))
				}
				h.box.Deliver(folder, mail(folder+"-today", now.Add(-time.Hour)))
			}
			h.start()
			h.waitLive()
			scans := func() int {
				return h.count(`SELECT count(*) FROM folders WHERE account_id = ? AND last_full_uid_scan_at <> 0
					AND name IN ('INBOX', 'Projects')`, h.account)
			}
			h.waitFor("a diff of both folders", 5*time.Second, func() bool { return scans() == 2 })
			time.Sleep(8 * short) // several more diffs, flag passes and sweeps

			cutoff := now.AddDate(0, 0, -91).Unix()
			if n := h.count(`SELECT count(*) FROM messages WHERE internal_date < ?`, cutoff); n != 0 {
				t.Errorf("%d messages from before the 90-day window were indexed", n)
			}
			if n := h.messages(); n != 4 {
				t.Errorf("%d messages indexed, want the 4 inside the window", n)
			}

			h.box.Deliver("INBOX", mail("restored", now.AddDate(-2, 0, 0)))
			h.waitFor("the restored message", 5*time.Second, func() bool { return h.messages() == 5 })
		})
	}
}

func TestAPersonsMailboxGetsNinetyDaysWhateverItsRowSays(t *testing.T) {
	// The column is the operator's, for mailboxes nobody owns. A person's
	// mailbox is synced back 90 days, the window the privacy policy
	// promises, even if the row asks for everything or for ten years.
	for _, days := range []int{-1, 3650} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			h := newHarness(t, setup{caps: providertest.GmailCaps(), initialDays: days})
			now := time.Now()
			h.box.Deliver("INBOX", mail("old", now.AddDate(-1, 0, 0)))
			h.box.Deliver("INBOX", mail("recent", now.AddDate(0, 0, -30)))
			h.start()
			h.waitLive()
			if n := h.messages(); n != 1 {
				t.Errorf("%d messages indexed with initial_days %d, want only the one from the last 90 days", n, days)
			}
			since := h.count(`SELECT initial_since FROM folders WHERE name = 'INBOX'`)
			if want := now.AddDate(0, 0, -90).Unix(); since < int(want)-60 || since > int(want)+60 {
				t.Errorf("the inbox's window starts at %d, want 90 days ago (%d)", since, want)
			}
		})
	}
}

func TestAnOperatorMailboxKeepsTheWindowItsRowSays(t *testing.T) {
	// A mailbox nobody owns, synced because the operator switched it on,
	// may be synced whole.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), initialDays: -1, unowned: true})
	now := time.Now()
	h.box.Deliver("INBOX", mail("old", now.AddDate(-1, 0, 0)))
	h.box.Deliver("INBOX", mail("recent", now.AddDate(0, 0, -30)))
	h.start()
	h.waitLive()
	if n := h.messages(); n != 2 {
		t.Errorf("%d messages indexed for an operator's mailbox with initial_days -1, want both", n)
	}
}
