package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The event stream and the long poll are one mechanism seen twice: a
// subscription to the bus, resumed from a journal cursor, holding only the
// events of mailboxes the caller may read, and of their own sends
// (send.finished goes to its sender alone). The rule is applied here, per
// event and with access as it stands when the event is decided, and not by
// the transport: an SSE handler that forgot it would hand one person's
// subjects to another.

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
// may read.
type EventFilter struct {
	// AccountIDs are accounts the caller may read; naming any other is
	// not_found, as it is everywhere else (forbidden, for one they see
	// without read).
	AccountIDs []string
	// Workspace narrows to the mailboxes of one workspace the caller is an
	// active member of.
	Workspace string
	// Types are event types; an unknown one is a bad request, not a filter
	// that silently matches nothing.
	Types []string
}

// Stream is a live subscription: the events it missed since its cursor, then
// every new one as it commits.
type Stream struct {
	sub  *events.Subscription
	gate *eventGate
	ch   chan Event
	stop chan struct{}
	once sync.Once
}

// AccessChange is a mailbox the stream's caller gained or lost read access
// to while it was open: the stream's events of it start, or stop.
type AccessChange struct {
	AccountID string `json:"account_id"`
	Read      bool   `json:"read"`
}

// CheckAccess reads again what the caller may read, when anything about
// access changed since it last looked, and reports the mailboxes that joined
// or left. It errs with not_found once the stream's filter can match nothing
// again — every mailbox it named with ?account=, or its key was restricted
// to, is gone, or the caller left the workspace it was narrowed to — so a
// client does not reconnect with the same filter for nothing. A stream of
// everything the caller may read stays open, however little that is: a
// mailbox granted or linked later appears on it. The transport calls it
// before each batch it writes and at each ping, as it re-checks the
// credential.
func (st *Stream) CheckAccess(ctx context.Context) ([]AccessChange, error) {
	return st.gate.check(ctx)
}

// Allowed decides an event again as it is about to be written: one decided
// when it was read from the journal may wait behind a slow client while the
// caller loses the mailbox, and is then held back. The transport asks it for
// each event it writes.
func (st *Stream) Allowed(ev Event) bool {
	return st.gate.allowEvent(ev.Type, ev.AccountID, ev.Payload)
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

func newStream(sub *events.Subscription, gate *eventGate) *Stream {
	st := &Stream{sub: sub, gate: gate, ch: make(chan Event), stop: make(chan struct{})}
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

// Subscribe opens a stream of the events the caller may read.
//
// since is the last sequence number the caller already has; zero starts from
// now. Access is decided per event rather than frozen into a list when the
// stream opens: a person who links a mailbox, or is granted one, while their
// stream is open sees its events — its consent finishing, above all —
// without reconnecting, and one who loses a mailbox stops seeing them the
// moment the revocation commits.
func (s *Service) Subscribe(ctx context.Context, p Principal, since int64, filter EventFilter) (*Stream, error) {
	f, gate, err := s.eventFilter(ctx, p, since, filter)
	if err != nil {
		return nil, err
	}
	sub, err := s.bus.Subscribe(ctx, since, f)
	if err != nil {
		return nil, E(CodeInternal, "opening the event stream failed", err)
	}
	return newStream(sub, gate), nil
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
// read, or until timeout. filter narrows to accounts or a workspace; its
// Types are new mail's, whatever it says.
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
func (s *Service) WaitForNewMail(ctx context.Context, p Principal, since int64, timeout time.Duration, filter EventFilter) (WaitResult, error) {
	switch {
	case timeout == 0:
		timeout = DefaultWait
	case timeout < time.Second || timeout > MaxWait:
		return WaitResult{}, Ef(CodeBadRequest, nil, "timeout must be between 1 and %d seconds", int(MaxWait/time.Second))
	}
	filter.Types = []string{string(events.TypeMessageNew)}
	f, gate, err := s.eventFilter(ctx, p, since, filter)
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
	// Decided again as the answer is written: mail of a mailbox the caller
	// lost during the wait is left out, however early it arrived.
	result.Events = slices.DeleteFunc(result.Events, func(ev Event) bool { return !gate.allowAccount(ev.AccountID) })
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

// MayFollow checks that the caller may be told when new mail arrives in an
// account — what an MCP resource subscription to its inbox does — which is
// reading it: the read scope, and read access to the mailbox. A mailbox the
// caller does not see is not_found; one they see without read is
// not_authorized, as for the rest of its index. Asked when a subscription
// starts and again before each notification, so one whose caller lost read
// is dropped rather than announced.
func (s *Service) MayFollow(ctx context.Context, p Principal, accountID string) error {
	_, err := s.authorizeAccount(ctx, p, auth.ScopeRead, accountID, needRead)
	return err
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
// that enforces who may read what.
func (s *Service) eventFilter(ctx context.Context, p Principal, since int64, req EventFilter) (events.Filter, *eventGate, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return events.Filter{}, nil, err
	}
	if since < 0 {
		return events.Filter{}, nil, E(CodeBadRequest, "the cursor must not be negative", nil)
	}
	if err := s.inWorkspace(ctx, p, req.Workspace); err != nil {
		return events.Filter{}, nil, err
	}
	var f events.Filter
	for _, id := range req.AccountIDs {
		a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, id, needRead)
		if err != nil {
			return events.Filter{}, nil, err
		}
		if req.Workspace != "" && a.WorkspaceID != req.Workspace {
			return events.Filter{}, nil, E(CodeNotFound, "no such account", nil)
		}
		f.AccountIDs = append(f.AccountIDs, id)
	}
	for _, name := range req.Types {
		t, err := events.ParseType(name)
		if err != nil {
			return events.Filter{}, nil, Ef(CodeBadRequest, err, "unknown event type %q", name)
		}
		f.Types = append(f.Types, t)
	}
	gate, err := s.newEventGate(ctx, p, req.Workspace, f.AccountIDs)
	if err != nil {
		return events.Filter{}, nil, err
	}
	f.Allow = gate.allow
	return f, gate, nil
}

// eventGate is the access rule for one subscription, applied per event.
//
// It runs on the subscription's own goroutine, after the journal batch is
// read and its cursor closed, so the reads below never hold up a publisher
// nor wait on a connection the replay is holding. It keeps the set of
// mailboxes the caller may read and the access epoch it read it at: when the
// epoch moved — a grant, a membership, a mailbox or a person changed — it
// reads the set again before deciding, so an event is decided against access
// as it stands. A mailbox not in the set costs one lookup, remembered until
// the epoch moves.
type eventGate struct {
	// ctx is the subscription's: its reads end with it.
	ctx context.Context
	s   *Service
	p   Principal
	// visible is the rule the set is read with: what the caller may read,
	// narrowed to the stream's workspace.
	visible account.Visibility
	// following are the mailboxes the stream was opened for; empty is every
	// one the caller may read.
	following []string

	mu sync.Mutex
	// epoch is the access epoch readable was read at; loaded says it was.
	epoch  int64
	loaded bool
	// readable is, at epoch, whether the caller may read each mailbox
	// asked about: the whole set when it was read, and the answers of
	// lookups since.
	readable map[string]bool
	// sendable is, at epoch, whether the caller may send from each mailbox
	// a send.finished was about: the answers of lookups.
	sendable map[string]bool
	// reported is the set the stream last told its client about, and ever
	// every mailbox it could read since it opened.
	reported map[string]bool
	ever     map[string]bool
	// outOfWorkspace is, at epoch, a stream narrowed to a workspace the
	// caller is no longer an active member of.
	outOfWorkspace bool
}

func (s *Service) newEventGate(ctx context.Context, p Principal, workspaceID string, following []string) (*eventGate, error) {
	v := readable(p)
	v.Workspace = workspaceID
	g := &eventGate{ctx: ctx, s: s, p: p, visible: v, following: following, ever: map[string]bool{}}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.refreshLocked(ctx); err != nil {
		return nil, E(CodeInternal, "listing accounts failed", err)
	}
	g.reported = g.currentLocked()
	for id := range g.reported {
		g.ever[id] = true
	}
	return g, nil
}

// refreshLocked reads the set again when the epoch moved. Requires g.mu.
func (g *eventGate) refreshLocked(ctx context.Context) error {
	epoch := g.s.accessEpoch.Load()
	if g.loaded && epoch == g.epoch {
		return nil
	}
	visible, err := g.s.accounts.Repo().ListVisible(ctx, g.visible)
	if err != nil {
		return err
	}
	g.readable = make(map[string]bool, len(visible))
	for _, a := range visible {
		g.readable[a.ID] = g.p.MayAccess(a.ID)
	}
	g.sendable = map[string]bool{}
	g.outOfWorkspace = false
	if g.visible.Workspace != "" {
		err := g.s.inWorkspace(ctx, g.p, g.visible.Workspace)
		switch {
		case CodeOf(err) == CodeNotFound:
			g.outOfWorkspace = true
		case err != nil:
			return err
		}
	}
	g.epoch, g.loaded = epoch, true
	return nil
}

// currentLocked is the set of mailboxes the caller may read now, within what
// the stream follows. Requires g.mu.
func (g *eventGate) currentLocked() map[string]bool {
	out := map[string]bool{}
	for id, ok := range g.readable {
		if ok && (len(g.following) == 0 || slices.Contains(g.following, id)) {
			out[id] = true
		}
	}
	return out
}

func (g *eventGate) allow(ev events.Event) bool {
	return g.allowEvent(string(ev.Type), ev.AccountID, ev.Payload)
}

// allowEvent decides whether the caller may have an event now.
//
// An event of a mailbox's index needs read on it. send.finished is not the
// mailbox's but one sender's record: several people may send from a shared
// mailbox, and its key and outcome are theirs. It goes to exactly whoever may
// read that record (SendStatus): the person who sent it, with the send scope
// and the send flag on the mailbox now, read or not; an instance key's send,
// to the instance keys with the send scope that reach the mailbox.
func (g *eventGate) allowEvent(typ, accountID string, payload json.RawMessage) bool {
	if typ == string(events.TypeSendFinished) {
		return g.allowSendFinished(accountID, payload)
	}
	return g.allowAccount(accountID)
}

// allowAccount decides whether the caller may read a mailbox's events now.
func (g *eventGate) allowAccount(accountID string) bool {
	return g.decide(accountID, false)
}

// allowSendFinished decides a send.finished: the caller's own send, from a
// mailbox they may send from now.
func (g *eventGate) allowSendFinished(accountID string, payload json.RawMessage) bool {
	if !g.p.Scope.Covers(auth.ScopeSend) {
		return false
	}
	var sent store.SendFinished
	if err := json.Unmarshal(payload, &sent); err != nil || sent.UserID != g.p.UserID {
		// Another person's send, or, for a person, an instance key's.
		return false
	}
	return g.decide(accountID, true)
}

// decide answers, from what the gate holds at the current epoch or one
// lookup, whether the caller may read a mailbox (send false) or send from it
// (send true) now.
func (g *eventGate) decide(accountID string, send bool) bool {
	if !g.p.MayAccess(accountID) {
		return false
	}
	ctx := g.ctx
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.refreshLocked(ctx); err != nil {
		// Withheld, because the safe answer to "may this person read
		// this" is no; the next event asks again.
		return false
	}
	known, rule := g.readable, g.visible
	if send {
		known = g.sendable
		rule.Need = workspace.Flags{Send: true}
	}
	if ok, found := known[accountID]; found {
		return ok
	}
	_, err := g.s.accounts.Repo().GetVisible(ctx, accountID, rule)
	switch {
	case err == nil:
		known[accountID] = true
		return true
	case errors.Is(err, account.ErrNotFound):
		known[accountID] = false
		return false
	default:
		// Not remembered: a failed lookup says nothing about the account.
		return false
	}
}

// errStreamGone ends a stream that has nothing left to follow: every mailbox
// it named, or its key was restricted to, is gone, or the workspace it was
// narrowed to.
var errStreamGone = E(CodeNotFound,
	"you can no longer read the mailboxes this stream follows; do not reconnect with the same filter", nil)

func (g *eventGate) check(ctx context.Context) ([]AccessChange, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.refreshLocked(ctx); err != nil {
		return nil, E(CodeInternal, "reading what the stream may follow failed", err)
	}
	current := g.currentLocked()
	var changes []AccessChange
	for id := range g.reported {
		if !current[id] {
			changes = append(changes, AccessChange{AccountID: id, Read: false})
		}
	}
	for id := range current {
		if !g.reported[id] {
			changes = append(changes, AccessChange{AccountID: id, Read: true})
		}
		g.ever[id] = true
	}
	slices.SortFunc(changes, func(a, b AccessChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	g.reported = current
	if g.goneLocked(current) {
		return changes, errStreamGone
	}
	return changes, nil
}

// goneLocked reports whether the stream has nothing left to follow, ever: a
// filter that can no longer match anything, which reconnecting with would
// only open the same empty stream. That is a stream narrowed to a workspace
// the caller left, or one that followed a fixed list of mailboxes — named by
// ?account=, or its key's restriction — once every one it could read is
// gone. A stream of everything the caller may read stays open however little
// that is now, as one opened with nothing does: a grant later, a link later,
// and the mailbox appears on it. Requires g.mu.
func (g *eventGate) goneLocked(current map[string]bool) bool {
	if g.outOfWorkspace {
		return true
	}
	fixed := len(g.following) > 0 || len(g.p.AccountIDs) > 0
	return fixed && len(current) == 0 && len(g.ever) > 0
}
