package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	messagemail "github.com/emersion/go-message/mail"
	messagetextproto "github.com/emersion/go-message/textproto"
	gomail "github.com/wneessen/go-mail"
	gomailsmtp "github.com/wneessen/go-mail/smtp"

	"github.com/thehappieco/mailie/internal/provider"
)

const sendTimeout = 60 * time.Second

// maxWriting is how many messages the process writes out for the wire at
// once. go-mail encodes each part of a message whole, in memory, before
// writing it — a buffer that grows to about twice the encoded part — so what
// bounds the memory of many sends in flight is how many are being written at
// the same moment. Writing one out to the spool takes well under a second;
// the minute a send may spend on the wire streams from that file and holds
// nothing.
const maxWriting = 2

// writing admits the messages being written out, process-wide.
var writing = make(chan struct{}, maxWriting)

// Sender submits mail over SMTP.
//
// One connection per send, serialised per account. go-mail's client is a
// single connection behind a mutex, so nothing is gained by holding one open,
// and Exchange Online caps concurrent submissions at three per mailbox — a
// limit this shape makes impossible to exceed rather than merely unlikely.
type Sender struct {
	cfg     provider.Config
	profile provider.Profile
	mu      sync.Mutex
}

var _ provider.Sender = (*Sender)(nil)

// Sender returns the account's submitter: always the same one, so the
// account's sends are serialised however many callers hold it.
func (m *Mailbox) Sender() provider.Sender { return m.sender }

// Send submits one message.
//
// The message is written out once, into the spool, and that file is what
// goes over the wire and what the copy in Sent is made of: the bytes a
// recipient gets and the bytes filed are the same bytes, and neither is held
// in memory while the send waits for its turn or talks to a slow server.
//
// The classification is the part that matters. Each step of the transaction
// fails in its own way, and the step decides whether a retry is safe: a
// refusal at MAIL FROM, at RCPT TO or of the DATA command means nothing was
// transmitted; a connection that breaks while the message is on the wire, or
// before the server answers for it, means the server may well have accepted
// it and simply failed to say so. Retrying that is how somebody's recipient
// gets the same mail twice — Exchange delivers both copies, and only Gmail
// deduplicates by Message-ID — so it is reported as an unknown outcome and
// reconciled later against the Sent folder instead. A server that did answer
// the message, with a refusal, did not take it: that is a failure
// (ErrAfterData), never unknown, and never retried here either.
//
// A server that took the message is a sent message, whatever goes wrong
// after: a QUIT that fails once the 250 has arrived changes nothing about the
// delivery, and reporting it as an error would invite a retry.
//
// An OAuth account whose token the server refused gets one more attempt with
// a freshly refreshed token, here and nowhere else: an access token that aged
// out is the common case. A second refusal is still ErrAuthFailed, not
// ErrNeedsReauth. A submission server refuses a grant the IMAP server still
// takes when SMTP AUTH is off for the mailbox, so whether the grant is dead is
// for IMAP to say; taking the account's reading away because it cannot send
// would be the wrong answer to the wrong question.
//
// out.BeforeDial is asked right before each connection, after the wait for
// the account's other sends: a send queued behind a slow one is asked again
// when its turn comes, not only when it was accepted.
func (s *Sender) Send(ctx context.Context, out provider.Outgoing) (provider.SendResult, error) {
	if out.MessageID == "" {
		return provider.SendResult{}, errors.New("imap: refusing to send without a message id generated in advance")
	}
	msg, err := BuildMessage(out)
	if err != nil {
		return provider.SendResult{}, err
	}
	wire, err := s.writeOut(ctx, msg)
	if err != nil {
		return provider.SendResult{}, err
	}
	// Removed on every way out but the one that hands it over as the copy
	// for Sent: a spool file nobody holds is a copy of somebody's mail.
	handedOver := false
	defer func() {
		if !handedOver {
			wire.remove()
		}
	}()
	if limit := s.profile.SMTPMaxSize; limit > 0 && wire.size > limit {
		return provider.SendResult{}, fmt.Errorf("%w: %d bytes, this provider accepts %d",
			provider.ErrTooLarge, wire.size, limit)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := beforeDial(ctx, out); err != nil {
		return provider.SendResult{}, err
	}
	reply, err := s.submit(ctx, out, wire, false)
	if err != nil && errors.Is(err, provider.ErrAuthFailed) && s.cfg.Credentials.UsesOAuth() {
		if err := beforeDial(ctx, out); err != nil {
			return provider.SendResult{}, err
		}
		reply, err = s.submit(ctx, out, wire, true)
		if err != nil && errors.Is(err, provider.ErrAuthFailed) {
			err = wrap(provider.ErrAuthFailed, err, "the server rejected a freshly refreshed access token")
		}
	}
	if err != nil {
		return provider.SendResult{}, err
	}
	res := provider.SendResult{ServerReply: reply, SubmittedAt: time.Now().UTC()}
	if out.KeepCopy {
		// The message went. A copy that cannot be made is a missing copy,
		// which the caller sees as a nil Copy, and not a failed send.
		if sent, err := wire.sentCopy(out.Bcc); err == nil {
			res.Copy, handedOver = sent, true
		}
	}
	return res, nil
}

// beforeDial asks out.BeforeDial, when there is one.
func beforeDial(ctx context.Context, out provider.Outgoing) error {
	if out.BeforeDial == nil {
		return nil
	}
	return out.BeforeDial(ctx)
}

// submit makes one connection and one attempt: dial, TLS, authenticate, and
// one mail transaction. forceRefresh discards the cached access token first.
// It returns the server's answer to the message.
func (s *Sender) submit(ctx context.Context, out provider.Outgoing, wire *wireCopy, forceRefresh bool) (string, error) {
	if forceRefresh {
		s.cfg.Credentials.Tokens.Invalidate()
	}
	// Opened before anything is dialed: once DATA is under way there is no
	// good way left to fail.
	body, err := wire.open()
	if err != nil {
		return "", err
	}
	//nolint:errcheck // a file only read
	defer func() { _ = body.Close() }()

	var conn net.Conn
	client, err := s.client(ctx, func(c net.Conn) { conn = c })
	if err != nil {
		return "", err
	}
	dialCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	c, err := client.DialToSMTPClientWithContext(dialCtx)
	if err != nil {
		if conn != nil {
			// go-mail leaves the socket open when STARTTLS or AUTH fails.
			//nolint:errcheck // abandoning a connection that failed to set up
			_ = conn.Close()
		}
		return "", classifySetup(err)
	}
	defer func() {
		if !c.HasConnection() {
			return
		}
		if err := c.Quit(); err != nil {
			//nolint:errcheck // the goodbye failed; the socket goes anyway
			_ = c.Close()
		}
	}()
	return transact(c, out, body)
}

// transact runs one mail transaction on an authenticated connection and
// classifies a failure by the step that failed.
func transact(c *gomailsmtp.Client, out provider.Outgoing, body io.Reader) (string, error) {
	if err := c.UpdateDeadline(sendTimeout); err != nil {
		return "", classify(err, nil)
	}
	if err := c.Mail(envelopeAddress(out.From.Email)); err != nil {
		return "", beforeData(err, "the server refused the sender")
	}
	var (
		refused []provider.RejectedRecipient
		notNow  error
	)
	for _, to := range recipients(out) {
		err := c.Rcpt(envelopeAddress(to))
		reply := replyOf(err)
		switch {
		case err == nil:
		case reply == nil:
			return "", beforeData(err, "the connection failed while naming the recipients")
		case reply.Code >= 500:
			refused = append(refused, provider.RejectedRecipient{
				Address: to, Code: reply.Code, Message: enhancedCode(reply),
			})
		case notNow == nil:
			// Not now rather than not ever: too many recipients in one
			// transaction (452), greylisting (450, 451). Nobody was refused,
			// and nothing was sent.
			notNow = err
		}
	}
	switch {
	case len(refused) > 0:
		return "", &provider.RecipientError{Rejected: refused}
	case notNow != nil:
		return "", beforeData(notNow, "the server did not take a recipient")
	}

	w, err := c.Data()
	if err != nil {
		// The DATA command refused, or the connection lost before the
		// server said to go ahead: not a byte of the message has left.
		return "", beforeData(err, "the server refused to take the message")
	}
	if _, err := io.Copy(w, body); err != nil {
		// Part of the message is on the wire. Ending it with the final dot
		// would deliver it cut short, and a QUIT would be read as more of
		// it, so the connection is dropped instead.
		//nolint:errcheck // dropping the connection is the point
		_ = c.Close()
		return "", wrap(provider.ErrOutcomeUnknown, err, "the connection failed while the message was being sent")
	}
	if err := w.Close(); err != nil {
		reply := replyOf(err)
		if reply == nil {
			//nolint:errcheck // nothing more can be said on it
			_ = c.Close()
			return "", wrap(provider.ErrOutcomeUnknown, err, "no answer came once the message was sent")
		}
		return "", afterData(reply)
	}
	answer := ""
	if dc, ok := w.(*gomailsmtp.DataCloser); ok {
		answer = dc.ServerResponse()
	}
	return answer, nil
}

// recipients are the envelope's: to, cc and bcc, in order.
func recipients(out provider.Outgoing) []string {
	list := make([]string, 0, len(out.To)+len(out.Cc)+len(out.Bcc))
	for _, group := range [][]provider.Address{out.To, out.Cc, out.Bcc} {
		for _, a := range group {
			list = append(list, a.Email)
		}
	}
	return list
}

// envelopeAddress is an address as MAIL FROM and RCPT TO carry it: in angle
// brackets, without a name, quoted where it needs to be.
func envelopeAddress(address string) string {
	return (&mail.Address{Address: address}).String()
}

func (s *Sender) client(ctx context.Context, opened func(net.Conn)) (*gomail.Client, error) {
	creds := s.cfg.Credentials
	dial := s.dialer()
	options := []gomail.Option{
		gomail.WithPort(s.cfg.SMTPPort),
		gomail.WithTimeout(sendTimeout),
		gomail.WithUsername(creds.User),
		gomail.WithDialContextFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if conn != nil {
				opened(conn)
			}
			return conn, err
		}),
	}
	if s.cfg.SMTPHelo != "" {
		options = append(options, gomail.WithHELO(s.cfg.SMTPHelo))
	}

	switch {
	case s.cfg.AllowInsecureAuth:
		// Tests only. go-mail does not require an encrypted channel for
		// XOAUTH2, so without this branch being explicit and narrow it would
		// be possible to put a bearer token on the wire in the clear.
		options = append(options, gomail.WithTLSPolicy(gomail.NoTLS))
	case s.cfg.SMTPImplicitTLS:
		options = append(options, gomail.WithSSL(), gomail.WithTLSConfig(s.tlsConfig()))
	default:
		// STARTTLS is go-mail's own upgrade, so it needs the same settings
		// the implicit-TLS dial below uses, or an override that reaches one
		// would not reach the other.
		options = append(options, gomail.WithTLSPolicy(gomail.TLSMandatory), gomail.WithTLSConfig(s.tlsConfig()))
	}

	if creds.UsesOAuth() {
		token, err := creds.Tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		// Set explicitly: XOAUTH2 is excluded from go-mail's mechanism
		// auto-discovery, so leaving it to negotiate silently falls back to
		// something the server will refuse.
		options = append(options, gomail.WithSMTPAuth(gomail.SMTPAuthXOAUTH2), gomail.WithPassword(token))
	} else {
		options = append(options, gomail.WithSMTPAuthCustom(&passwordAuth{
			user: creds.User, password: creds.Password, host: s.cfg.SMTPHost, insecure: s.cfg.AllowInsecureAuth,
		}))
	}

	client, err := gomail.NewClient(s.cfg.SMTPHost, options...)
	if err != nil {
		return nil, fmt.Errorf("imap: configure smtp client: %w", err)
	}
	return client, nil
}

// errNoPasswordMechanism is a server that offers no way to sign in with a
// password that passwordAuth speaks.
var errNoPasswordMechanism = errors.New("imap: the server offers neither AUTH PLAIN nor AUTH LOGIN")

// passwordAuth signs in with a password: by PLAIN when the server offers it,
// and by LOGIN when that is all it offers — Exchange on premises, and Office
// 365 with an app password, advertise LOGIN without PLAIN.
//
// Nothing else, and not go-mail's auto-discovery: go-mail cannot see that a
// connection this daemon dialed itself is encrypted, and discovering over
// what it takes for plain text would rule out both. Either mechanism sends
// the password only over TLS (in tests, when insecure says so, without).
type passwordAuth struct {
	user, password, host string
	insecure             bool
	chosen               gomailsmtp.Auth
}

func (a *passwordAuth) Start(server *gomailsmtp.ServerInfo) (string, []byte, error) {
	switch {
	case offers(server.Auth, "PLAIN"):
		a.chosen = gomailsmtp.PlainAuth("", a.user, a.password, a.host, a.insecure)
	case offers(server.Auth, "LOGIN"):
		a.chosen = gomailsmtp.LoginAuth(a.user, a.password, a.host, a.insecure)
	default:
		return "", nil, errNoPasswordMechanism
	}
	return a.chosen.Start(server)
}

func (a *passwordAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	return a.chosen.Next(fromServer, more)
}

// offers reports whether a server's AUTH line names a mechanism.
func offers(advertised []string, mechanism string) bool {
	for _, m := range advertised {
		if strings.EqualFold(m, mechanism) {
			return true
		}
	}
	return false
}

// dialer is how the SMTP connection is made: always this function, never
// go-mail's own, so the address check in DialControl applies to submission
// exactly as it does to IMAP.
//
// Handing go-mail a dial function also hands it the TLS handshake for port
// 465: it only wraps the connection itself when it dials. So the implicit-TLS
// case dials through a tls.Dialer here, with the settings go-mail would have
// used, and go-mail recognises the *tls.Conn it gets back as encrypted.
// STARTTLS on 587 is unchanged: go-mail upgrades the plain connection itself.
func (s *Sender) dialer() gomail.DialContextFunc {
	plain := &net.Dialer{Timeout: dialTimeout, Control: s.cfg.DialControl}
	if !s.cfg.SMTPImplicitTLS || s.cfg.AllowInsecureAuth {
		return plain.DialContext
	}
	secure := &tls.Dialer{NetDialer: plain, Config: s.tlsConfig()}
	return secure.DialContext
}

// tlsConfig is what submission verifies the server with, implicit TLS or
// STARTTLS alike: the configured override when there is one, as IMAP takes it,
// and in every case a server name to check and nothing older than TLS 1.2.
func (s *Sender) tlsConfig() *tls.Config {
	if s.cfg.TLSConfig == nil {
		return &tls.Config{ServerName: s.cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	}
	cfg := s.cfg.TLSConfig.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = s.cfg.SMTPHost
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

// wireCopy is a message as it goes on the wire, written out once into the
// spool.
type wireCopy struct {
	path string
	size int64
	once sync.Once
}

// writeOut writes msg into the spool, 0600, as the bytes DATA carries.
func (s *Sender) writeOut(ctx context.Context, msg *gomail.Msg) (*wireCopy, error) {
	select {
	case writing <- struct{}{}:
	case <-ctx.Done():
		return nil, wrap(provider.ErrTemporary, ctx.Err(), "too many messages are being written out at once")
	}
	defer func() { <-writing }()

	dir := s.cfg.SpoolDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("imap: create spool directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "send-message-*")
	if err != nil {
		return nil, fmt.Errorf("imap: create spool file: %w", err)
	}
	w := &wireCopy{path: file.Name()}
	buffered := bufio.NewWriterSize(file, 64<<10)
	n, err := msg.WriteTo(buffered)
	if err == nil {
		err = buffered.Flush()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		w.remove()
		return nil, fmt.Errorf("imap: write the message out: %w", err)
	}
	w.size = n
	return w, nil
}

// open reads the message from its first byte.
func (w *wireCopy) open() (*os.File, error) {
	//nolint:gosec // G304: a file writeOut created, never a name from outside
	f, err := os.Open(w.path)
	if err != nil {
		return nil, fmt.Errorf("imap: read the message written out: %w", err)
	}
	return f, nil
}

// remove deletes the file; again is nothing.
func (w *wireCopy) remove() {
	w.once.Do(func() {
		//nolint:errcheck // a file already gone is what was wanted
		_ = os.Remove(w.path)
	})
}

// sentCopy hands the file over as the copy for Sent, behind a Bcc header
// naming bcc. The wire never carries one — go-mail leaves it out, as it must
// — but the sender's own copy has to say who was sent it blind, or nobody
// can ever find out; Gmail and Exchange keep it in the copies they file.
func (w *wireCopy) sentCopy(bcc []provider.Address) (*provider.SentCopy, error) {
	var head bytes.Buffer
	if len(bcc) > 0 {
		list := make([]*mail.Address, 0, len(bcc))
		for _, a := range bcc {
			list = append(list, mailAddress(a))
		}
		var h messagemail.Header
		h.SetAddressList("Bcc", list)
		if err := messagetextproto.WriteHeader(&head, h.Header.Header); err != nil {
			return nil, fmt.Errorf("imap: write the bcc header: %w", err)
		}
		// Without the blank line that ends a header block: the message's
		// own header goes on right after.
		head.Truncate(head.Len() - len("\r\n"))
	}
	prefix := head.Bytes()
	return &provider.SentCopy{
		Size: int64(len(prefix)) + w.size,
		Open: func() (io.ReadCloser, error) {
			f, err := w.open()
			if err != nil {
				return nil, err
			}
			return &prefixed{Reader: io.MultiReader(bytes.NewReader(prefix), f), file: f}, nil
		},
		Discard: w.remove,
	}, nil
}

// prefixed reads a header in front of a file and closes the file.
type prefixed struct {
	io.Reader
	file *os.File
}

func (p *prefixed) Close() error { return p.file.Close() }

// BuildMessage turns an Outgoing into a go-mail message.
//
// Exported because the draft path renders a preview from the same builder: a
// preview produced by different code than the send would be a preview of
// something else.
//
// Attachments are not read here. Each is opened when the message is written
// out, streamed through the encoder and closed, so building a message holds
// nothing of them.
func BuildMessage(out provider.Outgoing) (*gomail.Msg, error) {
	// No User-Agent naming the library: it says nothing the recipient needs
	// and fingerprints the sender.
	msg := gomail.NewMsg(gomail.WithCharset(gomail.CharsetUTF8), gomail.WithNoDefaultUserAgent())

	// Addresses go in as parsed values, never as text for go-mail to parse
	// again: a display name with a comma or a quote in it would otherwise
	// come back as two addresses, or none. The service has already checked
	// each one; errors here name no address, since they can reach a log.
	if out.From.Email == "" {
		return nil, errors.New("imap: no sender address")
	}
	msg.SetAddrHeaderFromMailAddress(gomail.HeaderFrom, mailAddress(out.From))
	for _, group := range []struct {
		header gomail.AddrHeader
		addrs  []provider.Address
	}{
		{gomail.HeaderTo, out.To}, {gomail.HeaderCc, out.Cc}, {gomail.HeaderBcc, out.Bcc},
		{gomail.HeaderReplyTo, out.ReplyTo},
	} {
		if len(group.addrs) == 0 {
			continue
		}
		list := make([]*mail.Address, 0, len(group.addrs))
		for _, a := range group.addrs {
			list = append(list, mailAddress(a))
		}
		msg.SetAddrHeaderFromMailAddress(group.header, list...)
	}

	msg.Subject(out.Subject)
	// The brackets go on here and nowhere else: everything upstream stores
	// bare ids, so there is exactly one place a doubled pair could appear.
	msg.SetMessageIDWithValue(out.MessageID)
	if out.Date.IsZero() {
		msg.SetDate()
	} else {
		msg.SetDateWithValue(out.Date)
	}

	if out.InReplyTo != "" {
		msg.SetGenHeader(gomail.HeaderInReplyTo, bracket(out.InReplyTo))
	}
	if len(out.References) > 0 {
		// One value, space-joined. Passing several values to SetGenHeader
		// joins them with commas, which is not what References is, and Gmail
		// threads by this header.
		refs := make([]string, 0, len(out.References))
		for _, r := range out.References {
			refs = append(refs, bracket(r))
		}
		msg.SetGenHeader(gomail.HeaderReferences, strings.Join(refs, " "))
	}

	switch {
	case out.TextBody != "" && out.HTMLBody != "":
		msg.SetBodyString(gomail.TypeTextPlain, out.TextBody)
		msg.AddAlternativeString(gomail.TypeTextHTML, out.HTMLBody)
	case out.HTMLBody != "":
		msg.SetBodyString(gomail.TypeTextHTML, out.HTMLBody)
	default:
		msg.SetBodyString(gomail.TypeTextPlain, out.TextBody)
	}

	for _, att := range out.Attachments {
		file := &gomail.File{Name: att.Filename, Header: textproto.MIMEHeader{}, Writer: streamed(att.Open)}
		if att.ContentType != "" {
			gomail.WithFileContentType(gomail.ContentType(att.ContentType))(file)
		}
		if att.ContentID != "" {
			// Verbatim: go-mail does not add the angle brackets, and a HTML
			// body references this exact string with cid:.
			gomail.WithFileContentID(att.ContentID)(file)
			msg.SetEmbeds(append(msg.GetEmbeds(), file))
		} else {
			msg.SetAttachments(append(msg.GetAttachments(), file))
		}
	}
	return msg, nil
}

// streamed is an attachment's body as go-mail writes it: opened each time
// the message is written, read through and closed.
func streamed(open func() (io.ReadCloser, error)) func(io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		r, err := open()
		if err != nil {
			return 0, fmt.Errorf("imap: open an attachment: %w", err)
		}
		n, err := io.Copy(w, r)
		if cerr := r.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("imap: close an attachment: %w", cerr)
		}
		return n, err
	}
}

func mailAddress(a provider.Address) *mail.Address {
	return &mail.Address{Name: a.Name, Address: a.Email}
}

// bracket adds the angle brackets a message identifier wears in a header,
// tolerating a value that already has them.
func bracket(id string) string {
	return "<" + strings.Trim(id, "<> ") + ">"
}

// classifySetup maps a failure to connect, secure the connection or sign in
// onto the provider vocabulary. Nothing of the message was sent.
func classifySetup(err error) error {
	var (
		verify    *tls.CertificateVerificationError
		unknownCA x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		notTLS    tls.RecordHeaderError
	)
	lower := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &verify), errors.As(err, &unknownCA), errors.As(err, &hostname), errors.As(err, &invalid),
		errors.As(err, &notTLS):
		// A certificate that does not verify, or a port that does not
		// speak TLS: a configuration, or somebody in the middle. Neither
		// passes by waiting, and the credential never left.
		return wrap(provider.ErrInsecure, err, "the server could not be verified over TLS")
	case errors.Is(err, gomailsmtp.ErrUnencrypted), errors.Is(err, gomailsmtp.ErrWrongHostname),
		strings.Contains(lower, "does not support starttls"):
		return wrap(provider.ErrInsecure, err, "the server would not encrypt the connection")
	case errors.Is(err, errNoPasswordMechanism), errors.Is(err, gomail.ErrXOauth2AuthNotSupported),
		strings.Contains(lower, "does not support smtp auth"):
		return wrap(provider.ErrAuthUnsupported, err, "the server offers no sign-in this account can use")
	}
	reply := replyOf(err)
	switch {
	case reply == nil:
		return classify(err, nil)
	case smtpAuthDisabled(reply):
		return wrap(provider.ErrAuthUnsupported, err, "the server has SMTP AUTH turned off for this mailbox")
	case reply.Code == 535 || reply.Code == 534:
		return wrap(provider.ErrAuthFailed, err, "the server rejected the credentials")
	}
	return classifyReply(reply, err, "the server refused the connection")
}

// smtpAuthDisabled is Microsoft 365 saying SMTP AUTH is off for the tenant or
// the mailbox: 535 5.7.139, "SmtpClientAuthentication is disabled". The
// credential is fine — IMAP takes the same grant — and refreshing it changes
// nothing.
func smtpAuthDisabled(reply *textproto.Error) bool {
	return enhancedCode(reply) == "5.7.139" ||
		strings.Contains(strings.ToLower(reply.Msg), "smtpclientauthentication is disabled")
}

// beforeData classifies a failure before any of the message was sent, when
// trying again cannot deliver it twice: the server's answer says whether to,
// and a connection that broke is tried again like any other.
func beforeData(err error, what string) error {
	reply := replyOf(err)
	if reply == nil {
		return classify(err, nil)
	}
	return classifyReply(reply, err, what)
}

// afterData classifies the server's answer to a message it read whole and
// did not take. RFC 5321 is plain that a 4yz or 5yz there means the message
// was not accepted, so this is a failure and not an unknown outcome — with
// ErrAfterData, because it was transmitted all the same, and nothing retries
// that by itself.
func afterData(reply *textproto.Error) error {
	return classifyReply(reply, fmt.Errorf("%w: %w", provider.ErrAfterData, reply), "the server refused the message")
}

// classifyReply maps an SMTP reply onto the provider vocabulary. err is what
// the reply came in, kept for the log; what says which step it refused.
func classifyReply(reply *textproto.Error, err error, what string) error {
	enhanced := enhancedCode(reply)
	switch {
	case tooLarge(reply):
		return wrap(provider.ErrTooLarge, err, "the message is larger than the server accepts")
	case reply.Code == 421 || reply.Code == 454 || strings.HasPrefix(enhanced, "4.7."):
		return wrap(provider.ErrRateLimited, err, what+": the server is throttling this account")
	case reply.Code >= 400 && reply.Code < 500:
		return wrap(provider.ErrTemporary, err, what+" for now")
	}
	return wrap(provider.ErrTerminal, err, what)
}

// tooLarge is a refusal for size: the enhanced codes that say so, or a bare
// 552. Not every 552: Gmail refuses a blocked attachment with 552 5.7.0,
// which is about the content.
func tooLarge(reply *textproto.Error) bool {
	switch enhancedCode(reply) {
	case "5.3.4", "5.2.3":
		return true
	case "":
		return reply.Code == 552
	}
	return false
}

// replyOf is the server's reply inside err, when err is one.
func replyOf(err error) *textproto.Error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return reply
	}
	return nil
}

var enhancedPattern = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}$`)

// enhancedCode is the RFC 3463 status code a reply starts with, or "".
func enhancedCode(reply *textproto.Error) string {
	first, _, _ := strings.Cut(strings.TrimSpace(reply.Msg), " ")
	if enhancedPattern.MatchString(first) {
		return first
	}
	return ""
}
