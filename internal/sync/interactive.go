package sync

import (
	"context"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/service"
)

var _ account.ConnectionSlot = (*Manager)(nil).InPlaceOfInteractive

// slot is an account's third connection, the interactive one: what the
// service asks of the server on a person's behalf (today the live folder
// listing) runs on it.
//
// The engine owns it whether or not a worker runs for the account, so an
// account never has more than one, and never more than three connections in
// all. With a worker it is reused while callers keep coming and logged out
// after Options.InteractiveIdle unused. Without one — sync off, not consented,
// or not started yet — it is opened for one call and logged out right after,
// which is what the service did on its own before the engine existed.
type slot struct {
	m  *Manager
	id string
	// sem admits one caller at a time: a connection runs one command at a
	// time, and a second caller queues rather than opens a fourth.
	sem chan struct{}
	// users counts callers holding or waiting for the slot, under m.mu: a
	// slot is forgotten only when nobody is, so two callers never end up
	// with a slot each.
	users int

	// Held with sem.
	sess  provider.Session
	timer *time.Timer
}

// Interactive runs fn on the account's interactive connection, opening it if
// needed. It implements service.InteractiveRunner.
func (m *Manager) Interactive(ctx context.Context, accountID string, fn func(context.Context, provider.Session) error) error {
	s := m.takeSlot(accountID, true)
	if s == nil {
		return service.ErrSyncNotRunning
	}
	defer m.leaveSlot(s)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()
	return s.run(ctx, fn)
}

// InPlaceOfInteractive runs fn in the account's interactive slot with the
// connection logged out. It is for a caller that has to open a connection of
// its own for the account — proving a new grant logs in with the new
// credentials, which the engine's connection does not have — so that it takes
// the interactive connection's place in the account's budget instead of
// adding a fourth beside a running worker. Interactive callers queue
// meanwhile. It implements account.ConnectionSlot.
func (m *Manager) InPlaceOfInteractive(ctx context.Context, accountID string, fn func(context.Context) error) error {
	s := m.takeSlot(accountID, true)
	if s == nil {
		// The engine has shut down: nothing of its own is open.
		return fn(ctx)
	}
	defer m.leaveSlot(s)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()
	s.stopTimer()
	s.drop()
	return fn(ctx)
}

// takeSlot returns the account's slot, counted as in use, creating it when
// create is set. It returns nil once the engine has shut down, or when there
// is no slot and create is not set.
func (m *Manager) takeSlot(id string, create bool) *slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	switch {
	case s == nil && (!create || m.closed):
		return nil
	case s == nil:
		s = &slot{m: m, id: id, sem: make(chan struct{}, 1)}
		m.slots[id] = s
	case create && m.closed:
		return nil
	}
	s.users++
	return s
}

func (m *Manager) leaveSlot(s *slot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.users--
	if s.users > 0 {
		return
	}
	// Nobody holds or waits for it, so the session under it can be read
	// without its semaphore.
	if s.sess == nil && m.slots[s.id] == s {
		delete(m.slots, s.id)
	}
}

// closeInteractive logs the account's interactive connection out: its worker
// is stopping, or the provider said there are too many connections. A call
// in flight finishes first.
func (m *Manager) closeInteractive(id string) {
	s := m.takeSlot(id, false)
	if s == nil {
		return
	}
	defer m.leaveSlot(s)
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	s.stopTimer()
	s.drop()
}

func (m *Manager) hasWorker(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[id] != nil
}

// run is Interactive with the slot held.
func (s *slot) run(ctx context.Context, fn func(context.Context, provider.Session) error) error {
	s.stopTimer()
	if s.sess != nil {
		select {
		case <-s.sess.Closed():
			s.drop()
		default:
		}
	}
	if s.sess == nil {
		mb, err := s.m.mailboxes(ctx, s.id)
		if err != nil {
			return err
		}
		openCtx, cancel := context.WithTimeout(ctx, s.m.opts.OpenTimeout)
		sess, err := mb.Open(openCtx, provider.RoleInteractive)
		cancel()
		if err != nil {
			return err
		}
		s.sess = sess
	}
	finished := false
	defer func() {
		if !finished {
			// fn panicked: nobody knows what state it left the connection
			// in, and the next caller must not find out.
			s.drop()
		}
	}()
	err := fn(ctx, s.sess)
	finished = true
	select {
	case <-s.sess.Closed():
		s.drop()
	default:
		if s.m.hasWorker(s.id) {
			s.timer = time.AfterFunc(s.m.opts.InteractiveIdle, s.expire)
		} else {
			s.drop()
		}
	}
	return err
}

// expire logs out a connection nobody used for a while. A caller holding the
// slot now will arm a new timer when it is done.
func (s *slot) expire() {
	s.m.mu.Lock()
	s.users++
	s.m.mu.Unlock()
	defer s.m.leaveSlot(s)
	select {
	case s.sem <- struct{}{}:
	default:
		return
	}
	defer func() { <-s.sem }()
	s.timer = nil
	s.drop()
}

func (s *slot) stopTimer() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *slot) drop() {
	if s.sess == nil {
		return
	}
	//nolint:errcheck // logging out a connection we are done with
	_ = s.sess.Close()
	s.sess = nil
}
