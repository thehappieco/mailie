package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/ratelimit"
)

// The key scheme's ceremonies over REST (docs/key-scheme.md section 12).

// body reads an answer's body.
func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// meOf is GET /v1/auth/me as text, and its status.
func meOf(t *testing.T, h *harness, token string) (int, string) {
	t.Helper()
	resp := h.do(t, http.MethodGet, "/v1/auth/me", token, "")
	return resp.StatusCode, body(t, resp)
}

func TestNoRouteAnswersAWrapToASessionAlone(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	s := h.signIn(t, "ana@example.com")
	if len(s.PasswordWrap) != 82 {
		t.Fatalf("the sign-in answered the wrap %q: the auth key verified there is what opens it", s.PasswordWrap)
	}
	// The session alone: its own account, and a password change without
	// the current auth key.
	if status, me := meOf(t, h, s.Token); status != http.StatusOK || strings.Contains(me, "wrap") {
		t.Errorf("GET /v1/auth/me answered %d %s", status, me)
	}
	for _, c := range []struct{ path, body string }{
		{"/v1/auth/password/begin", `{"current_auth_key":""}`},
		{"/v1/auth/password/begin", jsonOf(t, map[string]any{"current_auth_key": secret("wrong")})},
	} {
		resp := h.do(t, http.MethodPost, c.path, s.Token, c.body)
		if text := body(t, resp); resp.StatusCode < 400 || strings.Contains(text, "wrap") {
			t.Errorf("POST %s %s answered %d %s", c.path, c.body, resp.StatusCode, text)
		}
	}
	// Nor does a session open a recovery: the proof does, on a public route.
	resp := h.do(t, http.MethodPost, "/v1/auth/recover/open", s.Token,
		jsonOf(t, map[string]any{"email": "ana@example.com", "recovery_proof": secret("wrong")}))
	if text := body(t, resp); resp.StatusCode != http.StatusUnauthorized || strings.Contains(text, "wrap") {
		t.Errorf("a recovery opened with a session and a wrong proof: %d %s", resp.StatusCode, text)
	}
}

func TestTheChallengeAnswersAnAccountsAddressAsOneWithoutAnAccount(t *testing.T) {
	h := newHarness(t, false)
	challenge := func(email string) string {
		t.Helper()
		resp := h.do(t, http.MethodPost, "/v1/auth/challenge", "", jsonOf(t, map[string]any{"email": email}))
		text := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("challenge for %s: %d %s", email, resp.StatusCode, text)
		}
		return text
	}
	before := challenge("ana@example.com")
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	if after := challenge("ana@example.com"); after != before {
		t.Errorf("the challenge answered %s before the account and %s after", before, after)
	}
	if other := challenge("bob@example.com"); other == before {
		t.Error("two addresses were answered one salt")
	}
	if resp := h.do(t, http.MethodPost, "/v1/auth/challenge", "", `{"email":"not an address"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a challenge for no address: %d", resp.StatusCode)
	}
}

func TestTheUpgradeOverRESTTakesThePasswordOnceAndNeverAgain(t *testing.T) {
	h := newHarness(t, false)
	old := authtest.NewLegacyUser(t, h.store, "old@example.com", auth.RoleMember)
	resp := h.do(t, http.MethodPost, "/v1/auth/challenge", "", `{"email":"old@example.com"}`)
	if text := body(t, resp); !strings.Contains(text, `"upgrade":true`) {
		t.Fatalf("the challenge for an account not upgraded: %s", text)
	}
	upgradeLogin := func(email, password string) (int, string) {
		resp := h.do(t, http.MethodPost, "/v1/auth/upgrade/login", "",
			jsonOf(t, map[string]any{"email": email, "password": password}))
		return resp.StatusCode, body(t, resp)
	}
	status, text := upgradeLogin("old@example.com", authtest.Password)
	var ticket struct {
		Ticket string `json:"ticket"`
		SealID string `json:"seal_id"`
		Salt   string `json:"salt"`
	}
	if status != http.StatusOK || !decodeText(text, &ticket) || ticket.Ticket == "" || ticket.SealID != old.SealID ||
		len(ticket.Salt) != 22 || strings.Contains(text, "token") {
		t.Fatalf("the upgrade's sign-in answered %d %s, want a ticket, the seal id and the target, and no session", status, text)
	}
	newKey := secret("upgraded")
	resp = h.do(t, http.MethodPost, "/v1/auth/upgrade/enrol", "",
		jsonOf(t, enrolment(t, newKey, secret("code"), map[string]any{"ticket": ticket.Ticket})))
	var s sessionReply
	decodeInto(t, resp, &s)
	if resp.StatusCode != http.StatusOK || s.Token == "" || s.AuthenticatedAt == 0 || s.User.PublicKey == "" ||
		s.User.SealID != ticket.SealID {
		t.Fatalf("the enrolment answered %d %+v", resp.StatusCode, s)
	}
	// From then on: no upgrade in the challenge, and the password in clear
	// answered exactly as a wrong one is.
	resp = h.do(t, http.MethodPost, "/v1/auth/challenge", "", `{"email":"old@example.com"}`)
	if text := body(t, resp); strings.Contains(text, "upgrade") {
		t.Errorf("the challenge after the upgrade: %s", text)
	}
	rightStatus, right := upgradeLogin("old@example.com", authtest.Password)
	wrongStatus, wrong := upgradeLogin("old@example.com", "not the password")
	if rightStatus != http.StatusUnauthorized || right != wrong || wrongStatus != rightStatus {
		t.Errorf("the old password after the upgrade: %d %s; a wrong one: %d %s", rightStatus, right, wrongStatus, wrong)
	}
	h.signInWith(t, "old@example.com", newKey)
}

// decodeText decodes JSON text into v, reporting whether it could.
func decodeText(text string, v any) bool {
	return json.Unmarshal([]byte(text), v) == nil
}

func TestARecoveryOverRESTKeepsTheAccountKeyAndEndsEverySession(t *testing.T) {
	h := newHarness(t, false)
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	s := h.signIn(t, "ana@example.com")

	resp := h.do(t, http.MethodPost, "/v1/auth/recover/open", "",
		jsonOf(t, map[string]any{"email": "ana@example.com", "recovery_proof": authtest.RecoveryProof}))
	var opened struct {
		SealID       string `json:"seal_id"`
		PublicKey    string `json:"public_key"`
		RecoveryWrap string `json:"recovery_wrap"`
		Ticket       string `json:"ticket"`
	}
	decodeInto(t, resp, &opened)
	if resp.StatusCode != http.StatusOK || opened.SealID != ana.SealID || opened.Ticket == "" || len(opened.RecoveryWrap) != 82 {
		t.Fatalf("recover/open answered %d %+v", resp.StatusCode, opened)
	}
	wrap := func() string { return base64.RawURLEncoding.EncodeToString(authtest.Wrap(t)) }
	newKey := secret("recovered")
	finish := map[string]any{
		"ticket": opened.Ticket, "auth_key": newKey, "kdf": defaultKDF(), "password_wrap": wrap(),
		"recovery_wrap": wrap(), "recovery_proof": secret("new code"),
	}
	// The ticket finishes only with the proof that opened the recovery, which
	// the answer carrying it never held.
	for name, c := range map[string]struct {
		current any
		status  int
		code    string
	}{
		"another proof":  {secret("guess"), http.StatusForbidden, "not_authorized"},
		"the new code's": {finish["recovery_proof"], http.StatusForbidden, "not_authorized"},
		"none":           {nil, http.StatusBadRequest, "bad_request"},
	} {
		finish["current_recovery_proof"] = c.current
		resp = h.do(t, http.MethodPost, "/v1/auth/recover/finish", "", jsonOf(t, finish))
		if code, _ := decodeError(t, resp); resp.StatusCode != c.status || code != c.code {
			t.Errorf("recover/finish with %s: %d %s, want %d %s", name, resp.StatusCode, code, c.status, c.code)
		}
	}
	finish["current_recovery_proof"] = authtest.RecoveryProof
	resp = h.do(t, http.MethodPost, "/v1/auth/recover/finish", "", jsonOf(t, finish))
	if resp.StatusCode != http.StatusNoContent {
		code, message := decodeError(t, resp)
		t.Fatalf("recover/finish: %d %s %s", resp.StatusCode, code, message)
	}
	if status, _ := meOf(t, h, s.Token); status != http.StatusUnauthorized {
		t.Errorf("a session survived the recovery: %d", status)
	}
	after := h.signInWith(t, "ana@example.com", newKey)
	if after.User.PublicKey != opened.PublicKey || after.User.SealID != ana.SealID {
		t.Errorf("the account key changed: %+v, was %s", after.User, opened.PublicKey)
	}
}

func TestAStepUpOverRESTRefreshesOnlyItsOwnSession(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, nil, serviceOptions{now: clock.Now})
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	laptop := h.signIn(t, "ana@example.com")
	phone := h.signIn(t, "ana@example.com")
	if laptop.AuthenticatedAt != clock.Now().Unix() {
		t.Fatalf("a sign-in's step-up time is %d, want now", laptop.AuthenticatedAt)
	}
	clock.Advance(auth.StepUpWindow + time.Second)
	if resp := h.do(t, http.MethodPost, "/v1/auth/stepup", laptop.Token, jsonOf(t, map[string]any{"auth_key": secret("x")})); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a step-up with a wrong auth key: %d", resp.StatusCode)
	}
	if _, me := meOf(t, h, laptop.Token); !strings.Contains(me, fmt.Sprintf(`"authenticated_at":%d`, laptop.AuthenticatedAt)) {
		t.Errorf("a refused step-up moved the step-up time: %s", me)
	}
	resp := h.do(t, http.MethodPost, "/v1/auth/stepup", laptop.Token, jsonOf(t, map[string]any{"auth_key": authtest.AuthKey}))
	var stepped struct {
		AuthenticatedAt int64 `json:"authenticated_at"`
	}
	decodeInto(t, resp, &stepped)
	if resp.StatusCode != http.StatusOK || stepped.AuthenticatedAt != clock.Now().Unix() {
		t.Fatalf("step-up answered %d %+v", resp.StatusCode, stepped)
	}
	if _, me := meOf(t, h, laptop.Token); !strings.Contains(me, fmt.Sprintf(`"authenticated_at":%d`, stepped.AuthenticatedAt)) {
		t.Errorf("the stepped-up session's time: %s", me)
	}
	// The other session was not stepped up with it.
	if _, me := meOf(t, h, phone.Token); !strings.Contains(me, fmt.Sprintf(`"authenticated_at":%d`, phone.AuthenticatedAt)) {
		t.Errorf("the other session's step-up time moved: %s", me)
	}
}

func TestASessionAloneCannotReplaceTheRecoveryCodeEvenRightAfterSignIn(t *testing.T) {
	// The sign-in limits on a clock of their own: the refused guesses below
	// spend the account's budget, as any guess at its password does
	// (TestASignInAndASessionsCeremoniesSpendOneBudgetPerAccount), and a
	// minute later it is whole again.
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, func(h *api.Handler) {
		limits := ratelimit.DefaultSignIn(nil)
		limits.PerIP.WithClock(clock.Now)
		limits.PerSubject.WithClock(clock.Now)
		h.SignInLimits = limits
	}, serviceOptions{})
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	// Ana's session token, copied right after her sign-in: its step-up
	// window is open, and its holder has neither her password nor her auth
	// key. A recovery code they could set would be a password they could
	// set (recover/open, recover/finish).
	stolen := h.signIn(t, "ana@example.com")
	theirs := secret("the thief's code")
	replace := func(token string, fields map[string]any) (int, string) {
		t.Helper()
		in := map[string]any{"recovery_wrap": base64.RawURLEncoding.EncodeToString(authtest.Wrap(t)), "recovery_proof": theirs}
		for k, v := range fields {
			in[k] = v
		}
		resp := h.do(t, http.MethodPost, "/v1/auth/recovery", token, jsonOf(t, in))
		if resp.StatusCode < 400 {
			return resp.StatusCode, ""
		}
		code, _ := decodeError(t, resp)
		return resp.StatusCode, code
	}
	for name, c := range map[string]struct {
		fields map[string]any
		status int
		code   string
	}{
		"no current auth key":      {nil, http.StatusBadRequest, "bad_request"},
		"a guessed current key":    {map[string]any{"current_auth_key": secret("guess")}, http.StatusForbidden, "not_authorized"},
		"the code's own proof":     {map[string]any{"current_auth_key": theirs}, http.StatusForbidden, "not_authorized"},
		"Ana's recovery proof too": {map[string]any{"current_auth_key": authtest.RecoveryProof}, http.StatusForbidden, "not_authorized"},
	} {
		if status, code := replace(stolen.Token, c.fields); status != c.status || code != c.code {
			t.Errorf("%s: %d %s, want %d %s", name, status, code, c.status, c.code)
		}
	}
	// So the thief's code opens no recovery, and Ana's password and code
	// still work.
	clock.Advance(time.Minute)
	resp := h.do(t, http.MethodPost, "/v1/auth/recover/open", "",
		jsonOf(t, map[string]any{"email": "ana@example.com", "recovery_proof": theirs}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the thief's code opened a recovery: %d", resp.StatusCode)
	}
	h.signIn(t, "ana@example.com")
	if status, _ := meOf(t, h, stolen.Token); status != http.StatusOK {
		t.Errorf("a refused replacement ended the session: %d", status)
	}
	// With her current auth key, Ana replaces it.
	mine := secret("Ana's new code")
	if status, code := replace(stolen.Token, map[string]any{"current_auth_key": authtest.AuthKey, "recovery_proof": mine}); status != http.StatusNoContent {
		t.Fatalf("the current auth key could not replace the code: %d %s", status, code)
	}
	resp = h.do(t, http.MethodPost, "/v1/auth/recover/open", "",
		jsonOf(t, map[string]any{"email": "ana@example.com", "recovery_proof": mine}))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the new code does not open a recovery: %d", resp.StatusCode)
	}
}

func TestAResetLinkOverRESTGivesANewAccountKeyAndEndsEverySession(t *testing.T) {
	h := newHarness(t, false)
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	s := h.signIn(t, "ana@example.com")
	code, _, err := h.users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	// The link answers what the new password is derived under, and only
	// for its own address.
	if resp := h.do(t, http.MethodPost, "/v1/auth/reset/open", "",
		jsonOf(t, map[string]any{"reset": code, "email": "bob@example.com"})); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a reset link opened for another address: %d", resp.StatusCode)
	}
	resp := h.do(t, http.MethodPost, "/v1/auth/reset/open", "", jsonOf(t, map[string]any{"reset": code, "email": "ana@example.com"}))
	var opened struct {
		Salt string         `json:"salt"`
		KDF  map[string]any `json:"kdf"`
	}
	decodeInto(t, resp, &opened)
	if resp.StatusCode != http.StatusOK || len(opened.Salt) != 22 || jsonOf(t, opened.KDF) != jsonOf(t, defaultKDF()) {
		t.Fatalf("the reset link opened %d %+v", resp.StatusCode, opened)
	}
	newKey := secret("after the reset")
	resp = h.do(t, http.MethodPost, "/v1/auth/reset", "",
		jsonOf(t, enrolment(t, newKey, secret("new code"), map[string]any{"reset": code, "email": "bob@example.com"})))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a reset link used for another address: %d", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/auth/reset", "",
		jsonOf(t, enrolment(t, newKey, secret("new code"), map[string]any{"reset": code, "email": "ana@example.com"})))
	var after sessionReply
	decodeInto(t, resp, &after)
	if resp.StatusCode != http.StatusOK || after.User.PublicKey == s.User.PublicKey || after.User.SealID != s.User.SealID {
		t.Fatalf("the reset answered %d %+v, want a new public key for the same seal id", resp.StatusCode, after)
	}
	if status, _ := meOf(t, h, s.Token); status != http.StatusUnauthorized {
		t.Errorf("a session survived the reset: %d", status)
	}
	h.signInWith(t, "ana@example.com", newKey)
	// What the challenge answers now is what the link answered: the
	// password chosen under it signs in again from any browser.
	resp = h.do(t, http.MethodPost, "/v1/auth/challenge", "", `{"email":"ana@example.com"}`)
	var challenged struct {
		Salt string `json:"salt"`
	}
	decodeInto(t, resp, &challenged)
	if challenged.Salt != opened.Salt {
		t.Errorf("after the reset the challenge answers %q, the link answered %q", challenged.Salt, opened.Salt)
	}
}

func TestASignUpOverRESTNamesTheSealIDOpeningItsInvitationAnswered(t *testing.T) {
	h := newHarness(t, false)
	code, _, err := h.users.CreateInvite(t.Context(), auth.NewInvite{Email: "new@example.com", Role: auth.RoleMember, CreatedBy: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, http.MethodPost, "/v1/auth/signup/open", "",
		jsonOf(t, map[string]any{"invite": code, "email": "other@example.com"})); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an invitation opened for another address: %d", resp.StatusCode)
	}
	resp := h.do(t, http.MethodPost, "/v1/auth/signup/open", "", jsonOf(t, map[string]any{"invite": code, "email": "new@example.com"}))
	var opened struct {
		Salt   string         `json:"salt"`
		KDF    map[string]any `json:"kdf"`
		SealID string         `json:"seal_id"`
	}
	decodeInto(t, resp, &opened)
	if resp.StatusCode != http.StatusOK || len(opened.Salt) != 22 || opened.SealID == "" ||
		jsonOf(t, opened.KDF) != jsonOf(t, defaultKDF()) {
		t.Fatalf("the invitation opened %d %+v", resp.StatusCode, opened)
	}
	signUp := func(seal string) *http.Response {
		return h.do(t, http.MethodPost, "/v1/auth/signup", "", jsonOf(t, enrolment(t, authtest.AuthKey, authtest.RecoveryProof,
			map[string]any{"invite": code, "email": "new@example.com", "name": "New", "seal_id": seal})))
	}
	if resp := signUp(""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a sign-up bound to no seal id: %d, want 409", resp.StatusCode)
	}
	resp = signUp(opened.SealID)
	var s sessionReply
	decodeInto(t, resp, &s)
	if resp.StatusCode != http.StatusCreated || s.User.SealID != opened.SealID {
		t.Fatalf("the sign-up answered %d %+v, want the seal id %q", resp.StatusCode, s.User, opened.SealID)
	}
}

func TestASignInAndASessionsCeremoniesSpendOneBudgetPerAccount(t *testing.T) {
	// A step-up, a password change's first step and a recovery code's
	// replacement are guesses at the same password a sign-in is. With a
	// bucket each, whoever holds a session would get five guesses a minute
	// through the sign-in and five more through each of them.
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, func(h *api.Handler) {
		limits := ratelimit.DefaultSignIn([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
		limits.PerIP.WithClock(clock.Now)
		limits.PerSubject.WithClock(clock.Now)
		h.SignInLimits = limits
	}, serviceOptions{})
	// Every request from an address of its own: only the account's budget
	// can run out.
	sent := 0
	post := func(path, token string, in map[string]any) int {
		t.Helper()
		sent++
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+path, strings.NewReader(jsonOf(t, in)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.%d.%d", sent/250, 1+sent%250))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp.StatusCode
	}
	signIn := func(email string) int {
		return post("/v1/auth/login", "", map[string]any{"email": email, "auth_key": secret("wrong")})
	}
	wrap := base64.RawURLEncoding.EncodeToString(authtest.Wrap(t))
	people := 0
	for _, c := range []struct {
		path string
		in   map[string]any
	}{
		{"/v1/auth/stepup", map[string]any{"auth_key": secret("wrong")}},
		{"/v1/auth/password/begin", map[string]any{"current_auth_key": secret("wrong")}},
		{"/v1/auth/recovery", map[string]any{"current_auth_key": secret("wrong"), "recovery_wrap": wrap, "recovery_proof": secret("code")}},
	} {
		person := func() (string, string) {
			people++
			email := fmt.Sprintf("person%d@example.com", people)
			authtest.NewUser(t, h.store, email, auth.RoleMember)
			return email, authtest.SignIn(t, h.users, email)
		}
		// Five sign-ins, then the session's ceremony; the address typed in
		// another case is the same account.
		email, session := person()
		for i := range 5 {
			if got := signIn(" " + strings.ToUpper(email)); got != http.StatusUnauthorized {
				t.Fatalf("%s: sign-in %d: %d", c.path, i+1, got)
			}
		}
		if got := post(c.path, session, c.in); got != http.StatusTooManyRequests {
			t.Errorf("%s after five sign-ins: %d, want 429", c.path, got)
		}
		// And the other way round.
		email, session = person()
		for i := range 5 {
			if got := post(c.path, session, c.in); got != http.StatusForbidden {
				t.Fatalf("%s: guess %d: %d", c.path, i+1, got)
			}
		}
		if got := signIn(email); got != http.StatusTooManyRequests {
			t.Errorf("a sign-in after five guesses through %s: %d, want 429", c.path, got)
		}
	}
	// The budget is the account's, not the routes': another person's is
	// whole.
	if got := signIn("someone@example.com"); got != http.StatusUnauthorized {
		t.Errorf("another account was throttled too: %d", got)
	}
}

func TestTheRecoveryAndTheUpgradeAreRateLimitedPerAccount(t *testing.T) {
	h := newHarnessWith(t, func(h *api.Handler) {
		h.SignInLimits = ratelimit.DefaultSignIn([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	}, serviceOptions{})
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	authtest.NewLegacyUser(t, h.store, "old@example.com", auth.RoleMember)
	for _, c := range []struct{ path, field, value, email string }{
		{"/v1/auth/recover/open", "recovery_proof", secret("wrong"), "ana@example.com"},
		{"/v1/auth/upgrade/login", "password", "not the password", "old@example.com"},
	} {
		attempt := func(email, from string) *http.Response {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+c.path,
				strings.NewReader(jsonOf(t, map[string]any{"email": email, c.field: c.value})))
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
			if resp := attempt(c.email, fmt.Sprintf("203.0.113.%d", i+1)); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: guess %d: %d", c.path, i+1, resp.StatusCode)
			}
		}
		if resp := attempt(" "+strings.ToUpper(c.email), "198.51.100.7"); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s: a sixth guess from a fresh address: %d, want 429", c.path, resp.StatusCode)
		}
	}
}
