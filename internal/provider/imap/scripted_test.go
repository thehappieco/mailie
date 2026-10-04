package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/thehappieco/mailie/internal/provider"
)

// scriptedServer is the far end of a pipe that answers each command line as
// the test says. It exists for what the in-process server cannot be made to
// do: answer NIL for a body section, or hold a response back until the test
// has cancelled the caller.
type scriptedServer struct {
	conn net.Conn
	// lines receives every command line the client sent, in order.
	lines chan string
}

// newScriptedSession is a session on a pipe whose far end answers with
// reply, which gets each command's tag and the rest of its line and returns
// the server's whole answer, tagged completion included.
func newScriptedSession(t *testing.T, reply func(tag, command string) string) (*session, *scriptedServer) {
	t.Helper()
	near, far := net.Pipe()
	srv := &scriptedServer{conn: far, lines: make(chan string, 64)}
	go srv.serve(reply)
	client := imapclient.New(near, nil)
	sess := &session{
		client: client, role: provider.RoleInteractive, spool: t.TempDir(),
		events: make(chan provider.IdleEvent, idleEventBuffer),
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = far.Close()
	})
	return sess, srv
}

func (s *scriptedServer) serve(reply func(tag, command string) string) {
	if _, err := io.WriteString(s.conn, "* OK [CAPABILITY IMAP4rev1] scripted\r\n"); err != nil {
		return
	}
	r := bufio.NewReader(s.conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.lines <- line
		tag, command, _ := strings.Cut(line, " ")
		if _, err := io.WriteString(s.conn, reply(tag, command)); err != nil {
			return
		}
	}
}

// answer is the usual reply to anything a test does not script: NOOP
// succeeds, anything else is refused.
func answer(tag, command string) string {
	if strings.HasPrefix(command, "NOOP") {
		return tag + " OK NOOP completed\r\n"
	}
	return tag + " BAD not scripted\r\n"
}

// bodyItem is the FETCH response item a command asked for: BODY[1] for
// BODY.PEEK[1], BODY[] for BODY.PEEK[].
func bodyItem(command string) string {
	start := strings.Index(command, "BODY.PEEK[")
	end := strings.Index(command[start:], "]")
	return "BODY[" + command[start+len("BODY.PEEK["):start+end] + "]"
}

func spoolFiles(t *testing.T, s *session) []string {
	t.Helper()
	entries, err := os.ReadDir(s.spool)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func stillOpen(t *testing.T, s *session) {
	t.Helper()
	select {
	case <-s.Closed():
		t.Fatal("the connection was closed")
	default:
	}
	if err := s.Noop(t.Context()); err != nil {
		t.Fatalf("the connection is out of step: NOOP = %v", err)
	}
}

func TestASectionTheServerAnswersWithNILIsGoneAndLeavesNothingSpooled(t *testing.T) {
	// Exchange and Cyrus answer NIL for a section they no longer have — a
	// stale part path, a message being expunged. That is a message that is
	// gone, not a panic that drops the request and leaves its spool file
	// behind, and not an empty section served as an empty original.
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		if strings.HasPrefix(command, "UID FETCH") {
			return "* 1 FETCH (UID 7 " + bodyItem(command) + " NIL)\r\n" + tag + " OK FETCH completed\r\n"
		}
		return answer(tag, command)
	})

	for name, fetch := range map[string]func() (provider.Part, error){
		"FetchPart": func() (provider.Part, error) {
			return sess.FetchPart(t.Context(), 7, provider.PartInfo{Path: []int{1}, MIMEType: "text/plain"}, 1<<20)
		},
		"FetchRaw": func() (provider.Part, error) { return sess.FetchRaw(t.Context(), 7, 1<<20) },
	} {
		var (
			part provider.Part
			err  error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panicked: %v", r)
				}
			}()
			part, err = fetch()
		}()
		if part.Body != nil {
			_ = part.Body.Close()
			t.Errorf("%s handed back a body for a NIL section", name)
		}
		if !errors.Is(err, provider.ErrMessageGone) {
			t.Errorf("%s = %v, want ErrMessageGone", name, err)
		}
		if left := spoolFiles(t, sess); len(left) > 0 {
			t.Errorf("%s left %v in the spool", name, left)
		}
	}
	// The response was read to its end: the shared connection goes on.
	stillOpen(t, sess)
}

func TestACallerThatCancelsDuringAFetchLeavesTheConnectionOpen(t *testing.T) {
	// The console cancels the read of one message when a person opens the
	// next. The FETCH still runs to its end — go-imap cannot abandon it — and
	// a connection whose command completed is in step: closing it would make
	// every following read log in again.
	arrived, release := make(chan struct{}), make(chan struct{})
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		if strings.HasPrefix(command, "UID FETCH") {
			close(arrived)
			<-release
			return "* 1 FETCH (UID 7 BODY[1] {5}\r\nhello)\r\n" + tag + " OK FETCH completed\r\n"
		}
		return answer(tag, command)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		part provider.Part
		err  error
	}
	done := make(chan result, 1)
	go func() {
		part, err := sess.FetchPart(ctx, 7, provider.PartInfo{Path: []int{1}, MIMEType: "text/plain"}, 1<<20)
		done <- result{part, err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the FETCH never reached the server")
	}
	cancel()
	close(release)

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FetchPart did not return")
	}
	if got.part.Body != nil {
		_ = got.part.Body.Close()
		t.Error("a cancelled fetch handed back a body")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("FetchPart = %v, want the cancellation", got.err)
	}
	if left := spoolFiles(t, sess); len(left) > 0 {
		t.Errorf("the cancelled fetch left %v in the spool", left)
	}
	stillOpen(t, sess)
}

func TestACallerThatHasAlreadyGoneSendsNothing(t *testing.T) {
	// Nobody is waiting for the answer, so the command is not worth the
	// server's time, and the connection is left exactly as it was.
	sess, srv := newScriptedSession(t, answer)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := sess.FetchPart(ctx, 7, provider.PartInfo{Path: []int{1}}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchPart = %v, want the cancellation", err)
	}
	if _, err := sess.FetchRaw(ctx, 7, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchRaw = %v, want the cancellation", err)
	}
	if _, err := sess.Select(ctx, "INBOX", true, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select = %v, want the cancellation", err)
	}
	stillOpen(t, sess)
	// The NOOP is the first thing the server heard.
	if first := <-srv.lines; !strings.HasSuffix(first, " NOOP") {
		t.Fatalf("the server received %q before the NOOP", first)
	}
	if left := spoolFiles(t, sess); len(left) > 0 {
		t.Errorf("a fetch that was never sent left %v in the spool", left)
	}
}

func TestACOPYUIDOfSeveralUIDsIsNeverTakenForTheirPairing(t *testing.T) {
	// Gmail answered a move of two messages with their new UIDs listed in
	// the other order. go-imap reads each set of COPYUID sorted ("11,10" as
	// 10:11), so the pairing by position RFC 4315 promises is gone: which
	// UIDs moved and where to is right, which went where is not, and the
	// result must not claim it. One UID is paired exactly.
	sess, _ := newScriptedSession(t, func(tag, command string) string {
		switch {
		case strings.HasPrefix(command, "UID COPY 3:4 "):
			return tag + " OK [COPYUID 77 3:4 11,10] COPY completed\r\n"
		case strings.HasPrefix(command, "UID COPY 5 "):
			return tag + " OK [COPYUID 77 5 12] COPY completed\r\n"
		}
		return answer(tag, command)
	})
	sess.selected = &provider.FolderStatus{Name: "INBOX", UIDValidity: 1}

	res, err := sess.Copy(t.Context(), imap.UIDSetNum(3, 4), "Trash")
	if err != nil {
		t.Fatal(err)
	}
	if res.Paired() {
		t.Fatalf("a COPYUID of two UIDs was taken for their pairing: %v", res.Mapping)
	}
	if res.DestUIDValidity != 77 || len(res.Mapping) != 2 || res.Mapping[3] == 0 || res.Mapping[4] == 0 ||
		res.Mapping[3]+res.Mapping[4] != 21 {
		t.Fatalf("mapping = %v under %d, want UIDs 3 and 4 onto 10 and 11", res.Mapping, res.DestUIDValidity)
	}
	// What go-imap makes of it: the ascending pairing, which is not what
	// the server said. Should go-imap ever keep the server's order, this
	// fails, and Paired may be worth revisiting.
	if res.Mapping[3] != 10 {
		t.Fatalf("mapping = %v: go-imap no longer sorts COPYUID's sets", res.Mapping)
	}

	res, err = sess.Copy(t.Context(), imap.UIDSetNum(5), "Trash")
	if err != nil || !res.Paired() || res.Mapping[5] != 12 {
		t.Fatalf("a COPYUID of one UID = %+v, %v; want 5 paired with 12", res, err)
	}
}
