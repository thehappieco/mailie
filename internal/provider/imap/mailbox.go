package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/xoauth2"
)

const dialTimeout = 20 * time.Second

// Mailbox is one account's connection factory.
type Mailbox struct {
	cfg     provider.Config
	profile provider.Profile
	// sender is the account's one submitter: its lock is what serialises
	// the account's sends.
	sender *Sender
}

var _ provider.Mailbox = (*Mailbox)(nil)

// New builds a mailbox for an account.
func New(cfg provider.Config) (*Mailbox, error) {
	if cfg.IMAPAddr == "" {
		return nil, errors.New("imap: no server address")
	}
	if cfg.Credentials.User == "" {
		return nil, errors.New("imap: no login user")
	}
	if cfg.Credentials.Password == "" && cfg.Credentials.Tokens == nil {
		return nil, errors.New("imap: neither a password nor a token source")
	}
	profile := provider.ProfileFor(cfg.Kind)
	return &Mailbox{cfg: cfg, profile: profile, sender: &Sender{cfg: cfg, profile: profile}}, nil
}

func (m *Mailbox) Kind() provider.Kind       { return m.cfg.Kind }
func (m *Mailbox) Profile() provider.Profile { return m.profile }
func (m *Mailbox) Close() error              { return nil }

// Open dials and authenticates one connection.
//
// The retry is the interesting part. A server that refuses a bearer token
// usually means the token expired, not that the grant is gone — and the two
// are indistinguishable from the outside on Exchange, which says only
// "AUTHENTICATE failed". So the first refusal invalidates the cached access
// token, takes a fresh one and tries once more. Only if that also fails, or
// if the refresh itself came back with invalid_grant, does the account stop
// and ask for consent. Getting this backwards means telling people to
// re-authorise their mailbox every time a token ages out.
func (m *Mailbox) Open(ctx context.Context, role provider.Role) (provider.Session, error) {
	sess, err := m.open(ctx, role, false)
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, provider.ErrAuthFailed) || !m.cfg.Credentials.UsesOAuth() {
		return nil, err
	}

	retried, retryErr := m.open(ctx, role, true)
	if retryErr == nil {
		return retried, nil
	}
	// A second refusal with a token minted seconds ago is a grant that is
	// really gone. What the server said the second time goes along, for the
	// log — its words and its status line, but not its sentinel, so nothing
	// downstream can read this as the retryable ErrAuthFailed it grew out of.
	if errors.Is(retryErr, provider.ErrAuthFailed) {
		return nil, wrap(provider.ErrNeedsReauth, statusOf(retryErr),
			provider.ErrNeedsReauth.Error()+": the server rejected a freshly refreshed access token: "+retryErr.Error())
	}
	return nil, retryErr
}

func (m *Mailbox) open(ctx context.Context, role provider.Role, forceRefresh bool) (provider.Session, error) {
	events := make(chan provider.IdleEvent, idleEventBuffer)
	sess := &session{
		role:    role,
		profile: m.profile,
		spool:   m.cfg.SpoolDir,
		events:  events,
	}

	options := &imapclient.Options{
		TLSConfig:   m.tlsConfig(),
		DebugWriter: m.cfg.DebugWriter,
		// Without this, a subject encoded in anything but UTF-8 arrives as
		// mojibake and stays that way in the index.
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
		// Control runs on the resolved address, so a host that passed the
		// check when the account was added and resolves somewhere private now
		// is still refused.
		Dialer: &net.Dialer{Timeout: dialTimeout, Control: m.cfg.DialControl},
	}
	if role == provider.RoleIdle {
		options.UnilateralDataHandler = sess.unilateralHandler()
	}

	client, err := m.dial(ctx, options)
	if err != nil {
		return nil, classify(err, nil)
	}
	sess.client = client

	if err := m.authenticate(ctx, sess, forceRefresh); err != nil {
		// Never reuse a connection that failed to authenticate: it may be
		// sitting mid-exchange, and the next command would be read as a SASL
		// response.
		//nolint:errcheck // the authentication error is the one to report
		_ = client.Close()
		return nil, err
	}

	sess.caps = capsFrom(client.Caps())

	// UTF8=ACCEPT, and only that, when the server offers it.
	//
	// Not a nicety. go-imap decides to send mailbox names as raw UTF-8 the
	// moment a server *advertises* IMAP4rev2 — but a server that advertises
	// it keeps decoding names as modified UTF-7 until the client enables
	// something. Skip this and every folder whose name carries an accent
	// becomes unreachable, which is most of them: both providers localise
	// their default folder names, and people name their own folders in their
	// own language.
	//
	// UTF8=ACCEPT rather than IMAP4rev2, deliberately. Enabling IMAP4rev2
	// makes the server report expunges as VANISHED, a response this client's
	// decoder does not understand: it ends the connection and fails every
	// pending command, so a single MOVE would break the account. UTF8=ACCEPT
	// fixes the names and leaves expunge reporting alone.
	//
	// Nothing else is ever enabled. QRESYNC is the other route to VANISHED.
	if client.Caps().Has(imap.CapUTF8Accept) {
		if _, err := client.Enable(imap.CapUTF8Accept).Wait(); err != nil {
			//nolint:errcheck // the enable error is the one to report
			_ = client.Close()
			return nil, classify(err, nil)
		}
	}
	return sess, nil
}

func (m *Mailbox) tlsConfig() *tls.Config {
	if m.cfg.TLSConfig != nil {
		return m.cfg.TLSConfig
	}
	host, _, err := net.SplitHostPort(m.cfg.IMAPAddr)
	if err != nil {
		host = m.cfg.IMAPAddr
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

func (m *Mailbox) dial(ctx context.Context, options *imapclient.Options) (*imapclient.Client, error) {
	type result struct {
		client *imapclient.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		if m.cfg.AllowInsecureAuth {
			// Tests only: the in-process server speaks plain TCP.
			client, err := imapclient.DialInsecure(m.cfg.IMAPAddr, options)
			ch <- result{client, err}
			return
		}
		client, err := imapclient.DialTLS(m.cfg.IMAPAddr, options)
		ch <- result{client, err}
	}()

	select {
	case r := <-ch:
		return r.client, r.err
	case <-ctx.Done():
		// The dial goroutine will finish and close whatever it opened.
		go func() {
			//nolint:errcheck // cleaning up a dial nobody is waiting for
			if r := <-ch; r.client != nil {
				_ = r.client.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *Mailbox) authenticate(ctx context.Context, sess *session, forceRefresh bool) error {
	creds := m.cfg.Credentials

	if !creds.UsesOAuth() {
		err := sess.run(ctx, opTimeout, func() error {
			return sess.client.Login(creds.User, creds.Password).Wait()
		})
		return classifyAuth(err, nil, m.profile.AuthFailureShape)
	}

	if forceRefresh {
		creds.Tokens.Invalidate()
	}
	token, err := creds.Tokens.Token(ctx)
	if err != nil {
		// The token source classifies its own failures: invalid_grant and the
		// AADSTS codes arrive here already meaning "needs re-authorisation".
		return err
	}

	xo := xoauth2.NewClient(creds.User, token)
	authErr := sess.run(ctx, opTimeout, func() error { return sess.client.Authenticate(xo) })
	return classifyAuth(authErr, xo, m.profile.AuthFailureShape)
}

// unilateralHandler feeds the idle session's event channel.
//
// Every one of these runs on the client's read goroutine, and go-imap says so:
// the handler blocks the client while it runs. Issuing a command from here
// deadlocks the connection, and doing real work here stalls every response the
// connection is waiting for. So they push a signal and return — and when the
// channel is full they drop it and set a flag, because every signal means the
// same thing and the answer to losing one is the pass the flag will trigger.
func (s *session) unilateralHandler() *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Expunge: func(seqNum uint32) {
			// The sequence number is deliberately not translated to a UID:
			// doing so would mean trusting a local shadow of the mailbox's
			// ordering. It only means "run the UID diff".
			s.push(provider.IdleEvent{Kind: provider.IdleExpunge, SeqNum: seqNum, At: time.Now()})
		},
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages == nil {
				return
			}
			s.push(provider.IdleEvent{
				Kind: provider.IdleExists, NumMessages: *data.NumMessages, At: time.Now(),
			})
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			// This one is invoked in its own goroutine, unlike the two above,
			// and it carries a stream that must be drained before the client
			// can move on.
			if _, err := msg.Collect(); err != nil {
				return
			}
			s.push(provider.IdleEvent{Kind: provider.IdleFetch, SeqNum: msg.SeqNum, At: time.Now()})
		},
	}
}

func (s *session) push(ev provider.IdleEvent) {
	select {
	case s.events <- ev:
	default:
		s.overflow.Store(true)
	}
}

// capsFrom reads the capabilities the sync engine branches on.
//
// Read after authentication, because Gmail advertises almost all of these only
// once logged in, and checked rather than assumed per provider: an account on
// an unexpected server should degrade to the slower path, not fail.
func capsFrom(set imap.CapSet) provider.Caps {
	out := provider.Caps{
		CondStore:  set.Has(imap.CapCondStore),
		ESearch:    set.Has(imap.CapESearch),
		Move:       set.Has(imap.CapMove),
		UIDPlus:    set.Has(imap.CapUIDPlus),
		SpecialUse: set.Has(imap.CapSpecialUse),
		ListStatus: set.Has(imap.CapListStatus),
		Idle:       set.Has(imap.CapIdle),
		UTF8Accept: set.Has(imap.CapUTF8Accept),
	}
	for cap := range set {
		name := string(cap)
		out.Raw = append(out.Raw, name)
		if limit, ok := strings.CutPrefix(name, "APPENDLIMIT="); ok {
			var n int64
			if _, err := fmt.Sscanf(limit, "%d", &n); err == nil {
				out.AppendLimit = n
			}
		}
	}
	return out
}
