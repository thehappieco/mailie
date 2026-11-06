package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A link at the provider (links.go): a Gmail or Microsoft mailbox whose
// address the provider names once the person chose the account on its page,
// created with its key and its linker's grant only once one login as that
// address worked.

const (
	// linkConsole is where the console of these daemons is served.
	linkConsole = "http://localhost:5174"
	// googleSub is the Google account's identifier an ID token carries,
	// which nothing keeps.
	googleSub = "110248495921238986420"
)

// linkWorld is a daemon whose web clients talk to one fake identity provider
// and whose Gmail and Microsoft mailboxes log in to one in-process IMAP
// server, with Ana signed in.
type linkWorld struct {
	*fixture
	idp  *fakeIDP
	imap *providertest.IMAPServer
	ana  service.Principal
}

// newLinkWorld is a daemon with links at the provider, whose mail server
// takes any address with a token the provider issued.
func newLinkWorld(t *testing.T) *linkWorld {
	t.Helper()
	return newLinkWorldWith(t, providertest.IMAPOptions{AnyUser: true}, fixtureOptions{})
}

// newLinkWorldWith is newLinkWorld with the mail server and the daemon's
// options a test names.
func newLinkWorldWith(t *testing.T, server providertest.IMAPOptions, o fixtureOptions) *linkWorld {
	t.Helper()
	idp := newFakeIDP(t)
	srv := providertest.NewIMAPServer(t, server)
	idp.accept = srv.AcceptToken
	o.publicURL = linkConsole
	o.providerIMAP = srv.Addr
	o.registry = linkRegistry(idp)
	w := &linkWorld{fixture: newFixtureWith(t, o), idp: idp, imap: srv}
	w.ana = w.person(t, "ana@example.test", auth.RoleMember)
	return w
}

// linkRegistry is a daemon's OAuth clients, every one talking to idp.
func linkRegistry(idp *fakeIDP) account.RegistryOptions {
	return account.RegistryOptions{
		Google:       account.OAuthClient{ClientID: "google-installed", Endpoint: idp.endpoint()},
		Microsoft:    account.OAuthClient{ClientID: "ms-installed", Tenant: "common", Endpoint: idp.endpoint()},
		GoogleWeb:    account.OAuthClient{ClientID: "google-web", ClientSecret: "google-web-secret", Endpoint: idp.endpoint()},
		MicrosoftWeb: account.OAuthClient{ClientID: "ms-web", ClientSecret: "ms-web-secret", Endpoint: idp.endpoint()},
	}
}

// googleNames makes the provider's next answer name email, as Google's ID
// token would for the account chosen on its page.
func (w *linkWorld) googleNames(email string) {
	now := time.Now()
	claims := map[string]any{
		"iss": "https://accounts.google.com", "aud": "google-web", "azp": "google-web", "sub": googleSub,
		"email_verified": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if email != "" {
		claims["email"] = email
	}
	w.idp.set(func(f *fakeIDP) { f.claims = claims })
}

// claim changes one claim of the provider's next ID token.
func (w *linkWorld) claim(name string, value any) {
	w.idp.set(func(f *fakeIDP) {
		claims := maps.Clone(f.claims)
		claims[name] = value
		f.claims = claims
	})
}

// start starts a link as p, failing the test on a refusal.
func (w *linkWorld) start(t *testing.T, p service.Principal, req service.LinkRequest) service.LinkStarted {
	t.Helper()
	if req.Provider == "" {
		req.Provider = "gmail"
	}
	started, err := w.svc.StartLink(t.Context(), p, req)
	if err != nil {
		t.Fatalf("starting a link: %v", err)
	}
	return started
}

// withKey is a link's return with a code, carrying a fresh mailbox key as a
// person's browser makes and seals it then.
func withKey(state string) service.CompleteOAuthRequest {
	return service.CompleteOAuthRequest{
		RedirectURL: linkConsole + "/oauth/return?code=the-code&state=" + url.QueryEscape(state),
		PublicKey:   b64(mailboxPublicKey()), Namespace: keyscheme.NewSealID(), Grant: b64(grantAt(1)),
	}
}

// finish posts a link's return with a code and a key, as p.
func (w *linkWorld) finish(t *testing.T, p service.Principal, started service.LinkStarted) (service.Account, error) {
	t.Helper()
	return w.svc.CompleteOAuth(t.Context(), p, withKey(stateOf(t, started.Auth)))
}

// link reads a link as Ana.
func (w *linkWorld) link(t *testing.T, id string) service.Link {
	t.Helper()
	l, err := w.svc.GetLink(t.Context(), w.ana, id)
	if err != nil {
		t.Fatalf("reading the link: %v", err)
	}
	return l
}

// links counts the rows of links at the provider.
func (w *linkWorld) links(t *testing.T) int {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM oauth_links`)
}

// nothingStored fails the test if any mailbox, key, grant, flag or
// credential exists.
func (w *linkWorld) nothingStored(t *testing.T, what string) {
	t.Helper()
	for _, table := range []string{"accounts", "credentials", "mailbox_keys", "mailbox_grants", "mailbox_access"} {
		if n := w.count(t, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s: %d rows in %s", what, n, table)
		}
	}
}

// team makes a team Ana owns, with Bea as its admin, and returns it and Bea.
func (w *linkWorld) team(t *testing.T) (string, service.Principal) {
	t.Helper()
	ws := workspace.NewRepository(w.db, nil)
	bea := w.person(t, "bea@example.test", auth.RoleMember)
	team, err := ws.CreateTeam(t.Context(), "Support", w.ana.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.db.Write(t.Context(), func(tx *sql.Tx) error {
		return ws.AddMemberTx(t.Context(), tx, team.ID, bea.UserID, workspace.RoleAdmin, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	return team.ID, bea
}

func TestAGmailLinkLearnsTheAddressFromGoogleAndStoresTheMailboxOnlyAfterItsLoginWorks(t *testing.T) {
	// The mail server takes one account alone: the one Google names. The
	// mailbox is stored as that address, with Gmail's servers, and only once
	// its login worked.
	w := newLinkWorldWith(t, providertest.IMAPOptions{User: "ana.lima@gmail.com"}, fixtureOptions{})
	started := w.start(t, w.ana, service.LinkRequest{DisplayName: "Ana at Gmail"})
	if n := w.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("a link's start stored %d mailboxes", n)
	}

	w.googleNames("ana.lima@gmail.com")
	created, err := w.finish(t, w.ana, started)
	if err != nil {
		t.Fatalf("the link's return: %v", err)
	}
	if created.Email != "ana.lima@gmail.com" || created.Provider != "gmail" || created.State != "active" ||
		created.DisplayName != "Ana at Gmail" || !created.Connected {
		t.Errorf("the mailbox the return created: %+v", created)
	}
	stored := w.stateOfAccount(t, created.ID)
	if stored.LoginUser != "ana.lima@gmail.com" || stored.IMAPHost != "imap.gmail.com" || stored.OAuthClient != account.ClientWeb ||
		stored.OwnerUserID != w.ana.UserID || stored.LinkedBy != w.ana.UserID {
		t.Errorf("stored as %+v", stored)
	}
	if n := w.credentialsOf(t, created.ID); n != 1 {
		t.Errorf("%d credentials stored, want the grant", n)
	}
	if n := len(w.imap.AuthAttempts()); n != 1 {
		t.Errorf("the mail server saw %d logins, want the one that proved the grant", n)
	}
	if l := w.link(t, started.Link.ID); l.State != "connected" || l.AccountID != created.ID || l.Email != "" {
		t.Errorf("the link once connected: %+v", l)
	}

	// Another account chosen at Google is not the mail server's: nothing.
	other := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.work@gmail.com")
	if _, err := w.finish(t, w.ana, other); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("an account the mail server does not open: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Errorf("%d mailboxes, want only the one whose login worked", n)
	}
	w.warned(t, nil, append([]string{"ana.work@gmail.com", googleSub}, issuedSecrets...)...)
}

func TestALinkStoresNoMailboxNoKeyAndNoAddressBeforeItsReturn(t *testing.T) {
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{DisplayName: "Work"})
	w.nothingStored(t, "a link waiting for its return")
	if n := w.idp.exchangeCount(); n != 0 {
		t.Errorf("a link's start exchanged %d codes", n)
	}
	if n := len(w.imap.AuthAttempts()); n != 0 {
		t.Errorf("a link's start logged in %d times", n)
	}
	l := w.link(t, started.Link.ID)
	if l.State != "waiting" || l.Email != "" || l.AccountID != "" || l.Reason != "" || l.WorkspaceID != "" {
		t.Errorf("a link waiting reads %+v", l)
	}
	if l.ExpiresAt != started.Auth.ExpiresAt || l.ExpiresAt <= time.Now().Unix() {
		t.Errorf("a link waiting expires at %d, its flow at %d", l.ExpiresAt, started.Auth.ExpiresAt)
	}
	// The authorization URL names no account: the provider's chooser does.
	u, err := url.Parse(started.Auth.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Has("login_hint") || u.Query().Get("prompt") != "select_account consent" ||
		!strings.Contains(u.Query().Get("scope"), "openid email") {
		t.Errorf("a link leaves for %s", started.Auth.AuthURL)
	}
}

func TestALinkCarriesItsKeyWithItsReturnAndWritesItWithTheMailbox(t *testing.T) {
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	req := withKey(stateOf(t, started.Auth))
	created, err := w.svc.CompleteOAuth(t.Context(), w.ana, req)
	if err != nil {
		t.Fatal(err)
	}
	var (
		pub, grant []byte
		ns         string
		epoch      int
	)
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT k.public_key, k.namespace, g.epoch, g.grant
		FROM mailbox_keys k JOIN mailbox_grants g ON g.account_id = k.account_id AND g.epoch = k.epoch
		WHERE k.account_id = ? AND g.user_id = ?`, created.ID, w.ana.UserID).Scan(&pub, &ns, &epoch, &grant); err != nil {
		t.Fatalf("the mailbox's key and Ana's grant: %v", err)
	}
	if b64(pub) != req.PublicKey || ns != req.Namespace || epoch != 1 || b64(grant) != req.Grant {
		t.Errorf("the key written is not the one the return carried")
	}
	if created.MailboxKey == nil || created.MailboxKey.PublicKey != req.PublicKey || !created.Access.Read {
		t.Errorf("the mailbox as its linker reads it: %+v", created)
	}
}

func TestALinkReturnWithoutAKeyIsRefusedBeforeItsCodeIsTaken(t *testing.T) {
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	bare := returnTo(linkConsole + "/oauth/return?code=the-code&state=" + stateOf(t, started.Auth))
	if _, err := w.svc.CompleteOAuth(t.Context(), w.ana, bare); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a link's return without a key: %v", err)
	}
	broken := withKey(stateOf(t, started.Auth))
	broken.Grant = b64(grantAt(2))
	if _, err := w.svc.CompleteOAuth(t.Context(), w.ana, broken); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a link's return with a grant at another epoch: %v", err)
	}
	if n := w.idp.exchangeCount(); n != 0 {
		t.Fatalf("a return without its key exchanged %d codes", n)
	}
	if l := w.link(t, started.Link.ID); l.State != "waiting" {
		t.Fatalf("the link after a refused return: %+v", l)
	}
	if _, err := w.finish(t, w.ana, started); err != nil {
		t.Fatalf("the same return with its key: %v", err)
	}
}

func TestALinkToAnAddressAlreadyInTheWorkspaceIsAConflictAndStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	w.googleNames("ana.lima@gmail.com")
	first, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ciphertext FROM credentials WHERE account_id = ?`,
		first.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	logins := len(w.imap.AuthAttempts())

	// The same account, under another spelling of its address.
	again := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("Ana.Lima+news@googlemail.com")
	_, err = w.finish(t, w.ana, again)
	if service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("linking the same account again: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Errorf("%d mailboxes, want one", n)
	}
	if n := len(w.imap.AuthAttempts()); n != logins {
		t.Errorf("the mail server was dialled for a mailbox the workspace holds")
	}
	var after []byte
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ciphertext FROM credentials WHERE account_id = ?`,
		first.ID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Errorf("the mailbox's grant changed: %v", err)
	}
	if l := w.link(t, again.Link.ID); l.State != "failed" || l.Reason != account.ReasonDuplicate ||
		l.Email != "Ana.Lima+news@googlemail.com" {
		t.Errorf("the duplicate link reads %+v", l)
	}
}

func TestALinkToAnAddressRemovedAndKeptIsTheSameConflictForAnAdmin(t *testing.T) {
	// An admin who is not an owner learns nothing of a removed mailbox: a
	// link to its address is answered as one to a live mailbox is.
	w := newLinkWorld(t)
	team, bea := w.team(t)
	w.googleNames("support@example.test")
	removed, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{WorkspaceID: team}))
	if err != nil {
		t.Fatal(err)
	}
	w.googleNames("billing@example.test")
	if _, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{WorkspaceID: team})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RemoveAccount(t.Context(), w.ana, removed.ID, service.RemoveAccountRequest{Confirm: removed.ID}); err != nil {
		t.Fatal(err)
	}

	answers := map[string]error{}
	reasons := map[string]string{}
	for name, email := range map[string]string{"removed": "support@example.test", "live": "billing@example.test"} {
		started := w.start(t, bea, service.LinkRequest{WorkspaceID: team})
		w.googleNames(email)
		_, answers[name] = w.finish(t, bea, started)
		l, err := w.svc.GetLink(t.Context(), bea, started.Link.ID)
		if err != nil {
			t.Fatal(err)
		}
		reasons[name] = l.Reason
	}
	if service.CodeOf(answers["removed"]) != service.CodeConflict ||
		service.MessageOf(answers["removed"]) != service.MessageOf(answers["live"]) ||
		service.CodeOf(answers["live"]) != service.CodeConflict {
		t.Errorf("a removed mailbox answers %v, a live one %v", answers["removed"], answers["live"])
	}
	if reasons["removed"] != account.ReasonDuplicate || reasons["live"] != account.ReasonDuplicate {
		t.Errorf("the links' reasons: %v", reasons)
	}
	if strings.Contains(service.MessageOf(answers["removed"]), "until") {
		t.Errorf("the answer says how long a removed mailbox is kept: %q", service.MessageOf(answers["removed"]))
	}
}

func TestTheSameAccountLinksIntoAnotherWorkspace(t *testing.T) {
	w := newLinkWorld(t)
	team, _ := w.team(t)
	w.googleNames("ana.lima@gmail.com")
	personal, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{WorkspaceID: team}))
	if err != nil {
		t.Fatalf("the same account into a team: %v", err)
	}
	if personal.WorkspaceID == shared.WorkspaceID || shared.WorkspaceID != team || shared.Email != personal.Email {
		t.Errorf("linked as %s in %s and %s in %s", personal.Email, personal.WorkspaceID, shared.Email, shared.WorkspaceID)
	}
}

func TestALinkWhoseGrantTheMailServerRefusesStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	w.idp.set(func(f *fakeIDP) { f.accept = nil })
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	_, err := w.finish(t, w.ana, started)
	if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "refused") {
		t.Fatalf("a grant the mail server refuses: %v", err)
	}
	w.nothingStored(t, "a refused grant")
	if l := w.link(t, started.Link.ID); l.State != "failed" || l.Reason != account.ReasonMailboxRefused ||
		l.Email != "ana.lima@gmail.com" {
		t.Errorf("the refused link reads %+v", l)
	}
	w.warned(t, []string{"the mail server refused a new authorization", "link=" + started.Link.ID},
		append([]string{"ana.lima@gmail.com", googleSub}, issuedSecrets...)...)
}

func TestALinkWithoutTheMailScopeStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	w.idp.set(func(f *fakeIDP) { f.scope = "openid https://www.googleapis.com/auth/userinfo.email" })
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	if _, err := w.finish(t, w.ana, started); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a grant without the mail scope: %v", err)
	}
	w.nothingStored(t, "a grant without the mail scope")
	if n := len(w.imap.AuthAttempts()); n != 0 {
		t.Errorf("a grant known not to reach the mailbox was tried %d times", n)
	}
	if l := w.link(t, started.Link.ID); l.State != "failed" || l.Reason != account.ReasonScopeMissing {
		t.Errorf("the link reads %+v", l)
	}
}

func TestADeclinedLinkIsRecordedWithoutAStepUp(t *testing.T) {
	// Refused at the provider, long enough after the step-up that a return
	// with a code would ask for another: the refusal is recorded without one,
	// and without a key.
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{Provider: "microsoft"})
	w.stale(t, w.ana)
	_, err := w.svc.CompleteOAuth(t.Context(), w.ana, returnTo(linkConsole+"/oauth/return?error=access_denied"+
		"&error_description=AADSTS65001&state="+stateOf(t, started.Auth)))
	if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "declined") {
		t.Fatalf("a declined link: %v", err)
	}
	if n := w.idp.exchangeCount(); n != 0 {
		t.Errorf("a declined link exchanged %d codes", n)
	}
	if l := w.link(t, started.Link.ID); l.State != "failed" || l.Reason != account.ReasonDeclined || l.Email != "" {
		t.Errorf("the declined link reads %+v", l)
	}
	w.nothingStored(t, "a declined link")
}

func TestALinkWithoutAnAddressFromTheProviderStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	for name, change := range map[string]func(){
		"no ID token":         func() { w.idp.set(func(f *fakeIDP) { f.claims = nil }) },
		"no address":          func() { w.googleNames("") },
		"an unverified one":   func() { w.googleNames("ana.lima@gmail.com"); w.claim("email_verified", false) },
		"another app's token": func() { w.googleNames("ana.lima@gmail.com"); w.claim("aud", "another-app") },
	} {
		started := w.start(t, w.ana, service.LinkRequest{})
		change()
		_, err := w.finish(t, w.ana, started)
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "no usable address") {
			t.Errorf("%s: %v", name, err)
		}
		if l := w.link(t, started.Link.ID); l.State != "failed" || l.Reason != account.ReasonNoAddress || l.Email != "" {
			t.Errorf("%s: the link reads %+v", name, l)
		}
	}
	w.nothingStored(t, "links whose provider named no address")
	if n := len(w.imap.AuthAttempts()); n != 0 {
		t.Errorf("the mail server was dialled %d times with no address to log in as", n)
	}
	w.warned(t, nil, append([]string{"ana.lima@gmail.com", googleSub}, issuedSecrets...)...)
}

func TestAUserCannotCompleteAnotherUsersLink(t *testing.T) {
	// A link's page sent to somebody else: whoever holds its return, only
	// its starter redeems it, and a refusal neither exchanges the code nor
	// uses the link up.
	w := newLinkWorld(t)
	bob := w.person(t, "bob@example.test", auth.RoleMember)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	for name, p := range map[string]service.Principal{"another person": bob, "an instance key": admin()} {
		if _, err := w.finish(t, p, started); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("%s posting Ana's return: %v, want not_found", name, err)
		}
		if _, err := w.svc.GetLink(t.Context(), p, started.Link.ID); err == nil {
			t.Errorf("%s read Ana's link", name)
		}
	}
	if n := w.idp.exchangeCount(); n != 0 {
		t.Fatalf("the code was exchanged %d times for callers who did not start the link", n)
	}
	if _, err := w.finish(t, w.ana, started); err != nil {
		t.Fatalf("Ana finishing her link: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM accounts WHERE owner_user_id = ?`, bob.UserID); n != 0 {
		t.Errorf("bob holds %d mailboxes", n)
	}
}

func TestALinkWhoseStepUpLapsedIsRefusedBeforeItsCodeIsExchanged(t *testing.T) {
	// Fresh now, but not once the exchange and the login are over: refused
	// before the code is spent, and the same return works after a step-up.
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.exec(t, `UPDATE sessions SET authenticated_at = ? WHERE id = ?`,
		time.Now().Add(-9*time.Minute-30*time.Second).Unix(), w.ana.SessionID)
	w.googleNames("ana.lima@gmail.com")
	if _, err := w.finish(t, w.ana, started); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("a step-up that lapses before the finish is over: %v", err)
	}
	if n := w.idp.exchangeCount(); n != 0 {
		t.Fatalf("the code was exchanged %d times", n)
	}
	w.stepUp(t, w.ana)
	if _, err := w.finish(t, w.ana, started); err != nil {
		t.Fatalf("the same return after a step-up: %v", err)
	}
}

func TestALinkReturnTheServerWasTooBusyToTakeIsToldToComeAgain(t *testing.T) {
	// The take waits for the writer a few seconds at most (the registry's
	// tests hold it): nothing was taken, so the same return is posted again
	// once the server says, and never read as a refusal.
	err := service.LinkErrorForTest(fmt.Errorf("%w: store: begin: %w", account.ErrLinkBusy, context.DeadlineExceeded))
	var se *service.Error
	if !errors.As(err, &se) || se.Code != service.CodeInternal || se.Retry <= 0 {
		t.Fatalf("a return too busy to take: %#v", err)
	}
	// And a step-up checked before the take lasts through the longest take
	// and the finish after it.
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.exec(t, `UPDATE sessions SET authenticated_at = ? WHERE id = ?`,
		time.Now().Add(-10*time.Minute+account.FinishTimeout+account.LinkTakeTimeout+time.Second).Unix(), w.ana.SessionID)
	w.googleNames("ana.lima@gmail.com")
	if _, err := w.finish(t, w.ana, started); service.CodeOf(err) != service.CodeNotAuthorized || w.idp.exchangeCount() != 0 {
		t.Errorf("a step-up that would lapse during the longest take and finish: %v, exchanged %d times", err,
			w.idp.exchangeCount())
	}
}

func TestALinkWhoseStepUpEndedDuringItsFinishStoresNothing(t *testing.T) {
	// The transaction that writes the key asks for the step-up again, and
	// its answer is the one that counts.
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	w.idp.set(func(f *fakeIDP) {
		f.onExchange = func() {
			if _, err := w.db.Writer().ExecContext(context.Background(),
				`UPDATE sessions SET authenticated_at = 1 WHERE id = ?`, w.ana.SessionID); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := w.finish(t, w.ana, started); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("a step-up that ended during the finish: %v", err)
	}
	w.nothingStored(t, "a link whose step-up ended")
	if l := w.link(t, started.Link.ID); l.State != "failed" {
		t.Errorf("the link reads %+v", l)
	}
}

func TestALinkFinishesWhenItsCallerHangsUpAfterTheCodeWasTaken(t *testing.T) {
	// The code is single-use: a caller that stops waiting once it was taken
	// must not throw the mailbox away halfway through creating it.
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.idp.set(func(f *fakeIDP) { f.onExchange = cancel })
	_, _ = w.svc.CompleteOAuth(ctx, w.ana, withKey(stateOf(t, started.Auth)))
	l := w.link(t, started.Link.ID)
	if l.State != "connected" || l.AccountID == "" {
		t.Fatalf("the link whose caller hung up reads %+v", l)
	}
	if a, err := w.svc.GetAccount(t.Context(), w.ana, l.AccountID); err != nil || a.State != "active" {
		t.Errorf("its mailbox: %+v %v", a, err)
	}
}

func TestALinkEndsWhenItsStarterIsDisabledLeavesOrIsDemotedOrTheTeamCloses(t *testing.T) {
	// Wherever the starter's place goes, their links go with it, in the same
	// transaction: a return finds nothing to finish.
	for name, end := range map[string]func(t *testing.T, w *linkWorld, team string, bea service.Principal) service.Principal{
		"disabled": func(t *testing.T, w *linkWorld, _ string, bea service.Principal) service.Principal {
			if _, err := w.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "bea@example.test"}); err != nil {
				t.Fatal(err)
			}
			return bea
		},
		"removed from the team": func(t *testing.T, w *linkWorld, team string, bea service.Principal) service.Principal {
			if err := w.svc.RemoveMember(t.Context(), w.ana, team, bea.UserID); err != nil {
				t.Fatal(err)
			}
			return bea
		},
		"demoted": func(t *testing.T, w *linkWorld, team string, bea service.Principal) service.Principal {
			member := string(workspace.RoleMember)
			if _, err := w.svc.SetMember(t.Context(), w.ana, team, bea.UserID, service.MemberRequest{Role: &member}); err != nil {
				t.Fatal(err)
			}
			return bea
		},
		"the team closed": func(t *testing.T, w *linkWorld, team string, bea service.Principal) service.Principal {
			if _, err := w.svc.CloseWorkspace(t.Context(), admin(), team); err != nil {
				t.Fatal(err)
			}
			return bea
		},
		"signed out everywhere": func(t *testing.T, w *linkWorld, _ string, bea service.Principal) service.Principal {
			if err := w.svc.SignOut(t.Context(), bea, service.SignOutRequest{Everywhere: true}); err != nil {
				t.Fatal(err)
			}
			return w.signedIn(t, "bea@example.test")
		},
		"reset": func(t *testing.T, w *linkWorld, _ string, bea service.Principal) service.Principal {
			code, _, err := w.users.CreateReset(t.Context(), bea.UserID, true, "cli")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := w.users.CompleteReset(t.Context(), code, "bea@example.test", authtest.Enrolment(t), "test"); err != nil {
				t.Fatal(err)
			}
			return w.signedIn(t, "bea@example.test")
		},
		"recovered": func(t *testing.T, w *linkWorld, _ string, _ service.Principal) service.Principal {
			w.recoverAccount(t, "bea@example.test")
			return w.signedIn(t, "bea@example.test")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newLinkWorld(t)
			team, bea := w.team(t)
			personal := w.start(t, bea, service.LinkRequest{})
			shared := w.start(t, bea, service.LinkRequest{WorkspaceID: team})
			after := end(t, w, team, bea)
			gone := 0
			for _, started := range []service.LinkStarted{personal, shared} {
				if w.count(t, `SELECT count(*) FROM oauth_links WHERE id = ?`, started.Link.ID) == 0 {
					gone++
					w.googleNames("bea.lima@gmail.com")
					if _, err := w.finish(t, after, started); service.CodeOf(err) != service.CodeNotFound {
						t.Errorf("a return of a link that ended: %v, want not_found", err)
					}
				}
			}
			// Leaving one team, or its closing, ends the links into that team.
			want := 2
			if name == "removed from the team" || name == "demoted" || name == "the team closed" {
				want = 1
			}
			if gone != want {
				t.Errorf("%d of bea's links ended, want %d", gone, want)
			}
			if n := w.idp.exchangeCount(); n != 0 {
				t.Errorf("a link that ended exchanged %d codes", n)
			}
			w.nothingStored(t, "links that ended")
		})
	}
}

// signedIn signs a person in again, as their console would.
func (w *linkWorld) signedIn(t *testing.T, email string) service.Principal {
	t.Helper()
	p, err := w.users.AuthenticateSession(t.Context(), authtest.SignIn(t, w.users, email))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestALinkFinishingAfterItsStarterWasDisabledStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	w.idp.set(func(f *fakeIDP) {
		f.onExchange = func() {
			if _, err := w.svc.DisableUser(context.Background(), admin(), service.CloseUserRequest{Email: "ana@example.test"}); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := w.finish(t, w.ana, started); err == nil {
		t.Fatal("a link finished for a person disabled meanwhile")
	}
	w.nothingStored(t, "a link whose starter was disabled")
}

func TestALinkIntoATeamTheLinkerNoLongerAdministersStoresNothing(t *testing.T) {
	// Demoted during the finish: the transaction that would create the
	// mailbox asks who may link into the team, and refuses.
	w := newLinkWorld(t)
	team, bea := w.team(t)
	started := w.start(t, bea, service.LinkRequest{WorkspaceID: team})
	w.googleNames("support@example.test")
	w.idp.set(func(f *fakeIDP) {
		f.onExchange = func() {
			member := string(workspace.RoleMember)
			if _, err := w.svc.SetMember(context.Background(), w.ana, team, bea.UserID, service.MemberRequest{Role: &member}); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := w.finish(t, bea, started); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("a link by an admin demoted meanwhile: %v", err)
	}
	w.nothingStored(t, "a link by an admin demoted meanwhile")
}

func TestALinkIntoATeamClosedMeanwhileStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	team, _ := w.team(t)
	started := w.start(t, w.ana, service.LinkRequest{WorkspaceID: team})
	w.googleNames("support@example.test")
	w.idp.set(func(f *fakeIDP) {
		f.onExchange = func() {
			if _, err := w.svc.CloseWorkspace(context.Background(), admin(), team); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := w.finish(t, w.ana, started); service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("a link into a team closed meanwhile: %v", err)
	}
	w.nothingStored(t, "a link into a team closed meanwhile")
	// Nor does one start while it is closed.
	if _, err := w.svc.StartLink(t.Context(), w.ana, service.LinkRequest{Provider: "gmail", WorkspaceID: team}); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("a link into a closed team: %v", err)
	}
}

func TestALinkIntoATeamAsksTheFreshnessHookAtItsStartAndAtItsReturn(t *testing.T) {
	hook := &freshHook{}
	w := newLinkWorldWith(t, providertest.IMAPOptions{AnyUser: true},
		fixtureOptions{source: workspace.Platform(), freshness: hook})
	ana := w.followed(t, "owner@example.test")
	w.apply(t, document("owner@example.test", 1, inTeam(workspace.RoleOwner, 1)))
	hook.take()
	want := []service.FreshAct{{Kind: service.FreshLink, ActorID: ana.UserID, WorkspaceID: dirTeamID}}

	started := w.start(t, ana, service.LinkRequest{WorkspaceID: dirTeamID})
	if got := hook.take(); !equalActs(got, want) {
		t.Errorf("a link's start asked %+v, want %+v", got, want)
	}
	w.googleNames("support@example.test")
	if _, err := w.finish(t, ana, started); err != nil {
		t.Fatal(err)
	}
	if got := hook.take(); !equalActs(got, want) {
		t.Errorf("a link's return asked %+v, want %+v", got, want)
	}

	// A directory that cannot be asked: nothing is started.
	hook.answerWith(func(context.Context, service.FreshAct) error { return errors.New("directory down") })
	if _, err := w.svc.StartLink(t.Context(), ana, service.LinkRequest{Provider: "gmail", WorkspaceID: dirTeamID}); service.CodeOf(err) != service.CodeInternal ||
		service.RetryAfterOf(err) == 0 {
		t.Errorf("a link while the directory cannot be asked: %v", err)
	}
	hook.answerWith(nil)

	// An answer at the return that ends the owner's place stops the link
	// before its code is exchanged.
	again := w.start(t, ana, service.LinkRequest{WorkspaceID: dirTeamID})
	exchanged := w.idp.exchangeCount()
	hook.answerWith(func(ctx context.Context, _ service.FreshAct) error {
		_, err := w.svc.ApplyDirectoryDocument(ctx, document("owner@example.test", 2), service.ApplyOptions{})
		return err
	})
	w.googleNames("billing@example.test")
	if _, err := w.finish(t, ana, again); err == nil {
		t.Fatal("a link finished for an owner the directory no longer lists")
	}
	if n := w.idp.exchangeCount(); n != exchanged {
		t.Errorf("the code was exchanged after the directory ended the owner's place")
	}
	if n := w.count(t, `SELECT count(*) FROM accounts WHERE email = 'billing@example.test'`); n != 0 {
		t.Error("the mailbox was linked")
	}
}

// equalActs compares freshness questions.
func equalActs(got, want []service.FreshAct) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Kind != want[i].Kind || got[i].ActorID != want[i].ActorID || got[i].WorkspaceID != want[i].WorkspaceID ||
			got[i].AccountID != want[i].AccountID || len(got[i].RecipientIDs) != len(want[i].RecipientIDs) {
			return false
		}
	}
	return true
}

func TestALinkWithTheTeamsAgreementRecordsItAtTheReturnToTheCurrentText(t *testing.T) {
	w := newLinkWorld(t)
	team, bea := w.team(t)
	current := w.consent().Sync
	before := time.Now().Unix()
	started := w.start(t, bea, service.LinkRequest{WorkspaceID: team, SyncConsentVersion: current})
	w.googleNames("support@example.test")
	created, err := w.finish(t, bea, started)
	if err != nil {
		t.Fatal(err)
	}
	stored := w.stateOfAccount(t, created.ID)
	if stored.SyncConsent.Version != current || stored.SyncConsent.By != bea.UserID || stored.SyncConsent.At < before {
		t.Errorf("the team's agreement: %+v", stored.SyncConsent)
	}

	// A text that changed while the person was at the provider is not the
	// one they agreed to: refused before the code is taken.
	stale := w.start(t, bea, service.LinkRequest{WorkspaceID: team, SyncConsentVersion: current})
	w.opts.consent = config.ConsentVersions{Sync: "2099-01-01-sync"}
	svc := w.rebuild(t, linkRegistry(w.idp))
	exchanged := w.idp.exchangeCount()
	w.googleNames("billing@example.test")
	if _, err := svc.CompleteOAuth(t.Context(), bea, withKey(stateOf(t, stale.Auth))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a return agreeing to a text no longer current: %v", err)
	}
	if n := w.idp.exchangeCount(); n != exchanged {
		t.Error("the code was exchanged for an agreement to an old text")
	}
}

func TestALinkIntoAPersonalWorkspaceReplacedMeanwhileLandsInTheNewOne(t *testing.T) {
	// A person a directory follows signs up with a personal workspace of
	// their own, which their first document replaces: a link started before
	// it lands where their personal workspace is at the return.
	w := newLinkWorldWith(t, providertest.IMAPOptions{AnyUser: true}, fixtureOptions{source: workspace.Platform()})
	cy := w.followed(t, "cy@example.test")
	started := w.start(t, cy, service.LinkRequest{})
	w.apply(t, document("cy@example.test", 1))
	w.googleNames("cy.lima@gmail.com")
	created, err := w.finish(t, cy, started)
	if err != nil {
		t.Fatalf("the return after the replacement: %v", err)
	}
	if created.WorkspaceID != personalOf("cy@example.test") {
		t.Errorf("the mailbox landed in %s, want %s", created.WorkspaceID, personalOf("cy@example.test"))
	}
}

func TestTwoLinksOfOneAccountIntoOneWorkspaceStoreOneMailbox(t *testing.T) {
	// Two returns naming the same account, at once: the transaction that
	// creates a mailbox is the one that counts, and one of them is stored.
	w := newLinkWorld(t)
	team, bea := w.team(t)
	w.googleNames("support@example.test")
	first := w.start(t, w.ana, service.LinkRequest{WorkspaceID: team})
	second := w.start(t, bea, service.LinkRequest{WorkspaceID: team})
	var (
		wg   sync.WaitGroup
		errs = make([]error, 2)
	)
	for i, run := range []struct {
		p       service.Principal
		started service.LinkStarted
	}{{w.ana, first}, {bea, second}} {
		wg.Go(func() { _, errs[i] = w.svc.CompleteOAuth(t.Context(), run.p, withKey(stateOf(t, run.started.Auth))) })
	}
	wg.Wait()
	if n := w.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d mailboxes stored for one account", n)
	}
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("the two returns answered %v and %v, want one of each", errs[0], errs[1])
	}
	for _, err := range errs {
		if err != nil && service.CodeOf(err) != service.CodeConflict {
			t.Errorf("the second return: %v, want conflict", err)
		}
	}
}

func TestAConnectedLinkKeepsNoAddressAndNoAccountOnceItsMailboxIsPurged(t *testing.T) {
	w := newLinkWorld(t)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("ana.lima@gmail.com")
	created, err := w.finish(t, w.ana, started)
	if err != nil {
		t.Fatal(err)
	}
	// The same account again, under another spelling: refused, its address
	// kept on the failed link.
	duplicate := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("AnaLima@gmail.com")
	if _, err := w.finish(t, w.ana, duplicate); service.CodeOf(err) != service.CodeConflict {
		t.Fatal(err)
	}
	if _, err := w.svc.RemoveAccount(t.Context(), w.ana, created.ID, service.RemoveAccountRequest{Confirm: created.ID}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.PurgeAccount(t.Context(), w.ana, created.ID, service.PurgeAccountRequest{Confirm: created.ID}); err != nil {
		t.Fatal(err)
	}
	if l := w.link(t, started.Link.ID); l.State != "connected" || l.AccountID != "" || l.Email != "" {
		t.Errorf("the link of a mailbox deleted for good reads %+v", l)
	}
	if _, err := w.svc.GetLink(t.Context(), w.ana, duplicate.Link.ID); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the failed link naming the address outlived its mailbox: %v", err)
	}
	if found := w.mentions(t, []string{"ana.lima@gmail.com", "AnaLima@gmail.com"}); len(found) != 0 {
		t.Errorf("the address outlived its mailbox in %v", found)
	}
}

// personalWorkspace is a person's personal workspace's id.
func personalWorkspace(t *testing.T, f *fixture, userID string) string {
	t.Helper()
	var id string
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT id FROM workspaces WHERE person_id = ?`, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestALinkIsRefusedToAnInstanceKeyAndToAWorkspaceKey(t *testing.T) {
	w := newLinkWorld(t)
	personal := personalWorkspace(t, w.fixture, w.ana.UserID)
	for name, p := range map[string]service.Principal{
		"an instance key": admin(),
		"a workspace key": {KeyPrefix: "cccccccc", Scope: auth.ScopeSend, WorkspaceID: personal},
	} {
		if _, err := w.svc.StartLink(t.Context(), p, service.LinkRequest{Provider: "gmail"}); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s starting a link: %v", name, err)
		}
	}
	if n := w.links(t); n != 0 {
		t.Errorf("%d links started by keys", n)
	}
}

func TestALinkNeedsTheWebFlow(t *testing.T) {
	w := newLinkWorld(t)
	bob := w.person(t, "bob@example.test", auth.RoleMember)
	for name, req := range map[string]service.LinkRequest{
		"iCloud":             {Provider: "icloud"},
		"generic IMAP":       {Provider: "imap"},
		"no provider":        {},
		"the loopback flow":  {Provider: "gmail", Flow: "loopback"},
		"the device flow":    {Provider: "microsoft", Flow: "device"},
		"a name too long":    {Provider: "gmail", DisplayName: strings.Repeat("a", 201)},
		"a personal agrees":  {Provider: "gmail", SyncConsentVersion: w.consent().Sync},
		"no such workspace":  {Provider: "gmail", WorkspaceID: "wsp_nobody"},
		"another's personal": {Provider: "gmail", WorkspaceID: personalWorkspace(t, w.fixture, bob.UserID)},
	} {
		if _, err := w.svc.StartLink(t.Context(), w.ana, req); service.CodeOf(err) != service.CodeBadRequest &&
			service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A server with only the installed client: the address path alone.
	installed := newFixture(t)
	ana := installed.person(t, "ana@example.test", auth.RoleOwner)
	if _, err := installed.svc.StartLink(t.Context(), ana, service.LinkRequest{Provider: "gmail"}); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a link where no web client is configured: %v", err)
	}
	if n := w.links(t); n != 0 {
		t.Errorf("%d links started", n)
	}
}

func TestTheLinkIsReadOnlyByItsStarterAndShowsNoAccount(t *testing.T) {
	w := newLinkWorld(t)
	team, bea := w.team(t)
	started := w.start(t, w.ana, service.LinkRequest{WorkspaceID: team})
	if _, err := w.svc.GetLink(t.Context(), bea, started.Link.ID); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("another admin of the team reading Ana's link: %v", err)
	}
	if _, err := w.svc.GetLink(t.Context(), w.ana, "lnk_0000000000000000"); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a link nobody started: %v", err)
	}
	w.googleNames("support@example.test")
	created, err := w.finish(t, w.ana, started)
	if err != nil {
		t.Fatal(err)
	}
	l := w.link(t, started.Link.ID)
	if l != (service.Link{ID: started.Link.ID, Provider: "gmail", WorkspaceID: team, State: "connected",
		ExpiresAt: started.Link.ExpiresAt, AccountID: created.ID}) {
		t.Errorf("Ana's connected link reads %+v", l)
	}
}

func TestAProvidersAccountNeverBecomesAPersonsIdentity(t *testing.T) {
	// The ID token names the account chosen at the provider so that its
	// mailbox can be connected, and for nothing else: an address that is a
	// person's sign-in here links only as a mailbox, and the provider's
	// identifier of the account is kept nowhere.
	w := newLinkWorld(t)
	w.person(t, "bea@example.test", auth.RoleMember)
	people := w.count(t, `SELECT count(*) FROM users`)
	started := w.start(t, w.ana, service.LinkRequest{})
	w.googleNames("bea@example.test")
	created, err := w.finish(t, w.ana, started)
	if err != nil {
		t.Fatal(err)
	}
	stored := w.stateOfAccount(t, created.ID)
	if stored.OwnerUserID != w.ana.UserID {
		t.Errorf("the mailbox belongs to %s", stored.OwnerUserID)
	}
	if n := w.count(t, `SELECT count(*) FROM users`); n != people {
		t.Errorf("%d people, want %d", n, people)
	}
	if n := w.count(t, `SELECT count(*) FROM user_identities`); n != 0 {
		t.Errorf("%d identities linked", n)
	}
	if found := w.mentions(t, []string{googleSub}); len(found) != 0 {
		t.Errorf("the provider's identifier of the account is kept in %v", found)
	}
}

func TestAReauthorizationByAnotherAccountIsRefusedAndKeepsTheGrant(t *testing.T) {
	// A mailbox linked at the provider is authorized again as any other:
	// for its address, with the mailbox's scope alone, and a grant that does
	// not open it replaces nothing.
	w := newLinkWorld(t)
	w.googleNames("ana.lima@gmail.com")
	created, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ciphertext FROM credentials WHERE account_id = ?`,
		created.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	flow, err := w.svc.StartOAuth(t.Context(), w.ana, created.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(flow.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("login_hint") != "ana.lima@gmail.com" || u.Query().Get("scope") != "https://mail.google.com/" {
		t.Errorf("a re-authorization leaves for %s", flow.AuthURL)
	}
	// Another account chosen at Google: the mail server does not open the
	// mailbox with what it was granted.
	w.idp.set(func(f *fakeIDP) { f.accept = nil })
	if _, err := w.svc.CompleteOAuth(t.Context(), w.ana, returnTo(linkConsole+"/oauth/return?code=again&state="+stateOf(t, flow))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a re-authorization by another account: %v", err)
	}
	var after []byte
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ciphertext FROM credentials WHERE account_id = ?`,
		created.ID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Errorf("the grant the mailbox had was replaced: %v", err)
	}
}
