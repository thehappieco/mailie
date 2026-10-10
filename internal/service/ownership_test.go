package service_test

import (
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// person makes a user and returns the principal their browser session is.
func (f *fixture) person(t *testing.T, email string, role auth.Role) service.Principal {
	t.Helper()
	authtest.NewUser(t, f.db, email, role)
	p, err := f.users.AuthenticateSession(t.Context(), authtest.SignIn(t, f.users, email))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// mailbox adds a generic IMAP account as p and returns its id.
func (f *fixture) mailbox(t *testing.T, p service.Principal, email string) string {
	t.Helper()
	result, err := f.svc.AddAccount(t.Context(), p, keyed(p, f.passwordAccount(t, email)))
	if err != nil {
		t.Fatalf("AddAccount(%s): %v", email, err)
	}
	return result.Account.ID
}

func ids(t *testing.T, f *fixture, p service.Principal) []string {
	t.Helper()
	accounts, err := f.svc.ListAccounts(t.Context(), p, "")
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.ID)
	}
	return out
}

func TestASessionOpensOnlyItsOwnersAccounts(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")

	stored, err := f.repo.Get(t.Context(), anas)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerUserID != ana.UserID {
		t.Fatalf("owner = %q, want the person who added it", stored.OwnerUserID)
	}

	if got := ids(t, f, ana); !slices.Equal(got, []string{anas}) {
		t.Errorf("ana lists %v, want only her own", got)
	}
	// Not found rather than forbidden, on every route: another person's
	// account ids must not be discoverable by probing.
	if _, err := f.svc.GetAccount(t.Context(), ana, bobs); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("GetAccount of someone else's account: %v", err)
	}
	if _, err := f.svc.ListFolders(t.Context(), ana, bobs); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("ListFolders of someone else's account: %v", err)
	}
	if _, err := f.svc.StartOAuth(t.Context(), ana, bobs, ""); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("StartOAuth of someone else's account: %v", err)
	}
	if err := f.svc.RemoveAccount(t.Context(), ana, bobs, service.RemoveAccountRequest{Confirm: bobs}); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("RemoveAccount of someone else's account: %v", err)
	}
	if _, err := f.repo.Get(t.Context(), bobs); err != nil {
		t.Fatalf("bob's account did not survive ana's attempt: %v", err)
	}
	if err := f.svc.RemoveAccount(t.Context(), bob, bobs, service.RemoveAccountRequest{Confirm: bobs}); err != nil {
		t.Fatalf("bob could not remove his own account: %v", err)
	}
}

func TestNobodySeesTheOperatorsMailboxesButAnInstanceKeyAndAnInstanceKeyNobodyElses(t *testing.T) {
	f := newFixture(t)
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	member := f.person(t, "member@example.com", auth.RoleMember)
	instance := f.mailbox(t, admin(), "cli@mail.example")
	owners := f.mailbox(t, owner, "owner@mail.example")
	members := f.mailbox(t, member, "member@mail.example")

	stored, err := f.repo.Get(t.Context(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerUserID != "" {
		t.Fatalf("an instance key's account got owner %q", stored.OwnerUserID)
	}

	// Administering the instance is neither reading other people's mail nor
	// the operator's: an owner of it sees their own mailboxes, like anyone.
	if got := ids(t, f, owner); !slices.Equal(got, []string{owners}) {
		t.Errorf("the owner lists %v, want only their own", got)
	}
	if got := ids(t, f, member); !slices.Equal(got, []string{members}) {
		t.Errorf("a member lists %v, want only their own", got)
	}
	for name, p := range map[string]service.Principal{"a member": member, "an owner": owner} {
		if _, err := f.svc.GetAccount(t.Context(), p, instance); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("%s reached an instance account: %v", name, err)
		}
	}
	if _, err := f.svc.GetAccount(t.Context(), owner, members); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the owner reached a member's account: %v", err)
	}
	// The operator's key, for its part, reaches the operator workspace's.
	if got := ids(t, f, admin()); !slices.Equal(got, []string{instance}) {
		t.Errorf("an instance key lists %v, want only the instance's account", got)
	}
}

func TestAKeySeesOnlyTheMailboxesItHolds(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	f.mailbox(t, ana, "ana@other.example")
	f.mailbox(t, admin(), "cli@mail.example")

	key := keyOf(t, f, ana, auth.ScopeRead)
	if got := ids(t, f, key); len(got) != 2 {
		t.Errorf("a key holding both of ana's mailboxes lists %v", got)
	}
	one := f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, authtest.Personal(t, f.db, ana.UserID),
		ana.UserID, workspace.KeyGrant{AccountID: anas, Flags: workspace.Flags{Read: true}}))
	if got := ids(t, f, one); !slices.Equal(got, []string{anas}) {
		t.Errorf("a key holding one of ana's mailboxes lists %v, want only it", got)
	}
	none := f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, authtest.Personal(t, f.db, ana.UserID),
		ana.UserID))
	if got := ids(t, f, none); len(got) != 0 {
		t.Errorf("a key holding nothing lists %v", got)
	}
	// Still a key: it is nobody at a keyboard.
	if _, err := f.svc.Me(t.Context(), key); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key reached a session-only use case: %v", err)
	}
}

func TestProvidersOfferWhatThisCallerCanUse(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{
		publicURL: "http://localhost:5174",
		registry: account.RegistryOptions{
			Google:    account.OAuthClient{ClientID: "google-installed"},
			Microsoft: account.OAuthClient{ClientID: "ms-installed", Tenant: "common"},
			GoogleWeb: account.OAuthClient{ClientID: "google-web", ClientSecret: "s"},
		},
	})
	session := f.person(t, "ana@example.com", auth.RoleOwner)

	byID := func(p service.Principal) map[string]service.Provider {
		providers, err := f.svc.Providers(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers) != 4 || providers[0].ID != "gmail" || providers[1].ID != "microsoft" ||
			providers[2].ID != "icloud" || providers[3].ID != "imap" {
			t.Fatalf("providers = %+v, want gmail, microsoft, icloud, imap in that order", providers)
		}
		out := map[string]service.Provider{}
		for _, p := range providers {
			out[p.ID] = p
		}
		return out
	}

	browser := byID(session)
	if got := browser["gmail"]; !got.OAuth || got.Password || !slices.Equal(got.Flows, []string{"web", "loopback"}) {
		t.Errorf("gmail for a local console = %+v, want the web flow first, then loopback, and no password", got)
	}
	if got := browser["microsoft"]; got.Password || !slices.Equal(got.Flows, []string{"loopback"}) {
		t.Errorf("microsoft for a local console = %+v", got)
	}
	if got := browser["imap"]; got.OAuth || !got.Password || len(got.Flows) != 0 {
		t.Errorf("imap = %+v", got)
	}
	if got := browser["icloud"]; got.OAuth || !got.Password || len(got.Flows) != 0 {
		t.Errorf("icloud = %+v, want an app-specific password and nothing else", got)
	}

	cli := byID(admin())
	if got := cli["gmail"]; !got.Password || !slices.Equal(got.Flows, []string{"loopback", "pasted", "web"}) {
		t.Errorf("gmail for the CLI = %+v", got)
	}
	// Off unless MAIL_MICROSOFT_DEVICE_CODE says otherwise.
	if got := cli["microsoft"]; slices.Contains(got.Flows, "device") {
		t.Errorf("the device flow is offered without being turned on: %+v", got)
	}
}

func TestAHostedConsoleIsNeverOfferedLoopback(t *testing.T) {
	// The loopback listener binds the daemon's own 127.0.0.1. From a browser
	// on another machine the redirect lands on that machine and fails, after
	// the person has already consented.
	f := newFixtureWith(t, fixtureOptions{
		publicURL: "https://console.mailie.example",
		registry: account.RegistryOptions{
			Google:    account.OAuthClient{ClientID: "google-installed"},
			Microsoft: account.OAuthClient{ClientID: "ms-installed", Tenant: "common"},
			GoogleWeb: account.OAuthClient{ClientID: "google-web", ClientSecret: "s"},
		},
	})
	session := f.person(t, "ana@example.com", auth.RoleOwner)

	providers, err := f.svc.Providers(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if slices.Contains(p.Flows, "loopback") || slices.Contains(p.Flows, "pasted") {
			t.Errorf("%s offers %v to a hosted console", p.ID, p.Flows)
		}
	}
	if providers[0].ID != "gmail" || !slices.Equal(providers[0].Flows, []string{"web"}) {
		t.Errorf("gmail = %+v, want only the web flow", providers[0])
	}
	if providers[1].ID != "microsoft" || len(providers[1].Flows) != 0 {
		t.Errorf("microsoft = %+v, want nothing: it has no web client here", providers[1])
	}

	// Asking for it by name does not get it either, whether adding an
	// account or re-authorising one.
	_, err = f.svc.AddAccount(t.Context(), session, keyed(session, service.AddAccountRequest{Email: "ana@gmail.com", Flow: "loopback"}))
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("AddAccount with flow=loopback from a hosted console: %v", err)
	}
	_, err = f.svc.AddAccount(t.Context(), session, keyed(session, service.AddAccountRequest{Email: "ana@outlook.com"}))
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a Microsoft account with no usable flow: %v", err)
	}
	if accounts, _ := f.svc.ListAccounts(t.Context(), session, ""); len(accounts) != 0 {
		t.Errorf("a refused flow still left %d accounts behind", len(accounts))
	}
	// An instance key keeps the CLI's default.
	result, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "cli@gmail.com"})
	if err != nil {
		t.Fatalf("the CLI's loopback flow: %v", err)
	}
	if result.Auth == nil || result.Auth.Flow != "loopback" {
		t.Errorf("auth = %+v, want the loopback flow", result.Auth)
	}
	hers, err := f.svc.AddAccount(t.Context(), session, keyed(session, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatalf("the web flow from a hosted console: %v", err)
	}
	if _, err := f.svc.StartOAuth(t.Context(), session, hers.Account.ID, "loopback"); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("StartOAuth with flow=loopback from a hosted console: %v", err)
	}
}

func TestTheConsoleConnectsGmailWithGoogleSignInOnly(t *testing.T) {
	f := newFixture(t)
	session := f.person(t, "ana@example.com", auth.RoleOwner)
	_, err := f.svc.AddAccount(t.Context(), session, keyed(session, service.AddAccountRequest{
		Email: "ana@gmail.com", Password: "app-password",
	}))
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a session added Gmail with a password: %v", err)
	}
	// Nor a key of her workspace, which links no mailbox at all.
	agent := keyOf(t, f, session, auth.ScopeAdmin)
	if _, err := f.svc.AddAccount(t.Context(), agent, service.AddAccountRequest{
		Email: "ana@gmail.com", Password: "app-password",
	}); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("a key of her workspace added Gmail with a password: %v", err)
	}
	// The CLI keeps app passwords. The servers are overridden only because
	// the password is tried before the account is stored, and a unit test
	// cannot reach imap.gmail.com.
	cli := f.passwordAccount(t, "cli@gmail.com")
	cli.SMTPHost, cli.SMTPPort = "", 0
	if _, err := f.svc.AddAccount(t.Context(), admin(), cli); err != nil {
		t.Fatalf("an instance key could not add Gmail with an app password: %v", err)
	}
}

func TestEveryAuthFlowSaysWhichFlowItIs(t *testing.T) {
	f := newFixture(t)
	for _, flow := range []string{"loopback", "pasted"} {
		result, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
			Email: "person-" + flow + "@gmail.com", Flow: flow,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Auth == nil || result.Auth.Flow != flow {
			t.Errorf("%s: auth = %+v", flow, result.Auth)
		}
		again, err := f.svc.StartOAuth(t.Context(), admin(), result.Account.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if again.Flow != "loopback" {
			t.Errorf("restarting with no flow named gave %q, want the default loopback", again.Flow)
		}
	}
}

func TestTheDeviceFlowIsOffUnlessTurnedOn(t *testing.T) {
	// Entra security defaults block it, so offering it by default would be a
	// flow that fails for most tenants.
	f := newFixture(t)
	_, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "a@outlook.com", Flow: "device"})
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("the device flow ran without MAIL_MICROSOFT_DEVICE_CODE: %v", err)
	}

	on := newFixtureWith(t, fixtureOptions{registry: account.RegistryOptions{
		Microsoft:  account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
		DeviceCode: true,
	}})
	providers, err := on.svc.Providers(t.Context(), admin())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(providers[1].Flows, "device") {
		t.Errorf("microsoft = %+v, want the device flow once it is turned on", providers[1])
	}
	if slices.Contains(providers[0].Flows, "device") {
		t.Errorf("gmail = %+v: Google does not allow the device flow for mail", providers[0])
	}
}

func TestAnAccountRequestIsCheckedBeforeAnythingIsStored(t *testing.T) {
	f := newFixture(t)
	for name, req := range map[string]service.AddAccountRequest{
		"no servers":      {Email: "a@example.com", Password: "x"},
		"no password":     {Email: "a@example.com", IMAPHost: "h", SMTPHost: "h"},
		"bad tls mode":    {Email: "a@example.com", Password: "x", IMAPHost: "h", SMTPHost: "h", SMTPTLS: "ssl"},
		"bad port":        {Email: "a@example.com", Password: "x", IMAPHost: "h", SMTPHost: "h", IMAPPort: 70000},
		"display name":    {Email: "Ana <a@example.com>", Password: "x", IMAPHost: "h", SMTPHost: "h"},
		"unknown kind":    {Email: "a@example.com", Provider: "aol", Password: "x"},
		"microsoft+pass":  {Email: "a@outlook.com", Password: "x"},
		"unknown flow":    {Email: "a@gmail.com", Flow: "carrier-pigeon"},
		"missing address": {Password: "x"},
	} {
		_, err := f.svc.AddAccount(t.Context(), admin(), req)
		if service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}
	// A bad TLS mode used to reach the database and come back as the text of
	// a CHECK constraint.
	_, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "a@example.com", Password: "x", IMAPHost: "h", SMTPHost: "h", SMTPTLS: "ssl",
	})
	if msg := service.MessageOf(err); msg != "smtp_tls must be implicit or starttls" {
		t.Errorf("message = %q", msg)
	}
	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Errorf("refused requests left %d accounts behind", len(accounts))
	}
}
