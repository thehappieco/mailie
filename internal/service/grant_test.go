package service_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// A grant is only worth calling connected once the mail server takes it. These
// are the ways it can fail to — at consent, and later, in use — and what each
// leaves behind: the account's state, what is stored, and the warning an
// operator reads.

// onServer is an account request whose IMAP connections go to srv, whatever
// the provider.
func onServer(t *testing.T, srv *providertest.IMAPServer, req service.AddAccountRequest) service.AddAccountRequest {
	t.Helper()
	host, port := splitHostPort(t, srv.Addr)
	req.IMAPHost, req.IMAPPort, req.SMTPHost, req.SMTPPort = host, port, host, port
	return req
}

// credentialsOf counts what is stored to authenticate an account with.
func (f *fixture) credentialsOf(t *testing.T, id string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, id)
}

// warned is the fixture's log, checked for what a warning must carry and
// must never carry.
func (f *fixture) warned(t *testing.T, carries []string, never ...string) {
	t.Helper()
	logs := f.logs.String()
	for _, want := range carries {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %q:\n%s", want, logs)
		}
	}
	for _, secret := range never {
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries %q:\n%s", secret, logs)
		}
	}
}

// issuedSecrets are what a consent in these tests handles that must never
// reach a log: the tokens the fake identity provider hands out, the web
// clients' secrets, and the authorization codes the tests redeem.
var issuedSecrets = []string{
	"access-1", "access-2", "access-3", "access-4", "refresh-1", "refresh-2", "refresh-3", "refresh-4",
	"google-web-secret", "ms-web-secret", webCode, pastedCode,
}

// Authorization codes distinctive enough to find in a log.
const (
	webCode    = "code-web-4f1d9a"
	pastedCode = "code-pasted-8c2e71"
)

func TestAGrantWithoutTheMailScopeIsRefusedAndStoresNothing(t *testing.T) {
	// A token response that says the grant lacks the mailbox. Google shows
	// the screen where a scope can be unticked only to apps that ask for more
	// than one, and this one asks for one; but the response is what says
	// what was granted, and a grant without the mailbox refreshes like any
	// other and never opens it.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	idp.set(func(f *fakeIDP) {
		f.onIssue = srv.AcceptToken
		f.scope = "openid https://www.googleapis.com/auth/userinfo.email"
	})
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	added, err := f.svc.AddAccount(t.Context(), ana, onServer(t, srv, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth))
	if service.CodeOf(err) != service.CodeBadRequest ||
		!strings.Contains(service.MessageOf(err), "did not grant access to the mailbox") {
		t.Fatalf("completing a grant without the mail scope: %s %q", service.CodeOf(err), service.MessageOf(err))
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonScopeMissing {
		t.Errorf("account = %s %q, want error %q", a.State, a.StateReason, account.ReasonScopeMissing)
	}
	if n := f.credentialsOf(t, added.Account.ID); n != 0 {
		t.Errorf("%d credentials stored for a grant without the mail scope", n)
	}
	if n := len(srv.AuthAttempts()); n != 0 {
		t.Errorf("the mail server was tried %d times with a grant already known not to reach it", n)
	}
	f.warned(t, []string{"authorization failed", "lacks https://mail.google.com/"}, issuedSecrets...)

	// The same person, starting again and granting the mailbox.
	idp.set(func(f *fakeIDP) { f.scope = "https://mail.google.com/" })
	flow, err := f.svc.StartOAuth(t.Context(), ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.CompleteOAuth(t.Context(), ana, "http://localhost:5174/oauth/return?code=whole&state="+stateOf(t, flow))
	if err != nil || a.State != "active" {
		t.Fatalf("the whole grant: %+v %v", a, err)
	}
	if n := f.credentialsOf(t, added.Account.ID); n != 1 {
		t.Errorf("%d credentials stored for a working grant, want one", n)
	}
}

func TestAGrantForAnotherAccountEndsInErrorAndStoresNoToken(t *testing.T) {
	// The other likely shape of the incident: the person typed one address
	// here and picked another account on the provider's page. The grant is
	// real, the scope is right, and the mail server refuses to open the
	// typed address with it. Whatever the flow, the account says so and
	// nothing is kept.
	f, idp := consentFixture(t, "http://localhost:5174")
	gmail := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	exchange := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@contoso.example"})
	idp.set(func(f *fakeIDP) {
		f.onIssue = func(token string) {
			gmail.AcceptToken(token)
			exchange.AcceptToken(token)
		}
	})
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	// Through the console: Gmail explains its refusal, and the explanation
	// is what the operator reads.
	web, err := f.svc.AddAccount(t.Context(), ana, onServer(t, gmail, service.AddAccountRequest{Email: "ana.work@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), ana, "http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, web.Auth))
	if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "sign in as the address") {
		t.Fatalf("web: %s %q", service.CodeOf(err), service.MessageOf(err))
	}
	f.warned(t, []string{
		"the mail server refused a new authorization", "class=needs_reauth",
		"status 400, scope " + providertest.GmailScope,
		`server="imap: NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)"`,
	}, append([]string{"ana.work@gmail.com"}, issuedSecrets...)...)

	// Pasted, from the CLI, on a server that refuses as Exchange does: the
	// check refreshes once before believing it.
	before := idp.refreshCount()
	pasted, err := f.svc.AddAccount(t.Context(), admin(),
		onServer(t, exchange, service.AddAccountRequest{Email: "cli.work@gmail.com", Flow: "pasted"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), admin(), "http://127.0.0.1:1/cb?code="+pastedCode+"&state="+stateOf(t, pasted.Auth))
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("pasted: %v", err)
	}

	// Loopback: the page the browser lands on says it failed.
	loop, err := f.svc.AddAccount(t.Context(), admin(),
		onServer(t, gmail, service.AddAccountRequest{Email: "loop.work@gmail.com", Flow: "loopback"}))
	if err != nil {
		t.Fatal(err)
	}
	page := getRedirect(t, loop.Auth, url.Values{"code": {"c"}, "state": {stateOf(t, loop.Auth)}})
	if page.status != http.StatusBadRequest || !strings.Contains(page.body, "Authorization failed") {
		t.Errorf("loopback page = %d %q", page.status, page.body)
	}

	// Device: nobody is waiting, and the account is all that records it.
	device, err := f.svc.AddAccount(t.Context(), admin(),
		onServer(t, exchange, service.AddAccountRequest{Email: "ana.work@contoso.example", Provider: "microsoft", Flow: "device"}))
	if err != nil {
		t.Fatal(err)
	}
	idp.set(func(f *fakeIDP) { f.deviceAnswer = "" })
	eventually(t, "the device flow's refused grant to be recorded", func() bool {
		return f.stateOfAccount(t, device.Account.ID).State == account.StateError
	})

	for name, id := range map[string]string{
		"web": web.Account.ID, "pasted": pasted.Account.ID, "loopback": loop.Account.ID, "device": device.Account.ID,
	} {
		if a := f.stateOfAccount(t, id); a.State != account.StateError || a.StateReason != account.ReasonMailboxRefused {
			t.Errorf("%s: account = %s %q, want error %q", name, a.State, a.StateReason, account.ReasonMailboxRefused)
		}
		if n := f.credentialsOf(t, id); n != 0 {
			t.Errorf("%s: %d credentials stored for a grant the mail server refused", name, n)
		}
	}
	// Gmail's 400 is not worth a refresh; Exchange's bare refusal is, once
	// per check: the pasted flow's and the device flow's.
	if n := idp.refreshCount() - before; n != 2 {
		t.Errorf("%d refreshes while checking, want one for each Exchange-shaped refusal", n)
	}
	f.warned(t, []string{"freshly refreshed", `server="imap: NO AUTHENTICATE failed."`}, issuedSecrets...)
}

func TestAMailServerThatIsDownAtConsentKeepsTheGrant(t *testing.T) {
	// A mail server's bad minute is not the grant's fault. Refusing the
	// consent over it would send the person back through the provider's
	// screens for nothing.
	f, _ := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitHostPort(t, closed.Addr().String())
	_ = closed.Close()

	added, err := f.svc.AddAccount(t.Context(), ana, service.AddAccountRequest{
		Email: "ana@gmail.com", IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.CompleteOAuth(t.Context(), ana, "http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth))
	if err != nil {
		t.Fatalf("a mail server that could not be reached failed the consent: %v", err)
	}
	if a.State != "active" {
		t.Errorf("state = %q, want active", a.State)
	}
	if n := f.credentialsOf(t, added.Account.ID); n != 1 {
		t.Errorf("%d credentials stored, want the grant kept", n)
	}
	f.warned(t, []string{"could not prove a new authorization opens the mailbox; keeping it", "class=connection_closed"},
		issuedSecrets...)
}

func TestACallerThatStopsWaitingChangesNothingAboutTheCheck(t *testing.T) {
	// The code is spent the moment it is exchanged. A console tab closed
	// right after must neither cost the grant nor skip the login that decides
	// whether it is kept.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	for _, c := range []struct {
		email  string
		state  account.State
		reason string
		stored int
	}{
		{"ana@gmail.com", account.StateActive, "", 1},
		// Someone else's token: refused, even with nobody left to tell.
		{"ana.work@gmail.com", account.StateError, account.ReasonMailboxRefused, 0},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		idp.set(func(f *fakeIDP) {
			f.onIssue = func(token string) {
				srv.AcceptToken(token)
				cancel() // the caller gives up while the token is being issued
			}
		})
		added, err := f.svc.AddAccount(t.Context(), ana, onServer(t, srv, service.AddAccountRequest{Email: c.email}))
		if err != nil {
			t.Fatal(err)
		}
		tried := len(srv.AuthAttempts())
		_, _ = f.svc.CompleteOAuth(ctx, ana, "http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth))
		cancel()

		if n := len(srv.AuthAttempts()) - tried; n != 1 {
			t.Errorf("%s: the mail server was tried %d times after the caller left, want once", c.email, n)
		}
		if a := f.stateOfAccount(t, added.Account.ID); a.State != c.state || a.StateReason != c.reason {
			t.Errorf("%s: account = %s %q, want %s %q", c.email, a.State, a.StateReason, c.state, c.reason)
		}
		if n := f.credentialsOf(t, added.Account.ID); n != c.stored {
			t.Errorf("%s: %d credentials stored, want %d", c.email, n, c.stored)
		}
	}
}

func TestARefusalOfAGrantAlreadyReplacedLeavesTheNewOneAlone(t *testing.T) {
	// A connection still opening with the old grant when the person consents
	// again: its refusal arrives after the new grant is stored, and is about a
	// grant the account no longer has. It must neither stop the account nor
	// write the old grant's refreshed token over the new one.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	idp.set(func(f *fakeIDP) { f.onIssue = srv.AcceptToken })
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	added, err := f.svc.AddAccount(t.Context(), ana, onServer(t, srv, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth)); err != nil {
		t.Fatal(err)
	}
	old, err := f.registry.Mailbox(t.Context(), added.Account.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The new consent, stored while the old mailbox is still in someone's
	// hands.
	flow, err := f.svc.StartOAuth(t.Context(), ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code=again&state="+stateOf(t, flow)); err != nil {
		t.Fatal(err)
	}

	// Now the old grant is refused, a refreshed token included.
	idp.set(func(f *fakeIDP) { f.onIssue = nil })
	srv.RejectToken("access-1")
	if _, err := old.Open(t.Context(), provider.RoleInteractive); !errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("the old grant: want ErrNeedsReauth, got %v", err)
	}

	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateActive || a.StateReason != "" {
		t.Errorf("a refusal of the replaced grant moved the account to %s %q", a.State, a.StateReason)
	}
	if _, err := f.svc.ListFolders(t.Context(), ana, added.Account.ID); err != nil {
		t.Errorf("the new grant no longer works: %v", err)
	}
	f.warned(t, []string{"the mail server refused an authorization the account no longer uses"}, issuedSecrets...)
}

func TestAMailServerThatSaysItIsUnavailableIsNotARefusal(t *testing.T) {
	// Exchange answers AUTHENTICATE with "Server Unavailable" when a mailbox
	// backend is down. Read as a refusal, twice, that would throw a new
	// consent away and park a working account; it is neither.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@contoso.example"})
	idp.set(func(f *fakeIDP) { f.onIssue = srv.AcceptToken })
	srv.Unavailable(true)

	added, err := f.svc.AddAccount(t.Context(), admin(),
		onServer(t, srv, service.AddAccountRequest{Email: "ana@contoso.example", Provider: "microsoft", Flow: "pasted"}))
	if err != nil {
		t.Fatal(err)
	}
	before := idp.refreshCount()
	a, err := f.svc.CompleteOAuth(t.Context(), admin(), "http://127.0.0.1:1/cb?code="+pastedCode+"&state="+stateOf(t, added.Auth))
	if err != nil || a.State != "active" {
		t.Fatalf("a server that is down at consent: %+v %v", a, err)
	}
	if n := f.credentialsOf(t, added.Account.ID); n != 1 {
		t.Errorf("%d credentials stored, want the grant kept", n)
	}
	if n := idp.refreshCount() - before; n != 0 {
		t.Errorf("%d refreshes spent on a server that said it was unavailable", n)
	}
	f.warned(t, []string{
		"could not prove a new authorization opens the mailbox; keeping it", "class=temporary",
		`server="imap: NO Server Unavailable. 15"`,
	}, issuedSecrets...)

	// In use, the same answer leaves the account as it was.
	if _, err := f.svc.ListFolders(t.Context(), admin(), added.Account.ID); err == nil {
		t.Fatal("listing on a server that is down succeeded")
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateActive {
		t.Errorf("a server that is down moved the account to %s %q", a.State, a.StateReason)
	}
	srv.Unavailable(false)
	if _, err := f.svc.ListFolders(t.Context(), admin(), added.Account.ID); err != nil {
		t.Errorf("once the server is back: %v", err)
	}
}

func TestARefusedRefreshedTokenInUseMovesTheAccountToNeedsReauth(t *testing.T) {
	// The grant worked at consent; later the mail server stops taking what
	// it mints, a freshly refreshed token included. Before, the account
	// stayed active and every listing answered a bare "needs
	// re-authorization" with nothing in the log and nothing in the console
	// to fix it.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	idp.set(func(f *fakeIDP) { f.onIssue = srv.AcceptToken })
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	added, err := f.svc.AddAccount(t.Context(), ana, onServer(t, srv, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code="+webCode+"&state="+stateOf(t, added.Auth)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ListFolders(t.Context(), ana, added.Account.ID); err != nil {
		t.Fatalf("the working grant: %v", err)
	}

	// Revoked at the mail server, not at the identity provider: tokens still
	// refresh, and the server takes none of them.
	idp.set(func(f *fakeIDP) { f.onIssue = nil })
	srv.RejectToken("access-1")
	before := idp.refreshCount()

	_, err = f.svc.ListFolders(t.Context(), ana, added.Account.ID)
	if service.CodeOf(err) != service.CodeConflict || service.MessageOf(err) != service.ErrNeedsReauth.Message {
		t.Fatalf("listing with a refused grant: %s %q", service.CodeOf(err), service.MessageOf(err))
	}
	// The cause survives the mapping, and the mapping is still recognisable.
	if !errors.Is(err, service.ErrNeedsReauth) || !errors.Is(err, provider.ErrNeedsReauth) {
		t.Errorf("errors.Is lost the service error or its cause: %v", err)
	}
	if !strings.Contains(err.Error(), "status 401, scope "+providertest.GmailScope) {
		t.Errorf("the error lost what the mail server said: %v", err)
	}
	if n := idp.refreshCount() - before; n != 1 {
		t.Errorf("%d refreshes before giving up, want one", n)
	}

	a := f.stateOfAccount(t, added.Account.ID)
	if a.State != account.StateNeedsReauth || a.StateReason != account.ReasonTokenRejected {
		t.Fatalf("account = %s %q, want needs_reauth %q", a.State, a.StateReason, account.ReasonTokenRejected)
	}
	f.warned(t, []string{
		"opening the mailbox to list its folders failed", "class=needs_reauth", "freshly refreshed",
		"the mail server refused the account's authorization; it needs consent again",
	}, append([]string{"ana@gmail.com"}, issuedSecrets...)...)

	// From now on the account answers from its state, and still says why.
	_, err = f.svc.ListFolders(t.Context(), ana, added.Account.ID)
	if !errors.Is(err, service.ErrNeedsReauth) || !strings.Contains(err.Error(), account.ReasonTokenRejected) {
		t.Errorf("a listing after the state changed: %v", err)
	}
	// Which is what the console's "Finish authorization" starts from.
	if _, err := f.svc.StartOAuth(t.Context(), ana, added.Account.ID, ""); err != nil {
		t.Fatal(err)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StatePendingAuth {
		t.Errorf("after starting again the account is %s, want pending_auth", a.State)
	}
}

func TestTheGrantCheckRunsInTheAccountsInteractiveSlot(t *testing.T) {
	// The daemon hands the registry the sync engine's interactive slot, so
	// re-authorising an account whose worker is running never opens a fourth
	// connection beside it. The check has to run inside that slot, for the
	// account it is checking — every time a consent finishes.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com", GmailRefusals: true})
	idp.set(func(f *fakeIDP) { f.onIssue = srv.AcceptToken })
	var (
		mu      sync.Mutex
		slotted []string
		inside  []int
	)
	f.registry.CheckGrantsIn(func(ctx context.Context, id string, check func(context.Context) error) error {
		before := len(srv.AuthAttempts())
		err := check(ctx)
		mu.Lock()
		defer mu.Unlock()
		slotted = append(slotted, id)
		inside = append(inside, len(srv.AuthAttempts())-before)
		return err
	})
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	added, err := f.svc.AddAccount(t.Context(), ana, onServer(t, srv, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	for i, code := range []string{webCode, "code-reconnect"} {
		state := stateOf(t, added.Auth)
		if i > 0 {
			// Reconnecting the account, now active, from the console.
			flow, err := f.svc.StartOAuth(t.Context(), ana, added.Account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			state = stateOf(t, flow)
		}
		a, err := f.svc.CompleteOAuth(t.Context(), ana, "http://localhost:5174/oauth/return?code="+code+"&state="+state)
		if err != nil || a.State != "active" {
			t.Fatalf("consent %d: %+v %v", i, a, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slotted) != 2 || slotted[0] != added.Account.ID || slotted[1] != added.Account.ID {
		t.Fatalf("the check ran in the slot for %v, want the account twice", slotted)
	}
	for i, n := range inside {
		if n != 1 {
			t.Errorf("consent %d logged in %d times inside the slot, want once", i, n)
		}
	}
}
