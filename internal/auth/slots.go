package auth

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrHashBusy is a password or key check that could not start: every hashing
// slot was taken and the queue for them was full. It is not a failed check,
// and says nothing about the credential; the caller is told to try again.
var ErrHashBusy = errors.New("auth: every hashing slot is taken; try again shortly")

// slots bounds how many Argon2id derivations of one kind run at once, and how
// many callers may wait for one.
//
// The first bound is memory: each derivation holds its whole cost for as long
// as it runs, so the worst case is the slot count times the cost, whatever
// arrives. The second bound is time. Without it a queue grows with the attack
// that fills it: every request waits out its route's deadline holding a
// goroutine and a connection, and a person who arrives behind the queue waits
// as long as the attacker's last request. A full queue is refused at once
// instead, and the caller is told to come back.
type slots struct {
	held    chan struct{}
	waiting atomic.Int32
	queue   int32
}

func newSlots(n, queue int32) *slots {
	return &slots{held: make(chan struct{}, n), queue: queue}
}

// acquire takes a slot, waits for one if the queue has room, or refuses with
// ErrHashBusy. A caller that gives up while waiting — a request whose client
// went away — leaves the queue with its context's error.
func (s *slots) acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.held <- struct{}{}:
		return s.release, nil
	default:
	}
	if s.waiting.Add(1) > s.queue {
		s.waiting.Add(-1)
		return nil, ErrHashBusy
	}
	defer s.waiting.Add(-1)
	select {
	case s.held <- struct{}{}:
		return s.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slots) release() { <-s.held }
