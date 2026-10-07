package api_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

type harness struct {
	server *httptest.Server
	keys   *auth.Keys
	users  *auth.Users
	store  *store.Store
	imap   *providertest.IMAPServer
	// bus is the service's, for tests that publish what the sync engine
	// would.
	bus *events.Bus
}

// serviceOptions vary what the daemon under test is configured with.
type serviceOptions struct {
	publicURL string
	registry  account.RegistryOptions
	// now fixes the database's clock, for output that has to be the same on
	// every run.
	now func() time.Time
	// sync stands in for the sync engine; nil runs without one.
	sync service.SyncController
	// downloadsPerCaller bounds one caller's downloads in flight; zero is
	// the service's default.
	downloadsPerCaller int
	// consent is the revisions of the texts people agree to, as
	// MAIL_CONSENT_VERSION_* configure them; empty ones are the defaults.
	consent config.ConsentVersions
	// mcpHTTP is whether the daemon serves MCP over HTTP at /mcp, which
	// GET /v1/me/mcp reports.
	mcpHTTP bool
	// externalSignInOnly is a daemon whose people sign in only through an
	// extension: passwords and invitations are off.
	externalSignInOnly bool
	// keysMayNotSend is MAIL_KEYS_MAY_SEND=false, which GET /v1/me/mcp
	// reports.
	keysMayNotSend bool
}

// newService wires the real service to a temporary database, because testing
// the transports against a fake service is how REST and MCP drift apart.
func newService(t *testing.T, db *store.Store, bus *events.Bus, o serviceOptions) *service.Service {
	t.Helper()
	keyring := testKeyring(t)
	opts := o.registry
	if opts.Google.ClientID == "" {
		opts.Google = account.OAuthClient{ClientID: "google-client"}
	}
	opts.PublicURL = o.publicURL
	opts.SpoolDir = t.TempDir()
	// Password accounts log in before they are stored, here to the
	// in-process IMAP server, which speaks plain TCP on 127.0.0.1.
	opts.AllowInsecureAuth = true
	opts.AllowPrivate = true
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, keyring), opts)
	t.Cleanup(func() { _ = registry.Close() })
	return service.New(service.Deps{
		Accounts:  registry,
		Keys:      auth.NewKeys(db),
		Users:     auth.NewUsers(db),
		Store:     db,
		Bus:       bus,
		Sync:      o.sync,
		Log:       obs.NewLogger("error", "text"),
		PublicURL: o.publicURL,

		DownloadsPerCaller: o.downloadsPerCaller,
		SpoolDir:           opts.SpoolDir,
		ConsentVersions:    o.consent,
		MCPHTTP:            o.mcpHTTP,
		KeysMayNotSend:     o.keysMayNotSend,
		ExternalSignInOnly: o.externalSignInOnly,
	})
}

// testKeyring is the credential keyring every harness uses, so a test can
// store a password the registry then reads.
func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0xC3
	}
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func newHarness(t *testing.T, adminAPI bool) *harness {
	t.Helper()
	return newHarnessWith(t, func(h *api.Handler) { h.AdminAPI = adminAPI }, serviceOptions{})
}

// newHarnessWith builds a server around the real service, letting a test
// adjust the handler — its limits above all — and the configuration.
func newHarnessWith(t *testing.T, adjust func(*api.Handler), o serviceOptions) *harness {
	t.Helper()
	db := storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), o.now)
	bus := events.NewBus(events.NewJournal(db))
	h := &api.Handler{
		Service:      newService(t, db, bus, o),
		Limits:       ratelimit.DefaultAuth(nil),
		SignInLimits: ratelimit.DefaultSignIn(nil),
		Metrics:      obs.NewMetrics(),
		Log:          obs.NewLogger("error", "text"),
		Version:      "test",
		Started:      time.Now(),
	}
	if adjust != nil {
		adjust(h)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{server: srv, keys: auth.NewKeys(db), users: auth.NewUsers(db), store: db, bus: bus}
}

func (h *harness) key(t *testing.T, scope auth.Scope, accounts ...string) string {
	t.Helper()
	secret, _, err := h.keys.Issue(t.Context(), auth.NewKeyRequest{Name: "test", Scope: scope, AccountIDs: accounts})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return secret
}

// passwordAccount is the JSON for a generic IMAP account on the in-process
// server, which accepts any address logging in as its one user.
func (h *harness) passwordAccount(t *testing.T, email string) string {
	t.Helper()
	if h.imap == nil {
		h.imap = providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2"})
	}
	host, port, err := net.SplitHostPort(h.imap.Addr)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"email": email, "password": "hunter2", "login_user": h.imap.User,
		"imap_host": host, "imap_port": atoi(t, port), "smtp_host": host, "smtp_port": atoi(t, port),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) do(t *testing.T, method, path, key string, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(t.Context(), method, h.server.URL+path, reader)
	} else {
		req, err = http.NewRequestWithContext(t.Context(), method, h.server.URL+path, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeError(t *testing.T, resp *http.Response) (code, message string) {
	t.Helper()
	var wire struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return wire.Code, wire.Message
}

func TestTheHealthProbeNeedsNoKeyAndRevealsNothing(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/healthz", "", "")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
	// A supervisor needs to know the process is alive; it does not need to
	// know which mailboxes exist or which of them is failing.
	for _, leak := range []string{"accounts", "email", "state_reason"} {
		if _, found := body[leak]; found {
			t.Errorf("the unauthenticated probe exposed %q", leak)
		}
	}
}

func TestEveryProtectedRouteRefusesAMissingKey(t *testing.T) {
	h := newHarness(t, true)
	for _, path := range []string{"/v1/accounts", "/v1/apikeys"} {
		resp := h.do(t, http.MethodGet, path, "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a key: status %d, want 401", path, resp.StatusCode)
		}
		code, _ := decodeError(t, resp)
		if code != "unauthorized" {
			t.Errorf("%s: code %q, want unauthorized", path, code)
		}
	}
}

func TestLocalhostGetsNoShortcut(t *testing.T) {
	// The test server is on loopback, which is where this daemon normally
	// runs. A key is still required: a process that can read several mailboxes
	// and send as their owner must not trust a caller for being local.
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a loopback request without a key got %d", resp.StatusCode)
	}
}

func TestOnlyOneAuthorizationHeaderIsAccepted(t *testing.T) {
	h := newHarness(t, false)
	key := h.key(t, auth.ScopeRead)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+"/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("Authorization", "Bearer "+key)
	req.Header.Add("Authorization", "Bearer something-else")

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Two headers is two intentions; picking one silently is how a proxy's
	// injected credential ends up being the one that counts.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a duplicated Authorization header", resp.StatusCode)
	}
}

func TestAnOversizedTokenIsRejectedBeforeItIsHashed(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts", strings.Repeat("a", 5000), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestA401CarriesNoOAuthDiscoveryHint(t *testing.T) {
	// MCP clients that see resource_metadata in this header abandon the static
	// bearer token they were configured with and start an OAuth flow they
	// cannot finish.
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts", "", "")

	header := resp.Header.Get("WWW-Authenticate")
	if header == "" {
		t.Fatal("a 401 should say which scheme it wants")
	}
	if strings.Contains(header, "resource_metadata") {
		t.Fatalf("WWW-Authenticate advertises OAuth discovery: %q", header)
	}
}

func TestTheOAuthDiscoveryDocumentsAreAbsent(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
	} {
		resp := h.do(t, http.MethodGet, path, "", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 so clients keep using the static header", path, resp.StatusCode)
		}
	}
}

func TestAReadKeyCannotReachAdminRoutes(t *testing.T) {
	h := newHarness(t, true)
	resp := h.do(t, http.MethodGet, "/v1/apikeys", h.key(t, auth.ScopeRead), "")

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	code, message := decodeError(t, resp)
	if code != "not_authorized" {
		t.Fatalf("code = %q, want not_authorized", code)
	}
	// 403 rather than 404: the key is valid, it simply may not do this, and
	// saying so is what lets an operator fix it.
	if !strings.Contains(message, "admin") {
		t.Errorf("the message should name the scope required, got %q", message)
	}
}

func TestTheKeyRoutesAreAbsentUnlessTheAdminAPIIsEnabled(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/apikeys", h.key(t, auth.ScopeAdmin), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when MAIL_ADMIN_API is off", resp.StatusCode)
	}
}

func TestAKeyCannotIssueAStrongerKey(t *testing.T) {
	// Otherwise a leaked send key is a leaked admin key one request later.
	h := newHarness(t, true)
	resp := h.do(t, http.MethodPost, "/v1/apikeys", h.key(t, auth.ScopeSend),
		`{"name":"escalation","scope":"admin"}`)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAnIssuedKeyIsShownOnceAndWorks(t *testing.T) {
	h := newHarness(t, true)
	resp := h.do(t, http.MethodPost, "/v1/apikeys", h.key(t, auth.ScopeAdmin),
		`{"name":"reader","scope":"read","expires_in_days":30}`)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var created struct {
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Key == "" || !strings.HasPrefix(created.Key, created.Prefix+".") {
		t.Fatalf("the response did not carry a usable key: %+v", created)
	}

	// The new key works, and only within its scope.
	if resp := h.do(t, http.MethodGet, "/v1/accounts", created.Key, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the new read key could not list accounts: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/apikeys", created.Key, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the new read key reached an admin route: %d", resp.StatusCode)
	}
}

func TestARevokedKeyStopsWorkingImmediately(t *testing.T) {
	h := newHarness(t, true)
	admin := h.key(t, auth.ScopeAdmin)
	resp := h.do(t, http.MethodPost, "/v1/apikeys", admin, `{"name":"temp","scope":"read"}`)
	var created struct {
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp := h.do(t, http.MethodDelete, "/v1/apikeys/"+created.Prefix, admin, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/accounts", created.Key, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked key still worked: %d", resp.StatusCode)
	}
}

func TestRevokingAnUnknownKeyIs404(t *testing.T) {
	h := newHarness(t, true)
	resp := h.do(t, http.MethodDelete, "/v1/apikeys/deadbeef", h.key(t, auth.ScopeAdmin), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAnUnknownQueryParameterIsRefused(t *testing.T) {
	// A misspelt filter that silently widens a search is how a caller ends up
	// acting on the wrong mail.
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts?unred=true", h.key(t, auth.ScopeRead), "")

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	code, message := decodeError(t, resp)
	if code != "bad_request" {
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(message, "unred") {
		t.Errorf("the message should name the offending parameter, got %q", message)
	}
}

func TestAnUnknownBodyFieldIsRefused(t *testing.T) {
	// A caller who misspells "confirm" must be told, not treated as having
	// omitted it.
	h := newHarness(t, true)
	resp := h.do(t, http.MethodPost, "/v1/apikeys", h.key(t, auth.ScopeAdmin),
		`{"name":"x","scope":"read","confrim":true}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_, message := decodeError(t, resp)
	if !strings.Contains(message, "unknown field") {
		t.Errorf("message = %q", message)
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	h := newHarness(t, true)
	body := `{"name":"` + strings.Repeat("x", 128<<10) + `","scope":"read"}`
	resp := h.do(t, http.MethodPost, "/v1/apikeys", h.key(t, auth.ScopeAdmin), body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestResponsesAreNotCacheableAndNotSniffable(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts", h.key(t, auth.ScopeRead), "")

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: mail metadata must not sit in a shared cache", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestAnErrorBodyNeverEchoesTheCredential(t *testing.T) {
	h := newHarness(t, false)
	key := h.key(t, auth.ScopeRead)
	resp := h.do(t, http.MethodGet, "/v1/accounts?bogus=1", key, "")

	var raw strings.Builder
	if _, err := fmt.Fprint(&raw, resp.Header); err != nil {
		t.Fatal(err)
	}
	_, message := decodeError(t, resp)
	if strings.Contains(message, key) || strings.Contains(raw.String(), key) {
		t.Fatal("the error response echoed the api key")
	}
}

func TestTooManyRequestsCarryRetryAfter(t *testing.T) {
	h := newHarnessWith(t, func(h *api.Handler) {
		// One token, so the second call is refused.
		h.Limits = &ratelimit.Auth{PerIP: ratelimit.New(1, 1)}
	}, serviceOptions{})
	secret := h.key(t, auth.ScopeRead)

	if resp := h.do(t, http.MethodGet, "/v1/accounts", secret, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	resp := h.do(t, http.MethodGet, "/v1/accounts", secret, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 should say how long to wait")
	}
}

func TestTheLimiterRunsBeforeTheExpensiveHash(t *testing.T) {
	// Verifying a key costs 19 MiB of Argon2id. A limiter that ran afterwards
	// would protect nothing from a script working through a key list.
	h := newHarnessWith(t, func(h *api.Handler) {
		h.Limits = &ratelimit.Auth{PerIP: ratelimit.New(1, 1)}
	}, serviceOptions{})

	bad := "00000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got := h.do(t, http.MethodGet, "/v1/accounts", bad, "").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("first guess: %d", got)
	}
	if got := h.do(t, http.MethodGet, "/v1/accounts", bad, "").StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("second guess: %d, want the limiter to stop it before hashing", got)
	}
}

func TestAddingAnAccountReturnsSomewhereToAuthorise(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodPost, "/v1/accounts", h.key(t, auth.ScopeAdmin),
		`{"email":"person@gmail.com","flow":"pasted"}`)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var result struct {
		Account struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"account"`
		Auth struct {
			AuthURL string `json:"auth_url"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Account.State != "pending_auth" {
		t.Errorf("state = %q", result.Account.State)
	}
	if !strings.Contains(result.Auth.AuthURL, "accounts.google.com") {
		t.Errorf("auth url = %q", result.Auth.AuthURL)
	}
}

func TestAReadKeyCannotAddAnAccount(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodPost, "/v1/accounts", h.key(t, auth.ScopeRead),
		`{"email":"person@gmail.com"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAnAccountListStartsEmptyAndThenHasTheAccount(t *testing.T) {
	h := newHarness(t, false)
	admin := h.key(t, auth.ScopeAdmin)

	resp := h.do(t, http.MethodGet, "/v1/accounts", admin, "")
	var before []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("a fresh database listed %d accounts", len(before))
	}

	if resp := h.do(t, http.MethodPost, "/v1/accounts", admin, h.passwordAccount(t, "person@example.com")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: status %d", resp.StatusCode)
	}

	resp = h.do(t, http.MethodGet, "/v1/accounts", admin, "")
	var after []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("listed %d accounts, want 1", len(after))
	}
	if after[0]["state"] != "active" {
		t.Errorf("a password account should be usable immediately, got %v", after[0]["state"])
	}
	// The stored password must not come back out through the API.
	if _, leaked := after[0]["password"]; leaked {
		t.Error("the account listing carried a password field")
	}
}

func TestAnUnknownAccountIs404NotAPanic(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodGet, "/v1/accounts/acc_nope", h.key(t, auth.ScopeRead), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCompletingConsentWithAStaleRedirectIsNotFound(t *testing.T) {
	h := newHarness(t, false)
	resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", h.key(t, auth.ScopeAdmin),
		`{"redirect_url":"http://127.0.0.1:1/oauth/callback?code=abc&state=gone"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
