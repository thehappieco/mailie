package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
)

// A COPY or MOVE of several messages is answered with COPYUID, which RFC
// 4315 pairs by position; go-imap reads each of its UID sets sorted, so when
// the server lists the new UIDs in another order — Gmail does — the pairing
// the adapter hands over is not the server's. ReversedCopyUID makes the fake
// answer that way. Every test below but the one about a single message
// fails when that pairing is trusted.

// crossedBox is ana's mailbox on a server whose COPYUID of several messages
// reads back crossed, as Gmail's does through go-imap: Gmail with its
// labels, or a generic server with MOVE and UIDPLUS. setup fills it before
// it is indexed. She has allowed actions.
func crossedBox(t *testing.T, kind provider.Kind, setup func(*providertest.FakeMailbox)) *actionBox {
	t.Helper()
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	o := providertest.FakeOptions{Caps: providertest.GmailCaps(), ReversedCopyUID: true}
	email := "ana@mail.example"
	if kind == provider.KindGmail {
		o.Kind, o.SharedFlags, o.Labels = provider.KindGmail, true, true
		email = "ana@gmail.com"
	}
	id, box := m.ownedBox(t, ana, email, o)
	setup(box)
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	return b
}

// gmailFolders are the folders of a Gmail account in these tests.
func gmailFolders(box *providertest.FakeMailbox) {
	box.CreateFolder("[Gmail]/All Mail", imap.MailboxAttrAll)
	box.CreateFolder("[Gmail]/Trash", imap.MailboxAttrTrash)
	box.CreateFolder("Work")
	box.CreateFolder("Travel")
}

// onItsOwn fails unless the row id is indexed at the UID where the server
// holds the message key, and opening it reads body, that message's.
func (b *actionBox) onItsOwn(t *testing.T, id int64, key, body string) {
	t.Helper()
	var (
		folder string
		uid    uint32
	)
	if err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT f.name, m.uid FROM messages m
		JOIN folders f ON f.id = m.folder_id WHERE m.id = ?`, id).Scan(&folder, &uid); err != nil {
		t.Fatalf("row %d (%s): %v", id, key, err)
	}
	if at := b.serverUID(folder, key); at == 0 || imap.UID(uid) != at {
		t.Fatalf("row %d (%s) points at UID %d in %s, where the server holds %s at UID %d", id, key, uid, folder, key, at)
	}
	msg, err := b.m.svc.GetMessage(t.Context(), b.owner, service.GetMessageRequest{ID: id})
	if err != nil {
		t.Fatalf("opening row %d (%s): %v", id, key, err)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, body) {
		t.Fatalf("opening row %d (%s) read %v, want %q", id, key, msg.Body.Text, body)
	}
}

// bodyOf is what the fake's message key reads as, when a test left its raw
// bytes out.
func bodyOf(subject string) string { return "Body of " + subject }

// verifications counts the reads of where a move landed: FETCHes of
// summaries on the interactive connection.
func (b *actionBox) verifications() int {
	n := 0
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodFetchSummaries && c.Role == provider.RoleInteractive {
			n++
		}
	}
	return n
}

func TestMovingSeveralMessagesLeavesEachRowOnItsOwnMessage(t *testing.T) {
	b := crossedBox(t, provider.KindIMAP, func(box *providertest.FakeMailbox) {
		box.CreateFolder("Work")
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
		box.Deliver("INBOX", message("b", "Quarterly numbers"))
		box.Deliver("INBOX", message("c", "Flights"))
	})
	ctx := t.Context()
	rows := map[string]int64{"a": b.row(t, "INBOX", "a"), "b": b.row(t, "INBOX", "b"), "c": b.row(t, "INBOX", "c")}
	work := b.folder(t, "Work")
	since := b.lastSeq(t)
	b.box.ResetCalls()

	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{
		IDs: []int64{rows["a"], rows["b"], rows["c"]}, To: strconv.FormatInt(work, 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 3 || len(res.Removed) != 0 {
		t.Fatalf("result = %+v, want the three rows in Work under their ids", res)
	}
	for i, key := range []string{"a", "b", "c"} {
		if res.Messages[i].ID != rows[key] || res.Messages[i].FolderID != work {
			t.Fatalf("result = %+v, want %s's row in Work", res.Messages[i], key)
		}
	}
	if n := b.verifications(); n != 1 {
		t.Errorf("%d reads of where the messages landed, want 1", n)
	}
	b.onItsOwn(t, rows["a"], "a", bodyOf("Lunch on Friday"))
	b.onItsOwn(t, rows["b"], "b", bodyOf("Quarterly numbers"))
	b.onItsOwn(t, rows["c"], "c", bodyOf("Flights"))
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("the move announced %d new messages", n)
	}
}

// newsletter is a message whose body has two parts, a plain and an HTML
// one, as the message that could no longer be opened had.
func newsletter(key string) providertest.FakeMessage {
	msg := message(key, "Newsletter")
	msg.Raw = []byte(strings.ReplaceAll(`From: Bea Lima <bea@example.org>
To: ana@gmail.com
Subject: Newsletter
Message-ID: <`+key+`@example.org>
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8

The newsletter, in plain text.
--alt
Content-Type: text/html; charset=utf-8

<p>The newsletter, in HTML.</p>
--alt--
`, "\n", "\r\n"))
	return msg
}

func TestTrashingSeveralGmailMessagesLeavesEachRowOnItsOwnMessage(t *testing.T) {
	// The incident: two messages trashed together, each row rewritten with
	// the other's UID. The newsletter's row then asked for a second part the
	// other message does not have ("Some messages could not be FETCHed"),
	// and moving it back to the inbox moved the other message.
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
		box.Deliver("INBOX", newsletter("n"))
	})
	ctx := t.Context()
	a, n := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "n")

	res, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a, n}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || res.Messages[0].ID != a || res.Messages[1].ID != n ||
		res.Messages[0].FolderRole != "trash" || res.Messages[1].FolderRole != "trash" {
		t.Fatalf("result = %+v, want both rows in the trash under their ids", res)
	}
	b.onItsOwn(t, a, "a", bodyOf("Lunch on Friday"))
	b.onItsOwn(t, n, "n", "The newsletter, in plain text.")

	back, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{n}, To: "inbox"})
	if err != nil || len(back.Messages) != 1 || back.Messages[0].ID != n {
		t.Fatalf("moving the newsletter back = %+v, %v", back, err)
	}
	if !b.on("INBOX", "n") || b.on("INBOX", "a") || !b.on("[Gmail]/Trash", "a") {
		t.Fatal("moving the newsletter back to the inbox moved another message")
	}
	b.onItsOwn(t, n, "n", "The newsletter, in plain text.")
	b.onItsOwn(t, a, "a", bodyOf("Lunch on Friday"))
}

func TestArchivingSeveralGmailMessagesAndUndoingBringsEachBackAsItself(t *testing.T) {
	// All Mail holds the messages in another order than the inbox, so the
	// archive's COPYUID reads back crossed, and so does the undo's COPY back
	// into the inbox, where they get new UIDs.
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		uids := map[string]imap.UID{}
		for _, m := range []struct{ key, subject string }{{"a", "Lunch on Friday"}, {"b", "Quarterly numbers"}, {"c", "Flights"}} {
			uids[m.key] = box.Deliver("INBOX", message(m.key, m.subject))
		}
		for _, key := range []string{"c", "b", "a"} {
			box.CopyTo("INBOX", uids[key], "[Gmail]/All Mail")
		}
	})
	ctx := t.Context()
	a, bb, c := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b"), b.row(t, "INBOX", "c")
	since := b.lastSeq(t)

	archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb, c}, To: "archive"})
	if err != nil || len(archived.Removed) != 3 {
		t.Fatalf("archive = %+v, %v", archived, err)
	}

	// Undoing one alone puts that one back, and only that one.
	one, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	if err != nil || len(one.Messages) != 1 {
		t.Fatalf("undoing a = %+v, %v", one, err)
	}
	if !b.on("INBOX", "a") || b.on("INBOX", "b") || b.on("INBOX", "c") {
		t.Fatal("undoing one archived message put another back in the inbox")
	}
	b.onItsOwn(t, one.Messages[0].ID, "a", bodyOf("Lunch on Friday"))

	// Undoing the other two at once gives each its own new row.
	two, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{bb, c}, To: "inbox"})
	if err != nil || len(two.Messages) != 2 || len(two.Removed) != 0 {
		t.Fatalf("undoing b and c = %+v, %v", two, err)
	}
	if !b.on("INBOX", "b") || !b.on("INBOX", "c") {
		t.Fatal("the messages are not back in the inbox")
	}
	b.onItsOwn(t, two.Messages[0].ID, "b", bodyOf("Quarterly numbers"))
	b.onItsOwn(t, two.Messages[1].ID, "c", bodyOf("Flights"))
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted} {
		if n := len(b.journaled(t, since, typ)); n != 0 {
			t.Fatalf("archiving and undoing announced %d %s", n, typ)
		}
	}
}

func TestUndoingAMoveOfSeveralGmailMessagesIntoALabelTheyHadPutsEachLabelBackOnItself(t *testing.T) {
	// Both messages carry Work and Travel, Travel in the other order. Moving
	// the Work copies to Travel only takes Work away, and the server reports
	// the UIDs they already have in Travel — crossed. The undo copies them
	// back into Work, where they get new UIDs — crossed again.
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		a := box.Deliver("INBOX", message("a", "Lunch on Friday"))
		bb := box.Deliver("INBOX", message("b", "Quarterly numbers"))
		box.CopyTo("INBOX", a, "Work")
		box.CopyTo("INBOX", bb, "Work")
		box.CopyTo("INBOX", bb, "Travel")
		box.CopyTo("INBOX", a, "Travel")
	})
	ctx := t.Context()
	a, bb := b.row(t, "Work", "a"), b.row(t, "Work", "b")
	work, travel := b.folder(t, "Work"), b.folder(t, "Travel")

	moved, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: strconv.FormatInt(travel, 10)})
	if err != nil || len(moved.Messages) != 2 {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	b.onItsOwn(t, a, "a", bodyOf("Lunch on Friday"))
	b.onItsOwn(t, bb, "b", bodyOf("Quarterly numbers"))

	b.box.ResetCalls()
	undo, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: strconv.FormatInt(work, 10)})
	if err != nil || len(undo.Messages) != 2 || len(undo.Removed) != 0 {
		t.Fatalf("undo = %+v, %v", undo, err)
	}
	if n := b.serverCalls(providertest.MethodMove); n != 0 {
		t.Fatalf("the undo sent %d MOVEs; it adds the Work label back with a COPY", n)
	}
	if !b.on("Work", "a") || !b.on("Work", "b") || !b.on("Travel", "a") || !b.on("Travel", "b") {
		t.Fatal("the messages do not have both labels back")
	}
	for i, m := range []struct{ key, body string }{{"a", bodyOf("Lunch on Friday")}, {"b", bodyOf("Quarterly numbers")}} {
		if undo.Messages[i].FolderID != work {
			t.Fatalf("undo = %+v, want %s's copy in Work", undo.Messages[i], m.key)
		}
		b.onItsOwn(t, undo.Messages[i].ID, m.key, m.body)
	}
	b.onItsOwn(t, a, "a", bodyOf("Lunch on Friday"))
	b.onItsOwn(t, bb, "b", bodyOf("Quarterly numbers"))
}

func TestAMoveOfOneMessageTrustsItsCOPYUIDWithoutReadingItBack(t *testing.T) {
	b := crossedBox(t, provider.KindIMAP, func(box *providertest.FakeMailbox) {
		box.CreateFolder("Work")
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
		box.Deliver("INBOX", message("b", "Quarterly numbers"))
	})
	a, work := b.row(t, "INBOX", "a"), b.folder(t, "Work")
	b.box.ResetCalls()
	res, err := b.m.svc.MoveMessages(t.Context(), b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderID != work {
		t.Fatalf("move = %+v, %v", res, err)
	}
	for _, c := range b.box.Calls() {
		if c.Role == provider.RoleInteractive && (c.Method == providertest.MethodFetchSummaries ||
			(c.Method == providertest.MethodSelect && c.Folder == "Work")) {
			t.Fatalf("a move of one message read where it landed: %+v", c)
		}
	}
	b.onItsOwn(t, a, "a", bodyOf("Lunch on Friday"))
}

func TestARowTheServersAnswerCannotPairLeavesTheIndexAndCannotBeUndone(t *testing.T) {
	// The index says one thing about a's size and the server another: a
	// cannot be told apart where it landed. It leaves the index as a move to
	// where the server did not say — never as a deletion, never as new mail —
	// and has no undo that would move whatever sits at a guessed UID. The
	// message that pairs is placed, and undone, as usual.
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		a := box.Deliver("INBOX", message("a", "Lunch on Friday"))
		bb := box.Deliver("INBOX", message("b", "Quarterly numbers"))
		box.CopyTo("INBOX", bb, "[Gmail]/All Mail")
		box.CopyTo("INBOX", a, "[Gmail]/All Mail")
	})
	ctx := t.Context()
	a, bb := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	if err := b.m.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET size = size + 1 WHERE id = ?`, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	since := b.lastSeq(t)

	archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: "archive"})
	if err != nil || len(archived.Messages) != 0 || len(archived.Removed) != 2 {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	if b.on("INBOX", "a") || b.on("INBOX", "b") {
		t.Fatal("the server did not archive both")
	}
	if left := b.leaving(t, since); len(left) != 2 || !left[a] || !left[bb] {
		t.Fatalf("rows announced leaving: %v, want both", left)
	}
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted} {
		if n := len(b.journaled(t, since, typ)); n != 0 {
			t.Fatalf("the archive announced %d %s", n, typ)
		}
	}

	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	wantCode(t, "undoing a message the server's answer could not place", err, service.CodeNotFound)
	if b.on("INBOX", "a") || b.on("INBOX", "b") {
		t.Fatal("the refused undo moved a message")
	}
	undo, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{bb}, To: "inbox"})
	if err != nil || len(undo.Messages) != 1 {
		t.Fatalf("undoing b = %+v, %v", undo, err)
	}
	if !b.on("INBOX", "b") || b.on("INBOX", "a") {
		t.Fatal("undoing b did not put b, and only b, back")
	}
	b.onItsOwn(t, undo.Messages[0].ID, "b", bodyOf("Quarterly numbers"))
}

func TestWhenWhereMessagesLandedCannotBeReadEachLeavesTheIndexAndItsFoldersPassFindsIt(t *testing.T) {
	// The move is done on the server; reading where the messages landed
	// fails. No row may stay at a UID nothing confirmed: each leaves the
	// index as a move, and Work's pass indexes the messages under new ids,
	// as messages that moved, not as new mail.
	b := newEnginedBoxWith(t, providertest.FakeOptions{Caps: providertest.GmailCaps(), ReversedCopyUID: true},
		func(box *providertest.FakeMailbox) {
			box.Deliver("INBOX", message("a", "Lunch on Friday"))
			box.Deliver("INBOX", message("b", "Quarterly numbers"))
		})
	ctx := t.Context()
	a, bb, work := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b"), b.folder(t, "Work")
	since := b.lastSeq(t)
	var failed atomic.Bool
	b.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodFetchSummaries && c.Role == provider.RoleInteractive &&
			failed.CompareAndSwap(false, true) {
			return fmt.Errorf("%w: the server is busy", provider.ErrTemporary)
		}
		return nil
	})
	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: strconv.FormatInt(work, 10)})
	b.box.OnCall(nil)
	if err != nil || len(res.Messages) != 0 || len(res.Removed) != 2 {
		t.Fatalf("move = %+v, %v; want both rows out of the index", res, err)
	}
	if !failed.Load() {
		t.Fatal("the move never read where the messages landed")
	}
	if left := b.leaving(t, since); len(left) != 2 || !left[a] || !left[bb] {
		t.Fatalf("rows announced leaving: %v, want both", left)
	}
	b.passed(t, map[string]int{"INBOX": 0, "Work": 2})
	b.waitFor(t, "Work's pass to index both messages", func() bool {
		return b.m.count(t, `SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE m.account_id = ? AND f.name = 'Work'`, b.id) == 2
	})
	for _, m := range []struct{ key, subject string }{{"a", "Lunch on Friday"}, {"b", "Quarterly numbers"}} {
		id := b.row(t, "Work", m.key)
		if id == a || id == bb {
			t.Fatalf("%s is back under an old id, %d", m.key, id)
		}
		b.onItsOwn(t, id, m.key, bodyOf(m.subject))
	}
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("messages that moved were announced as new mail %d times", n)
	}
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	wantCode(t, "moving back a row the move could not place", err, service.CodeNotFound)
	if v := b.box.Violations(); len(v) > 0 {
		t.Errorf("connections misused: %v", v)
	}
}

func TestAnUndoWhoseLandingCannotBeReadIsSpentAndLeftToTheInboxsPass(t *testing.T) {
	// The archive is undone on the server — both messages are back in the
	// inbox — but reading where they landed fails. Neither is indexed at a
	// guessed UID, and their undo is spent: the inbox's pass indexes them.
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		a := box.Deliver("INBOX", message("a", "Lunch on Friday"))
		bb := box.Deliver("INBOX", message("b", "Quarterly numbers"))
		box.CopyTo("INBOX", bb, "[Gmail]/All Mail")
		box.CopyTo("INBOX", a, "[Gmail]/All Mail")
	})
	ctx := t.Context()
	a, bb := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: "archive"})
	if err != nil || len(archived.Removed) != 2 {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	since := b.lastSeq(t)
	var failed atomic.Bool
	b.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodFetchSummaries && c.Role == provider.RoleInteractive &&
			failed.CompareAndSwap(false, true) {
			return fmt.Errorf("%w: the server is busy", provider.ErrTemporary)
		}
		return nil
	})
	undo, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, bb}, To: "inbox"})
	b.box.OnCall(nil)
	if err != nil || len(undo.Messages) != 0 || len(undo.Removed) != 2 {
		t.Fatalf("undo = %+v, %v; want both back on the server and left to the pass", undo, err)
	}
	if !failed.Load() || !b.on("INBOX", "a") || !b.on("INBOX", "b") {
		t.Fatal("the undo did not happen on the server, or never read where it landed")
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.name = 'INBOX'`, b.id); n != 0 {
		t.Fatalf("%d rows indexed in the inbox at UIDs nothing confirmed", n)
	}
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("the undo announced %d new messages", n)
	}
	b.box.ResetCalls()
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	wantCode(t, "undoing again", err, service.CodeNotFound)
	if n := b.serverCalls(providertest.MethodCopy); n != 0 {
		t.Fatal("a spent undo reached the server")
	}
}

// leaving reads the message.moved events since seq that took a row out of
// the index (to null), by row id.
func (b *actionBox) leaving(t *testing.T, since int64) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	for _, raw := range b.journaled(t, since, events.TypeMessageMoved) {
		var p store.ActionMoved
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.To == nil && !p.NewCopy {
			out[p.MessageID] = true
		}
	}
	return out
}

// twin is a message whose identity is that of every other twin with the same
// messageID ("" for none): the same second and the same size. Only its
// subject, as long as the others', and its body tell it apart.
func twin(messageID, subject string) providertest.FakeMessage {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	return providertest.FakeMessage{
		MessageID: messageID, Subject: subject, From: "Monitor <monitor@example.org>",
		To: []string{"ana@mail.example"}, Date: at, InternalDate: at, Size: 200,
	}
}

// rowBySubject is the id of the row indexed in folder for the message with
// subject, or 0.
func (b *actionBox) rowBySubject(t *testing.T, folder, subject string) int64 {
	t.Helper()
	var id int64
	err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.name = ? AND m.subject = ?`, b.id, folder, subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// subjectsIn reads the subjects of the messages the server holds in folder,
// by UID.
func (b *actionBox) subjectsIn(t *testing.T, folder string) map[imap.UID]string {
	t.Helper()
	ctx := context.Background()
	sess, err := b.box.Open(ctx, provider.RoleSync)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	st, err := sess.Select(ctx, folder, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[imap.UID]string{}
	if st.NumMessages == 0 {
		return out
	}
	if err := sess.FetchSummaries(ctx, imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, 0, func(sum provider.Summary) error {
		out[sum.UID] = sum.Envelope.Subject
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// neverAnother fails if the row id, as long as it is indexed, points at a
// message other than the one with subject, or opening it reads another
// message's body.
func (b *actionBox) neverAnother(t *testing.T, id int64, subject string) {
	t.Helper()
	var (
		folder string
		uid    uint32
	)
	err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT f.name, m.uid FROM messages m
		JOIN folders f ON f.id = m.folder_id WHERE m.id = ?`, id).Scan(&folder, &uid)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if at := b.subjectsIn(t, folder)[imap.UID(uid)]; at != subject {
		t.Fatalf("row %d (%q) points at UID %d in %s, where the server holds %q", id, subject, uid, folder, at)
	}
	msg, err := b.m.svc.GetMessage(t.Context(), b.owner, service.GetMessageRequest{ID: id})
	if err != nil {
		t.Fatalf("opening row %d (%q): %v", id, subject, err)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, bodyOf(subject)) {
		t.Fatalf("opening row %d (%q) read %v", id, subject, msg.Body.Text)
	}
}

func TestMovingSeveralMessagesThatShareAnIdentityNeverLeavesARowOnAnothersMessage(t *testing.T) {
	// Two alerts without a Message-ID, from the same second and of the same
	// size: nothing the index keeps tells them apart where they land, and
	// the order COPYUID reads back in is no answer. Neither row is placed:
	// both leave the index as moves, and Work's pass indexes each message
	// on its own row.
	disk1, disk2 := "Disk 91% full on host1", "Disk 92% full on host2"
	b := crossedBox(t, provider.KindIMAP, func(box *providertest.FakeMailbox) {
		box.CreateFolder("Work")
		box.Deliver("INBOX", twin("", disk1))
		box.Deliver("INBOX", twin("", disk2))
	})
	ctx := t.Context()
	one, two := b.rowBySubject(t, "INBOX", disk1), b.rowBySubject(t, "INBOX", disk2)
	work := b.folder(t, "Work")
	since := b.lastSeq(t)

	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{one, two}, To: strconv.FormatInt(work, 10)})
	if err != nil {
		t.Fatal(err)
	}
	b.neverAnother(t, one, disk1)
	b.neverAnother(t, two, disk2)
	if len(res.Messages) != 0 || len(res.Removed) != 2 {
		t.Fatalf("result = %+v, want both rows out of the index until Work's pass", res)
	}
	if left := b.leaving(t, since); len(left) != 2 || !left[one] || !left[two] {
		t.Fatalf("rows announced leaving: %v, want both", left)
	}
	if got := b.subjectsIn(t, "Work"); len(got) != 2 {
		t.Fatalf("Work holds %v, want both alerts", got)
	}

	b.m.index(t, b.id, b.box)
	for _, subject := range []string{disk1, disk2} {
		id := b.rowBySubject(t, "Work", subject)
		if id == 0 {
			t.Fatalf("Work's pass did not index %q", subject)
		}
		b.neverAnother(t, id, subject)
	}
}

func TestArchivingGmailMessagesThatShareAnIdentityNeverUndoesTheOtherOne(t *testing.T) {
	// Two codes a shop sent under one Message-ID, in the same second and of
	// the same size, archived together. Where each landed in All Mail cannot
	// be told, so neither is remembered for an undo: undoing one is refused,
	// rather than bringing back whichever message a guessed UID holds.
	code1, code2 := "Your code 111111", "Your code 222222"
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		box.Deliver("INBOX", twin("reused@shop.example", code1))
		box.Deliver("INBOX", twin("reused@shop.example", code2))
	})
	ctx := t.Context()
	first, second := b.rowBySubject(t, "INBOX", code1), b.rowBySubject(t, "INBOX", code2)

	archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{first, second}, To: "archive"})
	if err != nil || len(archived.Messages) != 0 || len(archived.Removed) != 2 {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	if left := b.subjectsIn(t, "INBOX"); len(left) != 0 {
		t.Fatalf("the inbox still holds %v", left)
	}

	b.box.ResetCalls()
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{first}, To: "inbox"})
	if back := b.subjectsIn(t, "INBOX"); len(back) != 0 {
		t.Fatalf("undoing %q put %v back in the inbox", code1, back)
	}
	wantCode(t, "undoing one of two archived messages nothing told apart", err, service.CodeNotFound)
	if n := b.serverCalls(providertest.MethodCopy) + b.serverCalls(providertest.MethodMove); n != 0 {
		t.Fatalf("the refused undo sent %d commands that change the mailbox", n)
	}
}

func TestUndoingTheArchiveOfGmailMessagesThatShareAnIdentityLeavesThemToTheInboxsPass(t *testing.T) {
	// Archived one at a time, each is remembered exactly: a COPYUID of one
	// UID says where it went. Undone together, they land in the inbox at new
	// UIDs nothing tells apart, and a guess would hand one code's id on to
	// the other's row. Both are back on the server; neither is indexed at a
	// guessed UID, their undo is spent, and the inbox's pass indexes them.
	code1, code2 := "Your code 111111", "Your code 222222"
	b := crossedBox(t, provider.KindGmail, func(box *providertest.FakeMailbox) {
		gmailFolders(box)
		box.Deliver("INBOX", twin("reused@shop.example", code1))
		box.Deliver("INBOX", twin("reused@shop.example", code2))
	})
	ctx := t.Context()
	first, second := b.rowBySubject(t, "INBOX", code1), b.rowBySubject(t, "INBOX", code2)
	for _, id := range []int64{first, second} {
		archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{id}, To: "archive"})
		if err != nil || len(archived.Removed) != 1 {
			t.Fatalf("archiving %d = %+v, %v", id, archived, err)
		}
	}
	since := b.lastSeq(t)

	undo, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{first, second}, To: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	// The result is in the order the ids were named: a row coming back for
	// one code must be that code's.
	for i, m := range undo.Messages {
		if named := []string{code1, code2}[i]; m.Subject != named {
			t.Fatalf("undoing %q came back as the row of %q", named, m.Subject)
		}
		b.neverAnother(t, m.ID, m.Subject)
	}
	if len(undo.Messages) != 0 || len(undo.Removed) != 2 {
		t.Fatalf("undo = %+v; want both back on the server and left to the inbox's pass", undo)
	}
	if back := b.subjectsIn(t, "INBOX"); len(back) != 2 {
		t.Fatalf("the inbox holds %v, want both codes back", back)
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.name = 'INBOX'`, b.id); n != 0 {
		t.Fatalf("%d rows indexed in the inbox at UIDs nothing told apart", n)
	}
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("the undo announced %d new messages", n)
	}
	b.box.ResetCalls()
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{first}, To: "inbox"})
	wantCode(t, "undoing again", err, service.CodeNotFound)
	if n := b.serverCalls(providertest.MethodCopy) + b.serverCalls(providertest.MethodMove); n != 0 {
		t.Fatal("a spent undo reached the server")
	}
}

func TestAMovedMessageIsPlacedOnlyWhenItsIdentityIsUniqueOnBothSides(t *testing.T) {
	// A row is placed where exactly one of the messages read has its
	// identity, and no other row moved with it has that identity: anything
	// else is a guess. Both alerts leave the index as moves, and Work's pass
	// indexes what is there.
	disk1, disk2 := "Disk 91% full on host1", "Disk 92% full on host2"
	for _, tc := range []struct {
		name string
		// before runs once the index holds both rows, before the move.
		before func(t *testing.T, b *actionBox, one int64)
		// reading runs as the move reads where the alerts landed.
		reading func(b *actionBox)
	}{
		{
			// Another client deletes one alert from Work before it is read:
			// two rows share the one identity read back.
			name: "one of two twins gone before the read",
			reading: func(b *actionBox) {
				st, _ := b.box.Folder("Work")
				b.box.Expunge("Work", st.UIDs[0])
			},
		},
		{
			// The index has the first alert's size wrong: the second is the
			// only row with the identity both messages read back have.
			name: "two messages read back with the identity of one row",
			before: func(t *testing.T, b *actionBox, one int64) {
				if err := b.m.db.Write(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `UPDATE messages SET size = size + 1 WHERE id = ?`, one)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := crossedBox(t, provider.KindIMAP, func(box *providertest.FakeMailbox) {
				box.CreateFolder("Work")
				box.Deliver("INBOX", twin("", disk1))
				box.Deliver("INBOX", twin("", disk2))
			})
			ctx := t.Context()
			one, two := b.rowBySubject(t, "INBOX", disk1), b.rowBySubject(t, "INBOX", disk2)
			work := b.folder(t, "Work")
			if tc.before != nil {
				tc.before(t, b, one)
			}
			if tc.reading != nil {
				var once atomic.Bool
				b.box.OnCall(func(_ context.Context, c providertest.Call) error {
					if c.Method == providertest.MethodFetchSummaries && c.Role == provider.RoleInteractive &&
						once.CompareAndSwap(false, true) {
						tc.reading(b)
					}
					return nil
				})
			}
			res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{one, two}, To: strconv.FormatInt(work, 10)})
			b.box.OnCall(nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Messages) != 0 || len(res.Removed) != 2 {
				t.Fatalf("result = %+v, want both rows out of the index until Work's pass", res)
			}
			b.m.index(t, b.id, b.box)
			for uid, subject := range b.subjectsIn(t, "Work") {
				id := b.rowBySubject(t, "Work", subject)
				if id == 0 {
					t.Fatalf("Work's pass did not index %q at UID %d", subject, uid)
				}
				b.neverAnother(t, id, subject)
			}
		})
	}
}
