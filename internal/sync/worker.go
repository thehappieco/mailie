package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// worker syncs one account for as long as it may.
//
// Its sync loop owns the "sync" connection and everything about the account's
// progress: it connects, lists folders, runs passes, and on failure backs off
// and connects again. The idle loop is a child of each sync connection: it
// parks a second connection in IDLE on the inbox and turns what it hears into
// signals for the sync loop, and it goes when the sync connection goes, so one
// backoff governs the account and a refused login is never retried twice as
// often.
type worker struct {
	m    *Manager
	id   string
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
	// prev is the previous worker's done, which must be closed before this
	// one dials. done is closed only after prev is, so each worker's done
	// covers every earlier worker for the account.
	prev <-chan struct{}

	signals chan struct{}

	mu           sync.Mutex
	inboxDirty   bool
	inboxExpunge bool
	triggerAll   bool
	rediscover   bool
	nudged       bool
	backoff      bool
	tier         string

	idleUp atomic.Bool

	// Retry bookkeeping, owned by the sync loop.
	failures    int
	lastFailure time.Time
	lastOKWrite time.Time
	recordedBad bool
}

func newWorker(parent context.Context, m *Manager, id string, prev <-chan struct{}) *worker {
	ctx, cancel := context.WithCancel(parent)
	w := &worker{
		m: m, id: id, ctx: ctx, stop: cancel, done: make(chan struct{}), prev: prev,
		signals: make(chan struct{}, 1),
	}
	return w
}

func (w *worker) cancel() { w.stop() }

// poke wakes the sync loop without blocking.
func (w *worker) poke() {
	select {
	case w.signals <- struct{}{}:
	default:
	}
}

// idleSignal is the idle loop saying the inbox changed.
func (w *worker) idleSignal(expunge bool) {
	w.mu.Lock()
	w.inboxDirty = true
	if expunge {
		w.inboxExpunge = true
	}
	w.mu.Unlock()
	w.poke()
}

func (w *worker) trigger() {
	w.mu.Lock()
	w.triggerAll = true
	w.rediscover = true
	w.mu.Unlock()
	w.poke()
}

func (w *worker) nudge() {
	w.mu.Lock()
	w.nudged = true
	w.rediscover = true
	w.mu.Unlock()
	w.poke()
}

// signalsSeen is what the sync loop has been asked to do since it last
// looked.
type signalsSeen struct {
	inbox, inboxExpunge, all, rediscover bool
}

func (w *worker) takeSignals() signalsSeen {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := signalsSeen{inbox: w.inboxDirty, inboxExpunge: w.inboxExpunge, all: w.triggerAll, rediscover: w.rediscover}
	w.inboxDirty, w.inboxExpunge, w.triggerAll, w.rediscover = false, false, false, false
	return s
}

func (w *worker) takeNudge() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.nudged
	w.nudged = false
	return n
}

func (w *worker) setBackoff(on bool) {
	w.mu.Lock()
	w.backoff = on
	w.mu.Unlock()
}

func (w *worker) backingOff() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.backoff
}

func (w *worker) setTier(tier string) {
	w.mu.Lock()
	w.tier = tier
	w.mu.Unlock()
}

func (w *worker) currentTier() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tier
}

// run is the sync loop: connect, sync until the connection fails, back off,
// again — until the worker is stopped.
func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	// The interactive connection counts against this worker's budget while
	// it runs; the next caller without a worker gets one for its call only.
	defer w.m.closeInteractive(w.id)
	if w.prev != nil {
		select {
		case <-w.prev:
		case <-ctx.Done():
			// Stopped before it dialed. Its done still waits for the worker
			// before it, which has been stopped too and logs out within its
			// own timeouts: the next worker waits only on this one's done,
			// and must not dial while any earlier one still holds a
			// connection. Sync toggled off and on twice in a row would
			// otherwise open a fourth.
			<-w.prev
			return
		}
	}
	log := w.m.log.With("account", w.id)
	for ctx.Err() == nil {
		err := w.connected(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, store.ErrNotEligible) {
			// Consent withdrawn, the account disabled or removed while a pass
			// was writing: nothing was stored, and the manager stops this
			// worker as soon as it looks. Until then, wait.
			w.m.Reconcile(w.id)
			w.wait(ctx, w.m.opts.EligibilityInterval)
			continue
		}
		delay := w.retryDelay(err)
		class := provider.Class(err)
		now := w.m.opts.Now()
		level, msg := failureLog(err, w.failures)
		log.Log(ctx, level, msg, "class", class, "retry_in", delay.Round(time.Second),
			"attempt", w.failures, "err", err, "server", provider.ServerReply(err))
		if _, rerr := w.m.store.RecordSyncFailure(ctx, w.id, class, now.Add(delay)); rerr != nil && ctx.Err() == nil {
			log.Warn("recording a sync failure failed", "err", rerr)
		}
		w.recordedBad = true
		w.setBackoff(true)
		w.wait(ctx, delay)
		w.setBackoff(false)
	}
}

// failureLog is how a failed sync connection is logged, by what failed and
// its place in a run of failures (1 for the first since the backoff reset).
// The server ending the connection (provider.ErrServerEnded) is routine —
// Gmail ends an OAuth session after about the token's lifetime, and some
// hosts close every connection after a few hours — so the first one is
// INFO. One that happens again right after reconnecting is a WARN, and so is
// every other failure from the first, a connection closed any other way
// included: a timeout, a dial refused, or a response go-imap could not
// parse, which ends the connection each time it comes and must not pass for
// routine.
func failureLog(err error, attempt int) (slog.Level, string) {
	if errors.Is(err, provider.ErrServerEnded) && attempt <= 1 {
		return slog.LevelInfo, "the server closed the connection; reconnecting"
	}
	return slog.LevelWarn, "account sync failed; retrying later"
}

// wait sleeps for d, or until the worker stops, or until the manager nudges
// it because the account changed. Signals from triggers do not end it.
func (w *worker) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case <-w.signals:
			if w.takeNudge() {
				return
			}
		}
	}
}

// retryDelay is how long to wait after a failed connection, by what failed.
func (w *worker) retryDelay(err error) time.Duration {
	o := w.m.opts
	now := o.Now()
	if now.Sub(w.lastFailure) > o.BackoffReset {
		w.failures = 0
	}
	w.lastFailure = now
	n := w.failures
	w.failures++

	switch {
	case errors.Is(err, provider.ErrNeedsReauth), errors.Is(err, provider.ErrAuthFailed),
		errors.Is(err, provider.ErrNotConnected):
		// A person has to act, or the server is refusing the mailbox for
		// reasons of its own: retrying soon only adds refused logins. A
		// grant found dead moves the account to needs_reauth, and the
		// manager stops this worker before the wait is over.
		return o.LongBackoff
	case errors.Is(err, provider.ErrTooManyConnections):
		w.m.closeInteractive(w.id)
		return o.TooManyBackoff
	case errors.Is(err, provider.ErrRateLimited):
		return max(o.RateLimitMin, exponential(o.BackoffBase, o.BackoffMax, n))
	case n >= stubbornFailures:
		// Failing for a while now: keep trying, but only as often as a
		// refusal a person has to fix. The account stays active — its state
		// is what makes it eligible — and says why in last_error.
		return o.LongBackoff
	}
	return exponential(o.BackoffBase, o.BackoffMax, n)
}

// stubbornFailures is how many failures in a row, none of them ten minutes
// apart, move the retries to the long interval.
const stubbornFailures = 10

// exponential is base·2ⁿ capped at limit, with ±20 % jitter so a server
// restart does not bring every account back in the same second.
func exponential(base, limit time.Duration, n int) time.Duration {
	d := base
	for i := 0; i < n && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	jitter := time.Duration(float64(d) * 0.2 * (2*rand.Float64() - 1)) //nolint:gosec // G404: jitter, not a secret
	return max(d+jitter, time.Millisecond)
}

// connected runs one sync connection from dial to failure. A panic is caught
// and becomes an error: one account's bug must not take the daemon down,
// and a reconnection is the right answer to not knowing what state a
// connection was left in.
func (w *worker) connected(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			w.m.log.Error("account sync panicked; reconnecting", "account", w.id, "panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("sync: panic: %v", r)
		}
	}()
	// A nudge asks for a fresh start, and this is one.
	w.takeNudge()
	acct, err := w.m.accounts.Get(ctx, w.id)
	if err != nil {
		return err
	}
	mb, err := w.m.mailboxes(ctx, w.id)
	if err != nil {
		return err
	}
	openCtx, cancel := context.WithTimeout(ctx, w.m.opts.OpenTimeout)
	sess, err := mb.Open(openCtx, provider.RoleSync)
	cancel()
	if err != nil {
		return err
	}
	//nolint:errcheck // closing a connection we are done with; nothing to act on
	defer func() { _ = sess.Close() }()

	caps := sess.Caps()
	tier := chooseTier(acct.SyncTier, caps)
	w.setTier(tier)
	if tier != acct.SyncTierResolved {
		if err := w.m.accounts.SetResolvedTier(ctx, w.id, tier); err != nil {
			return err
		}
	}

	p := newPass(w, sess, acct, mb, tier)
	if err := p.discover(ctx); err != nil {
		return err
	}
	// Logged in and listed: whatever failed before is over, and the account
	// row should stop saying so now rather than after the first round.
	w.recordOK(ctx)

	// The idle connection lives inside this one: when the sync connection
	// fails, the idle one is logged out before the backoff starts.
	idleCtx, stopIdle := context.WithCancel(ctx)
	idleDone := make(chan struct{})
	defer func() {
		stopIdle()
		<-idleDone
	}()
	if caps.Idle {
		go func() {
			defer close(idleDone)
			w.idleLoop(idleCtx, mb)
		}()
	} else {
		close(idleDone)
	}
	return p.loop(ctx)
}

// recordOK notes on the account that sync is working, at most every
// Options.OKEvery unless a failure was recorded since the last time.
func (w *worker) recordOK(ctx context.Context) {
	now := w.m.opts.Now()
	if !w.recordedBad && now.Sub(w.lastOKWrite) < w.m.opts.OKEvery {
		return
	}
	if err := w.m.store.RecordSyncOK(ctx, w.id, now); err != nil {
		if ctx.Err() == nil {
			w.m.log.Warn("recording a good sync pass failed", "account", w.id, "err", err)
		}
		return
	}
	w.lastOKWrite = now
	w.recordedBad = false
}

// Tiers: how flag changes are found.
const (
	// TierCondStore asks the server for what changed since a modification
	// sequence (CONDSTORE's CHANGEDSINCE). Gmail, Dovecot.
	TierCondStore = "condstore"
	// TierUIDPoll reads flags in windows of UIDs. Exchange, and any server
	// without CONDSTORE.
	TierUIDPoll = "uidpoll"
)

// chooseTier picks the tier from what the server says it can do, never from
// which provider it is. A configured condstore on a server without it falls
// back to polling; there is nothing else it could do.
func chooseTier(configured string, caps provider.Caps) string {
	if configured == TierUIDPoll || !caps.CondStore {
		return TierUIDPoll
	}
	return TierCondStore
}
