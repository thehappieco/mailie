package sync_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestARefusedLoginBacksOffLongAndSaysWhy(t *testing.T) {
	// A refused password is not fixed by trying again in a second: the
	// engine waits the long interval, keeps the account active, and the
	// status names the class of failure, never the server's words.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), opts: syncengine.Options{LongBackoff: 150 * time.Millisecond}})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.box.FailAlways(providertest.MethodOpen, fmt.Errorf("%w: NO [AUTHENTICATIONFAILED]", provider.ErrAuthFailed))
	h.start()
	h.waitFor("the backoff", 5*time.Second, func() bool {
		st, err := h.engine.Status(context.Background(), h.account)
		return err == nil && st.State == "backoff" && st.ErrorClass == "auth_failed"
	})
	opens := h.box.CallCount(providertest.MethodOpen)
	time.Sleep(200 * time.Millisecond)
	if more := h.box.CallCount(providertest.MethodOpen) - opens; more > 2 {
		t.Errorf("%d more logins in 200 ms after a refusal; the long backoff is 150 ms", more)
	}
	if n := h.count(`SELECT count(*) FROM accounts WHERE id = ? AND state = 'active' AND last_error = 'auth_failed'
		AND consecutive_failures > 0`, h.account); n != 1 {
		t.Error("the failure is not recorded on the account, or the account left active")
	}

	h.box.ClearFailures()
	h.waitLive()
	st, err := h.engine.Status(context.Background(), h.account)
	if err != nil {
		t.Fatal(err)
	}
	if st.ErrorClass != "" {
		t.Errorf("error class %q after recovering", st.ErrorClass)
	}
	h.waitFor("the good pass on the account row", 5*time.Second, func() bool {
		return h.count(`SELECT consecutive_failures FROM accounts WHERE id = ?`, h.account) == 0
	})
}

func TestAGrantFoundDeadStopsTheWorkerAndKeepsTheIndex(t *testing.T) {
	// needs_reauth: only the person can fix it. The worker stops — no
	// retries against a dead grant — and what was indexed stays, reported
	// as stopped, until they consent at the provider again.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	for i := range 3 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	h.start()
	h.waitLive()

	h.box.FailAlways(providertest.MethodOpen, fmt.Errorf("%w: invalid_grant", provider.ErrNeedsReauth))
	h.box.KillSessions()
	// What account.Registry does when the mailbox reports the dead grant.
	h.exec(`UPDATE accounts SET state = 'needs_reauth' WHERE id = ?`, h.account)
	h.engine.Reconcile(h.account)
	h.waitFor("the worker to stop", 5*time.Second, func() bool {
		st, err := h.engine.Status(context.Background(), h.account)
		return err == nil && !st.Running && st.State == "stopped"
	})
	opens := h.box.CallCount(providertest.MethodOpen)
	time.Sleep(150 * time.Millisecond)
	if more := h.box.CallCount(providertest.MethodOpen) - opens; more != 0 {
		t.Errorf("%d logins after the account stopped", more)
	}
	if n := h.messages(); n != 3 {
		t.Errorf("%d messages kept, want the 3 indexed", n)
	}
	if err := h.engine.Trigger(context.Background(), h.account); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("a trigger for a stopped account: %v", err)
	}

	// Consent at the provider again: the account is active and syncs.
	h.box.ClearFailures()
	h.exec(`UPDATE accounts SET state = 'active' WHERE id = ?`, h.account)
	h.engine.Reconcile(h.account)
	h.box.Deliver("INBOX", mail("after", time.Now()))
	h.waitFor("the account to sync again", 5*time.Second, func() bool { return h.messages() == 4 })
}

func TestTheEventJournalIsPrunedToItsRetention(t *testing.T) {
	// A week, and always the newest ten thousand; the events quote subjects
	// and senders, so what is pruned is scrubbed from the files too.
	h := newHarness(t, setup{noConsent: true})
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	h.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10005)
		INSERT INTO events(type, account_id, payload_json, created_at)
		SELECT 'message.new', 'acc_1', '{"subject":"Quetzalcoatl' || i || '"}', ? FROM n`, old)
	h.start()
	h.waitFor("the prune at start", 5*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM events`) == 10000
	})
	if n := h.count(`SELECT min(seq) FROM events`); n != 6 {
		t.Errorf("the oldest kept event is %d, want 6: the oldest go first", n)
	}
}

func TestSyncToggledOffAndOnTwiceNeverOpensAFourthConnection(t *testing.T) {
	// Consent withdrawn and given twice within a few seconds — through the
	// API, or an account's state flapping — while the first worker is still
	// logging out on a slow link. The second worker is stopped before it
	// dials; the third must still wait for the first, or the account holds
	// four connections of Gmail's fifteen.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.start()
	h.waitLive()
	h.waitFor("the idle connection", 5*time.Second, func() bool { return h.box.OpenSessions(provider.RoleIdle) == 1 })

	running := func(want bool) {
		t.Helper()
		h.waitFor(fmt.Sprintf("running=%t", want), 5*time.Second, func() bool {
			st, err := h.engine.Status(context.Background(), h.account)
			if err != nil {
				return !want
			}
			return st.Running == want
		})
	}
	consent := func(on bool) {
		t.Helper()
		at := 0
		if on {
			at = 1
		}
		h.exec(`UPDATE users SET sync_consent_at = ? WHERE id = ?`, at, h.user)
		h.engine.Reconcile(h.account)
		running(on)
	}

	h.box.DelayLogouts(300 * time.Millisecond)
	consent(false) // the first worker starts logging out, slowly
	consent(true)  // the second waits for it
	consent(false) // the second is stopped before it dialed
	consent(true)  // the third
	h.box.DelayLogouts(0)

	h.waitFor("the third worker's connections", 5*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleSync) == 1 && h.box.OpenSessions(provider.RoleIdle) == 1
	})
	if peak := h.box.PeakSessions(); peak > 3 {
		t.Errorf("%d connections open at once, want at most 3", peak)
	}
	for _, role := range []provider.Role{provider.RoleSync, provider.RoleIdle} {
		if peak := h.box.PeakSessions(role); peak > 1 {
			t.Errorf("%d %s connections at once, want one", peak, role)
		}
	}
}
