package account_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A link at the provider (links.go): a Gmail or Microsoft mailbox whose
// address the provider names, created only at the return.

const publicURL = "https://mail.example.test"

// linkIDP is a token endpoint that answers a code with an ID token of the
// claims it is given, and counts what reaches it.
type linkIDP struct {
	server *httptest.Server

	mu        sync.Mutex
	exchanges []url.Values
	issued    int
	// claims are the next ID token's; nil sends none.
	claims map[string]any
	// accept tells the mail server standing in for the provider to take a
	// token; off, it refuses every one.
	accept func(string)
	// onExchange runs while a code is exchanged, after its link was taken.
	onExchange func()
}

func newLinkIDP(t *testing.T) *linkIDP {
	t.Helper()
	idp := &linkIDP{}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.token))
	t.Cleanup(idp.server.Close)
	return idp
}

func (f *linkIDP) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: f.server.URL + "/authorize", TokenURL: f.server.URL + "/token"}
}

func (f *linkIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	hook := f.onExchange
	f.mu.Unlock()
	if r.Form.Get("grant_type") == "authorization_code" && hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Form.Get("grant_type") == "authorization_code" {
		f.exchanges = append(f.exchanges, r.Form)
	}
	f.issued++
	access := "access-" + strconv.Itoa(f.issued)
	if f.accept != nil {
		f.accept(access)
	}
	response := map[string]any{
		"access_token": access, "refresh_token": "refresh-" + strconv.Itoa(f.issued), "token_type": "Bearer",
		"expires_in": 3600,
	}
	if f.claims != nil && r.Form.Get("grant_type") == "authorization_code" {
		payload, _ := json.Marshal(f.claims)
		response["id_token"] = "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (f *linkIDP) set(change func(*linkIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *linkIDP) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchanges)
}

func (f *linkIDP) exchange(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges[i]
}

// linkFixture is a registry whose web clients talk to one fake identity
// provider and whose Gmail and Microsoft mailboxes log in to one in-process
// IMAP server, on a clock the test moves.
type linkFixture struct {
	t        *testing.T
	db       *store.Store
	repo     *account.Repository
	registry *account.Registry
	ws       *workspace.Repository
	idp      *linkIDP
	imap     *providertest.IMAPServer
	clock    *time.Time
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	db := storetest.New(t)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	clock := &now
	repo := account.NewRepository(db, keyring).WithClock(func() time.Time { return *clock })
	idp := newLinkIDP(t)
	imap := providertest.NewIMAPServer(t, providertest.IMAPOptions{AnyUser: true})
	idp.accept = imap.AcceptToken
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		GoogleWeb: account.OAuthClient{ClientID: googleClient, ClientSecret: "google-secret", Endpoint: idp.endpoint()},
		MicrosoftWeb: account.OAuthClient{ClientID: msClient, ClientSecret: "ms-secret", Endpoint: idp.endpoint(),
			Issuer: "https://idp.example.test/{tenantid}/v2.0"},
		PublicURL:         publicURL,
		SpoolDir:          t.TempDir(),
		AllowInsecureAuth: true,
		AllowPrivate:      true,
		RedirectIMAP:      map[string]string{"imap.gmail.com:993": imap.Addr, "outlook.office365.com:993": imap.Addr},
	})
	t.Cleanup(func() { _ = registry.Close() })
	return &linkFixture{t: t, db: db, repo: repo, registry: registry, ws: workspace.NewRepository(db, nil),
		idp: idp, imap: imap, clock: clock}
}

func (f *linkFixture) person(email string) auth.User {
	f.t.Helper()
	return authtest.NewUser(f.t, f.db, email, auth.RoleMember)
}

// start starts a link of owner's.
func (f *linkFixture) start(owner string, kind provider.Kind, workspaceID string) (account.Link, *account.AuthFlow) {
	f.t.Helper()
	link, flow, err := f.registry.StartLink(f.t.Context(), account.LinkStart{
		Provider: kind, OwnerUserID: owner, WorkspaceID: workspaceID, DisplayName: "Work",
	})
	if err != nil {
		f.t.Fatalf("StartLink: %v", err)
	}
	return link, flow
}

// names makes the provider's next answer name email, as Google or
// Microsoft would.
func (f *linkFixture) names(kind provider.Kind, email string) {
	f.t.Helper()
	claims := googleClaims(email)
	if kind == provider.KindMicrosoft {
		claims = with(microsoftClaims(email, consumers), map[string]any{
			"iss": "https://idp.example.test/" + consumers + "/v2.0",
		})
	}
	f.idp.set(func(i *linkIDP) { i.claims = claims })
}

// complete posts a link's return with a code, as owner, with a key.
func (f *linkFixture) complete(flow *account.AuthFlow, owner string, c account.LinkCompletion) (account.Account, error) {
	f.t.Helper()
	if c.Key == nil {
		key := authtest.LinkKey(f.t)
		c.Key = &key
	}
	return f.registry.CompleteLink(f.t.Context(), account.Redirect{Code: "the-code", State: flow.State}, owner, c)
}

// linkRow is what a link's row holds; the zero value is a row gone.
type linkRow struct {
	Outcome, AccountID, Email, Reason string
	// Verifier and Name are whether it keeps its PKCE verifier and the
	// name it was asked for; Taken whether its return was taken.
	Verifier, Name, Taken bool
	Exists                bool
}

// gone is a link's row deleted.
var gone = linkRow{}

// failed is a finished link's row, failed for reason with the address the
// provider named, if any.
func failed(reason, email string, taken bool) linkRow {
	return linkRow{Outcome: "failed", Email: email, Reason: reason, Taken: taken, Exists: true}
}

// waiting is a link's row waiting for its return, or taken and finishing.
func waiting(taken bool) linkRow {
	return linkRow{Verifier: true, Name: true, Taken: taken, Exists: true}
}

// connectedTo is a link's row that connected accountID, or "" once that
// mailbox was deleted for good.
func connectedTo(accountID string) linkRow {
	return linkRow{Outcome: "connected", AccountID: accountID, Taken: true, Exists: true}
}

// row reads what a link's row holds.
func (f *linkFixture) row(id string) linkRow {
	f.t.Helper()
	r := linkRow{Exists: true}
	err := f.db.Reader().QueryRowContext(f.t.Context(), `SELECT outcome, coalesce(account_id, ''), email, reason,
		pkce_verifier <> '', display_name <> '', taken_at <> 0 FROM oauth_links WHERE id = ?`, id).
		Scan(&r.Outcome, &r.AccountID, &r.Email, &r.Reason, &r.Verifier, &r.Name, &r.Taken)
	if errors.Is(err, sql.ErrNoRows) {
		return gone
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *linkFixture) count(query string, args ...any) int {
	f.t.Helper()
	return countOf(f.t, f.db, query, args...)
}

func TestOnlyALinkAsksTheProviderForTheAccountsAddress(t *testing.T) {
	for _, kind := range []provider.Kind{provider.KindGmail, provider.KindMicrosoft} {
		client := account.OAuthClient{ClientID: "client-id", Tenant: "common"}
		link, err := url.Parse(account.LinkAuthURLForTest(t, kind, client, publicURL+"/oauth/return", "state-1"))
		if err != nil {
			t.Fatal(err)
		}
		scopes := strings.Fields(link.Query().Get("scope"))
		if !slices.Contains(scopes, "openid") || !slices.Contains(scopes, "email") || link.Query().Has("login_hint") {
			t.Errorf("%s: a link asks for %v with the hint %q, want openid and email, and no hint", kind, scopes,
				link.Query().Get("login_hint"))
		}
		if slices.Contains(scopes, "profile") || strings.Contains(link.Query().Get("scope"), "userinfo") {
			t.Errorf("%s: a link asks for more of the account than its address: %v", kind, scopes)
		}
		// Re-authorizing a mailbox, or linking one by the address typed,
		// asks for the mailbox alone, for the address it names.
		again, err := url.Parse(account.ReauthURLForTest(t, kind, client, publicURL+"/oauth/return", "state-2", "ana@example.test"))
		if err != nil {
			t.Fatal(err)
		}
		scopes = strings.Fields(again.Query().Get("scope"))
		if slices.Contains(scopes, "openid") || slices.Contains(scopes, "email") ||
			again.Query().Get("login_hint") != "ana@example.test" {
			t.Errorf("%s: a re-authorization asks for %v with the hint %q", kind, scopes, again.Query().Get("login_hint"))
		}
	}

	// And so does the registry: a link at its start, a mailbox linked by
	// its address and its consent started again.
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	_, flow := f.start(ana.ID, provider.KindGmail, "")
	if scope := queryOf(t, flow.AuthURL).Get("scope"); scope != "https://mail.google.com/ openid email" {
		t.Errorf("a link's scope: %q", scope)
	}
	key := authtest.LinkKey(t)
	typed, byAddress, err := f.registry.Add(t.Context(), account.AddRequest{
		Email: "ana@gmail.com", Provider: provider.KindGmail, Flow: account.FlowWeb, LinkerID: ana.ID, Key: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.registry.StartAuth(t.Context(), typed.ID, account.FlowWeb, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	for what, flow := range map[string]*account.AuthFlow{"linked by its address": byAddress, "authorized again": again} {
		q := queryOf(t, flow.AuthURL)
		if q.Get("scope") != "https://mail.google.com/" || q.Get("login_hint") != "ana@gmail.com" || q.Get("prompt") != "consent" {
			t.Errorf("a mailbox %s asks for %q, hint %q, prompt %q", what, q.Get("scope"), q.Get("login_hint"), q.Get("prompt"))
		}
	}
}

// queryOf is an authorization URL's query.
func queryOf(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestALinkAsksGoogleToShowItsAccountChooser(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	link, flow := f.start(ana.ID, provider.KindGmail, "")
	q := queryOf(t, flow.AuthURL)
	for param, want := range map[string]string{
		"prompt": "select_account consent", "access_type": "offline", "code_challenge_method": "S256",
		"client_id": googleClient, "redirect_uri": publicURL + "/oauth/return", "state": flow.State,
	} {
		if got := q.Get(param); got != want {
			t.Errorf("a Gmail link's %s is %q, want %q", param, got, want)
		}
	}
	if flow.Kind != account.FlowWeb || flow.AccountID != "" || flow.State == "" ||
		!flow.ExpiresAt.Equal(f.clock.Add(10*time.Minute)) {
		t.Errorf("the flow: %+v", flow)
	}
	if link.State != account.LinkWaiting || link.Provider != provider.KindGmail || link.WorkspaceID != "" ||
		!strings.HasPrefix(link.ID, "lnk_") || !link.ExpiresAt.Equal(flow.ExpiresAt) {
		t.Errorf("the link: %+v", link)
	}
	// Microsoft opens its chooser too, with the tenant the deployment's.
	ms := queryOf(t, account.LinkAuthURLForTest(t, provider.KindMicrosoft,
		account.OAuthClient{ClientID: msClient, Tenant: "common"}, publicURL+"/oauth/return", "s"))
	if ms.Get("prompt") != "select_account" || ms.Has("login_hint") {
		t.Errorf("a Microsoft link's prompt %q and hint %q", ms.Get("prompt"), ms.Get("login_hint"))
	}
	raw := account.LinkAuthURLForTest(t, provider.KindMicrosoft, account.OAuthClient{ClientID: msClient, Tenant: "common"},
		publicURL+"/oauth/return", "s")
	if !strings.HasPrefix(raw, "https://login.microsoftonline.com/common/") {
		t.Errorf("a Microsoft link goes to %s", raw)
	}
	_, msFlow := f.start(ana.ID, provider.KindMicrosoft, "")
	if q := queryOf(t, msFlow.AuthURL); q.Get("prompt") != "select_account" || q.Get("client_id") != msClient {
		t.Errorf("the registry's Microsoft link: %v", q)
	}
}

func TestTheSignInScopesAreNeverRequiredOfAGrant(t *testing.T) {
	// Neither provider lists openid and email among what a token is for:
	// a grant is the mailbox's, or refused, whatever else it says.
	const (
		imapScope = "https://outlook.office.com/IMAP.AccessAsUser.All"
		smtpScope = "https://outlook.office.com/SMTP.Send"
	)
	for _, c := range []struct {
		name    string
		kind    provider.Kind
		scope   any
		missing bool
	}{
		{"google, the mailbox alone", provider.KindGmail, "https://mail.google.com/", false},
		{"google, as it answers a link", provider.KindGmail,
			"openid https://www.googleapis.com/auth/userinfo.email https://mail.google.com/", false},
		{"google, the sign-in alone", provider.KindGmail, "openid https://www.googleapis.com/auth/userinfo.email", true},
		{"google, saying nothing", provider.KindGmail, nil, false},
		{"microsoft, the mailbox alone", provider.KindMicrosoft, imapScope + " " + smtpScope, false},
		{"microsoft, with the sign-in", provider.KindMicrosoft, "openid profile email " + imapScope + " " + smtpScope, false},
		{"microsoft, the sign-in alone", provider.KindMicrosoft, "openid email", true},
	} {
		extra := map[string]any{}
		if c.scope != nil {
			extra["scope"] = c.scope
		}
		reason, err := account.LinkScopeCheckForTest(t, c.kind, extra)
		switch {
		case !c.missing && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.missing && (!errors.Is(err, account.ErrScopeMissing) || reason != account.ReasonScopeMissing):
			t.Errorf("%s: %q, %v, want ErrScopeMissing", c.name, reason, err)
		case c.missing:
			_, lacks, _ := strings.Cut(err.Error(), "lacks ")
			if fields := strings.Fields(lacks); slices.Contains(fields, "openid") || slices.Contains(fields, "email") {
				t.Errorf("%s: the sign-in scopes are said missing: %v", c.name, err)
			}
		}
	}
}

func TestALinkCreatesItsMailboxWithItsKeyOnlyOnceItsLoginWorked(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	link, flow := f.start(ana.ID, provider.KindGmail, "")
	// Nothing of the mailbox exists while the link waits, not its address.
	if n := f.count(`SELECT count(*) FROM accounts`) + f.count(`SELECT count(*) FROM mailbox_keys`) +
		f.count(`SELECT count(*) FROM mailbox_access`); n != 0 {
		t.Fatalf("a waiting link stored %d rows of a mailbox", n)
	}
	if got := f.row(link.ID); got != waiting(false) {
		t.Errorf("the waiting link holds %+v", got)
	}

	f.names(provider.KindGmail, "Ana.Doe@gmail.com")
	key := authtest.LinkKey(t)
	created, err := f.complete(flow, ana.ID, account.LinkCompletion{Key: &key, LinkedBy: ana.ID})
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "Ana.Doe@gmail.com" || created.LoginUser != "Ana.Doe@gmail.com" ||
		created.Provider != provider.KindGmail || created.AuthKind != "oauth2" || created.OAuthClient != account.ClientWeb ||
		created.State != account.StateActive || created.DisplayName != "Work" ||
		created.WorkspaceID != authtest.Personal(t, f.db, ana.ID) || created.OwnerUserID != ana.ID ||
		created.LinkedBy != ana.ID || created.IMAPHost != "imap.gmail.com" || created.SMTPHost != "smtp.gmail.com" {
		t.Errorf("the mailbox the link created: %+v", created)
	}
	stored, err := f.repo.Get(t.Context(), created.ID)
	if err != nil || stored.State != account.StateActive || !stored.Connected {
		t.Errorf("the mailbox as stored: %+v, %v", stored, err)
	}
	if token, err := f.repo.Token(t.Context(), created.ID); err != nil || token.RefreshToken == "" ||
		token.Extra("id_token") != nil {
		t.Errorf("its grant: %+v, %v", token, err)
	}
	pair, err := f.ws.CurrentKey(t.Context(), created.ID)
	if err != nil || !bytes.Equal(pair.PublicKey, key.PublicKey) || pair.Namespace != key.Namespace || pair.Epoch != 1 {
		t.Errorf("its key: %+v, %v", pair, err)
	}
	if state, err := f.ws.KeyState(t.Context(), created.ID, ana.ID); err != nil || !state.Reads ||
		!bytes.Equal(state.Grant, key.Grant) {
		t.Errorf("Ana's grant: %+v, %v", state, err)
	}
	// One login, as the address the provider named.
	if len(f.imap.AuthAttempts()) == 0 {
		t.Error("the grant was stored without a login")
	}
	exchange := f.idp.exchange(0)
	if exchange.Get("redirect_uri") != publicURL+"/oauth/return" || exchange.Get("code_verifier") == "" ||
		exchange.Get("client_id") != googleClient || exchange.Get("code") != "the-code" {
		t.Errorf("the exchange: %v", exchange)
	}
	// The link names the mailbox and keeps nothing else: no address, no
	// verifier, no name.
	if got := f.row(link.ID); got != connectedTo(created.ID) {
		t.Errorf("the connected link holds %+v", got)
	}
	if got, err := f.repo.Link(t.Context(), link.ID, ana.ID); err != nil || got.State != account.LinkConnected ||
		got.AccountID != created.ID || got.Email != "" || got.Reason != "" {
		t.Errorf("Ana reads the link as %+v, %v", got, err)
	}
}

func TestALinkIntoATeamRecordsTheTeamsConsentAndTheTenant(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	link, flow := f.start(ana.ID, provider.KindMicrosoft, team.ID)
	if link.WorkspaceID != team.ID {
		t.Errorf("the link goes into %q", link.WorkspaceID)
	}
	f.names(provider.KindMicrosoft, "support@outlook.com")
	created, err := f.complete(flow, ana.ID, account.LinkCompletion{
		SyncConsent: store.MailboxConsent{At: 1, By: ana.ID, Version: "sync-1"}, LinkedBy: ana.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceID != team.ID || created.OwnerUserID != "" || created.Tenant != "common" ||
		created.SyncConsent.Version != "sync-1" || created.SyncConsent.By != ana.ID ||
		created.SyncConsent.At != f.clock.Unix() || created.IMAPHost != "outlook.office365.com" {
		t.Errorf("the team's mailbox: %+v", created)
	}
	// Named as the person's own workspace, a link is theirs, resolved when
	// its mailbox is created.
	own, _ := f.start(ana.ID, provider.KindGmail, authtest.Personal(t, f.db, ana.ID))
	if own.WorkspaceID != "" || f.count(`SELECT count(*) FROM oauth_links WHERE id = ? AND workspace_id IS NULL`, own.ID) != 1 {
		t.Errorf("a link into the personal workspace names %q", own.WorkspaceID)
	}
	// Nobody else's personal workspace, nor the operator's.
	bea := f.person("bea@example.test")
	for what, into := range map[string]string{
		"Bea's personal workspace": authtest.Personal(t, f.db, bea.ID), "the operator's": workspace.OperatorID,
		"a workspace nobody knows": "wsp_0000000000000000",
	} {
		if _, _, err := f.registry.StartLink(t.Context(), account.LinkStart{
			Provider: provider.KindGmail, OwnerUserID: ana.ID, WorkspaceID: into,
		}); !errors.Is(err, account.ErrNoWorkspace) {
			t.Errorf("a link into %s: %v, want ErrNoWorkspace", what, err)
		}
	}
	// Nor a closed team.
	if _, _, err := f.ws.SetClosed(t.Context(), team.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.registry.StartLink(t.Context(), account.LinkStart{
		Provider: provider.KindGmail, OwnerUserID: ana.ID, WorkspaceID: team.ID,
	}); !errors.Is(err, workspace.ErrClosed) {
		t.Errorf("a link into a closed team: %v, want ErrClosed", err)
	}
}

func TestALinkIsTakenOnlyByItsStarterAndOnlyOnce(t *testing.T) {
	f := newLinkFixture(t)
	ana, bea := f.person("ana@example.test"), f.person("bea@example.test")
	link, flow := f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "ana@gmail.com")

	// Bea holding Ana's return finds nothing, and changes nothing: the code
	// is never exchanged, and the link stays Ana's to finish.
	if _, err := f.complete(flow, bea.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("Bea completing Ana's link: %v, want ErrNotFound", err)
	}
	if _, err := f.registry.OwnedLink(t.Context(), flow.State, bea.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("Bea reading Ana's link by its state: %v, want ErrNotFound", err)
	}
	if _, err := f.repo.Link(t.Context(), link.ID, bea.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("Bea reading Ana's link: %v, want ErrNotFound", err)
	}
	if _, err := f.registry.OwnedLink(t.Context(), "another-state", ana.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("a state nobody knows: %v, want ErrNotFound", err)
	}
	if n := f.idp.exchangeCount(); n != 0 {
		t.Fatalf("the code was exchanged %d times for somebody else", n)
	}
	pending, err := f.registry.OwnedLink(t.Context(), flow.State, ana.ID)
	if err != nil || pending.ID != link.ID || pending.OwnerUserID != ana.ID || pending.Provider != provider.KindGmail ||
		pending.DisplayName != "Work" || pending.WorkspaceID != "" {
		t.Fatalf("Ana reads her link as %+v, %v", pending, err)
	}
	// A return without the mailbox's key is refused before the take.
	if _, err := f.registry.CompleteLink(t.Context(), account.Redirect{Code: "the-code", State: flow.State}, ana.ID,
		account.LinkCompletion{}); !errors.Is(err, account.ErrNoLinkKey) {
		t.Errorf("a return without a key: %v, want ErrNoLinkKey", err)
	}
	if got, err := f.repo.Link(t.Context(), link.ID, ana.ID); err != nil || got.State != account.LinkWaiting ||
		f.idp.exchangeCount() != 0 {
		t.Fatalf("after a return without a key, the link is %+v (%v), exchanged %d times", got, err, f.idp.exchangeCount())
	}

	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); err != nil {
		t.Fatalf("Ana completing her link: %v", err)
	}
	// Once taken, no return takes it again.
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the same return again: %v, want ErrNotFound", err)
	}
	if _, err := f.registry.OwnedLink(t.Context(), flow.State, ana.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("reading a finished link by its state: %v, want ErrNotFound", err)
	}
	if n := f.idp.exchangeCount(); n != 1 || f.count(`SELECT count(*) FROM accounts`) != 1 {
		t.Errorf("the code was exchanged %d times and %d mailboxes stored", n, f.count(`SELECT count(*) FROM accounts`))
	}
}

func TestADeclinedLinkIsRecordedWithoutAKey(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	link, flow := f.start(ana.ID, provider.KindMicrosoft, "")
	_, err := f.registry.CompleteLink(t.Context(), account.Redirect{Error: "access_denied", State: flow.State}, ana.ID,
		account.LinkCompletion{})
	if !errors.Is(err, account.ErrConsentDeclined) {
		t.Fatalf("a declined link: %v, want ErrConsentDeclined", err)
	}
	if got := f.row(link.ID); got != failed(account.ReasonDeclined, "", true) {
		t.Errorf("the declined link holds %+v", got)
	}
	if n := f.idp.exchangeCount() + f.count(`SELECT count(*) FROM accounts`); n != 0 {
		t.Errorf("a declined link exchanged or stored %d things", n)
	}
}

func TestALinkToAMailboxTheWorkspaceHoldsUnderAnySpellingStoresNothing(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	own := authtest.Personal(t, f.db, ana.ID)
	held, err := f.repo.Create(t.Context(), account.Account{
		ID: "acc_jane", Email: "jane.doe@gmail.com", Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "jane.doe@gmail.com", WorkspaceID: own,
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := f.repo.Create(t.Context(), account.Account{
		ID: "acc_old", Email: "old@example.test", Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "old@example.test", WorkspaceID: own,
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	removeBySQL(t, f.db, removed.ID)

	for _, c := range []struct {
		named string
		want  error
	}{
		{"JaneDoe+news@googlemail.com", account.ErrDuplicate},
		{"OLD@example.test", account.ErrRemovedDuplicate},
	} {
		link, flow := f.start(ana.ID, provider.KindGmail, "")
		f.names(provider.KindGmail, c.named)
		if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, c.want) {
			t.Errorf("a link naming %s: %v, want %v", c.named, err, c.want)
		}
		// One reason for both, with the address the provider named.
		if got := f.row(link.ID); got != failed(account.ReasonDuplicate, c.named, true) {
			t.Errorf("the link naming %s holds %+v", c.named, got)
		}
		if got, err := f.repo.Link(t.Context(), link.ID, ana.ID); err != nil || got.State != account.LinkFailed ||
			got.Email != c.named || got.Reason != account.ReasonDuplicate || got.AccountID != "" {
			t.Errorf("Ana reads the link naming %s as %+v, %v", c.named, got, err)
		}
	}
	// Nothing was stored, and no mail server dialled to find out.
	if n := f.count(`SELECT count(*) FROM accounts`); n != 2 || len(f.imap.AuthAttempts()) != 0 {
		t.Errorf("%d mailboxes, %d logins", n, len(f.imap.AuthAttempts()))
	}
	if _, err := f.repo.Get(t.Context(), held.ID); err != nil {
		t.Errorf("the mailbox held: %v", err)
	}

	// The same account is another mailbox in another workspace.
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, flow := f.start(ana.ID, provider.KindGmail, team.ID)
	f.names(provider.KindGmail, "jane.doe@gmail.com")
	if created, err := f.complete(flow, ana.ID, account.LinkCompletion{}); err != nil || created.WorkspaceID != team.ID {
		t.Errorf("the same account into a team: %+v, %v", created, err)
	}
}

func TestALinkWithoutAUsableAddressOrALoginStoresNothing(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")

	link, flow := f.start(ana.ID, provider.KindGmail, "")
	f.idp.set(func(i *linkIDP) { i.claims = with(googleClaims("ana@gmail.com"), map[string]any{"email": nil}) })
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrNoAddress) {
		t.Errorf("an answer naming no address: %v, want ErrNoAddress", err)
	}
	if got := f.row(link.ID); got != failed(account.ReasonNoAddress, "", true) {
		t.Errorf("the link that learned no address holds %+v", got)
	}

	// A mail server that refuses the grant: the address is recorded.
	link, flow = f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "ana@gmail.com")
	f.idp.set(func(i *linkIDP) { i.accept = nil })
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrMailboxRefused) {
		t.Errorf("a grant the mail server refuses: %v, want ErrMailboxRefused", err)
	}
	if got := f.row(link.ID); got != failed(account.ReasonMailboxRefused, "ana@gmail.com", true) {
		t.Errorf("the refused link holds %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM accounts`) + f.count(`SELECT count(*) FROM credentials`); n != 0 {
		t.Errorf("%d rows of a mailbox stored", n)
	}
}

func TestALinkThatEndsWhileItFinishesCreatesNothing(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	users := auth.NewUsers(f.db)
	f.registry.CheckOwnersWith(users.RequireActiveTx)

	// Its starter signs out everywhere while the code is exchanged: the
	// link goes, and the transaction that would create the mailbox finds
	// none to finish.
	link, flow := f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "ana@gmail.com")
	f.idp.set(func(i *linkIDP) {
		i.onExchange = func() {
			if err := users.EndAllSessions(context.Background(), ana.ID); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrFlowExpired) {
		t.Errorf("a link that ended while it finished: %v, want ErrFlowExpired", err)
	}
	if got := f.row(link.ID); got != gone {
		t.Errorf("the ended link: %+v", got)
	}

	// Its starter disabled meanwhile: the transaction refuses.
	link, flow = f.start(ana.ID, provider.KindGmail, "")
	f.idp.set(func(i *linkIDP) {
		i.onExchange = func() {
			if _, err := users.Disable(context.Background(), ana.ID, false); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrOwnerInactive) {
		t.Errorf("a link whose starter was disabled while it finished: %v, want ErrOwnerInactive", err)
	}
	if got := f.row(link.ID); got != failed(account.ReasonFailed, "ana@gmail.com", true) {
		t.Errorf("the refused link holds %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM accounts`) + f.count(`SELECT count(*) FROM credentials`) +
		f.count(`SELECT count(*) FROM mailbox_keys`); n != 0 {
		t.Errorf("%d rows of a mailbox stored", n)
	}
}

func TestALinkFinishesWhenItsCallerHangsUpAfterTheCodeWasTaken(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	link, flow := f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "ana@gmail.com")
	ctx, cancel := context.WithCancel(t.Context())
	f.idp.set(func(i *linkIDP) { i.onExchange = cancel })
	key := authtest.LinkKey(t)
	created, err := f.registry.CompleteLink(ctx, account.Redirect{Code: "the-code", State: flow.State}, ana.ID,
		account.LinkCompletion{Key: &key, Check: func(ctx context.Context, _ *sql.Tx) error { return ctx.Err() }})
	if err != nil || created.ID == "" {
		t.Fatalf("a link whose caller hung up: %+v, %v", created, err)
	}
	if got := f.row(link.ID); got != connectedTo(created.ID) {
		t.Errorf("the link holds %+v", got)
	}
}

func TestAnUnansweredLinkExpiresAndIsForgottenTenMinutesLater(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	start := *f.clock
	at := func(d time.Duration) { *f.clock = start.Add(d) }
	read := func(id string) account.Link {
		t.Helper()
		l, err := f.repo.Link(t.Context(), id, ana.ID)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		return l
	}

	byState, flowA := f.start(ana.ID, provider.KindGmail, "")
	byReturn, flowB := f.start(ana.ID, provider.KindMicrosoft, "")
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	bySweep, _ := f.start(ana.ID, provider.KindGmail, team.ID)

	at(10 * time.Minute)
	if l := read(byState.ID); l.State != account.LinkWaiting {
		t.Errorf("at its expiry: %+v", l)
	}
	if _, err := f.registry.OwnedLink(t.Context(), flowA.State, ana.ID); err != nil {
		t.Errorf("read at its expiry: %v", err)
	}
	at(10*time.Minute + time.Second)
	if l := read(bySweep.ID); l.State != account.LinkFailed || l.Reason != account.ReasonExpired {
		t.Errorf("a second past its expiry it reads %+v", l)
	}
	if _, err := f.registry.OwnedLink(t.Context(), flowA.State, ana.ID); !errors.Is(err, account.ErrFlowExpired) {
		t.Errorf("read past its expiry: %v, want ErrFlowExpired", err)
	}
	if _, err := f.complete(flowB, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrFlowExpired) {
		t.Errorf("returned past its expiry: %v, want ErrFlowExpired", err)
	}
	if n := f.idp.exchangeCount(); n != 0 {
		t.Errorf("an expired link's code was exchanged %d times", n)
	}
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{byState.ID, byReturn.ID, bySweep.ID} {
		if got := f.row(id); got != failed(account.ReasonExpired, "", false) {
			t.Errorf("the expired link %s holds %+v", id, got)
		}
	}

	// A link a restart cut off while it finished: taken, never finished.
	cut, _ := f.start(ana.ID, provider.KindGmail, "")
	exec(t, f.db, `UPDATE oauth_links SET taken_at = ? WHERE id = ?`, f.clock.Unix(), cut.ID)
	at(10*time.Minute + time.Second + account.FinishTimeout)
	if l := read(cut.ID); l.State != account.LinkFinishing {
		t.Errorf("while its finish could still be running it reads %+v", l)
	}
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.row(cut.ID); got != waiting(true) {
		t.Errorf("the sweep ended a link that may still finish: %+v", got)
	}
	at(10*time.Minute + 2*time.Minute)
	if l := read(cut.ID); l.State != account.LinkFailed || l.Reason != account.ReasonExpired {
		t.Errorf("once its finish is over it reads %+v", l)
	}
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.row(cut.ID); got != failed(account.ReasonExpired, "", true) {
		t.Errorf("the link cut off holds %+v", got)
	}

	// Kept ten minutes past its expiry, for its starter's console, and then
	// forgotten.
	at(20 * time.Minute)
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.row(byState.ID); got == gone {
		t.Error("forgotten before ten minutes past its expiry")
	}
	at(20*time.Minute + time.Second)
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{byState.ID, byReturn.ID, bySweep.ID} {
		if _, err := f.repo.Link(t.Context(), id, ana.ID); !errors.Is(err, account.ErrNotFound) {
			t.Errorf("%s ten minutes past its expiry: %v, want ErrNotFound", id, err)
		}
	}
	if got := f.row(cut.ID); got == gone {
		t.Error("the link started later went with the earlier ones")
	}
}

func TestAPersonHasAtMostFiveLinksWaitingAndANewOneSupersedesTheSameOne(t *testing.T) {
	f := newLinkFixture(t)
	ana, bea := f.person("ana@example.test"), f.person("bea@example.test")
	first, firstFlow := f.start(ana.ID, provider.KindGmail, "")
	*f.clock = f.clock.Add(time.Minute)
	again, _ := f.start(ana.ID, provider.KindGmail, "")
	if got := f.row(first.ID); got != gone {
		t.Errorf("the link replaced holds %+v", got)
	}
	if _, err := f.registry.OwnedLink(t.Context(), firstFlow.State, ana.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the replaced link's return: %v, want ErrNotFound", err)
	}
	if l, err := f.repo.Link(t.Context(), again.ID, ana.ID); err != nil || l.State != account.LinkWaiting {
		t.Errorf("the link replacing it: %+v, %v", l, err)
	}
	// Another provider, or another workspace, is another link.
	f.start(ana.ID, provider.KindMicrosoft, "")
	var teams []string
	for _, name := range []string{"One", "Two", "Three", "Four"} {
		team, err := f.ws.CreateTeam(t.Context(), name, ana.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		teams = append(teams, team.ID)
	}
	for _, team := range teams[:3] {
		f.start(ana.ID, provider.KindGmail, team)
	}
	_, _, err := f.registry.StartLink(t.Context(), account.LinkStart{
		Provider: provider.KindGmail, OwnerUserID: ana.ID, WorkspaceID: teams[3],
	})
	var tooMany *account.TooManyLinksError
	if !errors.Is(err, account.ErrTooManyLinks) || !errors.As(err, &tooMany) ||
		!tooMany.Until.Equal(again.ExpiresAt.Add(time.Second)) {
		t.Fatalf("a sixth link: %v, want ErrTooManyLinks until a second past %s", err, again.ExpiresAt)
	}
	// Replacing one of the five is no sixth; nor is anybody else's.
	f.start(ana.ID, provider.KindGmail, teams[0])
	f.start(bea.ID, provider.KindGmail, "")
	// A link that ended, or ran out of time, no longer waits.
	_, ended := f.start(ana.ID, provider.KindGmail, teams[1])
	if _, err := f.registry.CompleteLink(t.Context(), account.Redirect{Error: "access_denied", State: ended.State}, ana.ID,
		account.LinkCompletion{}); !errors.Is(err, account.ErrConsentDeclined) {
		t.Fatal(err)
	}
	f.start(ana.ID, provider.KindGmail, teams[3])
	*f.clock = f.clock.Add(10*time.Minute + time.Second)
	for _, team := range teams {
		f.start(ana.ID, provider.KindMicrosoft, team)
	}
	if n := f.count(`SELECT count(*) FROM oauth_links WHERE owner_user_id = ? AND taken_at = 0 AND finished_at = 0
		AND expires_at >= ?`, ana.ID, f.clock.Unix()); n != 4 {
		t.Errorf("Ana has %d links waiting, want the four just started", n)
	}
}

func TestALinkGoesWithItsPersonAndItsWorkspace(t *testing.T) {
	f := newLinkFixture(t)
	ana, bea, cid := f.person("ana@example.test"), f.person("bea@example.test"), f.person("cid@example.test")
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	beasTeam, err := f.ws.CreateTeam(t.Context(), "Bea's", bea.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownAna, _ := f.start(ana.ID, provider.KindGmail, "")
	teamAna, _ := f.start(ana.ID, provider.KindGmail, team.ID)
	ownBea, _ := f.start(bea.ID, provider.KindGmail, "")
	teamBea, _ := f.start(bea.ID, provider.KindGmail, beasTeam.ID)
	ownCid, _ := f.start(cid.ID, provider.KindGmail, "")

	// Disabled or suspended, a person's links end with their consent
	// attempts.
	if err := f.registry.StopFlowsBy(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	// Closed, they go with the person and the team that goes with them.
	users := auth.NewUsers(f.db)
	if _, err := f.registry.RemoveOwner(t.Context(), bea.ID, nil, func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, bea.ID, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ownAna.ID, teamAna.ID, ownBea.ID, teamBea.ID} {
		if got := f.row(id); got != gone {
			t.Errorf("link %s holds %+v", id, got)
		}
	}
	if n := f.count(`SELECT count(*) FROM workspaces WHERE id = ?`, beasTeam.ID); n != 0 {
		t.Error("Bea's team did not go with her")
	}
	if got := f.row(ownCid.ID); got != waiting(false) {
		t.Errorf("Cid's link holds %+v", got)
	}
}

func TestAPurgedMailboxForgetsTheFailedLinksNamingItsAddress(t *testing.T) {
	f := newLinkFixture(t)
	ana := f.person("ana@example.test")
	connected, flow := f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "jane.doe@gmail.com")
	mailbox, err := f.complete(flow, ana.ID, account.LinkCompletion{})
	if err != nil {
		t.Fatal(err)
	}
	// Two attempts it refused, under another spelling and the same one, and
	// one that failed for another address.
	var refused []string
	for _, named := range []string{"JaneDoe+x@googlemail.com", "Jane.Doe@gmail.com"} {
		link, flow := f.start(ana.ID, provider.KindGmail, "")
		f.names(provider.KindGmail, named)
		if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrDuplicate) {
			t.Fatalf("a link naming %s: %v", named, err)
		}
		refused = append(refused, link.ID)
	}
	other, flow := f.start(ana.ID, provider.KindGmail, "")
	f.names(provider.KindGmail, "other@gmail.com")
	f.idp.set(func(i *linkIDP) { i.accept = nil })
	if _, err := f.complete(flow, ana.ID, account.LinkCompletion{}); !errors.Is(err, account.ErrMailboxRefused) {
		t.Fatal(err)
	}

	if err := f.repo.Delete(t.Context(), mailbox.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range refused {
		if got := f.row(id); got != gone {
			t.Errorf("a failed link naming the mailbox purged holds %+v", got)
		}
	}
	if got := f.row(connected.ID); got != connectedTo("") {
		t.Errorf("the link that connected the mailbox purged holds %+v", got)
	}
	if got, err := f.repo.Link(t.Context(), connected.ID, ana.ID); err != nil || got.AccountID != "" || got.Email != "" {
		t.Errorf("Ana reads it as %+v, %v", got, err)
	}
	if got := f.row(other.ID); got != failed(account.ReasonMailboxRefused, "other@gmail.com", true) {
		t.Errorf("the failed link naming another address holds %+v", got)
	}
}
