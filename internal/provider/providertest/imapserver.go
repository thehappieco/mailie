// Package providertest runs a real IMAP server in the test process.
//
// The point is that the authentication path is exercised for real. The
// in-memory server speaks XOAUTH2 through the same mechanism implementation
// the client uses, so a test can mint a token, present it, have it refused,
// refresh, and present it again — the whole sequence that decides whether an
// account recovers on its own or stops and asks a person to re-consent —
// without touching a provider or a network.
//
// What it cannot do is CONDSTORE, QRESYNC or SPECIAL-USE: those are missing
// from the server package itself, not merely unimplemented here. The UID-only
// path, which is the one Exchange Online uses, is fully covered; the CONDSTORE
// path belongs to the integration tier against Dovecot.
package providertest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"

	"github.com/thehappieco/mailie/internal/xoauth2"
)

// IMAPServer is an in-process IMAP server.
type IMAPServer struct {
	Addr string
	User string

	// anyUser and gmailRefusals are IMAPOptions.AnyUser and
	// IMAPOptions.GmailRefusals.
	anyUser       bool
	gmailRefusals bool
	// unavailable is set by Unavailable.
	unavailable bool
	// onLogin is set by OnLogin.
	onLogin func()

	mem      *imapmemserver.Server
	memUser  *imapmemserver.User
	server   *imapserver.Server
	listener net.Listener

	mu     sync.Mutex
	tokens map[string]bool // access token -> accepted
	// authAttempts records what was presented, so a test can assert that a
	// refresh actually happened rather than that the retry merely worked.
	authAttempts []string

	// idlers are the sessions in IDLE, woken by changed after anything that
	// may have queued an update for them: every command a client completes,
	// and every helper here that writes to the store.
	idleMu sync.Mutex
	idlers map[chan struct{}]struct{}
}

// IMAPOptions configure the server.
type IMAPOptions struct {
	// User is the account name. Defaults to a plausible address.
	User string
	// Password enables LOGIN for generic-IMAP tests.
	Password string
	// Caps overrides the advertised capability set. The default is a
	// deliberately modest IMAP4rev1 server plus the extensions Exchange
	// Online actually has, so the fallback paths are what run by default.
	Caps imap.CapSet
	// Debug tees the protocol to the test log.
	Debug io.Writer
	// AnyUser accepts XOAUTH2 for whatever user name presents an accepted
	// token, so one server can stand in for a provider behind many accounts.
	// The token is still checked.
	AnyUser bool
	// GmailRefusals refuses XOAUTH2 the way Gmail does: a continuation
	// carrying base64 JSON — status 400 for a user the token was not issued
	// to, 401 for a token that is not accepted, and the scope Gmail wants —
	// and only after the client answers it, a tagged NO. Without it, a
	// refusal is Exchange's plain "AUTHENTICATE failed."
	GmailRefusals bool
}

// GmailScope is the scope a Gmail-shaped refusal names.
const GmailScope = "https://mail.google.com/"

// MicrosoftCaps is what Exchange Online advertises: no CONDSTORE, no ESEARCH,
// no SPECIAL-USE. Tests that want the slower path get it by default.
func MicrosoftCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1:   {},
		imap.CapUIDPlus:     {},
		imap.CapMove:        {},
		imap.CapIdle:        {},
		imap.CapNamespace:   {},
		imap.CapLiteralPlus: {},
	}
}

// RichCaps adds the extensions Gmail has, apart from the ones the in-process
// server cannot implement.
func RichCaps() imap.CapSet {
	caps := MicrosoftCaps()
	caps[imap.CapESearch] = struct{}{}
	caps[imap.CapListStatus] = struct{}{}
	caps[imap.CapSpecialUse] = struct{}{}
	return caps
}

// NewIMAPServer starts a server on loopback and stops it when the test ends.
func NewIMAPServer(t *testing.T, opts IMAPOptions) *IMAPServer {
	t.Helper()

	user := opts.User
	if user == "" {
		user = "person@example.com"
	}
	password := opts.Password
	if password == "" {
		password = "hunter2"
	}
	caps := opts.Caps
	if caps == nil {
		caps = MicrosoftCaps()
	}

	memUser := imapmemserver.NewUser(user, password)
	// The in-memory server starts with no mailboxes at all, including the one
	// every client assumes exists.
	if err := memUser.Create("INBOX", nil); err != nil {
		t.Fatalf("providertest: create INBOX: %v", err)
	}
	mem := imapmemserver.New()
	mem.AddUser(memUser)

	s := &IMAPServer{
		User: user, mem: mem, memUser: memUser, tokens: map[string]bool{},
		anyUser: opts.AnyUser, gmailRefusals: opts.GmailRefusals,
		idlers: map[chan struct{}]struct{}{},
	}
	s.server = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			// The concrete session type is embedded, not the interface: the
			// server discovers NAMESPACE, MOVE and the rest through optional
			// interfaces, and a wrapper that embedded imapserver.Session
			// would hide every one of them. The server then advertises
			// capabilities its session cannot serve and panics on the first
			// command that uses one.
			return &saslSession{
				UserSession: imapmemserver.NewUserSession(memUser),
				owner:       s,
				user:        user,
				password:    password,
			}, nil, nil
		},
		Caps: caps,
		// The in-process server speaks plain TCP; the client is told to allow
		// it explicitly, and only in tests.
		InsecureAuth: true,
		Logger:       testLogger{t},
		DebugWriter:  opts.Debug,
	})

	//nolint:noctx // a listener makes no outbound connection
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("providertest: listen: %v", err)
	}
	s.listener = ln
	s.Addr = ln.Addr().String()

	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("providertest: serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		//nolint:errcheck // tearing down a test server
		_ = s.server.Close()
		//nolint:errcheck // tearing down a test server
		_ = ln.Close()
	})
	return s
}

// AcceptToken makes an access token valid.
func (s *IMAPServer) AcceptToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = true
}

// RejectToken makes an access token invalid, the way a provider does when one
// expires.
func (s *IMAPServer) RejectToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = false
}

// AuthAttempts is every token presented, in order.
func (s *IMAPServer) AuthAttempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authAttempts...)
}

// Unavailable makes every XOAUTH2 attempt fail the way a provider whose
// backend is down answers, until it is called with false: Exchange's tagged
// "NO Server Unavailable. 15", or, with GmailRefusals, a continuation whose
// status is 503. Google documents only 400 and 401 there; the 503 is the
// fault a client must not mistake for a refused token.
func (s *IMAPServer) Unavailable(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = on
}

// OnLogin runs fn on every password login the server accepts, before it
// answers: what a test changes while a client waits on its login, as the
// world may change while a real server takes seconds to answer. It runs on
// the server's goroutine, so fn reports its failures by other means than
// t.Fatal. Nil removes it.
func (s *IMAPServer) OnLogin(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onLogin = fn
}

// loggedIn runs the hook OnLogin set, if any.
func (s *IMAPServer) loggedIn() {
	s.mu.Lock()
	fn := s.onLogin
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *IMAPServer) checkToken(user, token string) error {
	if reason := s.refusal(user, token); reason != nil {
		if reason.ServerFault() {
			// Exchange Online's words when a mailbox backend is down, with
			// no response code: the client has only the text to go on.
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Server Unavailable. 15"}
		}
		// The wording, trailing full stop included, is what Exchange Online
		// actually sends, so the client's classifier is tested against the
		// real shape rather than a tidied-up one. An *imap.Error, because the
		// server sends any other error as "Internal server error" and the
		// wording would never reach the wire.
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "AUTHENTICATE failed."}
	}
	return nil
}

// refusal records an attempt and says why it is refused, as the JSON Gmail
// would send, or nil when it is accepted.
func (s *IMAPServer) refusal(user, token string) *xoauth2.Challenge {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authAttempts = append(s.authAttempts, token)
	switch {
	case s.unavailable:
		return &xoauth2.Challenge{Status: "503", Schemes: "Bearer", Scope: GmailScope}
	case !s.anyUser && user != s.User:
		return &xoauth2.Challenge{Status: "400", Schemes: "Bearer", Scope: GmailScope}
	case !s.tokens[token]:
		return &xoauth2.Challenge{Status: "401", Schemes: "Bearer", Scope: GmailScope}
	}
	return nil
}

// CreateMailbox adds a folder.
func (s *IMAPServer) CreateMailbox(t *testing.T, name string) {
	t.Helper()
	if err := s.memUser.Create(name, nil); err != nil {
		t.Fatalf("providertest: create %q: %v", name, err)
	}
}

// Append puts a message in a folder without going through a client, so a test
// can simulate mail arriving while the client under test is idling.
func (s *IMAPServer) Append(t *testing.T, mailbox string, message string, flags []imap.Flag, when time.Time) imap.UID {
	t.Helper()
	if when.IsZero() {
		when = time.Now()
	}
	data, err := s.memUser.Append(mailbox, literal{strings.NewReader(message), int64(len(message))},
		&imap.AppendOptions{Flags: flags, Time: when})
	if err != nil {
		t.Fatalf("providertest: append to %q: %v", mailbox, err)
	}
	s.changed()
	return data.UID
}

// listen registers a session that is about to idle. It must come before the
// session's first poll: a change is then either queued before that poll, which
// sends it, or wakes the session after it.
func (s *IMAPServer) listen() chan struct{} {
	wake := make(chan struct{}, 1)
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	s.idlers[wake] = struct{}{}
	return wake
}

func (s *IMAPServer) unlisten(wake chan struct{}) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	delete(s.idlers, wake)
}

// changed wakes every session in IDLE to poll for what the change queued for
// it. A wake already pending is enough: the poll it causes sends everything.
func (s *IMAPServer) changed() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	for wake := range s.idlers {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// literal adapts a reader to what APPEND wants.
type literal struct {
	io.Reader
	size int64
}

func (l literal) Size() int64 { return l.size }

// saslSession adds XOAUTH2 to the in-memory server's session.
//
// The embedded pointer is the pattern the memory server documents: it supplies
// everything except Login, which the wrapper provides.
type saslSession struct {
	*imapmemserver.UserSession
	owner    *IMAPServer
	user     string
	password string
}

var _ imapserver.SessionSASL = (*saslSession)(nil)

// Login is the password path, for generic-IMAP tests.
func (s *saslSession) Login(username, password string) error {
	if username != s.user || password != s.password {
		// A tagged NO with the code and text a real server sends, so the
		// server's own words cross the wire and a test can check they stop
		// at the provider boundary.
		return &imap.Error{
			Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "LOGIN failed.",
		}
	}
	s.owner.loggedIn()
	return nil
}

// Poll is called by go-imap's server after every command that succeeds, on
// every connection, which makes it where a change one client made reaches the
// others that are idling.
func (s *saslSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	defer s.owner.changed()
	return s.UserSession.Poll(w, allowExpunge)
}

// Idle replaces the memory server's, which can withhold an update a real
// server would send. go-imap's server answers "+ idling" — the moment the
// client's Idle returns — and only then starts the goroutine that listens for
// updates, and that listener never sends what was queued before it started.
// Mail appended in between waits for the client's next command, which a client
// that only idles never sends; on a loaded machine the goroutine starts late
// often enough to fail a test now and then. Here the session listens first and
// polls second, so nothing queued during IDLE stays unsent.
func (s *saslSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	wake := s.owner.listen()
	defer s.owner.unlisten(wake)
	for {
		// The embedded Poll, not this session's own: that one wakes every
		// idler, this one included, and the loop would never rest.
		if err := s.UserSession.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-wake:
		case <-stop:
			return nil
		}
	}
}

func (s *saslSession) AuthenticateMechanisms() []string {
	return []string{xoauth2.Name, sasl.Plain}
}

func (s *saslSession) Authenticate(mech string) (sasl.Server, error) {
	switch mech {
	case xoauth2.Name:
		if s.owner.gmailRefusals {
			return &gmailServer{owner: s.owner}, nil
		}
		return xoauth2.NewServer(s.owner.checkToken), nil
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.Login(username, password)
		}), nil
	default:
		return nil, fmt.Errorf("unsupported mechanism %q", mech)
	}
}

// gmailServer is the server half of XOAUTH2 as Gmail plays it. A refusal is
// explained first, in a continuation the client has to answer, and only then
// failed — the order that decides whether a client's SASL exchange finishes
// cleanly or leaves the connection mid-command.
type gmailServer struct {
	owner *IMAPServer
	// refused is the failure owed once the client has answered the
	// explanation.
	refused error
	done    bool
}

func (g *gmailServer) Next(response []byte) ([]byte, bool, error) {
	if g.refused != nil {
		err := g.refused
		g.refused, g.done = nil, true
		return nil, false, err
	}
	if g.done {
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
	if response == nil {
		// No initial response: ask for one.
		return []byte{}, false, nil
	}
	user, token, err := xoauth2.Decode(response)
	if err != nil {
		g.done = true
		return nil, false, err
	}
	reason := g.owner.refusal(user, token)
	if reason == nil {
		g.done = true
		return nil, true, nil
	}
	payload, err := json.Marshal(reason)
	if err != nil {
		g.done = true
		return nil, false, err
	}
	// Gmail's own tagged line for every refusal.
	g.refused = &imap.Error{
		Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed,
		Text: "Invalid credentials (Failure)",
	}
	if reason.ServerFault() {
		g.refused = &imap.Error{
			Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable,
			Text: "Temporary System Problem. Try again later (Failure)",
		}
	}
	return payload, false, nil
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf("imapserver: "+format, args...) }
