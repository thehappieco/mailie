package service_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

// enginedBox is ana's mailbox on a fake server with the real sync engine
// running over it, and the service using the engine's connection.
type enginedBox struct {
	*actionBox
	engine  *syncengine.Manager
	counted *countingMailbox
}

func newEnginedBox(t *testing.T, caps provider.Caps, setup func(*providertest.FakeMailbox)) *enginedBox {
	t.Helper()
	return newEnginedBoxWith(t, providertest.FakeOptions{Caps: caps}, setup)
}

// newEnginedBoxWith is newEnginedBox on a fake server with o.
func newEnginedBoxWith(t *testing.T, o providertest.FakeOptions, setup func(*providertest.FakeMailbox)) *enginedBox {
	t.Helper()
	late := &lateEngine{}
	f := newFixtureWith(t, fixtureOptions{sync: late})
	box := providertest.NewFakeMailbox(o)
	box.CreateFolder("Archive", imap.MailboxAttrArchive)
	box.CreateFolder("Trash", imap.MailboxAttrTrash)
	box.CreateFolder("Work")
	setup(box)
	counted := &countingMailbox{Mailbox: box, opens: map[provider.Role]int{}}
	late.m = syncengine.New(syncengine.Deps{
		Store: f.db, Accounts: f.repo, Bus: f.bus,
		Mailboxes: func(context.Context, string) (provider.Mailbox, error) { return counted, nil },
	}, syncengine.Options{})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		late.m.Run(runCtx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})

	ana := f.person(t, "ana@example.com", auth.RoleMember)
	const id = "acc_00000000000000f1"
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, Email: "ana@mail.example", Provider: provider.KindIMAP, AuthKind: "password", OwnerUserID: ana.UserID,
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "ana@mail.example", State: account.StateActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	b := &enginedBox{
		actionBox: &actionBox{m: &mailFixture{fixture: f}, owner: ana, id: id, box: box},
		engine:    late.m, counted: counted,
	}
	b.waitFor(t, "the initial sync", func() bool {
		st, err := late.m.Status(t.Context(), id)
		return err == nil && st.State == "live"
	})
	b.allow(t)
	return b
}

func (b *enginedBox) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// passed waits until a pass has seen each folder hold count messages: the
// engine records what SELECT said at the end of every folder's pass, after
// its fetches, flags and diff.
func (b *enginedBox) passed(t *testing.T, counts map[string]int) {
	t.Helper()
	b.waitFor(t, "the passes after the action", func() bool {
		for name, want := range counts {
			if b.m.count(t, `SELECT server_count FROM folders WHERE account_id = ? AND name = ?`, b.id, name) != want {
				return false
			}
		}
		return true
	})
}

func TestAMovedRowKeepsItsIdAndTheNextPassNeitherDuplicatesItNorAnnouncesNewMail(t *testing.T) {
	b := newEnginedBox(t, providertest.GmailCaps(), func(box *providertest.FakeMailbox) {
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
	})
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	since := b.lastSeq(t)

	work := b.folder(t, "Work")
	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderID != work {
		t.Fatalf("move = %+v, %v", res, err)
	}
	b.passed(t, map[string]int{"INBOX": 0, "Work": 1})

	// And back to the inbox, where a message the index has not seen would be
	// new mail.
	res, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderRole != "inbox" {
		t.Fatalf("move back = %+v, %v", res, err)
	}
	b.passed(t, map[string]int{"INBOX": 1, "Work": 0})
	// One more round of passes, triggered after everything settled.
	if err := b.engine.Trigger(ctx, b.id); err != nil {
		t.Fatal(err)
	}
	b.box.Deliver("Archive", message("z", "Unrelated"))
	b.passed(t, map[string]int{"INBOX": 1, "Work": 0, "Archive": 1})

	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE account_id = ? AND message_id = 'a@example.org'`, b.id); n != 1 {
		t.Fatalf("%d rows for the moved message", n)
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND vanished_at = 0`, a); n != 1 {
		t.Fatal("the moved row is gone or hidden")
	}
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted} {
		for _, p := range b.journaled(t, since, typ) {
			t.Errorf("after the moves: %s %s", typ, p)
		}
	}
	actions := 0
	for _, p := range b.journaled(t, since, events.TypeMessageMoved) {
		if strings.Contains(string(p), `"from_folder_id"`) {
			actions++
		} else if !strings.Contains(string(p), `"new_copy":true`) {
			// The unrelated message's arrival in the archive is a copy
			// appearing; nothing else may be.
			t.Errorf("a pass announced %s", p)
		}
	}
	if actions != 2 {
		t.Errorf("%d moves announced, want one per move", actions)
	}
}

func TestAMessageMovedWithoutANewUIDIsIndexedByTheDestinationsPassAsAMove(t *testing.T) {
	// No UIDPLUS and no Message-ID: nothing says where the message landed,
	// so its row leaves the index and the inbox's pass finds it. The inbox
	// has not seen it before, and it is still not new mail.
	caps := providertest.GmailCaps()
	caps.UIDPlus = false
	b := newEnginedBox(t, caps, func(box *providertest.FakeMailbox) {
		box.Deliver("Work", providertest.FakeMessage{
			Subject: "No id", From: "Bea <bea@example.org>", InternalDate: time.Now().Add(-48 * time.Hour),
		})
	})
	ctx := t.Context()
	var row int64
	if err := b.m.db.Reader().QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ?`, b.id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	since := b.lastSeq(t)
	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{row}, To: "inbox"})
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != row {
		t.Fatalf("move = %+v, %v", res, err)
	}
	b.passed(t, map[string]int{"INBOX": 1, "Work": 0})
	b.waitFor(t, "the inbox's pass to index the message", func() bool {
		return b.m.count(t, `SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE m.account_id = ? AND f.name = 'INBOX'`, b.id) == 1
	})
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("a moved message was announced as new mail %d times", n)
	}
	moved := b.journaled(t, since, events.TypeMessageMoved)
	if len(moved) != 2 {
		t.Fatalf("message.moved = %s, want the row leaving and the copy the pass found", moved)
	}
}

func TestActionsUseTheEnginesOneInteractiveConnection(t *testing.T) {
	b := newEnginedBox(t, providertest.GmailCaps(), func(box *providertest.FakeMailbox) {
		box.Deliver("INBOX", message("a", "Lunch"))
		box.Deliver("INBOX", message("b", "Numbers"))
	})
	ctx := t.Context()
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a, other}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{other}}); err != nil {
		t.Fatal(err)
	}
	b.passed(t, map[string]int{"INBOX": 0, "Archive": 1, "Trash": 1})
	if n := b.counted.count(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections for three actions, want the engine's one", n)
	}
	b.counted.mu.Lock()
	peak := b.counted.peak
	b.counted.mu.Unlock()
	if peak > 3 {
		t.Errorf("the account held %d connections at once", peak)
	}
	if v := b.box.Violations(); len(v) > 0 {
		t.Errorf("connections misused: %v", v)
	}
}

func TestAFlagAnotherClientChangesAfterAnActionStillReachesTheIndex(t *testing.T) {
	// Without CONDSTORE only the order of things says whether a flag answer
	// is older than a person's action: the engine notes where the actions
	// stood before it asks. What it reads after the action — another client
	// marking the message unread again — is the server's word, and reaches
	// the index.
	b := newEnginedBox(t, providertest.ExchangeCaps(), func(box *providertest.FakeMailbox) {
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
	})
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	if seen := b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, a); seen != 1 {
		t.Fatal("the action was not recorded")
	}
	b.box.SetFlags("INBOX", imap.UID(b.m.count(t, `SELECT uid FROM messages WHERE id = ?`, a)))
	if err := b.engine.Trigger(ctx, b.id); err != nil {
		t.Fatal(err)
	}
	b.waitFor(t, "the pass to read the other client's change", func() bool {
		return b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, a) == 0
	})
}
