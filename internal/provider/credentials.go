package provider

import (
	"context"
	"crypto/tls"
	"io"
	"syscall"
)

// TokenSource yields a usable OAuth access token.
//
// internal/account implements it over golang.org/x/oauth2, persisting the
// token whenever the library hands back a new one — Microsoft rotates the
// refresh token on every use, and a daemon that forgets to write the new one
// down works fine until it restarts and then cannot authenticate at all.
type TokenSource interface {
	// Token returns a token that is valid now, refreshing if needed.
	Token(ctx context.Context) (string, error)
	// Invalidate discards the cached access token so the next call refreshes.
	//
	// It exists because a server can reject a token the library still
	// considers valid: clock skew, an early server-side expiry, a grant
	// revoked and restored. Without it the retry after an XOAUTH2 401 would
	// replay the same token, fail again, and park the account for a human to
	// fix something that had already fixed itself.
	Invalidate()
}

// Credentials authenticate one account. Exactly one of Password and Tokens is
// set; which one is decided when the account is added, and Microsoft accepts
// only the second.
type Credentials struct {
	// User is the SASL identity, which on some generic servers is not the
	// address.
	User     string
	Password string
	Tokens   TokenSource
}

// UsesOAuth reports whether this account authenticates with a bearer token.
func (c Credentials) UsesOAuth() bool { return c.Tokens != nil }

// Config is everything needed to reach one account's servers.
type Config struct {
	Kind Kind
	// IMAPAddr is host:port, always implicit TLS. STARTTLS is not offered:
	// both providers serve 993, and a client that can be talked out of TLS is
	// a client that can be talked out of TLS.
	IMAPAddr string
	SMTPHost string
	SMTPPort int
	// SMTPImplicitTLS selects port 465 style TLS from the start, rather than
	// STARTTLS on 587.
	SMTPImplicitTLS bool
	// SMTPHelo is the name given in EHLO: the deployment's public host, so
	// the submission server's Received line names a host that resolves to
	// this one rather than a cloud instance's internal name. Empty leaves
	// the library's default, the machine's host name.
	SMTPHelo string

	Credentials Credentials

	// TLSConfig overrides the default, for tests and for a server with a
	// private certificate authority.
	TLSConfig *tls.Config
	// SpoolDir is where fetched sections are written before they are handed
	// on, and where a message being sent is written out for the wire.
	SpoolDir string
	// DebugWriter tees the raw protocol. It carries the XOAUTH2 line, which is
	// a live bearer token, so configuration refuses it outside development.
	DebugWriter io.Writer
	// AllowInsecureAuth permits authenticating over an unencrypted
	// connection. Tests only: the in-process server speaks plain TCP.
	AllowInsecureAuth bool
	// DialControl vets every address an IMAP or SMTP connection is about to
	// be made to, after the host name has been resolved: it is how accounts
	// are kept off loopback and private networks with no gap between checking
	// a name and dialing it. Nil allows every address.
	DialControl func(network, address string, c syscall.RawConn) error
}
