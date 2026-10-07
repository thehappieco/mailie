package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
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
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// actionBox is one person's mailbox on a fake server, indexed as sync would
// have it, for the actions to change.
type actionBox struct {
	m     *mailFixture
	owner service.Principal
	id    string
	box   *providertest.FakeMailbox
}

// ownedBox registers a mailbox of kind on a fake server with o, owned by
// owner (the operator's when owner is the zero principal), with sync on.
func (m *mailFixture) ownedBox(t *testing.T, owner service.Principal, email string, o providertest.FakeOptions) (string, *providertest.FakeMailbox) {
	t.Helper()
	return m.ownedBoxIn(t, owner, "", email, o)
}

// ownedBoxIn is ownedBox linked into a workspace of the owner's: empty is
// their personal workspace.
func (m *mailFixture) ownedBoxIn(t *testing.T, owner service.Principal, workspaceID, email string, o providertest.FakeOptions) (string, *providertest.FakeMailbox) {
	t.Helper()
	kind := o.Kind
	if kind == "" {
		kind = provider.KindIMAP
	}
	id := fmt.Sprintf("acc_%016x", len(m.boxes)+100)
	a := account.Account{
		ID: id, WorkspaceID: workspaceID, Email: email, Provider: kind, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}
	if kind == provider.KindGmail {
		a.AuthKind, a.IMAPHost, a.SMTPHost = "oauth2", "imap.gmail.com", "smtp.gmail.com"
	}
	created, err := m.repo.Create(t.Context(), a, owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case owner.UserID == "":
		if _, err := m.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
			t.Fatal(err)
		}
	case created.OwnerUserID == "":
		// A team's mailbox syncs under the team's consent, which its
		// linker, an owner or an admin, gives.
		on := true
		if _, err := m.svc.SetMailboxSync(t.Context(), owner, id,
			service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync}); err != nil {
			t.Fatal(err)
		}
	default:
		if _, err := m.svc.GrantSyncConsent(t.Context(), owner, m.consent().Sync); err != nil {
			t.Fatal(err)
		}
	}
	box := providertest.NewFakeMailbox(o)
	m.mu.Lock()
	m.boxes[id] = box
	m.mu.Unlock()
	return id, box
}

// genericBox is ana's mailbox on a server with MOVE and UIDPLUS: an inbox
// with two messages, an archive, a trash, sent mail and a folder of hers.
// She has allowed actions.
func genericBox(t *testing.T) *actionBox {
	t.Helper()
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Archive", imap.MailboxAttrArchive)
	box.CreateFolder("Trash", imap.MailboxAttrTrash)
	box.CreateFolder("Sent", imap.MailboxAttrSent)
	box.CreateFolder("Work")
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	box.Deliver("INBOX", message("b", "Quarterly numbers"))
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	return b
}

// gmailBox is ana's Gmail: the message "a" in the inbox, under the label
// Work, and in All Mail, which is not synced. Folders are labels: a message
// is in one at most once.
func gmailBox(t *testing.T) *actionBox {
	t.Helper()
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@gmail.com", providertest.FakeOptions{
		Kind: provider.KindGmail, Caps: providertest.GmailCaps(), SharedFlags: true, Labels: true,
	})
	box.CreateFolder("[Gmail]/All Mail", imap.MailboxAttrAll)
	box.CreateFolder("[Gmail]/Trash", imap.MailboxAttrTrash)
	box.CreateFolder("[Gmail]/Sent Mail", imap.MailboxAttrSent)
	box.CreateFolder("Work")
	uid := box.Deliver("INBOX", message("a", "Lunch on Friday"))
	box.CopyTo("INBOX", uid, "Work")
	box.CopyTo("INBOX", uid, "[Gmail]/All Mail")
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	return b
}

func message(key, subject string) providertest.FakeMessage {
	return providertest.FakeMessage{
		MessageID: key + "@example.org", Subject: subject, From: "Bea Lima <bea@example.org>",
		To: []string{"ana@mail.example"}, InternalDate: time.Now().Add(-time.Hour),
	}
}

func (b *actionBox) allow(t *testing.T) {
	t.Helper()
	if _, err := b.m.svc.GrantActionsConsent(t.Context(), b.owner, b.m.consent().Actions); err != nil {
		t.Fatal(err)
	}
}

// row is the id of the row indexed for the message key in folder.
func (b *actionBox) row(t *testing.T, folder, key string) int64 {
	t.Helper()
	var id int64
	err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.name = ? AND m.message_id = ?`, b.id, folder, key+"@example.org").Scan(&id)
	if err != nil {
		t.Fatalf("no row for %s in %s: %v", key, folder, err)
	}
	return id
}

func (b *actionBox) folder(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT id FROM folders WHERE account_id = ? AND name = ?`,
		b.id, name).Scan(&id); err != nil {
		t.Fatalf("no folder %s: %v", name, err)
	}
	return id
}

// on reports whether the server holds the message key in folder.
func (b *actionBox) on(folder, key string) bool {
	st, ok := b.box.Folder(folder)
	if !ok {
		return false
	}
	return len(st.UIDs) > 0 && b.serverUID(folder, key) != 0
}

func (b *actionBox) serverUID(folder, key string) imap.UID {
	st, _ := b.box.Folder(folder)
	sess, err := b.box.Open(context.Background(), provider.RoleSync)
	if err != nil {
		return 0
	}
	defer func() { _ = sess.Close() }()
	if _, err := sess.Select(context.Background(), folder, true, st.UIDValidity); err != nil {
		return 0
	}
	uids, err := sess.SearchMessageID(context.Background(), key+"@example.org")
	if err != nil || len(uids) == 0 {
		return 0
	}
	return uids[0]
}

// journaled lists the account's events of one type since seq.
func (b *actionBox) journaled(t *testing.T, since int64, typ events.Type) []json.RawMessage {
	t.Helper()
	rows, err := b.m.db.Reader().QueryContext(t.Context(),
		`SELECT payload_json FROM events WHERE account_id = ? AND type = ? AND seq > ? ORDER BY seq`, b.id, string(typ), since)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []json.RawMessage
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, json.RawMessage(p))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (b *actionBox) lastSeq(t *testing.T) int64 {
	t.Helper()
	return int64(b.m.count(t, `SELECT coalesce(max(seq), 0) FROM events`))
}

// serverCalls counts the calls that reached the fake server of one method.
func (b *actionBox) serverCalls(method providertest.Method) int { return b.box.CallCount(method) }

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestAReadKeyCannotAct(t *testing.T) {
	b := genericBox(t)
	a := b.row(t, "INBOX", "a")
	read := keyOf(t, b.m.fixture, b.owner, auth.ScopeRead)
	ctx := t.Context()
	_, err := b.m.svc.SetFlags(ctx, read, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "marking read with a read key", err, service.CodeNotAuthorized)
	_, err = b.m.svc.MoveMessages(ctx, read, service.MoveRequest{IDs: []int64{a}, To: "archive"})
	wantCode(t, "archiving with a read key", err, service.CodeNotAuthorized)
	_, err = b.m.svc.TrashMessages(ctx, read, service.TrashRequest{IDs: []int64{a}})
	wantCode(t, "trashing with a read key", err, service.CodeNotAuthorized)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("a read key's attempts opened %d connections", n)
	}
}

func TestNobodyActsWithoutTheirOwnCurrentConsent(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	if _, err := b.m.svc.WithdrawActionsConsent(ctx, b.owner); err != nil {
		t.Fatal(err)
	}
	a := b.row(t, "INBOX", "a")
	// Her session; a key of her workspace acts under its key terms instead
	// (TestAKeyActsOnlyWithActAndTheWriteScopeUnderItsKeyTerms).
	_, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "her session without consent", err, service.CodeConflict)
	if msg := service.MessageOf(err); msg != "actions are off: you have not allowed them in the console" {
		t.Errorf("message = %q", msg)
	}
	// Consent to an older text is not consent to this one.
	b.m.exec(t, `UPDATE users SET actions_consent_at = 5, actions_consent_version = '2025-01-older' WHERE id = ?`,
		b.owner.UserID)
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "archive"})
	wantCode(t, "acting on an older consent", err, service.CodeConflict)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("refused actions opened %d connections", n)
	}

	b.allow(t)
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatalf("with consent, her session: %v", err)
	}
}

func TestAnInstanceKeyCannotChangeAPersonsMessages(t *testing.T) {
	// An instance key reaches the operator workspace's mailboxes and no
	// person's: to it, a person's messages do not exist, and the policy
	// promises only the person acting changes them.
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	for _, who := range []struct {
		name string
		p    service.Principal
	}{
		{"an instance admin key", admin()},
		{"an instance write key", service.Principal{KeyPrefix: "dddddddd", Scope: auth.ScopeWrite, WorkspaceID: workspace.OperatorID}},
	} {
		_, err := b.m.svc.SetFlags(ctx, who.p, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
		wantCode(t, who.name, err, service.CodeNotFound)
		_, err = b.m.svc.TrashMessages(ctx, who.p, service.TrashRequest{IDs: []int64{a}})
		wantCode(t, who.name+" trashing", err, service.CodeNotFound)
	}
	// Another person, even an owner of the instance, cannot see it at all.
	bea := b.m.person(t, "bea@example.com", auth.RoleOwner)
	_, err := b.m.svc.MoveMessages(ctx, bea, service.MoveRequest{IDs: []int64{a}, To: "archive"})
	wantCode(t, "another person", err, service.CodeNotFound)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("refused actions opened %d connections", n)
	}

	res, err := b.m.svc.SetFlags(ctx, keyOf(t, b.m.fixture, b.owner, auth.ScopeWrite),
		service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	if err != nil || len(res.Messages) != 1 || !res.Messages[0].Seen {
		t.Fatalf("a write key of her workspace: %+v, %v", res, err)
	}
}

func TestAMailboxNobodyOwnsIsChangedOnlyByTheOperator(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.ownedBox(t, service.Principal{}, "ops@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Status"))
	m.index(t, id, box)
	b := &actionBox{m: m, id: id, box: box}
	a := b.row(t, "INBOX", "a")
	owner := m.person(t, "olga@example.com", auth.RoleOwner)
	member := m.person(t, "mo@example.com", auth.RoleMember)
	ctx := t.Context()

	operator := service.Principal{KeyPrefix: "dddddddd", Scope: auth.ScopeWrite, WorkspaceID: workspace.OperatorID}
	if _, err := m.svc.SetFlags(ctx, operator, service.SetFlagsRequest{IDs: []int64{a}, Flagged: yes()}); err != nil {
		t.Fatalf("an instance write key: %v", err)
	}
	// An owner of the instance administers its people, not the operator
	// workspace's mail: it does not exist for her, signed in or by key.
	for name, p := range map[string]service.Principal{
		"an owner signed in": owner, "a key of an owner's workspace": keyOf(t, m.fixture, owner, auth.ScopeWrite),
		"a member": member,
	} {
		_, err := m.svc.SetFlags(ctx, p, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
		wantCode(t, name, err, service.CodeNotFound)
	}
}

func TestWithdrawingActionsStopsThemAtOnce(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	opened := b.box.Opens(provider.RoleInteractive)
	c, err := b.m.svc.WithdrawActionsConsent(ctx, b.owner)
	if err != nil || c.Consented {
		t.Fatalf("withdraw = %+v, %v", c, err)
	}
	_, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: no()})
	wantCode(t, "acting after withdrawing", err, service.CodeConflict)
	if n := b.box.Opens(provider.RoleInteractive); n != opened {
		t.Fatal("an action after the withdrawal reached the server")
	}
	// Nothing was deleted: the index is as it was.
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, b.id); n != 2 {
		t.Errorf("%d rows after withdrawing actions", n)
	}
}

func TestAPersonCannotActOnAnothersMessage(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	bob := b.m.person(t, "bob@example.com", auth.RoleMember)
	bobs, bobBox := b.m.ownedBox(t, bob, "bob@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	bobBox.Deliver("INBOX", message("x", "Bob's"))
	b.m.index(t, bobs, bobBox)
	if _, err := b.m.svc.GrantActionsConsent(ctx, bob, service.DefaultActionsConsentVersion); err != nil {
		t.Fatal(err)
	}
	anas := b.row(t, "INBOX", "a")
	var own int64
	if err := b.m.db.Reader().QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ?`, bobs).Scan(&own); err != nil {
		t.Fatal(err)
	}

	_, err := b.m.svc.SetFlags(ctx, bob, service.SetFlagsRequest{IDs: []int64{anas}, Seen: yes()})
	wantCode(t, "bob marking ana's message", err, service.CodeNotFound)
	_, err = b.m.svc.MoveMessages(ctx, bob, service.MoveRequest{IDs: []int64{own, anas}, To: "inbox"})
	wantCode(t, "bob moving his and ana's", err, service.CodeNotFound)
	_, err = b.m.svc.TrashMessages(ctx, bob, service.TrashRequest{IDs: []int64{anas}})
	wantCode(t, "bob trashing ana's message", err, service.CodeNotFound)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatal("bob's attempts reached ana's server")
	}
}

func TestMarkingReadStoresOnTheServerAndRecordsWhatItEchoed(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	uid := b.serverUID("INBOX", "a")
	since := b.lastSeq(t)
	b.box.ResetCalls()

	res, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes(), Flagged: yes()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].ID != a || !res.Messages[0].Seen || !res.Messages[0].Flagged ||
		len(res.Removed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if flags := b.box.Flags("INBOX", uid); !slices.Contains(flags, `\seen`) || !slices.Contains(flags, `\flagged`) {
		t.Fatalf("the server holds %v", flags)
	}
	var stores []providertest.Call
	for _, c := range b.box.Calls() {
		switch c.Method {
		case providertest.MethodSelect:
			if c.ReadOnly || c.Folder != "INBOX" || c.Role != provider.RoleInteractive {
				t.Errorf("select = %+v, want INBOX read-write on the interactive connection", c)
			}
		case providertest.MethodStoreFlags:
			stores = append(stores, c)
		}
	}
	if len(stores) != 1 || stores[0].Set != strconv.Itoa(int(uid)) || stores[0].Flags != `+FLAGS (\flagged \seen)` {
		t.Fatalf("stores = %+v", stores)
	}
	if evs := b.journaled(t, since, events.TypeMessageFlags); len(evs) != 1 {
		t.Fatalf("%d message.flags, want 1", len(evs))
	}
	if seen := b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, other); seen != 0 {
		t.Error("the other message was marked read")
	}

	// And back: one STORE removes both.
	res, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: no(), Flagged: no()})
	if err != nil || res.Messages[0].Seen || res.Messages[0].Flagged {
		t.Fatalf("unmarking: %+v, %v", res, err)
	}
	if flags := b.box.Flags("INBOX", uid); len(flags) != 0 {
		t.Fatalf("the server still holds %v", flags)
	}
	// Opening a message still does not mark it read.
	if _, err := b.m.svc.GetMessage(ctx, b.owner, service.GetMessageRequest{ID: a}); err != nil {
		t.Fatal(err)
	}
	if flags := b.box.Flags("INBOX", uid); len(flags) != 0 {
		t.Fatalf("reading marked the message: %v", flags)
	}
}

func TestFlagsOnGmailReachEveryLabelCopy(t *testing.T) {
	b := gmailBox(t)
	inbox, work := b.row(t, "INBOX", "a"), b.row(t, "Work", "a")
	since := b.lastSeq(t)
	res, err := b.m.svc.SetFlags(t.Context(), b.owner, service.SetFlagsRequest{IDs: []int64{inbox}, Seen: yes()})
	if err != nil || len(res.Messages) != 1 || !res.Messages[0].Seen {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if seen := b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, work); seen != 1 {
		t.Fatal("the Work label's copy still reads unread")
	}
	if evs := b.journaled(t, since, events.TypeMessageFlags); len(evs) != 2 {
		t.Fatalf("%d message.flags, want one per copy", len(evs))
	}
	if n := b.serverCalls(providertest.MethodStoreFlags); n != 1 {
		t.Errorf("%d STOREs, want 1: Gmail changes the message, not the copy", n)
	}
}

func TestArchiveOnGmailMovesOnlyTheInboxCopyAndTheRowLeavesTheIndex(t *testing.T) {
	b := gmailBox(t)
	ctx := t.Context()
	inbox, work := b.row(t, "INBOX", "a"), b.row(t, "Work", "a")
	since := b.lastSeq(t)
	b.box.ResetCalls()

	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{inbox}, To: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 0 || len(res.Removed) != 1 || res.Removed[0] != inbox {
		t.Fatalf("result = %+v, want the inbox row removed", res)
	}
	var moves []providertest.Call
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodMove || c.Method == providertest.MethodCopy {
			moves = append(moves, c)
		}
	}
	if len(moves) != 1 || moves[0].Method != providertest.MethodMove || moves[0].Folder != "INBOX" ||
		moves[0].Dest != "[Gmail]/All Mail" {
		t.Fatalf("server calls = %+v, want one MOVE from the inbox to All Mail", moves)
	}
	if b.on("INBOX", "a") || !b.on("Work", "a") {
		t.Fatal("the server does not hold the message under Work only")
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE id = ?`, inbox); n != 0 {
		t.Fatal("the inbox row is still indexed")
	}
	if primary := b.m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND dup_of IS NULL`, work); primary != 1 {
		t.Fatal("the Work copy did not become the one listings show")
	}
	moved := b.journaled(t, since, events.TypeMessageMoved)
	if len(moved) != 1 {
		t.Fatalf("%d message.moved", len(moved))
	}
	var payload store.ActionMoved
	if err := json.Unmarshal(moved[0], &payload); err != nil {
		t.Fatal(err)
	}
	if payload.To != nil || payload.ToFolderID != nil || payload.MessageID != inbox || payload.PrimaryID != work {
		t.Errorf("payload = %s", moved[0])
	}
	for _, typ := range []events.Type{events.TypeMessageDeleted, events.TypeMessageNew} {
		if n := len(b.journaled(t, since, typ)); n != 0 {
			t.Errorf("an archive announced %d %s", n, typ)
		}
	}
}

func TestArchivingALabelCopyOnGmailIsRefused(t *testing.T) {
	b := gmailBox(t)
	_, err := b.m.svc.MoveMessages(t.Context(), b.owner, service.MoveRequest{
		IDs: []int64{b.row(t, "Work", "a")}, To: "archive",
	})
	wantCode(t, "archiving a label copy", err, service.CodeBadRequest)
	// All Mail by id is archiving too.
	_, err = b.m.svc.MoveMessages(t.Context(), b.owner, service.MoveRequest{
		IDs: []int64{b.row(t, "Work", "a")}, To: strconv.FormatInt(b.folder(t, "[Gmail]/All Mail"), 10),
	})
	wantCode(t, "moving a label copy to All Mail", err, service.CodeBadRequest)
	if n := b.serverCalls(providertest.MethodMove); n != 0 {
		t.Fatal("a refused archive reached the server")
	}
}

func TestUndoingAGmailArchiveCopiesTheMessageBackIntoTheInbox(t *testing.T) {
	// The console undoes an archive by moving the id it got back in
	// Removed to the inbox. On Gmail that is a COPY out of All Mail — adding
	// the Inbox label — never a MOVE, which would take the message out of
	// the mailbox.
	b := gmailBox(t)
	ctx := t.Context()
	inbox := b.row(t, "INBOX", "a")
	archived, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{inbox}, To: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	since := b.lastSeq(t)
	b.box.ResetCalls()

	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: archived.Removed, To: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].FolderRole != "inbox" || res.Messages[0].ID == inbox ||
		len(res.Removed) != 0 {
		t.Fatalf("result = %+v, want the message back in the inbox under a new id", res)
	}
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodMove {
			t.Fatalf("a MOVE was sent to undo an archive: %+v", c)
		}
	}
	if n := b.serverCalls(providertest.MethodCopy); n != 1 {
		t.Fatalf("%d COPYs, want 1", n)
	}
	if !b.on("INBOX", "a") || !b.on("[Gmail]/All Mail", "a") || !b.on("Work", "a") {
		t.Fatal("the message is not back in the inbox with its other labels")
	}
	if n := len(b.journaled(t, since, events.TypeMessageNew)); n != 0 {
		t.Fatalf("undoing an archive announced %d new messages", n)
	}
	// The id is spent: a second undo finds nothing.
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: archived.Removed, To: "inbox"})
	wantCode(t, "a second undo", err, service.CodeNotFound)
}

func TestTrashMovesAndNeverExpungesAnythingButTheMovedUID(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	marked := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "marked@example.org", Subject: "Marked by the phone", Flags: []imap.Flag{imap.FlagDeleted},
	})
	a := b.row(t, "INBOX", "a")
	uid := b.serverUID("INBOX", "a")
	since := b.lastSeq(t)
	b.box.ResetCalls()

	res, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderRole != "trash" {
		t.Fatalf("result = %+v, want the row in the trash under its id", res)
	}
	var moves []providertest.Call
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodMove {
			moves = append(moves, c)
		}
	}
	if len(moves) != 1 || moves[0].Set != strconv.Itoa(int(uid)) || moves[0].Dest != "Trash" {
		t.Fatalf("moves = %+v, want one MOVE of exactly UID %d to the trash", moves, uid)
	}
	inbox, _ := b.box.Folder("INBOX")
	if !slices.Contains(inbox.UIDs, marked) || len(inbox.UIDs) != 2 {
		t.Fatalf("the inbox holds %v: something besides the trashed message left it", inbox.UIDs)
	}
	if !b.on("Trash", "a") {
		t.Fatal("the message is not in the trash")
	}
	if n := len(b.journaled(t, since, events.TypeMessageDeleted)); n != 0 {
		t.Fatalf("moving to the trash announced %d deletions", n)
	}
}

func TestTrashInTheTrashIsRefused(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	if _, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}}); err != nil {
		t.Fatal(err)
	}
	moves := b.serverCalls(providertest.MethodMove)
	_, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}})
	wantCode(t, "trashing a message in the trash", err, service.CodeConflict)
	if b.serverCalls(providertest.MethodMove) != moves || !b.on("Trash", "a") {
		t.Fatal("a message in the trash was touched")
	}
}

func TestWithoutMOVEAndUIDPLUSAMoveIsRefusedAndNothingIsExpunged(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	b.box.SetCaps(provider.Caps{CondStore: true, SpecialUse: true})
	a := b.row(t, "INBOX", "a")
	b.box.ResetCalls()
	_, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}})
	wantCode(t, "trashing without MOVE and UIDPLUS", err, service.CodeConflict)
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "archive"})
	wantCode(t, "archiving without MOVE and UIDPLUS", err, service.CodeConflict)
	for _, c := range b.box.Calls() {
		switch c.Method {
		case providertest.MethodMove, providertest.MethodCopy, providertest.MethodStoreFlags:
			t.Fatalf("a refused move sent %+v", c)
		}
	}
	if inbox, _ := b.box.Folder("INBOX"); len(inbox.UIDs) != 2 {
		t.Fatalf("the inbox holds %v", inbox.UIDs)
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND folder_id = ?`, a, b.folder(t, "INBOX")); n != 1 {
		t.Fatal("the index moved a message the server did not")
	}
}

func TestAMoveWithoutUIDPLUSFindsTheMessageByItsMessageIDAndKeepsItsID(t *testing.T) {
	b := genericBox(t)
	caps := providertest.GmailCaps()
	caps.UIDPlus = false
	b.box.SetCaps(caps)
	a := b.row(t, "INBOX", "a")
	work := b.folder(t, "Work")
	res, err := b.m.svc.MoveMessages(t.Context(), b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].ID != a || res.Messages[0].FolderID != work {
		t.Fatalf("result = %+v, want the row in Work under its id", res)
	}
	if n := b.serverCalls(providertest.MethodSearch); n != 1 {
		t.Errorf("%d searches by Message-ID, want 1", n)
	}
	if uid := b.m.count(t, `SELECT uid FROM messages WHERE id = ?`, a); imap.UID(uid) != b.serverUID("Work", "a") {
		t.Errorf("the row points at UID %d in Work", uid)
	}
}

func TestMovingAMessageToTheFolderItIsInChangesNothing(t *testing.T) {
	b := genericBox(t)
	a := b.row(t, "INBOX", "a")
	res, err := b.m.svc.MoveMessages(t.Context(), b.owner, service.MoveRequest{IDs: []int64{a}, To: "inbox"})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != a {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatal("a move to where the message is reached the server")
	}
}

func TestAMoveGoesOnlyWhereAMessageCanBeMoved(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	bob := b.m.person(t, "bob@example.com", auth.RoleMember)
	bobs, bobBox := b.m.ownedBox(t, bob, "bob@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	bobBox.CreateFolder("Elsewhere")
	b.m.index(t, bobs, bobBox)
	var elsewhere int64
	if err := b.m.db.Reader().QueryRowContext(ctx, `SELECT id FROM folders WHERE account_id = ? AND name = 'Elsewhere'`,
		bobs).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		to   string
		code service.Code
	}{
		{"trash", service.CodeBadRequest},
		{strconv.FormatInt(b.folder(t, "Trash"), 10), service.CodeBadRequest},
		{strconv.FormatInt(b.folder(t, "Sent"), 10), service.CodeBadRequest},
		{strconv.FormatInt(elsewhere, 10), service.CodeBadRequest},
		{"99999", service.CodeBadRequest},
		{"spam", service.CodeBadRequest},
		{"", service.CodeBadRequest},
	} {
		_, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: tc.to})
		wantCode(t, "moving to "+strconv.Quote(tc.to), err, tc.code)
	}
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatal("a refused destination reached the server")
	}

	// No archive folder: nothing to archive to.
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@plain.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Hi"))
	m.index(t, id, box)
	plain := &actionBox{m: m, owner: ana, id: id, box: box}
	plain.allow(t)
	_, err := m.svc.MoveMessages(ctx, ana, service.MoveRequest{IDs: []int64{plain.row(t, "INBOX", "a")}, To: "archive"})
	wantCode(t, "archiving without an archive folder", err, service.CodeConflict)
	_, err = m.svc.TrashMessages(ctx, ana, service.TrashRequest{IDs: []int64{plain.row(t, "INBOX", "a")}})
	wantCode(t, "trashing without a trash folder", err, service.CodeConflict)
}

func TestAnActionNamesOneToAHundredMessagesOfOneAccount(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	_, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{Seen: yes()})
	wantCode(t, "no ids", err, service.CodeBadRequest)
	many := make([]int64, 101)
	for i := range many {
		many[i] = int64(i + 1)
	}
	_, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: many, Seen: yes()})
	wantCode(t, "101 ids", err, service.CodeBadRequest)
	_, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{b.row(t, "INBOX", "a")}})
	wantCode(t, "nothing to change", err, service.CodeBadRequest)

	second, box := b.m.ownedBox(t, b.owner, "ana@second.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("z", "Other account"))
	b.m.index(t, second, box)
	var other int64
	if err := b.m.db.Reader().QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ?`, second).Scan(&other); err != nil {
		t.Fatal(err)
	}
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{b.row(t, "INBOX", "a"), other}, To: "inbox"})
	wantCode(t, "two accounts", err, service.CodeBadRequest)
}

func TestAStaleMessageIsBusyAndAVanishedOneIsGone(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	b.m.exec(t, `UPDATE messages SET stale = 1 WHERE id = ?`, a)
	b.m.exec(t, `UPDATE messages SET vanished_at = 1 WHERE id = ?`, other)
	_, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "a stale message", err, service.CodeConflict)
	_, err = b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{other}})
	wantCode(t, "a vanished message", err, service.CodeNotFound)
	_, err = b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{987654}, To: "inbox"})
	wantCode(t, "a message that does not exist", err, service.CodeNotFound)
}

func TestEveryActionGoesThroughTheInteractiveRunner(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	syncOpens := b.box.Opens(provider.RoleSync)
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a, other}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: "archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{other}}); err != nil {
		t.Fatal(err)
	}
	b.m.engine.mu.Lock()
	lent := append([]string(nil), b.m.engine.lent...)
	b.m.engine.mu.Unlock()
	if len(lent) != 3 {
		t.Fatalf("the engine lent its connection %d times for three actions: %v", len(lent), lent)
	}
	if n := b.box.Opens(provider.RoleInteractive); n != 3 {
		t.Errorf("%d interactive connections for three actions", n)
	}
	if n := b.box.Opens(provider.RoleSync) + b.box.Opens(provider.RoleIdle); n != syncOpens {
		t.Errorf("an action opened a sync or idle connection")
	}
	if peak := b.box.PeakSessions(provider.RoleInteractive); peak > 1 {
		t.Errorf("%d interactive connections at once", peak)
	}
}

func TestAPassRacingAMoveDoesNotResurrectTheSourceRow(t *testing.T) {
	// A pass fetched the inbox's summary of a message, and applies it after
	// the message has moved to Work. Storing it would put the row back in
	// the inbox and announce a copy nobody made.
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	inbox, _ := b.box.Folder("INBOX")
	uid := b.serverUID("INBOX", "a")
	pass, err := b.box.Open(ctx, provider.RoleSync)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	if _, err := pass.Select(ctx, "INBOX", true, inbox.UIDValidity); err != nil {
		t.Fatal(err)
	}
	var fetched []provider.Summary
	if err := pass.FetchSummaries(ctx, imap.UIDSetNum(uid), 0, func(s provider.Summary) error {
		fetched = append(fetched, s)
		return nil
	}); err != nil || len(fetched) != 1 {
		t.Fatalf("fetch = %v, %v", fetched, err)
	}

	work := b.folder(t, "Work")
	if _, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(work, 10)}); err != nil {
		t.Fatal(err)
	}
	since := b.lastSeq(t)
	inboxID := b.folder(t, "INBOX")
	var res store.ApplyResult
	if err := b.m.db.Write(ctx, func(tx *sql.Tx) error {
		res, err = b.m.db.ApplySummaries(ctx, tx, store.SummaryBatch{
			AccountID: b.id, FolderID: inboxID, UIDValidity: inbox.UIDValidity, Mode: store.ApplyLive,
			Summaries: fetched,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 0 || res.Moved != 1 {
		t.Fatalf("the late summary did %+v", res)
	}
	if n := b.m.count(t, `SELECT count(*) FROM messages WHERE account_id = ? AND message_id = 'a@example.org'`, b.id); n != 1 {
		t.Fatalf("%d rows for the moved message", n)
	}
	if folder := b.m.count(t, `SELECT folder_id FROM messages WHERE id = ?`, a); int64(folder) != work {
		t.Fatal("the moved row is not in Work")
	}
	if n := b.m.count(t, `SELECT count(*) FROM events WHERE seq > ?`, since); n != 0 {
		t.Fatalf("the late summary announced %d events", n)
	}
}

func TestAFailureHalfwayKeepsOnlyWhatTheServerConfirmed(t *testing.T) {
	// Two folders, two MOVEs. The second fails: the first is in the index,
	// the second is not claimed.
	b := genericBox(t)
	ctx := t.Context()
	w := b.box.Deliver("Work", message("w", "Project"))
	b.m.index(t, b.id, b.box)
	_ = w
	a, work := b.row(t, "INBOX", "a"), b.row(t, "Work", "w")
	b.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodMove && c.Folder == "Work" {
			return fmt.Errorf("%w: refused", provider.ErrTemporary)
		}
		return nil
	})
	_, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a, work}, To: "archive"})
	if err == nil {
		t.Fatal("a failed move reported success")
	}
	archive := b.folder(t, "Archive")
	if folder := b.m.count(t, `SELECT folder_id FROM messages WHERE id = ?`, a); int64(folder) != archive {
		t.Error("the move the server confirmed is not in the index")
	}
	if folder := b.m.count(t, `SELECT folder_id FROM messages WHERE id = ?`, work); int64(folder) != b.folder(t, "Work") {
		t.Error("the move the server refused is in the index")
	}
}

func TestActionsLogNoSubjectAddressOrFolderName(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{a}, To: strconv.FormatInt(b.folder(t, "Work"), 10)}); err != nil {
		t.Fatal(err)
	}
	b.box.FailNext(providertest.MethodMove, fmt.Errorf("%w: the connection dropped", provider.ErrConnClosed))
	if _, err := b.m.svc.TrashMessages(ctx, b.owner, service.TrashRequest{IDs: []int64{a}}); err == nil {
		t.Fatal("the injected failure did not surface")
	}
	logs := b.m.logs.String()
	for _, secret := range []string{"Lunch on Friday", "bea@example.org", "Bea Lima", "a@example.org", "\"Work\"", "Trash", "Archive"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries %q:\n%s", secret, logs)
		}
	}
}

func TestAccountsSayWhichActionsTheyCanOffer(t *testing.T) {
	generic := genericBox(t)
	a, err := generic.m.svc.GetAccount(t.Context(), generic.owner, generic.id)
	if err != nil || !a.Actions.Archive || !a.Actions.Trash {
		t.Fatalf("a mailbox with an archive and a trash offers %+v (%v)", a.Actions, err)
	}
	gmail := gmailBox(t)
	if a, err := gmail.m.svc.GetAccount(t.Context(), gmail.owner, gmail.id); err != nil || !a.Actions.Archive || !a.Actions.Trash {
		t.Fatalf("Gmail with All Mail and a trash offers %+v (%v)", a.Actions, err)
	}
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@plain.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	if a, err := m.svc.GetAccount(t.Context(), ana, id); err != nil || a.Actions.Archive || a.Actions.Trash {
		t.Fatalf("a mailbox with nothing indexed offers %+v (%v)", a.Actions, err)
	}
	box.CreateFolder("Trash", imap.MailboxAttrTrash)
	m.index(t, id, box)
	if a, err := m.svc.GetAccount(t.Context(), ana, id); err != nil || a.Actions.Archive || !a.Actions.Trash {
		t.Fatalf("a mailbox with a trash and no archive offers %+v (%v)", a.Actions, err)
	}
	if a, _ := gmail.m.svc.GetAccount(t.Context(), gmail.owner, gmail.id); a.Actions.ArchiveReason != "" {
		t.Errorf("Gmail with All Mail names a reason not to archive: %q", a.Actions.ArchiveReason)
	}
}

func TestAGmailMailboxThatHidesAllMailSaysSo(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@gmail.com", providertest.FakeOptions{
		Kind: provider.KindGmail, Caps: providertest.GmailCaps(), SharedFlags: true, Labels: true,
	})
	if a, err := m.svc.GetAccount(t.Context(), ana, id); err != nil || a.Actions.ArchiveReason != "" {
		t.Fatalf("before its folders were read, the mailbox gives a reason: %+v (%v)", a.Actions, err)
	}
	box.CreateFolder("[Gmail]/Trash", imap.MailboxAttrTrash)
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	m.index(t, id, box)
	a, err := m.svc.GetAccount(t.Context(), ana, id)
	if err != nil || a.Actions.Archive || a.Actions.ArchiveReason != service.ArchiveReasonAllMailHidden {
		t.Fatalf("Gmail without All Mail offers %+v (%v), want no archive and the reason %q",
			a.Actions, err, service.ArchiveReasonAllMailHidden)
	}
}

func TestActionsConsentIsThePersonsAndNamesTheCurrentPolicy(t *testing.T) {
	m := newMailFixture(t)
	ctx := t.Context()
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	c, err := m.svc.ActionsConsent(ctx, ana)
	if err != nil || c.Consented || c.CurrentVersion != service.DefaultActionsConsentVersion {
		t.Fatalf("before: %+v, %v", c, err)
	}
	_, err = m.svc.GrantActionsConsent(ctx, ana, "2026-01-older")
	wantCode(t, "an older text", err, service.CodeBadRequest)
	key := keyOf(t, m.fixture, ana, auth.ScopeAdmin)
	_, err = m.svc.GrantActionsConsent(ctx, key, service.DefaultActionsConsentVersion)
	wantCode(t, "a key agreeing for her", err, service.CodeNotAuthorized)
	_, err = m.svc.ActionsConsent(ctx, admin())
	wantCode(t, "an instance key reading", err, service.CodeNotAuthorized)
	// A key of her workspace acts as nobody: it has no consent to read.
	_, err = m.svc.ActionsConsent(ctx, keyOf(t, m.fixture, ana, auth.ScopeRead))
	wantCode(t, "a key of her workspace reading", err, service.CodeNotAuthorized)
	c, err = m.svc.GrantActionsConsent(ctx, ana, service.DefaultActionsConsentVersion)
	if err != nil || !c.Consented || c.Version != service.DefaultActionsConsentVersion || c.ConsentedAt == 0 {
		t.Fatalf("grant: %+v, %v", c, err)
	}
	_, err = m.svc.WithdrawActionsConsent(ctx, key)
	wantCode(t, "a key withdrawing for her", err, service.CodeNotAuthorized)
	if sync, _ := m.svc.SyncConsent(ctx, ana); sync.Consented {
		t.Error("allowing actions turned sync on")
	}
}

func TestAnActionsEventsReachOnlyThoseWhoMaySeeTheMailbox(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	bob := b.m.person(t, "bob@example.com", auth.RoleMember)
	since := b.lastSeq(t)
	bobs, err := b.m.svc.Subscribe(ctx, bob, since, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer bobs.Close()
	anas, err := b.m.svc.Subscribe(ctx, b.owner, since, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer anas.Close()

	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{b.row(t, "INBOX", "a")}, Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-anas.Events():
		if ev.Type != string(events.TypeMessageFlags) || ev.AccountID != b.id {
			t.Fatalf("ana received %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ana's stream did not hear her own action")
	}
	select {
	case ev := <-bobs.Events():
		t.Fatalf("bob's stream heard ana's action: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}
