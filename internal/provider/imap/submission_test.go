package imap_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// fakeSubmission is just enough of an SMTP submission server to see what a
// client authenticated with, over what, and what it sent.
type fakeSubmission struct {
	port int

	mu       sync.Mutex
	auths    []string // the mechanism of every AUTH, with whether it came over TLS
	messages int
}

func (f *fakeSubmission) record(auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, auth)
}

func (f *fakeSubmission) seen() (auths []string, messages int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...), f.messages
}

// How the fake server offers TLS.
type submissionTLS int

const (
	plainText submissionTLS = iota // never
	implicit                       // from the first byte, as on 465
	startTLS                       // after STARTTLS, as on 587
)

// startSubmission listens on 127.0.0.1 and offers TLS with config as mode
// says.
func startSubmission(t *testing.T, mode submissionTLS, config *tls.Config) *fakeSubmission {
	t.Helper()
	var ln net.Listener
	var err error
	if mode == implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", config)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeSubmission{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, mode, config)
		}
	}()
	return f
}

func (f *fakeSubmission) serve(conn net.Conn, mode submissionTLS, config *tls.Config) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	overTLS := mode == implicit
	w := bufio.NewWriter(conn)
	r := textproto.NewReader(bufio.NewReader(conn))
	say := func(lines ...string) bool {
		for _, l := range lines {
			if _, err := w.WriteString(l + "\r\n"); err != nil {
				return false
			}
		}
		return w.Flush() == nil
	}
	if !say("220 localhost ESMTP fake") {
		return
	}
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		verb, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if mode == startTLS && !overTLS {
				say("250-localhost", "250-STARTTLS", "250 8BITMIME")
				continue
			}
			say("250-localhost", "250-AUTH PLAIN XOAUTH2", "250-8BITMIME", "250 SIZE 10485760")
		case "STARTTLS":
			if mode != startTLS || overTLS {
				say("502 5.5.2 not here")
				continue
			}
			say("220 2.0.0 ready")
			secure := tls.Server(conn, config)
			if err := secure.Handshake(); err != nil {
				return
			}
			conn, overTLS = secure, true
			w = bufio.NewWriter(conn)
			r = textproto.NewReader(bufio.NewReader(conn))
		case "AUTH":
			mechanism, initial, _ := strings.Cut(rest, " ")
			f.record(mechanism + " tls=" + strconv.FormatBool(overTLS))
			if initial == "" {
				say("334 ")
				if _, err := r.ReadLine(); err != nil {
					return
				}
			}
			say("235 2.7.0 Authentication successful")
		case "MAIL", "RCPT", "RSET", "NOOP":
			say("250 2.0.0 OK")
		case "DATA":
			say("354 Go ahead")
			if _, err := r.ReadDotBytes(); err != nil {
				return
			}
			f.mu.Lock()
			f.messages++
			f.mu.Unlock()
			say("250 2.0.0 queued")
		case "QUIT":
			say("221 2.0.0 bye")
			return
		default:
			say("502 5.5.2 not here")
		}
	}
}

// selfSigned is a certificate for 127.0.0.1, and the pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake submission"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
func (staticToken) Invalidate()                             {}

// countingControl stands in for netguard.Control: it lets every address
// through and counts the sockets it was asked about.
func countingControl(calls *atomic.Int32) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if _, err := netip.ParseAddrPort(address); err == nil {
			calls.Add(1)
		}
		return nil
	}
}

func submit(t *testing.T, cfg provider.Config) error {
	t.Helper()
	cfg.Kind = provider.KindIMAP
	cfg.IMAPAddr = "127.0.0.1:1"
	cfg.SpoolDir = t.TempDir()
	mb, err := imapprovider.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mb.Sender().Send(t.Context(), provider.Outgoing{
		MessageID: "submission@example.com",
		From:      provider.Address{Email: "person@example.com"},
		To:        []provider.Address{{Email: "someone@example.com"}},
		Subject:   "over TLS",
		TextBody:  "over TLS",
	})
	return err
}

func TestImplicitTLSSubmissionAuthenticatesOverTheDaemonsOwnTLSDial(t *testing.T) {
	// Handing go-mail a dial function hands it the port-465 handshake too.
	// The daemon's dial must do that handshake once, with the configured
	// trust, and go-mail must see the result as encrypted — or Gmail
	// submission breaks on a double wrap, or on a PLAIN refused as
	// unencrypted, while every other test stays green.
	cert, pool := selfSigned(t)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	for _, c := range []struct {
		name        string
		credentials provider.Credentials
		mechanism   string
	}{
		{"password", provider.Credentials{User: "person@example.com", Password: "hunter2"}, "PLAIN"},
		{"xoauth2", provider.Credentials{User: "person@example.com", Tokens: staticToken("an-access-token")}, "XOAUTH2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := startSubmission(t, implicit, serverTLS)
			var dials atomic.Int32
			err := submit(t, provider.Config{
				SMTPHost: "127.0.0.1", SMTPPort: server.port, SMTPImplicitTLS: true,
				// No ServerName: the sender fills it in from SMTPHost.
				TLSConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
				DialControl: countingControl(&dials),
				Credentials: c.credentials,
			})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			auths, messages := server.seen()
			if len(auths) != 1 || auths[0] != c.mechanism+" tls=true" {
				t.Errorf("the server saw AUTH %v, want one %s over TLS", auths, c.mechanism)
			}
			if messages != 1 {
				t.Errorf("the server received %d messages, want 1", messages)
			}
			if n := dials.Load(); n != 1 {
				t.Errorf("the dial control saw %d sockets, want the one", n)
			}
		})
	}

	// Implicit TLS against a server that does not speak it fails the
	// handshake, and no credential is ever sent.
	plain := startSubmission(t, plainText, nil)
	err := submit(t, provider.Config{
		SMTPHost: "127.0.0.1", SMTPPort: plain.port, SMTPImplicitTLS: true,
		TLSConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Credentials: provider.Credentials{User: "person@example.com", Password: "hunter2"},
	})
	if err == nil {
		t.Fatal("implicit TLS against a plain-text server succeeded")
	}
	if !errors.Is(err, provider.ErrInsecure) || errors.Is(err, provider.ErrTemporary) {
		t.Fatalf("implicit TLS against a plain-text server = %v, want ErrInsecure, not a passing failure", err)
	}
	if auths, messages := plain.seen(); len(auths) != 0 || messages != 0 {
		t.Fatalf("a plain-text server was sent AUTH %v and %d messages", auths, messages)
	}
}

func TestSTARTTLSSubmissionVerifiesTheServerWithTheConfiguredTrust(t *testing.T) {
	// The TLS override reached IMAP and not SMTP: a server with a private
	// certificate authority could be read from and never sent through.
	cert, pool := selfSigned(t)
	server := startSubmission(t, startTLS, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	err := submit(t, provider.Config{
		SMTPHost: "127.0.0.1", SMTPPort: server.port,
		TLSConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Credentials: provider.Credentials{User: "person@example.com", Password: "hunter2"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if auths, messages := server.seen(); len(auths) != 1 || auths[0] != "PLAIN tls=true" || messages != 1 {
		t.Fatalf("the server saw AUTH %v and %d messages; want one PLAIN after STARTTLS and one message", auths, messages)
	}

	// Without the override the same certificate is not trusted, and the
	// credential never leaves.
	untrusted := startSubmission(t, startTLS, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	err = submit(t, provider.Config{
		SMTPHost: "127.0.0.1", SMTPPort: untrusted.port,
		Credentials: provider.Credentials{User: "person@example.com", Password: "hunter2"},
	})
	if err == nil {
		t.Fatal("a certificate nobody configured trust for was accepted")
	}
	if auths, _ := untrusted.seen(); len(auths) != 0 {
		t.Fatalf("AUTH %v went to a server whose certificate failed", auths)
	}
}

func TestASubmissionServerThatCannotBeVerifiedOrWillNotEncryptIsNotTriedAgain(t *testing.T) {
	// A certificate that does not verify and a server that stops offering
	// STARTTLS are a configuration, or somebody in the middle. Called
	// temporary, each send would try again for a minute and then say "try
	// later"; it is ErrInsecure, which nothing retries.
	cert, pool := selfSigned(t)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	password := provider.Credentials{User: "person@example.com", Password: "hunter2"}
	for _, c := range []struct {
		name   string
		server *fakeSubmission
		cfg    provider.Config
	}{
		{"a certificate nobody trusts, STARTTLS", startSubmission(t, startTLS, serverTLS), provider.Config{}},
		{"a certificate nobody trusts, implicit TLS", startSubmission(t, implicit, serverTLS),
			provider.Config{SMTPImplicitTLS: true}},
		{"a certificate for another name", startSubmission(t, implicit, serverTLS), provider.Config{
			SMTPImplicitTLS: true, TLSConfig: &tls.Config{RootCAs: pool, ServerName: "smtp.example.org", MinVersion: tls.VersionTLS12},
		}},
		{"no STARTTLS offered", startSubmission(t, plainText, nil), provider.Config{
			TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			cfg.SMTPHost, cfg.SMTPPort, cfg.Credentials = "127.0.0.1", c.server.port, password
			err := submit(t, cfg)
			if !errors.Is(err, provider.ErrInsecure) || errors.Is(err, provider.ErrTemporary) ||
				errors.Is(err, provider.ErrConnClosed) {
				t.Fatalf("send = %v, want ErrInsecure and nothing a caller retries", err)
			}
			if auths, messages := c.server.seen(); len(auths) != 0 || messages != 0 {
				t.Fatalf("the server saw AUTH %v and %d messages", auths, messages)
			}
		})
	}
}
