package sync_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestNothingIsFetchedBeforeConsent(t *testing.T) {
	// The privacy policy: nothing about a person's messages is stored until
	// they agree to it. Not a login, not a folder list.
	h := newHarness(t, setup{noConsent: true, caps: providertest.GmailCaps(),
		opts: syncengine.Options{EligibilityInterval: 20 * time.Millisecond}})
	h.box.Deliver("INBOX", mail("a", time.Now()))
	h.start()
	h.engine.Reconcile(h.account)

	time.Sleep(200 * time.Millisecond) // ten eligibility checks
	if n := h.box.CallCount(providertest.MethodOpen); n != 0 {
		t.Fatalf("the engine opened %d connections for a person who has not consented", n)
	}
	if n := h.count(`SELECT count(*) FROM folders`) + h.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("%d rows stored before consent", n)
	}
	if _, err := h.engine.Status(context.Background(), h.account); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("status before consent: %v, want not running", err)
	}

	if _, _, err := h.db.GrantSyncConsent(context.Background(), h.user, "test"); err != nil {
		t.Fatal(err)
	}
	h.engine.Reconcile(h.account)
	h.waitFor("the consented mailbox to be indexed", 5*time.Second, func() bool { return h.messages() == 1 })
}

func TestAnAccountIsPickedUpByThePeriodicCheckWithoutBeingTold(t *testing.T) {
	// Reconcile is how a change is noticed at once; the periodic re-read is
	// the backstop for a change nobody reported.
	h := newHarness(t, setup{noConsent: true, caps: providertest.GmailCaps(),
		opts: syncengine.Options{EligibilityInterval: 30 * time.Millisecond}})
	h.box.Deliver("INBOX", mail("a", time.Now()))
	h.start()
	if _, _, err := h.db.GrantSyncConsent(context.Background(), h.user, "test"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the periodic check to start the account", 5*time.Second, func() bool { return h.messages() == 1 })
}

func TestTheInitialSyncTakesTheWindowNewestFirstInBoundedBatchesAndAnnouncesNothing(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(), initialDays: 90})
	now := time.Now()
	for i := range 20 { // older than the window, lower UIDs
		h.box.Deliver("INBOX", mail(fmt.Sprintf("old%02d", i), now.AddDate(0, 0, -120+i)))
	}
	for i := range 430 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("in%03d", i), now.Add(-time.Duration(430-i)*2*time.Hour)))
	}
	h.box.CreateFolder("[Gmail]/Sent Mail", imap.MailboxAttrSent)
	for i := range 5 {
		h.box.Deliver("[Gmail]/Sent Mail", mail(fmt.Sprintf("sent%d", i), now.Add(-time.Hour)))
	}
	h.start()
	h.waitLive()

	if n := h.messages(); n != 435 {
		t.Fatalf("%d messages indexed, want the 430 + 5 inside the window", n)
	}
	cutoff := now.AddDate(0, 0, -91).Unix()
	if n := h.count(`SELECT count(*) FROM messages WHERE internal_date < ?`, cutoff); n != 0 {
		t.Errorf("%d messages from outside the 90-day window were indexed", n)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE body_text <> '' OR snippet <> ''`); n != 0 {
		t.Errorf("%d messages have a body or a snippet; phase 2 stores metadata only", n)
	}
	if n := h.box.CallCount(providertest.MethodFetchPart) + h.box.CallCount(providertest.MethodFetchRaw) +
		h.box.CallCount(providertest.MethodFetchHeader); n != 0 {
		t.Errorf("the engine fetched message content %d times", n)
	}

	var inbox [][]imap.UID
	for _, c := range h.box.Calls() {
		if c.Method == providertest.MethodFetchSummaries && c.Folder == "INBOX" {
			inbox = append(inbox, uidsOf(t, c.Set))
		}
	}
	if len(inbox) < 3 {
		t.Fatalf("the inbox was fetched in %d batches, want at least 3 for 430 messages", len(inbox))
	}
	for _, batch := range inbox {
		if len(batch) > 200 {
			t.Errorf("a batch of %d UIDs; batches are at most 200", len(batch))
		}
	}
	state, _ := h.box.Folder("INBOX")
	newest := state.UIDs[len(state.UIDs)-1]
	if !slices.Contains(inbox[0], newest) {
		t.Errorf("the first batch %v does not hold the newest UID %d: the initial sync goes newest first", inbox[0], newest)
	}
	if slices.Contains(inbox[0], state.UIDs[20]) {
		t.Error("the first batch holds the oldest message of the window")
	}

	if evs := h.journaled(events.TypeMessageNew); len(evs) != 0 {
		t.Errorf("the initial sync announced %d messages as new", len(evs))
	}
	done := map[string]bool{}
	for _, ev := range h.journaled(events.TypeFolderChanged) {
		p := payload[store.FolderChanged](t, ev)
		if p.Change == store.FolderInitialDone {
			if done[p.Name] {
				t.Errorf("folder %d announced initial_done twice", p.FolderID)
			}
			done[p.Name] = true
		}
	}
	if !done["INBOX"] || !done["[Gmail]/Sent Mail"] {
		t.Errorf("initial_done for %v, want the inbox and sent mail", done)
	}
	h.waitProgressDone()
	st, err := h.engine.Status(context.Background(), h.account)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.State != "live" || st.InitialProgress != 100 || st.Messages != 435 ||
		st.FoldersSynced != st.FoldersTotal || st.Tier != syncengine.TierCondStore {
		t.Errorf("status after the initial sync: %s", describe(st))
	}
}

func TestAnInterruptedInitialSyncResumesWithoutFetchingAgain(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft,
		opts: syncengine.Options{BatchSize: 50}})
	now := time.Now()
	for i := range 230 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%03d", i), now.Add(-time.Duration(230-i)*time.Hour)))
	}
	var (
		mu      sync.Mutex
		fetches []string
		failed  string
		calls   int
	)
	h.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method != providertest.MethodFetchSummaries {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 3 {
			failed = c.Set
			return fmt.Errorf("%w: the network went away", provider.ErrConnClosed)
		}
		fetches = append(fetches, c.Set)
		return nil
	})
	h.start()
	h.waitLive()
	if n := h.messages(); n != 230 {
		t.Fatalf("%d messages indexed after the interruption, want 230", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if failed == "" {
		t.Fatal("the interruption never happened")
	}
	seen := map[imap.UID]int{}
	for _, set := range fetches {
		for _, uid := range uidsOf(t, set) {
			seen[uid]++
		}
	}
	for uid, n := range seen {
		if n > 1 {
			t.Errorf("UID %d was fetched %d times: the backfill did not resume from its cursor", uid, n)
		}
	}
	if h.box.Opens(provider.RoleSync) < 2 {
		t.Error("the engine did not reconnect after the connection dropped")
	}
}

func TestNewMailArrivesThroughIdleWithinTwoSeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind provider.Kind
		caps provider.Caps
	}{
		{"condstore", provider.KindGmail, providertest.GmailCaps()},
		{"uidpoll", provider.KindMicrosoft, providertest.ExchangeCaps()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every timer is an hour away: only IDLE can deliver this.
			h := newHarness(t, setup{kind: tc.kind, caps: tc.caps})
			h.box.Deliver("INBOX", mail("before", time.Now().Add(-time.Hour)))
			h.start()
			h.waitLive()
			h.waitFor("the idle connection", 5*time.Second, func() bool {
				return h.box.CallCount(providertest.MethodIdle) > 0
			})
			sub := h.subscribe()

			sent := time.Now()
			uid := h.box.Deliver("INBOX", mail("fresh", time.Now()))
			ev := next(t, sub, events.TypeMessageNew, 2*time.Second)
			if took := time.Since(sent); took > 2*time.Second {
				t.Fatalf("new mail took %s", took)
			}
			got := payload[store.MessageNew](t, ev)
			if got.Subject != "Subject fresh" || !got.FirstInboxCopy || got.FolderRole != "inbox" || ev.AccountID != h.account {
				t.Errorf("message.new = %s", describe(got))
			}
			if n := h.count(`SELECT count(*) FROM messages WHERE uid = ? AND subject = 'Subject fresh'`, uint32(uid)); n != 1 {
				t.Errorf("the new message is indexed %d times", n)
			}
			if evs := h.journaled(events.TypeMessageNew); len(evs) != 1 {
				t.Errorf("%d message.new journaled, want exactly the one", len(evs))
			}
		})
	}
}

func TestAFlagChangeReachesTheIndexOnEitherTier(t *testing.T) {
	for _, tc := range []struct {
		name, tier string
		kind       provider.Kind
		caps       provider.Caps
	}{
		{"condstore", syncengine.TierCondStore, provider.KindGmail, providertest.GmailCaps()},
		{"uidpoll", syncengine.TierUIDPoll, provider.KindMicrosoft, providertest.ExchangeCaps()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, setup{kind: tc.kind, caps: tc.caps})
			h.box.CreateFolder("Receipts")
			var uids []imap.UID
			for i := range 3 {
				uids = append(uids, h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour))))
			}
			label := h.box.Deliver("Receipts", mail("r", time.Now().Add(-time.Hour)))
			h.start()
			h.waitLive()
			h.waitFor("the idle connection", 5*time.Second, func() bool {
				return h.box.CallCount(providertest.MethodIdle) > 0
			})
			if got := h.count(`SELECT count(*) FROM accounts WHERE id = ? AND sync_tier_resolved = ?`, h.account, tc.tier); got != 1 {
				t.Errorf("the resolved tier was not recorded as %s", tc.tier)
			}
			sub := h.subscribe()
			h.box.ResetCalls()

			// In the inbox, IDLE says something changed.
			h.box.SetFlags("INBOX", uids[1], imap.FlagSeen, imap.FlagFlagged)
			ev := next(t, sub, events.TypeMessageFlags, 2*time.Second)
			if got := payload[store.MessageFlags](t, ev); !got.Seen || !got.Flagged {
				t.Errorf("message.flags = %s", describe(got))
			}
			if n := h.count(`SELECT count(*) FROM messages WHERE uid = ? AND folder_id = ? AND seen = 1 AND flagged = 1`,
				uint32(uids[1]), h.folderID("INBOX")); n != 1 {
				t.Error("the index does not show the new flags")
			}

			// Elsewhere nothing is listening; a requested pass finds it.
			h.box.SetFlags("Receipts", label, imap.FlagSeen)
			if err := h.engine.Trigger(context.Background(), h.account); err != nil {
				t.Fatal(err)
			}
			ev = next(t, sub, events.TypeMessageFlags, 2*time.Second)
			if got := payload[store.MessageFlags](t, ev); !got.Seen || got.FolderID != h.folderID("Receipts") {
				t.Errorf("message.flags for the label = %s", describe(got))
			}

			var changedSince, plain int
			for _, c := range h.box.Calls() {
				if c.Method == providertest.MethodFetchFlags {
					if c.ChangedSince > 0 {
						changedSince++
					} else {
						plain++
					}
				}
			}
			switch tc.tier {
			case syncengine.TierCondStore:
				if changedSince == 0 {
					t.Error("the condstore tier never asked CHANGEDSINCE")
				}
			case syncengine.TierUIDPoll:
				if changedSince != 0 || plain == 0 {
					t.Errorf("the uidpoll tier fetched flags %d times with CHANGEDSINCE and %d without", changedSince, plain)
				}
			}
		})
	}
}

func TestAnAccountNeverHoldsMoreThanThreeConnections(t *testing.T) {
	// Gmail allows fifteen connections across every client the person runs.
	// Reconnections, a busy interactive caller and dropped connections must
	// never take this account past its three, one per role.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{OtherInterval: 20 * time.Millisecond}})
	for _, name := range []string{"Receipts", "Travel", "[Gmail]/Trash"} {
		h.box.CreateFolder(name)
		h.box.Deliver(name, mail(name, time.Now().Add(-time.Hour)))
	}
	for i := range 30 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	h.start()
	h.waitLive()

	var (
		wg   sync.WaitGroup
		busy atomic.Int32
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
					if busy.Add(1) > 1 {
						t.Error("two interactive callers ran at once")
					}
					defer busy.Add(-1)
					_, err := s.Select(ctx, "INBOX", false, 0)
					return err
				})
				if err != nil && !errors.Is(err, provider.ErrConnClosed) {
					t.Errorf("interactive: %v", err)
				}
			}
		}()
	}
	for i := range 5 {
		h.box.KillSessions([]provider.Role{provider.RoleSync, provider.RoleIdle, provider.RoleInteractive}[i%3])
		h.box.Deliver("INBOX", mail(fmt.Sprintf("late%d", i), time.Now()))
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
	h.waitFor("the late mail", 5*time.Second, func() bool { return h.messages() == 30+3+5 })

	if peak := h.box.PeakSessions(); peak > 3 {
		t.Errorf("%d connections open at once, want at most 3", peak)
	}
	for _, role := range []provider.Role{provider.RoleSync, provider.RoleIdle, provider.RoleInteractive} {
		if peak := h.box.PeakSessions(role); peak > 1 {
			t.Errorf("%d %s connections at once, want one", peak, role)
		}
	}
}

func TestGmailNeverSyncsAllMailStarredOrImportant(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.CreateContainer("[Gmail]")
	h.box.CreateFolder("[Gmail]/All Mail", imap.MailboxAttrAll)
	h.box.CreateFolder("[Gmail]/Starred", imap.MailboxAttrFlagged)
	h.box.CreateFolder("[Gmail]/Important", imap.MailboxAttrImportant)
	h.box.CreateFolder("[Gmail]/Spam", imap.MailboxAttrJunk)
	uid := h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	for _, name := range []string{"[Gmail]/All Mail", "[Gmail]/Starred", "[Gmail]/Important"} {
		h.box.CopyTo("INBOX", uid, name)
	}
	// An override naming All Mail as the archive changes its label, not what
	// it holds.
	h.exec(`UPDATE accounts SET folder_overrides = '{"archive":"[Gmail]/All Mail"}' WHERE id = ?`, h.account)
	h.start()
	h.waitLive()

	for _, c := range h.box.Calls() {
		switch c.Folder {
		case "[Gmail]/All Mail", "[Gmail]/Starred", "[Gmail]/Important", "[Gmail]":
			t.Errorf("%s on %q: gmail's archive and flag views are never synced", c.Method, c.Folder)
		}
	}
	if n := h.count(`SELECT count(*) FROM folders WHERE synced = 1 AND name IN
		('[Gmail]/All Mail', '[Gmail]/Starred', '[Gmail]/Important', '[Gmail]')`); n != 0 {
		t.Errorf("%d of gmail's never-synced folders are marked synced", n)
	}
	if n := h.count(`SELECT count(*) FROM folders WHERE synced = 1 AND name = '[Gmail]/Spam'`); n != 1 {
		t.Error("spam is not synced")
	}
	if n := h.messages(); n != 1 {
		t.Errorf("%d messages indexed, want only the inbox copy", n)
	}
}

func TestAGmailMailboxConnectedAsGenericIMAPStillNeverSyncsAllMail(t *testing.T) {
	// A Workspace domain the provider guess did not recognise, or "other
	// IMAP" with an app password: the account says imap, the server is
	// Gmail. Its All Mail is the whole mailbox again, and Starred and
	// Important are views of flags; the policy says they are never synced.
	for _, tc := range []struct {
		name string
		caps provider.Caps
		host string
	}{
		{"gmail's capability", func() provider.Caps {
			c := providertest.GmailCaps()
			c.Raw = []string{"IMAP4rev1", "X-GM-EXT-1", "IDLE"}
			return c
		}(), "imap.mail.example"},
		{"gmail's host", providertest.GmailCaps(), "imap.gmail.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, setup{caps: tc.caps, kind: provider.KindIMAP})
			h.exec(`UPDATE accounts SET imap_host = ? WHERE id = ?`, tc.host, h.account)
			h.box.CreateContainer("[Gmail]")
			h.box.CreateFolder("[Gmail]/All Mail", imap.MailboxAttrAll)
			h.box.CreateFolder("[Gmail]/Starred", imap.MailboxAttrFlagged)
			h.box.CreateFolder("[Gmail]/Important", imap.MailboxAttrImportant)
			uid := h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
			for _, name := range []string{"[Gmail]/All Mail", "[Gmail]/Starred", "[Gmail]/Important"} {
				h.box.CopyTo("INBOX", uid, name)
			}
			h.start()
			h.waitLive()

			for _, c := range h.box.Calls() {
				switch c.Folder {
				case "[Gmail]/All Mail", "[Gmail]/Starred", "[Gmail]/Important":
					t.Errorf("%s on %q", c.Method, c.Folder)
				}
			}
			if n := h.count(`SELECT count(*) FROM folders WHERE synced = 1 AND name IN
				('[Gmail]/All Mail', '[Gmail]/Starred', '[Gmail]/Important')`); n != 0 {
				t.Errorf("%d of gmail's never-synced folders are marked synced", n)
			}
			if n := h.messages(); n != 1 {
				t.Errorf("%d messages indexed, want only the inbox copy", n)
			}
		})
	}
}

func TestWithdrawingConsentStopsTheAccountAndNothingMoreIsStored(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.CreateFolder("Receipts")
	for i := range 5 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	h.box.Deliver("Receipts", mail("r", time.Now().Add(-time.Hour)))
	h.start()
	h.waitLive()
	h.waitFor("the idle connection", 5*time.Second, func() bool { return h.box.OpenSessions(provider.RoleIdle) == 1 })

	if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
		t.Fatal(err)
	}
	h.engine.Reconcile(h.account)
	h.waitFor("every connection to log out", 5*time.Second, func() bool { return h.box.OpenSessions() == 0 })

	h.box.Deliver("INBOX", mail("after", time.Now()))
	time.Sleep(100 * time.Millisecond)
	for table, query := range map[string]string{
		"messages": `SELECT count(*) FROM messages WHERE account_id = ?`,
		"folders":  `SELECT count(*) FROM folders WHERE account_id = ?`,
		"events":   `SELECT count(*) FROM events WHERE account_id = ?`,
	} {
		if n := h.count(query, h.account); n != 0 {
			t.Errorf("%d %s rows after the withdrawal", n, table)
		}
	}
	if _, err := h.engine.Status(context.Background(), h.account); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("status after the withdrawal: %v, want not running", err)
	}
	if err := h.engine.Trigger(context.Background(), h.account); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("a trigger after the withdrawal: %v, want not running", err)
	}
}

func TestABatchInFlightWhenConsentIsWithdrawnStoresNothing(t *testing.T) {
	// The withdrawal commits while a batch is on its way; the engine hears
	// about it only later. The batch's own transaction re-checks consent, so
	// the index stays empty whatever the worker does next.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	for i := range 10 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.box.OnCall(func(ctx context.Context, c providertest.Call) error {
		if c.Method != providertest.MethodFetchSummaries {
			return nil
		}
		once.Do(func() { close(held) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	h.start()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the backfill never started")
	}
	if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
		t.Fatal(err)
	}
	close(release) // the batch lands after the withdrawal; nobody told the engine yet
	time.Sleep(150 * time.Millisecond)
	if n := h.count(`SELECT count(*) FROM messages`) + h.count(`SELECT count(*) FROM folders`); n != 0 {
		t.Fatalf("%d rows stored after consent was withdrawn", n)
	}
	h.waitFor("the worker to notice and stop", 5*time.Second, func() bool { return h.box.OpenSessions() == 0 })
}

func TestTheInteractiveConnectionIsReusedAndClosedWhenIdle(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{InteractiveIdle: 50 * time.Millisecond}})
	h.start()
	h.waitLive()
	for range 3 {
		err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
			return s.Noop(ctx)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.box.Opens(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections opened for three calls in a row, want one", n)
	}
	h.waitFor("the unused interactive connection to close", 2*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleInteractive) == 0
	})
}

func TestWithoutAWorkerTheInteractiveConnectionLastsOneCallAndCallersQueue(t *testing.T) {
	// Before consent the service still lists folders live. The connection is
	// the account's interactive one all the same: callers queue for it, and
	// with no worker to count against it is logged out after each call.
	h := newHarness(t, setup{noConsent: true, caps: providertest.GmailCaps()})
	h.start()
	var (
		wg   sync.WaitGroup
		busy atomic.Int32
	)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
				if busy.Add(1) > 1 {
					t.Error("two callers ran on the interactive connection at once")
				}
				defer busy.Add(-1)
				time.Sleep(5 * time.Millisecond)
				_, err := s.ListFolders(ctx, true)
				return err
			})
			if err != nil {
				t.Errorf("interactive: %v", err)
			}
		}()
	}
	wg.Wait()
	if peak := h.box.PeakSessions(); peak != 1 {
		t.Errorf("%d connections at once for five queued callers, want one", peak)
	}
	if n := h.box.OpenSessions(); n != 0 {
		t.Errorf("%d connections left open without a worker", n)
	}
	if n := h.box.Opens(provider.RoleInteractive); n != 5 {
		t.Errorf("%d interactive connections opened for five calls, want one each", n)
	}
	if n := h.count(`SELECT count(*) FROM folders`) + h.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("%d rows stored by a live listing before consent", n)
	}

	// An engine that has shut down lends nothing.
	h.stop()
	if err := h.engine.Interactive(context.Background(), h.account, func(context.Context, provider.Session) error {
		return nil
	}); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("after shutdown: %v", err)
	}
}

func TestProvingANewGrantTakesTheInteractiveConnectionsPlace(t *testing.T) {
	// A person re-authorises an account whose worker holds its sync, idle
	// and interactive connections. The check that the new grant opens the
	// mailbox has to log in with it, so it cannot use the engine's
	// connection; it takes that connection's place instead, and callers
	// wanting the interactive connection wait for it, so the account never
	// holds four.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.start()
	h.waitLive()
	h.waitFor("the idle connection", 5*time.Second, func() bool { return h.box.OpenSessions(provider.RoleIdle) == 1 })
	noop := func(ctx context.Context, s provider.Session) error { return s.Noop(ctx) }
	if err := h.engine.Interactive(context.Background(), h.account, noop); err != nil {
		t.Fatal(err)
	}
	if n := h.box.OpenSessions(); n != 3 {
		t.Fatalf("%d connections open before the check, want sync, idle and interactive", n)
	}

	waited := make(chan error, 1)
	err := h.engine.InPlaceOfInteractive(context.Background(), h.account, func(ctx context.Context) error {
		if n := h.box.OpenSessions(provider.RoleInteractive); n != 0 {
			t.Errorf("the engine's interactive connection is still open during the check")
		}
		s, err := h.box.Open(ctx, provider.RoleInteractive) // the check's own login
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		go func() { waited <- h.engine.Interactive(ctx, h.account, noop) }()
		select {
		case err := <-waited:
			t.Errorf("an interactive call ran during the check: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the interactive call that waited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the interactive call that waited never ran")
	}
	if peak := h.box.PeakSessions(); peak > 3 {
		t.Errorf("%d connections open at once, want at most 3", peak)
	}
}
