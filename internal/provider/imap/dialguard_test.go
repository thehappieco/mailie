package imap_test

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/thehappieco/mailie/internal/netguard"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// countingListener accepts on loopback and counts who got through, which is
// the only honest way to prove a connection was never made.
func countingListener(t *testing.T) (port string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, accepted
}

func TestAMailHostThatResolvesPrivatelyIsRefusedAtDialTime(t *testing.T) {
	// "localhost" stands in for a name that resolved to a public address when
	// the account was added and resolves to a private one now: the guard sees
	// only the address the socket is about to use.
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2"})
	_, imapPort, _ := net.SplitHostPort(srv.Addr)
	smtpPort, smtpAccepted := countingListener(t)

	mb, err := imapprovider.New(provider.Config{
		Kind:              provider.KindIMAP,
		IMAPAddr:          net.JoinHostPort("localhost", imapPort),
		SMTPHost:          "localhost",
		SMTPPort:          atoi(t, smtpPort),
		Credentials:       provider.Credentials{User: srv.User, Password: "hunter2"},
		SpoolDir:          t.TempDir(),
		AllowInsecureAuth: true,
		DialControl:       netguard.Control,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = mb.Open(t.Context(), provider.RoleInteractive)
	if !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Fatalf("IMAP to a private address: %v, want ErrPrivateAddress", err)
	}
	if errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("IMAP to a private address: %v, which passes for the server ending a connection", err)
	}
	_, err = mb.Sender().Send(t.Context(), provider.Outgoing{
		MessageID: "guard@example.com",
		From:      provider.Address{Email: srv.User},
		To:        []provider.Address{{Email: "someone@example.com"}},
		Subject:   "never sent",
		TextBody:  "never sent",
	})
	if !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Fatalf("SMTP to a private address: %v, want ErrPrivateAddress", err)
	}
	if n := smtpAccepted.Load(); n != 0 {
		t.Fatalf("the SMTP listener accepted %d connections the guard refused", n)
	}

	// The same mailbox without the guard reaches the server, so it was the
	// guard that refused and not something else about the setup.
	open, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: net.JoinHostPort("localhost", imapPort),
		Credentials: provider.Credentials{User: srv.User, Password: "hunter2"},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := open.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("without the guard: %v", err)
	}
	_ = sess.Close()
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a port: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
