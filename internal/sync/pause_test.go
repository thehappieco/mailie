package sync_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

// A paused mailbox (store.PauseSync, migration 0025) as the engine runs it:
// no worker, its interactive connection kept between calls, and a resume
// that catches up from what the index kept.

// pause pauses the harness's mailbox and tells the engine, as the service
// does after the pause commits.
func (h *harness) pause() {
	h.t.Helper()
	if _, err := h.db.PauseSync(context.Background(), h.account, h.user, nil); err != nil {
		h.t.Fatalf("pausing: %v", err)
	}
	h.engine.Reconcile(h.account)
}

// resume resumes it and tells the engine.
func (h *harness) resume() {
	h.t.Helper()
	if _, err := h.db.ResumeSync(context.Background(), h.account, h.user, "", nil); err != nil {
		h.t.Fatalf("resuming: %v", err)
	}
	h.engine.Reconcile(h.account)
}

func (h *harness) noop() {
	h.t.Helper()
	if err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
		return s.Noop(ctx)
	}); err != nil {
		h.t.Fatalf("an interactive call: %v", err)
	}
}

// pausedWhileRunning pauses a mailbox whose worker holds its interactive
// connection, and waits until that worker has stopped and logged it out.
func (h *harness) pausedWhileRunning() {
	h.t.Helper()
	h.noop()
	h.pause()
	h.waitFor("the stopping worker to log the interactive connection out", 5*time.Second, func() bool {
		return h.box.Logouts(provider.RoleInteractive) == 1
	})
	h.waitFor("the sync and idle connections to log out", 5*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleSync, provider.RoleIdle) == 0
	})
}

func TestAPausedMailboxHasNoWorkerAndKeepsItsInteractiveConnectionBetweenCalls(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{InteractiveIdle: 150 * time.Millisecond}})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.start()
	h.waitLive()
	h.pausedWhileRunning()
	if err := h.engine.Trigger(context.Background(), h.account); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("a trigger while paused: %v, want not running", err)
	}
	opens := h.box.Opens(provider.RoleInteractive)
	h.noop()
	h.noop()
	if n := h.box.Opens(provider.RoleInteractive) - opens; n != 1 {
		t.Errorf("two calls on the paused mailbox logged in %d times, want once", n)
	}
	if n := h.box.OpenSessions(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections open between calls, want the one kept", n)
	}
	h.waitFor("the unused interactive connection to close", 3*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleInteractive) == 0
	})
	if n := h.box.OpenSessions(); n != 0 {
		t.Errorf("%d connections open on the idle paused mailbox", n)
	}
	if st, err := h.engine.Status(context.Background(), h.account); err != nil || st.Running || st.State != "stopped" {
		t.Errorf("the paused mailbox's status: %+v (%v), want stopped with its index", st, err)
	}
}

func TestAPauseThatEndsWithoutAResumeLogsTheInteractiveConnectionOut(t *testing.T) {
	t.Run("at the reconcile", func(t *testing.T) {
		h := newHarness(t, setup{caps: providertest.GmailCaps()})
		h.start()
		h.waitLive()
		h.pausedWhileRunning()
		h.noop()
		if n := h.box.OpenSessions(provider.RoleInteractive); n != 1 {
			t.Fatalf("%d interactive connections kept while paused, want one", n)
		}
		if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
			t.Fatal(err)
		}
		h.engine.Reconcile(h.account)
		h.waitFor("the interactive connection to log out", 3*time.Second, func() bool {
			return h.box.OpenSessions(provider.RoleInteractive) == 0
		})
	})
	t.Run("at the periodic check", func(t *testing.T) {
		h := newHarness(t, setup{caps: providertest.GmailCaps(),
			opts: syncengine.Options{EligibilityInterval: 30 * time.Millisecond}})
		if _, err := h.db.PauseSync(context.Background(), h.account, h.user, nil); err != nil {
			t.Fatal(err)
		}
		h.start()
		// Nothing is told: the check at start and every 30 ms finds it
		// paused, and keeps its slot.
		h.waitFor("the slot kept between calls", 3*time.Second, func() bool {
			h.noop()
			return h.box.OpenSessions(provider.RoleInteractive) == 1
		})
		if n := h.box.OpenSessions(provider.RoleSync, provider.RoleIdle); n != 0 {
			t.Errorf("%d sync and idle connections on a paused mailbox", n)
		}
		if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
			t.Fatal(err)
		}
		h.waitFor("the interactive connection to log out", 3*time.Second, func() bool {
			return h.box.OpenSessions(provider.RoleInteractive) == 0
		})
	})
}

func TestAPausedMailboxNeverHoldsMoreThanThreeConnections(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	for i := range 5 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	h.start()
	h.waitLive()
	h.pausedWhileRunning()
	h.noop()
	// Resumed while its interactive connection is kept, with callers on it.
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
					return s.Noop(ctx)
				})
				if err != nil && !errors.Is(err, provider.ErrConnClosed) {
					t.Errorf("interactive: %v", err)
				}
			}
		}()
	}
	h.resume()
	wg.Wait()
	h.waitFor("the idle connection again", 5*time.Second, func() bool { return h.box.OpenSessions(provider.RoleIdle) == 1 })
	if peak := h.box.PeakSessions(); peak > 3 {
		t.Errorf("%d connections open at once, want at most 3", peak)
	}
	for _, role := range []provider.Role{provider.RoleSync, provider.RoleIdle, provider.RoleInteractive} {
		if peak := h.box.PeakSessions(role); peak > 1 {
			t.Errorf("%d %s connections at once, want one", peak, role)
		}
	}
}

func TestAnUnknownSendOnAPausedMailboxIsSettledByTheFirstPassAfterTheResume(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.CreateFolder("[Gmail]/Sent Mail", imap.MailboxAttrSent)
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.start()
	h.waitLive()
	h.pausedWhileRunning()
	h.exec(`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_at, updated_at)
		VALUES (?, 'k-unknown', 'h', 'reply@mail.example', 'unknown', 1, 1)`, h.account)
	h.box.Deliver("[Gmail]/Sent Mail", mail("reply", time.Now()))
	time.Sleep(100 * time.Millisecond)
	state := func() int {
		return h.count(`SELECT count(*) FROM sends WHERE idempotency_key = 'k-unknown' AND state = 'sent'`)
	}
	if state() != 0 {
		t.Fatal("the send was settled while the mailbox was paused")
	}
	h.resume()
	if err := triggerOnce(h); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the send settled by the pass after the resume", 5*time.Second, func() bool { return state() == 1 })
}

// triggerOnce asks the resumed mailbox's new worker for a pass, once it is
// there.
func triggerOnce(h *harness) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := h.engine.Trigger(context.Background(), h.account)
		if !errors.Is(err, service.ErrSyncNotRunning) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAResumedMailboxCatchesUpFromWhatItKept(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps(),
		opts: syncengine.Options{ConfirmDiffDelay: 100 * time.Millisecond}})
	h.box.CreateFolder("Receipts")
	var uids []imap.UID
	for i := range 3 {
		uids = append(uids, h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour))))
	}
	h.box.Deliver("Receipts", mail("r", time.Now().Add(-time.Hour)))
	h.start()
	h.waitLive()
	kept, flagged, gone := h.rowID("INBOX", uids[0]), h.rowID("INBOX", uids[1]), h.rowID("INBOX", uids[2])
	cursors := h.count(`SELECT count(*) FROM folders WHERE account_id = ? AND backfill_cursor = 0 AND uidvalidity <> 0`, h.account)
	h.pausedWhileRunning()

	// While it is paused: new mail, one of it older than the window; a flag
	// changed; a deletion; and a new folder, with mail inside and outside
	// its window.
	h.box.Deliver("INBOX", mail("new", time.Now()))
	h.box.Deliver("INBOX", mail("old", time.Now().Add(-200*24*time.Hour)))
	h.box.SetFlags("INBOX", uids[1], imap.FlagSeen)
	h.box.Expunge("INBOX", uids[2])
	h.box.CreateFolder("Travel")
	h.box.Deliver("Travel", mail("trip", time.Now().Add(-time.Hour)))
	h.box.Deliver("Travel", mail("ancient", time.Now().Add(-200*24*time.Hour)))
	time.Sleep(50 * time.Millisecond)
	if n := h.count(`SELECT count(*) FROM messages WHERE account_id = ? AND subject IN ('Subject new', 'Subject old')`, h.account); n != 0 {
		t.Fatalf("%d messages indexed while paused", n)
	}
	h.box.ResetCalls()
	h.resume()
	if err := triggerOnce(h); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the catch-up", 10*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM messages WHERE account_id = ? AND subject IN ('Subject new', 'Subject old', 'Subject trip')`,
			h.account) == 3 &&
			h.count(`SELECT count(*) FROM messages WHERE id = ? AND seen = 1`, flagged) == 1 &&
			h.count(`SELECT count(*) FROM messages WHERE id = ?`, gone) == 0
	})
	if n := h.count(`SELECT count(*) FROM messages WHERE subject = 'Subject ancient'`); n != 0 {
		t.Error("the new folder's mail outside its window was indexed")
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE id = ?`, kept); n != 1 {
		t.Error("a message kept through the pause lost its row")
	}
	// Nothing indexed before the pause was fetched again from scratch: the
	// old folders' initial sync stays done.
	if n := h.count(`SELECT count(*) FROM folders WHERE account_id = ? AND backfill_cursor = 0 AND uidvalidity <> 0
		AND name IN ('INBOX', 'Receipts')`, h.account); n != 2 || cursors < 2 {
		t.Errorf("%d of the old folders kept their finished initial sync (%d before)", n, cursors)
	}
	for _, c := range h.box.Calls() {
		if c.Method == providertest.MethodFetchSummaries && c.Folder == "INBOX" {
			for _, uid := range uidsOf(t, c.Set) {
				if uid == uids[0] || uid == uids[1] {
					t.Errorf("the resume fetched again the summary of INBOX UID %d (%s)", uid, c.Set)
				}
			}
		}
	}
}

func TestAPassInFlightWhenThePauseCommitsStoresNothingAndRecordsNoFailure(t *testing.T) {
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
	if _, err := h.db.PauseSync(context.Background(), h.account, h.user, nil); err != nil {
		t.Fatal(err)
	}
	close(release) // the batch lands after the pause; nobody told the engine yet
	time.Sleep(150 * time.Millisecond)
	if n := h.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("%d rows stored after the pause", n)
	}
	h.waitFor("the worker to notice and stop", 5*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleSync, provider.RoleIdle) == 0
	})
	if n := h.count(`SELECT consecutive_failures FROM accounts WHERE id = ?`, h.account); n != 0 {
		t.Errorf("the pause was recorded as %d sync failures", n)
	}
}

func TestACallInFlightOnAMailboxThatLeftItsPauseNeverHoldsTheEngineBack(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.start()
	h.waitLive()
	h.pausedWhileRunning()
	// A long call on the paused mailbox's kept connection: a download.
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
			close(entered)
			<-release
			return s.Noop(ctx)
		})
	}()
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo) // before the engine stops: cleanups run last first
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}
	// Its sync turned off while the call runs: the engine lets go of the
	// pause and of the connection, which the call still holds.
	if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
		t.Fatal(err)
	}
	h.engine.Reconcile(h.account)
	time.Sleep(100 * time.Millisecond) // the engine has looked at it
	// Turned on again: the engine starts its worker while the call is still
	// running, never once it ends.
	if _, _, err := h.db.GrantSyncConsent(context.Background(), h.user, "test"); err != nil {
		t.Fatal(err)
	}
	h.engine.Reconcile(h.account)
	h.waitFor("the worker of the mailbox turned on again, the call still running", 3*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleIdle) == 1
	})
	select {
	case err := <-done:
		t.Fatalf("the call ended before it was let go: %v", err)
	default:
	}
	letGo()
	if err := <-done; err != nil {
		t.Errorf("the call: %v", err)
	}
	if peak := h.box.PeakSessions(); peak > 3 {
		t.Errorf("%d connections open at once, want at most 3", peak)
	}
}

func TestAPausedMailboxsKeptConnectionIsLoggedOutWhenTheEngineStops(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.start()
	h.waitLive()
	h.pausedWhileRunning()
	h.noop()
	if n := h.box.OpenSessions(provider.RoleInteractive); n != 1 {
		t.Fatalf("%d interactive connections kept while paused, want one", n)
	}
	h.stop()
	if n := h.box.OpenSessions(); n != 0 {
		t.Errorf("%d connections still open once the engine stopped", n)
	}
	if n := h.box.Logouts(provider.RoleInteractive); n != 2 {
		t.Errorf("%d interactive connections logged out, want the worker's and the one the pause kept", n)
	}
	if err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
		return s.Noop(ctx)
	}); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("a call once the engine stopped: %v, want not running", err)
	}
}

func TestAWorkerIsStoppedWhenWhetherItsMailboxIsPausedCannotBeRead(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	syncengine.FailIndexFollowsForTest(h.engine, errors.New("database is locked"))
	h.start()
	h.waitLive()
	if _, err := h.db.WithdrawSyncConsent(context.Background(), h.user); err != nil {
		t.Fatal(err)
	}
	h.engine.Reconcile(h.account)
	// Nothing else looks again for an hour: the reconcile told stops it.
	h.waitFor("the worker of a mailbox that may no longer sync to stop", 3*time.Second, func() bool {
		err := h.engine.Trigger(context.Background(), h.account)
		return errors.Is(err, service.ErrSyncNotRunning) && h.box.OpenSessions(provider.RoleSync, provider.RoleIdle) == 0
	})
}
