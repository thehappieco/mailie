package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// The event stream and the long poll are one mechanism seen twice: a
// subscription to the bus, resumed from a journal cursor, holding only the
// events of accounts the caller may see. The ownership rule is applied here,
// per event, and not by the transport: an SSE handler that forgot it would
// hand one person's subjects to another.

// Event is one journal entry as a caller sees it. The same shape in the event
// stream's data lines and in a long poll's answer, so a client has one parser.
type Event struct {
	// Seq is the journal's sequence number: the SSE id, and the cursor to
	// resume from.
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	AccountID string `json:"account_id"`
	At        int64  `json:"at"`
	// Payload is the event's own JSON. It never carries a credential; it may
	// carry a subject or a sender, which are the caller's own mail.
	Payload json.RawMessage `json:"payload"`
}

// NewMail is mail that arrived, as a message.new event tells it: enough for a
// caller to say what came and to read it by ID.
type NewMail struct {
	// ID is the local message id every message route takes.
	ID         int64  `json:"id"`
	AccountID  string `json:"account_id"`
	FolderID   int64  `json:"folder_id"`
	FolderRole string `json:"folder_role,omitempty"`
	Subject    string `json:"subject"`
	// From is the first From address, or nil when the header had none.
	From         *Address `json:"from"`
	InternalDate int64    `json:"internal_date"`
}

// NewMail reads the message a message.new event announces. ok is false for
// any other event.
func (e Event) NewMail() (m NewMail, ok bool) {
	if e.Type != string(events.TypeMessageNew) {
		return NewMail{}, false
	}
	var payload store.MessageNew
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return NewMail{}, false
	}
	m = NewMail{
		ID: payload.MessageID, AccountID: e.AccountID, FolderID: payload.FolderID, FolderRole: payload.FolderRole,
		Subject: payload.Subject, InternalDate: payload.InternalDate,
	}
	if payload.From != nil {
		m.From = &Address{Name: payload.From.Name, Email: payload.From.Email}
	}
	return m, true
}

func presentEvent(ev events.Event) Event {
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return Event{Seq: ev.Seq, Type: string(ev.Type), AccountID: ev.AccountID, At: ev.At.Unix(), Payload: payload}
}

// EventFilter narrows a subscription. Empty fields mean everything the caller
// may see.
type EventFilter struct {
	// AccountIDs are accounts the caller may see; naming any other is
	// not_found, as it is everywhere else.
	AccountIDs []string
	// Types are event types; an unknown one is a bad request, not a filter
	// that silently matches nothing.
	Types []string
}

// Stream is a live subscription: the events it missed since its cursor, then
// every new one as it commits.
type Stream struct {
	sub  *events.Subscription
	ch   chan Event
	stop chan struct{}
	once sync.Once
}

// Events yields the stream's events in sequence order. It is closed when the
// stream is.
func (st *Stream) Events() <-chan Event { return st.ch }

// Gap reports that the cursor the stream resumed from was older than the
// journal keeps: events after it are gone for good, and the caller has to
// re-read whatever it keeps rather than trust the stream to be complete.
func (st *Stream) Gap() bool { return st.sub.Gap() }

// Err is why the stream ended by itself: the journal could not be read. The
// caller resumes from the last event it received and loses nothing.
func (st *Stream) Err() error { return st.sub.Err() }

// Close ends the stream.
func (st *Stream) Close() {
	st.once.Do(func() {
		close(st.stop)
		st.sub.Close()
	})
}

func newStream(sub *events.Subscription) *Stream {
	st := &Stream{sub: sub, ch: make(chan Event), stop: make(chan struct{})}
	go func() {
		defer close(st.ch)
		for ev := range sub.Events() {
			select {
			case st.ch <- presentEvent(ev):
			case <-st.stop:
				return
			}
		}
	}()
	return st
}

// Subscribe opens a stream of the events the caller may see.
//
// since is the last sequence number the caller already has; zero starts from
// now. Ownership is decided per event rather than frozen into a list when
// the stream opens: a person who connects a mailbox while their stream is
// open sees its events — its consent finishing, above all — without
// reconnecting.
func (s *Service) Subscribe(ctx context.Context, p Principal, since int64, filter EventFilter) (*Stream, error) {
	f, err := s.eventFilter(ctx, p, since, filter)
	if err != nil {
		return nil, err
	}
	sub, err := s.bus.Subscribe(ctx, since, f)
	if err != nil {
		return nil, E(CodeInternal, "opening the event stream failed", err)
	}
	return newStream(sub), nil
}

// DefaultWait is how long a long poll waits when the caller does not say.
const DefaultWait = 30 * time.Second

// MaxWait bounds a long poll. Under a minute, because the proxies a console
// sits behind close a response that has been silent for one.
const MaxWait = 55 * time.Second

// WaitResult is what a long poll returns: always an answer, never an error
// for having waited in vain.
type WaitResult struct {
	// TimedOut is a wait that ended with nothing new.
	TimedOut bool `json:"timed_out"`
	// Lagged is a cursor older than the journal keeps: mail may have arrived
	// that no cursor can bring back, so the caller should list what it cares
	// about instead of trusting the answer to be complete.
	Lagged bool    `json:"lagged"`
	Events []Event `json:"events"`
	// NextCursor is the since for the next call.
	NextCursor int64 `json:"next_cursor"`
}

// WaitForNewMail waits until new mail arrives in an account the caller may
// see, or until timeout.
//
// New mail is message.new in an inbox or in a folder with no role — mail a
// filter filed straight under a label. The sync engine never emits
// message.new elsewhere, since a copy landing in Sent, Drafts, Trash or Spam
// is not mail arriving, and the check here keeps it that way for this caller
// whatever the engine does.
//
// since is the next_cursor of the previous call; zero waits from now. The
// answer comes as soon as there is something to say, with everything that
// arrived by then.
func (s *Service) WaitForNewMail(ctx context.Context, p Principal, since int64, timeout time.Duration, accountIDs []string) (WaitResult, error) {
	switch {
	case timeout == 0:
		timeout = DefaultWait
	case timeout < time.Second || timeout > MaxWait:
		return WaitResult{}, Ef(CodeBadRequest, nil, "timeout must be between 1 and %d seconds", int(MaxWait/time.Second))
	}
	f, err := s.eventFilter(ctx, p, since, EventFilter{
		AccountIDs: accountIDs, Types: []string{string(events.TypeMessageNew)},
	})
	if err != nil {
		return WaitResult{}, err
	}
	visible := f.Allow
	f.Allow = func(ev events.Event) bool { return arrived(ev) && visible(ev) }

	sub, err := s.bus.Subscribe(ctx, since, f)
	if err != nil {
		return WaitResult{}, E(CodeInternal, "waiting for new mail failed", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var first *events.Event
	select {
	case ev, ok := <-sub.Events():
		if ok {
			first = &ev
		}
	case <-timer.C:
	case <-ctx.Done():
		// The caller left, or its deadline came: answer as a timeout, which
		// is true, and costs nothing if nobody reads it.
	}

	// Closed before the rest is read, so nothing is queued after the cursor
	// is taken: every event up to it is either below, or was not for this
	// caller.
	sub.Close()
	result := WaitResult{Lagged: sub.Gap(), Events: []Event{}}
	if first != nil {
		result.Events = append(result.Events, presentEvent(*first))
	}
	for ev := range sub.Events() {
		result.Events = append(result.Events, presentEvent(ev))
	}
	if err := sub.Err(); err != nil && len(result.Events) == 0 {
		// The journal could not be read. Answering "nothing arrived" would
		// be untrue and would bring the caller straight back.
		return WaitResult{}, E(CodeInternal, "waiting for new mail failed", err)
	}
	result.TimedOut = len(result.Events) == 0
	result.NextCursor = sub.Cursor()
	if result.NextCursor < since {
		result.NextCursor = since
	}
	return result, nil
}

// arrived is the new-mail rule: message.new in an inbox or in a folder with no
// role.
func arrived(ev events.Event) bool {
	var payload struct {
		FolderRole string `json:"folder_role"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false
	}
	return payload.FolderRole == string(provider.RoleInbox) || payload.FolderRole == string(provider.RoleNone)
}

// eventFilter checks what a subscription asks for and builds the bus filter
// that enforces who may see what.
func (s *Service) eventFilter(ctx context.Context, p Principal, since int64, req EventFilter) (events.Filter, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return events.Filter{}, err
	}
	if since < 0 {
		return events.Filter{}, E(CodeBadRequest, "the cursor must not be negative", nil)
	}
	var f events.Filter
	for _, id := range req.AccountIDs {
		if _, err := s.authorizeAccount(ctx, p, auth.ScopeRead, id); err != nil {
			return events.Filter{}, err
		}
		f.AccountIDs = append(f.AccountIDs, id)
	}
	for _, name := range req.Types {
		t, err := events.ParseType(name)
		if err != nil {
			return events.Filter{}, Ef(CodeBadRequest, err, "unknown event type %q", name)
		}
		f.Types = append(f.Types, t)
	}
	gate, err := s.newEventGate(ctx, p)
	if err != nil {
		return events.Filter{}, err
	}
	f.Allow = gate.allow
	return f, nil
}

// eventGate is the ownership rule for one subscription, applied per event.
//
// It runs on the subscription's own goroutine, after the journal batch is
// read and its cursor closed, so the lookup below never holds up a publisher
// nor waits on a connection the replay is holding. Each account's answer is
// remembered: an account never changes owner and an id is never reused, so
// the answer cannot go stale. The accounts the caller could see when the
// stream opened are known up front; one connected later costs a single
// lookup, the first time one of its events goes by.
type eventGate struct {
	ctx    context.Context
	p      Principal
	all    bool
	lookup func(context.Context, string, account.Visibility) (account.Account, error)

	mu    sync.Mutex
	known map[string]bool
}

func (s *Service) newEventGate(ctx context.Context, p Principal) (*eventGate, error) {
	v := visibility(p)
	g := &eventGate{ctx: ctx, p: p, all: v.All, lookup: s.accounts.Repo().GetVisible, known: map[string]bool{}}
	if g.all {
		return g, nil
	}
	visibleNow, err := s.accounts.Repo().ListVisible(ctx, v)
	if err != nil {
		return nil, E(CodeInternal, "listing accounts failed", err)
	}
	for _, a := range visibleNow {
		g.known[a.ID] = true
	}
	return g, nil
}

func (g *eventGate) allow(ev events.Event) bool {
	if !g.p.MayAccess(ev.AccountID) {
		return false
	}
	if g.all {
		return true
	}
	g.mu.Lock()
	seen, ok := g.known[ev.AccountID]
	g.mu.Unlock()
	if ok {
		return seen
	}
	_, err := g.lookup(g.ctx, ev.AccountID, visibility(g.p))
	switch {
	case err == nil:
		seen = true
	case errors.Is(err, account.ErrNotFound):
		seen = false
	default:
		// Not remembered: a failed lookup says nothing about the account,
		// and the next event asks again. Withheld meanwhile, because the
		// safe answer to "may this person see this" is no.
		return false
	}
	g.mu.Lock()
	g.known[ev.AccountID] = seen
	g.mu.Unlock()
	return seen
}
