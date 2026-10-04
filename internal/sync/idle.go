package sync

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
)

// idleLoop keeps a connection parked in IDLE on the inbox and turns what it
// hears into signals for the sync loop. It never reads mail and never writes
// the index: an EXISTS, EXPUNGE or FETCH only means "look again", and the
// looking is done by the sync loop with UID commands.
//
// A dropped connection — Gmail ends OAuth sessions about hourly — is dialled
// again after a short backoff, and each (re)connection asks for a full pass
// of the inbox, so nothing that arrived while it was down is missed. A
// refusal that retrying will not fix (credentials, the provider's connection
// cap, IDLE not offered) ends the loop instead; the sync loop then polls the
// inbox on its shorter interval until its own next connection tries again.
func (w *worker) idleLoop(ctx context.Context, mb provider.Mailbox) {
	defer func() {
		w.idleUp.Store(false)
		// The inbox's interval depends on whether IDLE is up.
		w.idleSignal(false)
	}()
	o := w.m.opts
	failures := 0
	var lastFailure time.Time
	for ctx.Err() == nil {
		up, err := w.idleConnection(ctx, mb)
		w.idleUp.Store(false)
		if ctx.Err() != nil {
			return
		}
		if permanentForIdle(err) {
			w.m.log.Info("the inbox will be polled instead of watched", "account", w.id,
				"class", provider.Class(err), "err", err)
			return
		}
		now := o.Now()
		if up || now.Sub(lastFailure) > o.BackoffReset {
			failures = 0
		}
		lastFailure = now
		delay := exponential(o.BackoffBase, o.BackoffMax, failures)
		failures++
		w.m.log.Debug("the idle connection dropped; reconnecting", "account", w.id, "class", provider.Class(err),
			"retry_in", delay.Round(time.Millisecond))
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// permanentForIdle is a failure the idle loop should not retry on its own.
func permanentForIdle(err error) bool {
	for _, target := range []error{
		provider.ErrUnsupported, provider.ErrAuthFailed, provider.ErrNeedsReauth, provider.ErrNotConnected,
		provider.ErrTooManyConnections,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// idleConnection is one idle connection from dial to failure. It reports
// whether it got as far as idling, which resets the backoff.
func (w *worker) idleConnection(ctx context.Context, mb provider.Mailbox) (up bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			w.m.log.Error("the idle loop panicked; reconnecting", "account", w.id, "panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("sync: idle panic: %v", r)
		}
	}()
	o := w.m.opts
	openCtx, cancel := context.WithTimeout(ctx, o.OpenTimeout)
	sess, err := mb.Open(openCtx, provider.RoleIdle)
	cancel()
	if err != nil {
		return false, err
	}
	//nolint:errcheck // logging out a connection we are done with
	defer func() { _ = sess.Close() }()
	if !sess.Caps().Idle {
		return false, fmt.Errorf("%w: the server does not offer IDLE", provider.ErrUnsupported)
	}
	if _, err := sess.Select(ctx, "INBOX", true, 0); err != nil {
		return false, err
	}
	renew := o.IdleRenew
	if renew <= 0 {
		renew = mb.Profile().IdleRenew
	}
	if renew <= 0 {
		renew = 7 * time.Minute
	}

	w.idleUp.Store(true)
	// Whatever arrived while no connection was watching is found by a full
	// pass of the inbox now.
	w.idleSignal(true)
	for {
		handle, err := sess.Idle(ctx)
		if err != nil {
			return true, err
		}
		err = w.idleWait(ctx, sess, renew)
		if stopErr := w.stopIdle(handle, sess); err == nil {
			err = stopErr
		}
		if err != nil {
			return true, err
		}
		// The renewal: DONE, then NOOP. Exchange has delivered EXISTS only
		// after DONE, and the NOOP is also what finds a socket that died
		// quietly, through its timeout.
		if err := sess.Noop(ctx); err != nil {
			return true, err
		}
	}
}

// idleWait listens while the connection idles, until the renewal is due
// (nil), the connection drops, or ctx ends. Notifications are gathered for
// Options.IdleDebounce and passed on as one signal.
func (w *worker) idleWait(ctx context.Context, sess provider.Session, renew time.Duration) error {
	renewal := time.NewTimer(renew)
	defer renewal.Stop()
	var (
		debounce         <-chan time.Time
		pending, expunge bool
		heardAt          time.Time
	)
	flush := func() {
		if pending {
			w.idleSignal(expunge)
		}
		pending, expunge, debounce = false, false, nil
	}
	events := sess.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sess.Closed():
			flush()
			return fmt.Errorf("%w: the idle connection closed", provider.ErrConnClosed)
		case <-renewal.C:
			flush()
			return nil
		case <-debounce:
			flush()
		case ev, ok := <-events:
			if !ok {
				flush()
				return fmt.Errorf("%w: the idle connection closed", provider.ErrConnClosed)
			}
			pending = true
			if ev.Kind == provider.IdleExpunge || sess.Overflowed() {
				// An overflow dropped signals of unknown kinds: assume the
				// worst, which is only a diff.
				expunge = true
			}
			if debounce == nil {
				debounce = time.After(w.m.opts.IdleDebounce)
			}
			if now := w.m.opts.Now(); now.Sub(heardAt) >= time.Minute {
				heardAt = now
				if err := w.m.store.RecordIdleEvent(ctx, w.id, now); err != nil && ctx.Err() == nil {
					w.m.log.Debug("recording an idle notification failed", "account", w.id, "err", err)
				}
			}
		}
	}
}

// stopIdle ends an IDLE, closing the connection if the server does not
// answer DONE in time: a stuck connection must not hold the loop, or a
// shutdown, forever.
func (w *worker) stopIdle(handle provider.IdleHandle, sess provider.Session) error {
	done := make(chan error, 1)
	go func() { done <- handle.Stop() }()
	t := time.NewTimer(w.m.opts.IdleStopTimeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		//nolint:errcheck // the connection is being abandoned; Stop's own error follows
		_ = sess.Close()
		<-done
		return fmt.Errorf("%w: the server did not end IDLE", provider.ErrConnClosed)
	}
}
