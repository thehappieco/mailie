// Package provider is the mailbox contract the sync engine and the service
// layer program against.
//
// One implementation ships in v1: internal/provider/imap, driving go-imap and
// go-mail, and serving Gmail, Microsoft 365 and generic IMAP alike. Gmail's
// REST API and Microsoft Graph would be further implementations of the same
// interfaces; nothing here assumes otherwise, but nothing here pretends the
// shape is provider-neutral either. It is IMAP-shaped, because IMAP is what
// the sync engine has to be correct against.
//
// Three interfaces rather than one, for reasons the alternative makes obvious:
//
//   - A Session is ONE connection with ONE selected folder and one command in
//     flight. Modelling it as a mailbox with methods would hide that, and the
//     first consequence would be two goroutines interleaving commands on the
//     same socket.
//   - Mailbox is therefore a factory, not a connection. It owns the account's
//     credentials and its token source; the caller owns the connection budget,
//     because only the caller knows that Gmail counts fifteen connections
//     across every client the person runs, not fifteen for us.
//   - Sender is separate because SMTP submission has its own connection, its
//     own TLS policy and its own error taxonomy, and because "send" must never
//     be quietly coupled to "file a copy in Sent".
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Kind selects a quirk profile. It never selects a code path the generic IMAP
// implementation could not take: the differences between providers are data,
// not branches, so a new server that behaves like Exchange needs a table entry
// rather than a patch to the sync engine.
type Kind string

const (
	KindGmail     Kind = "gmail"
	KindMicrosoft Kind = "microsoft"
	KindIMAP      Kind = "imap"
)

// ParseKind converts a string, rejecting anything unknown.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindGmail, KindMicrosoft, KindIMAP:
		return k, nil
	default:
		return "", fmt.Errorf("provider: unknown provider %q (want gmail, microsoft or imap)", s)
	}
}

// Role tags a session for logging and for the connection budget.
type Role string

const (
	// RoleIdle is the single connection parked in IDLE on the inbox.
	RoleIdle Role = "idle"
	// RoleSync runs folder passes, one after another.
	RoleSync Role = "sync"
	// RoleInteractive serves the API: body fetches, flag changes, moves. It is
	// opened on demand and closed when it goes quiet, so a person reading mail
	// never waits behind a backfill.
	RoleInteractive Role = "interactive"
)

// Mailbox is the per-account factory.
type Mailbox interface {
	Kind() Kind
	Profile() Profile
	// Open dials and authenticates one connection.
	//
	// ctx bounds the dial and the authentication only; the returned Session
	// outlives it. On an XOAUTH2 refusal the implementation invalidates the
	// token source, refreshes once and retries — an expired access token is
	// the common case and must not look like a revoked grant.
	Open(ctx context.Context, role Role) (Session, error)
	// Sender submits mail for this account. One connection per send,
	// serialised: go-mail's client is a single connection behind a mutex, and
	// Microsoft allows three concurrent submissions per mailbox.
	Sender() Sender
	// Close releases anything the factory holds.
	Close() error
}

// Caps is the subset of server capabilities the sync engine branches on,
// read after authentication because Gmail advertises most of them only then.
type Caps struct {
	CondStore  bool // CONDSTORE: SELECT (CONDSTORE), FETCH CHANGEDSINCE, STORE UNCHANGEDSINCE
	ESearch    bool // ESEARCH: UID SEARCH RETURN (...); Exchange answers BAD, so it is checked, never assumed
	Move       bool // MOVE: UID MOVE, else COPY + STORE \Deleted + UID EXPUNGE
	UIDPlus    bool // UIDPLUS: UID EXPUNGE, APPENDUID, COPYUID
	SpecialUse bool // SPECIAL-USE: folder roles from LIST attributes
	ListStatus bool // LIST-STATUS: counters without selecting every folder
	Idle       bool
	UTF8Accept bool
	// AppendLimit is APPENDLIMIT=n, or zero when the server does not say.
	AppendLimit int64
	// Raw is everything the server advertised, for the debug endpoint and for
	// the fixture an end-to-end run captures.
	Raw []string
}

// Folder is one LIST row.
type Folder struct {
	// Name is what SELECT takes. go-imap decodes modified UTF-7 on the way in
	// and re-encodes it on the way out, so this is UTF-8 and must be stored
	// exactly as it arrived.
	Name       string
	Delim      rune
	Attrs      []imap.MailboxAttr
	Selectable bool
	// Status is filled only when LIST-STATUS was requested and honoured.
	Status *FolderStatus
}

// HasAttr reports whether the folder carries an attribute.
func (f Folder) HasAttr(attr imap.MailboxAttr) bool {
	for _, a := range f.Attrs {
		if a == attr {
			return true
		}
	}
	return false
}

// FolderStatus is what SELECT or STATUS reports.
type FolderStatus struct {
	Name        string
	UIDValidity uint32
	UIDNext     imap.UID
	// NumMessages is EXISTS. Advisory only: it is a hint that something
	// changed, never the number the index reports.
	NumMessages    uint32
	NumUnseen      uint32
	HighestModSeq  uint64 // zero without CONDSTORE
	ReadOnly       bool
	PermanentFlags []imap.Flag
}

// PartInfo is one leaf of the BODYSTRUCTURE walk. Producing these at header
// sync is what lets the API list a message's attachments without downloading
// any of them.
type PartInfo struct {
	// Path is the IMAP section path, 1-based: {1,2} means BODY[1.2].
	Path        []int
	MIMEType    string // lowercase "type/subtype"
	Params      map[string]string
	Encoding    string // Content-Transfer-Encoding, needed to decode the part alone
	Disposition string
	Filename    string
	ContentID   string
	Size        int64 // encoded octets, as advertised
	// IsBody marks a text part that could be the message body.
	IsBody bool
	// IsAttachment marks a part a person would call an attachment.
	IsAttachment bool
}

// PathString renders a part path the way IMAP writes it.
func (p PartInfo) PathString() string { return PathString(p.Path) }

// PathString renders an IMAP part path, e.g. {1,2} as "1.2".
func PathString(path []int) string {
	if len(path) == 0 {
		return ""
	}
	out := make([]byte, 0, len(path)*2)
	for i, n := range path {
		if i > 0 {
			out = append(out, '.')
		}
		out = fmt.Appendf(out, "%d", n)
	}
	return string(out)
}

// Summary is one message's metadata: FETCH (UID FLAGS ENVELOPE INTERNALDATE
// RFC822.SIZE BODYSTRUCTURE [MODSEQ]).
type Summary struct {
	UID          imap.UID
	ModSeq       uint64
	Flags        []imap.Flag
	Envelope     *imap.Envelope
	InternalDate time.Time
	Size         int64
	// Parts is the flattened BODYSTRUCTURE. Nil when the server omitted it.
	Parts []PartInfo
	// References is the References header's ids, bare (no angle brackets),
	// oldest first. ENVELOPE does not carry it, so it is read as a header
	// field alongside; nil when the message has none.
	References []string
}

// FlagUpdate is one FETCH (UID FLAGS [MODSEQ]) row.
type FlagUpdate struct {
	UID    imap.UID
	ModSeq uint64
	Flags  []imap.Flag
}

// FlagOp maps onto imap.StoreFlagsOp.
type FlagOp int

const (
	FlagAdd FlagOp = iota
	FlagDel
	FlagSet
)

// MoveResult reports where the messages landed.
type MoveResult struct {
	// DestUIDValidity is the destination's UIDVALIDITY, from COPYUID; zero
	// when the server did not say.
	DestUIDValidity uint32
	// Mapping is source UID to destination UID, from COPYUID. Nil without
	// UIDPLUS, in which case the caller looks the message up by its
	// Message-ID or leaves it to a pass of the destination, never guessing.
	//
	// Its keys are the source UIDs the server reported and its values the
	// destination UIDs, both exactly. Which value belongs to which key is
	// known only for a mapping of one UID (Paired): RFC 4315 pairs the two
	// sets of COPYUID by position, but go-imap parses each into sorted,
	// merged ranges, so a server that lists the new UIDs in another order —
	// Gmail does — is read as pairing them in ascending order. Paired by
	// position here, a mapping of several UIDs can send one message's row to
	// another message.
	Mapping map[imap.UID]imap.UID
}

// Paired reports whether Mapping says which source UID became which
// destination UID: only a mapping of one UID does. Of several, the caller
// reads the destination UIDs and tells the messages apart by what they are.
func (r MoveResult) Paired() bool { return len(r.Mapping) == 1 }

// AppendResult is APPENDUID, or zero when the server does not support it.
type AppendResult struct {
	UID         imap.UID
	UIDValidity uint32
}

// Part is a fetched body section.
//
// It has already been spooled to a temporary file: the fetch completed and the
// connection was released before this was returned. Handing back a reader
// still bound to the connection would hold an IMAP session — and, with it,
// every sync pass for that account — for as long as the slowest HTTP client
// took to read a thirty megabyte attachment.
type Part struct {
	Info PartInfo
	Body io.ReadSeekCloser // Close removes the temporary file
	Size int64
}

// IdleEventKind is what the connection parked in IDLE saw.
type IdleEventKind int

const (
	// IdleExists is "* n EXISTS": the message count moved.
	IdleExists IdleEventKind = iota
	// IdleExpunge is "* n EXPUNGE". The sequence number is useless to us —
	// mapping it to a UID means trusting a shadow copy of the mailbox — so it
	// only means "run the UID diff".
	IdleExpunge
	// IdleFetch is an unsolicited FETCH, which in practice means flags moved.
	IdleFetch
)

// IdleEvent is a signal, never data. Nothing here is applied to the index:
// these say "look again", and looking is done with UID commands.
type IdleEvent struct {
	Kind        IdleEventKind
	SeqNum      uint32
	NumMessages uint32
	At          time.Time
}

// IdleHandle is a running IDLE command.
type IdleHandle interface {
	// Stop ends the IDLE and waits for the server to acknowledge it.
	// Idempotent.
	Stop() error
}

// Session is one authenticated connection.
//
// Not safe for concurrent use: the caller guarantees one command at a time.
// Every method that names messages issues a UID command — in go-imap the
// dynamic type of the number set is what decides between UIDs and sequence
// numbers, and getting that wrong deletes the wrong message.
//
// Any error that is not a classified server refusal leaves the session dead.
// Close it and open another; there is no recovering a connection whose decoder
// has lost its place.
type Session interface {
	Caps() Caps
	Role() Role

	// ListFolders runs LIST "" "*".
	//
	// It never passes the SPECIAL-USE selector: Exchange Online rejects the
	// LIST-EXTENDED form outright, and the attributes arrive anyway on servers
	// that have them.
	ListFolders(ctx context.Context, withStatus bool) ([]Folder, error)

	// Select opens a folder. When expectUIDValidity is non-zero and the server
	// reports a different one, it returns the status along with
	// ErrUIDValidityChanged, so the caller can resync without another round
	// trip.
	Select(ctx context.Context, name string, readOnly bool, expectUIDValidity uint32) (FolderStatus, error)
	// Selected is the currently selected folder, or nil.
	Selected() *FolderStatus
	// Status queries a folder without selecting it.
	Status(ctx context.Context, name string) (FolderStatus, error)
	// Create makes a folder. Needed on generic servers, where the folder a
	// sent copy belongs in may simply not exist yet.
	Create(ctx context.Context, name string) error

	// UIDs runs UID SEARCH over a range, optionally bounded by INTERNALDATE.
	// A range set rather than a list: the initial window, "everything new" and
	// a windowed diff are all expressible, and a literal list of fifty
	// thousand UIDs would exceed the command line Exchange accepts.
	UIDs(ctx context.Context, set imap.UIDSet, since time.Time) ([]imap.UID, error)
	// UIDCount asks the server to count instead of listing. ESEARCH only;
	// ErrUnsupported otherwise.
	UIDCount(ctx context.Context, set imap.UIDSet) (uint32, error)

	// FetchSummaries streams metadata. changedSince requires CONDSTORE. The
	// command is drained even when fn stops early, because abandoning a FETCH
	// half-read leaves the connection out of step.
	FetchSummaries(ctx context.Context, set imap.UIDSet, changedSince uint64, fn func(Summary) error) error
	// FetchFlags reads flags for a range. Every UID that still exists comes
	// back, so on a server without CONDSTORE one windowed call doubles as
	// expunge detection.
	FetchFlags(ctx context.Context, set imap.UIDSet, changedSince uint64) ([]FlagUpdate, error)
	// FetchHeader reads BODY.PEEK[HEADER].
	FetchHeader(ctx context.Context, uid imap.UID) ([]byte, error)
	// FetchPart reads one section, spooled to disk. Refused above maxBytes.
	FetchPart(ctx context.Context, uid imap.UID, info PartInfo, maxBytes int64) (Part, error)
	// FetchRaw reads the whole message, spooled to disk.
	FetchRaw(ctx context.Context, uid imap.UID, maxBytes int64) (Part, error)

	// StoreFlags changes flags and returns what the server echoed back, so the
	// index is updated from the server's answer rather than from what we hoped
	// would happen.
	//
	// StoreFlags, Move and Copy change the mailbox. Once one has been sent,
	// its answer is returned even when ctx ends while it runs: the server
	// made the change either way, and the caller has to learn what it was. A
	// ctx already done sends nothing. An error wrapping ErrConnClosed means
	// the command may or may not have been carried out.
	StoreFlags(ctx context.Context, set imap.UIDSet, op FlagOp, flags []imap.Flag, unchangedSince uint64) ([]FlagUpdate, error)
	// Move moves messages out of the selected folder, which it opens
	// read-write first if it was examined. Where MOVE is absent it falls back
	// to COPY, STORE \Deleted and UID EXPUNGE of exactly the moved UIDs.
	// Without MOVE and without UIDPLUS it refuses with ErrUnsupported before
	// sending anything: the only expunge left would be a blanket EXPUNGE,
	// which would also destroy messages another client had flagged \Deleted.
	// Once the fallback's COPY is made, the STORE and UID EXPUNGE follow
	// whatever ctx does.
	Move(ctx context.Context, set imap.UIDSet, dest string) (MoveResult, error)
	// Copy copies messages to another folder and leaves them where they are.
	// On Gmail that is adding a label, and the only safe way to take a message
	// out of All Mail again: a MOVE out of it removes the message from the
	// mailbox's every label.
	Copy(ctx context.Context, set imap.UIDSet, dest string) (MoveResult, error)
	// SearchMessageID finds messages in the selected folder by their
	// Message-ID header (bare or bracketed): UID SEARCH HEADER. It is how a
	// move on a server without UIDPLUS, which reports no new UIDs, finds
	// where the message landed.
	SearchMessageID(ctx context.Context, messageID string) ([]imap.UID, error)
	// Append adds a message to a folder.
	Append(ctx context.Context, folder string, r io.Reader, size int64, flags []imap.Flag, t time.Time) (AppendResult, error)

	// Idle parks the connection. Only valid on RoleIdle.
	Idle(ctx context.Context) (IdleHandle, error)
	// Events yields what the idling connection saw. The handlers that feed it
	// run on the client's read goroutine and only ever push into this channel:
	// issuing a command from there deadlocks the connection.
	Events() <-chan IdleEvent
	// Overflowed reports, and clears, whether events were dropped because the
	// consumer was busy. Dropped signals are harmless — the answer is always a
	// full pass — but the caller has to know to run one.
	Overflowed() bool

	// Noop is the keepalive, and the cheapest way to find out that a socket
	// has quietly died.
	Noop(ctx context.Context) error
	Close() error
	// Closed is closed once the connection is: the server ended it, the
	// network dropped it, or this side closed it.
	Closed() <-chan struct{}
	// CloseCause is why the connection closed, once Closed is closed, for a
	// caller that saw it close while no command of its own failed: an error
	// wrapping ErrConnClosed, and ErrServerEnded when the server ended it.
	// A command sent on a closed connection fails with it too. Nil while the
	// connection is open.
	CloseCause() error
}

// Address is a parsed mail address.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Outgoing is a message to submit.
type Outgoing struct {
	From        Address
	To, Cc, Bcc []Address
	ReplyTo     []Address
	Subject     string
	TextBody    string
	HTMLBody    string
	// InReplyTo and References hold bare ids, without angle brackets: the
	// brackets are added in exactly one place, when the message is built.
	InReplyTo  string
	References []string
	// MessageID is required and generated before the first attempt, so a retry
	// carries the same one and a recipient's server can recognise the
	// duplicate.
	MessageID   string
	Date        time.Time
	Attachments []OutgoingAttachment

	// BeforeDial, when set, is asked right before every connection to the
	// submission server: after any wait for the account's other sends, and
	// again before the retry with a refreshed token. An error stops the send
	// there, with nothing dialed, and Send returns it as it is. Who may send
	// can change while a send waits its turn.
	BeforeDial func(ctx context.Context) error
	// KeepCopy asks Send for the message as it was sent (SendResult.Copy),
	// for a Sent folder the provider does not fill by itself.
	KeepCopy bool
}

// OutgoingAttachment is a file to attach. Open is called each time the
// message is written out, and what it returns is streamed and closed: no
// copy of the attachment is held in memory.
type OutgoingAttachment struct {
	Filename    string
	ContentType string
	// ContentID, when set, makes this an inline part a HTML body can reference
	// with cid:.
	ContentID string
	Open      func() (io.ReadCloser, error)
}

// SendResult is what the submission server acknowledged.
type SendResult struct {
	// Copy is the message for the Sent folder when the Outgoing asked to
	// keep one; nil otherwise. The caller discards it when it is done.
	Copy *SentCopy
	// ServerReply is the final 250 line, kept for the audit trail.
	ServerReply string
	SubmittedAt time.Time
}

// SentCopy is a sent message kept for the Sent folder: exactly the bytes that
// went over the wire, behind a Bcc header naming the blind recipients, which
// the wire never carries and a Sent folder must. It is held in a file, not in
// memory.
type SentCopy struct {
	// Size is its length in bytes, what an APPEND announces.
	Size int64
	// Open reads it from its first byte; each call starts over.
	Open func() (io.ReadCloser, error)
	// Discard deletes it. Calling it again does nothing.
	Discard func()
}

// Sender submits one message.
//
// An implementation calls msg.BeforeDial, when set, right before each
// connection it makes, and never connects when it fails: that is where a
// send's authorization is asked for the last time.
type Sender interface {
	Send(ctx context.Context, msg Outgoing) (SendResult, error)
}

// Sentinel errors. Implementations wrap these; callers classify with
// errors.Is, and the send path uses errors.As for RecipientError.
var (
	// ErrAuthFailed is a credential the server refused.
	ErrAuthFailed = errors.New("provider: authentication failed")
	// ErrNeedsReauth is a refresh token that is gone for good: invalid_grant,
	// AADSTS70008 and friends, or an XOAUTH2 401 that survived a refresh. The
	// account stops until a human consents again, so nothing else may return
	// this speculatively.
	ErrNeedsReauth = errors.New("provider: re-authentication required")
	// ErrNotConnected is Microsoft's "BAD User is authenticated but not
	// connected": the credential was fine and the mailbox still refused.
	// Usually IMAP disabled on the mailbox, a token for the wrong resource, or
	// too many sessions. Long backoff; never a re-consent prompt.
	ErrNotConnected = errors.New("provider: authenticated but not connected")
	// ErrTooManyConnections is Gmail's connection cap, which counts every
	// client the person runs, not just this one.
	ErrTooManyConnections = errors.New("provider: too many simultaneous connections")
	// ErrConnClosed is a dead connection: BYE, EOF, a decoder error, a
	// timeout, a dial that failed. Retried like any connection failure; which
	// of these it was is for the log, and ErrServerEnded marks the routine
	// ones.
	ErrConnClosed = errors.New("provider: connection closed")
	// ErrServerEnded is an ErrConnClosed the server ended on its own: a BYE,
	// or the socket closed or reset from its end. Gmail ends an OAuth session
	// after roughly the token's lifetime, and some hosts end every connection
	// after a few hours, so this one is an ordinary event, not a failure. A
	// connection that timed out, could not be dialed or was refused by the
	// address guard, or was dropped because the server sent a response this
	// client cannot parse, is ErrConnClosed without it.
	ErrServerEnded = fmt.Errorf("%w by the server", ErrConnClosed)
	// ErrTemporary is a server saying "later".
	ErrTemporary = errors.New("provider: temporary failure")
	// ErrRateLimited is throttling, ours or theirs.
	ErrRateLimited = errors.New("provider: rate limited")
	// ErrFolderNotFound is a folder that is gone.
	ErrFolderNotFound = errors.New("provider: folder not found")
	// ErrUIDValidityChanged means the folder must be resynced. Exchange does
	// this without anyone asking.
	ErrUIDValidityChanged = errors.New("provider: uidvalidity changed")
	// ErrMessageGone is a UID that no longer exists.
	ErrMessageGone = errors.New("provider: message no longer exists")
	// ErrUnsupported is a capability this server lacks.
	ErrUnsupported = errors.New("provider: capability not supported by server")
	// ErrTooLarge is over SIZE or APPENDLIMIT.
	ErrTooLarge = errors.New("provider: message too large")
	// ErrTerminal is a permanent refusal: the message will never be accepted.
	ErrTerminal = errors.New("provider: permanently rejected")
	// ErrOutcomeUnknown is a submission that failed once the message was on
	// the wire and before the server answered for it: the connection broke,
	// or no answer came. The message may well have been delivered, so it is
	// never retried automatically: Exchange delivers both copies, and only
	// Gmail deduplicates by Message-ID.
	ErrOutcomeUnknown = errors.New("provider: send outcome unknown")

	// ErrAuthUnsupported is a server that will not let the account sign in
	// the way it signs in: no mechanism this client speaks, or SMTP AUTH
	// turned off for the mailbox (Microsoft 365's 535 5.7.139, while IMAP
	// with the same grant works). No refresh or retry changes it, and it says
	// nothing about the grant; an administrator changes it. It is
	// ErrUnsupported.
	ErrAuthUnsupported = fmt.Errorf("%w: the server does not accept this sign-in", ErrUnsupported)
	// ErrInsecure is a server that could not be reached over a verified,
	// encrypted connection: a certificate that failed verification, a port
	// that does not speak TLS, or STARTTLS not offered where it is required.
	// Nothing was authenticated. It is ErrTerminal: trying again does not
	// make the connection secure, and a downgrade must not look like a
	// passing glitch.
	ErrInsecure = fmt.Errorf("%w: no verified encrypted connection", ErrTerminal)
	// ErrAfterData marks a refusal the server answered once it had the whole
	// message: a definite answer — it did not take the message — but one a
	// sender does not retry by itself, since the message was transmitted.
	// It travels with the class of the refusal (ErrTerminal, ErrTooLarge,
	// ErrTemporary, ErrRateLimited), never alone.
	ErrAfterData = errors.New("provider: refused after the message was transmitted")
)

// RecipientError carries the addresses a server rejected. The message was not
// sent to anyone: go-mail abandons the transaction on the first refused RCPT.
type RecipientError struct {
	Rejected []RejectedRecipient
}

// RejectedRecipient is one refused address.
type RejectedRecipient struct {
	Address string
	Code    int
	Message string
}

// Error names no address: an error is what ends up in a log, and the
// addresses are for the caller who wrote them, through Rejected.
func (e *RecipientError) Error() string {
	if len(e.Rejected) == 1 {
		return fmt.Sprintf("provider: a recipient was rejected (%d)", e.Rejected[0].Code)
	}
	return fmt.Sprintf("provider: %d recipients rejected", len(e.Rejected))
}

func (e *RecipientError) Unwrap() error { return ErrTerminal }

// Retryable reports whether the sync engine should back off and try again, as
// opposed to parking the account until a human does something.
//
// It answers for synchronisation only. Per-message send outcomes are
// classified by the send path, which has more to go on: "too large" is a fact
// about one message, not about the account.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNeedsReauth), errors.Is(err, ErrUnsupported), errors.Is(err, ErrTerminal):
		return false
	}
	return true
}

// classes names each sentinel for a log line, most specific first: a refusal
// that survived a refresh is needs_reauth even though it began as an
// authentication failure, and a refused recipient is terminal.
var classes = []struct {
	err  error
	name string
}{
	{ErrNeedsReauth, "needs_reauth"},
	{ErrAuthFailed, "auth_failed"},
	{ErrNotConnected, "not_connected"},
	{ErrTooManyConnections, "too_many_connections"},
	{ErrRateLimited, "rate_limited"},
	{ErrConnClosed, "connection_closed"},
	{ErrFolderNotFound, "folder_not_found"},
	{ErrUIDValidityChanged, "uidvalidity_changed"},
	{ErrMessageGone, "message_gone"},
	{ErrAuthUnsupported, "auth_unsupported"},
	{ErrUnsupported, "unsupported"},
	{ErrTooLarge, "too_large"},
	{ErrOutcomeUnknown, "outcome_unknown"},
	{ErrInsecure, "insecure"},
	{ErrTerminal, "terminal"},
	{ErrTemporary, "temporary"},
}

// ServerReply is what the server answered, word for word, when err carries
// it: the status line of the command it refused, such as
// "imap: NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)". Empty
// otherwise. It is for a log line, beside Class, where the logger masks the
// addresses such lines can name; a caller is never shown it. Without it an
// answer no classifier recognised reaches the log only as "the command
// failed".
func ServerReply(err error) string {
	var reply interface{ ServerReply() string }
	if errors.As(err, &reply) {
		return reply.ServerReply()
	}
	return ""
}

// Class is a short, fixed name for what kind of failure err is, for a log line
// that an operator filters on. It never carries the server's own words; the
// error beside it does, masked by the logger.
func Class(err error) string {
	if err == nil {
		return ""
	}
	for _, c := range classes {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "other"
}
