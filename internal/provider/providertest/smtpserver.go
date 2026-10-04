package providertest

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/thehappieco/mailie/internal/xoauth2"
)

// SMTPServer is an in-process submission server, go-smtp's, that records what
// it was sent byte for byte and can be told to refuse in each of the ways that
// decide what a send becomes: a recipient refused, for good or for now, a
// sender refused for now, a credential refused, a message refused once it was
// read whole, and a connection lost once the message was already on the wire.
//
// It speaks plain TCP, so the client must be configured for tests
// (provider.Config.AllowInsecureAuth), and it takes any user with any
// password or bearer token unless told to refuse.
type SMTPServer struct {
	// Host and Port are where it listens: 127.0.0.1 and a free port.
	Host string
	Port int

	server   *smtp.Server
	listener net.Listener

	mu sync.Mutex
	// messages is every message accepted, in order.
	messages []SMTPMessage
	// dataCommands counts DATA commands that reached the server, accepted
	// or not: a send that got that far may have been delivered.
	dataCommands int
	auths        []string
	mechanisms   []string
	rejected     map[string]*smtp.SMTPError
	mailFailures []error
	authFailures []error
	dataFailures []error
	dropAfter    int
	onData       func(raw []byte)
}

// Reply is an answer the server can be told to give instead of taking what
// it was sent.
type Reply struct {
	Code     int
	Enhanced [3]int
	Message  string
}

func (r Reply) err() *smtp.SMTPError {
	return &smtp.SMTPError{Code: r.Code, EnhancedCode: smtp.EnhancedCode(r.Enhanced), Message: r.Message}
}

// SMTPMessage is one message the server accepted.
type SMTPMessage struct {
	// Helo is the name the client gave in EHLO/HELO.
	Helo string
	From string
	To   []string
	Raw  []byte
}

// NewSMTPServer starts a server for the duration of the test.
func NewSMTPServer(t testing.TB) *SMTPServer {
	t.Helper()
	//nolint:noctx // a listener makes no outbound connection
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("providertest: listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("providertest: listening on %v", ln.Addr())
	}
	s := &SMTPServer{
		Host: "127.0.0.1", Port: addr.Port, listener: ln, rejected: map[string]*smtp.SMTPError{},
		mechanisms: []string{sasl.Plain, "XOAUTH2"},
	}
	s.server = smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		return &smtpSession{server: s, conn: c}, nil
	}))
	s.server.Domain = "localhost"
	s.server.AllowInsecureAuth = true
	s.server.ReadTimeout = 10 * time.Second
	s.server.WriteTimeout = 10 * time.Second
	s.server.MaxMessageBytes = 64 << 20
	s.server.ErrorLog = log.New(io.Discard, "", 0)
	go func() {
		//nolint:errcheck // Close ends it
		_ = s.server.Serve(ln)
	}()
	t.Cleanup(func() {
		//nolint:errcheck // tearing down a test server
		_ = s.server.Close()
	})
	return s
}

// RejectRecipient makes RCPT TO for address answer 550 5.1.1, as a server does
// for a mailbox that does not exist.
func (s *SMTPServer) RejectRecipient(address string) {
	s.AnswerRecipient(address, Reply{Code: 550, Enhanced: [3]int{5, 1, 1}, Message: "No such user here"})
}

// AnswerRecipient makes RCPT TO for address answer r, every time.
func (s *SMTPServer) AnswerRecipient(address string, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected[strings.ToLower(address)] = r.err()
}

// FailMailFrom makes the next n MAIL FROM commands answer 451 4.3.0: a server
// asking to be tried again later, before anything was transmitted.
func (s *SMTPServer) FailMailFrom(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.mailFailures = append(s.mailFailures, &smtp.SMTPError{
			Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Try again later",
		})
	}
}

// RefuseAuth makes the next n AUTH attempts answer 535 5.7.8.
func (s *SMTPServer) RefuseAuth(n int) {
	s.RefuseAuthWith(n, Reply{Code: 535, Enhanced: [3]int{5, 7, 8}, Message: "Authentication failed"})
}

// RefuseAuthWith makes the next n AUTH attempts answer r.
func (s *SMTPServer) RefuseAuthWith(n int, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.authFailures = append(s.authFailures, r.err())
	}
}

// OfferAuth sets the mechanisms EHLO advertises, among PLAIN, LOGIN and
// XOAUTH2 (which it speaks) and any other name (which it only advertises).
// PLAIN and XOAUTH2 until told otherwise.
func (s *SMTPServer) OfferAuth(mechanisms ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mechanisms = append([]string(nil), mechanisms...)
}

// RefuseData makes the next n messages be read whole and then answered r
// instead of taken: the server says, once it has the message, that it will
// not deliver it. The server keeps nothing of them.
func (s *SMTPServer) RefuseData(n int, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.dataFailures = append(s.dataFailures, r.err())
	}
}

// DropAfterData makes the next n messages be read whole and then lost with
// the connection, before the server answers: the client cannot tell whether
// the message was delivered. The server keeps nothing of them.
func (s *SMTPServer) DropAfterData(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAfter += n
}

// OnData runs fn with each message the server has read, before it answers:
// while fn runs, the client is waiting to hear whether it was taken. Nil
// removes it.
func (s *SMTPServer) OnData(fn func(raw []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onData = fn
}

// Messages returns what the server accepted, oldest first.
func (s *SMTPServer) Messages() []SMTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SMTPMessage(nil), s.messages...)
}

// DataCommands counts the DATA commands that reached the server.
func (s *SMTPServer) DataCommands() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dataCommands
}

// Auths lists the mechanism of every AUTH attempt, in order.
func (s *SMTPServer) Auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

func (s *SMTPServer) authenticate(mechanism string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths = append(s.auths, mechanism)
	if len(s.authFailures) > 0 {
		err := s.authFailures[0]
		s.authFailures = s.authFailures[1:]
		return err
	}
	return nil
}

type smtpSession struct {
	server *SMTPServer
	conn   *smtp.Conn
	from   string
	to     []string
}

func (s *smtpSession) AuthMechanisms() []string {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	return append([]string(nil), s.server.mechanisms...)
}

func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, _, _ string) error { return s.server.authenticate(mech) }), nil
	case sasl.Login:
		return &loginServer{authenticate: func() error { return s.server.authenticate(mech) }}, nil
	case "XOAUTH2":
		return xoauth2.NewServer(func(_, _ string) error { return s.server.authenticate(mech) }), nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

// loginServer is the server side of AUTH LOGIN, which go-sasl has only the
// client of: a user name, a password, each asked for in turn.
type loginServer struct {
	step         int
	authenticate func() error
}

func (l *loginServer) Next(_ []byte) ([]byte, bool, error) {
	l.step++
	switch l.step {
	case 1:
		return []byte("Username:"), false, nil
	case 2:
		return []byte("Password:"), false, nil
	}
	if err := l.authenticate(); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	if len(s.server.mailFailures) > 0 {
		err := s.server.mailFailures[0]
		s.server.mailFailures = s.server.mailFailures[1:]
		return err
	}
	s.from = from
	return nil
}

func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	if err := s.server.rejected[strings.ToLower(to)]; err != nil {
		return err
	}
	s.to = append(s.to, to)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	s.server.mu.Lock()
	s.server.dataCommands++
	hook := s.server.onData
	drop := s.server.dropAfter > 0
	if drop {
		s.server.dropAfter--
	}
	var refuse error
	if !drop && len(s.server.dataFailures) > 0 {
		refuse = s.server.dataFailures[0]
		s.server.dataFailures = s.server.dataFailures[1:]
	}
	s.server.mu.Unlock()
	var raw bytes.Buffer
	if _, err := io.Copy(&raw, r); err != nil {
		return err
	}
	if hook != nil {
		hook(raw.Bytes())
	}
	if drop {
		// Gone before the answer: the client has sent the whole message and
		// will never hear whether it was taken.
		//nolint:errcheck // the point is to break the connection
		_ = s.conn.Conn().Close()
		return errors.New("providertest: connection dropped after DATA")
	}
	if refuse != nil {
		return refuse
	}
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	s.server.messages = append(s.server.messages, SMTPMessage{
		Helo: s.conn.Hostname(), From: s.from, To: append([]string(nil), s.to...), Raw: raw.Bytes(),
	})
	return nil
}

func (s *smtpSession) Reset() { s.from, s.to = "", nil }

func (s *smtpSession) Logout() error { return nil }
