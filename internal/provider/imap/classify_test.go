package imap

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/netguard"
	"github.com/thehappieco/mailie/internal/provider"
)

// ErrServerEnded is logged as routine, the first time in a row (the sync
// engine's failureLog). Only the server ending the connection may carry it:
// a response go-imap cannot parse ends the connection the same way, every
// time it comes, and hidden as routine it would never be found.

func TestOnlyTheServerEndingAConnectionClassifiesAsRoutine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		routine bool
	}{
		{"the server hung up", io.EOF, true},
		{"what go-imap fails a pending command with when the server hangs up", io.ErrUnexpectedEOF, true},
		{"a BYE", errors.New("imap: BYE Session expired, please login again."), true},
		{"a reset from the server", errors.New("in response: cannot read tag: read tcp 10.0.0.1:993: connection reset by peer"), true},
		{"a response go-imap cannot parse", errors.New(`in response-data: unsupported response type "XYZZY"`), false},
		{"a read that timed out", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"a dial the address guard refused", &net.OpError{Op: "dial", Net: "tcp", Err: netguard.ErrPrivateAddress}, false},
		{"a socket this side closed", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, false},
		{"use of a closed connection", net.ErrClosed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err, nil)
			if !errors.Is(got, provider.ErrConnClosed) {
				t.Fatalf("classify(%v) = %v, want a closed connection", tc.err, got)
			}
			if errors.Is(got, provider.ErrServerEnded) != tc.routine {
				t.Fatalf("classify(%v) = %q; the server ending it: %v, want %v",
					tc.err, got, errors.Is(got, provider.ErrServerEnded), tc.routine)
			}
		})
	}
}

// closedWithin waits for the session's connection to close.
func closedWithin(t *testing.T, s *session) {
	t.Helper()
	select {
	case <-s.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection did not close")
	}
}

func TestAServerHangingUpBetweenCommandsIsTheServerEndingTheConnection(t *testing.T) {
	sess, srv := newScriptedSession(t, answer)
	stillOpen(t, sess)
	if err := sess.CloseCause(); err != nil {
		t.Fatalf("CloseCause of an open connection = %v", err)
	}
	_ = srv.conn.Close()
	closedWithin(t, sess)
	if err := sess.CloseCause(); !errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("CloseCause = %v, want the server ending the connection", err)
	}
	// A command on it is not sent, and fails with why.
	if err := sess.Noop(t.Context()); !errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("NOOP on the closed connection = %v, want the server ending it", err)
	}
}

func TestAResponseTheClientCannotParseIsNotTheServerEndingTheConnection(t *testing.T) {
	// go-imap v2 ends the connection over a response it does not know, and
	// fails every pending command with that.
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		if strings.HasPrefix(command, "NOOP") {
			return "* XYZZY what is this\r\n" + tag + " OK NOOP completed\r\n"
		}
		return answer(tag, command)
	})
	err := sess.Noop(t.Context())
	if !errors.Is(err, provider.ErrConnClosed) || errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("NOOP = %v, want a closed connection the server did not end", err)
	}
	closedWithin(t, sess)
	if cause := sess.CloseCause(); !errors.Is(cause, provider.ErrConnClosed) || errors.Is(cause, provider.ErrServerEnded) {
		t.Fatalf("CloseCause = %v, want a closed connection the server did not end", cause)
	}
}

func TestACommandTheServerNeverAnswersIsNotTheServerEndingTheConnection(t *testing.T) {
	// The operation timer closes the connection, and go-imap fails the
	// command with what it fails one with when the server hangs up.
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		if strings.HasPrefix(command, "NOOP") {
			return ""
		}
		return answer(tag, command)
	})
	err := classify(sess.run(t.Context(), 100*time.Millisecond, func() error { return sess.client.Noop().Wait() }), nil)
	if !errors.Is(err, provider.ErrConnClosed) || errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("an unanswered NOOP = %v, want a closed connection the server did not end", err)
	}
	closedWithin(t, sess)
	if cause := sess.CloseCause(); !errors.Is(cause, provider.ErrConnClosed) || errors.Is(cause, provider.ErrServerEnded) {
		t.Fatalf("CloseCause = %v, want a closed connection the server did not end", cause)
	}
}

func TestASessionClosedOnThisSideIsNotTheServerEndingIt(t *testing.T) {
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		if strings.HasPrefix(command, "LOGOUT") {
			return "* BYE logging out\r\n" + tag + " OK LOGOUT completed\r\n"
		}
		return answer(tag, command)
	})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	closedWithin(t, sess)
	if cause := sess.CloseCause(); !errors.Is(cause, provider.ErrConnClosed) || errors.Is(cause, provider.ErrServerEnded) {
		t.Fatalf("CloseCause = %v, want a connection closed on this side", cause)
	}
}
