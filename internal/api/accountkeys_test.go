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
	resp = h.do(t, http.MethodPost, "/v1/auth/recover/finish", "", jsonOf(t, map[string]any{
		"ticket": opened.Ticket, "auth_key": newKey, "kdf": defaultKDF(), "password_wrap": wrap(),
		"recovery_wrap": wrap(), "recovery_proof": secret("new code"),
	}))
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

func TestAStepUpOverRESTGuardsTheRecoveryCodeAndRefreshesOnlyItsSession(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, nil, serviceOptions{now: clock.Now})
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	laptop := h.signIn(t, "ana@example.com")
	phone := h.signIn(t, "ana@example.com")
	if laptop.AuthenticatedAt != clock.Now().Unix() {
		t.Fatalf("a sign-in's step-up time is %d, want now", laptop.AuthenticatedAt)
	}
	replace := func(token string) int {
		resp := h.do(t, http.MethodPost, "/v1/auth/recovery", token, jsonOf(t, map[string]any{
			"recovery_wrap": base64.RawURLEncoding.EncodeToString(authtest.Wrap(t)), "recovery_proof": secret("replaced"),
		}))
		return resp.StatusCode
	}
	clock.Advance(auth.StepUpWindow + time.Second)
	if status := replace(laptop.Token); status != http.StatusForbidden {
		t.Fatalf("replacing the recovery code past the window: %d, want 403", status)
	}
	if resp := h.do(t, http.MethodPost, "/v1/auth/stepup", laptop.Token, jsonOf(t, map[string]any{"auth_key": secret("x")})); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a step-up with a wrong auth key: %d", resp.StatusCode)
	}
	resp := h.do(t, http.MethodPost, "/v1/auth/stepup", laptop.Token, jsonOf(t, map[string]any{"auth_key": authtest.AuthKey}))
	var stepped struct {
		AuthenticatedAt int64 `json:"authenticated_at"`
	}
	decodeInto(t, resp, &stepped)
	if resp.StatusCode != http.StatusOK || stepped.AuthenticatedAt != clock.Now().Unix() {
		t.Fatalf("step-up answered %d %+v", resp.StatusCode, stepped)
	}
	if status := replace(laptop.Token); status != http.StatusNoContent {
		t.Errorf("replacing the recovery code after a step-up: %d", status)
	}
	// The other session was not stepped up with it.
	if status := replace(phone.Token); status != http.StatusForbidden {
		t.Errorf("the other session replaced the recovery code: %d", status)
	}
	if _, me := meOf(t, h, phone.Token); !strings.Contains(me, fmt.Sprintf(`"authenticated_at":%d`, phone.AuthenticatedAt)) {
		t.Errorf("the other session's step-up time moved: %s", me)
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
