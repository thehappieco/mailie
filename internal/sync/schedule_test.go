package sync_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestIdleIsRenewedWithDoneAndNoopOnTheSameConnection(t *testing.T) {
	// Exchange delivers EXISTS only after DONE, and a socket that died
	// quietly is found by the NOOP's timeout: IDLE is ended and restarted on
	// a schedule, on the connection it already has.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{IdleRenew: 30 * time.Millisecond}})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	sub := h.live()
	time.Sleep(250 * time.Millisecond)

	idles, noops := 0, 0
	for _, c := range h.box.Calls() {
		if c.Role != provider.RoleIdle {
			continue
		}
		switch c.Method {
		case providertest.MethodIdle:
			idles++
		case providertest.MethodNoop:
			noops++
		}
	}
	if idles < 3 || noops < 2 {
		t.Errorf("%d IDLEs and %d NOOPs on the idle connection in 250 ms with a 30 ms renewal", idles, noops)
	}
	if n := h.box.Opens(provider.RoleIdle); n != 1 {
		t.Errorf("%d idle connections opened: a renewal is not a reconnection", n)
	}
	if n := h.box.ClosedIdling(); n != 0 {
		t.Errorf("%d connections closed mid-IDLE", n)
	}
	h.box.Deliver("INBOX", mail("fresh", time.Now()))
	next(t, sub, events.TypeMessageNew, 2*time.Second)
}

func TestAnOverflowedIdleBufferMakesTheNextPassReadEveryFlagWindow(t *testing.T) {
	// On the uidpoll tier a pass reads the newest flag window only; older
	// windows wait for the sweep. The notifications are what make that
	// safe, so when some were lost the next pass reads everything.
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft,
		opts: syncengine.Options{FlagWindow: 5}})
	var uids []imap.UID
	for i := range 12 {
		uids = append(uids, h.box.Deliver("INBOX", mail(fmt.Sprintf("m%02d", i), time.Now().Add(-time.Hour))))
	}
	h.live()
	h.settled()
	seen := func(uid imap.UID) bool {
		return h.count(`SELECT seen FROM messages WHERE folder_id = ? AND uid = ?`, h.folderID("INBOX"), uint32(uid)) == 1
	}

	// Heard: a pass runs, and reads the newest window only.
	h.box.ResetCalls()
	h.box.SetFlags("INBOX", uids[1], imap.FlagSeen)
	h.waitFor("the signalled pass", 2*time.Second, func() bool { return h.box.CallCount(providertest.MethodFetchFlags) > 0 })
	time.Sleep(100 * time.Millisecond)
	for _, c := range h.box.Calls() {
		if c.Method == providertest.MethodFetchFlags && uidsOf(t, c.Set)[0] < uids[7] {
			t.Errorf("FETCH FLAGS %s on a signalled pass; the newest window starts at %d", c.Set, uids[7])
		}
	}
	if seen(uids[1]) {
		t.Fatal("an old window was read without a sweep; the control is void")
	}

	// Lost: the next notification carries the overflow, and the pass after
	// it reads every window.
	h.box.Unheard(func() { h.box.SetFlags("INBOX", uids[0], imap.FlagSeen) })
	h.box.SetFlags("INBOX", uids[11], imap.FlagSeen)
	h.waitFor("every window to be read", 3*time.Second, func() bool { return seen(uids[0]) && seen(uids[1]) })
}

func TestUIDPollReadsOnlyTheNewestFlagWindowUntilTheSweep(t *testing.T) {
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft,
		opts: syncengine.Options{Now: now, FlagWindow: 5, OtherInterval: 20 * time.Millisecond}})
	h.box.CreateFolder("Receipts")
	var uids []imap.UID
	for i := range 12 {
		uids = append(uids, h.box.Deliver("Receipts", mail(fmt.Sprintf("r%02d", i), time.Now().Add(-time.Hour))))
	}
	h.live()
	oldest := func() int {
		return h.count(`SELECT seen FROM messages WHERE folder_id = ? AND uid = ?`, h.folderID("Receipts"), uint32(uids[0]))
	}

	h.box.SetFlags("Receipts", uids[0], imap.FlagSeen)
	h.box.ResetCalls()
	time.Sleep(250 * time.Millisecond)
	passes, reads := 0, 0
	for _, c := range h.box.Calls() {
		if c.Folder != "Receipts" {
			continue
		}
		switch c.Method {
		case providertest.MethodSelect:
			passes++
		case providertest.MethodFetchFlags:
			reads++
			if lo := uidsOf(t, c.Set)[0]; lo < uids[7] {
				t.Errorf("FETCH FLAGS %s between sweeps; the newest window starts at %d", c.Set, uids[7])
			}
		}
	}
	if passes < 3 || reads < 3 {
		t.Fatalf("%d passes and %d flag reads in 250 ms with a 20 ms interval", passes, reads)
	}
	if oldest() != 0 {
		t.Fatal("the oldest window was read before its sweep")
	}

	offset.Store(int64(4 * time.Hour)) // past the sweep interval
	h.waitFor("the sweep to read the oldest window", 3*time.Second, func() bool { return oldest() == 1 })
}

func TestAListStatusAnswerSparesPassesOverUnchangedFolders(t *testing.T) {
	// One LIST-STATUS says which folders moved; on the condstore tier the
	// counters include HIGHESTMODSEQ, so a folder whose numbers are the same
	// has nothing to fetch and is not selected at all.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{OtherInterval: 20 * time.Millisecond}})
	h.box.CreateFolder("Receipts")
	uid := h.box.Deliver("Receipts", mail("r", time.Now().Add(-time.Hour)))
	sub := h.live()

	h.box.ResetCalls()
	time.Sleep(250 * time.Millisecond)
	lists, selects := 0, 0
	for _, c := range h.box.Calls() {
		switch {
		case c.Method == providertest.MethodListFolders && c.Role == provider.RoleSync:
			lists++
		case c.Method == providertest.MethodSelect && c.Folder == "Receipts":
			selects++
		}
	}
	if lists < 3 {
		t.Fatalf("%d LIST-STATUS in 250 ms with a 20 ms interval", lists)
	}
	if selects != 0 {
		t.Errorf("an unchanged folder was selected %d times", selects)
	}

	h.box.SetFlags("Receipts", uid, imap.FlagSeen)
	got := payload[store.MessageFlags](t, next(t, sub, events.TypeMessageFlags, 2*time.Second))
	if got.FolderID != h.folderID("Receipts") || !got.Seen {
		t.Errorf("message.flags = %s", describe(got))
	}
}

func TestProgressIsThrottledDuringTheInitialSync(t *testing.T) {
	// Twelve batches, two folders: with the throttle nothing between the
	// first batch and each folder's end is announced.
	h := newHarness(t, setup{caps: providertest.GmailCaps(),
		opts: syncengine.Options{ProgressEvery: time.Hour, BatchSize: 20}})
	h.box.CreateFolder("Receipts")
	for i := range 200 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%03d", i), time.Now().Add(-time.Duration(200-i)*time.Hour)))
	}
	for i := range 40 {
		h.box.Deliver("Receipts", mail(fmt.Sprintf("r%02d", i), time.Now().Add(-time.Hour)))
	}
	h.start()
	h.waitLive()
	progress := h.waitProgressDone()
	if len(progress) < 2 || len(progress) > 3 {
		t.Fatalf("%d sync.progress events, want the first batch and one per finished folder", len(progress))
	}
	last := -1
	for _, ev := range progress {
		p := payload[store.SyncProgress](t, ev).Progress
		if p <= last {
			t.Errorf("progress went %d → %d", last, p)
		}
		last = p
	}
}

func TestSpamIsLookedAtOnItsOwnIntervalNotTheTrashs(t *testing.T) {
	// Mail that never reached the inbox is looked for in spam first, so spam
	// has its own, shorter interval; the trash keeps the long one.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{JunkInterval: 20 * time.Millisecond}})
	h.box.CreateFolder("Spam", imap.MailboxAttrJunk)
	h.box.CreateFolder("Trash", imap.MailboxAttrTrash)
	h.live()

	h.box.Deliver("Spam", mail("junk", time.Now()))
	h.box.Deliver("Trash", mail("binned", time.Now()))
	h.waitFor("the spam folder's next pass", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM messages WHERE folder_id = ?`, h.folderID("Spam")) == 1
	})
	if n := h.count(`SELECT count(*) FROM messages WHERE folder_id = ?`, h.folderID("Trash")); n != 0 {
		t.Fatalf("the trash was passed on the spam's interval: %d messages indexed", n)
	}
}
