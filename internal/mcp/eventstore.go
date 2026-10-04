package mcp

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The event store is what lets a client whose connection dropped in the
// middle of a streamed answer pick it up again (Last-Event-ID). What it holds
// is those answers: search results, message bodies, attachments. So it is
// bounded twice, and holds nothing anywhere but in memory:
//
//   - in bytes, over every session at once, oldest first;
//   - in time: nothing is kept longer than its age limit, whether or not
//     anybody reads it, the session ends or the daemon has room.
//
// An answer larger than the per-event limit is delivered but not kept, so the
// answers that are kept cannot all be pushed out by one. Resuming past
// anything no longer kept fails, and the client asks again; nothing is ever
// half replayed.
//
// The bookkeeping goes with what it describes: a stream of which nothing is
// left is forgotten, so a session that stays busy for hours holds no more
// than what the last few minutes left.
const (
	// DefaultEventStoreBytes bounds the answers held for resumption.
	DefaultEventStoreBytes = 16 << 20
	// DefaultEventStoreAge bounds how long any one is held.
	DefaultEventStoreAge = 5 * time.Minute
	// maxEventFraction is the share of the byte bound one answer may take.
	maxEventFraction = 4
)

// EventStore is a bounded, in-memory sdk.EventStore.
type EventStore struct {
	maxBytes int
	maxAge   time.Duration
	now      func() time.Time
	// afterFunc schedules the expiry sweep; time.AfterFunc outside tests.
	afterFunc func(time.Duration, func()) *time.Timer

	mu       sync.Mutex
	nBytes   int
	sessions map[string]map[string]*eventStream
	// queue is every item, oldest first, across all streams: what the byte
	// bound and the age bound remove from. An answer too large to keep is
	// here too, with no bytes, so that its place in its stream ages out.
	queue []queued
	next  uint64
	timer *time.Timer
}

// eventStream is one SSE stream's items. first is the stream index of
// items[0]; everything before it is gone.
type eventStream struct {
	first int
	items []storedEvent
	// gapped is a stream forgotten and then written to again: the indexes of
	// its items are not known, so none can be replayed.
	gapped bool
}

type storedEvent struct {
	data []byte
	at   time.Time
	// dropped is an answer delivered but not kept: too large to hold.
	dropped bool
	seq     uint64
}

// queued points at one held item from the queue: its stream and its
// sequence number, which identify it even after the stream moved on.
type queued struct {
	session, stream string
	seq             uint64
	at              time.Time
}

var _ sdk.EventStore = (*EventStore)(nil)

// NewEventStore builds a store holding at most maxBytes, for at most maxAge;
// zero picks the default for either.
func NewEventStore(maxBytes int, maxAge time.Duration) *EventStore {
	if maxBytes <= 0 {
		maxBytes = DefaultEventStoreBytes
	}
	if maxAge <= 0 {
		maxAge = DefaultEventStoreAge
	}
	return &EventStore{
		maxBytes: maxBytes, maxAge: maxAge, now: time.Now, afterFunc: time.AfterFunc,
		sessions: map[string]map[string]*eventStream{},
	}
}

// Bounds reports the limits the store keeps, for the documentation of a
// running daemon to quote.
func (s *EventStore) Bounds() (maxBytes int, maxAge time.Duration) { return s.maxBytes, s.maxAge }

// Held reports how many bytes the store holds now.
func (s *EventStore) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nBytes
}

// Open implements sdk.EventStore. The transport opens every stream before
// it writes to it, at index 0.
func (s *EventStore) Open(_ context.Context, sessionID, streamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stream(sessionID, streamID)
	return nil
}

// stream returns a stream, creating it, and whether it was there. Requires
// s.mu.
func (s *EventStore) stream(sessionID, streamID string) (*eventStream, bool) {
	streams, ok := s.sessions[sessionID]
	if !ok {
		streams = map[string]*eventStream{}
		s.sessions[sessionID] = streams
	}
	st, ok := streams[streamID]
	if !ok {
		st = &eventStream{}
		streams[streamID] = st
	}
	return st, ok
}

// Append implements sdk.EventStore. It never fails: an item it cannot hold
// is recorded as dropped, so the stream's indexes stay aligned with the ids
// the transport handed out, and a resumption past it fails instead of
// skipping it.
func (s *EventStore) Append(_ context.Context, sessionID, streamID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expire(now)
	st, known := s.stream(sessionID, streamID)
	if !known {
		// Opened once, then forgotten when nothing of it was left, while
		// its request was still answering: the transport's index for this
		// item is past what the stream would count from.
		st.gapped = true
	}
	s.next++
	item := storedEvent{at: now, seq: s.next}
	if len(data) > s.maxBytes/maxEventFraction {
		item.dropped = true
	} else {
		item.data = slices.Clone(data)
		s.nBytes += len(data)
	}
	s.queue = append(s.queue, queued{session: sessionID, stream: streamID, seq: item.seq, at: now})
	st.items = append(st.items, item)
	for s.nBytes > s.maxBytes && len(s.queue) > 0 {
		s.removeOldest()
	}
	s.schedule()
	return nil
}

// After implements sdk.EventStore.
func (s *EventStore) After(_ context.Context, sessionID, streamID string, index int) iter.Seq2[[]byte, error] {
	data, err := s.after(sessionID, streamID, index)
	return func(yield func([]byte, error) bool) {
		if err != nil {
			yield(nil, err)
			return
		}
		for _, d := range data {
			if !yield(d, nil) {
				return
			}
		}
	}
}

func (s *EventStore) after(sessionID, streamID string, index int) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	st, ok := s.sessions[sessionID][streamID]
	switch {
	case !ok:
		return nil, fmt.Errorf("mcp: resuming an unknown stream: %w", sdk.ErrEventsPurged)
	case st.gapped:
		return nil, fmt.Errorf("mcp: resuming a stream that was forgotten: %w", sdk.ErrEventsPurged)
	}
	start := index + 1 - st.first
	if start < 0 {
		return nil, fmt.Errorf("mcp: resuming a stream past what is kept: %w", sdk.ErrEventsPurged)
	}
	if start >= len(st.items) {
		return nil, nil
	}
	out := make([][]byte, 0, len(st.items)-start)
	for _, item := range st.items[start:] {
		if item.dropped {
			return nil, fmt.Errorf("mcp: resuming a stream past an answer too large to keep: %w", sdk.ErrEventsPurged)
		}
		out = append(out, item.data)
	}
	return out, nil
}

// SessionClosed implements sdk.EventStore: everything the session's streams
// held goes now.
func (s *EventStore) SessionClosed(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.sessions[sessionID] {
		for _, item := range st.items {
			s.nBytes -= len(item.data)
		}
	}
	delete(s.sessions, sessionID)
	// The session's queue entries are skipped as they come up; dropping them
	// now keeps the queue from holding the ids of closed sessions.
	s.queue = slices.DeleteFunc(s.queue, func(q queued) bool { return q.session == sessionID })
	return nil
}

// expire removes everything older than the age bound. Requires s.mu.
func (s *EventStore) expire(now time.Time) {
	for len(s.queue) > 0 && now.Sub(s.queue[0].at) >= s.maxAge {
		s.removeOldest()
	}
}

// removeOldest removes the oldest item, and everything before it in its
// stream: a stream is only ever cut from the front, so what is left of it can
// still be resumed. A stream with nothing left is forgotten, except a
// session's standalone stream (""), which lives as long as the session and
// may be resumed by index at any time. Requires s.mu.
func (s *EventStore) removeOldest() {
	q := s.queue[0]
	s.queue[0] = queued{}
	s.queue = s.queue[1:]
	streams := s.sessions[q.session]
	st, ok := streams[q.stream]
	if !ok {
		return
	}
	for len(st.items) > 0 && st.items[0].seq <= q.seq {
		s.nBytes -= len(st.items[0].data)
		st.items[0] = storedEvent{}
		st.items = st.items[1:]
		st.first++
	}
	if len(st.items) > 0 || q.stream == "" {
		return
	}
	delete(streams, q.stream)
	if len(streams) == 0 {
		delete(s.sessions, q.session)
	}
}

// schedule arms the sweep that expires held items when nobody touches the
// store: the age bound holds for an idle session too. One timer at a time,
// set for the oldest item. Requires s.mu.
func (s *EventStore) schedule() {
	if s.timer != nil || len(s.queue) == 0 {
		return
	}
	wait := s.maxAge - s.now().Sub(s.queue[0].at)
	if wait < 0 {
		wait = 0
	}
	s.timer = s.afterFunc(wait, s.sweep)
}

func (s *EventStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timer = nil
	s.expire(s.now())
	s.schedule()
}
