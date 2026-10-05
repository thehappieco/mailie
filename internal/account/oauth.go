package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/microsoft"

	"github.com/thehappieco/mailie/internal/provider"
)

// Scopes each provider requires.
//
// Google offers nothing narrower for IMAP and SMTP than the full-mailbox
// scope, which it classifies as restricted. Microsoft's are per protocol and
// must carry the resource URL; mixing them with Graph scopes makes the token
// endpoint issue a token for the wrong resource, which then fails at
// AUTHENTICATE with no indication why.
var (
	googleScopes = []string{"https://mail.google.com/"}

	microsoftScopes = []string{
		"https://outlook.office.com/IMAP.AccessAsUser.All",
		"https://outlook.office.com/SMTP.Send",
		"offline_access",
	}
)

// OAuthClient is a provider's application registration.
type OAuthClient struct {
	ClientID     string
	ClientSecret string
	Tenant       string
	// Endpoint replaces the provider's authorization and token URLs. Nothing
	// in the daemon sets it: it exists so tests can put a fake identity
	// provider behind a real flow and count what reaches it.
	Endpoint oauth2.Endpoint
}

// configFor builds the OAuth configuration for a provider's registration.
// which is ClientInstalled or ClientWeb, and only decides which variable an
// error names.
func configFor(kind provider.Kind, client OAuthClient, which, redirectURL string) (*oauth2.Config, error) {
	switch kind {
	case provider.KindGmail:
		if client.ClientID == "" {
			if which == ClientWeb {
				return nil, fmt.Errorf("%w: MAIL_GOOGLE_WEB_CLIENT_ID is not set; see docs/console.md", ErrNotConfigured)
			}
			return nil, fmt.Errorf("%w: MAIL_GOOGLE_CLIENT_ID is not set; see the README for how to create one", ErrNotConfigured)
		}
		endpoint := google.Endpoint
		if client.Endpoint.TokenURL != "" {
			endpoint = client.Endpoint
		}
		// Explicit rather than left to auto-detection: the zero AuthStyle
		// makes the first request per configuration a Basic-auth probe that
		// fails and is retried, so a real invalid_grant costs two round trips
		// and is reported twice.
		endpoint.AuthStyle = oauth2.AuthStyleInParams
		return &oauth2.Config{
			ClientID:     client.ClientID,
			ClientSecret: client.ClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  redirectURL,
			Scopes:       googleScopes,
		}, nil

	case provider.KindMicrosoft:
		if client.ClientID == "" {
			if which == ClientWeb {
				return nil, fmt.Errorf("%w: MAIL_MICROSOFT_WEB_CLIENT_ID is not set; see docs/console.md", ErrNotConfigured)
			}
			return nil, fmt.Errorf("%w: MAIL_MICROSOFT_CLIENT_ID is not set; see the README for how to register the app", ErrNotConfigured)
		}
		tenant := client.Tenant
		if tenant == "" {
			tenant = "common"
		}
		endpoint := microsoft.AzureADEndpoint(tenant)
		if client.Endpoint.TokenURL != "" {
			endpoint = client.Endpoint
		}
		endpoint.AuthStyle = oauth2.AuthStyleInParams
		return &oauth2.Config{
			ClientID: client.ClientID,
			// Empty for the installed client and set for the web one, and
			// both halves of that matter. The installed client is public:
			// sending it a secret makes the token endpoint answer AADSTS700025.
			// The web client is confidential: leaving the secret out gets
			// AADSTS7000218.
			ClientSecret: client.ClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  redirectURL,
			Scopes:       microsoftScopes,
		}, nil

	default:
		return nil, fmt.Errorf("%w: %s accounts authenticate with a password", ErrNotOAuth, kind)
	}
}

// redirectHost is the loopback address each provider expects.
//
// They differ, and both are picky. Google matches the exact loopback address
// and accepts any port; Microsoft matches "http://localhost" and ignores the
// port entirely, while refusing the IPv6 form outright.
func redirectHost(kind provider.Kind) string {
	if kind == provider.KindMicrosoft {
		return "localhost"
	}
	return "127.0.0.1"
}

// webRedirectPath is where a web flow sends the browser back to: a route of
// the console, not of the API. The page there restores the session and hands
// the address to POST /v1/accounts/oauth/callback with its own credential,
// which is what ties the consent to the person who started it. Never
// /oauth/callback, the loopback path: Entra ignores the port of a localhost
// redirect, so two registrations differing only by port would be ambiguous.
const webRedirectPath = "/oauth/return"

// AuthFlow is a consent flow waiting to be completed.
type AuthFlow struct {
	// AccountID is the account being authorised.
	AccountID string
	// AuthURL is where the person has to go.
	AuthURL string
	// State ties the callback to this flow.
	State string
	// RedirectURI is exactly what the provider was told, and what the callback
	// must match.
	RedirectURI string
	// Verifier is the PKCE secret. It never leaves this process.
	Verifier string
	// Kind is which flow this is, so a caller knows whether to send the
	// person to AuthURL, wait for a loopback redirect, or show a code.
	Kind FlowKind
	// DeviceCode, UserCode and VerificationURI are set for the device flow.
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	// interval is how often the provider asked to be polled, in seconds.
	interval int64
}

// FlowKind is how the person will consent.
type FlowKind string

const (
	// FlowLoopback opens a browser and catches the redirect on a local port.
	FlowLoopback FlowKind = "loopback"
	// FlowPasted shows the URL and takes the redirect back by hand, for a
	// daemon on a machine with no browser.
	FlowPasted FlowKind = "pasted"
	// FlowDevice shows a code to type on another device. Microsoft only, and
	// off by default: Entra security defaults block it, mandatorily for
	// tenants created after July 2026.
	FlowDevice FlowKind = "device"
	// FlowWeb redirects to the console's /oauth/return with the console's own
	// web client, for a browser that is not on the daemon's machine.
	FlowWeb FlowKind = "web"
)

// client is the registration a flow runs under. Only the web flow uses the
// console's web client; everything else is the CLI's installed one.
func (k FlowKind) client() string {
	if k == FlowWeb {
		return ClientWeb
	}
	return ClientInstalled
}

// Errors about which flows can run, and how they end.
var (
	// ErrNotConfigured is a provider with no OAuth client registered here.
	ErrNotConfigured = errors.New("account: no OAuth client is configured for that provider")
	// ErrFlowUnavailable is a flow this daemon cannot offer for the account.
	ErrFlowUnavailable = errors.New("account: that consent flow is not available")
	// ErrNotOAuth is an OAuth operation on an account that uses a password.
	ErrNotOAuth = errors.New("account: the account does not use OAuth")
	// ErrBadRedirect is an address that is not one a provider sent a browser
	// back to.
	ErrBadRedirect = errors.New("account: that is not the address the provider sent the browser back to")
	// ErrFlowExpired is a consent attempt whose time ran out.
	ErrFlowExpired = errors.New("account: that authorisation attempt expired")
	// ErrConsentDeclined is the person saying no on the provider's page, or
	// the provider saying it for them.
	ErrConsentDeclined = errors.New("account: consent was declined")
	// ErrExchangeFailed is a token endpoint that would not turn an
	// authorization code into a grant.
	ErrExchangeFailed = errors.New("account: the provider did not issue a token")
	// ErrProviderRefused is a redirect that came back with an error that is
	// neither the person declining nor this server's registration: the
	// provider failing, or refusing for reasons of its own.
	ErrProviderRefused = errors.New("account: the provider refused the authorization")
	// ErrNoRefreshToken is a grant that came without the half that keeps it
	// alive past the hour.
	ErrNoRefreshToken = errors.New("account: the provider returned no refresh token")
	// ErrClientRejected is the identity provider refusing this server's own
	// registration: a wrong client id, an expired or rotated secret, a
	// deleted app. It is the deployment's to fix, once, and says nothing
	// about any account's grant — so it never marks an account for
	// re-consent, which would send every person through a consent screen to
	// fix a setting.
	ErrClientRejected = errors.New("account: the identity provider rejected this server's OAuth client")
	// ErrOwnerInactive is a consent or a new account whose person was
	// disabled or deleted while it was under way. Nothing is stored for them.
	ErrOwnerInactive = errors.New("account: the person this is for is no longer active")
	// ErrStarterLostAccess is a consent whose starter no longer manages the
	// account — their grant lost manage, or they lost their place in its
	// workspace — while it was under way. Nothing is stored.
	ErrStarterLostAccess = errors.New("account: whoever started the consent no longer manages the account")
	// ErrScopeMissing is a grant the token endpoint says does not cover the
	// mailbox. Google lets a person untick a scope only on the granular
	// screen it shows apps that ask for more than one, which this server
	// does not; Entra returns what was consented. Either way the response is
	// the provider saying what the token is for, and a token without the
	// mailbox refreshes like any other and fails at every login, so it is
	// refused before anything is stored.
	ErrScopeMissing = errors.New("account: the provider did not grant access to the mailbox")
	// ErrMailboxRefused is a grant the mail server would not log in with,
	// even after a refresh: most often a token for another account than the
	// address being connected, because the person picked a different one on
	// the provider's page. Nothing is stored for it.
	ErrMailboxRefused = errors.New("account: the mail server refused the new authorization")
)

// Why a consent attempt or a grant stopped working, as an account's
// state_reason says it. Short and fixed rather than the provider's own words:
// a caller may show these, and an error_description is text anyone can put in
// a redirect URL.
const (
	ReasonDeclined       = "consent was declined"
	ReasonExpired        = "the authorization attempt expired"
	ReasonRefused        = "the provider refused the authorization"
	ReasonNoRefreshToken = "the provider issued no refresh token"
	ReasonClientRejected = "the provider rejected this server's OAuth client"
	ReasonClientMissing  = "the OAuth client that authorized this account is not configured"
	ReasonScopeMissing   = "the provider did not grant access to the mailbox"
	ReasonMailboxRefused = "the mail server refused this authorization for the mailbox"
	ReasonFailed         = "the authorization could not be completed"
	// ReasonTokenRejected is not a failed consent but a grant that stopped
	// working: the mail server refused the account's access token, a freshly
	// refreshed one included, and the account moved to needs_reauth.
	ReasonTokenRejected = "the mail server rejected the account's access token"
)

// failureReason is the state_reason a failed consent attempt leaves behind.
func failureReason(err error) string {
	switch {
	case errors.Is(err, ErrConsentDeclined):
		return ReasonDeclined
	case errors.Is(err, ErrFlowExpired):
		return ReasonExpired
	case errors.Is(err, ErrClientRejected):
		return ReasonClientRejected
	case errors.Is(err, ErrNotConfigured):
		return ReasonClientMissing
	case errors.Is(err, ErrNoRefreshToken):
		return ReasonNoRefreshToken
	case errors.Is(err, ErrScopeMissing):
		return ReasonScopeMissing
	case errors.Is(err, ErrMailboxRefused):
		return ReasonMailboxRefused
	case errors.Is(err, ErrExchangeFailed), errors.Is(err, ErrProviderRefused):
		return ReasonRefused
	default:
		return ReasonFailed
	}
}

// startAuthCode begins an authorisation-code flow with PKCE.
//
// loginHint is the mailbox being connected. Both providers use it to pick or
// prefill the account on their sign-in page, which matters most exactly when
// it goes wrong: a browser signed in as somebody else would otherwise offer
// that person, and the grant would open the wrong mailbox.
func startAuthCode(kind provider.Kind, config *oauth2.Config, state, loginHint string) (*AuthFlow, error) {
	verifier := oauth2.GenerateVerifier()

	options := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		// Without offline access there is no refresh token, and the account
		// would stop working within the hour.
		oauth2.AccessTypeOffline,
	}
	if loginHint != "" {
		options = append(options, oauth2.SetAuthURLParam("login_hint", loginHint))
	}
	switch kind {
	case provider.KindGmail:
		// Google only issues a refresh token on the first consent unless
		// asked again explicitly, and an account added twice would otherwise
		// arrive with nothing to refresh.
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent"))
	case provider.KindMicrosoft:
		// Otherwise a browser already signed in as someone else silently
		// authorises the wrong mailbox.
		options = append(options, oauth2.SetAuthURLParam("prompt", "select_account"))
	}

	return &AuthFlow{
		AuthURL:     config.AuthCodeURL(state, options...),
		State:       state,
		RedirectURI: config.RedirectURL,
		Verifier:    verifier,
	}, nil
}

// exchange turns an authorisation code into a token.
func exchange(ctx context.Context, config *oauth2.Config, code, verifier string) (*oauth2.Token, error) {
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, describeExchangeError(err)
	}
	if err := checkGrantedScopes(token, config.Scopes); err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		// Without one the account works for an hour and then stops. Better to
		// refuse now, while the person is still in front of the browser.
		return nil, fmt.Errorf("%w; for Google, make sure the consent screen is published and "+
			"the account has not already authorised this client", ErrNoRefreshToken)
	}
	return token, nil
}

// Redirect is the address a provider sent a browser back to, taken apart.
type Redirect struct {
	Code  string
	State string
	// Error is the provider's error code — access_denied when the person
	// said no — and ErrorDescription its prose, which is for logs only.
	Error            string
	ErrorDescription string
}

// ParseRedirect reads the whole address a browser landed on.
//
// Accepting the whole URL rather than the code is what makes the headless flow
// usable: the person copies what the browser landed on, which is one paste
// instead of finding a query parameter inside it. It is also all the console
// has to send: the page at /oauth/return posts its own location. The state is
// checked by the caller against what it stored.
func ParseRedirect(raw string) (Redirect, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Redirect{}, fmt.Errorf("%w: it does not look like a URL", ErrBadRedirect)
	}
	q := parsed.Query()
	rd := Redirect{
		Code: q.Get("code"), State: q.Get("state"),
		Error: q.Get("error"), ErrorDescription: q.Get("error_description"),
	}
	if rd.Error == "" && rd.Code == "" {
		return Redirect{}, fmt.Errorf("%w: it carries no authorization code; paste the address the browser ended on",
			ErrBadRedirect)
	}
	return rd, nil
}

// refusal is the error a redirect carrying error= stands for.
//
// Only the person saying no — or the provider saying it for them, because
// they would have had to sign in or pick an account first — is a decline. A
// registration the provider will not serve is the operator's to fix, and
// recording it as "consent was declined" would have them blame the person
// instead; anything else is the provider failing to authorize.
func (rd Redirect) refusal() error {
	switch rd.Error {
	case "access_denied", "consent_required", "interaction_required",
		"login_required", "account_selection_required":
		return fmt.Errorf("%w (%s)", ErrConsentDeclined, rd.Error)
	case "unauthorized_client", "invalid_client", "unsupported_response_type", "invalid_scope":
		return fmt.Errorf("%w (%s): %s: check the client registration and scopes this server is configured with",
			ErrClientRejected, rd.Error, rd.ErrorDescription)
	default: // server_error, temporarily_unavailable, invalid_request, and codes nobody documented
		return fmt.Errorf("%w: the provider answered %s: %s", ErrProviderRefused, rd.Error, rd.ErrorDescription)
	}
}

// loopbackServer catches one flow's redirect on a local port.
//
// It knows the state it is waiting for and answers anything else itself.
// Otherwise one request with the wrong state — a stale tab, a scanner, a
// page that fires the URL on purpose — would be taken for the answer and end
// the flow while the real consent was still on its way.
type loopbackServer struct {
	listener net.Listener
	server   *http.Server
	redirect string
	state    string

	// results hands the redirect to the goroutine completing the flow.
	// Unbuffered: the handler then waits for that goroutine's verdict, which
	// is what lets the page say how the attempt actually ended.
	results chan loopbackResult
	// claimed is set by the first redirect carrying this flow's state. A
	// second one — a reload — is told the attempt is already being handled
	// rather than exchanging a code twice.
	claimed atomic.Bool

	closed    chan struct{}
	closeOnce sync.Once
}

type loopbackResult struct {
	redirect Redirect
	// err is a redirect that carried the right state and nothing usable.
	err error
	// outcome carries how the flow ended back to the tab that finished it.
	outcome chan<- error
}

// What the browser tab that finished a loopback flow is shown. Fixed text: the
// account's state says why, and the provider's own description is not
// something to echo into a page.
const (
	pageComplete = "Authorization complete. You can close this tab.\n"
	pageFailed   = "Authorization failed. Go back to where you started connecting the account and try again.\n"
	pageStray    = "This address does not belong to an authorization in progress. " +
		"If you were connecting an account, start again from where you began.\n"
	pageHandled = "This authorization is already being completed. You can close this tab.\n"
)

// outcomeWait bounds how long the page waits for the exchange behind it: all
// of finishing the flow, with a margin for taking its row first, so the page
// says how it ended rather than that it is still going.
const outcomeWait = finishTimeout + 5*time.Second

// listenLoopback starts a one-shot server on an ephemeral port.
//
// Inside the daemon rather than the command line tool: the daemon is what
// holds the flow's state and what will own the account afterwards, and a
// listener in a short-lived CLI process would be gone before a slow consent
// screen came back.
func listenLoopback(kind provider.Kind, path, state string) (*loopbackServer, error) {
	host := redirectHost(kind)
	// A listener, not a dialler: there is no context to honour, and the
	// caller's deadline governs how long the flow stays open.
	//nolint:noctx // no outbound connection is made here
	listener, err := net.Listen("tcp", host+":0")
	if err != nil {
		// Microsoft insists on the name "localhost"; if that does not resolve
		// there is nothing to fall back to that the provider would accept.
		return nil, fmt.Errorf("account: cannot listen on %s for the OAuth redirect: %w", host, err)
	}
	//nolint:errcheck // a TCP listener's address is always a *net.TCPAddr
	port := listener.Addr().(*net.TCPAddr).Port

	s := &loopbackServer{
		listener: listener,
		redirect: fmt.Sprintf("http://%s:%d%s", host, port, path),
		state:    state,
		results:  make(chan loopbackResult),
		closed:   make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Serve always ends with an error, and the only one that can happen here
	// is the shutdown this flow asked for.
	//nolint:errcheck // shutdown is the expected end
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

func (s *loopbackServer) handle(w http.ResponseWriter, r *http.Request) {
	// The URL carries an authorization code: nothing may cache the page or
	// pass the address on in a Referer.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.state)) != 1 {
		// Not this flow's redirect. The flow keeps waiting for its own.
		s.page(w, http.StatusBadRequest, pageStray)
		return
	}
	if !s.claimed.CompareAndSwap(false, true) {
		s.page(w, http.StatusConflict, pageHandled)
		return
	}
	rd, err := ParseRedirect(r.URL.String())

	outcome := make(chan error, 1)
	select {
	case s.results <- loopbackResult{redirect: rd, err: err, outcome: outcome}:
	case <-s.closed:
		s.page(w, http.StatusBadRequest, pageFailed)
		return
	case <-r.Context().Done():
		// Never delivered, so the flow is still open: the next redirect
		// with this state may finish it.
		s.claimed.Store(false)
		return
	}
	select {
	case err := <-outcome:
		if err != nil {
			s.page(w, http.StatusBadRequest, pageFailed)
			return
		}
		s.page(w, http.StatusOK, pageComplete)
	case <-s.closed:
		s.page(w, http.StatusBadRequest, pageFailed)
	case <-time.After(outcomeWait):
		s.page(w, http.StatusAccepted, pageHandled)
	case <-r.Context().Done():
	}
}

func (s *loopbackServer) page(w http.ResponseWriter, status int, text string) {
	w.WriteHeader(status)
	//nolint:errcheck // the browser is being told, not the caller
	_, _ = fmt.Fprint(w, text)
}

// RedirectURI is what the provider must be told.
func (s *loopbackServer) RedirectURI() string { return s.redirect }

// Wait blocks until the browser comes back with this flow's state, or the
// context ends.
func (s *loopbackServer) Wait(ctx context.Context) (loopbackResult, error) {
	select {
	case r := <-s.results:
		return r, nil
	case <-ctx.Done():
		return loopbackResult{}, ctx.Err()
	}
}

// Close stops the listener. A handler still waiting on the flow is released
// first, so the shutdown does not wait out its timer.
func (s *loopbackServer) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// startDevice begins the device-code flow.
func startDevice(ctx context.Context, config *oauth2.Config) (*AuthFlow, error) {
	response, err := config.DeviceAuth(ctx)
	if err != nil {
		return nil, describeExchangeError(err)
	}
	verification := response.VerificationURIComplete
	if verification == "" {
		verification = response.VerificationURI
	}
	return &AuthFlow{
		DeviceCode:      response.DeviceCode,
		UserCode:        response.UserCode,
		VerificationURI: verification,
		ExpiresAt:       response.Expiry,
		interval:        response.Interval,
	}, nil
}

// pollDevice waits for the person to finish on the other device.
func pollDevice(ctx context.Context, config *oauth2.Config, flow *AuthFlow) (*oauth2.Token, error) {
	// The provider's own interval: polling faster earns slow_down, and the
	// five seconds RFC 8628 defaults to is only for a provider that says
	// nothing.
	interval := flow.interval
	if interval <= 0 {
		interval = 5
	}
	token, err := config.DeviceAccessToken(ctx, &oauth2.DeviceAuthResponse{
		DeviceCode: flow.DeviceCode,
		Expiry:     flow.ExpiresAt,
		Interval:   interval,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Timed out or called off: the caller tells the two apart.
			return nil, ctxErr
		}
		return nil, describeExchangeError(err)
	}
	if err := checkGrantedScopes(token, config.Scopes); err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		return nil, ErrNoRefreshToken
	}
	return token, nil
}

// unechoedScopes are asked for and never listed among the scopes a token is
// valid for: they are about the grant or the sign-in, not a resource. Entra
// leaves offline_access out of every token response — MSAL, which checks the
// same thing, special-cases these four — and a refresh token is the evidence
// it was granted, which exchange checks on its own.
var unechoedScopes = map[string]bool{"offline_access": true, "openid": true, "profile": true, "email": true}

// checkGrantedScopes refuses a token whose response says it was granted less
// than this server asked for.
//
// Only when the response says so. The scope field is optional in OAuth, and
// its absence means the grant is what was asked for; a response that says
// nothing is left to the login that proves the grant, which catches the rest.
func checkGrantedScopes(token *oauth2.Token, requested []string) error {
	granted := grantedScopes(token)
	if len(granted) == 0 {
		return nil
	}
	var missing []string
	for _, want := range requested {
		if unechoedScopes[strings.ToLower(want)] {
			continue
		}
		if !scopeGranted(granted, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		// Scope names identify nobody, and saying which were left out is
		// the whole diagnosis.
		return fmt.Errorf("%w: the token is valid for %q, which lacks %s",
			ErrScopeMissing, strings.Join(granted, " "), strings.Join(missing, " "))
	}
	return nil
}

// grantedScopes reads the token response's scope field, which both
// providers send space-separated. An empty field counts as absent: that is
// also what oauth2 reports for a form-encoded response that had none.
func grantedScopes(token *oauth2.Token) []string {
	var raw []string
	switch v := token.Extra("scope").(type) {
	case string:
		raw = strings.Fields(v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				raw = append(raw, strings.Fields(s)...)
			}
		}
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		// Microsoft's own documentation shows the field percent-encoded,
		// where the space between two scopes is %20 too: decoded first, then
		// split again.
		if strings.Contains(s, "%") {
			if decoded, err := url.QueryUnescape(s); err == nil {
				out = append(out, strings.Fields(decoded)...)
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// scopeGranted reports whether want is among granted, in any of the forms a
// provider writes it back in.
//
// Case never matters, nor a trailing slash. Entra writes a resource's
// permission qualified, "https://outlook.office.com/IMAP.AccessAsUser.All",
// and may name the resource by its other host, outlook.office365.com; an
// unqualified "IMAP.AccessAsUser.All" is accepted too, since the login that
// proves the grant is what catches a token minted for the wrong resource.
func scopeGranted(granted []string, want string) bool {
	wantResource, wantPermission := splitScope(want)
	for _, g := range granted {
		if strings.EqualFold(strings.TrimSuffix(g, "/"), strings.TrimSuffix(want, "/")) {
			return true
		}
		if wantPermission == "" {
			continue
		}
		resource, permission := splitScope(g)
		if !strings.EqualFold(permission, wantPermission) {
			continue
		}
		if resource == "" || sameResource(resource, wantResource) {
			return true
		}
	}
	return false
}

// splitScope separates a resource-qualified scope into its resource and its
// permission. A scope that is a URL and nothing more, like Google's
// "https://mail.google.com/", has no permission part; an unqualified one has
// no resource.
func splitScope(scope string) (resource, permission string) {
	if !strings.Contains(scope, "://") {
		return "", scope
	}
	idx := strings.LastIndex(scope, "/")
	if idx < len("https://") {
		return scope, ""
	}
	return scope[:idx], scope[idx+1:]
}

// outlookResources are the names Exchange Online answers to as a resource.
var outlookResources = map[string]bool{
	"https://outlook.office.com":    true,
	"https://outlook.office365.com": true,
}

func sameResource(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	return a == b || (outlookResources[a] && outlookResources[b])
}

// describeExchangeError turns a token-endpoint failure into one of this
// package's sentinels, with enough detail for an operator and without quoting
// the whole response body back.
func describeExchangeError(err error) error {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		return fmt.Errorf("%w: the token request failed: %w", ErrExchangeFailed, err)
	}
	if clientRejected(retrieve) {
		return fmt.Errorf("%w (%s%s): check the client id and secret this server is configured with",
			ErrClientRejected, retrieve.ErrorCode, microsoftDetail(retrieve))
	}
	switch retrieve.ErrorCode {
	case "access_denied", "authorization_declined":
		return fmt.Errorf("%w: the user declined the request", ErrConsentDeclined)
	case "expired_token":
		return fmt.Errorf("%w: the device code expired before it was approved", ErrFlowExpired)
	case "invalid_grant":
		// For a code exchange this is the code itself: expired, already
		// redeemed, or issued for a different redirect or PKCE verifier.
		return fmt.Errorf("%w: the provider refused the authorization code%s", ErrExchangeFailed, microsoftDetail(retrieve))
	}
	if retrieve.ErrorDescription != "" {
		return fmt.Errorf("%w: %s: %s", ErrExchangeFailed, retrieve.ErrorCode, retrieve.ErrorDescription)
	}
	return fmt.Errorf("%w: %s", ErrExchangeFailed, retrieve.ErrorCode)
}

// clientRejected reports a token-endpoint refusal of the client itself rather
// than of a grant. invalid_client is a bad id or secret — Entra's AADSTS7000215
// (wrong secret), AADSTS7000222 (expired secret), AADSTS7000218 (confidential
// client that sent none) and AADSTS700025 (public client that sent one) all
// arrive as it. unauthorized_client is a registration not allowed this grant,
// which Google also answers for a refresh token presented to a client other
// than the one that issued it.
func clientRejected(err *oauth2.RetrieveError) bool {
	return err.ErrorCode == "invalid_client" || err.ErrorCode == "unauthorized_client"
}
