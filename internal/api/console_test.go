package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/ratelimit"
)

// fakeClock drives the limiters, so ten minutes of polling take no ten
// minutes.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
}

type sessionReply struct {
	Token           string `json:"token"`
	ExpiresAt       int64  `json:"expires_at"`
	AuthenticatedAt int64  `json:"authenticated_at"`
	PasswordWrap    string `json:"password_wrap"`
	User            struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		SealID    string `json:"seal_id"`
		PublicKey string `json:"public_key"`
	} `json:"user"`
}

// signIn signs in over HTTP with a user's auth key (authtest.AuthKey) and
// returns the session.
func (h *harness) signIn(t *testing.T, email string) sessionReply {
	t.Helper()
	return h.signInWith(t, email, authtest.AuthKey)
}

// signInWith signs in over HTTP with an auth key and returns the session.
func (h *harness) signInWith(t *testing.T, email, authKey string) sessionReply {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/auth/login", "", jsonOf(t, map[string]any{"email": email, "auth_key": authKey}))
	if resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("sign in as %s: %d %s %s", email, resp.StatusCode, code, message)
	}
	var s sessionReply
	decodeInto(t, resp, &s)
	return s
}

// jsonOf is v as a request body.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// enrolment is what a browser sends to enrol a person, as the routes take it,
// with the auth key and the proof given and a fresh public key and wraps;
// extra adds the rest of a request's members.
func enrolment(t *testing.T, authKey, proof string, extra map[string]any) map[string]any {
	t.Helper()
	in := authtest.EnrolmentWith(t, authKey, proof)
	enc := base64.RawURLEncoding.EncodeToString
	out := map[string]any{
		"auth_key": authKey, "recovery_proof": proof, "kdf": defaultKDF(),
		"public_key": enc(in.PublicKey), "password_wrap": enc(in.PasswordWrap), "recovery_wrap": enc(in.RecoveryWrap),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func defaultKDF() map[string]any {
	return map[string]any{"alg": "argon2id", "m": auth.DefaultKDF.M, "t": auth.DefaultKDF.T, "p": auth.DefaultKDF.P}
}

// openInvite opens an invitation as a browser does before it signs up, and
// returns the seal id it answered, or "" when it does not open.
func openInvite(t *testing.T, h *harness, code, email string) string {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/auth/signup/open", "", jsonOf(t, map[string]any{"invite": code, "email": email}))
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var opened struct {
		SealID string `json:"seal_id"`
	}
	decodeInto(t, resp, &opened)
	return opened.SealID
}

// secret is base64url of 32 bytes made from a label: an auth key or a proof.
func secret(label string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(label, 32)[:32]))
}

func TestAnAPIKeyCannotUseTheSessionRoutes(t *testing.T) {
	// The routes a person uses on their own account. A key acting for them is
	// still not them at a keyboard, and an admin key is nobody at all.
	h := newHarness(t, false)
	key := h.key(t, auth.ScopeAdmin)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/auth/me", ""},
		{http.MethodPost, "/v1/auth/logout", ""},
		{http.MethodPost, "/v1/auth/password/begin", `{"current_auth_key":"a"}`},
		{http.MethodPost, "/v1/auth/password/finish", `{"ticket":"a"}`},
		{http.MethodPost, "/v1/auth/recovery", `{"current_auth_key":"a","recovery_proof":"a"}`},
		{http.MethodPost, "/v1/auth/stepup", `{"auth_key":"a"}`},
		{http.MethodPut, "/v1/auth/profile", `{"name":"x"}`},
		// A mailbox's key: what only a person's browser seals and opens.
		{http.MethodGet, "/v1/accounts/acc_0000000000000001/mailbox-key", ""},
		{http.MethodPost, "/v1/accounts/acc_0000000000000001/mailbox-key", `{"public_key":"a","namespace":"a","grants":[]}`},
		{http.MethodPut, "/v1/accounts/acc_0000000000000001/mailbox-key", `{"epoch":2,"public_key":"a","grant":"a"}`},
		{http.MethodPut, "/v1/accounts/acc_0000000000000001/grants/usr_0000000000000001", `{"epoch":1,"grant":"a"}`},
	} {
		resp := h.do(t, route.method, route.path, key, route.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with an API key: %d, want 403", route.method, route.path, resp.StatusCode)
			continue
		}
		if code, message := decodeError(t, resp); code != "not_authorized" || !strings.Contains(message, "signed-in") {
			t.Errorf("%s %s: %s %q", route.method, route.path, code, message)
		}
	}
}

func TestASignedInPersonCanReadAndRenameThemselves(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	s := h.signIn(t, "Ana@Example.com")
	if s.User.Email != "ana@example.com" || s.User.Role != "member" || len(s.Token) != 43 {
		t.Fatalf("session = %+v", s)
	}

	resp := h.do(t, http.MethodGet, "/v1/auth/me", s.Token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on an auth reply", got)
	}
	var me struct {
		User    struct{ ID, Email string } `json:"user"`
		Session struct {
			ID        string `json:"id"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"session"`
	}
	decodeInto(t, resp, &me)
	if me.User.ID != s.User.ID || !strings.HasPrefix(me.Session.ID, "ses_") || me.Session.ExpiresAt != s.ExpiresAt {
		t.Errorf("me = %+v", me)
	}

	resp = h.do(t, http.MethodPut, "/v1/auth/profile", s.Token, `{"name":"  Ana Lima  "}`)
	var user struct{ Name string }
	decodeInto(t, resp, &user)
	if resp.StatusCode != http.StatusOK || user.Name != "Ana Lima" {
		t.Errorf("profile: %d %+v", resp.StatusCode, user)
	}
	if resp := h.do(t, http.MethodPut, "/v1/auth/profile", s.Token,
		fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", 121))); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a 121-character name: %d", resp.StatusCode)
	}
}

func TestAnUnknownEmailAnswersLikeAWrongPassword(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)

	type answer struct {
		status        int
		code, message string
	}
	ask := func(email, authKey string) answer {
		resp := h.do(t, http.MethodPost, "/v1/auth/login", "", jsonOf(t, map[string]any{"email": email, "auth_key": authKey}))
		code, message := decodeError(t, resp)
		return answer{resp.StatusCode, code, message}
	}
	wrong := ask("ana@example.com", secret("not the password"))
	unknown := ask("nobody@example.com", secret("not the password"))
	if wrong != unknown {
		t.Fatalf("a wrong password answered %+v and an unknown address %+v", wrong, unknown)
	}
	if wrong.status != http.StatusUnauthorized || wrong.code != "unauthorized" {
		t.Fatalf("answer = %+v, want 401 unauthorized", wrong)
	}
}

func TestSignInIsRateLimitedPerEmail(t *testing.T) {
	// Five guesses at one account a minute, whichever addresses they come
	// from; the per-address budget is larger, so another account is still
	// reachable from the same place.
	h := newHarnessWith(t, func(h *api.Handler) {
		h.SignInLimits = ratelimit.DefaultSignIn([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	}, serviceOptions{})
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	authtest.NewUser(t, h.store, "bob@example.com", auth.RoleMember)

	attempt := func(email, from string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/auth/login",
			strings.NewReader(jsonOf(t, map[string]any{"email": email, "auth_key": secret("wrong")})))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", from)
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	for i := range 5 {
		if resp := attempt("ana@example.com", fmt.Sprintf("203.0.113.%d", i+1)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i+1, resp.StatusCode)
		}
	}
	// Padding or recasing the address is the same account, so the same
	// bucket.
	resp := attempt(" ANA@example.com ", "198.51.100.7")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a sixth guess from a fresh address: %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 should say how long to wait")
	}
	if resp := attempt("bob@example.com", "198.51.100.7"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another account was throttled too: %d", resp.StatusCode)
	}
}

func TestAPasswordChangeNeedsTheCurrentAuthKeyAndEndsEverySession(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	laptop := h.signIn(t, "ana@example.com")
	phone := h.signIn(t, "ana@example.com")

	resp := h.do(t, http.MethodPost, "/v1/auth/password/begin", laptop.Token,
		jsonOf(t, map[string]any{"current_auth_key": secret("wrong")}))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a wrong current password: %d, want 403 (the session itself is fine)", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/auth/password/begin", laptop.Token,
		jsonOf(t, map[string]any{"current_auth_key": authtest.AuthKey}))
	if resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("begin: %d %s %s", resp.StatusCode, code, message)
	}
	var begun struct {
		PasswordWrap string         `json:"password_wrap"`
		Salt         string         `json:"salt"`
		KDF          map[string]any `json:"kdf"`
		Ticket       string         `json:"ticket"`
	}
	decodeInto(t, resp, &begun)
	if begun.PasswordWrap != laptop.PasswordWrap || begun.Ticket == "" || len(begun.Salt) != 22 {
		t.Fatalf("begin answered %+v", begun)
	}

	newKey := secret("a brand new password")
	// The ticket and the session alone, as an answer's log holds them: the
	// auth key that began the change is not there, and nothing changes.
	finish := map[string]any{
		"ticket": begun.Ticket, "auth_key": newKey, "kdf": defaultKDF(),
		"password_wrap": base64.RawURLEncoding.EncodeToString(authtest.Wrap(t)),
	}
	for _, current := range []string{"", secret("another")} {
		finish["current_auth_key"] = current
		if resp := h.do(t, http.MethodPost, "/v1/auth/password/finish", laptop.Token, jsonOf(t, finish)); resp.StatusCode/100 != 4 {
			t.Fatalf("finish with the current auth key %q: %d", current, resp.StatusCode)
		}
	}
	finish["current_auth_key"] = authtest.AuthKey
	resp = h.do(t, http.MethodPost, "/v1/auth/password/finish", laptop.Token, jsonOf(t, finish))
	if resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("finish: %d %s %s", resp.StatusCode, code, message)
	}
	var fresh sessionReply
	decodeInto(t, resp, &fresh)
	if fresh.Token == "" || fresh.Token == laptop.Token {
		t.Fatalf("the reply carried no new token: %+v", fresh)
	}
	for name, token := range map[string]string{"the session that asked": laptop.Token, "another device": phone.Token} {
		if resp := h.do(t, http.MethodGet, "/v1/auth/me", token, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s survived the change: %d", name, resp.StatusCode)
		}
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", fresh.Token, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the new token does not work: %d", resp.StatusCode)
	}
	h.signInWith(t, "ana@example.com", newKey)
}

func TestSigningOutEndsTheSessionOrEverySession(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	laptop := h.signIn(t, "ana@example.com")
	phone := h.signIn(t, "ana@example.com")
	tablet := h.signIn(t, "ana@example.com")

	if resp := h.do(t, http.MethodPost, "/v1/auth/logout", laptop.Token, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout without a body: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", laptop.Token, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a signed-out token still works: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", phone.Token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("signing out one browser ended another: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodPost, "/v1/auth/logout", phone.Token, `{"everywhere":true}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout everywhere: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", tablet.Token, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session survived signing out everywhere: %d", resp.StatusCode)
	}
}

func TestAnInviteLinkSignsUpItsAddressOnce(t *testing.T) {
	h := newHarnessWith(t, nil, serviceOptions{publicURL: "http://localhost:5174"})
	admin := h.key(t, auth.ScopeAdmin)

	resp := h.do(t, http.MethodPost, "/v1/users/invites", admin, `{"email":"ana@example.com"}`)
	if resp.StatusCode != http.StatusCreated {
		code, message := decodeError(t, resp)
		t.Fatalf("invite: %d %s %s", resp.StatusCode, code, message)
	}
	var invite struct {
		Email, Role, URL string
		ExpiresAt        int64 `json:"expires_at"`
	}
	decodeInto(t, resp, &invite)
	link, err := url.Parse(invite.URL)
	if err != nil || link.Host != "localhost:5174" || link.RawQuery != "" {
		t.Fatalf("invite url = %q: want the console origin with the secrets only in the fragment", invite.URL)
	}
	fragment, err := url.ParseQuery(link.Fragment)
	if err != nil || fragment.Get("email") != "ana@example.com" || fragment.Get("invite") == "" {
		t.Fatalf("fragment = %q", link.Fragment)
	}
	// The invite says what was asked for, and that is what its person gets.
	if invite.Role != "member" {
		t.Errorf("role = %q, want the member an invite without a role asks for", invite.Role)
	}

	signUp := func(email string) *http.Response {
		return h.do(t, http.MethodPost, "/v1/auth/signup", "", jsonOf(t, enrolment(t, authtest.AuthKey, authtest.RecoveryProof,
			map[string]any{"invite": fragment.Get("invite"), "email": email, "name": "Ana",
				"seal_id": openInvite(t, h, fragment.Get("invite"), email)})))
	}
	if resp := signUp("mallory@example.com"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another address used the invite: %d", resp.StatusCode)
	}
	resp = signUp("ana@example.com")
	if resp.StatusCode != http.StatusCreated {
		code, message := decodeError(t, resp)
		t.Fatalf("sign up: %d %s %s", resp.StatusCode, code, message)
	}
	var s sessionReply
	decodeInto(t, resp, &s)
	// Even the first person on a daemon: only an invite for an owner makes
	// one.
	if s.User.Role != "member" || s.User.Name != "Ana" {
		t.Errorf("user = %+v", s.User)
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", s.Token, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the sign-up token does not work: %d", resp.StatusCode)
	}
	if resp := signUp("ana@example.com"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the invite worked twice: %d", resp.StatusCode)
	}
}

func TestPollingAnAccountEveryTwoSecondsIsNeverThrottled(t *testing.T) {
	// The CLI waits for consent by asking every two seconds, and the console
	// every three, for up to ten minutes. Both prove a good credential each
	// time, and neither may ever see a 429 for it.
	clock := &fakeClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, func(h *api.Handler) {
		limits := ratelimit.DefaultAuth(nil)
		limits.PerIP.WithClock(clock.Now)
		limits.PerSubject.WithClock(clock.Now)
		h.Limits = limits
	}, serviceOptions{})

	cli := authtest.NewKey(t, h.store, auth.ScopeAdmin)
	resp := h.do(t, http.MethodPost, "/v1/accounts", cli, h.passwordAccount(t, "person@example.com"))
	var added struct{ Account struct{ ID string } }
	decodeInto(t, resp, &added)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleOwner)
	console := authtest.SignIn(t, h.users, "ana@example.com")
	// Each polls its own mailbox: an owner of the instance does not see the
	// operator's.
	var hers struct{ Account struct{ ID string } }
	decodeInto(t, h.do(t, http.MethodPost, "/v1/accounts", console, linkAs(t, console, h.passwordAccount(t, "ana@mail.example"))), &hers)
	consolePath := "/v1/accounts/" + hers.Account.ID

	path := "/v1/accounts/" + added.Account.ID
	for elapsed := time.Duration(0); elapsed <= 10*time.Minute; elapsed += time.Second {
		if elapsed%(2*time.Second) == 0 {
			if resp := h.do(t, http.MethodGet, path, cli, ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("the CLI's poll at %v: %d", elapsed, resp.StatusCode)
			}
		}
		if elapsed%(3*time.Second) == 0 {
			if resp := h.do(t, http.MethodGet, consolePath, console, ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("the console's poll at %v: %d", elapsed, resp.StatusCode)
			}
		}
		clock.Advance(time.Second)
	}
}

// statusOf sends GET path with a bearer token, as if forwarded for an
// address when from is not empty, and returns the status.
func (h *harness) statusOf(t *testing.T, path, token, from string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Error(err)
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if from != "" {
		req.Header.Set("X-Forwarded-For", from)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Error(err)
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// revokedKey is a key that existed and no longer works: what an MCP client
// still holds after its key was revoked.
func (h *harness) revokedKey(t *testing.T) string {
	t.Helper()
	key := authtest.NewKey(t, h.store, auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")
	if err := h.keys.Revoke(t.Context(), prefix); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestGuessingCredentialsIsThrottled(t *testing.T) {
	// The other half of the same limiter: misses at a key pay into that
	// key's bucket, keys that match nothing into their address's, and no
	// miss is ever charged to a credential somebody else holds.
	proxy := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	h := newHarnessWith(t, func(h *api.Handler) { h.Limits = ratelimit.DefaultAuth(proxy) }, serviceOptions{})
	get := func(token, from string) int { return h.statusOf(t, "/v1/accounts", token, from) }

	// Session tokens are 256 random bits found by one SHA-256: guessing them
	// is bounded by the address bucket, and costs a person signed in from
	// the same address nothing.
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleOwner)
	session := authtest.SignIn(t, h.users, "ana@example.com")
	guess := strings.Repeat("A", 43)
	for i := range 12 {
		if got := get(guess, "203.0.113.1"); got != http.StatusUnauthorized {
			t.Fatalf("session guess %d: %d", i+1, got)
		}
	}
	if got := get(session, "203.0.113.1"); got != http.StatusOK {
		t.Fatalf("a good session from the address that guessed: %d, want 200", got)
	}

	key := authtest.NewKey(t, h.store, auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")
	wrong := prefix + "." + strings.Repeat("B", 43)
	for i := range 10 {
		if got := get(wrong, fmt.Sprintf("198.51.100.%d", i+1)); got != http.StatusUnauthorized {
			t.Fatalf("key guess %d: %d", i+1, got)
		}
	}
	// Spreading the guesses across addresses does not help.
	if got := get(wrong, "198.51.100.200"); got != http.StatusTooManyRequests {
		t.Fatalf("an eleventh guess at one key from a fresh address: %d, want 429", got)
	}

	// Inventing a prefix per request does not help either: keys that match
	// nothing are counted per address. A real key from there still works.
	for i := range 10 {
		if got := get(fmt.Sprintf("%08x.%s", 0xf0000000+i, strings.Repeat("C", 43)), "192.0.2.1"); got != http.StatusUnauthorized {
			t.Fatalf("unknown key %d: %d", i+1, got)
		}
	}
	if got := get("f00000ff."+strings.Repeat("C", 43), "192.0.2.1"); got != http.StatusTooManyRequests {
		t.Fatalf("an eleventh unknown key from one address: %d, want 429", got)
	}
	good := authtest.NewKey(t, h.store, auth.ScopeRead)
	if got := get(good, "192.0.2.1"); got != http.StatusOK {
		t.Fatalf("a real key from the address that sent unknown ones: %d, want 200", got)
	}
}

func TestConcurrentWrongSecretsCannotOutrunTheFailureBucket(t *testing.T) {
	// Checking the bucket and charging it only after the check let every
	// request in a simultaneous burst see the same last token.
	proxy := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	h := newHarnessWith(t, func(h *api.Handler) { h.Limits = ratelimit.DefaultAuth(proxy) }, serviceOptions{})
	key := authtest.NewKey(t, h.store, auth.ScopeRead)
	prefix, _, _ := strings.Cut(key, ".")

	burst := func(token func(i int) string, from func(i int) string) map[int]int {
		var mu sync.Mutex
		statuses := map[int]int{}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 40 {
			wg.Go(func() {
				<-start
				got := h.statusOf(t, "/v1/accounts", token(i), from(i))
				mu.Lock()
				statuses[got]++
				mu.Unlock()
			})
		}
		close(start)
		wg.Wait()
		return statuses
	}

	// One real prefix, a wrong secret, forty addresses.
	got := burst(
		func(int) string { return prefix + "." + strings.Repeat("B", 43) },
		func(i int) string { return fmt.Sprintf("198.51.100.%d", i+1) })
	if got[http.StatusUnauthorized] > 10 || got[http.StatusUnauthorized]+got[http.StatusTooManyRequests] != 40 {
		t.Errorf("one key, forty addresses: %v; want at most 10 checked and the rest refused", got)
	}

	// Forty invented prefixes, one address.
	got = burst(
		func(i int) string { return fmt.Sprintf("%08x.%s", 0xe0000000+i, strings.Repeat("C", 43)) },
		func(int) string { return "192.0.2.7" })
	if got[http.StatusUnauthorized] > 10 || got[http.StatusUnauthorized]+got[http.StatusTooManyRequests] != 40 {
		t.Errorf("forty prefixes, one address: %v; want at most 10 checked and the rest refused", got)
	}
}

func TestAnotherCallersMissesNeverThrottleAGoodCredential(t *testing.T) {
	// One address is a whole office behind a NAT, or every client behind a
	// proxy MAIL_TRUSTED_PROXIES does not name. Somebody there sending bad
	// tokens — a stale tab, a revoked MCP client, an attacker — must not
	// lock out the console and the CLI polling beside them.
	clock := &fakeClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, func(h *api.Handler) {
		limits := ratelimit.DefaultAuth(nil)
		limits.PerIP.WithClock(clock.Now)
		limits.PerSubject.WithClock(clock.Now)
		h.Limits = limits
	}, serviceOptions{})

	cli := authtest.NewKey(t, h.store, auth.ScopeAdmin)
	resp := h.do(t, http.MethodPost, "/v1/accounts", cli, h.passwordAccount(t, "person@example.com"))
	var added struct{ Account struct{ ID string } }
	decodeInto(t, resp, &added)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleOwner)
	console := authtest.SignIn(t, h.users, "ana@example.com")
	// Each polls its own mailbox: an owner of the instance does not see the
	// operator's.
	var hers struct{ Account struct{ ID string } }
	decodeInto(t, h.do(t, http.MethodPost, "/v1/accounts", console, linkAs(t, console, h.passwordAccount(t, "ana@mail.example"))), &hers)
	consolePath := "/v1/accounts/" + hers.Account.ID
	path := "/v1/accounts/" + added.Account.ID

	// Every kind of bad bearer, all from 127.0.0.1 like the pollers.
	bogus := []string{
		strings.Repeat("A", 43),               // a session that ended
		h.revokedKey(t),                       // a revoked key
		"0badc0de." + strings.Repeat("C", 43), // a key that never existed
		"not.a.key",                           // nothing at all
	}
	for i := range 10 {
		if got := h.statusOf(t, "/v1/accounts", bogus[i%len(bogus)], ""); got == http.StatusOK {
			t.Fatalf("bogus bearer %d was accepted", i+1)
		}
	}
	// Then one a second for two minutes, the cheap kinds: the expensive
	// ones' own limit is TestGuessingCredentialsIsThrottled's business.
	for elapsed := time.Duration(0); elapsed <= 2*time.Minute; elapsed += time.Second {
		if got := h.statusOf(t, "/v1/accounts", bogus[int(elapsed/time.Second)%2], ""); got == http.StatusOK {
			t.Fatalf("a bogus bearer at %v was accepted", elapsed)
		}
		if elapsed%(2*time.Second) == 0 {
			if got := h.statusOf(t, path, cli, ""); got != http.StatusOK {
				t.Fatalf("the CLI's poll at %v: %d", elapsed, got)
			}
		}
		if elapsed%(3*time.Second) == 0 {
			for _, p := range []string{consolePath, "/v1/accounts", "/v1/auth/me"} {
				if got := h.statusOf(t, p, console, ""); got != http.StatusOK {
					t.Fatalf("the console's %s at %v: %d", p, elapsed, got)
				}
			}
		}
		clock.Advance(time.Second)
	}
}

func TestAnotherClientsFailuresNeverThrottleAValidPoller(t *testing.T) {
	// The spec's own case: the console every 3 s and the CLI every 2 s for
	// the whole ten minutes of a consent, while a client beside them fails
	// every 1.5 s. The failing client is throttled; the pollers never are.
	clock := &fakeClock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, func(h *api.Handler) {
		limits := ratelimit.DefaultAuth(nil)
		limits.PerIP.WithClock(clock.Now)
		limits.PerSubject.WithClock(clock.Now)
		h.Limits = limits
	}, serviceOptions{})

	cli := authtest.NewKey(t, h.store, auth.ScopeAdmin)
	resp := h.do(t, http.MethodPost, "/v1/accounts", cli, h.passwordAccount(t, "person@example.com"))
	var added struct{ Account struct{ ID string } }
	decodeInto(t, resp, &added)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleOwner)
	console := authtest.SignIn(t, h.users, "ana@example.com")
	// Each polls its own mailbox: an owner of the instance does not see the
	// operator's.
	var hers struct{ Account struct{ ID string } }
	decodeInto(t, h.do(t, http.MethodPost, "/v1/accounts", console, linkAs(t, console, h.passwordAccount(t, "ana@mail.example"))), &hers)
	consolePath := "/v1/accounts/" + hers.Account.ID
	path := "/v1/accounts/" + added.Account.ID
	revoked := h.revokedKey(t)
	ended := strings.Repeat("A", 43)

	// The failing client's first ten misses at its key are answered; the
	// eleventh is throttled.
	for i := range 10 {
		if got := h.statusOf(t, path, revoked, ""); got != http.StatusUnauthorized {
			t.Fatalf("revoked key, miss %d: %d", i+1, got)
		}
	}
	if got := h.statusOf(t, path, revoked, ""); got != http.StatusTooManyRequests {
		t.Fatalf("revoked key, eleventh miss: %d, want 429", got)
	}

	const tick = 500 * time.Millisecond
	for elapsed := time.Duration(0); elapsed <= 10*time.Minute; elapsed += tick {
		if elapsed%(1500*time.Millisecond) == 0 {
			token := ended
			if (elapsed/(1500*time.Millisecond))%2 == 1 {
				token = revoked
			}
			if got := h.statusOf(t, path, token, ""); got == http.StatusOK {
				t.Fatalf("the failing client got in at %v", elapsed)
			}
		}
		if elapsed%(2*time.Second) == 0 {
			if got := h.statusOf(t, path, cli, ""); got != http.StatusOK {
				t.Fatalf("the CLI's poll at %v: %d", elapsed, got)
			}
		}
		if elapsed%(3*time.Second) == 0 {
			if got := h.statusOf(t, consolePath, console, ""); got != http.StatusOK {
				t.Fatalf("the console's poll at %v: %d", elapsed, got)
			}
		}
		clock.Advance(tick)
	}
}
