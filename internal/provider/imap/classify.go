package imap

import (
	"errors"
	"io"
	"net"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/xoauth2"
)

// classify turns whatever a server or a socket produced into one of the
// provider sentinels.
//
// This is where the two providers' very different ideas of "no" are
// reconciled. Gmail answers a stale token with a SASL continuation carrying
// base64 JSON and only then fails the command; Exchange fails outright with a
// tagged NO; and Exchange has a third answer, "authenticated but not
// connected", which means the credential was fine and the mailbox still would
// not open. Treating any of those as the others produces the two worst
// outcomes available: asking a person to re-consent when their token merely
// expired, or retrying forever against a mailbox that will never answer.
func classify(err error, xo *xoauth2.Client) error {
	if err == nil {
		return nil
	}
	if already := asProviderError(err); already != nil {
		return already
	}

	// A refusal Gmail explained in a continuation. The challenge was recorded
	// while the exchange finished; the command's own error is what arrives
	// here. Its status and scope are the message either way: they are what
	// tells an operator an expired token from one that never reached the
	// mailbox, and they name nobody.
	if xo != nil {
		if ch := xo.Challenge(); ch != nil {
			switch {
			case ch.Expired():
				return wrap(provider.ErrAuthFailed, err, ch.Error())
			case ch.Throttled():
				return wrap(provider.ErrRateLimited, err, ch.Error())
			case ch.ServerFault():
				// The server failing, not refusing. Calling it a dead grant
				// would throw away a consent, or park a working account, over
				// the provider's bad minute.
				return wrap(provider.ErrTemporary, err, ch.Error())
			}
			return wrap(provider.ErrNeedsReauth, err, ch.Error())
		}
	}

	// A socket that timed out or failed — a dial refused, by the server or by
	// the address guard, included — is a closed connection, but not one the
	// server ended: never ErrServerEnded, which is logged as routine.
	if netErr := asNetError(err); netErr != nil {
		if netErr.Timeout() {
			return wrap(provider.ErrConnClosed, err, "the connection timed out")
		}
		return wrap(provider.ErrConnClosed, err, "the connection failed")
	}
	// go-imap fails what is pending with io.ErrUnexpectedEOF when its reader
	// stops with no error of its own: the server hung up, or this side
	// closed the socket, which the session reports itself (closedHere).
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return wrap(provider.ErrServerEnded, err, "the server closed the connection")
	}
	if errors.Is(err, net.ErrClosed) {
		return wrap(provider.ErrConnClosed, err, "the connection was closed on this side")
	}

	text := err.Error()
	lower := strings.ToLower(text)

	// Ordered from most specific to least: several of these phrases contain
	// the others, and the first match wins.
	switch {
	case containsAny(lower, "authenticated but not connected"):
		// Exchange: the token was accepted and the mailbox still refused.
		// IMAP disabled on the mailbox, a token minted for the wrong resource,
		// too many sessions, or the long-running consumer-account defect.
		return wrap(provider.ErrNotConnected, err,
			"the server authenticated the account but refused to open the mailbox")
	case containsAny(lower, "too many simultaneous connections", "too many connections"):
		// Gmail counts every client the person runs, not only this one.
		return wrap(provider.ErrTooManyConnections, err, "the account has too many open imap connections")
	case containsAny(lower, "authenticationfailed", "authenticate failed", "invalid credentials",
		"invalid_grant", "login failed", "5.7.8", "5.7.3", "535 "):
		return wrap(provider.ErrAuthFailed, err, "the server rejected the credentials")
	case containsAny(lower, "session expired", "* bye", "connection closed", "connection reset"):
		// Gmail ends an OAuth session at roughly the token's lifetime. An
		// ordinary event, not a failure.
		return wrap(provider.ErrServerEnded, err, "the server ended the session")
	case containsAny(lower, "nonexistent", "[trycreate]", "no such mailbox",
		"unknown mailbox", "mailbox doesn't exist", "mailbox does not exist"):
		// [TRYCREATE] is what a server answers when a COPY or MOVE names a
		// folder that is not there — the same condition as [NONEXISTENT] on a
		// SELECT, reached by a different command.
		return wrap(provider.ErrFolderNotFound, err, "the folder does not exist")
	case containsAny(lower, "[limit]", "rate limit", "throttl", "4.7.0", "421 ", "454 "):
		return wrap(provider.ErrRateLimited, err, "the server is throttling this account")
	case containsAny(lower, askedToRetry...):
		return wrap(provider.ErrTemporary, err, "the server asked us to try again later")
	case containsAny(lower, "[toobig]", "message too large", "5.3.4", "552 "):
		return wrap(provider.ErrTooLarge, err, "the message is larger than the server accepts")
	case containsAny(lower, "[clientbug]", "unknown command", "unknown fetch data item", "syntax error"):
		// The server did not understand what we sent, which for this client
		// means a capability it does not have.
		return wrap(provider.ErrUnsupported, err, "the server does not support that command")
	}

	// Anything the decoder could not follow leaves the connection unusable:
	// go-imap fails every pending command and the reader has lost its place.
	// Not the server ending it: a response this client cannot parse ends the
	// connection every time it comes, and must not pass for routine.
	if strings.Contains(lower, "unsupported response") || strings.Contains(lower, "in literal") {
		return wrap(provider.ErrConnClosed, err, "the server sent a response this client cannot parse")
	}
	return wrap(provider.ErrTemporary, err, "the command failed")
}

// classifyAuth is classify for the authentication step, where a plain refusal
// means the credential and not the connection.
func classifyAuth(err error, xo *xoauth2.Client, shape provider.AuthFailureShape) error {
	if err == nil {
		return nil
	}
	classified := classify(err, xo)

	// Gmail's continuation is the authoritative answer when there is one; the
	// tagged failure that follows says nothing useful.
	if xo != nil && xo.Challenge() != nil {
		return classified
	}
	// Exchange says only "NO AUTHENTICATE failed." A token that has simply
	// expired and one that has been revoked look identical here, so the caller
	// refreshes once and only then gives up — which is why this stays
	// ErrAuthFailed rather than escalating to ErrNeedsReauth.
	//
	// Only a refusal nobody recognised, though. A server that said to come
	// back later meant it: read as a refusal, two of those in a row become a
	// dead grant, and a consent is thrown away or a working account parked
	// because Exchange had a bad minute.
	if shape == provider.AuthFailureTagged && errors.Is(classified, provider.ErrTemporary) &&
		!containsAny(strings.ToLower(err.Error()), askedToRetry...) {
		return wrap(provider.ErrAuthFailed, err, "the server rejected the credentials")
	}
	return classified
}

// askedToRetry is how servers say a failure will pass: Gmail's and Dovecot's
// response codes, and the words Exchange uses when a backend is down
// ("Server Unavailable. 15").
var askedToRetry = []string{"[unavailable]", "[inuse]", "try again", "temporarily", "server unavailable"}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// wrap attaches a sentinel to a cause without letting the server's own text
// reach a caller: banners and NO lines carry addresses, and occasionally a
// token.
func wrap(sentinel, cause error, message string) error {
	return &classifiedError{sentinel: sentinel, cause: cause, message: message}
}

type classifiedError struct {
	sentinel error
	cause    error
	message  string
}

func (e *classifiedError) Error() string { return e.message }

// Unwrap reports both the sentinel and the cause, so errors.Is finds the
// classification and a log can still say what the server said.
func (e *classifiedError) Unwrap() []error { return []error{e.sentinel, e.cause} }

// ServerReply is the server's own status line when the cause is one — a
// tagged NO or BAD, response code and text — for provider.ServerReply.
// Nothing else: a decoder's error can quote the mail it choked on.
func (e *classifiedError) ServerReply() string {
	var status *imap.Error
	if errors.As(e.cause, &status) {
		return status.Error()
	}
	return ""
}

// statusOf is the server's status line in err, as a cause for wrap, or nil.
func statusOf(err error) error {
	var status *imap.Error
	if errors.As(err, &status) {
		return status
	}
	return nil
}

func asProviderError(err error) error {
	for _, sentinel := range []error{
		provider.ErrNeedsReauth, provider.ErrAuthFailed, provider.ErrNotConnected,
		provider.ErrTooManyConnections, provider.ErrConnClosed, provider.ErrRateLimited,
		provider.ErrFolderNotFound, provider.ErrUIDValidityChanged, provider.ErrMessageGone,
		provider.ErrUnsupported, provider.ErrTooLarge, provider.ErrTerminal,
		provider.ErrOutcomeUnknown, provider.ErrTemporary,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return nil
}

func asNetError(err error) net.Error {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr
	}
	return nil
}

// isClosed reports whether a client is past the point of being useful.
func isClosed(c *imapclient.Client) bool {
	select {
	case <-c.Closed():
		return true
	default:
		return false
	}
}
