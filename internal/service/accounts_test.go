package service_test

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

type fixture struct {
	svc      *service.Service
	registry *account.Registry
	repo     *account.Repository
	imap     *providertest.IMAPServer
	db       *store.Store
	users    *auth.Users
	keys     *auth.Keys
	// bus is the service's event bus, for tests that publish what the sync
	// engine would.
	bus  *events.Bus
	opts fixtureOptions
	// logs is what the service and the registry logged at warn and above,
	// through the production logger and its masking.
	logs *logBuffer
	// spool is where the registry's mailboxes spool fetched sections.
	spool string
}

// fixtureOptions vary the daemon's configuration: which OAuth clients exist
// and where the console is served from.
type fixtureOptions struct {
	publicURL string
	registry  account.RegistryOptions
	// strictHosts keeps the production rule that mail servers on loopback
	// and private networks are refused. Off by default, because the
	// in-process IMAP server every other test logs in to is on 127.0.0.1.
	strictHosts bool
	// providerIMAP, when set, is dialed in place of Gmail's and Microsoft's
	// IMAP servers, which finishing a consent logs in to.
	providerIMAP string
	// sync stands in for the sync engine. nil runs without one, as the
	// daemon's tools that only open the database do.
	sync service.SyncController
	// downloadSpool and downloadsPerCaller bound the downloads in flight;
	// zero is the service's default.
	downloadSpool      int64
	downloadsPerCaller int
	// logLevel is what the logs keep; empty is warn.
	logLevel string
	// sendHashKey is the key compose hashes are made under; nil is the
	// service's random one.
	sendHashKey []byte
	// sendSpool bounds what sends in flight hold; zero is the default.
	sendSpool int64
	// consent is the revisions of the texts people agree to, as
	// MAIL_CONSENT_VERSION_* configure them; empty ones are the defaults.
	consent config.ConsentVersions
	// externalSignInOnly is a daemon whose people sign in only through an
	// extension: passwords and invitations are off.
	externalSignInOnly bool
	// keysMayNotSend is MAIL_KEYS_MAY_SEND=false.
	keysMayNotSend bool
	// keysActUnderCreator is MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT=true.
	keysActUnderCreator bool
}

// providerHosts are the IMAP addresses an account gets from its provider
// rather than from the person adding it.
var providerHosts = []string{"imap.gmail.com:993", "outlook.office365.com:993"}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, fixtureOptions{registry: account.RegistryOptions{
		Google:    account.OAuthClient{ClientID: "google-client"},
		Microsoft: account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
	}})
}

func newFixtureWith(t *testing.T, o fixtureOptions) *fixture {
	t.Helper()
	db := storetest.New(t)
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0xA1
	}
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		repo: account.NewRepository(db, keyring), db: db,
		users: auth.NewUsers(db).WithSaltKey(authtest.SaltKey), keys: auth.NewKeys(db), opts: o,
		bus:  events.NewBus(events.NewJournal(db)),
		logs: &logBuffer{},
	}
	f.svc, f.registry = f.build(t, o.registry)
	return f
}

// consent is the revisions the fixture's service asks for, every one set.
func (f *fixture) consent() config.ConsentVersions { return f.opts.consent.OrDefaults() }

// build wires a registry and a service over the fixture's database.
func (f *fixture) build(t *testing.T, registryOpts account.RegistryOptions) (*service.Service, *account.Registry) {
	t.Helper()
	opts := registryOpts
	opts.PublicURL = f.opts.publicURL
	opts.SpoolDir = t.TempDir()
	f.spool = opts.SpoolDir
	// The in-process IMAP server speaks plain TCP; production configuration
	// has no way to set this.
	opts.AllowInsecureAuth = true
	opts.AllowPrivate = !f.opts.strictHosts
	if f.opts.providerIMAP != "" {
		opts.RedirectIMAP = map[string]string{}
		for _, host := range providerHosts {
			opts.RedirectIMAP[host] = f.opts.providerIMAP
		}
	}
	level := f.opts.logLevel
	if level == "" {
		level = "warn"
	}
	logger := obs.NewLoggerTo(f.logs, level, "text")
	opts.Log = logger
	registry := account.NewRegistry(t.Context(), f.repo, opts)
	t.Cleanup(func() { _ = registry.Close() })
	return service.New(service.Deps{
		Accounts:  registry,
		Keys:      f.keys,
		Users:     f.users,
		Store:     f.db,
		Bus:       f.bus,
		Sync:      f.opts.sync,
		Log:       logger,
		PublicURL: f.opts.publicURL,

		DownloadSpoolBytes:         f.opts.downloadSpool,
		DownloadsPerCaller:         f.opts.downloadsPerCaller,
		SpoolDir:                   f.spool,
		SendHashKey:                f.opts.sendHashKey,
		SendSpoolBytes:             f.opts.sendSpool,
		ConsentVersions:            f.opts.consent,
		ExternalSignInOnly:         f.opts.externalSignInOnly,
		KeysMayNotSend:             f.opts.keysMayNotSend,
		KeysActUnderCreatorConsent: f.opts.keysActUnderCreator,
	}), registry
}

// logBuffer collects log output from the request and from the goroutines a
// loopback or device flow finishes on.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rebuild is the same database under a daemon restarted with other OAuth
// clients in its environment.
func (f *fixture) rebuild(t *testing.T, registryOpts account.RegistryOptions) *service.Service {
	t.Helper()
	svc, _ := f.build(t, registryOpts)
	return svc
}

func admin() service.Principal {
	return service.Principal{KeyPrefix: "aaaaaaaa", Scope: auth.ScopeAdmin, WorkspaceID: workspace.OperatorID}
}
func reader() service.Principal {
	return service.Principal{KeyPrefix: "bbbbbbbb", Scope: auth.ScopeRead, WorkspaceID: workspace.OperatorID}
}

// switchSync is a request to switch a mailbox's own sync on or off: an
// operator mailbox's, which takes no text revision.
func switchSync(on bool) service.MailboxSyncRequest {
	return service.MailboxSyncRequest{Enabled: &on}
}

func TestAddingAGmailAccountStartsConsentAndFillsInTheServers(t *testing.T) {
	// Nobody should have to know imap.gmail.com and its port to add an
	// account.
	f := newFixture(t)
	result, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "person@gmail.com", Flow: "pasted",
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if result.Account.Provider != "gmail" {
		t.Errorf("provider = %q, want it guessed from the address", result.Account.Provider)
	}
	if result.Account.State != "pending_auth" {
		t.Errorf("state = %q; nothing can sync before consent", result.Account.State)
	}
	if result.Auth == nil || result.Auth.AuthURL == "" {
		t.Fatal("no authorisation URL was returned")
	}
	if !strings.Contains(result.Auth.AuthURL, "accounts.google.com") {
		t.Errorf("auth url = %q", result.Auth.AuthURL)
	}

	stored, err := f.repo.Get(t.Context(), result.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IMAPHost != "imap.gmail.com" || stored.IMAPPort != 993 {
		t.Errorf("imap = %s:%d", stored.IMAPHost, stored.IMAPPort)
	}
	if stored.SMTPTLS != "implicit" {
		t.Errorf("smtp tls = %q, want implicit for port 465", stored.SMTPTLS)
	}
	if stored.SaveSentCopy {
		t.Error("Gmail files the sent copy itself; appending a second one is the duplicate users notice")
	}
}

func TestAGenericIMAPAccountWithAPasswordIsActiveImmediately(t *testing.T) {
	f := newFixture(t)
	result, err := f.svc.AddAccount(t.Context(), admin(), f.passwordAccount(t, "person@example.com"))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if result.Auth != nil {
		t.Error("a password account needs no consent flow")
	}
	if result.Account.State != "active" {
		t.Errorf("state = %q, want active", result.Account.State)
	}
	if !result.Account.SaveSentCopy {
		t.Error("a generic server does not file the sent copy, so we must")
	}
}

func TestMicrosoftRefusesAPasswordWithAnExplanation(t *testing.T) {
	// Not a limitation of this server: Exchange Online disabled basic auth
	// for IMAP in 2022 and it cannot be re-enabled.
	f := newFixture(t)
	_, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "person@outlook.com", Password: "hunter2",
	})
	if err == nil || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("want an explanation pointing at OAuth, got %v", err)
	}
}

func TestTheSameMailboxCannotBeAddedTwice(t *testing.T) {
	f := newFixture(t)
	req := f.passwordAccount(t, "person@example.com")
	if _, err := f.svc.AddAccount(t.Context(), admin(), req); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.AddAccount(t.Context(), admin(), req)
	if service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("want a conflict, got %v", err)
	}
}

func TestOnlyAnAdminKeyCanAddOrRemoveAnAccount(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.AddAccount(t.Context(), reader(), service.AddAccountRequest{Email: "x@example.com"})
	if service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("want not_authorized, got %v", err)
	}
	if err := f.svc.RemoveAccount(t.Context(), reader(), "acc_1", service.RemoveAccountRequest{Confirm: "acc_1"}); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("want not_authorized, got %v", err)
	}
}

func TestAKeyRestrictedToOtherAccountsCannotEvenLearnTheyExist(t *testing.T) {
	// Forbidden would confirm the id is real. Not found does not.
	f := newFixture(t)
	created, err := f.svc.AddAccount(t.Context(), admin(), f.passwordAccount(t, "person@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeRead, AccountIDs: []string{"acc_other"}, WorkspaceID: workspace.OperatorID}

	_, err = f.svc.GetAccount(t.Context(), restricted, created.Account.ID)
	if service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	accounts, err := f.svc.ListAccounts(t.Context(), restricted, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("a restricted key saw %d accounts", len(accounts))
	}
}

func TestAKeyRestrictedToSomeAccountsCannotAddAnother(t *testing.T) {
	// Its restriction names the accounts it was issued for; an account it
	// added would be one it could never see again, so it may not add any.
	f := newFixture(t)
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, AccountIDs: []string{"acc_other"}, WorkspaceID: workspace.OperatorID}
	_, err := f.svc.AddAccount(t.Context(), restricted, f.passwordAccount(t, "person@example.com"))
	if service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("want not_authorized, got %v", err)
	}
	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("the refused add left %d accounts behind", len(accounts))
	}
}

func TestListingFoldersResolvesRolesFromTheServersAttributes(t *testing.T) {
	f := newFixture(t)
	f.withIMAPAccount(t, provider.KindIMAP, providertest.RichCaps())
	f.imap.CreateMailbox(t, "Archive")

	folders, err := f.svc.ListFolders(t.Context(), admin(), f.accountID(t))
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}

	byName := map[string]service.Folder{}
	for _, folder := range folders {
		byName[folder.Name] = folder
	}
	inbox, ok := byName["INBOX"]
	if !ok {
		t.Fatalf("INBOX missing from %v", byName)
	}
	if inbox.Role != "inbox" || inbox.RoleSource != "inbox" {
		t.Errorf("INBOX role = %q from %q", inbox.Role, inbox.RoleSource)
	}
	// The in-process server publishes no SPECIAL-USE attributes, so the
	// localised name table is what finds this — the same path Exchange needs.
	archive, ok := byName["Archive"]
	if !ok {
		t.Fatal("Archive missing")
	}
	if archive.Role != "archive" || archive.RoleSource != "name-table" {
		t.Errorf("Archive role = %q from %q", archive.Role, archive.RoleSource)
	}
}

func TestAFolderOverrideBeatsEveryGuess(t *testing.T) {
	// A person who has said which folder is Sent must not be overruled by a
	// name table that happens to match something else.
	f := newFixture(t)
	f.withIMAPAccount(t, provider.KindIMAP, providertest.MicrosoftCaps())
	f.imap.CreateMailbox(t, "Correio Enviado")

	id := f.accountID(t)
	a, err := f.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	_ = a
	if err := f.repo.SetFolderOverrides(t.Context(), id, map[string]string{"sent": "Correio Enviado"}); err != nil {
		t.Fatal(err)
	}

	folders, err := f.svc.ListFolders(t.Context(), admin(), id)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	for _, folder := range folders {
		if folder.Name == "Correio Enviado" {
			if folder.Role != "sent" || folder.RoleSource != "override" {
				t.Fatalf("role = %q from %q, want the override to win", folder.Role, folder.RoleSource)
			}
			return
		}
	}
	t.Fatal("the folder was not listed")
}

func TestTheInboxIsListedFirst(t *testing.T) {
	f := newFixture(t)
	f.withIMAPAccount(t, provider.KindIMAP, providertest.MicrosoftCaps())
	f.imap.CreateMailbox(t, "Aardvark")
	f.imap.CreateMailbox(t, "Trash")

	folders, err := f.svc.ListFolders(t.Context(), admin(), f.accountID(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) == 0 || folders[0].Name != "INBOX" {
		t.Fatalf("first folder = %v, want INBOX", folders)
	}
	if last := folders[len(folders)-1]; last.Role != "" {
		t.Errorf("last folder = %+v, want a folder with no role", last)
	}
}

func TestListingFoldersOfAnUnauthorisedAccountSaysSo(t *testing.T) {
	f := newFixture(t)
	created, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "person@gmail.com", Flow: "pasted",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.ListFolders(t.Context(), admin(), created.Account.ID)
	if service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("want a conflict, got %v", err)
	}
	if !strings.Contains(service.MessageOf(err), "authorised") {
		t.Errorf("message = %q", service.MessageOf(err))
	}
}

func TestAnAccountNeedingReauthReportsThatRatherThanAnUpstreamError(t *testing.T) {
	// The difference matters: one is a thing a person must do, the other is a
	// thing to wait out.
	f := newFixture(t)
	f.withIMAPAccount(t, provider.KindIMAP, providertest.MicrosoftCaps())
	id := f.accountID(t)
	if err := f.repo.MarkNeedsReauth(t.Context(), id, "AADSTS70008"); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.ListFolders(t.Context(), admin(), id)
	if service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("code = %q, got %v", service.CodeOf(err), err)
	}
	if !strings.Contains(service.MessageOf(err), "re-authorization") {
		t.Errorf("message = %q", service.MessageOf(err))
	}
}

func TestAnUnknownAccountIsNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.GetAccount(t.Context(), admin(), "acc_nope"); service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	if err := f.svc.RemoveAccount(t.Context(), admin(), "acc_nope", service.RemoveAccountRequest{Confirm: "acc_nope"}); service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

func TestRemovingAnAccountForgetsIt(t *testing.T) {
	f := newFixture(t)
	created, err := f.svc.AddAccount(t.Context(), admin(), f.passwordAccount(t, "person@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveAccount(t.Context(), admin(), created.Account.ID, service.RemoveAccountRequest{Confirm: created.Account.ID}); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := f.svc.GetAccount(t.Context(), admin(), created.Account.ID); service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("the account survived removal: %v", err)
	}
}

func TestAnUnknownConsentFlowIsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "person@gmail.com", Flow: "carrier-pigeon",
	})
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestCompletingConsentNeedsTheWholeRedirect(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.CompleteOAuth(t.Context(), admin(), "")
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("want bad_request, got %v", err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), admin(), "http://127.0.0.1/cb?code=abc")
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a redirect with no state should be refused, got %v", err)
	}
}

func TestAReplayedRedirectFindsNothing(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.CompleteOAuth(t.Context(), admin(), "http://127.0.0.1/cb?code=abc&state=unknown")
	if service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

// --- fixture helpers -------------------------------------------------------

// mailServer is the in-process IMAP server the fixture's password accounts
// log in to, started on first use.
func (f *fixture) mailServer(t *testing.T) *providertest.IMAPServer {
	t.Helper()
	if f.imap == nil {
		f.imap = providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2"})
	}
	return f.imap
}

// passwordAccount is a request for a generic IMAP account on the fixture's
// server. Any address will do: it logs in as the server's one user, which is
// what login_user is for.
func (f *fixture) passwordAccount(t *testing.T, email string) service.AddAccountRequest {
	t.Helper()
	srv := f.mailServer(t)
	host, port := splitHostPort(t, srv.Addr)
	return service.AddAccountRequest{
		Email: email, Password: "hunter2", LoginUser: srv.User,
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port,
	}
}

// withIMAPAccount registers an account backed by the in-process server.
func (f *fixture) withIMAPAccount(t *testing.T, kind provider.Kind, caps goimap.CapSet) {
	t.Helper()
	f.imap = providertest.NewIMAPServer(t, providertest.IMAPOptions{Caps: caps, Password: "hunter2"})
	host, port := splitHostPort(t, f.imap.Addr)

	if _, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: f.imap.User, Provider: string(kind), Password: "hunter2",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
}

func (f *fixture) accountID(t *testing.T) string {
	t.Helper()
	accounts, err := f.repo.List(t.Context())
	if err != nil || len(accounts) == 0 {
		t.Fatalf("no account registered: %v", err)
	}
	return accounts[0].ID
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("bad address %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in %q: %v", addr, err)
	}
	return host, port
}

func TestEveryBrowserFlowGetsARedirectTheProviderWillAccept(t *testing.T) {
	// Google matches the exact loopback address and accepts any port, but a
	// URI with no port at all is not a loopback redirect. Binding a real
	// listener in both flows also means a browser on this machine finishes
	// the flow without anyone pasting anything.
	f := newFixture(t)
	for _, flow := range []string{"loopback", "pasted"} {
		result, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
			Email: "person-" + flow + "@gmail.com", Flow: flow,
		})
		if err != nil {
			t.Fatalf("%s: AddAccount: %v", flow, err)
		}
		redirect := redirectURIOf(t, result.Auth.AuthURL)
		if strings.HasSuffix(redirect, ":0/oauth/callback") || !strings.Contains(redirect, "127.0.0.1:") {
			t.Errorf("%s: redirect_uri = %q, want a real loopback port", flow, redirect)
		}
	}
}

func TestTheMicrosoftRedirectUsesTheNameEntraExpects(t *testing.T) {
	// Entra matches "http://localhost" and ignores the port; the IPv6 form is
	// refused outright, so the host has to be the name, not an address.
	f := newFixture(t)
	result, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "person@outlook.com", Flow: "loopback",
	})
	if err != nil {
		t.Fatal(err)
	}
	redirect := redirectURIOf(t, result.Auth.AuthURL)
	if !strings.HasPrefix(redirect, "http://localhost:") {
		t.Fatalf("redirect_uri = %q, want http://localhost:<port>", redirect)
	}
}

func redirectURIOf(t *testing.T, authURL string) string {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	return parsed.Query().Get("redirect_uri")
}

func TestAPersonCannotWidenTheInitialSyncWindow(t *testing.T) {
	// A person consented to the last 90 days of their mail being indexed. A
	// session — or a script holding its token — asking for everything, or
	// for ten years, is refused; the operator's key keeps the setting for
	// mailboxes nobody owns.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	for _, days := range []int{-1, 30, 3650} {
		req := f.passwordAccount(t, "ana"+strconv.Itoa(days)+"@mail.example")
		req.InitialDays = days
		if _, err := f.svc.AddAccount(t.Context(), ana, req); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("initial_days %d from a person: %v, want bad_request", days, err)
		}
	}
	if accounts, err := f.svc.ListAccounts(t.Context(), ana, ""); err != nil || len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind (%v)", len(accounts), err)
	}
	for _, days := range []int{0, account.PersonInitialDays} {
		req := f.passwordAccount(t, "ana.ok"+strconv.Itoa(days)+"@mail.example")
		req.InitialDays = days
		if _, err := f.svc.AddAccount(t.Context(), ana, req); err != nil {
			t.Errorf("initial_days %d from a person: %v", days, err)
		}
	}

	req := f.passwordAccount(t, "shared@mail.example")
	req.InitialDays = -1
	added, err := f.svc.AddAccount(t.Context(), admin(), req)
	if err != nil {
		t.Fatalf("initial_days -1 from the operator: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND initial_days = -1`, added.Account.ID); n != 1 {
		t.Error("the operator's window was not kept")
	}
}

func TestAPersonCannotConnectGmailAsAGenericIMAPAccount(t *testing.T) {
	// Gmail under another name — a Workspace domain, or "other IMAP" with an
	// app password — is still Gmail, and a person connects Gmail with Google
	// sign-in only.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	for _, req := range []service.AddAccountRequest{
		{Email: "ana@gmail.com", Provider: "imap", IMAPHost: "mail.example.net", SMTPHost: "mail.example.net"},
		{Email: "ana@googlemail.com", Provider: "imap", IMAPHost: "mail.example.net", SMTPHost: "mail.example.net"},
		{Email: "ana@workspace.example", IMAPHost: "imap.gmail.com", SMTPHost: "smtp.gmail.com"},
		{Email: "ana@workspace.example", Provider: "imap", IMAPHost: "IMAP.GoogleMail.com.", SMTPHost: "smtp.gmail.com"},
	} {
		req.Password = "abcd efgh ijkl mnop"
		_, err := f.svc.AddAccount(t.Context(), ana, req)
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "Google sign-in") {
			t.Errorf("%s on %s: %v, want a refusal pointing at Google sign-in", req.Email, req.IMAPHost, err)
		}
	}
	if accounts, err := f.svc.ListAccounts(t.Context(), ana, ""); err != nil || len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind (%v)", len(accounts), err)
	}
}
