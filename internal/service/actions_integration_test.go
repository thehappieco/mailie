//go:build integration

// Actions on a real Dovecot, through the sync engine's own interactive
// connection: STORE with CONDSTORE, and MOVE into the folders SPECIAL-USE
// names, which the in-process server can only approximate.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration -run Dovecot ./internal/service/
package service_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestActionsOnDovecotChangeTheServerAndTheIndexFollowsWithoutNewMail(t *testing.T) {
	addr := os.Getenv("MAIL_IT_IMAP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:31143"
	}
	password := os.Getenv("MAIL_IT_IMAP_PASSWORD")
	if password == "" {
		password = "integration"
	}
	user := "actor-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	dovecot, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: addr,
		Credentials: provider.Credentials{User: user, Password: password},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The person's phone: two messages in the inbox, and one already
	// marked for deletion there, not yet expunged.
	phone, err := dovecot.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	defer func() { _ = phone.Close() }()
	for i, flags := range [][]imap.Flag{nil, nil, {imap.FlagDeleted}} {
		raw := "From: Bea <bea@example.org>\r\nTo: ana@example.com\r\nSubject: Message " + strconv.Itoa(i) + "\r\n" +
			"Message-ID: <act-" + strconv.Itoa(i) + "@example.org>\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
		if _, err := phone.Append(t.Context(), "INBOX", strings.NewReader(raw), int64(len(raw)), flags,
			time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	late := &lateEngine{}
	f := newFixtureWith(t, fixtureOptions{sync: late})
	counted := &countingMailbox{Mailbox: dovecot, opens: map[provider.Role]int{}}
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
	defer func() {
		stop()
		<-done
	}()

	ana := f.person(t, "ana@example.com", auth.RoleMember)
	const id = "acc_00000000000000e9"
	host, port := splitHostPort(t, addr)
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, Email: user, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: 31025, SMTPTLS: "starttls", LoginUser: user,
		State: account.StateActive,
	}, ana.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	b := &actionBox{m: &mailFixture{fixture: f}, owner: ana, id: id}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the initial sync", func() bool {
		st, err := late.m.Status(t.Context(), id)
		return err == nil && st.State == "live"
	})
	b.allow(t)
	ctx := t.Context()
	first, second := b.row(t, "INBOX", "act-0"), b.row(t, "INBOX", "act-1")
	since := b.lastSeq(t)

	// Read and starred, on the server as the phone sees it.
	res, err := f.svc.SetFlags(ctx, ana, service.SetFlagsRequest{IDs: []int64{first}, Seen: yes(), Flagged: yes()})
	if err != nil || !res.Messages[0].Seen || !res.Messages[0].Flagged {
		t.Fatalf("SetFlags = %+v, %v", res, err)
	}
	if _, err := phone.Select(ctx, "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := phone.FetchFlags(ctx, imap.UIDSetNum(1), 0)
	if err != nil || len(flags) != 1 || !seen(flags[0].Flags) || !hasFlag(flags[0].Flags, `\flagged`) {
		t.Fatalf("the phone sees %+v, %v", flags, err)
	}

	// Archived into the folder SPECIAL-USE calls \Archive, and the second
	// message into the trash: moves, under the ids they had.
	archive := b.folder(t, "Archive")
	res, err = f.svc.MoveMessages(ctx, ana, service.MoveRequest{IDs: []int64{first}, To: "archive"})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != first || res.Messages[0].FolderID != archive {
		t.Fatalf("archive = %+v, %v", res, err)
	}
	res, err = f.svc.TrashMessages(ctx, ana, service.TrashRequest{IDs: []int64{second}})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].ID != second || res.Messages[0].FolderRole != "trash" {
		t.Fatalf("trash = %+v, %v", res, err)
	}
	if _, err := phone.Select(ctx, "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	left, err := phone.UIDs(ctx, imap.UIDSet{{Start: 1, Stop: 0}}, time.Time{})
	if err != nil || len(left) != 1 || left[0] != 3 {
		t.Fatalf("the inbox holds %v (%v), want only the message the phone marked for deletion", left, err)
	}
	for folder, want := range map[string]string{"Archive": "act-0@example.org", "Trash": "act-1@example.org"} {
		if _, err := phone.Select(ctx, folder, true, 0); err != nil {
			t.Fatal(err)
		}
		if found, err := phone.SearchMessageID(ctx, want); err != nil || len(found) != 1 {
			t.Fatalf("%s holds %v (%v)", folder, found, err)
		}
	}

	// Back out of the trash into the inbox, as an undo does.
	inbox := b.folder(t, "INBOX")
	res, err = f.svc.MoveMessages(ctx, ana, service.MoveRequest{IDs: []int64{second}, To: strconv.FormatInt(inbox, 10)})
	if err != nil || res.Messages[0].ID != second || res.Messages[0].FolderRole != "inbox" {
		t.Fatalf("undo = %+v, %v", res, err)
	}

	// Let the engine's passes confirm it all.
	if err := late.m.Trigger(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor("the passes after the actions", func() bool {
		return f.count(t, `SELECT server_count FROM folders WHERE account_id = ? AND name = 'INBOX'`, id) == 2 &&
			f.count(t, `SELECT server_count FROM folders WHERE account_id = ? AND name = 'Archive'`, id) == 1 &&
			f.count(t, `SELECT server_count FROM folders WHERE account_id = ? AND name = 'Trash'`, id) == 0
	})
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted} {
		if evs := b.journaled(t, since, typ); len(evs) != 0 {
			t.Errorf("the actions produced %s: %s", typ, evs)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE account_id = ? AND message_id LIKE 'act-%'`, id); n != 3 {
		t.Errorf("%d rows for three messages", n)
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE id = ? AND seen = 1 AND flagged = 1 AND folder_id = ?`,
		first, archive); n != 1 {
		t.Error("the archived message lost its flags or its place in the index")
	}
	if n := counted.count(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections for five actions, want the engine's one", n)
	}
	counted.mu.Lock()
	peak := counted.peak
	counted.mu.Unlock()
	if peak > 3 {
		t.Errorf("the account held %d connections at once", peak)
	}
	if logs := f.logs.String(); strings.Contains(logs, "Message 0") || strings.Contains(logs, "bea@example.org") {
		t.Errorf("the log carries message content:\n%s", logs)
	}
}

func TestMovingSeveralMessagesOnDovecotLeavesEachRowOnItsOwnMessage(t *testing.T) {
	// A move of several messages reads where they landed back from the
	// destination and tells them apart by Message-ID, INTERNALDATE and size:
	// on a real server those must survive a MOVE for the rows to keep their
	// ids. Each row ends at its own message, and opening it reads its own
	// body — to the trash and back, three and two at a time.
	addr := os.Getenv("MAIL_IT_IMAP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:31143"
	}
	password := os.Getenv("MAIL_IT_IMAP_PASSWORD")
	if password == "" {
		password = "integration"
	}
	user := "pairs-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	dovecot, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: addr,
		Credentials: provider.Credentials{User: user, Password: password},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := dovecot.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	defer func() { _ = phone.Close() }()
	keys := []string{"pair-0", "pair-1", "pair-2"}
	for i, key := range keys {
		raw := "From: Bea <bea@example.org>\r\nTo: ana@example.com\r\nSubject: Pair " + strconv.Itoa(i) + "\r\n" +
			"Message-ID: <" + key + "@example.org>\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\nThe body of " + key + ".\r\n"
		if _, err := phone.Append(t.Context(), "INBOX", strings.NewReader(raw), int64(len(raw)), nil,
			time.Now().Add(-time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	const id = "acc_00000000000000ea"
	host, port := splitHostPort(t, addr)
	if _, err := m.repo.Create(t.Context(), account.Account{
		ID: id, Email: user, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: 31025, SMTPTLS: "starttls", LoginUser: user,
		State: account.StateActive,
	}, ana.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.boxes[id] = dovecot
	m.mu.Unlock()
	storetest.IndexMailbox(t, m.db, id, dovecot)
	b := &actionBox{m: m, owner: ana, id: id}
	b.allow(t)
	ctx := t.Context()
	rows := make([]int64, len(keys))
	for i, key := range keys {
		rows[i] = b.row(t, "INBOX", key)
	}

	onItsOwn := func(folder string) {
		t.Helper()
		if _, err := phone.Select(ctx, folder, true, 0); err != nil {
			t.Fatal(err)
		}
		for i, key := range keys {
			var (
				in  string
				uid uint32
			)
			if err := m.db.Reader().QueryRowContext(ctx, `SELECT f.name, m.uid FROM messages m
				JOIN folders f ON f.id = m.folder_id WHERE m.id = ?`, rows[i]).Scan(&in, &uid); err != nil {
				t.Fatalf("row %d (%s): %v", rows[i], key, err)
			}
			if in != folder {
				continue
			}
			found, err := phone.SearchMessageID(ctx, key+"@example.org")
			if err != nil || len(found) != 1 || found[0] != imap.UID(uid) {
				t.Fatalf("row %d (%s) points at UID %d in %s; the server holds it at %v (%v)", rows[i], key, uid, folder, found, err)
			}
			msg, err := m.svc.GetMessage(ctx, ana, service.GetMessageRequest{ID: rows[i]})
			if err != nil || msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, "The body of "+key+".") {
				t.Fatalf("opening row %d (%s) = %+v, %v", rows[i], key, msg.Body, err)
			}
		}
	}

	res, err := m.svc.TrashMessages(ctx, ana, service.TrashRequest{IDs: rows})
	if err != nil || len(res.Messages) != 3 || len(res.Removed) != 0 {
		t.Fatalf("trash = %+v, %v; want the three rows in the trash under their ids", res, err)
	}
	onItsOwn("Trash")
	inbox := b.folder(t, "INBOX")
	res, err = m.svc.MoveMessages(ctx, ana, service.MoveRequest{IDs: rows[1:], To: strconv.FormatInt(inbox, 10)})
	if err != nil || len(res.Messages) != 2 || res.Messages[0].ID != rows[1] || res.Messages[1].ID != rows[2] ||
		res.Messages[0].FolderID != inbox || res.Messages[1].FolderID != inbox {
		t.Fatalf("moving two back = %+v, %v; want both rows in the inbox under their ids", res, err)
	}
	onItsOwn("INBOX")
	onItsOwn("Trash")
}

func hasFlag(flags []imap.Flag, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(string(f), want) {
			return true
		}
	}
	return false
}
