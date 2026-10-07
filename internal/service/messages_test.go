package service_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// mailFixture is the service with an engine that lends each account's
// interactive connection, as the real one does, over mail servers the test
// holds: a FakeMailbox per account, or the in-process IMAP server behind the
// registry.
type mailFixture struct {
	*fixture
	engine *lendingSync

	mu    sync.Mutex
	boxes map[string]provider.Mailbox
}

func newMailFixture(t *testing.T) *mailFixture {
	t.Helper()
	return newMailFixtureWith(t, fixtureOptions{})
}

// newMailFixtureWith is newMailFixture with options; its sync is the
// lending engine, whatever o says.
func newMailFixtureWith(t *testing.T, o fixtureOptions) *mailFixture {
	t.Helper()
	engine := &lendingSync{fakeSync: newFakeSync()}
	m := &mailFixture{engine: engine, boxes: map[string]provider.Mailbox{}}
	o.sync = engine
	m.fixture = newFixtureWith(t, o)
	engine.mailbox = func(ctx context.Context, id string) (provider.Mailbox, error) {
		m.mu.Lock()
		box, ok := m.boxes[id]
		m.mu.Unlock()
		if ok {
			return box, nil
		}
		return m.registry.Mailbox(ctx, id)
	}
	return m
}

// fakeAccount registers an active mailbox on a FakeMailbox, owned by owner
// (an instance mailbox when owner is the zero principal), with sync allowed:
// its owner consented, or the operator switched it on.
func (m *mailFixture) fakeAccount(t *testing.T, owner service.Principal, email string) (string, *providertest.FakeMailbox) {
	t.Helper()
	id := fmt.Sprintf("acc_%016x", len(m.boxes)+1)
	if _, err := m.repo.Create(t.Context(), account.Account{
		ID: id, Email: email, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if owner.UserID == "" {
		if _, err := m.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
			t.Fatal(err)
		}
	} else if _, err := m.svc.GrantSyncConsent(t.Context(), owner, m.consent().Sync); err != nil {
		t.Fatal(err)
	}
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	m.mu.Lock()
	m.boxes[id] = box
	m.mu.Unlock()
	return id, box
}

// index stores what sync would for the account's mailbox as it is now, and
// forgets the calls that took, so a test sees only what reading asks for.
func (m *mailFixture) index(t *testing.T, id string, box *providertest.FakeMailbox) {
	t.Helper()
	storetest.IndexMailbox(t, m.db, id, box)
	box.ResetCalls()
}

// messageID is the local id of the row the index holds for a UID.
func (m *mailFixture) messageID(t *testing.T, accountID, folder string, uid imap.UID) int64 {
	t.Helper()
	var id int64
	if err := m.db.Reader().QueryRowContext(t.Context(), `SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.name = ? AND m.uid = ?`, accountID, folder, uint32(uid)).Scan(&id); err != nil {
		t.Fatalf("no indexed row for %s UID %d: %v", folder, uid, err)
	}
	return id
}

// report is a message as mail clients send one: a plain and an HTML
// version, in different charsets and encodings, and three attachments — a
// PDF, an HTML page whose name climbs directories, and an SVG whose name
// hides a right-to-left override. "zebrafish" appears only in its bodies.
var report = strings.ReplaceAll(`From: Bea Lima <bea@example.org>
To: Ana <ana@example.org>
Subject: September report
Message-ID: <report-1@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

Ol=E1 Ana, segue o relat=F3rio. zebrafish
--inner
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: base64

`+base64.StdEncoding.EncodeToString([]byte(`<p>Olá Ana, segue o <b>relatório</b>. zebrafish</p><script>alert(1)</script>`))+`
--inner--
--outer
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

`+base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 zebrafish"))+`
--outer
Content-Type: text/html; name="page.html"
Content-Disposition: attachment; filename="../../etc/evil.html"
Content-Transfer-Encoding: 7bit

<script>alert(document.cookie)</script>
--outer
Content-Type: image/svg+xml
Content-Disposition: attachment; filename*=utf-8''invoice%E2%80%AEgpj.svg
Content-Transfer-Encoding: 7bit

<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>
--outer--
`, "\n", "\r\n")

// deliverReport puts the report in the INBOX, unread.
func deliverReport(box *providertest.FakeMailbox, at time.Time) imap.UID {
	return box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "report-1@example.org", Subject: "September report", From: "Bea Lima <bea@example.org>",
		To: []string{"Ana <ana@example.org>"}, InternalDate: at, Raw: []byte(report),
	})
}

// seen reports whether flags hold \Seen, in whatever case the server
// writes it: the in-process server and the fake both lowercase flags.
func seen(flags []imap.Flag) bool {
	return slices.ContainsFunc(flags, func(f imap.Flag) bool { return strings.EqualFold(string(f), string(imap.FlagSeen)) })
}

func readAll(t *testing.T, dl service.Download) string {
	t.Helper()
	defer func() { _ = dl.Body.Close() }()
	body, err := io.ReadAll(dl.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func wantCode(t *testing.T, what string, err error, code service.Code) {
	t.Helper()
	if service.CodeOf(err) != code {
		t.Fatalf("%s = %v, want %s", what, err, code)
	}
}

func TestReadingAMessageDecodesItsBodiesAndListsItsParts(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	msg, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Subject != "September report" || msg.FolderRole != "inbox" || msg.MessageID != "report-1@example.org" {
		t.Errorf("summary = %+v", msg.MessageSummary)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, "Olá Ana, segue o relatório. zebrafish") {
		t.Errorf("text = %v, want the latin-1 quoted-printable part decoded", msg.Body.Text)
	}
	if msg.Body.HTML == nil || !strings.Contains(*msg.Body.HTML, "<b>relatório</b>") ||
		!strings.Contains(*msg.Body.HTML, "<script>") {
		t.Errorf("html = %v, want the base64 part decoded and left as the sender wrote it", msg.Body.HTML)
	}
	if !msg.Body.HTMLUnsafe || msg.Body.Truncated || msg.Body.CharsetFallback {
		t.Errorf("body flags = %+v", msg.Body)
	}
	var got []string
	for _, p := range msg.Parts {
		got = append(got, fmt.Sprintf("%s %s %q %t", p.Path, p.MIMEType, p.Filename, p.IsAttachment))
	}
	want := []string{
		`1.1 text/plain "" false`, `1.2 text/html "" false`, `2 application/pdf "report.pdf" true`,
		`3 text/html "evil.html" true`, `4 image/svg+xml "invoicegpj.svg" true`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("parts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// One format fetches one part; a small cap truncates on a character.
	box.ResetCalls()
	text, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID, Format: "text", MaxBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if text.Body.HTML != nil || text.Body.Text == nil || *text.Body.Text != "Olá " || !text.Body.Truncated {
		t.Errorf("text only, 5 bytes = %+v", text.Body)
	}
	var sections []string
	for _, c := range box.Calls() {
		if c.Method == providertest.MethodFetchPart {
			sections = append(sections, c.Section)
		}
	}
	if !slices.Equal(sections, []string{"1.1"}) {
		t.Errorf("format=text fetched %v, want only the plain part", sections)
	}
	for _, bad := range []service.GetMessageRequest{
		{ID: msgID, Format: "rtf"}, {ID: msgID, MaxBytes: service.MaxBodyBytes + 1}, {ID: msgID, MaxBytes: -1},
	} {
		_, err := m.svc.GetMessage(t.Context(), reader(), bad)
		wantCode(t, fmt.Sprintf("%+v", bad), err, service.CodeBadRequest)
	}
}

func TestReadingNeverSetsSeenOrChangesTheMailbox(t *testing.T) {
	// The policy allows reading, and reading is not marking as read: every
	// fetch is a PEEK in a folder opened read-only, and nothing reading does
	// writes to the mailbox.
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	if _, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID}); err != nil {
		t.Fatal(err)
	}
	raw, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, raw)
	pdf, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "2")
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, pdf)

	if flags := box.Flags("INBOX", uid); seen(flags) {
		t.Errorf("the server's flags are %v after reading", flags)
	}
	for _, c := range box.Calls() {
		switch c.Method {
		case providertest.MethodStoreFlags, providertest.MethodMove, providertest.MethodAppend,
			providertest.MethodCreate:
			t.Errorf("reading wrote to the mailbox: %+v", c)
		case providertest.MethodSelect:
			if !c.ReadOnly {
				t.Errorf("reading opened %s read-write; EXAMINE cannot change a flag, SELECT can", c.Folder)
			}
		}
	}
	msg, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID, Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Seen {
		t.Error("the index says the message is read")
	}
}

func TestNothingIsPersistedAfterReading(t *testing.T) {
	// The policy promises no message text or attachment is stored. After a
	// message, its original and an attachment were read, no table holds a
	// word of them, the database files do not either, and the spool is
	// empty.
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	if _, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"2", "3", "1.1"} {
		dl, err := m.svc.GetAttachment(t.Context(), reader(), msgID, path)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, dl)
	}
	raw, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, raw)

	for _, table := range []string{"bodies", "attachment_blobs"} {
		if n := m.count(t, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s has %d rows after reading", table, n)
		}
	}
	if n := m.count(t, `SELECT count(*) FROM messages WHERE body_text <> '' OR snippet <> '' OR body_fetched_at <> 0`); n != 0 {
		t.Errorf("%d messages gained a body, a snippet or a fetch time", n)
	}
	if n := m.count(t, `SELECT count(*) FROM parts WHERE sha256 IS NOT NULL`); n != 0 {
		t.Errorf("%d parts were marked downloaded", n)
	}
	if found := m.mentions(t, []string{"zebrafish", "PDF-1.4", "alert("}); len(found) > 0 {
		t.Errorf("content reached the database: %v", found)
	}
	if found := m.onDisk(t, "zebrafish", "PDF-1.4"); len(found) > 0 {
		t.Errorf("content reached the database files: %v", found)
	}
}

func TestReadingThroughARealIMAPConnectionLeavesTheMessageUnread(t *testing.T) {
	// The in-process IMAP server marks a message \Seen on any FETCH of a
	// body section that is not a PEEK, as RFC 3501 says a server must. The
	// real adapter, the engine's connection and the service together must
	// leave it unread — and remove what they spooled.
	m := newMailFixture(t)
	m.withIMAPAccount(t, provider.KindIMAP, providertest.RichCaps())
	id := m.accountID(t)
	if _, err := m.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	uid := m.imap.Append(t, "INBOX", report, nil, time.Unix(1_790_000_000, 0))
	mailbox, err := m.registry.Mailbox(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	storetest.IndexMailbox(t, m.db, id, mailbox)
	msgID := m.messageID(t, id, "INBOX", uid)

	msg, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, "relatório") {
		t.Fatalf("text = %v", msg.Body.Text)
	}
	for _, path := range []string{"2", "4"} {
		dl, err := m.svc.GetAttachment(t.Context(), reader(), msgID, path)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, dl)
	}
	raw, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, raw); !strings.Contains(body, "Subject: September report") {
		t.Errorf("the original is %d bytes without its header", len(body))
	}

	other, err := mailbox.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := other.FetchFlags(t.Context(), imap.UIDSetNum(uid), 0)
	if err != nil || len(flags) != 1 {
		t.Fatalf("FetchFlags = %v, %v", flags, err)
	}
	if seen(flags[0].Flags) {
		t.Errorf("the server marked the message read: %v", flags[0].Flags)
	}
	spooled, err := os.ReadDir(m.spool)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(spooled) > 0 {
		t.Errorf("the spool still holds %d files after every download was closed", len(spooled))
	}
}

func TestEveryFetchGoesThroughTheInteractiveRunner(t *testing.T) {
	// An account holds three connections, and the third is the engine's.
	// Reading borrows it; it never opens a fourth, whatever it reads.
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)
	opened := box.Opens(provider.RoleSync)

	if _, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID}); err != nil {
		t.Fatal(err)
	}
	raw, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, raw)
	dl, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "2")
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, dl)

	m.engine.mu.Lock()
	lent := slices.Clone(m.engine.lent)
	m.engine.mu.Unlock()
	if !slices.Equal(lent, []string{id, id, id}) {
		t.Errorf("the engine lent its connection for %v, want the three reads of %s", lent, id)
	}
	if box.Opens(provider.RoleSync) != opened || box.Opens(provider.RoleIdle) != 0 {
		t.Error("reading opened a sync or idle connection")
	}
	if n := box.Opens(provider.RoleInteractive); n != 3 {
		t.Errorf("%d interactive connections were opened, want the one per borrowing", n)
	}
	if peak := box.PeakSessions(provider.RoleInteractive); peak != 1 {
		t.Errorf("%d interactive connections were open at once", peak)
	}
	for _, c := range box.Calls() {
		if c.Method != providertest.MethodOpen && c.Role != provider.RoleInteractive {
			t.Errorf("%s ran on the %s connection", c.Method, c.Role)
		}
	}
	if v := box.Violations(); len(v) > 0 {
		t.Errorf("the fake saw misuse: %v", v)
	}
	// Searching is the index alone.
	box.ResetCalls()
	if _, err := m.svc.SearchMessages(t.Context(), reader(), service.SearchRequest{Query: "report"}); err != nil {
		t.Fatal(err)
	}
	if calls := box.Calls(); len(calls) != 0 {
		t.Errorf("a search talked to the mail server: %v", calls)
	}
}

func TestAVanishedMessageIsNotFoundAndARenumberedOneIsBusy(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	first := box.Deliver("INBOX", providertest.FakeMessage{Subject: "one", From: "a@example.org"})
	second := box.Deliver("INBOX", providertest.FakeMessage{Subject: "two", From: "a@example.org"})
	third := box.Deliver("INBOX", providertest.FakeMessage{Subject: "three", From: "a@example.org"})
	m.index(t, id, box)

	// Tombstoned by a diff: gone from the folder until confirmed.
	tombstoned := m.messageID(t, id, "INBOX", first)
	m.exec(t, `UPDATE messages SET vanished_at = 1 WHERE id = ?`, tombstoned)
	// Waiting for a resync to find its new UID.
	stale := m.messageID(t, id, "INBOX", second)
	m.exec(t, `UPDATE messages SET stale = 1 WHERE id = ?`, stale)
	// Expunged on the server, and the index has not noticed yet.
	expunged := m.messageID(t, id, "INBOX", third)
	box.Expunge("INBOX", third)

	for name, msgID := range map[string]int64{"tombstoned": tombstoned, "expunged": expunged} {
		_, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
		wantCode(t, name+" GetMessage", err, service.CodeNotFound)
		_, err = m.svc.GetRaw(t.Context(), reader(), msgID)
		wantCode(t, name+" GetRaw", err, service.CodeNotFound)
		_, err = m.svc.GetAttachment(t.Context(), reader(), msgID, "1")
		wantCode(t, name+" GetAttachment", err, service.CodeNotFound)
	}
	// A row a resync has not matched yet is still a message: try again later.
	_, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: stale})
	wantCode(t, "stale GetMessage", err, service.CodeConflict)
	_, err = m.svc.GetRaw(t.Context(), reader(), stale)
	wantCode(t, "stale GetRaw", err, service.CodeConflict)
	_, err = m.svc.GetAttachment(t.Context(), reader(), stale, "1")
	wantCode(t, "stale GetAttachment", err, service.CodeConflict)
	_, err = m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: 999999})
	wantCode(t, "an id nobody has", err, service.CodeNotFound)

	// A folder renumbered under the index: its UIDs name nothing now.
	fourth := box.Deliver("INBOX", providertest.FakeMessage{Subject: "four", From: "a@example.org"})
	m.index(t, id, box)
	renumbered := m.messageID(t, id, "INBOX", fourth)
	box.ChangeUIDValidity("INBOX")
	_, err = m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: renumbered})
	wantCode(t, "after a UIDVALIDITY change", err, service.CodeConflict)
}

func TestAPersonCannotReadAnotherPersonsMessage(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	bob := m.person(t, "bob@example.com", auth.RoleMember)
	anas, anaBox := m.fakeAccount(t, ana, "ana@mail.example")
	bobs, bobBox := m.fakeAccount(t, bob, "bob@mail.example")
	anaBox.Deliver("INBOX", providertest.FakeMessage{Subject: "for ana", From: "x@example.org"})
	bobUID := deliverReport(bobBox, time.Unix(1_790_000_000, 0))
	m.index(t, anas, anaBox)
	m.index(t, bobs, bobBox)
	bobsMessage := m.messageID(t, bobs, "INBOX", bobUID)
	var bobsInbox int64
	for _, f := range m.folderIDs(t, bobs) {
		bobsInbox = f
	}

	_, err := m.svc.GetMessage(t.Context(), ana, service.GetMessageRequest{ID: bobsMessage})
	wantCode(t, "GetMessage", err, service.CodeNotFound)
	if service.MessageOf(err) != "no such message" {
		t.Errorf("the refusal says %q; it must read as a message that does not exist", service.MessageOf(err))
	}
	_, err = m.svc.GetRaw(t.Context(), ana, bobsMessage)
	wantCode(t, "GetRaw", err, service.CodeNotFound)
	_, err = m.svc.GetAttachment(t.Context(), ana, bobsMessage, "2")
	wantCode(t, "GetAttachment", err, service.CodeNotFound)
	_, err = m.svc.SearchMessages(t.Context(), ana, service.SearchRequest{AccountID: bobs})
	wantCode(t, "a search of bob's account", err, service.CodeNotFound)
	_, err = m.svc.SearchMessages(t.Context(), ana, service.SearchRequest{FolderID: bobsInbox})
	wantCode(t, "a search of bob's folder", err, service.CodeNotFound)
	_, err = m.svc.SearchMessages(t.Context(), ana, service.SearchRequest{AccountID: anas, FolderID: bobsInbox})
	wantCode(t, "bob's folder under ana's account", err, service.CodeNotFound)

	page, err := m.svc.SearchMessages(t.Context(), ana, service.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].AccountID != anas {
		t.Errorf("ana's search found %+v", page.Messages)
	}
	if calls := bobBox.Calls(); len(calls) != 0 {
		t.Errorf("ana's attempts reached bob's mail server: %v", calls)
	}

	// A key restricted to one account reads nothing of another's, even the
	// instance's.
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeRead, AccountIDs: []string{anas}}
	_, err = m.svc.GetMessage(t.Context(), restricted, service.GetMessageRequest{ID: bobsMessage})
	wantCode(t, "a key restricted to ana's account", err, service.CodeNotFound)
	// And a key without read scope reads nothing at all.
	_, err = m.svc.GetMessage(t.Context(), service.Principal{KeyPrefix: "dddddddd"}, service.GetMessageRequest{ID: bobsMessage})
	wantCode(t, "a key with no scope", err, service.CodeNotAuthorized)
}

// folderIDs lists an account's folder ids in the index.
func (m *mailFixture) folderIDs(t *testing.T, accountID string) []int64 {
	t.Helper()
	folders, err := m.db.Folders(t.Context(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, f := range folders {
		out = append(out, f.ID)
	}
	return out
}

func TestSearchContinuesFromItsCursor(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	base := time.Unix(1_790_000_000, 0)
	for i := range 5 {
		box.Deliver("INBOX", providertest.FakeMessage{
			Subject: fmt.Sprintf("invoice %d", i), From: "Billing <billing@example.org>",
			InternalDate: base.Add(time.Duration(i) * time.Hour),
		})
	}
	box.Deliver("INBOX", providertest.FakeMessage{Subject: "lunch", From: "Bea <bea@example.org>", InternalDate: base})
	m.index(t, id, box)

	var subjects []string
	req := service.SearchRequest{Query: "invoice", Limit: 2}
	for range 5 {
		page, err := m.svc.SearchMessages(t.Context(), reader(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range page.Messages {
			subjects = append(subjects, msg.Subject)
		}
		if page.NextCursor == "" {
			break
		}
		req.Cursor = page.NextCursor
	}
	want := []string{"invoice 4", "invoice 3", "invoice 2", "invoice 1", "invoice 0"}
	if !slices.Equal(subjects, want) {
		t.Fatalf("pages gave %v, want %v", subjects, want)
	}

	for _, bad := range []service.SearchRequest{
		{Cursor: "not-a-cursor"}, {Limit: service.MaxPageSize + 1}, {Limit: -1},
		{Query: strings.Repeat("a", 300)}, {Since: "yesterday"}, {Until: "2026-13-01"}, {FolderID: -1},
	} {
		_, err := m.svc.SearchMessages(t.Context(), reader(), bad)
		wantCode(t, fmt.Sprintf("%+v", bad), err, service.CodeBadRequest)
	}

	unseen, other := true, false
	page, err := m.svc.SearchMessages(t.Context(), reader(), service.SearchRequest{
		From: "bea", Unseen: &unseen, HasAttachments: &other, Since: "2026-09-21", Until: "2026-09-21",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Subject != "lunch" {
		t.Errorf("filters found %+v", page.Messages)
	}
	if got := page.Messages[0].From; len(got) != 1 || got[0] != (service.Address{Name: "Bea", Email: "bea@example.org"}) {
		t.Errorf("from = %+v", got)
	}
}

func TestAttachmentsAreServedAsFilesABrowserWillNotRun(t *testing.T) {
	// The sender chose each part's type and name. A type a browser would
	// run or render actively is served as application/octet-stream, and a
	// name can neither climb directories nor hide its extension.
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	for _, c := range []struct {
		path, contentType, filename, body string
	}{
		{"2", "application/pdf", "report.pdf", "%PDF-1.4 zebrafish"},
		{"3", "application/octet-stream", "evil.html", "<script>alert(document.cookie)</script>"},
		{"4", "application/octet-stream", "invoicegpj.svg", `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`},
		{"1.2", "application/octet-stream", "part-1.2.html", ""},
	} {
		dl, err := m.svc.GetAttachment(t.Context(), reader(), msgID, c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		body := readAll(t, dl)
		if dl.ContentType != c.contentType || dl.Filename != c.filename {
			t.Errorf("part %s is served as %q named %q, want %q named %q",
				c.path, dl.ContentType, dl.Filename, c.contentType, c.filename)
		}
		if c.body != "" && body != c.body {
			t.Errorf("part %s = %q, want its transfer encoding undone", c.path, body)
		}
	}
	for path, code := range map[string]service.Code{
		"9": service.CodeNotFound, "0": service.CodeBadRequest, "../2": service.CodeBadRequest, "": service.CodeBadRequest,
	} {
		_, err := m.svc.GetAttachment(t.Context(), reader(), msgID, path)
		wantCode(t, "part "+path, err, code)
	}
}

func TestReadingAMailboxThatNeedsConsentAgainIsAConflict(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)
	m.exec(t, `UPDATE accounts SET state = 'needs_reauth' WHERE id = ?`, id)

	_, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	if !errors.Is(err, service.ErrNeedsReauth) {
		t.Fatalf("GetMessage = %v, want the re-authorization conflict", err)
	}
	if calls := box.Calls(); len(calls) != 0 {
		t.Errorf("a mailbox waiting for consent was dialled: %v", calls)
	}
	// The index still answers: the list is what was stored, and stays.
	page, err := m.svc.SearchMessages(t.Context(), reader(), service.SearchRequest{AccountID: id})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("search = %+v, %v", page, err)
	}
}

func TestAnUnreachableServerIsReportedAsUpstream(t *testing.T) {
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	box.FailNext(providertest.MethodOpen, fmt.Errorf("%w: dial refused", provider.ErrConnClosed))
	_, err := m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	wantCode(t, "a server that refused the connection", err, service.CodeInternal)
	if !strings.HasPrefix(service.MessageOf(err), "upstream:") {
		t.Errorf("message = %q, want it to say the fault is upstream", service.MessageOf(err))
	}
	box.FailNext(providertest.MethodOpen, fmt.Errorf("%w: too many", provider.ErrTooManyConnections))
	_, err = m.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	wantCode(t, "too many connections", err, service.CodeRateLimited)
	if logs := m.logs.String(); strings.Contains(logs, "September report") || strings.Contains(logs, "bea@example.org") {
		t.Errorf("a failure logged the message's subject or sender:\n%s", logs)
	}
}
