package providertest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/thehappieco/mailie/internal/mime"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// FakeMailbox is a state-based stand-in for one account's mail server: the
// seam the sync engine is tested against.
//
// It holds what a server holds — folders, each with a UIDVALIDITY, a UIDNEXT,
// a HIGHESTMODSEQ and rows by UID — and answers provider.Session calls from
// that state with the semantics that matter to a sync engine and that are
// easy to get wrong against a fake that merely replays answers:
//
//   - UIDs are assigned from UIDNEXT and never reused; a UIDVALIDITY change
//     renumbers everything.
//   - "n:*" with n above every UID still matches the highest UID, as on a real
//     server: "*" is the largest UID in the mailbox, and ranges are unordered.
//   - SINCE compares dates, not instants, each in its own zone: a message's
//     INTERNALDATE in the zone it was received in.
//   - CHANGEDSINCE and MODSEQ exist only when the Caps say CONDSTORE; ESEARCH
//     counting only with ESEARCH; UIDs in MOVE mappings only with UIDPLUS, and
//     a move with neither MOVE nor UIDPLUS is refused before anything changes.
//   - A write (STORE, MOVE) on a folder opened read-only selects it again,
//     read-write, as the adapter does, and that SELECT is in the call log.
//   - A change made "by another client" (Deliver, Expunge, SetFlags, ...)
//     pushes EXISTS, EXPUNGE or FETCH signals to every session that has the
//     folder selected, through a bounded channel that overflows the way the
//     real adapter's does.
//
// Tests drive it from the server side with Deliver, CopyTo, Expunge and the
// rest, inject failures per method, kill connections, and read back what the
// engine asked (Calls), how many connections it held (PeakSessions), and
// whether it ever broke the one-command-per-session rule (Violations).
//
// It is safe for concurrent use: the engine's sessions and the test's
// server-side changes all go through one lock.
type FakeMailbox struct {
	kind    provider.Kind
	profile provider.Profile
	clock   func() time.Time

	mu              sync.Mutex
	caps            provider.Caps
	folders         map[string]*fakeFolder
	order           []string
	nextUIDValidity uint32
	nextKey         int64
	sharedFlags     bool
	labels          bool
	reversedCopyUID bool
	maxSessions     int
	sessions        map[*fakeSession]struct{}
	peak            int
	peakByRole      map[provider.Role]int
	opens           map[provider.Role]int
	failures        map[Method][]*failure
	hook            func(ctx context.Context, c Call) error
	calls           []Call
	violations      []string
	sender          *FakeSender
	// deaf: inside Unheard, notifications are lost.
	deaf bool
	// logouts counts sessions closed by their owner, by role; closedIdling
	// counts those closed while still in IDLE, without ending it first.
	logouts      map[provider.Role]int
	closedIdling int
	// logoutDelay is how long a LOGOUT takes, the session still open
	// meanwhile: a slow link, or a server slow to say BYE.
	logoutDelay time.Duration
}

// Method names a provider call, for failure injection and the call log.
type Method string

// The methods a FakeMailbox answers.
const (
	MethodOpen           Method = "Open"
	MethodListFolders    Method = "ListFolders"
	MethodSelect         Method = "Select"
	MethodStatus         Method = "Status"
	MethodCreate         Method = "Create"
	MethodUIDs           Method = "UIDs"
	MethodUIDCount       Method = "UIDCount"
	MethodFetchSummaries Method = "FetchSummaries"
	MethodFetchFlags     Method = "FetchFlags"
	MethodFetchHeader    Method = "FetchHeader"
	MethodFetchPart      Method = "FetchPart"
	MethodFetchRaw       Method = "FetchRaw"
	MethodStoreFlags     Method = "StoreFlags"
	MethodMove           Method = "Move"
	MethodCopy           Method = "Copy"
	MethodSearch         Method = "SearchMessageID"
	MethodAppend         Method = "Append"
	MethodIdle           Method = "Idle"
	MethodNoop           Method = "Noop"
	MethodSend           Method = "Send"
)

// Call is one call the fake answered (or refused), for assertions.
type Call struct {
	Method Method
	Role   provider.Role
	// Folder is the folder the call named, or the selected one.
	Folder string
	// Set is the UID set, as IMAP writes it.
	Set          string
	ChangedSince uint64
	Since        time.Time
	// Section is the part path FetchPart asked for, as IMAP writes it.
	Section string
	// ReadOnly is a Select that asked for EXAMINE.
	ReadOnly bool
	// Dest is where a Move or a Copy went.
	Dest string
	// Flags is what a StoreFlags changed, as IMAP writes it: "+FLAGS
	// (\seen)", "-FLAGS (\flagged)" or "FLAGS (...)".
	Flags string
	// Header is the Message-ID a SearchMessageID looked for.
	Header string
}

type failure struct {
	err   error
	times int // < 0: always
}

type fakeFolder struct {
	name          string
	delim         rune
	attrs         []imap.MailboxAttr
	selectable    bool
	uidValidity   uint32
	uidNext       imap.UID
	highestModSeq uint64
	rows          map[imap.UID]*fakeRow
}

type fakeRow struct {
	uid    imap.UID
	key    int64 // the message's identity across folders, as Gmail's X-GM-MSGID
	flags  []imap.Flag
	modseq uint64
	msg    FakeMessage
}

// FakeMessage is a message as a test describes it. Zero fields get plausible
// values: dates from the clock, a size, and one text/plain body part.
type FakeMessage struct {
	MessageID string // bare or bracketed; "" for none
	Subject   string
	From      string // "Name <a@example.com>" or "a@example.com"
	To        []string
	Cc        []string
	InReplyTo string
	// References are the ids of the References header, oldest first.
	References   []string
	Date         time.Time
	InternalDate time.Time
	Size         int64
	Flags        []imap.Flag
	// Parts is the flattened BODYSTRUCTURE. Left nil, it is read from Raw
	// the way the real adapter reads a server's BODYSTRUCTURE, or is one
	// text/plain part when Raw is empty too.
	Parts []provider.PartInfo
	// Raw is what FetchRaw and FetchHeader return, and what FetchPart cuts
	// sections from. Synthesised when empty.
	Raw []byte
}

// FakeOptions configure a FakeMailbox.
type FakeOptions struct {
	// Kind picks the profile. Defaults to generic IMAP.
	Kind provider.Kind
	// Profile overrides the kind's profile.
	Profile *provider.Profile
	// Caps are what every session reports. The zero value is a bare
	// IMAP4rev1 server: the uidpoll path, no CONDSTORE, no ESEARCH, and no
	// IDLE either, so the engine's polling fallback is what runs.
	// ExchangeCaps and GmailCaps are the two common shapes.
	Caps provider.Caps
	// Clock stamps dates the test leaves out, and idle events. Defaults to
	// time.Now.
	Clock func() time.Time
	// SharedFlags makes a flag change reach every copy of a message across
	// folders, as Gmail's labels do.
	SharedFlags bool
	// Labels makes folders hold a message at most once, as Gmail's labels
	// do: a COPY or MOVE into a folder that already holds it adds no second
	// copy, and COPYUID reports the UID it has there. (That is what Gmail is
	// taken to do, and something to check again on a real account.)
	Labels bool
	// ReversedCopyUID reports a COPY or MOVE of several messages the way the
	// adapter hands Gmail's answer over: go-imap parses COPYUID's two UID
	// sets into sorted ranges, so pairing them by position — all a
	// MoveResult of several UIDs can do — is not the server's pairing. The
	// fake gives new UIDs in the reverse order of the source UIDs and
	// reports both sides in ascending order, paired by position: a Mapping
	// of several UIDs that sends each message to another's UID. (With
	// Labels, a folder that already holds a message reports the UID it has
	// there, sorted in with the rest.)
	ReversedCopyUID bool
	// MaxSessions refuses Open with ErrTooManyConnections once this many
	// sessions are open. Zero is unlimited.
	MaxSessions int
}

// ExchangeCaps is what Exchange Online reports after login: no CONDSTORE, no
// ESEARCH, no SPECIAL-USE, no LIST-STATUS.
func ExchangeCaps() provider.Caps {
	return provider.Caps{Move: true, UIDPlus: true, Idle: true}
}

// GmailCaps is what Gmail reports after login, less the extensions this
// server never enables.
func GmailCaps() provider.Caps {
	return provider.Caps{
		CondStore: true, ESearch: true, Move: true, UIDPlus: true, SpecialUse: true,
		ListStatus: true, Idle: true, UTF8Accept: true, AppendLimit: 35882577,
	}
}

// NewFakeMailbox builds an empty server with an INBOX.
func NewFakeMailbox(opts FakeOptions) *FakeMailbox {
	kind := opts.Kind
	if kind == "" {
		kind = provider.KindIMAP
	}
	profile := provider.ProfileFor(kind)
	if opts.Profile != nil {
		profile = *opts.Profile
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	m := &FakeMailbox{
		kind: kind, profile: profile, clock: clock,
		caps:            opts.Caps,
		folders:         map[string]*fakeFolder{},
		nextUIDValidity: 1000,
		sharedFlags:     opts.SharedFlags,
		labels:          opts.Labels,
		reversedCopyUID: opts.ReversedCopyUID,
		maxSessions:     opts.MaxSessions,
		sessions:        map[*fakeSession]struct{}{},
		peakByRole:      map[provider.Role]int{},
		opens:           map[provider.Role]int{},
		logouts:         map[provider.Role]int{},
		failures:        map[Method][]*failure{},
	}
	m.sender = &FakeSender{box: m}
	m.CreateFolder("INBOX")
	return m
}

var _ provider.Mailbox = (*FakeMailbox)(nil)

// Kind implements provider.Mailbox.
func (m *FakeMailbox) Kind() provider.Kind { return m.kind }

// Profile implements provider.Mailbox.
func (m *FakeMailbox) Profile() provider.Profile { return m.profile }

// Close implements provider.Mailbox. Like the real adapter's, it releases
// nothing: sessions belong to whoever opened them.
func (m *FakeMailbox) Close() error { return nil }

// Sender implements provider.Mailbox.
func (m *FakeMailbox) Sender() provider.Sender { return m.sender }

// Open implements provider.Mailbox.
func (m *FakeMailbox) Open(ctx context.Context, role provider.Role) (provider.Session, error) {
	if err := m.enter(ctx, nil, Call{Method: MethodOpen, Role: role}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		return nil, fmt.Errorf("%w: the fake allows %d", provider.ErrTooManyConnections, m.maxSessions)
	}
	s := &fakeSession{
		box: m, role: role, caps: m.caps,
		events: make(chan provider.IdleEvent, fakeEventBuffer),
		closed: make(chan struct{}),
	}
	m.sessions[s] = struct{}{}
	m.opens[role]++
	if n := len(m.sessions); n > m.peak {
		m.peak = n
	}
	byRole := 0
	for other := range m.sessions {
		if other.role == role {
			byRole++
		}
	}
	if byRole > m.peakByRole[role] {
		m.peakByRole[role] = byRole
	}
	return s, nil
}

// fakeEventBuffer matches the real adapter's: signals beyond it are dropped
// and the session reports it overflowed.
const fakeEventBuffer = 64

// enter runs the failure injection and the hook for one call, and logs it.
// s is nil for mailbox-level calls.
func (m *FakeMailbox) enter(ctx context.Context, s *fakeSession, c Call) error {
	m.mu.Lock()
	m.calls = append(m.calls, c)
	var injected error
	if queue := m.failures[c.Method]; len(queue) > 0 {
		f := queue[0]
		injected = f.err
		if f.times > 0 {
			f.times--
			if f.times == 0 {
				m.failures[c.Method] = queue[1:]
			}
		}
	}
	hook := m.hook
	m.mu.Unlock()

	if injected != nil {
		if s != nil && errors.Is(injected, provider.ErrConnClosed) {
			s.kill(injected)
		}
		return injected
	}
	if hook != nil {
		if err := hook(ctx, c); err != nil {
			if s != nil && errors.Is(err, provider.ErrConnClosed) {
				s.kill(err)
			}
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		// As the real adapter: a caller that has gone away sends nothing,
		// and the connection stays as it was for the next one.
		return fmt.Errorf("%w: %w", provider.ErrConnClosed, err)
	}
	return nil
}

// FailNext makes the next call of method return err. An error wrapping
// provider.ErrConnClosed also kills the session it was issued on, as a dropped
// connection would, and is its CloseCause.
func (m *FakeMailbox) FailNext(method Method, err error) { m.FailTimes(method, 1, err) }

// FailTimes makes the next n calls of method return err.
func (m *FakeMailbox) FailTimes(method Method, n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[method] = append(m.failures[method], &failure{err: err, times: n})
}

// FailAlways makes every call of method return err until ClearFailures.
func (m *FakeMailbox) FailAlways(method Method, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[method] = append(m.failures[method], &failure{err: err, times: -1})
}

// ClearFailures forgets every injected failure.
func (m *FakeMailbox) ClearFailures() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = map[Method][]*failure{}
}

// OnCall installs a hook run at the start of every call, after injected
// failures. It may block — honouring ctx — to hold a command in flight, or
// return an error to fail it. Nil removes it.
func (m *FakeMailbox) OnCall(hook func(ctx context.Context, c Call) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hook = hook
}

// SetCaps changes what sessions opened from now on report.
func (m *FakeMailbox) SetCaps(caps provider.Caps) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps = caps
}

// Calls returns every call so far, oldest first.
func (m *FakeMailbox) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

// CallCount counts the calls of one method.
func (m *FakeMailbox) CallCount(method Method) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// ResetCalls empties the call log.
func (m *FakeMailbox) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

// Violations lists every misuse the fake saw: two commands in flight on one
// session, a command issued while the session was idling, IDLE on a session
// that is not the idle one, a command with no folder selected, a call on a
// closed session. A correct engine produces none.
func (m *FakeMailbox) Violations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.violations...)
}

func (m *FakeMailbox) violate(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

// OpenSessions counts the sessions open now, for one role or (none given)
// all of them.
func (m *FakeMailbox) OpenSessions(roles ...provider.Role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for s := range m.sessions {
		if len(roles) == 0 || containsRole(roles, s.role) {
			n++
		}
	}
	return n
}

// PeakSessions is the most sessions that were ever open at once, for one role
// or (none given) all of them.
func (m *FakeMailbox) PeakSessions(roles ...provider.Role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(roles) == 0 {
		return m.peak
	}
	n := 0
	for _, r := range roles {
		n += m.peakByRole[r]
	}
	return n
}

// Opens counts the sessions ever opened for a role.
func (m *FakeMailbox) Opens(role provider.Role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens[role]
}

// Logouts counts sessions of a role their owner closed, as opposed to ones
// the network dropped and nobody logged out.
func (m *FakeMailbox) Logouts(role provider.Role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logouts[role]
}

// ClosedIdling counts sessions closed while still in IDLE: logged out without
// DONE first.
func (m *FakeMailbox) ClosedIdling() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closedIdling
}

// DelayLogouts makes every LOGOUT from now on take d, with the connection
// still counted as open until it is done. Zero makes them instant again.
func (m *FakeMailbox) DelayLogouts(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logoutDelay = d
}

// KillSessions drops the connections of the given roles (all, when none is
// given), as a server hanging up would: Closed fires, and CloseCause, and
// every later call, is an error wrapping ErrServerEnded.
func (m *FakeMailbox) KillSessions(roles ...provider.Role) int {
	return m.KillSessionsWith(fmt.Errorf("%w: the server closed the connection", provider.ErrServerEnded), roles...)
}

// KillSessionsWith is KillSessions for a connection that ended some other
// way: cause, which must wrap ErrConnClosed, is its CloseCause — a response
// the client could not parse, say.
func (m *FakeMailbox) KillSessionsWith(cause error, roles ...provider.Role) int {
	m.mu.Lock()
	var victims []*fakeSession
	for s := range m.sessions {
		if len(roles) == 0 || containsRole(roles, s.role) {
			victims = append(victims, s)
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		s.kill(cause)
	}
	return len(victims)
}

func containsRole(roles []provider.Role, r provider.Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// CreateFolder adds a selectable folder with a fresh UIDVALIDITY. Creating
// one that exists changes nothing.
func (m *FakeMailbox) CreateFolder(name string, attrs ...imap.MailboxAttr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createLocked(name, true, attrs)
}

// CreateContainer adds a \Noselect folder, the kind that only holds others.
func (m *FakeMailbox) CreateContainer(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createLocked(name, false, []imap.MailboxAttr{imap.MailboxAttrNoSelect})
}

func (m *FakeMailbox) createLocked(name string, selectable bool, attrs []imap.MailboxAttr) *fakeFolder {
	if f, ok := m.folders[name]; ok {
		return f
	}
	m.nextUIDValidity++
	f := &fakeFolder{
		name: name, delim: '/', attrs: append([]imap.MailboxAttr(nil), attrs...), selectable: selectable,
		uidValidity: m.nextUIDValidity, uidNext: 1, highestModSeq: 1, rows: map[imap.UID]*fakeRow{},
	}
	m.folders[name] = f
	m.order = append(m.order, name)
	return f
}

func (m *FakeMailbox) mustFolder(name string) *fakeFolder {
	f, ok := m.folders[name]
	if !ok {
		panic(fmt.Sprintf("providertest: no folder %q in the fake mailbox", name))
	}
	return f
}

// DeleteFolder removes a folder and everything in it.
func (m *FakeMailbox) DeleteFolder(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mustFolder(name)
	delete(m.folders, name)
	m.order = removeString(m.order, name)
}

// RenameFolder renames a folder, keeping its UIDVALIDITY and rows.
func (m *FakeMailbox) RenameFolder(from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.mustFolder(from)
	if _, taken := m.folders[to]; taken {
		panic(fmt.Sprintf("providertest: folder %q already exists", to))
	}
	delete(m.folders, from)
	f.name = to
	m.folders[to] = f
	for i, n := range m.order {
		if n == from {
			m.order[i] = to
		}
	}
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// Deliver adds a message to a folder, as mail arriving or another client
// appending, and returns its UID.
func (m *FakeMailbox) Deliver(folder string, msg FakeMessage) imap.UID {
	m.mu.Lock()
	f := m.mustFolder(folder)
	m.nextKey++
	uid := m.addLocked(f, m.nextKey, m.complete(msg), msg.Flags)
	signals := m.existsLocked(f)
	m.mu.Unlock()
	signals.send()
	return uid
}

// complete fills in what a test left out. Called with m.mu held.
func (m *FakeMailbox) complete(msg FakeMessage) FakeMessage {
	now := m.clock()
	if msg.InternalDate.IsZero() {
		msg.InternalDate = now
	}
	if msg.Date.IsZero() {
		msg.Date = msg.InternalDate
	}
	if msg.Parts == nil && len(msg.Raw) > 0 {
		msg.Parts = imapprovider.FlattenBodyStructure(
			imapserver.ExtractBodyStructure(bytes.NewReader(msg.Raw)))
	}
	if msg.Parts == nil {
		msg.Parts = []provider.PartInfo{{
			Path: []int{1}, MIMEType: "text/plain", Params: map[string]string{"charset": "utf-8"},
			Encoding: "7bit", Size: 64, IsBody: true,
		}}
	}
	if msg.Size == 0 {
		msg.Size = int64(len(msg.Raw))
		if msg.Size == 0 {
			msg.Size = int64(len(synthesize(msg)))
		}
	}
	return msg
}

func (m *FakeMailbox) addLocked(f *fakeFolder, key int64, msg FakeMessage, flags []imap.Flag) imap.UID {
	uid := f.uidNext
	f.uidNext++
	f.highestModSeq++
	f.rows[uid] = &fakeRow{uid: uid, key: key, flags: normalizeFlags(flags), modseq: f.highestModSeq, msg: msg}
	return uid
}

// CopyTo puts another copy of a message in dest — a Gmail label being added —
// sharing its identity, and returns the copy's UID.
func (m *FakeMailbox) CopyTo(src string, uid imap.UID, dest string) imap.UID {
	m.mu.Lock()
	from, to := m.mustFolder(src), m.mustFolder(dest)
	row := mustRow(from, uid)
	copied := m.addLocked(to, row.key, row.msg, row.flags)
	signals := m.existsLocked(to)
	m.mu.Unlock()
	signals.send()
	return copied
}

// MoveTo moves a message to dest, as another client would, and returns its
// UID there.
func (m *FakeMailbox) MoveTo(src string, uid imap.UID, dest string) imap.UID {
	m.mu.Lock()
	from, to := m.mustFolder(src), m.mustFolder(dest)
	row := mustRow(from, uid)
	signals := m.expungeLocked(from, uid)
	moved := m.addLocked(to, row.key, row.msg, row.flags)
	signals = append(signals, m.existsLocked(to)...)
	m.mu.Unlock()
	signals.send()
	return moved
}

func mustRow(f *fakeFolder, uid imap.UID) *fakeRow {
	row, ok := f.rows[uid]
	if !ok {
		panic(fmt.Sprintf("providertest: no UID %d in %q", uid, f.name))
	}
	return row
}

// Expunge removes messages from a folder, as another client deleting them.
func (m *FakeMailbox) Expunge(folder string, uids ...imap.UID) {
	m.mu.Lock()
	f := m.mustFolder(folder)
	var signals pending
	for _, uid := range uids {
		signals = append(signals, m.expungeLocked(f, uid)...)
	}
	m.mu.Unlock()
	signals.send()
}

// expungeLocked removes one row and returns the EXPUNGE signal for the
// sessions watching the folder, with the sequence number the row had.
func (m *FakeMailbox) expungeLocked(f *fakeFolder, uid imap.UID) pending {
	if _, ok := f.rows[uid]; !ok {
		return nil
	}
	seq := uint32(0)
	for _, u := range f.sortedUIDs() {
		seq++
		if u == uid {
			break
		}
	}
	delete(f.rows, uid)
	f.highestModSeq++
	return m.signalLocked(f, provider.IdleEvent{Kind: provider.IdleExpunge, SeqNum: seq})
}

// SetFlags replaces a message's flags, as another client would. With
// SharedFlags, every copy of the message changes too.
func (m *FakeMailbox) SetFlags(folder string, uid imap.UID, flags ...imap.Flag) {
	m.mu.Lock()
	f := m.mustFolder(folder)
	row := mustRow(f, uid)
	var signals pending
	for _, target := range m.copiesLocked(f, row) {
		target.row.flags = normalizeFlags(flags)
		target.folder.highestModSeq++
		target.row.modseq = target.folder.highestModSeq
		signals = append(signals, m.signalLocked(target.folder, provider.IdleEvent{
			Kind: provider.IdleFetch, SeqNum: target.folder.seqOf(target.row.uid),
		})...)
	}
	m.mu.Unlock()
	signals.send()
}

type rowRef struct {
	folder *fakeFolder
	row    *fakeRow
}

// copiesLocked is the row itself, and with SharedFlags every other copy of the
// same message.
func (m *FakeMailbox) copiesLocked(f *fakeFolder, row *fakeRow) []rowRef {
	if !m.sharedFlags {
		return []rowRef{{f, row}}
	}
	var out []rowRef
	for _, name := range m.order {
		other := m.folders[name]
		for _, r := range other.rows {
			if r.key == row.key {
				out = append(out, rowRef{other, r})
			}
		}
	}
	return out
}

// ReassignUID gives a message a new UID in the same folder, as Exchange does
// after moving a mailbox between databases, and returns it.
func (m *FakeMailbox) ReassignUID(folder string, uid imap.UID) imap.UID {
	m.mu.Lock()
	f := m.mustFolder(folder)
	row := mustRow(f, uid)
	signals := m.expungeLocked(f, uid)
	fresh := m.addLocked(f, row.key, row.msg, row.flags)
	signals = append(signals, m.existsLocked(f)...)
	m.mu.Unlock()
	signals.send()
	return fresh
}

// ChangeUIDValidity gives a folder a new UIDVALIDITY and renumbers its
// messages from 1 in their current order, as Exchange does unprompted. It
// returns the new UIDVALIDITY.
func (m *FakeMailbox) ChangeUIDValidity(folder string) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.mustFolder(folder)
	m.nextUIDValidity++
	f.uidValidity = m.nextUIDValidity
	old := f.sortedUIDs()
	rows := make(map[imap.UID]*fakeRow, len(old))
	next := imap.UID(1)
	for _, uid := range old {
		row := f.rows[uid]
		row.uid = next
		rows[row.uid] = row
		next++
	}
	f.rows = rows
	f.uidNext = next
	f.highestModSeq++
	return f.uidValidity
}

// FolderState is a snapshot of one folder on the fake server.
type FolderState struct {
	UIDValidity   uint32
	UIDNext       imap.UID
	HighestModSeq uint64
	UIDs          []imap.UID
}

// Folder returns a snapshot of a folder.
func (m *FakeMailbox) Folder(name string) (FolderState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.folders[name]
	if !ok {
		return FolderState{}, false
	}
	return FolderState{UIDValidity: f.uidValidity, UIDNext: f.uidNext, HighestModSeq: f.highestModSeq, UIDs: f.sortedUIDs()}, true
}

// Flags returns a message's flags on the server.
func (m *FakeMailbox) Flags(folder string, uid imap.UID) []imap.Flag {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]imap.Flag(nil), mustRow(m.mustFolder(folder), uid).flags...)
}

// uidOf is the UID a message has in the folder, if it is there.
func (f *fakeFolder) uidOf(key int64) (imap.UID, bool) {
	for uid, r := range f.rows {
		if r.key == key {
			return uid, true
		}
	}
	return 0, false
}

func (f *fakeFolder) sortedUIDs() []imap.UID {
	out := make([]imap.UID, 0, len(f.rows))
	for uid := range f.rows {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (f *fakeFolder) seqOf(uid imap.UID) uint32 {
	seq := uint32(0)
	for _, u := range f.sortedUIDs() {
		seq++
		if u == uid {
			return seq
		}
	}
	return 0
}

// count is EXISTS: how many messages the folder holds.
func (f *fakeFolder) count() uint32 {
	n := uint32(0)
	for range f.rows {
		n++
	}
	return n
}

func (f *fakeFolder) unseen() uint32 {
	n := uint32(0)
	for _, r := range f.rows {
		if !hasFlag(r.flags, imap.FlagSeen) {
			n++
		}
	}
	return n
}

// match resolves a UID set against the folder the way a server does: "*" is
// the largest UID present, ranges are unordered, UIDs that do not exist are
// skipped. Ascending.
func (f *fakeFolder) match(set imap.UIDSet) []imap.UID {
	all := f.sortedUIDs()
	if len(all) == 0 {
		return nil
	}
	star := all[len(all)-1]
	var out []imap.UID
	for _, uid := range all {
		for _, r := range set {
			lo, hi := r.Start, r.Stop
			if lo == 0 {
				lo = star
			}
			if hi == 0 {
				hi = star
			}
			if lo > hi {
				lo, hi = hi, lo
			}
			if uid >= lo && uid <= hi {
				out = append(out, uid)
				break
			}
		}
	}
	return out
}

// pending signals are sent after the lock is released.
type pending []signal

type signal struct {
	s  *fakeSession
	ev provider.IdleEvent
}

func (p pending) send() {
	for _, sig := range p {
		sig.s.push(sig.ev)
	}
}

func (m *FakeMailbox) existsLocked(f *fakeFolder) pending {
	return m.signalLocked(f, provider.IdleEvent{Kind: provider.IdleExists, NumMessages: f.count()})
}

// signalLocked addresses an event to every live session with f selected.
// Inside Unheard the event is lost instead, and the session only learns that
// it missed something.
func (m *FakeMailbox) signalLocked(f *fakeFolder, ev provider.IdleEvent) pending {
	ev.At = m.clock()
	var out pending
	for s := range m.sessions {
		if !s.watching(f) {
			continue
		}
		if m.deaf {
			s.overflow.Store(true)
			continue
		}
		out = append(out, signal{s: s, ev: ev})
	}
	return out
}

// Unheard runs fn — deliveries, expunges, flag changes made through this
// fake's methods — with every notification those changes would send lost, as
// when a session's event buffer was full: the sessions watching the folders
// fn touched only report Overflowed afterwards. Whatever they hear next is
// all a client has to go on.
func (m *FakeMailbox) Unheard(fn func()) {
	m.mu.Lock()
	m.deaf = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.deaf = false
		m.mu.Unlock()
	}()
	fn()
}

// fakeSession is one connection.
type fakeSession struct {
	box    *FakeMailbox
	role   provider.Role
	caps   provider.Caps
	events chan provider.IdleEvent

	busy     atomic.Bool
	overflow atomic.Bool

	mu       sync.Mutex
	selected *provider.FolderStatus
	folder   *fakeFolder // what selected points at, for signals
	idling   bool
	dead     bool
	// loggedOut is a session its owner closed, as opposed to one the
	// network dropped: using it afterwards is the owner's mistake.
	loggedOut bool
	// eventsClosed: like the real adapter, Close (and only Close) closes the
	// events channel, so a reader ranging over it ends.
	eventsClosed bool
	closed       chan struct{}
	// cause is why a dead session died (CloseCause).
	cause error
}

var _ provider.Session = (*fakeSession)(nil)

func (s *fakeSession) Caps() provider.Caps { return s.caps }
func (s *fakeSession) Role() provider.Role { return s.role }

func (s *fakeSession) Selected() *provider.FolderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selected == nil {
		return nil
	}
	out := *s.selected
	return &out
}

func (s *fakeSession) Events() <-chan provider.IdleEvent { return s.events }

func (s *fakeSession) Overflowed() bool { return s.overflow.Swap(false) }

func (s *fakeSession) Closed() <-chan struct{} { return s.closed }

// CloseCause is what killed the session: the injected failure that wrapped
// ErrConnClosed, KillSessions' hang-up, or Close.
func (s *fakeSession) CloseCause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dead {
		return nil
	}
	return s.cause
}

func (s *fakeSession) watching(f *fakeFolder) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.dead && s.folder == f
}

// push delivers a signal without blocking, holding the session's lock so it
// can never race Close closing the channel.
func (s *fakeSession) push(ev provider.IdleEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.overflow.Store(true)
	}
}

// kill ends the connection as a network failure would.
func (s *fakeSession) kill(cause error) {
	s.box.mu.Lock()
	defer s.box.mu.Unlock()
	s.killLocked(cause)
}

// killLocked is kill with box.mu already held.
func (s *fakeSession) killLocked(cause error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead = true
	s.cause = cause
	s.idling = false
	s.mu.Unlock()
	close(s.closed)
	delete(s.box.sessions, s)
}

// begin guards one command: the session must be alive, not idling, and not
// already running another command. It returns the function that ends it.
func (s *fakeSession) begin(ctx context.Context, c Call) (func(), error) {
	c.Role = s.role
	if !s.busy.CompareAndSwap(false, true) {
		s.box.violate("%s on the %s session while another command was in flight", c.Method, s.role)
		return nil, fmt.Errorf("%w: concurrent command on one session", provider.ErrConnClosed)
	}
	end := func() { s.busy.Store(false) }
	s.mu.Lock()
	dead, idling, loggedOut := s.dead, s.idling, s.loggedOut
	if c.Folder == "" && s.selected != nil {
		c.Folder = s.selected.Name
	}
	s.mu.Unlock()
	if dead {
		end()
		if loggedOut {
			s.box.violate("%s on the %s session after it was closed", c.Method, s.role)
		}
		return nil, s.CloseCause()
	}
	if idling {
		end()
		s.box.violate("%s on the %s session while it was idling", c.Method, s.role)
		return nil, fmt.Errorf("%w: command issued during IDLE", provider.ErrConnClosed)
	}
	if err := s.box.enter(ctx, s, c); err != nil {
		end()
		return nil, err
	}
	return end, nil
}

// current is the selected folder, checked against the server: a folder deleted
// or renamed under the session is gone, and one whose UIDVALIDITY changed
// ends the connection, as servers do. Called with box.mu held.
func (s *fakeSession) current(method Method) (*fakeFolder, error) {
	s.mu.Lock()
	f, sel := s.folder, s.selected
	s.mu.Unlock()
	if f == nil || sel == nil {
		s.box.violations = append(s.box.violations, fmt.Sprintf("%s on the %s session with no folder selected", method, s.role))
		return nil, fmt.Errorf("%w: no folder selected", provider.ErrTemporary)
	}
	if s.box.folders[sel.Name] != f {
		return nil, fmt.Errorf("%w: %s", provider.ErrFolderNotFound, sel.Name)
	}
	if f.uidValidity != sel.UIDValidity {
		err := fmt.Errorf("%w: the folder's UIDVALIDITY changed under the session", provider.ErrConnClosed)
		s.killLocked(err)
		return nil, err
	}
	return f, nil
}

func (s *fakeSession) ListFolders(ctx context.Context, withStatus bool) ([]provider.Folder, error) {
	end, err := s.begin(ctx, Call{Method: MethodListFolders})
	if err != nil {
		return nil, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provider.Folder, 0, len(m.order))
	for _, name := range m.order {
		f := m.folders[name]
		entry := provider.Folder{
			Name: f.name, Delim: f.delim, Attrs: append([]imap.MailboxAttr(nil), f.attrs...), Selectable: f.selectable,
		}
		if withStatus && s.caps.ListStatus && f.selectable {
			st := s.statusOf(f)
			entry.Status = &st
		}
		out = append(out, entry)
	}
	return out, nil
}

// statusOf is what SELECT or STATUS reports. Called with box.mu held.
func (s *fakeSession) statusOf(f *fakeFolder) provider.FolderStatus {
	st := provider.FolderStatus{
		Name: f.name, UIDValidity: f.uidValidity, UIDNext: f.uidNext,
		NumMessages: f.count(), NumUnseen: f.unseen(),
	}
	if s.caps.CondStore {
		st.HighestModSeq = f.highestModSeq
	}
	return st
}

func (s *fakeSession) Select(ctx context.Context, name string, readOnly bool, expectUIDValidity uint32) (provider.FolderStatus, error) {
	end, err := s.begin(ctx, Call{Method: MethodSelect, Folder: name, ReadOnly: readOnly})
	if err != nil {
		return provider.FolderStatus{}, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	f, ok := m.folders[name]
	if !ok || !f.selectable {
		m.mu.Unlock()
		// A SELECT that fails leaves no folder selected, as on a server.
		s.mu.Lock()
		s.selected, s.folder = nil, nil
		s.mu.Unlock()
		return provider.FolderStatus{}, fmt.Errorf("%w: %s", provider.ErrFolderNotFound, name)
	}
	st := s.statusOf(f)
	st.ReadOnly = readOnly
	st.PermanentFlags = []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft}
	m.mu.Unlock()

	s.mu.Lock()
	s.selected, s.folder = &st, f
	s.mu.Unlock()
	if expectUIDValidity != 0 && st.UIDValidity != expectUIDValidity {
		return st, fmt.Errorf("%w: was %d, now %d", provider.ErrUIDValidityChanged, expectUIDValidity, st.UIDValidity)
	}
	return st, nil
}

func (s *fakeSession) Status(ctx context.Context, name string) (provider.FolderStatus, error) {
	end, err := s.begin(ctx, Call{Method: MethodStatus, Folder: name})
	if err != nil {
		return provider.FolderStatus{}, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.folders[name]
	if !ok || !f.selectable {
		return provider.FolderStatus{}, fmt.Errorf("%w: %s", provider.ErrFolderNotFound, name)
	}
	return s.statusOf(f), nil
}

func (s *fakeSession) Create(ctx context.Context, name string) error {
	end, err := s.begin(ctx, Call{Method: MethodCreate, Folder: name})
	if err != nil {
		return err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.folders[name]; ok {
		return fmt.Errorf("%w: %s already exists", provider.ErrTerminal, name)
	}
	m.createLocked(name, true, nil)
	return nil
}

func (s *fakeSession) UIDs(ctx context.Context, set imap.UIDSet, since time.Time) ([]imap.UID, error) {
	end, err := s.begin(ctx, Call{Method: MethodUIDs, Set: set.String(), Since: since})
	if err != nil {
		return nil, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := s.current(MethodUIDs)
	if err != nil {
		return nil, err
	}
	var out []imap.UID
	for _, uid := range f.match(set) {
		if !since.IsZero() && dateOf(f.rows[uid].msg.InternalDate).Before(dateOf(since)) {
			continue
		}
		out = append(out, uid)
	}
	return out, nil
}

// dateOf is SEARCH SINCE's comparison: the calendar date, ignoring the time
// of day and the zone (RFC 3501). Each side is read in its own location: the
// date a client puts on the wire is its time's date where it was computed
// (go-imap formats it so), and a server reads a message's INTERNALDATE in the
// zone it carries. A message received at 23:30 -0300 is on the 1st to the
// server, though it is 02:30 on the 2nd in UTC.
func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *fakeSession) UIDCount(ctx context.Context, set imap.UIDSet) (uint32, error) {
	end, err := s.begin(ctx, Call{Method: MethodUIDCount, Set: set.String()})
	if err != nil {
		return 0, err
	}
	defer end()
	if !s.caps.ESearch {
		return 0, fmt.Errorf("%w: ESEARCH is required to count without listing", provider.ErrUnsupported)
	}
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := s.current(MethodUIDCount)
	if err != nil {
		return 0, err
	}
	n := uint32(0)
	for range f.match(set) {
		n++
	}
	return n, nil
}

func (s *fakeSession) FetchSummaries(ctx context.Context, set imap.UIDSet, changedSince uint64, fn func(provider.Summary) error) error {
	end, err := s.begin(ctx, Call{Method: MethodFetchSummaries, Set: set.String(), ChangedSince: changedSince})
	if err != nil {
		return err
	}
	defer end()
	if changedSince > 0 && !s.caps.CondStore {
		return fmt.Errorf("%w: CONDSTORE is required for CHANGEDSINCE", provider.ErrUnsupported)
	}
	m := s.box
	m.mu.Lock()
	f, err := s.current(MethodFetchSummaries)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	var batch []provider.Summary
	for _, uid := range f.match(set) {
		row := f.rows[uid]
		if changedSince > 0 && row.modseq <= changedSince {
			continue
		}
		batch = append(batch, s.summaryOf(row))
	}
	m.mu.Unlock()

	// Drained whatever fn says, as the real adapter does.
	var fnErr error
	for _, sum := range batch {
		if fnErr != nil {
			continue
		}
		fnErr = fn(sum)
	}
	return fnErr
}

// summaryOf builds what FETCH (UID FLAGS ENVELOPE INTERNALDATE RFC822.SIZE
// BODYSTRUCTURE [MODSEQ]) returns for a row: fresh copies, so nothing the
// caller does reaches the fake's state.
func (s *fakeSession) summaryOf(row *fakeRow) provider.Summary {
	msg := row.msg
	env := &imap.Envelope{
		Date: msg.Date, Subject: msg.Subject, MessageID: strings.Trim(msg.MessageID, "<> "),
		From: parseAddresses([]string{msg.From}), To: parseAddresses(msg.To), Cc: parseAddresses(msg.Cc),
	}
	if msg.InReplyTo != "" {
		env.InReplyTo = []string{strings.Trim(msg.InReplyTo, "<> ")}
	}
	parts := make([]provider.PartInfo, len(msg.Parts))
	for i, p := range msg.Parts {
		p.Path = append([]int(nil), p.Path...)
		if p.Params != nil {
			params := make(map[string]string, len(p.Params))
			for k, v := range p.Params {
				params[k] = v
			}
			p.Params = params
		}
		parts[i] = p
	}
	sum := provider.Summary{
		UID: row.uid, Flags: append([]imap.Flag(nil), row.flags...), Envelope: env,
		InternalDate: msg.InternalDate, Size: msg.Size, Parts: parts,
		References: append([]string(nil), msg.References...),
	}
	if s.caps.CondStore {
		sum.ModSeq = row.modseq
	}
	return sum
}

func parseAddresses(list []string) []imap.Address {
	var out []imap.Address
	for _, raw := range list {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil {
			addr = &mail.Address{Address: raw}
		}
		local, host, _ := strings.Cut(addr.Address, "@")
		out = append(out, imap.Address{Name: addr.Name, Mailbox: local, Host: host})
	}
	return out
}

func (s *fakeSession) FetchFlags(ctx context.Context, set imap.UIDSet, changedSince uint64) ([]provider.FlagUpdate, error) {
	end, err := s.begin(ctx, Call{Method: MethodFetchFlags, Set: set.String(), ChangedSince: changedSince})
	if err != nil {
		return nil, err
	}
	defer end()
	if changedSince > 0 && !s.caps.CondStore {
		return nil, fmt.Errorf("%w: CONDSTORE is required for CHANGEDSINCE", provider.ErrUnsupported)
	}
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := s.current(MethodFetchFlags)
	if err != nil {
		return nil, err
	}
	out := []provider.FlagUpdate{}
	for _, uid := range f.match(set) {
		row := f.rows[uid]
		if changedSince > 0 && row.modseq <= changedSince {
			continue
		}
		u := provider.FlagUpdate{UID: uid, Flags: append([]imap.Flag(nil), row.flags...)}
		if s.caps.CondStore {
			u.ModSeq = row.modseq
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *fakeSession) FetchHeader(ctx context.Context, uid imap.UID) ([]byte, error) {
	end, err := s.begin(ctx, Call{Method: MethodFetchHeader, Set: imap.UIDSetNum(uid).String()})
	if err != nil {
		return nil, err
	}
	defer end()
	raw, err := s.raw(uid, MethodFetchHeader)
	if err != nil {
		return nil, err
	}
	header, _, found := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !found {
		return raw, nil
	}
	return append(header, "\r\n\r\n"...), nil
}

// FetchPart serves one section of the message: what an IMAP server answers
// for BODY.PEEK[<path>], cut from its raw bytes. Like every fetch here it
// leaves the flags alone, as a PEEK does. A section above maxBytes is
// refused, as the real adapter refuses it.
func (s *fakeSession) FetchPart(ctx context.Context, uid imap.UID, info provider.PartInfo, maxBytes int64) (provider.Part, error) {
	end, err := s.begin(ctx, Call{
		Method: MethodFetchPart, Set: imap.UIDSetNum(uid).String(), Section: info.PathString(),
	})
	if err != nil {
		return provider.Part{}, err
	}
	defer end()
	raw, err := s.raw(uid, MethodFetchPart)
	if err != nil {
		return provider.Part{}, err
	}
	section, err := mime.Section(raw, info.Path)
	if err != nil {
		// A real server answers a section that does not exist with an empty
		// one, or not at all; either way there is nothing to decode.
		return provider.Part{}, fmt.Errorf("%w: UID %d has no section %s", provider.ErrMessageGone, uid, info.PathString())
	}
	if maxBytes > 0 && int64(len(section)) > maxBytes {
		return provider.Part{}, fmt.Errorf("%w: %d bytes", provider.ErrTooLarge, len(section))
	}
	return provider.Part{Info: info, Body: nopSeekCloser{bytes.NewReader(section)}, Size: int64(len(section))}, nil
}

func (s *fakeSession) FetchRaw(ctx context.Context, uid imap.UID, maxBytes int64) (provider.Part, error) {
	end, err := s.begin(ctx, Call{Method: MethodFetchRaw, Set: imap.UIDSetNum(uid).String()})
	if err != nil {
		return provider.Part{}, err
	}
	defer end()
	raw, err := s.raw(uid, MethodFetchRaw)
	if err != nil {
		return provider.Part{}, err
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return provider.Part{}, fmt.Errorf("%w: %d bytes", provider.ErrTooLarge, len(raw))
	}
	return provider.Part{Body: nopSeekCloser{bytes.NewReader(raw)}, Size: int64(len(raw))}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

func (s *fakeSession) raw(uid imap.UID, method Method) ([]byte, error) {
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := s.current(method)
	if err != nil {
		return nil, err
	}
	row, ok := f.rows[uid]
	if !ok {
		return nil, fmt.Errorf("%w: UID %d", provider.ErrMessageGone, uid)
	}
	if len(row.msg.Raw) > 0 {
		return append([]byte(nil), row.msg.Raw...), nil
	}
	return synthesize(row.msg), nil
}

// synthesize writes a minimal RFC 5322 message for a FakeMessage.
func synthesize(msg FakeMessage) []byte {
	var b bytes.Buffer
	if msg.From != "" {
		fmt.Fprintf(&b, "From: %s\r\n", msg.From)
	}
	if len(msg.To) > 0 {
		fmt.Fprintf(&b, "To: %s\r\n", strings.Join(msg.To, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	if !msg.Date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\r\n", msg.Date.Format(time.RFC1123Z))
	}
	if msg.MessageID != "" {
		fmt.Fprintf(&b, "Message-ID: <%s>\r\n", strings.Trim(msg.MessageID, "<> "))
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "Body of %s\r\n", msg.Subject)
	return b.Bytes()
}

// storeText writes a STORE's change as IMAP does, for the call log.
func storeText(op provider.FlagOp, flags []imap.Flag) string {
	names := make([]string, 0, len(flags))
	for _, f := range normalizeFlags(flags) {
		names = append(names, string(f))
	}
	prefix := "FLAGS"
	switch op {
	case provider.FlagAdd:
		prefix = "+FLAGS"
	case provider.FlagDel:
		prefix = "-FLAGS"
	}
	return prefix + " (" + strings.Join(names, " ") + ")"
}

// forWrite does what the adapter does before a command that changes the
// folder: a folder opened with EXAMINE is selected again, read-write — a
// SELECT that is logged, and can fail, like any other.
func (s *fakeSession) forWrite(ctx context.Context) error {
	s.mu.Lock()
	sel := s.selected
	s.mu.Unlock()
	if sel == nil || !sel.ReadOnly {
		return nil
	}
	// The UIDVALIDITY the UIDs were read under: re-opening a folder that was
	// renumbered meanwhile must not let the write land on other messages.
	_, err := s.Select(ctx, sel.Name, false, sel.UIDValidity)
	return err
}

func (s *fakeSession) StoreFlags(ctx context.Context, set imap.UIDSet, op provider.FlagOp, flags []imap.Flag, unchangedSince uint64) ([]provider.FlagUpdate, error) {
	if err := s.forWrite(ctx); err != nil {
		return nil, err
	}
	end, err := s.begin(ctx, Call{Method: MethodStoreFlags, Set: set.String(), Flags: storeText(op, flags)})
	if err != nil {
		return nil, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	f, err := s.current(MethodStoreFlags)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	var (
		out     []provider.FlagUpdate
		signals pending
	)
	for _, uid := range f.match(set) {
		row := f.rows[uid]
		if unchangedSince > 0 && row.modseq > unchangedSince {
			continue
		}
		next := applyOp(row.flags, op, flags)
		for _, target := range m.copiesLocked(f, row) {
			target.row.flags = next
			target.folder.highestModSeq++
			target.row.modseq = target.folder.highestModSeq
			for _, sig := range m.signalLocked(target.folder, provider.IdleEvent{
				Kind: provider.IdleFetch, SeqNum: target.folder.seqOf(target.row.uid),
			}) {
				if sig.s != s {
					signals = append(signals, sig)
				}
			}
		}
		u := provider.FlagUpdate{UID: uid, Flags: append([]imap.Flag(nil), next...)}
		if s.caps.CondStore {
			u.ModSeq = row.modseq
		}
		out = append(out, u)
	}
	m.mu.Unlock()
	signals.send()
	return out, nil
}

func applyOp(current []imap.Flag, op provider.FlagOp, flags []imap.Flag) []imap.Flag {
	change := normalizeFlags(flags)
	switch op {
	case provider.FlagSet:
		return change
	case provider.FlagAdd:
		return normalizeFlags(append(append([]imap.Flag(nil), current...), change...))
	case provider.FlagDel:
		var out []imap.Flag
		for _, f := range current {
			if !hasFlag(change, f) {
				out = append(out, f)
			}
		}
		return out
	}
	return current
}

// Move answers as the adapter does: MOVE where the Caps have it, and
// otherwise COPY, STORE \Deleted and UID EXPUNGE of exactly the set, which
// needs UIDPLUS. With neither it refuses before changing anything. Only the
// moved UIDs leave the folder either way; COPYUID (the mapping and the
// destination's UIDVALIDITY) comes back only with UIDPLUS.
func (s *fakeSession) Move(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	if !s.caps.Move && !s.caps.UIDPlus {
		return provider.MoveResult{}, fmt.Errorf("%w: neither MOVE nor UIDPLUS", provider.ErrUnsupported)
	}
	if err := s.forWrite(ctx); err != nil {
		return provider.MoveResult{}, err
	}
	return s.transfer(ctx, Call{Method: MethodMove, Set: set.String(), Dest: dest}, set, dest, true)
}

// Copy puts a copy of each message in dest and leaves the originals, as a
// COPY does — on Gmail, adding a label.
func (s *fakeSession) Copy(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	return s.transfer(ctx, Call{Method: MethodCopy, Set: set.String(), Dest: dest}, set, dest, false)
}

func (s *fakeSession) transfer(ctx context.Context, c Call, set imap.UIDSet, dest string, remove bool) (provider.MoveResult, error) {
	end, err := s.begin(ctx, c)
	if err != nil {
		return provider.MoveResult{}, err
	}
	defer end()
	m := s.box
	m.mu.Lock()
	f, err := s.current(c.Method)
	if err != nil {
		m.mu.Unlock()
		return provider.MoveResult{}, err
	}
	to, ok := m.folders[dest]
	if !ok || !to.selectable {
		m.mu.Unlock()
		return provider.MoveResult{}, fmt.Errorf("%w: %s", provider.ErrFolderNotFound, dest)
	}
	var result provider.MoveResult
	if s.caps.UIDPlus {
		result = provider.MoveResult{DestUIDValidity: to.uidValidity, Mapping: map[imap.UID]imap.UID{}}
	}
	uids := f.match(set)
	if m.reversedCopyUID {
		slices.Reverse(uids)
	}
	var signals pending
	for _, uid := range uids {
		row := f.rows[uid]
		if remove {
			signals = append(signals, m.expungeLocked(f, uid)...)
		}
		landed, held := imap.UID(0), false
		if m.labels && to != f {
			landed, held = to.uidOf(row.key)
		}
		if !held {
			landed = m.addLocked(to, row.key, row.msg, row.flags)
		}
		if result.Mapping != nil {
			result.Mapping[uid] = landed
		}
	}
	if m.reversedCopyUID && result.Mapping != nil {
		result.Mapping = sortedPairing(result.Mapping)
	}
	signals = append(signals, m.existsLocked(to)...)
	m.mu.Unlock()
	signals.send()
	return result, nil
}

// sortedPairing is a mapping as go-imap reads it back from COPYUID: both
// sides sorted, and paired by position.
func sortedPairing(made map[imap.UID]imap.UID) map[imap.UID]imap.UID {
	from := make([]imap.UID, 0, len(made))
	to := make([]imap.UID, 0, len(made))
	for src, dst := range made {
		from, to = append(from, src), append(to, dst)
	}
	slices.Sort(from)
	slices.Sort(to)
	out := make(map[imap.UID]imap.UID, len(made))
	for i := range from {
		out[from[i]] = to[i]
	}
	return out
}

// SearchMessageID finds the selected folder's messages whose Message-ID is
// id, as UID SEARCH HEADER Message-ID does.
func (s *fakeSession) SearchMessageID(ctx context.Context, messageID string) ([]imap.UID, error) {
	id := strings.Trim(strings.TrimSpace(messageID), "<>")
	end, err := s.begin(ctx, Call{Method: MethodSearch, Header: id})
	if err != nil {
		return nil, err
	}
	defer end()
	if id == "" {
		return nil, nil
	}
	m := s.box
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := s.current(MethodSearch)
	if err != nil {
		return nil, err
	}
	var out []imap.UID
	for _, uid := range f.sortedUIDs() {
		if strings.Trim(f.rows[uid].msg.MessageID, "<> ") == id {
			out = append(out, uid)
		}
	}
	return out, nil
}

func (s *fakeSession) Append(ctx context.Context, folder string, r io.Reader, size int64, flags []imap.Flag, t time.Time) (provider.AppendResult, error) {
	end, err := s.begin(ctx, Call{Method: MethodAppend, Folder: folder})
	if err != nil {
		return provider.AppendResult{}, err
	}
	defer end()
	if s.caps.AppendLimit > 0 && size > s.caps.AppendLimit {
		return provider.AppendResult{}, fmt.Errorf("%w: %d bytes", provider.ErrTooLarge, size)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return provider.AppendResult{}, fmt.Errorf("%w: %w", provider.ErrConnClosed, err)
	}
	msg := parseRaw(raw)
	msg.Flags, msg.InternalDate = flags, t
	m := s.box
	m.mu.Lock()
	f, ok := m.folders[folder]
	if !ok || !f.selectable {
		m.mu.Unlock()
		return provider.AppendResult{}, fmt.Errorf("%w: %s", provider.ErrFolderNotFound, folder)
	}
	m.nextKey++
	msg = m.complete(msg)
	msg.Size = int64(len(raw))
	uid := m.addLocked(f, m.nextKey, msg, flags)
	signals := m.existsLocked(f)
	result := provider.AppendResult{UIDValidity: f.uidValidity}
	if s.caps.UIDPlus {
		result.UID = uid
	}
	m.mu.Unlock()
	signals.send()
	return result, nil
}

// parseRaw reads the headers a summary needs from a raw message.
func parseRaw(raw []byte) FakeMessage {
	msg := FakeMessage{Raw: raw}
	parsed, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return msg
	}
	h := parsed.Header
	msg.Subject = h.Get("Subject")
	msg.From = h.Get("From")
	if to := h.Get("To"); to != "" {
		msg.To = strings.Split(to, ",")
	}
	msg.MessageID = strings.Trim(h.Get("Message-Id"), "<> ")
	msg.InReplyTo = strings.Trim(h.Get("In-Reply-To"), "<> ")
	for _, ref := range strings.Fields(h.Get("References")) {
		msg.References = append(msg.References, strings.Trim(ref, "<>"))
	}
	if d, err := h.Date(); err == nil {
		msg.Date = d
	}
	return msg
}

func (s *fakeSession) Idle(ctx context.Context) (provider.IdleHandle, error) {
	if s.role != provider.RoleIdle {
		s.box.violate("IDLE on the %s session; only the idle session parks", s.role)
		return nil, fmt.Errorf("%w: IDLE is for the idle session", provider.ErrUnsupported)
	}
	if !s.caps.Idle {
		// As the adapter answers: a server that does not advertise IDLE is
		// not asked to.
		return nil, fmt.Errorf("%w: the server does not advertise IDLE", provider.ErrUnsupported)
	}
	end, err := s.begin(ctx, Call{Method: MethodIdle})
	if err != nil {
		return nil, err
	}
	defer end()
	s.box.mu.Lock()
	_, err = s.current(MethodIdle)
	s.box.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.idling = true
	s.mu.Unlock()
	return &fakeIdle{s: s}, nil
}

type fakeIdle struct {
	s    *fakeSession
	once sync.Once
}

// Stop ends the IDLE. Idempotent; on a dead connection it reports the drop.
func (h *fakeIdle) Stop() error {
	var err error
	h.once.Do(func() {
		h.s.mu.Lock()
		dead := h.s.dead
		h.s.idling = false
		h.s.mu.Unlock()
		if dead {
			err = fmt.Errorf("%w: the connection dropped during IDLE", provider.ErrConnClosed)
		}
	})
	return err
}

func (s *fakeSession) Noop(ctx context.Context) error {
	end, err := s.begin(ctx, Call{Method: MethodNoop})
	if err != nil {
		return err
	}
	end()
	return nil
}

// Close logs out. Idempotent, and never an error, like the adapter's, and
// closes the events channel as the adapter's does. It takes as long as
// DelayLogouts says, and the connection counts as open until it is over.
func (s *fakeSession) Close() error {
	s.box.mu.Lock()
	delay := s.box.logoutDelay
	s.box.mu.Unlock()
	s.mu.Lock()
	first := !s.loggedOut && !s.dead
	s.mu.Unlock()
	if first && delay > 0 {
		time.Sleep(delay)
	}
	s.box.mu.Lock()
	s.mu.Lock()
	if !s.loggedOut {
		s.box.logouts[s.role]++
		if s.idling && !s.dead {
			s.box.closedIdling++
		}
	}
	s.loggedOut = true
	s.mu.Unlock()
	s.box.mu.Unlock()
	s.kill(fmt.Errorf("%w: the connection was closed on this side", provider.ErrConnClosed))
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
	return nil
}

// FakeSender records what was submitted.
type FakeSender struct {
	box *FakeMailbox

	mu   sync.Mutex
	sent []provider.Outgoing
}

// Send implements provider.Sender. It asks msg.BeforeDial first, as a real
// sender does before it connects, and keeps no copy for Sent.
func (f *FakeSender) Send(ctx context.Context, msg provider.Outgoing) (provider.SendResult, error) {
	if msg.BeforeDial != nil {
		if err := msg.BeforeDial(ctx); err != nil {
			return provider.SendResult{}, err
		}
	}
	if err := f.box.enter(ctx, nil, Call{Method: MethodSend}); err != nil {
		return provider.SendResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return provider.SendResult{ServerReply: "250 2.0.0 OK", SubmittedAt: f.box.clock()}, nil
}

// Sent returns what was submitted, oldest first.
func (f *FakeSender) Sent() []provider.Outgoing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Outgoing(nil), f.sent...)
}

func normalizeFlags(flags []imap.Flag) []imap.Flag {
	seen := map[imap.Flag]bool{}
	var out []imap.Flag
	for _, f := range flags {
		v := imap.Flag(strings.ToLower(string(f)))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	want = imap.Flag(strings.ToLower(string(want)))
	for _, f := range flags {
		if imap.Flag(strings.ToLower(string(f))) == want {
			return true
		}
	}
	return false
}

// Clock is a manual clock for tests: it says what it was last set to.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward and returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
