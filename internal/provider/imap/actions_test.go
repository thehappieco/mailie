package imap_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// transcript records what a connection sends, for asserting on the wire.
type transcript struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *transcript) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// sent lists the commands the client sent, tag stripped, in order.
func (w *transcript) sent() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, line := range strings.Split(w.buf.String(), "\r\n") {
		tag, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(tag, "T") {
			continue
		}
		out = append(out, rest)
	}
	return out
}

func (w *transcript) count(prefix string) int {
	n := 0
	for _, c := range w.sent() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// cancelOn is a transcript that cancels a context the moment a command
// naming one verb goes out: a caller that leaves while the command is on the
// wire, before the server's answer is back.
type cancelOn struct {
	transcript
	verb string

	armed  sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (w *cancelOn) arm(cancel context.CancelFunc) {
	w.armed.Lock()
	defer w.armed.Unlock()
	w.cancel, w.fired = cancel, false
}

func (w *cancelOn) Write(p []byte) (int, error) {
	w.armed.Lock()
	if w.cancel != nil && bytes.Contains(p, []byte(" "+w.verb+" ")) {
		w.cancel()
		w.cancel, w.fired = nil, true
	}
	w.armed.Unlock()
	return w.transcript.Write(p)
}

func (w *cancelOn) didFire() bool {
	w.armed.Lock()
	defer w.armed.Unlock()
	return w.fired
}

// wireSession opens a password session on an in-process server with caps,
// recording what it sends.
func wireSession(t *testing.T, caps goimap.CapSet) (*providertest.IMAPServer, provider.Session, *transcript) {
	t.Helper()
	wire := &transcript{}
	srv, sess := wireSessionTo(t, caps, wire)
	return srv, sess, wire
}

// wireSessionTo is wireSession with what the connection sends written to w.
func wireSessionTo(t *testing.T, caps goimap.CapSet, wire io.Writer) (*providertest.IMAPServer, provider.Session) {
	t.Helper()
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2", Caps: caps})
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: srv.Addr,
		Credentials: provider.Credentials{User: srv.User, Password: "hunter2"},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true, DebugWriter: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := mb.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return srv, sess
}

// withoutMove is a server with UIDPLUS and no MOVE.
func withoutMove() goimap.CapSet {
	caps := providertest.MicrosoftCaps()
	delete(caps, goimap.CapMove)
	return caps
}

func message(id string) string {
	return "From: Ana <ana@example.com>\r\nTo: Person <person@example.com>\r\nSubject: " + id + "\r\n" +
		"Message-ID: <" + id + "@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
}

func TestAMoveWithoutMOVEExpungesOnlyTheMessagesItMoved(t *testing.T) {
	// COPY, STORE \Deleted and UID EXPUNGE of the moved UID: a plain EXPUNGE
	// would also take a message another client marked for deletion and has
	// not expunged yet, and that message is somebody else's decision.
	srv, sess, wire := wireSession(t, withoutMove())
	srv.CreateMailbox(t, "Trash")
	moved := srv.Append(t, "INBOX", message("moved"), nil, time.Time{})
	marked := srv.Append(t, "INBOX", message("marked"), []goimap.Flag{goimap.FlagDeleted}, time.Time{})
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	res, err := sess.Move(t.Context(), goimap.UIDSetNum(moved), "Trash")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(res.Mapping) != 1 || res.Mapping[moved] == 0 {
		t.Errorf("mapping = %v, want the new UID from COPYUID", res.Mapping)
	}
	for _, c := range wire.sent() {
		if strings.HasPrefix(c, "EXPUNGE") {
			t.Fatalf("a plain EXPUNGE was sent: %q", c)
		}
	}
	if wire.count("UID EXPUNGE "+goimap.UIDSetNum(moved).String()) != 1 {
		t.Errorf("no UID EXPUNGE of exactly the moved UID; sent %q", wire.sent())
	}
	left, err := sess.UIDs(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != marked {
		t.Fatalf("the inbox holds %v after the move, want only the message another client marked (%d)", left, marked)
	}
}

func TestWithoutMOVEOrUIDPLUSAMoveIsRefusedBeforeAnythingIsSent(t *testing.T) {
	caps := withoutMove()
	delete(caps, goimap.CapUIDPlus)
	srv, sess, wire := wireSession(t, caps)
	srv.CreateMailbox(t, "Trash")
	uid := srv.Append(t, "INBOX", message("kept"), nil, time.Time{})
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	before := len(wire.sent())

	_, err := sess.Move(t.Context(), goimap.UIDSetNum(uid), "Trash")
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Move = %v, want ErrUnsupported", err)
	}
	if after := wire.sent(); len(after) != before {
		t.Fatalf("a refused move still sent %q", after[before:])
	}
	flags, err := sess.FetchFlags(t.Context(), goimap.UIDSetNum(uid), 0)
	if err != nil || len(flags) != 1 || containsFlag(flags[0].Flags, `\deleted`) {
		t.Fatalf("the message after a refused move: %+v, %v", flags, err)
	}
}

func TestAFolderOpenForWritingIsNotSelectedAgainForEachWrite(t *testing.T) {
	// The session remembers which folder is open and how: writes into a
	// folder already open read-write send no SELECT, and a write into one
	// opened with EXAMINE sends one SELECT under the UIDVALIDITY it had.
	srv, sess, wire := wireSession(t, providertest.MicrosoftCaps())
	srv.Append(t, "INBOX", message("a"), nil, time.Time{})
	st, err := sess.Select(t.Context(), "INBOX", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd,
			[]goimap.Flag{goimap.FlagSeen}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := wire.count("SELECT "); n != 1 {
		t.Errorf("%d SELECTs for three writes after an EXAMINE, want 1: %q", n, wire.sent())
	}
	if sel := sess.Selected(); sel == nil || sel.ReadOnly || sel.UIDValidity != st.UIDValidity {
		t.Errorf("selected = %+v", sel)
	}
}

func TestAFailedSelectLeavesNoFolderForAWriteToLandIn(t *testing.T) {
	// A SELECT that fails leaves nothing selected on the server. A write
	// that trusted the folder opened before would be refused by the server
	// at best and, after a reconnect, land somewhere else at worst.
	srv, sess, wire := wireSession(t, providertest.MicrosoftCaps())
	srv.Append(t, "INBOX", message("a"), nil, time.Time{})
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Select(t.Context(), "Nowhere", false, 0); !errors.Is(err, provider.ErrFolderNotFound) {
		t.Fatalf("Select of a missing folder = %v", err)
	}
	if sess.Selected() != nil {
		t.Fatal("a failed SELECT left the previous folder selected")
	}
	if _, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd,
		[]goimap.Flag{goimap.FlagSeen}, 0); !errors.Is(err, provider.ErrFolderNotFound) {
		t.Fatalf("a write with no folder selected = %v", err)
	}
	if n := wire.count("UID STORE"); n != 0 {
		t.Fatalf("a STORE went out with no folder selected: %q", wire.sent())
	}
}

func TestACopyLeavesTheMessageAndItIsFoundByItsMessageID(t *testing.T) {
	// COPY is how a message leaves Gmail's All Mail (adding a label); a
	// search by Message-ID is how a move without COPYUID finds where the
	// message landed.
	srv, sess, _ := wireSession(t, providertest.MicrosoftCaps())
	srv.CreateMailbox(t, "Archive")
	srv.Append(t, "Archive", message("other"), nil, time.Time{})
	uid := srv.Append(t, "INBOX", message("wanted"), nil, time.Time{})
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	res, err := sess.Copy(t.Context(), goimap.UIDSetNum(uid), "Archive")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	landed := res.Mapping[uid]
	if landed == 0 || res.DestUIDValidity == 0 {
		t.Fatalf("copy result = %+v", res)
	}
	if still, err := sess.UIDs(t.Context(), goimap.UIDSetNum(uid), time.Time{}); err != nil || len(still) != 1 {
		t.Fatalf("the original after a copy: %v, %v", still, err)
	}
	if _, err := sess.Select(t.Context(), "Archive", true, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"wanted@example.com", "<wanted@example.com>"} {
		found, err := sess.SearchMessageID(t.Context(), id)
		if err != nil || len(found) != 1 || found[0] != landed {
			t.Errorf("search %q = %v, %v; want [%d]", id, found, err, landed)
		}
	}
	if found, err := sess.SearchMessageID(t.Context(), "anted@example.com"); err != nil || len(found) != 0 {
		t.Errorf("a part of an id found %v, %v", found, err)
	}
}

func TestAChangeTheServerAnsweredIsReportedEvenWhenItsCallerLeft(t *testing.T) {
	// The caller gives up — the person closed the tab — while the command is
	// on the wire. The server carries it out anyway, so what it answered is
	// what the caller gets: the new UIDs of a move, the flags a STORE set.
	// "Cancelled" would leave the index saying the message is where it was.
	for _, tc := range []struct {
		name string
		caps goimap.CapSet
		verb string
	}{
		{"a MOVE", providertest.MicrosoftCaps(), "MOVE"},
		{"the fallback's COPY", withoutMove(), "COPY"},
		{"a STORE", providertest.MicrosoftCaps(), "STORE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trip := &cancelOn{verb: tc.verb}
			srv, sess := wireSessionTo(t, tc.caps, trip)
			srv.CreateMailbox(t, "Trash")
			uid := srv.Append(t, "INBOX", message("left"), nil, time.Time{})
			if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			trip.arm(cancel)

			if tc.verb == "STORE" {
				ups, err := sess.StoreFlags(ctx, goimap.UIDSetNum(uid), provider.FlagAdd,
					[]goimap.Flag{goimap.FlagSeen}, 0)
				if !trip.didFire() {
					t.Fatal("the caller did not leave while the STORE was on the wire")
				}
				if err != nil || len(ups) != 1 || !containsFlag(ups[0].Flags, `\seen`) {
					t.Fatalf("StoreFlags = %+v, %v; want the flags the server set", ups, err)
				}
				return
			}
			res, err := sess.Move(ctx, goimap.UIDSetNum(uid), "Trash")
			if !trip.didFire() {
				t.Fatalf("the caller did not leave while the %s was on the wire", tc.verb)
			}
			if err != nil || res.Mapping[uid] == 0 {
				t.Fatalf("Move = %+v, %v; want the UID the server reported", res, err)
			}
			// The fallback went on to take the message out of the inbox, and
			// the connection is in step for the next command.
			left, err := sess.UIDs(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, time.Time{})
			if err != nil || len(left) != 0 {
				t.Fatalf("the inbox after the move: %v, %v", left, err)
			}
			if _, err := sess.Select(t.Context(), "Trash", true, 0); err != nil {
				t.Fatal(err)
			}
			in, err := sess.UIDs(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, time.Time{})
			if err != nil || !slices.Contains(in, res.Mapping[uid]) {
				t.Fatalf("the trash holds %v, %v; want UID %d", in, err, res.Mapping[uid])
			}
		})
	}
}

func TestACallerGoneBeforeAChangeIsSentSendsNothing(t *testing.T) {
	srv, sess, wire := wireSession(t, providertest.MicrosoftCaps())
	srv.CreateMailbox(t, "Trash")
	uid := srv.Append(t, "INBOX", message("kept"), nil, time.Time{})
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sess.Move(ctx, goimap.UIDSetNum(uid), "Trash"); !errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("Move with its caller gone = %v", err)
	}
	if n := wire.count("UID MOVE"); n != 0 {
		t.Fatalf("a move whose caller had gone was sent: %q", wire.sent())
	}
}
