package events

import (
	"context"
	"fmt"
	"sync"
)

// Filter narrows a subscription. A zero Filter matches everything.
type Filter struct {
	// AccountIDs limits the stream to these accounts. Empty means all the
	// caller is entitled to; the caller applies its own authorisation before
	// building the filter.
	AccountIDs []string
	// Types limits the stream to these event types. Empty means all.
	Types []Type
	// Allow, when set, has the last word on every event the other fields
	// pass. It is how a subscriber's authorisation reaches the fan-out: who
	// may see an account is not a fixed list, because a person who connects
	// a mailbox while their stream is open must see its events without
	// reconnecting.
	//
	// It runs on the subscription's own goroutine, never on the publisher's,
	// and never while a journal cursor is open: it may look something up in
	// the database. It is called for one subscription at a time, but several
	// subscriptions may share a filter, so it must be safe for concurrent use.
	Allow func(Event) bool
}

// Match reports whether ev passes the filter.
func (f Filter) Match(ev Event) bool {
	if len(f.AccountIDs) > 0 {
		found := false
		for _, id := range f.AccountIDs {
			if id == ev.AccountID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Types) > 0 {
		found := false
		for _, t := range f.Types {
			if t == ev.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return f.Allow == nil || f.Allow(ev)
}

// queueDepth is how many events a subscription reads ahead of its consumer.
//
// Only a buffer, not a limit: a consumer that stops reading holds its
// subscription where it is, and the journal — not memory — keeps what it has
// not read yet. Nothing is dropped for being slow.
const queueDepth = 512

// Bus tells live subscribers that the journal has grown. It holds no events
// and carries none: every subscription reads what it delivers from the
// journal, in sequence order, from its own cursor. There is therefore one
// implementation of "what did I miss" — the same read serves a resume and a
// live stream — and no seam between a replay and a live fan-out where an
// event can be lost or overtaken.
//
// Why not hand the published events straight to subscribers: two
// transactions commit in sequence order, because the database has one
// writer, but the goroutines that committed them publish in whatever order
// the scheduler picks. A subscriber that trusted publication order would see
// seq 101 before seq 100, move its cursor to 101, and drop 100 for good. A
// journal read from the cursor cannot skip a committed row.
type Bus struct {
	journal *Journal

	mu     sync.Mutex
	nextID int64
	subs   map[int64]*Subscription
}

// NewBus builds a bus over a journal.
func NewBus(j *Journal) *Bus { return &Bus{journal: j, subs: map[int64]*Subscription{}} }

// Publish tells every subscriber that events were committed. It never blocks
// and never touches the database: each subscription reads the journal on its
// own goroutine. The events themselves are not delivered from here, so only
// whether there are any matters.
//
// Call it only after the transaction that journalled them has committed; a
// subscriber woken early simply finds nothing new, and is woken again by the
// next publish.
func (b *Bus) Publish(evs ...Event) {
	if len(evs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		s.notify()
	}
}

// Subscribe attaches a subscriber and starts delivering, from the journal,
// every event after since that the filter passes.
//
// since is the last sequence number the caller has already seen. Zero means
// "from now": no history is replayed, because the journal exists for resuming
// a stream, not for reading a mailbox. A since below what the journal still
// retains is honoured as far as possible and reported as a gap.
//
// The subscription is attached before its cursor is set and before its first
// read, so a commit at any moment is either below the cursor or wakes a read
// that finds it.
func (b *Bus) Subscribe(ctx context.Context, since int64, filter Filter) (*Subscription, error) {
	runCtx, cancel := context.WithCancel(ctx)
	s := &Subscription{
		bus:    b,
		filter: filter,
		ch:     make(chan Event, queueDepth),
		wake:   make(chan struct{}, 1),
		stop:   cancel,
		exited: make(chan struct{}),
	}
	b.mu.Lock()
	b.nextID++
	s.id = b.nextID
	b.subs[s.id] = s
	b.mu.Unlock()

	fail := func(err error) (*Subscription, error) {
		b.remove(s.id)
		cancel()
		return nil, err
	}
	if since <= 0 {
		latest, err := b.journal.Latest(ctx)
		if err != nil {
			return fail(err)
		}
		since = latest
	} else {
		earliest, err := b.journal.Earliest(ctx)
		if err != nil {
			return fail(err)
		}
		// The events between the caller's cursor and what survives retention
		// are gone. The stream cannot be made whole, so say so rather than
		// resume quietly from a gap the caller would read as "nothing
		// happened".
		if earliest > 0 && since+1 < earliest {
			s.gap = true
		}
	}
	s.last = since
	go s.run(runCtx)
	return s, nil
}

// Journal exposes the journal for callers that need a cursor without
// subscribing, such as a long-poll that starts "from now".
func (b *Bus) Journal() *Journal { return b.journal }

// Subscribers reports how many subscriptions are attached, for the health
// endpoint.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *Bus) remove(id int64) {
	b.mu.Lock()
	delete(b.subs, id)
	b.mu.Unlock()
}

// Subscription is one attached consumer, with a goroutine of its own that
// reads the journal whenever the bus says it grew.
type Subscription struct {
	bus    *Bus
	id     int64
	filter Filter
	ch     chan Event
	wake   chan struct{}
	stop   context.CancelFunc
	exited chan struct{}
	once   sync.Once

	mu   sync.Mutex
	last int64 // highest sequence examined: delivered, or filtered out
	// gap is a cursor older than what retention kept: events are missing
	// that no re-read can recover.
	gap bool
	// err is what ended the stream early, if anything did.
	err error
}

// Events yields events in sequence order, each exactly once. It is closed
// when the subscription is closed, or when reading the journal failed (Err).
func (s *Subscription) Events() <-chan Event { return s.ch }

// Gap reports whether the cursor the subscription resumed from was older than
// the journal's retention, so events after it are gone for good. What
// survived is still delivered; the subscriber has to re-read whatever state
// it keeps, because no cursor can bring the rest back.
func (s *Subscription) Gap() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gap
}

// Err is why Events closed before Close was called: the journal could not be
// read. Nil while the stream runs and after an ordinary Close.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Cursor is the highest sequence number examined for this subscriber: every
// event up to it was either queued on Events or not for this subscriber. It
// is exact once Close has returned; before that it may trail what Events has
// already handed out by the event in flight.
func (s *Subscription) Cursor() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Close detaches the subscription and waits for its goroutine to finish.
// Events already queued stay readable from Events until it is drained;
// nothing is queued after Close returns, and Cursor then covers exactly what
// was queued. It must not be called from inside the filter's Allow.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.bus.remove(s.id)
		s.stop()
	})
	<-s.exited
}

// notify wakes the subscription's goroutine without blocking. A wake-up that
// finds one already pending is dropped: the read it triggers covers both.
func (s *Subscription) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run reads the journal from the cursor now and every time the bus wakes it,
// until the subscription is closed or a read fails.
func (s *Subscription) run(ctx context.Context) {
	defer close(s.exited)
	defer close(s.ch)
	for {
		if err := s.catchUp(ctx); err != nil {
			if ctx.Err() == nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
	}
}

// catchUp delivers everything the journal holds after the cursor. It moves
// the cursor past each event only once the event is queued, and past rows the
// filter rejected only once everything before them is: a consumer that stops
// reading holds the cursor at the last event it was actually handed.
func (s *Subscription) catchUp(ctx context.Context) error {
	for {
		from := s.Cursor()
		evs, scanned, rows, err := s.bus.journal.since(ctx, from, s.filter, DefaultReplayLimit)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			select {
			case s.ch <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
			s.advanceTo(ev.Seq)
		}
		s.advanceTo(scanned)
		if rows < DefaultReplayLimit {
			return nil
		}
	}
}

// advanceTo moves the cursor forward, never back.
func (s *Subscription) advanceTo(seq int64) {
	s.mu.Lock()
	if seq > s.last {
		s.last = seq
	}
	s.mu.Unlock()
}

// String makes a subscription printable in logs without exposing its contents.
func (s *Subscription) String() string {
	return fmt.Sprintf("subscription(%d, cursor=%d, gap=%t)", s.id, s.Cursor(), s.Gap())
}
