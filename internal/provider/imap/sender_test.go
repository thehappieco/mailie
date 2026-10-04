package imap_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/mail"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// countingTokens is a token source that hands out a new token after every
// Invalidate, and counts them.
type countingTokens struct {
	mu          sync.Mutex
	invalidated int
}

func (c *countingTokens) Token(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "access-" + strings.Repeat("x", c.invalidated+1), nil
}

func (c *countingTokens) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidated++
}

// senderTo is a sender that submits to smtp in the clear, as tests may.
func senderTo(t *testing.T, smtp *providertest.SMTPServer, creds provider.Credentials) provider.Sender {
	t.Helper()
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: "127.0.0.1:1", SMTPHost: smtp.Host, SMTPPort: smtp.Port,
		Credentials: creds, SpoolDir: t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mb.Sender()
}

func outgoing(to ...string) provider.Outgoing {
	out := provider.Outgoing{
		MessageID: "4f1c2d3e-0000-4000-8000-000000000001@example.com",
		From:      provider.Address{Name: "Lima, Ana \"A\"", Email: "ana@example.com"},
		Subject:   "Lunch",
		TextBody:  "Noon.",
	}
	for _, addr := range to {
		out.To = append(out.To, provider.Address{Email: addr})
	}
	return out
}

func TestARefusedRecipientIsReportedByAddressAndNothingIsSent(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	smtp.RejectRecipient("ghost@example.org")
	smtp.RejectRecipient("phantom@example.org")
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	_, err := sender.Send(t.Context(), outgoing("bea@example.org", "ghost@example.org", "phantom@example.org"))
	var refused *provider.RecipientError
	if !errors.As(err, &refused) {
		t.Fatalf("Send = %v, want a RecipientError", err)
	}
	var got []string
	for _, r := range refused.Rejected {
		got = append(got, r.Address)
	}
	// Not the message's id, which go-mail writes into the same error text
	// right after the recipients.
	if strings.Join(got, ",") != "ghost@example.org,phantom@example.org" {
		t.Fatalf("rejected = %q", got)
	}
	if n := smtp.DataCommands(); n != 0 {
		t.Fatalf("DATA %d times after a refused recipient", n)
	}
}

func TestAConnectionLostAfterDataIsAnUnknownOutcome(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	smtp.DropAfterData(1)
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	_, err := sender.Send(t.Context(), outgoing("bea@example.org"))
	if !errors.Is(err, provider.ErrOutcomeUnknown) {
		t.Fatalf("Send = %v, want ErrOutcomeUnknown", err)
	}
	if errors.Is(err, provider.ErrTemporary) || errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("Send = %v, which a caller would retry", err)
	}
}

func TestAnOAuthTokenTheServerRefusesIsRefreshedOnceBeforeTheSendGivesUp(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	tokens := &countingTokens{}
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Tokens: tokens})

	// An access token that aged out: refused once, refreshed, sent.
	smtp.RefuseAuth(1)
	res, err := sender.Send(t.Context(), outgoing("bea@example.org"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ServerReply == "" || res.Copy != nil {
		t.Errorf("result = reply %q, copy %v; want the server's answer and no copy nobody asked for", res.ServerReply, res.Copy)
	}
	if tokens.invalidated != 1 || len(smtp.Auths()) != 2 || len(smtp.Messages()) != 1 {
		t.Fatalf("invalidated %d, AUTH %v, %d messages; want one refresh and one message",
			tokens.invalidated, smtp.Auths(), len(smtp.Messages()))
	}

	// A freshly refreshed token refused again: the send fails, and that is
	// all it says. Not ErrNeedsReauth, which parks the account: a submission
	// server refuses a grant the IMAP server still takes when SMTP AUTH is
	// off for the mailbox.
	smtp.RefuseAuth(2)
	_, err = sender.Send(t.Context(), outgoing("bea@example.org"))
	if !errors.Is(err, provider.ErrAuthFailed) || errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("Send = %v, want ErrAuthFailed and not ErrNeedsReauth", err)
	}
	if tokens.invalidated != 2 || len(smtp.Messages()) != 1 {
		t.Fatalf("invalidated %d, %d messages", tokens.invalidated, len(smtp.Messages()))
	}

	// A password is not refreshed: refused once is refused.
	password := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	before := len(smtp.Auths())
	smtp.RefuseAuth(1)
	if _, err := password.Send(t.Context(), outgoing("bea@example.org")); !errors.Is(err, provider.ErrAuthFailed) {
		t.Fatalf("a refused password: %v", err)
	}
	if n := len(smtp.Auths()) - before; n != 1 {
		t.Fatalf("a refused password was tried %d times", n)
	}
}

func TestADisplayNameWithACommaOrAQuoteStaysOneAddress(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	out := outgoing()
	out.To = []provider.Address{{Name: "Lima, Bea", Email: "bea@example.org"}, {Name: "Caio \"C\" Souza", Email: "caio@example.org"}}
	if _, err := sender.Send(t.Context(), out); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := smtp.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 2 {
		t.Fatalf("the server was given recipients %v", msgs)
	}
	raw := string(msgs[0].Raw)
	for _, want := range []string{`"Lima, Bea" <bea@example.org>`, `<caio@example.org>`, `From: "Lima, Ana \"A\"" <ana@example.com>`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the message lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "go-mail") {
		t.Errorf("the message names the library that built it:\n%s", raw)
	}
}

func TestSMTPAuthTurnedOffForTheMailboxIsNeitherRefreshedNorADeadGrant(t *testing.T) {
	// Microsoft 365 with SMTP AUTH disabled for the tenant answers every
	// XOAUTH2 with 535 5.7.139, while IMAP takes the same token.
	smtp := providertest.NewSMTPServer(t)
	tokens := &countingTokens{}
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Tokens: tokens})
	smtp.RefuseAuthWith(2, providertest.Reply{Code: 535, Enhanced: [3]int{5, 7, 139},
		Message: "Authentication unsuccessful, SmtpClientAuthentication is disabled for the Tenant."})
	_, err := sender.Send(t.Context(), outgoing("bea@example.org"))
	if !errors.Is(err, provider.ErrAuthUnsupported) || errors.Is(err, provider.ErrAuthFailed) ||
		errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("Send = %v, want ErrAuthUnsupported: no refresh changes it, and the grant is fine", err)
	}
	if tokens.invalidated != 0 || len(smtp.Auths()) != 1 {
		t.Fatalf("invalidated %d, AUTH %v; a refresh cannot turn SMTP AUTH on", tokens.invalidated, smtp.Auths())
	}
}

func TestAPasswordSignsInWithLoginWhenThatIsAllTheServerOffers(t *testing.T) {
	// Exchange on premises, and Office 365 with an app password, offer
	// LOGIN without PLAIN.
	smtp := providertest.NewSMTPServer(t)
	smtp.OfferAuth("LOGIN")
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	if _, err := sender.Send(t.Context(), outgoing("bea@example.org")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if auths := smtp.Auths(); len(auths) != 1 || auths[0] != "LOGIN" || len(smtp.Messages()) != 1 {
		t.Fatalf("AUTH %v, %d messages; want one LOGIN and the message", auths, len(smtp.Messages()))
	}

	// PLAIN still, when it is offered too.
	both := providertest.NewSMTPServer(t)
	both.OfferAuth("LOGIN", "PLAIN")
	if _, err := senderTo(t, both, provider.Credentials{User: "ana@example.com", Password: "hunter2"}).
		Send(t.Context(), outgoing("bea@example.org")); err != nil || both.Auths()[0] != "PLAIN" {
		t.Fatalf("Send = %v, AUTH %v; want PLAIN", err, both.Auths())
	}

	// Nothing this client speaks: said at once, and not as a passing
	// failure the service would try again for a minute.
	none := providertest.NewSMTPServer(t)
	none.OfferAuth("CRAM-MD5")
	_, err := senderTo(t, none, provider.Credentials{User: "ana@example.com", Password: "hunter2"}).
		Send(t.Context(), outgoing("bea@example.org"))
	if !errors.Is(err, provider.ErrAuthUnsupported) || errors.Is(err, provider.ErrTemporary) {
		t.Fatalf("Send = %v, want ErrAuthUnsupported and not ErrTemporary", err)
	}
	if n := len(none.Auths()); n != 0 {
		t.Fatalf("%d AUTH attempts with a mechanism the server does not offer", n)
	}
}

func TestAServerThatRefusesTheMessageItReadIsAFailureNotAnUnknown(t *testing.T) {
	// RFC 5321: a 4yz or 5yz answer to the end of the data means the
	// message was not accepted. Microsoft over its daily quota, Gmail
	// blocking an attachment, a server short of space for now.
	for _, c := range []struct {
		name  string
		reply providertest.Reply
		want  error
	}{
		{"over the submission quota", providertest.Reply{Code: 554, Enhanced: [3]int{5, 2, 0},
			Message: "STOREDRV.Submission.Exception:SubmissionQuotaExceededException"}, provider.ErrTerminal},
		{"a blocked attachment", providertest.Reply{Code: 552, Enhanced: [3]int{5, 7, 0},
			Message: "This message was blocked because its content presents a potential security issue"}, provider.ErrTerminal},
		{"too big after all", providertest.Reply{Code: 552, Enhanced: [3]int{5, 3, 4}, Message: "Message too big"},
			provider.ErrTooLarge},
		{"not now", providertest.Reply{Code: 451, Enhanced: [3]int{4, 3, 0}, Message: "Try again later"},
			provider.ErrTemporary},
	} {
		t.Run(c.name, func(t *testing.T) {
			smtp := providertest.NewSMTPServer(t)
			smtp.RefuseData(1, c.reply)
			sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
			_, err := sender.Send(t.Context(), outgoing("bea@example.org"))
			if !errors.Is(err, c.want) || !errors.Is(err, provider.ErrAfterData) {
				t.Fatalf("Send = %v, want %v, marked as refused after the message was sent", err, c.want)
			}
			if errors.Is(err, provider.ErrOutcomeUnknown) {
				t.Fatalf("Send = %v: the server answered, so the outcome is known", err)
			}
			if !errors.Is(c.want, provider.ErrTooLarge) && errors.Is(err, provider.ErrTooLarge) {
				t.Fatalf("Send = %v: a 552 about the content is not about the size", err)
			}
			if smtp.DataCommands() != 1 || len(smtp.Messages()) != 0 {
				t.Fatalf("DATA %d, %d messages kept", smtp.DataCommands(), len(smtp.Messages()))
			}
		})
	}
}

func TestARecipientRefusedForNowIsNotARefusedRecipient(t *testing.T) {
	// 452 4.5.3: too many recipients in one transaction. Nobody's address is
	// wrong, and nothing was sent: the send may be tried again.
	smtp := providertest.NewSMTPServer(t)
	smtp.AnswerRecipient("bea@example.org", providertest.Reply{Code: 452, Enhanced: [3]int{4, 5, 3}, Message: "Too many recipients"})
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Password: "hunter2"})
	_, err := sender.Send(t.Context(), outgoing("bea@example.org", "caio@example.org"))
	var refused *provider.RecipientError
	if errors.As(err, &refused) || !errors.Is(err, provider.ErrTemporary) || errors.Is(err, provider.ErrTerminal) {
		t.Fatalf("Send = %v, want ErrTemporary and no refused recipient", err)
	}
	if n := smtp.DataCommands(); n != 0 {
		t.Fatalf("DATA %d times", n)
	}

	// Refused for good and for now at once: the ones refused for good are
	// what the caller has to fix, and only they are named.
	smtp.RejectRecipient("ghost@example.org")
	_, err = sender.Send(t.Context(), outgoing("bea@example.org", "ghost@example.org"))
	if !errors.As(err, &refused) || len(refused.Rejected) != 1 || refused.Rejected[0].Address != "ghost@example.org" {
		t.Fatalf("Send = %v, want only the recipient refused for good", err)
	}
}

func TestTheCopyForSentNamesTheBlindRecipientsTheWireNeverCarries(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	spool := t.TempDir()
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: "127.0.0.1:1", SMTPHost: smtp.Host, SMTPPort: smtp.Port,
		Credentials: provider.Credentials{User: "ana@example.com", Password: "hunter2"}, SpoolDir: spool,
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := outgoing("bea@example.org")
	out.Bcc = []provider.Address{{Name: "Caio Souza", Email: "caio@example.org"}, {Email: "dora@example.org"}}
	out.KeepCopy = true
	res, err := mb.Sender().Send(t.Context(), out)
	if err != nil || res.Copy == nil {
		t.Fatalf("Send = %+v, %v; want a copy for Sent", res, err)
	}
	sent := smtp.Messages()
	if len(sent) != 1 || len(sent[0].To) != 3 {
		t.Fatalf("the server was given %v; the blind recipients are envelope recipients", sent)
	}
	wire := sent[0].Raw
	if bytes.Contains(bytes.ToLower(wire), []byte("bcc")) || bytes.Contains(wire, []byte("dora@")) {
		t.Fatalf("the transmitted message names its blind recipients:\n%s", wire)
	}

	body, err := res.Copy.Open()
	if err != nil {
		t.Fatal(err)
	}
	copied, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(copied)) != res.Copy.Size {
		t.Fatalf("the copy is %d bytes and says %d", len(copied), res.Copy.Size)
	}
	head, rest, ok := bytes.Cut(copied, []byte("\r\n"))
	if !ok || string(head) != `Bcc: "Caio Souza" <caio@example.org>, <dora@example.org>` {
		t.Fatalf("the copy starts %q, want the Bcc header", head)
	}
	// Otherwise exactly what went over the wire.
	if !bytes.Equal(bytes.TrimRight(rest, "\r\n"), bytes.TrimRight(wire, "\r\n")) {
		t.Fatalf("the copy differs from what was sent by more than its Bcc:\n%s\n---\n%s", rest, wire)
	}
	if parsed, err := mail.ReadMessage(bytes.NewReader(copied)); err != nil || parsed.Header.Get("Bcc") == "" ||
		parsed.Header.Get("Message-Id") == "" {
		t.Fatalf("the copy does not parse as one message with its Bcc: %v", err)
	}

	// The copy lives in the spool until it is discarded; nothing else does.
	if files, _ := filepath.Glob(filepath.Join(spool, "*")); len(files) != 1 {
		t.Fatalf("the spool holds %v, want the copy alone", files)
	}
	res.Copy.Discard()
	res.Copy.Discard()
	if files, _ := filepath.Glob(filepath.Join(spool, "*")); len(files) != 0 {
		t.Fatalf("the spool still holds %v", files)
	}
}

func TestAnAttachmentIsReadWhenTheMessageIsWrittenOutNeverHeld(t *testing.T) {
	// go-mail's AttachReader reads the whole file into memory when it is
	// attached, and a send used to keep that copy for as long as it waited
	// its turn. The attachment is opened when the message is written, and
	// only then.
	opened := 0
	out := outgoing("bea@example.org")
	out.Attachments = []provider.OutgoingAttachment{{
		Filename: "notes.txt", ContentType: "text/plain",
		Open: func() (io.ReadCloser, error) {
			opened++
			return io.NopCloser(strings.NewReader("remember the soup")), nil
		},
	}}
	msg, err := imapprovider.BuildMessage(out)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Fatalf("building the message opened the attachment %d times", opened)
	}
	for i := 1; i <= 2; i++ {
		var buf bytes.Buffer
		if _, err := msg.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		if opened != i || !strings.Contains(buf.String(), "notes.txt") {
			t.Fatalf("writing the message out %d times opened the attachment %d times", i, opened)
		}
	}

	// And a send leaves nothing of the message behind in the spool.
	smtp := providertest.NewSMTPServer(t)
	spool := t.TempDir()
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: "127.0.0.1:1", SMTPHost: smtp.Host, SMTPPort: smtp.Port,
		Credentials: provider.Credentials{User: "ana@example.com", Password: "hunter2"}, SpoolDir: spool,
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var during []string
	smtp.OnData(func([]byte) { during, _ = filepath.Glob(filepath.Join(spool, "*")) })
	if _, err := mb.Sender().Send(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("remember the soup"))
	if len(during) != 1 || !strings.Contains(string(smtp.Messages()[0].Raw), encoded) {
		t.Fatalf("during the send the spool held %v; want the message written out, streamed from there", during)
	}
	smtp.DropAfterData(1)
	if _, err := mb.Sender().Send(t.Context(), out); !errors.Is(err, provider.ErrOutcomeUnknown) {
		t.Fatalf("Send = %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(spool, "*")); len(files) != 0 {
		t.Fatalf("the spool still holds %v", files)
	}
}

func TestBeforeDialIsAskedAfterTheWaitAndBeforeEveryConnection(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	tokens := &countingTokens{}
	sender := senderTo(t, smtp, provider.Credentials{User: "ana@example.com", Tokens: tokens})

	// A send of the same account in flight: the second waits its turn, and
	// is asked only when it comes.
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	smtp.OnData(func([]byte) {
		once.Do(func() { close(arrived) })
		<-release
	})
	first := make(chan error, 1)
	go func() {
		_, err := sender.Send(t.Context(), outgoing("bea@example.org"))
		first <- err
	}()
	<-arrived
	withdrawn := errors.New("sending was withdrawn")
	var mu sync.Mutex
	var asked []string
	second := outgoing("caio@example.org")
	second.MessageID = "second@example.com"
	second.BeforeDial = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, "second")
		return withdrawn
	}
	done := make(chan error, 1)
	go func() {
		_, err := sender.Send(t.Context(), second)
		done <- err
	}()
	// However long it waits, it is not asked while the first holds the
	// account.
	select {
	case err := <-done:
		t.Fatalf("the second send ended (%v) while the first was still on the wire", err)
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	early := len(asked)
	mu.Unlock()
	if early != 0 {
		t.Fatal("the second send was asked before its turn")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, withdrawn) {
		t.Fatalf("the second send = %v, want the refusal it was given, as it is", err)
	}
	if n := len(smtp.Auths()); n != 1 || smtp.DataCommands() != 1 {
		t.Fatalf("AUTH %d, DATA %d; the refused send connected", n, smtp.DataCommands())
	}

	// The retry with a refreshed token is a connection too.
	smtp.OnData(nil)
	smtp.RefuseAuth(1)
	calls := 0
	third := outgoing("bea@example.org")
	third.BeforeDial = func(context.Context) error {
		calls++
		if calls > 1 {
			return withdrawn
		}
		return nil
	}
	before := len(smtp.Auths())
	if _, err := sender.Send(t.Context(), third); !errors.Is(err, withdrawn) {
		t.Fatalf("Send = %v, want the refusal before the retry", err)
	}
	if calls != 2 || len(smtp.Auths())-before != 1 {
		t.Fatalf("asked %d times, AUTH %d times; want asked before each connection and no second one",
			calls, len(smtp.Auths())-before)
	}
}

func TestASubmissionIntroducesItselfWithTheDeploymentsPublicHost(t *testing.T) {
	smtp := providertest.NewSMTPServer(t)
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: "127.0.0.1:1", SMTPHost: smtp.Host, SMTPPort: smtp.Port,
		Credentials: provider.Credentials{User: "ana@example.com", Password: "hunter2"},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true, SMTPHelo: "console.mailie.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Sender().Send(t.Context(), outgoing("bea@example.org")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msgs := smtp.Messages(); len(msgs) != 1 || msgs[0].Helo != "console.mailie.example" {
		t.Fatalf("the server was greeted as %q, want the public host", msgs[0].Helo)
	}
}
