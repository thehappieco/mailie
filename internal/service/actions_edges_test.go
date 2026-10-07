package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func (s *fakeSync) triggeredIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.triggered)
}

// holdFirst makes the first call that match accepts wait, once it has
// reached the server, until the test releases it.
type holdFirst struct {
	once     sync.Once
	entered  chan struct{}
	released chan struct{}
	// ended is what the call's context said once released.
	ended error
}

func hold(box *providertest.FakeMailbox, match func(providertest.Call) bool) *holdFirst {
	h := &holdFirst{entered: make(chan struct{}), released: make(chan struct{})}
	box.OnCall(func(ctx context.Context, c providertest.Call) error {
		if match(c) {
			h.once.Do(func() {
				close(h.entered)
				<-h.released
				h.ended = ctx.Err()
			})
		}
		return nil
	})
	return h
}

func TestAGenericServersAllFolderIsLeftWithAMoveLikeAnyOther(t *testing.T) {
	// Dovecot can publish a virtual folder of everything with \All. It is not
	// Gmail's All Mail: moving a message out of it is a MOVE, which the server
	// carries to the message's real folder, and it needs MOVE or UIDPLUS like
	// any other move. A COPY would leave the message where it was while the
	// index said it had moved.
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("All", imap.MailboxAttrAll)
	box.CreateFolder("Work")
	box.Deliver("All", message("a", "Lunch on Friday"))
	box.Deliver("All", message("b", "Quarterly numbers"))
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	a, work := b.row(t, "All", "a"), b.folder(t, "Work")

	res, err := m.svc.MoveMessages(t.Context(), ana, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderID != work {
		t.Fatalf("move = %+v, %v", res, err)
	}
	for _, c := range box.Calls() {
		if c.Method == providertest.MethodCopy {
			t.Fatalf("a COPY was sent out of a generic \\All folder: %+v", c)
		}
	}
	if n := b.serverCalls(providertest.MethodMove); n != 1 {
		t.Fatalf("%d MOVEs, want 1", n)
	}
	if b.on("All", "a") || !b.on("Work", "a") {
		t.Fatal("the server does not hold the message in Work only")
	}
	// Nor is moving into it archiving, which on Gmail only a message in the
	// inbox may be: it is a folder like Work.
	all := b.folder(t, "All")
	res, err = m.svc.MoveMessages(t.Context(), ana, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(all, 10)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].FolderID != all {
		t.Fatalf("moving back into \\All = %+v, %v", res, err)
	}

	// Without MOVE and UIDPLUS it is refused like any move, before anything
	// is sent.
	box.SetCaps(provider.Caps{CondStore: true, SpecialUse: true})
	box.ResetCalls()
	_, err = m.svc.MoveMessages(t.Context(), ana, service.MoveRequest{
		IDs: []int64{b.row(t, "All", "b")}, To: strconv.FormatInt(work, 10),
	})
	wantCode(t, "moving out of \\All without MOVE and UIDPLUS", err, service.CodeConflict)
	for _, c := range box.Calls() {
		switch c.Method {
		case providertest.MethodMove, providertest.MethodCopy, providertest.MethodStoreFlags:
			t.Fatalf("a refused move sent %+v", c)
		}
	}
}

func TestAMoveSentBeforeItsCallerLeftIsCarriedOutAndRecorded(t *testing.T) {
	// The person clicks Move and closes the tab while the MOVE is on its way.
	// The server carries it out; the index must say so, not keep the message
	// where it was.
	b := genericBox(t)
	a, work := b.row(t, "INBOX", "a"), b.folder(t, "Work")
	h := hold(b.box, func(c providertest.Call) bool { return c.Method == providertest.MethodMove })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
		done <- err
	}()
	<-h.entered
	cancel()
	close(h.released)
	<-done
	b.box.OnCall(nil)

	if h.ended != nil {
		t.Fatalf("the MOVE was sent under the caller's context, which ended with it: %v", h.ended)
	}
	if b.on("INBOX", "a") || !b.on("Work", "a") {
		t.Fatal("the move was not carried out")
	}
	if folder := b.m.count(t, `SELECT folder_id FROM messages WHERE id = ?`, a); int64(folder) != work {
		t.Fatal("the index does not say the message moved")
	}
}

// cancelOnWire cancels a context the moment the connection sends a command
// that contains needle: a caller that leaves while it is on the wire.
type cancelOnWire struct {
	needle []byte

	mu     sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOnWire) arm(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel, c.fired = cancel, false
}

func (c *cancelOnWire) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil && bytes.Contains(p, c.needle) {
		c.cancel()
		c.cancel, c.fired = nil, true
	}
	return len(p), nil
}

func (c *cancelOnWire) didFire() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

func TestAMoveWhoseCallerLeftWhileItWasOnTheWireIsRecorded(t *testing.T) {
	// The same, over a real IMAP connection: the caller leaves the moment
	// UID MOVE goes out, and the server's answer — the new UID — is what the
	// index records, under the row's own id.
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2", Caps: providertest.MicrosoftCaps()})
	srv.CreateMailbox(t, "Work")
	srv.Append(t, "INBOX", "From: Bea <bea@example.org>\r\nTo: Ana <ana@example.org>\r\nSubject: Lunch\r\n"+
		"Message-ID: <a@example.org>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nNoon?\r\n",
		nil, time.Now().Add(-time.Hour))
	trip := &cancelOnWire{needle: []byte("UID MOVE ")}
	mailbox, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: srv.Addr,
		Credentials: provider.Credentials{User: srv.User, Password: "hunter2"},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true, DebugWriter: trip,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	const id = "acc_00000000000000d1"
	host, port := splitHostPort(t, srv.Addr)
	if _, err := m.repo.Create(t.Context(), account.Account{
		ID: id, Email: srv.User, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port, SMTPTLS: "implicit", LoginUser: srv.User,
		State: account.StateActive,
	}, ana.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.boxes[id] = mailbox
	m.mu.Unlock()
	storetest.IndexMailbox(t, m.db, id, mailbox)
	b := &actionBox{m: m, owner: ana, id: id}
	b.allow(t)
	a, work := b.row(t, "INBOX", "a"), b.folder(t, "Work")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trip.arm(cancel)
	_, _ = m.svc.MoveMessages(ctx, ana, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
	if !trip.didFire() {
		t.Fatal("the caller did not leave while the MOVE was on the wire")
	}
	var folder, uid int64
	if err := m.db.Reader().QueryRowContext(t.Context(), `SELECT folder_id, uid FROM messages WHERE id = ?`, a).
		Scan(&folder, &uid); err != nil {
		t.Fatalf("the moved row: %v", err)
	}
	if folder != work || uid == 0 {
		t.Fatalf("the row is in folder %d at UID %d, want Work (%d) at the UID the server reported", folder, uid, work)
	}
	if n := m.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, id); n != 1 {
		t.Fatalf("%d rows for one message", n)
	}
}

func TestAnArchiveWhoseAnswerNeverCameIsNeverAnnouncedAsADeletion(t *testing.T) {
	// Gmail archives the message and the connection drops before its answer
	// arrives. Nobody knows yet whether it moved: the row stays, a pass is
	// asked for, and when the inbox's diffs find the message gone it is
	// announced as a copy leaving — it is in All Mail, not deleted.
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@gmail.com", providertest.FakeOptions{
		Kind: provider.KindGmail, Caps: providertest.GmailCaps(), SharedFlags: true, Labels: true,
	})
	box.CreateFolder("[Gmail]/All Mail", imap.MailboxAttrAll)
	box.CreateFolder("[Gmail]/Trash", imap.MailboxAttrTrash)
	uid := box.Deliver("INBOX", message("a", "Lunch on Friday"))
	box.CopyTo("INBOX", uid, "[Gmail]/All Mail")
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	m.engine.running(id, service.SyncStatus{Running: true})
	row := b.row(t, "INBOX", "a")
	since := b.lastSeq(t)

	box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodMove {
			box.Expunge("INBOX", uid)
			return fmt.Errorf("%w: connection reset by peer", provider.ErrConnClosed)
		}
		return nil
	})
	if _, err := m.svc.MoveMessages(t.Context(), ana, service.MoveRequest{IDs: []int64{row}, To: "archive"}); err == nil {
		t.Fatal("an archive whose answer never came reported success")
	}
	box.OnCall(nil)
	if !slices.Contains(m.engine.triggeredIDs(), id) {
		t.Fatal("no pass was asked for after a MOVE that may have been carried out")
	}
	if n := m.count(t, `SELECT count(*) FROM messages WHERE id = ?`, row); n != 1 {
		t.Fatal("the row left the index on a move nobody confirmed")
	}

	// The inbox's pass: two diffs that find the message gone.
	inbox := b.folder(t, "INBOX")
	st, _ := box.Folder("INBOX")
	for range 2 {
		if err := m.db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := m.db.ApplyUIDDiff(t.Context(), tx, store.UIDDiff{
				AccountID: id, FolderID: inbox, UIDValidity: st.UIDValidity, Floor: 1, ServerUIDs: st.UIDs,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := m.count(t, `SELECT count(*) FROM messages WHERE id = ?`, row); n != 0 {
		t.Fatal("the diffs did not take the row out")
	}
	if evs := b.journaled(t, since, events.TypeMessageDeleted); len(evs) != 0 {
		t.Fatalf("an archive was announced as a deletion: %s", evs)
	}
	if evs := b.journaled(t, since, events.TypeMessageMoved); len(evs) != 1 {
		t.Fatalf("message.moved = %s, want the row leaving", evs)
	}
}

func TestWithdrawingWhileAnActionWaitsForTheServerStopsItBeforeAnyChange(t *testing.T) {
	// The action was accepted, and is opening its folder on the connection,
	// when the person turns actions off in another tab. Nothing may change
	// on the server after that.
	b := genericBox(t)
	a, work := b.row(t, "INBOX", "a"), b.folder(t, "Work")
	for _, tc := range []struct {
		name string
		act  func(context.Context) error
	}{
		{"marking read", func(ctx context.Context) error {
			_, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
			return err
		}},
		{"moving", func(ctx context.Context) error {
			_, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
			return err
		}},
		{"trashing", func(ctx context.Context) error {
			_, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}})
			return err
		}},
	} {
		b.allow(t)
		b.box.ResetCalls()
		h := hold(b.box, func(c providertest.Call) bool {
			return c.Method == providertest.MethodSelect && c.Role == provider.RoleInteractive
		})
		done := make(chan error, 1)
		go func() { done <- tc.act(t.Context()) }()
		<-h.entered
		if _, err := b.m.svc.WithdrawActionsConsent(t.Context(), b.owner); err != nil {
			t.Fatal(err)
		}
		close(h.released)
		err := <-done
		b.box.OnCall(nil)
		wantCode(t, tc.name+" after the withdrawal", err, service.CodeConflict)
		if msg := service.MessageOf(err); msg != "actions are off: you have not allowed them in the console" {
			t.Errorf("%s: message = %q", tc.name, msg)
		}
		for _, c := range b.box.Calls() {
			switch c.Method {
			case providertest.MethodStoreFlags, providertest.MethodMove, providertest.MethodCopy:
				t.Fatalf("%s: %s reached the server after the withdrawal", tc.name, c.Method)
			}
		}
	}
	if b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, a) != 0 || !b.on("INBOX", "a") {
		t.Fatal("the message changed")
	}
}

func TestUndoingAMoveIntoALabelTheMessageHadPutsTheOldLabelBack(t *testing.T) {
	// Gmail: the message has the labels Work and Travel. Moving the Work copy
	// to Travel takes the Work label away and leaves Travel, which it had.
	// The undo adds Work back — a COPY — and never MOVEs out of Travel, which
	// would take away a label the message had before any of this.
	b := gmailBox(t)
	ctx := t.Context()
	b.box.CreateFolder("Travel")
	b.box.CopyTo("INBOX", b.serverUID("INBOX", "a"), "Travel")
	b.m.index(t, b.id, b.box)
	row, work, travel := b.row(t, "Work", "a"), b.folder(t, "Work"), b.folder(t, "Travel")

	moved, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{row}, To: strconv.FormatInt(travel, 10)})
	if err != nil || len(moved.Messages) != 1 || moved.Messages[0].ID != row || moved.Messages[0].FolderID != travel {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if b.on("Work", "a") || !b.on("Travel", "a") {
		t.Fatal("the move did not take the Work label away")
	}
	since := b.lastSeq(t)
	b.box.ResetCalls()

	undo, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{row}, To: strconv.FormatInt(work, 10)})
	if err != nil {
		t.Fatal(err)
	}
	var sent []providertest.Call
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodMove || c.Method == providertest.MethodCopy {
			sent = append(sent, c)
		}
	}
	if len(sent) != 1 || sent[0].Method != providertest.MethodCopy || sent[0].Folder != "Travel" || sent[0].Dest != "Work" {
		t.Fatalf("the undo sent %+v, want one COPY from Travel to Work", sent)
	}
	if !b.on("Work", "a") || !b.on("Travel", "a") || !b.on("INBOX", "a") {
		t.Fatal("the message does not have its labels back")
	}
	if len(undo.Messages) != 1 || undo.Messages[0].FolderID != work || len(undo.Removed) != 0 {
		t.Fatalf("undo = %+v, want the message back in Work", undo)
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND folder_id = ?`, row, travel); n != 1 {
		t.Fatal("the Travel copy's row is not where the message still is")
	}
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("the undo announced %d new messages", n)
	}
}
