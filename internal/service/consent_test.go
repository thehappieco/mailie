package service_test

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// fakeIDP is a token endpoint — and a device endpoint — that records every
// request, so a test can prove a code was never exchanged rather than merely
// that the call failed.
type fakeIDP struct {
	server *httptest.Server

	mu        sync.Mutex
	exchanges []url.Values
	refreshes []url.Values
	polls     int
	issued    int
	// expiresIn is the lifetime of issued access tokens, in seconds.
	expiresIn int
	// deviceAnswer is what a device-code poll gets while it is set.
	deviceAnswer string
	// onIssue sees every access token handed out, so an IMAP server can be
	// told to accept it.
	onIssue func(token string)
	// accept is the fixture's own onIssue: it tells the server standing in
	// for Gmail and Microsoft to take every token issued, so a test that sets
	// onIssue for its own purposes does not make consent fail by accident.
	accept func(token string)
	// scope, when set, is the token response's scope field: what the
	// provider says the token is valid for.
	scope string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{expiresIn: 3600, deviceAnswer: "authorization_pending"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", idp.token)
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "device-code", "user_code": "WXYZ-1234",
			"verification_uri": "https://idp.example/device", "expires_in": 600, "interval": 1,
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (f *fakeIDP) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{
		AuthURL:       f.server.URL + "/authorize",
		TokenURL:      f.server.URL + "/token",
		DeviceAuthURL: f.server.URL + "/device",
	}
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		f.exchanges = append(f.exchanges, r.Form)
		switch r.Form.Get("code") {
		case "refused":
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		case "wrong-client":
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
			return
		}
	case "refresh_token":
		f.refreshes = append(f.refreshes, r.Form)
	case "urn:ietf:params:oauth:grant-type:device_code":
		f.polls++
		if f.deviceAnswer != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": f.deviceAnswer})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	f.issued++
	access := "access-" + strconv.Itoa(f.issued)
	if f.accept != nil {
		f.accept(access)
	}
	if f.onIssue != nil {
		f.onIssue(access)
	}
	response := map[string]any{
		"access_token": access, "refresh_token": "refresh-" + strconv.Itoa(f.issued),
		"token_type": "Bearer", "expires_in": f.expiresIn,
	}
	if f.scope != "" {
		response["scope"] = f.scope
	}
	writeJSON(w, http.StatusOK, response)
}

func (f *fakeIDP) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchanges)
}

func (f *fakeIDP) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// exchange and refresh return one recorded request, and the counts how many
// there were; the handler writes them on the server's goroutines.
func (f *fakeIDP) exchange(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges[i]
}

func (f *fakeIDP) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refreshes)
}

func (f *fakeIDP) refresh(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes[i]
}

func (f *fakeIDP) set(change func(*fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// consentFixture is a daemon whose installed and web clients all talk to one
// fake identity provider, and whose Gmail and Microsoft accounts log in to one
// in-process IMAP server that takes every token that provider issues —
// finishing a consent logs in once, to prove the grant opens the mailbox.
func consentFixture(t *testing.T, publicURL string) (*fixture, *fakeIDP) {
	t.Helper()
	idp := newFakeIDP(t)
	standIn := providertest.NewIMAPServer(t, providertest.IMAPOptions{AnyUser: true})
	idp.accept = standIn.AcceptToken
	f := newFixtureWith(t, fixtureOptions{
		publicURL:    publicURL,
		providerIMAP: standIn.Addr,
		registry: account.RegistryOptions{
			Google:       account.OAuthClient{ClientID: "google-installed", Endpoint: idp.endpoint()},
			Microsoft:    account.OAuthClient{ClientID: "ms-installed", Tenant: "common", Endpoint: idp.endpoint()},
			GoogleWeb:    account.OAuthClient{ClientID: "google-web", ClientSecret: "google-web-secret", Endpoint: idp.endpoint()},
			MicrosoftWeb: account.OAuthClient{ClientID: "ms-web", ClientSecret: "ms-web-secret", Endpoint: idp.endpoint()},
			DeviceCode:   true,
		},
	})
	return f, idp
}

func stateOf(t *testing.T, flow *service.AuthFlow) string {
	t.Helper()
	if flow == nil || flow.AuthURL == "" {
		t.Fatalf("no authorization URL in %+v", flow)
	}
	u, err := url.Parse(flow.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func (f *fixture) stateOfAccount(t *testing.T, id string) account.Account {
	t.Helper()
	a, err := f.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// eventually polls until cond holds, for the outcomes a background flow
// records on its own time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAUserCannotCompleteAnotherUsersOAuthFlow(t *testing.T) {
	// The consent-phishing case: somebody starts a flow under their own
	// console account and sends the provider's page to a victim. Whoever
	// holds the redirect, only the person who started the flow can redeem
	// it — and a refusal must neither exchange the code nor use up the flow.
	f, idp := consentFixture(t, "https://console.mailie.example")
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	mallory := f.person(t, "mallory@example.com", auth.RoleOwner)

	added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if added.Auth.Flow != "web" {
		t.Fatalf("flow = %q, want web for a hosted console", added.Auth.Flow)
	}
	redirect := "https://console.mailie.example/oauth/return?code=the-code&state=" + stateOf(t, added.Auth)

	for name, p := range map[string]service.Principal{"another user": mallory, "an instance key": admin()} {
		_, err := f.svc.CompleteOAuth(t.Context(), p, redirect)
		if service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("%s completing ana's flow: %v, want not_found", name, err)
		}
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("the code was exchanged %d times for callers who did not start the flow", n)
	}

	// The flow is still there for the person it belongs to.
	a, err := f.svc.CompleteOAuth(t.Context(), ana, redirect)
	if err != nil {
		t.Fatalf("ana could not finish her own flow after the refusals: %v", err)
	}
	if a.State != "active" || idp.exchangeCount() != 1 {
		t.Fatalf("state = %q after %d exchanges", a.State, idp.exchangeCount())
	}
	exchanged := idp.exchange(0)
	for field, want := range map[string]string{
		"client_id": "google-web", "client_secret": "google-web-secret",
		"redirect_uri": "https://console.mailie.example/oauth/return", "code": "the-code",
	} {
		if got := exchanged.Get(field); got != want {
			t.Errorf("exchange %s = %q, want %q", field, got, want)
		}
	}
	if exchanged.Get("code_verifier") == "" {
		t.Error("the exchange carried no PKCE verifier")
	}
	if stored := f.stateOfAccount(t, a.ID); stored.OAuthClient != account.ClientWeb || stored.OwnerUserID != ana.UserID {
		t.Errorf("stored client %q owner %q, want the web client and ana", stored.OAuthClient, stored.OwnerUserID)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana, redirect); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a replayed redirect: %v, want not_found", err)
	}
}

func TestAWebFlowRedirectsToThePublicOrigin(t *testing.T) {
	for _, origin := range []string{"https://console.mailie.example", "http://localhost:5174"} {
		f, _ := consentFixture(t, origin)
		ana := f.person(t, "ana@example.com", auth.RoleOwner)
		for address, client := range map[string]string{"ana@gmail.com": "google-web", "ana@outlook.com": "ms-web"} {
			added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: address}))
			if err != nil {
				t.Fatalf("%s %s: %v", origin, address, err)
			}
			if added.Auth.Flow != "web" {
				t.Errorf("%s %s: flow %q, want web whenever a web client is configured", origin, address, added.Auth.Flow)
			}
			u, err := url.Parse(added.Auth.AuthURL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			for field, want := range map[string]string{
				// An SPA route of the console, never the loopback path.
				"redirect_uri": origin + "/oauth/return",
				"client_id":    client,
				// The mailbox being connected, so a browser signed in as
				// somebody else is not quietly offered that person.
				"login_hint":            address,
				"code_challenge_method": "S256",
				"access_type":           "offline",
			} {
				if got := q.Get(field); got != want {
					t.Errorf("%s %s: %s = %q, want %q", origin, address, field, got, want)
				}
			}
		}
	}
}

func TestADeniedConsentMarksTheAccountFailed(t *testing.T) {
	f, idp := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	// Through the console: the provider sends the browser back with error=.
	web, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "web@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	denied := "http://localhost:5174/oauth/return?error=access_denied&error_description=The+user+said+no&state=" +
		stateOf(t, web.Auth)
	_, err = f.svc.CompleteOAuth(t.Context(), ana, denied)
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a declined consent answered %v, want bad_request", err)
	}
	if a := f.stateOfAccount(t, web.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonDeclined {
		t.Errorf("web account = %s %q, want error %q", a.State, a.StateReason, account.ReasonDeclined)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana, denied); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the declined flow could be answered twice: %v", err)
	}

	// A code the provider refuses to exchange.
	refused, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "refused@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code=refused&state="+stateOf(t, refused.Auth))
	if service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a refused exchange answered %v", err)
	}
	if a := f.stateOfAccount(t, refused.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonRefused {
		t.Errorf("refused account = %s %q", a.State, a.StateReason)
	}

	// This server's own client refused: the operator's problem, said as
	// such, and no reason to ask the person to consent differently.
	rejected, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "rejected@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code=wrong-client&state="+stateOf(t, rejected.Auth))
	if service.CodeOf(err) != service.CodeInternal || !strings.Contains(service.MessageOf(err), "operator") {
		t.Fatalf("a rejected client answered %v", err)
	}
	if a := f.stateOfAccount(t, rejected.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonClientRejected {
		t.Errorf("rejected-client account = %s %q", a.State, a.StateReason)
	}

	// Through a loopback listener: the page says it failed, and so does the
	// account, by the time the page is shown.
	loop, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "loop@gmail.com", Flow: "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	resp := getRedirect(t, loop.Auth, url.Values{"error": {"access_denied"}, "state": {stateOf(t, loop.Auth)}})
	if resp.status != http.StatusBadRequest || !strings.Contains(resp.body, "Authorization failed") {
		t.Errorf("loopback page = %d %q", resp.status, resp.body)
	}
	if a := f.stateOfAccount(t, loop.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonDeclined {
		t.Errorf("loopback account = %s %q", a.State, a.StateReason)
	}

	// Through the device flow, which only the daemon is polling.
	idp.set(func(f *fakeIDP) { f.deviceAnswer = "access_denied" })
	device, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "device@outlook.com", Flow: "device"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the declined device flow to be recorded", func() bool {
		return f.stateOfAccount(t, device.Account.ID).State == account.StateError
	})
	if a := f.stateOfAccount(t, device.Account.ID); a.StateReason != account.ReasonDeclined {
		t.Errorf("device account reason = %q", a.StateReason)
	}
	if n := idp.exchangeCount(); n != 2 {
		t.Errorf("%d code exchanges, want only the refused one and the rejected one", n)
	}

	// Starting again puts the account back to waiting for consent.
	if _, err := f.svc.StartOAuth(t.Context(), ana, web.Account.ID, ""); err != nil {
		t.Fatal(err)
	}
	if a := f.stateOfAccount(t, web.Account.ID); a.State != account.StatePendingAuth || a.StateReason != "" {
		t.Errorf("a restarted account = %s %q, want pending_auth", a.State, a.StateReason)
	}
}

func TestAProviderFailureOnTheRedirectIsNotRecordedAsADecline(t *testing.T) {
	// A misconfigured registration or a provider outage comes back on the
	// redirect too. Recording either as "consent was declined" tells the
	// person they said no and hides a broken configuration from its operator.
	f, idp := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	for _, c := range []struct {
		error, reason string
		code          service.Code
	}{
		{"unauthorized_client", account.ReasonClientRejected, service.CodeInternal},
		{"invalid_scope", account.ReasonClientRejected, service.CodeInternal},
		{"server_error", account.ReasonRefused, service.CodeBadRequest},
	} {
		added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: c.error + "@gmail.com"}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.CompleteOAuth(t.Context(), ana, "http://localhost:5174/oauth/return?error="+c.error+
			"&error_description=Something+went+wrong&state="+stateOf(t, added.Auth))
		if service.CodeOf(err) != c.code || strings.Contains(service.MessageOf(err), "declined") {
			t.Errorf("error=%s answered %s %q, want %s", c.error, service.CodeOf(err), service.MessageOf(err), c.code)
		}
		if strings.Contains(service.MessageOf(err), "Something went wrong") {
			t.Errorf("error=%s: the answer quotes the redirect: %q", c.error, service.MessageOf(err))
		}
		if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateError || a.StateReason != c.reason {
			t.Errorf("error=%s left the account %s %q, want error %q", c.error, a.State, a.StateReason, c.reason)
		}
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Errorf("%d code exchanges for redirects that carried no code", n)
	}
}

func TestAnAccountWhoseFirstConsentFailedHasNoFoldersToList(t *testing.T) {
	// It is in error with no grant ever stored. Asking for its folders is a
	// question about an account nobody authorised, not a mail server that
	// could not be reached, and not a 500.
	f, _ := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	declined, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "declined@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?error=access_denied&state="+stateOf(t, declined.Auth)); err == nil {
		t.Fatal("a declined consent completed")
	}
	abandoned, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "abandoned@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	f.repo.WithClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}

	for name, id := range map[string]string{"declined": declined.Account.ID, "expired": abandoned.Account.ID} {
		if a := f.stateOfAccount(t, id); a.State != account.StateError {
			t.Fatalf("%s: account is %s, want error", name, a.State)
		}
		_, err := f.svc.ListFolders(t.Context(), ana, id)
		if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "not been authorised") {
			t.Errorf("%s: folders answered %s %q, want conflict", name, service.CodeOf(err), service.MessageOf(err))
		}
	}
}

func TestAnAttemptNobodyFinishesIsRecordedAsExpired(t *testing.T) {
	// A web flow has nothing on the daemon waiting for it: if the person
	// never comes back, only the sweep notices.
	f, _ := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	f.repo.WithClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if err := f.registry.SweepFlows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonExpired {
		t.Errorf("account = %s %q, want error %q", a.State, a.StateReason, account.ReasonExpired)
	}
}

func TestAStrayCallbackDoesNotEndTheLoopbackFlow(t *testing.T) {
	// Before, the first request to reach the listener ended the flow
	// whatever its state, so one stale tab — or anything that fired the URL
	// on purpose — could close it while the real consent was on its way.
	f, idp := consentFixture(t, "")
	added, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "person@gmail.com", Flow: "loopback"})
	if err != nil {
		t.Fatal(err)
	}

	stray := getRedirect(t, added.Auth, url.Values{"code": {"forged"}, "state": {"not-this-flow"}})
	if stray.status != http.StatusBadRequest || !strings.Contains(stray.body, "does not belong") {
		t.Errorf("stray page = %d %q", stray.status, stray.body)
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("a stray code was exchanged %d times", n)
	}

	real := getRedirect(t, added.Auth, url.Values{"code": {"the-code"}, "state": {stateOf(t, added.Auth)}})
	if real.status != http.StatusOK || real.body != "Authorization complete. You can close this tab.\n" {
		t.Fatalf("the real redirect after a stray one: %d %q", real.status, real.body)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateActive || a.OAuthClient != account.ClientInstalled {
		t.Errorf("account = %s with client %q", a.State, a.OAuthClient)
	}
	if n := idp.exchangeCount(); n != 1 || idp.exchange(0).Get("code") != "the-code" {
		t.Errorf("%d exchanges, want the real code once", n)
	}
}

func TestALoopbackFlowDoesNotExpireAtOnceUnderAClockHeldInThePast(t *testing.T) {
	// A flow's expiry is an instant on the registry's clock, which a test
	// holds still. Its listener used to wait for that instant on the real
	// clock — long gone — so every attempt expired the moment it started, on
	// a goroutine of its own racing the request that started it, and the
	// events a test journaled depended on which won and on the date it ran.
	f, _ := consentFixture(t, "")
	held := time.Now().Add(-time.Hour)
	f.repo.WithClock(func() time.Time { return held })

	added, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "person@gmail.com", Flow: "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StatePendingAuth {
		t.Fatalf("account = %s %q right after the flow started, want pending_auth", a.State, a.StateReason)
	}
	reply := getRedirect(t, added.Auth, url.Values{"code": {"the-code"}, "state": {stateOf(t, added.Auth)}})
	if reply.status != http.StatusOK || reply.body != "Authorization complete. You can close this tab.\n" {
		t.Fatalf("the redirect: %d %q", reply.status, reply.body)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateActive {
		t.Errorf("account = %s %q, want active", a.State, a.StateReason)
	}
}

func TestRemovingAPendingAccountClosesItsListener(t *testing.T) {
	f, idp := consentFixture(t, "")
	loop, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "person@gmail.com", Flow: "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(redirectURIOf(t, loop.Auth.AuthURL))
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", redirect.Host); err != nil {
		t.Fatalf("the listener was not up to begin with: %v", err)
	} else {
		_ = conn.Close()
	}
	if err := f.svc.RemoveAccount(t.Context(), admin(), loop.Account.ID, service.RemoveAccountRequest{Confirm: loop.Account.ID}); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", redirect.Host); err == nil {
		_ = conn.Close()
		t.Fatal("the loopback listener still answers after its account was removed")
	}

	device, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{Email: "person@outlook.com", Flow: "device"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the device flow to start polling", func() bool { return idp.pollCount() > 0 })
	if err := f.svc.RemoveAccount(t.Context(), admin(), device.Account.ID, service.RemoveAccountRequest{Confirm: device.Account.ID}); err != nil {
		t.Fatal(err)
	}
	after := idp.pollCount()
	time.Sleep(2500 * time.Millisecond)
	if n := idp.pollCount(); n != after {
		t.Fatalf("the device poll went on %d more times after its account was removed", n-after)
	}
}

func TestAWebClientTokenIsRefreshedWithTheWebClient(t *testing.T) {
	// A refresh token belongs to the client that issued it. The console's
	// accounts and the CLI's live side by side, so each must be refreshed
	// with its own.
	f, idp := consentFixture(t, "http://localhost:5174")
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@gmail.com"})
	idp.set(func(f *fakeIDP) {
		f.onIssue = srv.AcceptToken
		// Every token is at the end of its life on arrival, so the first
		// use refreshes.
		f.expiresIn = 1
	})
	host, port := splitHostPort(t, srv.Addr)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)

	connect := func(p service.Principal, email, flow string) string {
		t.Helper()
		added, err := f.svc.AddAccount(t.Context(), p, keyed(p, service.AddAccountRequest{
			Email: email, Flow: flow, LoginUser: srv.User,
			IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.CompleteOAuth(t.Context(), p,
			"http://127.0.0.1:1/cb?code=code-"+flow+"&state="+stateOf(t, added.Auth)); err != nil {
			t.Fatalf("%s: %v", flow, err)
		}
		return added.Account.ID
	}
	console := connect(ana, "ana@gmail.com", "web")
	cli := connect(admin(), "cli@gmail.com", "pasted")

	for _, c := range []struct {
		p              service.Principal
		id, client     string
		secret, stored string
	}{
		{ana, console, "google-web", "google-web-secret", account.ClientWeb},
		{admin(), cli, "google-installed", "", account.ClientInstalled},
	} {
		if got := f.stateOfAccount(t, c.id).OAuthClient; got != c.stored {
			t.Errorf("%s: stored client %q, want %q", c.client, got, c.stored)
		}
		before := idp.refreshCount()
		if _, err := f.svc.ListFolders(t.Context(), c.p, c.id); err != nil {
			t.Fatalf("%s: ListFolders: %v", c.client, err)
		}
		if n := idp.refreshCount() - before; n != 1 {
			t.Fatalf("%s: %d refreshes, want one", c.client, n)
		}
		refresh := idp.refresh(before)
		if refresh.Get("client_id") != c.client || refresh.Get("client_secret") != c.secret {
			t.Errorf("refreshed as %q with secret %q, want %q with %q",
				refresh.Get("client_id"), refresh.Get("client_secret"), c.client, c.secret)
		}
	}
}

func TestAnAccountWhoseClientWasRemovedSaysSo(t *testing.T) {
	// The web client taken out of the environment after an account was
	// authorised with it: a clear state, not a refresh against the wrong
	// client or a crash.
	f, idp := consentFixture(t, "http://localhost:5174")
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	added, err := f.svc.AddAccount(t.Context(), ana, keyed(ana, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CompleteOAuth(t.Context(), ana,
		"http://localhost:5174/oauth/return?code=c&state="+stateOf(t, added.Auth)); err != nil {
		t.Fatal(err)
	}

	without := f.rebuild(t, account.RegistryOptions{
		Google: account.OAuthClient{ClientID: "google-installed", Endpoint: idp.endpoint()},
	})
	_, err = without.ListFolders(t.Context(), ana, added.Account.ID)
	if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "re-authorise") {
		t.Fatalf("ListFolders = %v", err)
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State != account.StateError || a.StateReason != account.ReasonClientMissing {
		t.Errorf("account = %s %q", a.State, a.StateReason)
	}
	if n := idp.refreshCount(); n != 0 {
		t.Errorf("%d refreshes were attempted with another client", n)
	}
}

func TestAPasswordAccountIsTestedBeforeItIsSaved(t *testing.T) {
	f := newFixture(t)

	wrong := f.passwordAccount(t, "person@example.com")
	wrong.Password = "not the password"
	_, refusedErr := f.svc.AddAccount(t.Context(), admin(), wrong)
	if service.CodeOf(refusedErr) != service.CodeBadRequest || !strings.Contains(service.MessageOf(refusedErr), "refused") {
		t.Errorf("a wrong password: %v", refusedErr)
	}
	// The server's own words did cross the wire and sit in the error chain,
	// for a debug log; that is what makes the leak check below mean
	// something.
	if !chainQuotes(refusedErr, "LOGIN failed") {
		t.Errorf("the fake server's refusal text never reached the provider: %v", refusedErr)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitHostPort(t, closed.Addr().String())
	_ = closed.Close()
	unreachable := f.passwordAccount(t, "person@example.com")
	unreachable.IMAPHost, unreachable.IMAPPort = host, port
	_, err = f.svc.AddAccount(t.Context(), admin(), unreachable)
	if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "could not be reached") {
		t.Errorf("a server that is not there: %v", err)
	}

	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused logins left %d accounts behind", len(accounts))
	}
	// Whoever the person asks sees why in the log, and never the password.
	f.warned(t, []string{
		"the mail server did not accept a new password account", "class=auth_failed", "class=connection_closed",
	}, "not the password", "hunter2")
	// A server message never reaches the caller, only this layer's own.
	for _, e := range []error{refusedErr, err} {
		for _, leak := range []string{"LOGIN failed", "connection refused"} {
			if strings.Contains(service.MessageOf(e), leak) {
				t.Errorf("the message quotes the server: %q", service.MessageOf(e))
			}
		}
	}

	result, err := f.svc.AddAccount(t.Context(), admin(), f.passwordAccount(t, "person@example.com"))
	if err != nil || result.Account.State != "active" {
		t.Fatalf("the right password: %+v %v", result, err)
	}
}

// chainQuotes reports whether any error in err's chain has text in its own
// message, which a wrapper that hides its cause's text does not.
func chainQuotes(err error, text string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), text) {
		return true
	}
	switch u := err.(type) { //nolint:errorlint // walking the chain by hand is the point
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if chainQuotes(e, text) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return chainQuotes(u.Unwrap(), text)
	}
	return false
}

func TestAPrivateMailHostIsRefusedUnlessAllowed(t *testing.T) {
	strict := newFixtureWith(t, fixtureOptions{strictHosts: true})
	srv := strict.mailServer(t)
	_, port := splitHostPort(t, srv.Addr)

	for name, req := range map[string]service.AddAccountRequest{
		"a loopback address":     {IMAPHost: "127.0.0.1", SMTPHost: "127.0.0.1"},
		"a name for loopback":    {IMAPHost: "localhost", SMTPHost: "localhost"},
		"a private SMTP host":    {IMAPHost: "93.184.215.14", SMTPHost: "10.0.0.25"},
		"the metadata service":   {IMAPHost: "169.254.169.254", SMTPHost: "169.254.169.254"},
		"loopback spelt as IPv6": {IMAPHost: "::ffff:127.0.0.1", SMTPHost: "::ffff:127.0.0.1"},
	} {
		req.Email, req.Password, req.LoginUser, req.IMAPPort, req.SMTPPort = "person@example.com", "hunter2", srv.User, port, port
		_, err := strict.svc.AddAccount(t.Context(), admin(), req)
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "private") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if accounts, _ := strict.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused hosts left %d accounts behind", len(accounts))
	}

	// A name that resolved somewhere public when it was added and somewhere
	// private now — DNS rebinding — is refused when it is dialed.
	stored, err := strict.repo.Create(t.Context(), account.Account{
		ID: "acc_rebound", Email: "person@example.com", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "localhost", IMAPPort: port, SMTPHost: "localhost", SMTPPort: port, SMTPTLS: "starttls",
		LoginUser: srv.User, State: account.StateActive,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.repo.SavePassword(t.Context(), stored.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}
	_, err = strict.svc.ListFolders(t.Context(), admin(), stored.ID)
	if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "private") {
		t.Fatalf("dialing a name that now resolves to loopback: %v", err)
	}

	// Allowed, the same server is fine: the default fixture allows it.
	allowed := newFixture(t)
	if _, err := allowed.svc.AddAccount(t.Context(), admin(), allowed.passwordAccount(t, "person@example.com")); err != nil {
		t.Fatalf("with private hosts allowed: %v", err)
	}
}

// redirectReply is what a browser tab landing on a loopback listener sees.
type redirectReply struct {
	status int
	body   string
}

// getRedirect plays the browser coming back to a flow's loopback listener.
func getRedirect(t *testing.T, flow *service.AuthFlow, query url.Values) redirectReply {
	t.Helper()
	target, err := url.Parse(redirectURIOf(t, flow.AuthURL))
	if err != nil {
		t.Fatal(err)
	}
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return redirectReply{status: resp.StatusCode, body: string(body)}
}
