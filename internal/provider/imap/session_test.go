package imap_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

const sampleMessage = "From: Ana <ana@example.com>\r\n" +
	"To: Person <person@example.com>\r\n" +
	"Subject: Fatura de setembro\r\n" +
	"Message-ID: <fatura-09@example.com>\r\n" +
	"Date: Mon, 21 Sep 2026 10:15:00 -0300\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Segue a fatura em anexo.\r\n"

type harness struct {
	server  *providertest.IMAPServer
	mailbox *imapprovider.Mailbox
	tokens  *providertest.TokenSource
}

func newHarness(t *testing.T, opts providertest.IMAPOptions, tokens ...string) *harness {
	t.Helper()
	srv := providertest.NewIMAPServer(t, opts)
	src := providertest.NewTokenSource(tokens...)
	for _, token := range src.Tokens() {
		srv.AcceptToken(token)
	}
	mb, err := imapprovider.New(provider.Config{
		Kind:     provider.KindMicrosoft,
		IMAPAddr: srv.Addr,
		Credentials: provider.Credentials{
			User:   srv.User,
			Tokens: src,
		},
		SpoolDir:          t.TempDir(),
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{server: srv, mailbox: mb, tokens: src}
}

func (h *harness) open(t *testing.T, role provider.Role) provider.Session {
	t.Helper()
	sess, err := h.mailbox.Open(t.Context(), role)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestXOAUTH2LogsInAgainstARealServer(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleSync)

	if len(h.server.AuthAttempts()) != 1 {
		t.Fatalf("expected exactly one authentication, got %v", h.server.AuthAttempts())
	}
	if !sess.Caps().UIDPlus {
		t.Error("capabilities were not read after authentication")
	}
}

func TestAnExpiredTokenIsRefreshedOnceAndTheAccountRecovers(t *testing.T) {
	// The common case by far: the access token aged out. Asking a person to
	// re-consent here would be wrong, and is what happens if a refusal is
	// taken at face value.
	h := newHarness(t, providertest.IMAPOptions{}, "stale-token", "fresh-token")
	h.server.RejectToken("stale-token")

	sess, err := h.mailbox.Open(t.Context(), provider.RoleSync)
	if err != nil {
		t.Fatalf("Open should have recovered by refreshing: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if h.tokens.Refreshes() != 1 {
		t.Errorf("refreshes = %d, want exactly one", h.tokens.Refreshes())
	}
	attempts := h.server.AuthAttempts()
	if len(attempts) != 2 || attempts[0] != "stale-token" || attempts[1] != "fresh-token" {
		t.Fatalf("authentication attempts = %v, want the stale token then the fresh one", attempts)
	}
}

func TestARefusalThatSurvivesARefreshStopsTheAccount(t *testing.T) {
	// A token minted seconds ago and still refused is a grant that is really
	// gone; only a person can fix it, and retrying would be a loop.
	h := newHarness(t, providertest.IMAPOptions{}, "one", "two")
	h.server.RejectToken("one")
	h.server.RejectToken("two")

	_, err := h.mailbox.Open(t.Context(), provider.RoleSync)
	if !errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("want ErrNeedsReauth, got %v", err)
	}
}

// gmailMailbox is a Gmail account on a server that refuses the way Gmail does,
// logging in as user.
func gmailMailbox(t *testing.T, srv *providertest.IMAPServer, user string, src *providertest.TokenSource) *imapprovider.Mailbox {
	t.Helper()
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindGmail, IMAPAddr: srv.Addr,
		Credentials: provider.Credentials{User: user, Tokens: src},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mb
}

func TestAGmailRefusalCarriesItsStatusAndScopeIntoTheError(t *testing.T) {
	// The production incident: a grant that refreshes fine and never opens
	// the mailbox, and a log that said only "needs re-authorization". What
	// Gmail explained in its continuation has to survive to the log.
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})

	// A token for another Google account than the address typed: 400, which
	// no refresh can fix, so none is attempted.
	other := providertest.NewTokenSource("for-someone-else", "fresh")
	srv.AcceptToken("for-someone-else")
	_, err := gmailMailbox(t, srv, "ana.work@gmail.com", other).Open(t.Context(), provider.RoleInteractive)
	if !errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("a token for another user: want ErrNeedsReauth, got %v", err)
	}
	if want := "status 400, scope " + providertest.GmailScope; !strings.Contains(err.Error(), want) {
		t.Errorf("the error lost what Gmail said: %q, want it to contain %q", err, want)
	}
	if other.Refreshes() != 0 {
		t.Errorf("a 400 was refreshed %d times; only an expired token is worth a refresh", other.Refreshes())
	}
	// The tagged line that followed goes to the log too, never to a caller.
	if reply := provider.ServerReply(err); !strings.Contains(reply, "NO [AUTHENTICATIONFAILED] Invalid credentials") {
		t.Errorf("ServerReply = %q, want Gmail's tagged refusal", reply)
	}
	if strings.Contains(err.Error(), "Invalid credentials") {
		t.Errorf("the server's words reached the error a caller sees: %q", err)
	}

	// A token refused as expired, twice: one refresh, then the account stops,
	// and the error still says what the second refusal was.
	refused := providertest.NewTokenSource("stale", "also-refused")
	_, err = gmailMailbox(t, srv, "ana@gmail.com", refused).Open(t.Context(), provider.RoleInteractive)
	if !errors.Is(err, provider.ErrNeedsReauth) || errors.Is(err, provider.ErrAuthFailed) {
		t.Fatalf("a refusal that survived a refresh: want only ErrNeedsReauth, got %v", err)
	}
	for _, want := range []string{"freshly refreshed", "status 401, scope " + providertest.GmailScope} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if refused.Refreshes() != 1 {
		t.Errorf("refreshes = %d, want exactly one", refused.Refreshes())
	}
	if reply := provider.ServerReply(err); !strings.Contains(reply, "Invalid credentials") {
		t.Errorf("the second refusal's tagged line was lost: ServerReply = %q", reply)
	}
	for _, token := range []string{"stale", "also-refused"} {
		if strings.Contains(err.Error(), token) {
			t.Errorf("the error carries the access token %q: %q", token, err)
		}
	}

	// And an expired token that a refresh fixes is no failure at all.
	recovers := providertest.NewTokenSource("expired", "renewed")
	srv.AcceptToken("renewed")
	sess, err := gmailMailbox(t, srv, "ana@gmail.com", recovers).Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("a 401 followed by a good token: %v", err)
	}
	_ = sess.Close()
}

func TestAServerThatIsDownIsNotARefusedToken(t *testing.T) {
	// A refusal that survives a refresh now throws away a new consent and
	// parks a working account. A provider whose backend is down must not
	// look like one, whichever way it says so.
	exchange := newHarness(t, providertest.IMAPOptions{}, "good", "also-good")
	exchange.server.Unavailable(true)
	_, err := exchange.mailbox.Open(t.Context(), provider.RoleInteractive)
	if !errors.Is(err, provider.ErrTemporary) || errors.Is(err, provider.ErrAuthFailed) || errors.Is(err, provider.ErrNeedsReauth) {
		t.Errorf("Exchange's \"Server Unavailable\": want only ErrTemporary, got %v", err)
	}
	if n := exchange.tokens.Refreshes(); n != 0 {
		t.Errorf("a server that is down cost %d refreshes", n)
	}
	if reply := provider.ServerReply(err); reply != "imap: NO Server Unavailable. 15" {
		t.Errorf("ServerReply = %q, want Exchange's line", reply)
	}

	gmail := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	src := providertest.NewTokenSource("good", "also-good")
	gmail.AcceptToken("good")
	gmail.Unavailable(true)
	_, err = gmailMailbox(t, gmail, "ana@gmail.com", src).Open(t.Context(), provider.RoleInteractive)
	if !errors.Is(err, provider.ErrTemporary) || errors.Is(err, provider.ErrNeedsReauth) {
		t.Errorf("a 503 continuation: want ErrTemporary, got %v", err)
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("the error lost what the server said: %v", err)
	}

	// And once they are back, the same tokens work.
	exchange.server.Unavailable(false)
	exchange.open(t, provider.RoleInteractive)
	gmail.Unavailable(false)
	sess, err := gmailMailbox(t, gmail, "ana@gmail.com", src).Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("after the outage: %v", err)
	}
	_ = sess.Close()
}

func TestARevokedGrantIsReportedByTheTokenSourceNotTheServer(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{}, "one", "two")
	h.server.RejectToken("one")
	h.tokens.FailRefreshWith(providertest.ErrGrantRevoked)

	_, err := h.mailbox.Open(t.Context(), provider.RoleSync)
	if !errors.Is(err, providertest.ErrGrantRevoked) {
		t.Fatalf("the token source's classification should reach the caller, got %v", err)
	}
}

func TestListingFoldersReturnsThemAsTheServerNamesThem(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.CreateMailbox(t, "Itens Enviados")
	h.server.CreateMailbox(t, "Projeto Fênix")
	sess := h.open(t, provider.RoleSync)

	folders, err := sess.ListFolders(t.Context(), false)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	names := map[string]bool{}
	for _, f := range folders {
		names[f.Name] = true
		if !f.Selectable {
			t.Errorf("%q came back unselectable", f.Name)
		}
	}
	for _, want := range []string{"INBOX", "Itens Enviados", "Projeto Fênix"} {
		if !names[want] {
			t.Errorf("%q missing from %v", want, names)
		}
	}
}

func TestSelectReportsTheFolderState(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleSync)

	status, err := sess.Select(t.Context(), "INBOX", true, 0)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if status.UIDValidity == 0 {
		t.Error("UIDVALIDITY was not reported")
	}
	if status.NumMessages != 1 {
		t.Errorf("NumMessages = %d, want 1", status.NumMessages)
	}
	if got := sess.Selected(); got == nil || got.Name != "INBOX" {
		t.Errorf("Selected() = %+v", got)
	}
}

func TestAChangedUIDValidityIsReportedWithTheNewStateAttached(t *testing.T) {
	// Exchange bumps UIDVALIDITY with nobody asking. The caller has to resync,
	// and it should not need a second round trip to learn what to resync to.
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleSync)

	status, err := sess.Select(t.Context(), "INBOX", true, 999999)
	if !errors.Is(err, provider.ErrUIDValidityChanged) {
		t.Fatalf("want ErrUIDValidityChanged, got %v", err)
	}
	if status.UIDValidity == 0 || status.UIDValidity == 999999 {
		t.Fatalf("the new state should come back with the error, got %+v", status)
	}
}

func TestFetchSummariesReadsEnvelopeFlagsAndStructureInOneCommand(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, []goimap.Flag{goimap.FlagSeen}, time.Unix(1_700_000_000, 0))
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	var got []provider.Summary
	err := sess.FetchSummaries(t.Context(), all(), 0, func(s provider.Summary) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	s := got[0]
	if s.Envelope == nil || s.Envelope.Subject != "Fatura de setembro" {
		t.Errorf("envelope = %+v", s.Envelope)
	}
	// The envelope's message id arrives without angle brackets, which is what
	// the index stores.
	if s.Envelope.MessageID != "fatura-09@example.com" {
		t.Errorf("MessageID = %q, want it bare", s.Envelope.MessageID)
	}
	if s.Size == 0 {
		t.Error("RFC822.SIZE was not reported")
	}
	if s.InternalDate.IsZero() {
		t.Error("INTERNALDATE was not reported")
	}
	if len(s.Parts) == 0 {
		t.Fatal("BODYSTRUCTURE was not flattened; attachments could not be listed without downloading")
	}
	if !containsFlag(s.Flags, "\\seen") {
		t.Errorf("flags = %v, want the seen flag lowercased for comparison", s.Flags)
	}
}

func TestTheReferencesHeaderIsReadWithTheSummaryWithoutMarkingAnythingRead(t *testing.T) {
	// ENVELOPE carries In-Reply-To but not References, and threading needs
	// both. The header is read in the same FETCH, with PEEK.
	h := newHarness(t, providertest.IMAPOptions{})
	reply := strings.Replace(sampleMessage, "Message-ID: <fatura-09@example.com>\r\n",
		"Message-ID: <reply@example.com>\r\nIn-Reply-To: <fatura-09@example.com>\r\n"+
			"References: <root@example.com>\r\n <fatura-09@example.com>\r\n", 1)
	h.server.Append(t, "INBOX", reply, nil, time.Unix(1_700_000_000, 0))
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Unix(1_700_000_100, 0))
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	var got []provider.Summary
	if err := sess.FetchSummaries(t.Context(), all(), 0, func(s provider.Summary) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d summaries", len(got))
	}
	if refs := got[0].References; len(refs) != 2 || refs[0] != "root@example.com" || refs[1] != "fatura-09@example.com" {
		t.Errorf("References = %q, want both ids, bare, oldest first", refs)
	}
	if got[1].References != nil {
		t.Errorf("a message without References reported %q", got[1].References)
	}
	for _, s := range got {
		if containsFlag(s.Flags, "\\seen") {
			t.Errorf("UID %d was marked read by a summary fetch", s.UID)
		}
	}
	flags, err := sess.FetchFlags(t.Context(), all(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		if containsFlag(f.Flags, "\\seen") {
			t.Errorf("UID %d is read after the summary fetch", f.UID)
		}
	}
}

func TestFlagsAreLowercasedSoAServersCasingDoesNotLookLikeAChange(t *testing.T) {
	// The RFC says flags are case-insensitive and servers disagree in
	// practice. Comparing raw strings would make every pass think the flags
	// moved.
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, []goimap.Flag{goimap.FlagFlagged}, time.Time{})
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	updates, err := sess.FetchFlags(t.Context(), all(), 0)
	if err != nil {
		t.Fatalf("FetchFlags: %v", err)
	}
	for _, u := range updates {
		for _, f := range u.Flags {
			if strings.ToLower(string(f)) != string(f) {
				t.Errorf("flag %q was not normalised", f)
			}
		}
	}
}

func TestFetchSummariesRefusesChangedSinceWithoutCondstore(t *testing.T) {
	// Sending CHANGEDSINCE to a server without CONDSTORE is a syntax error
	// that kills the connection; refusing locally is the difference between a
	// clear error and a dead account.
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	err := sess.FetchSummaries(t.Context(), all(), 42, func(provider.Summary) error { return nil })
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestSearchingByUIDRangeFindsTheNewMessages(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	for i := 0; i < 3; i++ {
		h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	}
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	uids, err := sess.UIDs(t.Context(), goimap.UIDSet{{Start: 2, Stop: 0}}, time.Time{})
	if err != nil {
		t.Fatalf("UIDs: %v", err)
	}
	if len(uids) != 2 {
		t.Fatalf("got %d uids from 2:*, want 2", len(uids))
	}
}

func TestCountingWithoutListingNeedsESearch(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	// Exchange answers BAD to RETURN, so the capability is checked rather
	// than attempted.
	if _, err := sess.UIDCount(t.Context(), all()); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported without ESEARCH, got %v", err)
	}

	rich := newHarness(t, providertest.IMAPOptions{Caps: providertest.RichCaps()})
	rich.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	richSess := rich.open(t, provider.RoleSync)
	if _, err := richSess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	n, err := richSess.UIDCount(t.Context(), all())
	if err != nil {
		t.Fatalf("UIDCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestStoringFlagsReturnsWhatTheServerEchoed(t *testing.T) {
	// The index is updated from the server's answer, never from what we hoped
	// would happen: a flag the server refused must not appear as set.
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	updates, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd, []goimap.Flag{goimap.FlagSeen}, 0)
	if err != nil {
		t.Fatalf("StoreFlags: %v", err)
	}
	if len(updates) != 1 || !containsFlag(updates[0].Flags, "\\seen") {
		t.Fatalf("the server's echo did not come back: %+v", updates)
	}
}

func TestAWriteReSelectsAFolderOpenedReadOnly(t *testing.T) {
	// Gmail answers a STORE against an EXAMINEd mailbox with NO [READ-ONLY],
	// which is a confusing way to discover this.
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd, []goimap.Flag{goimap.FlagFlagged}, 0); err != nil {
		t.Fatalf("StoreFlags after a read-only select: %v", err)
	}
}

func TestMovingAMessageReportsWhereItLanded(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.CreateMailbox(t, "Archive")
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	result, err := sess.Move(t.Context(), goimap.UIDSetNum(1), "Archive")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(result.Mapping) != 1 {
		t.Fatalf("COPYUID mapping = %v, want one entry so the index can follow the message", result.Mapping)
	}
}

func TestFetchingAPartSpoolsItAndReleasesTheConnection(t *testing.T) {
	// A reader still bound to the connection would hold an IMAP session — and
	// every sync pass for the account — for as long as the slowest client
	// took to read a large attachment.
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	part, err := sess.FetchPart(t.Context(), 1, provider.PartInfo{Path: []int{1}, MIMEType: "text/plain"}, 1<<20)
	if err != nil {
		t.Fatalf("FetchPart: %v", err)
	}
	defer func() { _ = part.Body.Close() }()

	// The connection is usable again immediately, before the body is read.
	if err := sess.Noop(t.Context()); err != nil {
		t.Fatalf("the session was still busy after FetchPart returned: %v", err)
	}

	body, err := io.ReadAll(part.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "fatura em anexo") {
		t.Fatalf("body = %q", body)
	}
}

func TestAnOversizedPartIsRefusedRatherThanBuffered(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	_, err := sess.FetchPart(t.Context(), 1, provider.PartInfo{Path: []int{1}}, 8)
	if !errors.Is(err, provider.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestClosingAPartRemovesItsSpoolFile(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleInteractive)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	part, err := sess.FetchRaw(t.Context(), 1, 1<<20)
	if err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if err := part.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reading after Close must fail: otherwise the file is still on disk.
	if _, err := part.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("the spool file outlived its reader")
	}
}

func TestAppendingAMessageReportsItsNewUID(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.CreateMailbox(t, "Sent")
	sess := h.open(t, provider.RoleInteractive)

	result, err := sess.Append(t.Context(), "Sent", strings.NewReader(sampleMessage),
		int64(len(sampleMessage)), []goimap.Flag{goimap.FlagSeen}, time.Now())
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result.UID == 0 {
		t.Error("APPENDUID was not reported, so the sent copy cannot be located")
	}
}

func TestOnlyTheIdleSessionMayIdle(t *testing.T) {
	// IDLE occupies the connection completely. Letting the sync session do it
	// would stall every pass behind it.
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleSync)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Idle(t.Context()); err == nil {
		t.Fatal("the sync session should refuse to idle")
	}
}

func TestIdleDeliversNewMailAsASignal(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleIdle)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	handle, err := sess.Idle(t.Context())
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	defer func() { _ = handle.Stop() }()

	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})

	select {
	case ev := <-sess.Events():
		if ev.Kind != provider.IdleExists {
			t.Fatalf("event kind = %v, want an EXISTS signal", ev.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal arrived while idling")
	}
}

func TestMailThatArrivesBetweenSelectAndIdleIsStillSignalled(t *testing.T) {
	// The idle loop selects the inbox and then idles, and mail can land in
	// between. The server owes the client that change once IDLE starts, and it
	// must become a signal like any other: nothing may discard what was queued
	// before Idle. This is also the state a late listener on the server side
	// leaves behind, made certain instead of left to the scheduler.
	h := newHarness(t, providertest.IMAPOptions{})
	sess := h.open(t, provider.RoleIdle)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})

	handle, err := sess.Idle(t.Context())
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	defer func() { _ = handle.Stop() }()

	select {
	case ev := <-sess.Events():
		if ev.Kind != provider.IdleExists {
			t.Fatalf("event kind = %v, want an EXISTS signal", ev.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mail that arrived before IDLE was never signalled")
	}
}

func TestAnIdleSignalCarriesNoDataToApply(t *testing.T) {
	// The sequence number in an EXPUNGE is meaningless without a local shadow
	// of the mailbox's ordering, which is exactly the thing not to keep. The
	// signal means "run the UID diff" and nothing else.
	h := newHarness(t, providertest.IMAPOptions{})
	h.server.Append(t, "INBOX", sampleMessage, nil, time.Time{})
	sess := h.open(t, provider.RoleIdle)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	handle, err := sess.Idle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Stop() }()

	// A second connection removes the message.
	worker := h.open(t, provider.RoleInteractive)
	if _, err := worker.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd, []goimap.Flag{goimap.FlagDeleted}, 0); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sess.Events():
			// Whatever arrives, it is a signal with no message content.
			if ev.Kind == provider.IdleFetch || ev.Kind == provider.IdleExpunge || ev.Kind == provider.IdleExists {
				return
			}
		case <-deadline:
			t.Fatal("no signal arrived")
		}
	}
}

func TestAHungServerDoesNotHoldAnAccountForever(t *testing.T) {
	// go-imap has no timeout knobs at all, so without the operation timer a
	// server that stops answering is an account that never syncs again.
	listener, addr := listenAndHang(t)
	defer func() { _ = listener.Close() }()

	mb, err := imapprovider.New(provider.Config{
		Kind:              provider.KindIMAP,
		IMAPAddr:          addr,
		Credentials:       provider.Credentials{User: "someone@example.com", Password: "x"},
		SpoolDir:          t.TempDir(),
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := mb.Open(ctx, provider.RoleSync)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connecting to a server that never speaks should fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the attempt hung: the operation deadline did not fire")
	}
}

func TestClosingASessionTwiceIsHarmless(t *testing.T) {
	h := newHarness(t, providertest.IMAPOptions{})
	sess, err := h.mailbox.Open(t.Context(), provider.RoleSync)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestAMailboxNeedsSomeWayToAuthenticate(t *testing.T) {
	_, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: "127.0.0.1:1", Credentials: provider.Credentials{User: "x"},
	})
	if err == nil {
		t.Fatal("a mailbox with neither a password nor a token source should not be constructible")
	}
}

// --- helpers ---------------------------------------------------------------

// listenAndHang accepts connections and never sends the IMAP greeting, which
// is how a wedged middlebox or an overloaded server behaves.
func listenAndHang(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	return ln, ln.Addr().String()
}

func all() goimap.UIDSet { return goimap.UIDSet{{Start: 1, Stop: 0}} }

func containsFlag(flags []goimap.Flag, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(string(f), want) {
			return true
		}
	}
	return false
}
