package service_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/mail"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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

// sendBox is a mailbox that sends: its IMAP side on a fake server, for the
// copy in Sent and the replied message, and its submission to an in-process
// SMTP server that keeps every byte it is sent.
type sendBox struct {
	m     *mailFixture
	owner service.Principal
	id    string
	box   *providertest.FakeMailbox
	smtp  *providertest.SMTPServer
}

// sendingBox registers a mailbox owned by owner (the operator's when owner
// is the zero principal) of the kind o names, with a Sent folder and
// submission to its own SMTP server. Its owner allows sending.
func (m *mailFixture) sendingBox(t *testing.T, owner service.Principal, email string, o providertest.FakeOptions) *sendBox {
	t.Helper()
	return m.sendingBoxIn(t, owner, "", email, o)
}

// sendingBoxIn is sendingBox linked into a workspace of the owner's: empty
// is their personal workspace.
func (m *mailFixture) sendingBoxIn(t *testing.T, owner service.Principal, workspaceID, email string, o providertest.FakeOptions) *sendBox {
	t.Helper()
	id, box := m.ownedBoxIn(t, owner, workspaceID, email, o)
	box.CreateFolder("Sent", imap.MailboxAttrSent)
	smtp := providertest.NewSMTPServer(t)
	saveSent := 0
	if o.Kind == "" || o.Kind == provider.KindIMAP {
		saveSent = 1
	}
	if _, err := m.db.Writer().ExecContext(t.Context(),
		`UPDATE accounts SET smtp_host = ?, smtp_port = ?, smtp_tls = 'starttls', auth_kind = 'password',
		        save_sent_copy = ?, display_name = 'Ana Lima' WHERE id = ?`,
		smtp.Host, smtp.Port, saveSent, id); err != nil {
		t.Fatal(err)
	}
	if err := m.repo.SavePassword(t.Context(), id, "hunter2"); err != nil {
		t.Fatal(err)
	}
	b := &sendBox{m: m, owner: owner, id: id, box: box, smtp: smtp}
	if owner.UserID != "" {
		if _, err := m.svc.GrantSendConsent(t.Context(), owner, m.consent().Send); err != nil {
			t.Fatal(err)
		}
	}
	service.SetSendRetryForTest(m.svc, time.Millisecond, time.Millisecond, time.Millisecond)
	return b
}

// anaSends is ana's generic IMAP mailbox, sending.
func anaSends(t *testing.T) *sendBox {
	t.Helper()
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	return m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
}

func (b *sendBox) compose(to ...string) service.Compose {
	c := service.Compose{AccountID: b.id, Subject: "Lunch on Friday", Text: "Noon at the usual place.", Confirm: true}
	for _, addr := range to {
		c.To = append(c.To, service.Address{Email: addr})
	}
	return c
}

func (b *sendBox) send(t *testing.T, p service.Principal, key string, c service.Compose, files ...service.Upload) (service.SendResult, error) {
	t.Helper()
	req := service.SendRequest{Compose: c, IdempotencyKey: key}
	if len(files) > 0 {
		req.Attachments = func(add func(service.Upload) error) error {
			for _, f := range files {
				if err := add(f); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return b.m.svc.SendMessage(t.Context(), p, req)
}

// row is the send record for key.
func (b *sendBox) row(t *testing.T, key string) (state, reason string) {
	t.Helper()
	if err := b.m.db.Reader().QueryRowContext(t.Context(),
		`SELECT state, error FROM sends WHERE account_id = ? AND idempotency_key = ?`, b.id, key).Scan(&state, &reason); err != nil {
		t.Fatalf("no send record for %s: %v", key, err)
	}
	return state, reason
}

// header reads one header of a message the SMTP server received, raw: its
// value exactly as transmitted, folding undone.
func header(t *testing.T, raw []byte, name string) []string {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the transmitted message does not parse: %v", err)
	}
	return msg.Header[textprotoKey(name)]
}

func textprotoKey(name string) string {
	switch strings.ToLower(name) {
	case "message-id":
		return "Message-Id"
	case "in-reply-to":
		return "In-Reply-To"
	}
	return strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
}

func TestNobodySendsWithoutTheOwnersCurrentSendConsent(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	bob := m.person(t, "bob@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	if _, err := m.svc.WithdrawSendConsent(t.Context(), ana); err != nil {
		t.Fatal(err)
	}
	c := b.compose("bea@example.org")

	_, err := b.send(t, ana, "k-1", c)
	wantCode(t, "ana without consent", err, service.CodeConflict)

	// Agreed to an older text: the person has not agreed to this one.
	if _, err := m.db.Writer().ExecContext(t.Context(),
		`UPDATE users SET send_consent_at = 1, send_consent_version = '2026-01-sending' WHERE id = ?`, ana.UserID); err != nil {
		t.Fatal(err)
	}
	_, err = b.send(t, ana, "k-2", c)
	wantCode(t, "ana with an old consent", err, service.CodeConflict)

	// Only a session gives or takes it back.
	key := keyOf(t, m.fixture, ana, auth.ScopeWrite)
	_, err = m.svc.GrantSendConsent(t.Context(), key, service.DefaultSendConsentVersion)
	wantCode(t, "a key granting the consent", err, service.CodeNotAuthorized)
	_, err = m.svc.GrantSendConsent(t.Context(), ana, "2026-01-sending")
	wantCode(t, "an old version", err, service.CodeBadRequest)

	// Bob cannot see ana's mailbox, consent or not.
	if _, err := m.svc.GrantSendConsent(t.Context(), bob, service.DefaultSendConsentVersion); err != nil {
		t.Fatal(err)
	}
	_, err = b.send(t, bob, "k-3", c)
	wantCode(t, "bob", err, service.CodeNotFound)

	if n := b.smtp.DataCommands(); n != 0 || len(b.smtp.Auths()) != 0 {
		t.Fatalf("the server saw %d DATA and AUTH %v before anyone could send", n, b.smtp.Auths())
	}
	var rows int
	if err := m.db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM sends`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d send records (%v); a refused send reserves nothing", rows, err)
	}

	given, err := m.svc.GrantSendConsent(t.Context(), ana, service.DefaultSendConsentVersion)
	if err != nil || !given.Consented || given.Version != service.DefaultSendConsentVersion {
		t.Fatalf("GrantSendConsent = %+v, %v", given, err)
	}
	res, err := b.send(t, ana, "k-4", c)
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("with consent: %+v, %v", res, err)
	}
	if n := len(b.smtp.Messages()); n != 1 {
		t.Fatalf("the server took %d messages, want 1", n)
	}
}

func TestAnInstanceKeyCannotSendFromAPersonsMailbox(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	owner := m.person(t, "olga@example.com", auth.RoleOwner)
	anas := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	shared := m.sendingBox(t, service.Principal{}, "team@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	instance := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeSend, WorkspaceID: workspace.OperatorID}

	// The instance key reaches the operator workspace's mailboxes only:
	// ana's does not exist for it.
	_, err := anas.send(t, instance, "k-1", anas.compose("bea@example.org"))
	wantCode(t, "an instance key on ana's mailbox", err, service.CodeNotFound)
	// An owner of the instance does not see it at all.
	_, err = anas.send(t, owner, "k-2", anas.compose("bea@example.org"))
	wantCode(t, "an owner on ana's mailbox", err, service.CodeNotFound)
	// A key below the send scope sends nothing anywhere.
	_, err = shared.send(t, service.Principal{KeyPrefix: "eeeeeeee", Scope: auth.ScopeWrite, WorkspaceID: workspace.OperatorID}, "k-3",
		shared.compose("bea@example.org"))
	wantCode(t, "a write key", err, service.CodeNotAuthorized)
	// Ana does not see the operator's mailbox.
	_, err = shared.send(t, ana, "k-4", shared.compose("bea@example.org"))
	wantCode(t, "a member on the operator's mailbox", err, service.CodeNotFound)
	if n := anas.smtp.DataCommands() + shared.smtp.DataCommands(); n != 0 {
		t.Fatalf("%d DATA commands before anyone could send", n)
	}

	// The operator's mailbox sends for the operator, with its key; an owner
	// of the instance does not see it.
	_, err = shared.send(t, owner, "op-owner", shared.compose("bea@example.org"))
	wantCode(t, "an owner on the operator's mailbox", err, service.CodeNotFound)
	res, err := shared.send(t, instance, "op-0", shared.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("the operator's mailbox for its key: %+v, %v", res, err)
	}
	// And ana's for ana.
	if res, err := anas.send(t, ana, "k-5", anas.compose("bea@example.org")); err != nil || res.State != service.SendStateSent {
		t.Fatalf("ana: %+v, %v", res, err)
	}

	// The account says which of them can send for the caller.
	for _, c := range []struct {
		p    service.Principal
		id   string
		want service.AccountSend
	}{
		{instance, shared.id, service.AccountSend{Available: true}},
		{ana, anas.id, service.AccountSend{Available: true}},
	} {
		a, err := m.svc.GetAccount(t.Context(), c.p, c.id)
		if err != nil || a.Send != c.want {
			t.Errorf("%s's view of %s: send = %+v (%v), want %+v", c.p.Actor(), c.id, a.Send, err, c.want)
		}
	}
}

func TestSendingTheSameIdempotencyKeyTwiceSubmitsOnce(t *testing.T) {
	b := anaSends(t)
	c := b.compose("bea@example.org")
	first, err := b.send(t, b.owner, "5b1f2e3d-0000-4000-8000-000000000001", c)
	if err != nil || first.State != service.SendStateSent || first.Replayed {
		t.Fatalf("first send: %+v, %v", first, err)
	}
	again, err := b.send(t, b.owner, "5b1f2e3d-0000-4000-8000-000000000001", c)
	if err != nil || again.State != service.SendStateSent || !again.Replayed || again.MessageID != first.MessageID ||
		again.SentAt != first.SentAt {
		t.Fatalf("the retry = %+v, %v; want the first send replayed", again, err)
	}
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("the server received DATA %d times, want once", n)
	}

	// A caller that sends no key — a script, a model that retries after a
	// timeout — gets one made from the message and the minute.
	shared := b.m.sendingBox(t, service.Principal{}, "team@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	instance := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeSend, WorkspaceID: workspace.OperatorID}
	one, err := shared.send(t, instance, "", shared.compose("bea@example.org"))
	if err != nil || one.State != service.SendStateSent {
		t.Fatalf("keyless send: %+v, %v", one, err)
	}
	two, err := shared.send(t, instance, "", shared.compose("bea@example.org"))
	if err != nil || !two.Replayed || two.MessageID != one.MessageID {
		t.Fatalf("the keyless retry = %+v, %v; want it replayed", two, err)
	}
	if n := shared.smtp.DataCommands(); n != 1 {
		t.Fatalf("a keyless retry reached DATA %d times, want once", n)
	}

	// Two requests with one key at the same time: one sends, the other is
	// told a send is in progress, and a third, after, is the replay.
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.smtp.OnData(func([]byte) {
		once.Do(func() { close(arrived) })
		<-release
	})
	done := make(chan service.SendResult, 1)
	go func() {
		res, err := b.send(t, b.owner, "concurrent", c)
		if err != nil {
			t.Errorf("the first of two: %v", err)
		}
		done <- res
	}()
	<-arrived
	_, err = b.send(t, b.owner, "concurrent", c)
	wantCode(t, "the second of two at once", err, service.CodeConflict)
	close(release)
	if res := <-done; res.State != service.SendStateSent {
		t.Fatalf("the first of two: %+v", res)
	}
	if res, err := b.send(t, b.owner, "concurrent", c); err != nil || !res.Replayed {
		t.Fatalf("after both: %+v, %v", res, err)
	}
	if n := b.smtp.DataCommands(); n != 2 {
		t.Fatalf("DATA %d times for two keys, want 2", n)
	}

	// A signed-in person must send a key: the console makes one per message.
	_, err = b.send(t, b.owner, "", c)
	wantCode(t, "a session without a key", err, service.CodeBadRequest)
	_, err = b.send(t, b.owner, "not a key/at all", c)
	wantCode(t, "a malformed key", err, service.CodeBadRequest)
}

func TestTheSameKeyWithADifferentMessageIsAConflict(t *testing.T) {
	b := anaSends(t)
	if _, err := b.send(t, b.owner, "reused", b.compose("bea@example.org")); err != nil {
		t.Fatal(err)
	}
	other := b.compose("bea@example.org")
	other.Text = "Actually, one o'clock."
	_, err := b.send(t, b.owner, "reused", other)
	wantCode(t, "another text under the same key", err, service.CodeConflict)
	// An attachment is part of the message: the same text with a file is
	// another message.
	_, err = b.send(t, b.owner, "reused", b.compose("bea@example.org"),
		service.Upload{Filename: "menu.txt", ContentType: "text/plain", Body: strings.NewReader("soup")})
	wantCode(t, "the same text with an attachment", err, service.CodeConflict)
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("DATA %d times, want only the first", n)
	}
	if n := len(b.m.sendFiles(t)); n != 0 {
		t.Fatalf("a refused send left %d files in the spool", n)
	}
}

func TestARejectedRecipientSendsNothingAndStoresNoAddress(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{logLevel: "debug"})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	b.smtp.RejectRecipient("ghost.recipient@nowhere.example")
	c := b.compose("bea@example.org", "Ghost.Recipient@nowhere.example")
	c.Cc = []service.Address{{Name: "Caio", Email: "caio@example.org"}}

	res, err := b.send(t, ana, "refused", c)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.State != service.SendStateFailed || res.Reason != service.SendReasonRecipientsRefused ||
		!slices.Equal(res.Rejected, []string{"Ghost.Recipient@nowhere.example"}) {
		t.Fatalf("result = %+v; want failed, naming the refused recipient as it was written", res)
	}
	if n := b.smtp.DataCommands(); n != 0 || len(b.smtp.Messages()) != 0 {
		t.Fatalf("a refused recipient still reached DATA (%d)", n)
	}
	if state, reason := b.row(t, "refused"); state != "failed" || reason != service.SendReasonRecipientsRefused {
		t.Fatalf("record = %s %s", state, reason)
	}
	for _, needle := range []string{"ghost.recipient", "nowhere.example", "bea@example.org", "caio@example.org"} {
		if found := m.mentions(t, []string{needle}); len(found) > 0 {
			t.Errorf("the database holds %q: %v", needle, found)
		}
		if found := m.onDisk(t, needle); len(found) > 0 {
			t.Errorf("the database files hold %q: %v", needle, found)
		}
		if strings.Contains(strings.ToLower(m.logs.String()), needle) {
			t.Errorf("the log holds %q", needle)
		}
	}

	// Fixed, the same key sends: a failed send sent nothing.
	c.To = c.To[:1]
	res, err = b.send(t, ana, "refused", c)
	if err != nil || res.State != service.SendStateSent || res.Replayed {
		t.Fatalf("the corrected message: %+v, %v", res, err)
	}
}

func TestAnErrorAfterDataIsUnknownAndNeverRetried(t *testing.T) {
	b := anaSends(t)
	since := b.lastSeq(t)
	b.smtp.DropAfterData(1)
	res, err := b.send(t, b.owner, "lost", b.compose("bea@example.org"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.State != service.SendStateUnknown || res.Reason != service.SendReasonAfterData || res.MessageID == "" {
		t.Fatalf("result = %+v; want unknown", res)
	}
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("DATA %d times; a message that may have been delivered is never sent again", n)
	}
	// The same request again is refused, not sent.
	_, err = b.send(t, b.owner, "lost", b.compose("bea@example.org"))
	wantCode(t, "retrying an unknown send", err, service.CodeConflict)
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("DATA %d times after a retry of an unknown send", n)
	}
	finished := b.finished(t, since)
	if len(finished) != 1 || finished[0].State != "unknown" || finished[0].Key != "lost" {
		t.Fatalf("send.finished = %+v", finished)
	}
}

func TestAnUnknownSendIsReconciledFromTheSentFolder(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	// A provider that files the copy itself, with the index syncing it.
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Kind: provider.KindMicrosoft, Caps: providertest.ExchangeCaps()})
	m.index(t, b.id, b.box)
	since := b.lastSeq(t)

	b.smtp.DropAfterData(1)
	res, err := b.send(t, ana, "filed-later", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateUnknown {
		t.Fatalf("SendMessage = %+v, %v; want unknown", res, err)
	}
	// The provider took it and filed the copy; the next pass of Sent finds it.
	b.box.Deliver("Sent", providertest.FakeMessage{
		MessageID: res.MessageID, Subject: "Lunch on Friday", From: "ana@mail.example",
		To: []string{"bea@example.org"}, InternalDate: time.Now(),
	})
	m.index(t, b.id, b.box)
	if state, reason := b.row(t, "filed-later"); state != "sent" || reason != "" {
		t.Fatalf("after the Sent folder's pass: %s %q, want sent", state, reason)
	}
	status, err := m.svc.SendStatus(t.Context(), ana, b.id, "filed-later")
	if err != nil || status.State != service.SendStateSent || status.SentAt == 0 || status.MessageID != res.MessageID {
		t.Fatalf("SendStatus = %+v, %v", status, err)
	}
	finished := b.finished(t, since)
	if len(finished) != 2 || finished[0].State != "unknown" || finished[1].State != "sent" {
		t.Fatalf("send.finished = %+v; want unknown, then sent", finished)
	}

	// When the copy is indexed before the failure is even reported, the
	// send is sent at once.
	b.smtp.DropAfterData(1)
	b.smtp.OnData(func(raw []byte) {
		id := strings.Trim(header(t, raw, "Message-ID")[0], "<>")
		b.box.Deliver("Sent", providertest.FakeMessage{
			MessageID: id, Subject: "Early", From: "ana@mail.example", InternalDate: time.Now(),
		})
		m.index(t, b.id, b.box)
	})
	res, err = b.send(t, ana, "filed-first", b.compose("caio@example.org"))
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("a send whose copy was already indexed = %+v, %v; want sent", res, err)
	}
	b.smtp.OnData(nil)

	// A generic account has no such source: its unknown stays unknown.
	g := m.sendingBox(t, ana, "ana@other.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	g.smtp.DropAfterData(1)
	res, err = g.send(t, ana, "never-filed", g.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateUnknown {
		t.Fatalf("generic: %+v, %v", res, err)
	}
	m.index(t, g.id, g.box)
	if state, _ := g.row(t, "never-filed"); state != "unknown" {
		t.Fatalf("a generic account's unknown send became %s", state)
	}
}

func TestReferencesHeaderIsOneSpaceJoinedValue(t *testing.T) {
	b := anaSends(t)
	var refs []string
	for i := range 25 {
		refs = append(refs, fmt.Sprintf("thread-%02d@example.org", i))
	}
	uid := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "parent@example.org", Subject: "Lunch on Friday", From: "Bea Lima <bea@example.org>",
		To: []string{"ana@mail.example"}, References: refs, InternalDate: time.Now().Add(-time.Hour),
	})
	b.m.index(t, b.id, b.box)
	c := b.compose("bea@example.org")
	c.Subject = ""
	c.InReplyTo = b.m.messageID(t, b.id, "INBOX", uid)
	res, err := b.send(t, b.owner, "reply", c)
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("reply: %+v, %v", res, err)
	}
	raw := b.smtp.Messages()[0].Raw
	got := header(t, raw, "References")
	if len(got) != 1 {
		t.Fatalf("References appears %d times, want once", len(got))
	}
	want := "<thread-00@example.org>"
	for _, r := range refs[len(refs)-19:] {
		want += " <" + r + ">"
	}
	want += " <parent@example.org>"
	if got[0] != want {
		t.Fatalf("References = %q\nwant       %q", got[0], want)
	}
	if subject := header(t, raw, "Subject"); len(subject) != 1 || subject[0] != "Re: Lunch on Friday" {
		t.Errorf("Subject = %q, want the original's, prefixed once", subject)
	}
	// Answering a reply prefixes nothing more.
	c.Subject = "RE: Lunch on Friday"
	if _, err := b.send(t, b.owner, "reply-2", c); err != nil {
		t.Fatal(err)
	}
	if subject := header(t, b.smtp.Messages()[1].Raw, "Subject"); subject[0] != "RE: Lunch on Friday" {
		t.Errorf("Subject = %q, want no second prefix", subject)
	}
}

func TestEachRFCIdHasExactlyOnePairOfBrackets(t *testing.T) {
	b := anaSends(t)
	uid := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "<parent@example.org>", Subject: "Menu", From: "bea@example.org", To: []string{"ana@mail.example"},
		References: []string{"<root@example.org>", "middle@example.org"}, InternalDate: time.Now().Add(-time.Hour),
	})
	b.m.index(t, b.id, b.box)
	parent := b.m.messageID(t, b.id, "INBOX", uid)
	// However the index came to hold them, the ids reach the header bare
	// and are bracketed once, there.
	if _, err := b.m.db.Writer().ExecContext(t.Context(),
		`UPDATE messages SET references_json = '["<root@example.org>","middle@example.org <tail@example.org>"]' WHERE id = ?`,
		parent); err != nil {
		t.Fatal(err)
	}
	c := b.compose("bea@example.org")
	c.InReplyTo = parent
	res, err := b.send(t, b.owner, "brackets", c)
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("reply: %+v, %v", res, err)
	}
	if strings.ContainsAny(res.MessageID, "<>") || !strings.HasSuffix(res.MessageID, "@mail.example") {
		t.Errorf("result message_id = %q, want bare, at the sender's domain", res.MessageID)
	}
	raw := b.smtp.Messages()[0].Raw
	one := regexp.MustCompile(`^<[^<>\s]+>$`)
	for _, name := range []string{"Message-ID", "In-Reply-To"} {
		v := header(t, raw, name)
		if len(v) != 1 || !one.MatchString(v[0]) {
			t.Errorf("%s = %q, want one id in exactly one pair of brackets", name, v)
		}
	}
	if v := header(t, raw, "Message-ID"); len(v) == 1 && v[0] != "<"+res.MessageID+">" {
		t.Errorf("Message-ID = %q, want the result's %q bracketed", v[0], res.MessageID)
	}
	refs := header(t, raw, "References")
	if len(refs) != 1 {
		t.Fatalf("References = %q", refs)
	}
	tokens := strings.Fields(refs[0])
	want := []string{"<root@example.org>", "<middle@example.org>", "<tail@example.org>", "<parent@example.org>"}
	if !slices.Equal(tokens, want) {
		t.Errorf("References = %q, want %q", tokens, want)
	}
	for _, tok := range tokens {
		if !one.MatchString(tok) {
			t.Errorf("reference %q is not one id in one pair of brackets", tok)
		}
	}
	var stored string
	if err := b.m.db.Reader().QueryRowContext(t.Context(),
		`SELECT message_id_hdr FROM sends WHERE idempotency_key = 'brackets'`).Scan(&stored); err != nil || stored != res.MessageID {
		t.Errorf("stored Message-ID = %q (%v), want it bare", stored, err)
	}
}

func TestGmailAndMicrosoftNeverGetAnAppendedSentCopy(t *testing.T) {
	m := newMailFixture(t)
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	gmail := m.sendingBox(t, ana, "ana@gmail.example", providertest.FakeOptions{
		Kind: provider.KindGmail, Caps: providertest.GmailCaps(), SharedFlags: true, Labels: true,
	})
	microsoft := m.sendingBox(t, ana, "ana@outlook.example", providertest.FakeOptions{
		Kind: provider.KindMicrosoft, Caps: providertest.ExchangeCaps(),
	})
	// A Gmail mailbox connected as generic IMAP is still Gmail.
	disguised := providertest.GmailCaps()
	disguised.Raw = []string{"IMAP4rev1", "X-GM-EXT-1"}
	generic := m.sendingBox(t, ana, "ana@workspace.example", providertest.FakeOptions{Caps: disguised})
	for _, b := range []*sendBox{gmail, microsoft, generic} {
		// Whatever the row says.
		if _, err := m.db.Writer().ExecContext(t.Context(), `UPDATE accounts SET save_sent_copy = 1 WHERE id = ?`, b.id); err != nil {
			t.Fatal(err)
		}
		res, err := b.send(t, ana, "k", b.compose("bea@example.org"))
		if err != nil || res.State != service.SendStateSent {
			t.Fatalf("%s: %+v, %v", b.id, res, err)
		}
		if n := b.box.CallCount(providertest.MethodAppend); n != 0 {
			t.Errorf("%s: %d APPENDs; the provider files the copy itself", b.id, n)
		}
		if sent, _ := b.box.Folder("Sent"); len(sent.UIDs) != 0 {
			t.Errorf("%s: Sent holds %d messages Mailie put there", b.id, len(sent.UIDs))
		}
		status, err := m.svc.SendStatus(t.Context(), ana, b.id, "k")
		if err != nil || status.SentCopy != "n/a" {
			t.Errorf("%s: sent_copy = %q (%v), want n/a", b.id, status.SentCopy, err)
		}
	}
}

func TestAGenericImapAccountAppendsASentCopyOnlyOnce(t *testing.T) {
	b := anaSends(t)
	res, err := b.send(t, b.owner, "copy", b.compose("bea@example.org"),
		service.Upload{Filename: "menu.pdf", ContentType: "application/pdf", Body: strings.NewReader("%PDF-1.4 menu")})
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	sent, _ := b.box.Folder("Sent")
	if len(sent.UIDs) != 1 || b.box.CallCount(providertest.MethodAppend) != 1 {
		t.Fatalf("Sent holds %d messages after %d APPENDs, want one of each", len(sent.UIDs),
			b.box.CallCount(providertest.MethodAppend))
	}
	// Exactly what was transmitted, and marked read.
	if got, want := b.rawIn(t, "Sent", sent.UIDs[0]), b.smtp.Messages()[0].Raw; !bytes.Equal(normalizeEnd(got), normalizeEnd(want)) {
		t.Errorf("the copy in Sent differs from what the server received:\n%s\n---\n%s", got, want)
	}
	if !seen(b.box.Flags("Sent", sent.UIDs[0])) {
		t.Error("the copy in Sent is unread")
	}
	status, err := b.m.svc.SendStatus(t.Context(), b.owner, b.id, "copy")
	if err != nil || status.SentCopy != "appended" {
		t.Fatalf("SendStatus = %+v, %v", status, err)
	}

	// The replay sends nothing, so it files nothing.
	if res, err := b.send(t, b.owner, "copy", b.compose("bea@example.org"),
		service.Upload{Filename: "menu.pdf", ContentType: "application/pdf", Body: strings.NewReader("%PDF-1.4 menu")}); err != nil || !res.Replayed {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	// A server that filed the copy itself by the time Mailie looks gets no
	// second one.
	b.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodSearch && c.Folder == "Sent" {
			b.box.Deliver("Sent", providertest.FakeMessage{MessageID: c.Header, Subject: "Filed", InternalDate: time.Now()})
		}
		return nil
	})
	if _, err := b.send(t, b.owner, "filed", b.compose("caio@example.org")); err != nil {
		t.Fatal(err)
	}
	b.box.OnCall(nil)
	if n := b.box.CallCount(providertest.MethodAppend); n != 1 {
		t.Fatalf("%d APPENDs; the Message-ID was already in Sent", n)
	}
	status, err = b.m.svc.SendStatus(t.Context(), b.owner, b.id, "filed")
	if err != nil || status.SentCopy != "appended" {
		t.Fatalf("SendStatus = %+v, %v", status, err)
	}
	// Found by the Message-ID the send generated, in read-only mode.
	for _, c := range b.box.Calls() {
		if c.Method == providertest.MethodSelect && c.Folder == "Sent" && !c.ReadOnly {
			t.Errorf("Sent was opened read-write to look for the copy")
		}
	}
}

func TestNothingOfTheComposedMessageIsStoredOrLogged(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{logLevel: "debug"})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	since := b.lastSeq(t)
	c := b.compose("quokka.recipient@islands.example")
	c.Cc = []service.Address{{Name: "Wombat Person", Email: "wombat@burrow.example"}}
	c.Bcc = []service.Address{{Email: "numbat@hidden.example"}}
	c.Subject = "Pangolin expedition budget"
	c.Text = "The axolotl sanctuary needs funding before winter."
	res, err := b.send(t, ana, "private", c,
		service.Upload{Filename: "narwhal-itinerary.txt", ContentType: "text/plain", Body: strings.NewReader("capybara logistics")})
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	needles := []string{
		"quokka", "islands.example", "Wombat", "burrow.example", "numbat", "hidden.example",
		"Pangolin", "axolotl", "narwhal", "capybara",
	}
	if found := m.mentions(t, needles); len(found) > 0 {
		t.Errorf("rows hold what was composed:\n  %s", strings.Join(found, "\n  "))
	}
	if found := m.onDisk(t, needles...); len(found) > 0 {
		t.Errorf("the database files hold what was composed:\n  %s", strings.Join(found, "\n  "))
	}
	logs := m.logs.String()
	if !strings.Contains(logs, "message sent") {
		t.Fatalf("the debug log does not have the send at all; the test proves nothing:\n%s", logs)
	}
	for _, needle := range needles {
		if strings.Contains(logs, needle) {
			t.Errorf("the log holds %q", needle)
		}
	}
	if files := m.sendFiles(t); len(files) != 0 {
		t.Errorf("the spool still holds %v", files)
	}
	// What is kept is the record and its event: ids, state, counts.
	status, err := m.svc.SendStatus(t.Context(), ana, b.id, "private")
	if err != nil || status.Recipients != 3 || status.State != "sent" {
		t.Errorf("SendStatus = %+v, %v", status, err)
	}
	if finished := b.finished(t, since); len(finished) != 1 || finished[0].State != "sent" {
		t.Errorf("send.finished = %+v", finished)
	}
}

func TestUploadedAttachmentsAreRemovedFromTheSpoolWhenTheSendEnds(t *testing.T) {
	b := anaSends(t)
	uid := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "files@example.org", Subject: "Files", From: "bea@example.org", To: []string{"ana@mail.example"},
		InternalDate: time.Now().Add(-time.Hour), Raw: []byte(report),
	})
	b.m.index(t, b.id, b.box)
	original := b.m.messageID(t, b.id, "INBOX", uid)
	upload := func() service.Upload {
		return service.Upload{Filename: "notes.txt", ContentType: "text/plain", Body: strings.NewReader("remember the soup")}
	}
	// While the send runs, the upload and the forwarded part are in the
	// spool; each way it ends, they are gone.
	var during []string
	b.smtp.OnData(func([]byte) { during = b.m.sendFiles(t) })
	c := b.compose("bea@example.org")
	c.ForwardOf = original
	c.ForwardAttachments = []service.ForwardAttachment{{MessageID: original, Path: "2"}}
	if res, err := b.send(t, b.owner, "sent", c, upload()); err != nil || res.State != service.SendStateSent {
		t.Fatalf("sent: %+v, %v", res, err)
	}
	if len(during) != 3 {
		t.Errorf("during the send the spool held %v, want the upload, the forwarded part and the message "+
			"written out for the wire", during)
	}
	raw := string(b.smtp.Messages()[0].Raw)
	if !strings.Contains(raw, "notes.txt") || !strings.Contains(raw, "report.pdf") {
		t.Errorf("the message does not carry both attachments:\n%s", raw)
	}
	b.smtp.OnData(nil)

	b.smtp.RejectRecipient("nobody@example.org")
	if res, err := b.send(t, b.owner, "failed", b.compose("nobody@example.org"), upload()); err != nil || res.State != service.SendStateFailed {
		t.Fatalf("failed: %+v, %v", res, err)
	}
	b.smtp.DropAfterData(1)
	if res, err := b.send(t, b.owner, "unknown", b.compose("bea@example.org"), upload()); err != nil || res.State != service.SendStateUnknown {
		t.Fatalf("unknown: %+v, %v", res, err)
	}
	// Refused after the upload was spooled: too large for the provider.
	huge := service.Upload{Filename: "huge.bin", ContentType: "application/octet-stream",
		Body: io.LimitReader(zeros{}, 26<<20)}
	_, err := b.send(t, b.owner, "too-large", b.compose("bea@example.org"), upload(), huge)
	wantCode(t, "an attachment over the provider's limit", err, service.CodeBadRequest)
	if files := b.m.sendFiles(t); len(files) != 0 {
		t.Fatalf("the spool still holds %v", files)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestWithdrawingSendConsentStopsASendThatHasNotDialedYet(t *testing.T) {
	b := anaSends(t)
	uid := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "files@example.org", Subject: "Files", From: "bea@example.org", To: []string{"ana@mail.example"},
		InternalDate: time.Now().Add(-time.Hour), Raw: []byte(report),
	})
	b.m.index(t, b.id, b.box)
	original := b.m.messageID(t, b.id, "INBOX", uid)
	// The send is accepted and fetching what it forwards when the person
	// withdraws, in the console, in another tab.
	b.box.OnCall(func(ctx context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodFetchPart {
			if _, err := b.m.svc.WithdrawSendConsent(context.WithoutCancel(ctx), b.owner); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	c := b.compose("bea@example.org")
	c.ForwardOf = original
	c.ForwardAttachments = []service.ForwardAttachment{{MessageID: original, Path: "2"}}
	_, err := b.send(t, b.owner, "withdrawn", c)
	wantCode(t, "a send whose consent was withdrawn before it dialed", err, service.CodeConflict)
	if n := len(b.smtp.Auths()); n != 0 || b.smtp.DataCommands() != 0 {
		t.Fatalf("the submission server was reached (AUTH %v)", b.smtp.Auths())
	}
	if state, reason := b.row(t, "withdrawn"); state != "failed" || reason != service.SendReasonStopped {
		t.Fatalf("record = %s %s; want failed, so nothing is left in flight", state, reason)
	}
	if files := b.m.sendFiles(t); len(files) != 0 {
		t.Fatalf("the spool still holds %v", files)
	}
}

func TestAHeaderInjectionInTheSubjectIsRefused(t *testing.T) {
	b := anaSends(t)
	for name, change := range map[string]func(*service.Compose){
		"subject CRLF":    func(c *service.Compose) { c.Subject = "Hi\r\nBcc: evil@attacker.example" },
		"subject LF":      func(c *service.Compose) { c.Subject = "Hi\nBcc: evil@attacker.example" },
		"subject NUL":     func(c *service.Compose) { c.Subject = "Hi\x00there" },
		"name CRLF":       func(c *service.Compose) { c.To[0].Name = "Bea\r\nBcc: evil@attacker.example" },
		"address phrase":  func(c *service.Compose) { c.To[0].Email = "Bea <bea@example.org>" },
		"address CRLF":    func(c *service.Compose) { c.To[0].Email = "bea@example.org\r\nBcc: evil@attacker.example" },
		"two addresses":   func(c *service.Compose) { c.To[0].Email = "bea@example.org, evil@attacker.example" },
		"no recipient":    func(c *service.Compose) { c.To = nil },
		"subject too big": func(c *service.Compose) { c.Subject = strings.Repeat("a", service.MaxSubjectBytes+1) },
		"text too big":    func(c *service.Compose) { c.Text = strings.Repeat("a", service.MaxTextBytes+1) },
		"text not UTF-8":  func(c *service.Compose) { c.Text = "caf\xe9" },
		"too many recipients": func(c *service.Compose) {
			for i := range service.MaxRecipients {
				c.Cc = append(c.Cc, service.Address{Email: fmt.Sprintf("r%d@example.org", i)})
			}
		},
	} {
		c := b.compose("bea@example.org")
		change(&c)
		_, err := b.send(t, b.owner, "injected", c)
		wantCode(t, name, err, service.CodeBadRequest)
	}
	if n := b.smtp.DataCommands(); n != 0 || len(b.smtp.Auths()) != 0 {
		t.Fatalf("a refused message reached the server (%d DATA)", n)
	}
}

func TestAReplySetsAnsweredOnlyWithActionsAllowed(t *testing.T) {
	b := anaSends(t)
	uid := b.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "question@example.org", Subject: "Lunch?", From: "bea@example.org", To: []string{"ana@mail.example"},
		InternalDate: time.Now().Add(-time.Hour),
	})
	b.m.index(t, b.id, b.box)
	original := b.m.messageID(t, b.id, "INBOX", uid)
	reply := func(key string) {
		t.Helper()
		c := b.compose("bea@example.org")
		c.InReplyTo = original
		if res, err := b.send(t, b.owner, key, c); err != nil || res.State != service.SendStateSent {
			t.Fatalf("reply: %+v, %v", res, err)
		}
	}
	answered := func() bool {
		return slices.ContainsFunc(b.box.Flags("INBOX", uid), func(f imap.Flag) bool {
			return strings.EqualFold(string(f), string(imap.FlagAnswered))
		})
	}

	// Without actions allowed, the reply goes and the original is left alone.
	b.box.ResetCalls()
	reply("before")
	if answered() || b.box.CallCount(providertest.MethodStoreFlags) != 0 {
		t.Fatal("a reply changed the mailbox without the owner allowing actions")
	}
	msg, err := b.m.svc.GetMessage(t.Context(), b.owner, service.GetMessageRequest{ID: original})
	if err != nil || msg.Answered {
		t.Fatalf("the index says answered = %t (%v)", msg.Answered, err)
	}

	// Allowed, the original is marked on the server and in the index.
	if _, err := b.m.svc.GrantActionsConsent(t.Context(), b.owner, service.DefaultActionsConsentVersion); err != nil {
		t.Fatal(err)
	}
	reply("after")
	if !answered() {
		t.Fatal("the replied message is not answered on the server")
	}
	msg, err = b.m.svc.GetMessage(t.Context(), b.owner, service.GetMessageRequest{ID: original})
	if err != nil || !msg.Answered {
		t.Fatalf("the index says answered = %t (%v)", msg.Answered, err)
	}
}

func TestATemporaryRefusalBeforeDataIsRetriedWithTheSameMessage(t *testing.T) {
	b := anaSends(t)
	b.smtp.FailMailFrom(2)
	res, err := b.send(t, b.owner, "later", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	status, err := b.m.svc.SendStatus(t.Context(), b.owner, b.id, "later")
	if err != nil || status.Attempts != 3 {
		t.Fatalf("status = %+v, %v; want three attempts", status, err)
	}
	if msgs := b.smtp.Messages(); len(msgs) != 1 || header(t, msgs[0].Raw, "Message-ID")[0] != "<"+res.MessageID+">" {
		t.Fatalf("the server took %d messages", len(msgs))
	}

	// Past the retries the send fails, and nothing was sent.
	b.smtp.FailMailFrom(4)
	res, err = b.send(t, b.owner, "never", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonTemporary {
		t.Fatalf("SendMessage = %+v, %v; want failed after four attempts", res, err)
	}
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("DATA %d times", n)
	}
}

func TestAPersonSendsAtMostTheDailyLimit(t *testing.T) {
	b := anaSends(t)
	now := time.Now().Unix()
	for i := range service.DailySendLimit {
		if _, err := b.m.db.Writer().ExecContext(t.Context(),
			`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, user_id, created_at, updated_at)
			 VALUES (?, ?, 'h', 'm@x', 'sent', ?, ?, ?)`, b.id, fmt.Sprintf("old-%d", i), b.owner.UserID, now-60, now-60); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.send(t, b.owner, "one-too-many", b.compose("bea@example.org"))
	wantCode(t, "the 201st send of the day", err, service.CodeRateLimited)
	if n := b.smtp.DataCommands(); n != 0 {
		t.Fatalf("DATA %d times over the limit", n)
	}
}

// sendFiles lists what sends hold in the spool.
func (m *mailFixture) sendFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(m.spool, "send-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func (b *sendBox) lastSeq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := b.m.db.Reader().QueryRowContext(t.Context(), `SELECT coalesce(max(seq), 0) FROM events`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// finished are the send.finished events of the account after seq.
func (b *sendBox) finished(t *testing.T, since int64) []store.SendFinished {
	t.Helper()
	evs, _, err := events.NewJournal(b.m.db).Since(t.Context(), since, events.Filter{Types: []events.Type{events.TypeSendFinished}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.SendFinished
	for _, ev := range evs {
		if ev.Type != events.TypeSendFinished || ev.AccountID != b.id {
			continue
		}
		var p store.SendFinished
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// rawIn is a message's bytes as the fake server holds them.
func (b *sendBox) rawIn(t *testing.T, folder string, uid imap.UID) []byte {
	t.Helper()
	sess, err := b.box.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, err := sess.Select(t.Context(), folder, true, 0); err != nil {
		t.Fatal(err)
	}
	part, err := sess.FetchRaw(t.Context(), uid, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = part.Body.Close() }()
	raw, err := io.ReadAll(part.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// normalizeEnd drops the line break SMTP's end of data may add or take.
func normalizeEnd(raw []byte) []byte { return bytes.TrimRight(raw, "\r\n") }

func TestASubmissionServerRefusingARefreshedTokenFailsTheSendAndLeavesTheAccountAlone(t *testing.T) {
	// The grant opens the mailbox, and the submission server stops taking
	// what it mints, a freshly refreshed token included. A Microsoft 365
	// mailbox with SMTP AUTH turned off does exactly that while IMAP takes
	// the same grant: parking the account would take its reading away each
	// time its owner tried to send. The send fails; whether the grant is
	// dead is for IMAP to say.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	idp.set(func(f *fakeIDP) { f.onIssue = srv.AcceptToken })
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, onServer(t, srv, service.AddAccountRequest{Email: "ana@gmail.com"})))
	if err != nil {
		t.Fatal(err)
	}
	smtp := providertest.NewSMTPServer(t)
	if _, err := f.db.Writer().ExecContext(t.Context(),
		`UPDATE accounts SET smtp_host = ?, smtp_port = ?, smtp_tls = 'starttls' WHERE id = ?`,
		smtp.Host, smtp.Port, added.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSendConsent(t.Context(), ana, service.DefaultSendConsentVersion); err != nil {
		t.Fatal(err)
	}
	service.SetSendRetryForTest(f.svc, time.Millisecond)
	send := func(key string) (service.SendResult, error) {
		return f.svc.SendMessage(t.Context(), ana, service.SendRequest{IdempotencyKey: key, Compose: service.Compose{
			AccountID: added.Account.ID, To: []service.Address{{Email: "bea@example.org"}}, Subject: "Hi", Text: "Hello",
			Confirm: true,
		}})
	}

	// An expired token is refreshed, and the message goes.
	smtp.RefuseAuth(1)
	if res, err := send("first"); err != nil || res.State != service.SendStateSent {
		t.Fatalf("with a token that aged out: %+v, %v", res, err)
	}

	before := idp.refreshCount()
	smtp.RefuseAuth(2)
	res, err := send("second")
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonAuthFailed {
		t.Fatalf("with a refused token: %+v, %v", res, err)
	}
	if n := idp.refreshCount() - before; n != 1 {
		t.Errorf("%d refreshes before giving up, want one", n)
	}
	if n := len(smtp.Messages()); n != 1 {
		t.Errorf("the server took %d messages, want only the first", n)
	}
	stillActive := func(when string) {
		t.Helper()
		if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateActive {
			t.Fatalf("%s: account = %s %q, want active: the IMAP server still takes the grant", when, a.State, a.StateReason)
		}
		got, err := f.svc.GetAccount(t.Context(), ana, added.Account.ID)
		if err != nil || got.Send != (service.AccountSend{Available: true}) {
			t.Fatalf("%s: account send = %+v (%v)", when, got.Send, err)
		}
	}
	stillActive("after a refused token")

	// SMTP AUTH turned off for the mailbox says so, and a refresh does not
	// change it, so none is made.
	before = idp.refreshCount()
	smtp.RefuseAuthWith(1, providertest.Reply{Code: 535, Enhanced: [3]int{5, 7, 139},
		Message: "Authentication unsuccessful, SmtpClientAuthentication is disabled for the Tenant."})
	res, err = send("third")
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonAuthUnsupported {
		t.Fatalf("with SMTP AUTH off: %+v, %v", res, err)
	}
	if n := idp.refreshCount() - before; n != 0 {
		t.Errorf("%d refreshes for a server that has SMTP AUTH off", n)
	}
	stillActive("after SMTP AUTH turned out to be off")
	if res, err := send("fourth"); err != nil || res.State != service.SendStateSent {
		t.Fatalf("once the server takes the token again: %+v, %v", res, err)
	}
}

func TestWithdrawingSendConsentStopsASendWaitingBehindAnotherOnTheSameMailbox(t *testing.T) {
	// Two sends of one person on one mailbox: the first is on the wire —
	// a slow server, a large attachment — and the second, accepted and
	// reserved, waits for the mailbox's turn. The person withdraws sending
	// in My account. The second must not connect when its turn comes.
	b := anaSends(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.smtp.OnData(func([]byte) {
		once.Do(func() { close(arrived) })
		<-release
	})
	first := make(chan error, 1)
	go func() {
		res, err := b.send(t, b.owner, "first", b.compose("bea@example.org"))
		if err == nil && res.State != service.SendStateSent {
			err = fmt.Errorf("first = %+v", res)
		}
		first <- err
	}()
	<-arrived
	second := make(chan error, 1)
	go func() {
		_, err := b.send(t, b.owner, "second", b.compose("caio@example.org"))
		second <- err
	}()
	// Reserved, and past everything the service asks before the wait.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		err := b.m.db.Reader().QueryRowContext(t.Context(),
			`SELECT state FROM sends WHERE account_id = ? AND idempotency_key = 'second'`, b.id).Scan(&state)
		if err == nil && state == "sending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second send was never reserved")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := b.m.svc.WithdrawSendConsent(t.Context(), b.owner); err != nil {
		t.Fatal(err)
	}
	auths := len(b.smtp.Auths())
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the send already on the wire: %v", err)
	}

	err := <-second
	wantCode(t, "the send that was waiting its turn", err, service.CodeConflict)
	if n := len(b.smtp.Messages()); n != 1 || b.smtp.DataCommands() != 1 || len(b.smtp.Auths()) != auths {
		t.Fatalf("the server took %d messages, DATA %d, AUTH %d after the withdrawal; the second connected "+
			"after its sender withdrew consent", n, b.smtp.DataCommands(), len(b.smtp.Auths())-auths)
	}
	if state, reason := b.row(t, "second"); state != "failed" || reason != service.SendReasonStopped {
		t.Fatalf("record = %s %s; want failed, stopped", state, reason)
	}
}

func TestAFailedKeyFromYesterdayStillCountsTowardTheDailyLimit(t *testing.T) {
	// A key that failed sent nothing and may be used again, for any
	// message. Counted on the day it first failed, a stock of keys failed
	// cheaply one day would send past the limit the next.
	b := anaSends(t)
	now := time.Now().Unix()
	for i := range service.DailySendLimit - 1 {
		if _, err := b.m.db.Writer().ExecContext(t.Context(),
			`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, user_id, created_at, updated_at)
			 VALUES (?, ?, 'h', 'm@x', 'sent', ?, ?, ?)`, b.id, fmt.Sprintf("today-%d", i), b.owner.UserID, now-60, now-60); err != nil {
			t.Fatal(err)
		}
	}
	yesterday := now - 25*3600
	for i := range 3 {
		if _, err := b.m.db.Writer().ExecContext(t.Context(),
			`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, error, user_id, created_at, updated_at)
			 VALUES (?, ?, 'h', 'm@x', 'failed', 'recipients_refused', ?, ?, ?)`,
			b.id, fmt.Sprintf("yesterday-%d", i), b.owner.UserID, yesterday, yesterday); err != nil {
			t.Fatal(err)
		}
	}
	// The last one of today's two hundred.
	if res, err := b.send(t, b.owner, "yesterday-0", b.compose("bea@example.org")); err != nil || res.State != service.SendStateSent {
		t.Fatalf("the 200th send of the day: %+v, %v", res, err)
	}
	for _, key := range []string{"yesterday-1", "yesterday-2"} {
		_, err := b.send(t, b.owner, key, b.compose("caio@example.org"))
		wantCode(t, "a key that failed yesterday, over today's limit", err, service.CodeRateLimited)
	}
	if n := len(b.smtp.Messages()); n != 1 {
		t.Fatalf("%d messages went out with %d already sent today (limit %d)", n, service.DailySendLimit-1,
			service.DailySendLimit)
	}
}

func TestTheStoredComposeHashDoesNotConfirmAGuessOfTheMessage(t *testing.T) {
	// A one-word reply to one person is easy to guess. What the record and
	// the log keep of it must not let whoever holds them check a guess.
	key := bytes.Repeat([]byte{0x5a}, 32)
	m := newMailFixtureWith(t, fixtureOptions{logLevel: "debug", sendHashKey: key})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	c := service.Compose{AccountID: b.id, To: []service.Address{{Email: "bea@example.org"}}, Subject: "Yes", Text: "OK",
		Confirm: true}
	if res, err := b.send(t, ana, "guessable", c); err != nil || res.State != service.SendStateSent {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	// The message, encoded as the service encodes it for its hash.
	canonical, err := json.Marshal(struct {
		AccountID          string                      `json:"account_id"`
		To                 []service.Address           `json:"to"`
		Cc                 []service.Address           `json:"cc"`
		Bcc                []service.Address           `json:"bcc"`
		Subject            string                      `json:"subject"`
		Text               string                      `json:"text"`
		InReplyTo          int64                       `json:"in_reply_to"`
		ForwardOf          int64                       `json:"forward_of"`
		ForwardAttachments []service.ForwardAttachment `json:"forward_attachments"`
		Attachments        []struct{}                  `json:"attachments"`
	}{AccountID: b.id, To: c.To, Cc: []service.Address{}, Bcc: []service.Address{}, Subject: "Yes", Text: "OK"})
	if err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256(canonical)
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	var stored string
	if err := m.db.Reader().QueryRowContext(t.Context(),
		`SELECT compose_hash FROM sends WHERE idempotency_key = 'guessable'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("the stored compose_hash %s is not the keyed hash of the message; the test's encoding is stale", stored)
	}
	if stored == hex.EncodeToString(plain[:]) {
		t.Fatalf("the stored compose_hash %s is the plain SHA-256 of the message", stored)
	}

	// A key-less send gets a key made from the hash, and that goes in the
	// log and back to the caller.
	shared := m.sendingBox(t, service.Principal{}, "team@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	instance := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeSend, WorkspaceID: workspace.OperatorID}
	c.AccountID = shared.id
	if res, err := shared.send(t, instance, "", c); err != nil || res.State != service.SendStateSent {
		t.Fatalf("keyless SendMessage = %+v, %v", res, err)
	}
	canonical = bytes.Replace(canonical, []byte(b.id), []byte(shared.id), 1)
	plain = sha256.Sum256(canonical)
	if logs := m.logs.String(); !strings.Contains(logs, "send_key") || strings.Contains(logs, hex.EncodeToString(plain[:])) {
		t.Fatalf("the log holds the plain SHA-256 of the message (or no send key at all):\n%s", logs)
	}
}

func TestADefiniteRefusalAfterDataFailsTheSendAndIsNeverRetried(t *testing.T) {
	// The server read the message and said no: nothing was delivered, so it
	// is not unknown — the person is not told it may have gone, and the key
	// is free for another try — but nothing retries it by itself either.
	b := anaSends(t)
	b.smtp.RefuseData(1, providertest.Reply{Code: 554, Enhanced: [3]int{5, 2, 0},
		Message: "STOREDRV.Submission.Exception:SubmissionQuotaExceededException"})
	res, err := b.send(t, b.owner, "over-quota", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonRefused {
		t.Fatalf("SendMessage = %+v, %v; want failed, refused", res, err)
	}
	b.smtp.RefuseData(1, providertest.Reply{Code: 451, Enhanced: [3]int{4, 3, 0}, Message: "Try again later"})
	res, err = b.send(t, b.owner, "later", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonTemporary {
		t.Fatalf("SendMessage = %+v, %v; want failed, temporary", res, err)
	}
	if n := b.smtp.DataCommands(); n != 2 || len(b.smtp.Messages()) != 0 {
		t.Fatalf("DATA %d times for two sends; a message the server read is never sent again by itself", n)
	}
	// Failed, so the person may try again, with the same key.
	if res, err := b.send(t, b.owner, "over-quota", b.compose("bea@example.org")); err != nil ||
		res.State != service.SendStateSent || res.Replayed {
		t.Fatalf("again = %+v, %v; want sent now", res, err)
	}
}

func TestARecipientRefusedForNowIsTriedAgainAndNamesNobody(t *testing.T) {
	b := anaSends(t)
	b.smtp.AnswerRecipient("bea@example.org", providertest.Reply{Code: 452, Enhanced: [3]int{4, 5, 3}, Message: "Too many recipients"})
	res, err := b.send(t, b.owner, "for-now", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateFailed || res.Reason != service.SendReasonTemporary || len(res.Rejected) != 0 {
		t.Fatalf("SendMessage = %+v, %v; want failed, temporary, with no recipient named as refused", res, err)
	}
	status, err := b.m.svc.SendStatus(t.Context(), b.owner, b.id, "for-now")
	if err != nil || status.Attempts != 4 {
		t.Fatalf("status = %+v, %v; want the first attempt and three more", status, err)
	}
	if n := b.smtp.DataCommands(); n != 0 {
		t.Fatalf("DATA %d times", n)
	}
}

func TestTheCopyInSentNamesTheBlindRecipients(t *testing.T) {
	b := anaSends(t)
	c := b.compose("bea@example.org")
	c.Bcc = []service.Address{{Name: "Caio", Email: "caio@example.org"}}
	if res, err := b.send(t, b.owner, "blind", c); err != nil || res.State != service.SendStateSent {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	wire := b.smtp.Messages()[0].Raw
	if bcc := header(t, wire, "Bcc"); len(bcc) != 0 {
		t.Fatalf("the transmitted message has Bcc %q", bcc)
	}
	sent, _ := b.box.Folder("Sent")
	if len(sent.UIDs) != 1 {
		t.Fatalf("Sent holds %d messages", len(sent.UIDs))
	}
	filed := b.rawIn(t, "Sent", sent.UIDs[0])
	if bcc := header(t, filed, "Bcc"); len(bcc) != 1 || bcc[0] != `"Caio" <caio@example.org>` {
		t.Fatalf("the copy in Sent has Bcc %q, want the blind recipient", bcc)
	}
	head, rest, _ := bytes.Cut(filed, []byte("\r\n"))
	if !bytes.HasPrefix(head, []byte("Bcc:")) || !bytes.Equal(normalizeEnd(rest), normalizeEnd(wire)) {
		t.Fatalf("the copy in Sent is not the transmitted message behind one Bcc header:\n%s\n---\n%s", filed, wire)
	}
}

func TestAnUploadIsLabelledUTF8OnlyWhenAllOfItIsUTF8(t *testing.T) {
	b := anaSends(t)
	// ASCII for longer than the first bytes a type is sniffed from, then a
	// name in Windows-1252: a CSV exported by Excel.
	latin1 := strings.Repeat("id;city\r\n", 100) + "7;S\xe3o Paulo\r\n"
	if _, err := b.send(t, b.owner, "csv", b.compose("bea@example.org"),
		service.Upload{Filename: "cities.csv", ContentType: "text/csv", Body: strings.NewReader(latin1)},
		service.Upload{Filename: "names.csv", ContentType: "text/csv", Body: strings.NewReader("José;São Paulo\r\n")}); err != nil {
		t.Fatal(err)
	}
	raw := string(b.smtp.Messages()[0].Raw)
	if !strings.Contains(raw, "Content-Type: text/csv; name=\"cities.csv\"") ||
		!strings.Contains(raw, "Content-Type: text/csv; charset=utf-8; name=\"names.csv\"") {
		t.Fatalf("the attachments' types are wrong:\n%s", raw)
	}
}

func TestASendHoldsTheSpoolItUsesNotTheMostItCouldHaveUsed(t *testing.T) {
	// Until its attachments are in, a send holds the most they may be: the
	// provider's limit. Once they are, it holds what it has — the files and
	// the message written out for the wire — and a short text holds almost
	// nothing, so a budget of little more than one limit still lets a
	// second send through while the first is on the wire.
	limit := provider.ProfileFor(provider.KindIMAP).SMTPMaxSize
	m := newMailFixtureWith(t, fixtureOptions{sendSpool: limit + 1<<20})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	one := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	two := m.sendingBox(t, ana, "ana@other.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	arrived, release := make(chan struct{}), make(chan struct{})
	one.smtp.OnData(func([]byte) {
		close(arrived)
		<-release
	})
	first := make(chan error, 1)
	go func() {
		_, err := one.send(t, ana, "first", one.compose("bea@example.org"))
		first <- err
	}()
	<-arrived
	res, err := two.send(t, ana, "second", two.compose("bea@example.org"))
	close(release)
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("a short message while another is on the wire: %+v, %v", res, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestAMessageGoesOutUnderItsOwnersNameNeverTheMailboxLabel(t *testing.T) {
	b := anaSends(t)
	// The console lets a person label a mailbox ("gmail", "work"); recipients
	// must never see that as the sender's name.
	if _, err := b.m.db.Writer().ExecContext(t.Context(),
		`UPDATE accounts SET display_name = 'gmail' WHERE id = ?`, b.id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, b.owner, "no-name-yet", b.compose("bea@example.org")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.db.Writer().ExecContext(t.Context(),
		`UPDATE users SET name = 'Ana Souza' WHERE id = ?`, b.owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, b.owner, "named", b.compose("bea@example.org")); err != nil {
		t.Fatal(err)
	}
	sent := b.smtp.Messages()
	if len(sent) != 2 {
		t.Fatalf("%d messages reached the server, want 2", len(sent))
	}
	if from := header(t, sent[0].Raw, "From"); len(from) != 1 || from[0] != "<ana@mail.example>" {
		t.Errorf("with no name in the profile, From = %q, want the address alone", from)
	}
	if from := header(t, sent[1].Raw, "From"); len(from) != 1 || from[0] != `"Ana Souza" <ana@mail.example>` {
		t.Errorf("From = %q, want the owner's profile name", from)
	}
	a, err := b.m.svc.GetAccount(t.Context(), b.owner, b.id)
	if err != nil || a.Send.FromName != "Ana Souza" {
		t.Fatalf("the account shows from_name %q (%v), want the owner's name", a.Send.FromName, err)
	}
}
