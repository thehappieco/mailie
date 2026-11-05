package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// A link at the provider over HTTP (internal/service/links.go): its start,
// its reading by its starter, and its return through the callback.

// linkClient is the web client a link harness's Google registration has.
const linkClient = "google-web-client"

// linkProvider stands in for Google behind a link: a token endpoint whose
// answer to a code carries an ID token naming the address it is told to,
// and the mail server that address logs in to.
type linkProvider struct {
	server *httptest.Server
	imap   *providertest.IMAPServer

	mu     sync.Mutex
	issued int
	// email is the address the next ID token names; empty names none.
	email string
	// refuse makes the mail server refuse what the token endpoint issues.
	refuse bool
}

func newLinkProvider(t *testing.T) *linkProvider {
	t.Helper()
	p := &linkProvider{imap: providertest.NewIMAPServer(t, providertest.IMAPOptions{AnyUser: true})}
	p.server = httptest.NewServer(http.HandlerFunc(p.token))
	t.Cleanup(p.server.Close)
	return p
}

func (p *linkProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issued++
	access := "access-" + strconv.Itoa(p.issued)
	if !p.refuse {
		p.imap.AcceptToken(access)
	}
	answer := map[string]any{
		"access_token": access, "refresh_token": "refresh-" + strconv.Itoa(p.issued), "token_type": "Bearer",
		"expires_in": 3600, "scope": "https://mail.google.com/ openid https://www.googleapis.com/auth/userinfo.email",
	}
	if r.Form.Get("grant_type") == "authorization_code" {
		// An ID token's times are on the wall clock, whatever the harness's.
		now := time.Now()
		claims := map[string]any{
			"iss": "https://accounts.google.com", "aud": linkClient, "azp": linkClient, "sub": "108309262217400187115",
			"email_verified": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
		if p.email != "" {
			claims["email"] = p.email
		}
		payload, _ := json.Marshal(claims)
		answer["id_token"] = "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// names makes the provider's next answer name email, and its mail server
// take or refuse what it issues.
func (p *linkProvider) names(email string, refuse bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.email, p.refuse = email, refuse
}

// registry is the daemon's configuration with this provider as Google's.
func (p *linkProvider) registry() account.RegistryOptions {
	return account.RegistryOptions{
		Google: account.OAuthClient{ClientID: "google-installed-client"},
		GoogleWeb: account.OAuthClient{ClientID: linkClient, ClientSecret: "not-a-real-secret",
			Endpoint: oauth2.Endpoint{AuthURL: p.server.URL + "/authorize", TokenURL: p.server.URL + "/token"}},
		RedirectIMAP: map[string]string{"imap.gmail.com:993": p.imap.Addr},
	}
}

// newLinkHarness is a daemon whose console is at http://localhost:5174 and
// whose Google is p, on the clock now gives (nil is the wall clock).
func newLinkHarness(t *testing.T, p *linkProvider, now func() time.Time) *harness {
	t.Helper()
	return newHarnessWith(t, func(h *api.Handler) { h.Limits = nil }, serviceOptions{
		publicURL: "http://localhost:5174", now: now, registry: p.registry(),
	})
}

// startLink starts a Gmail link as token and returns its id and its state.
func (h *harness) startLink(t *testing.T, token string) (id, state string) {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/links", token, `{"provider":"gmail"}`)
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("starting a link: %d %s %s", resp.StatusCode, code, msg)
	}
	var started struct {
		Link struct {
			ID string `json:"id"`
		} `json:"link"`
		Auth struct {
			AuthURL string `json:"auth_url"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(started.Auth.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	return started.Link.ID, u.Query().Get("state")
}

// linkReturn is the callback's body for a link's return with a code and the
// mailbox key pair n.
func linkReturn(t *testing.T, state string, n byte) string {
	t.Helper()
	return jsonOf(t, contractLinkKey(n, map[string]any{
		"redirect_url": "http://localhost:5174/oauth/return?code=the-code&state=" + url.QueryEscape(state),
	}))
}

func TestALinkIsStartedOnlyByAPersonAndReadOnlyByItsStarter(t *testing.T) {
	p := newLinkProvider(t)
	h := newLinkHarness(t, p, nil)
	authtest.NewUser(t, h.store, "ana@example.test", auth.RoleOwner)
	authtest.NewUser(t, h.store, "bob@example.test", auth.RoleMember)
	ana := authtest.SignIn(t, h.users, "ana@example.test")
	bob := authtest.SignIn(t, h.users, "bob@example.test")

	// A key, even the operator's, starts none: a link's key is sealed to a
	// person, and its return is theirs.
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/links", h.key(t, auth.ScopeAdmin),
		`{"provider":"gmail"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an instance key starting a link: %d, want 403", resp.StatusCode)
	}
	// No address is asked for, and none is taken: an unknown field is
	// refused, as on every route.
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/links", ana,
		`{"provider":"gmail","email":"ana@gmail.com"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a link naming an address: %d, want 400", resp.StatusCode)
	}

	id, state := h.startLink(t, ana)
	for name, token := range map[string]string{"another person": bob, "an instance key": h.key(t, auth.ScopeAdmin)} {
		resp := h.do(t, http.MethodGet, "/v1/accounts/oauth/links/"+id, token, "")
		if want := map[string]int{"another person": http.StatusNotFound, "an instance key": http.StatusForbidden}[name]; resp.StatusCode != want {
			t.Errorf("%s reading ana's link: %d, want %d", name, resp.StatusCode, want)
		}
	}
	// Bob posting ana's return is answered as a state nobody knows, and the
	// link stays hers to finish.
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", bob, linkReturn(t, state, 1)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob posting ana's return: %d, want 404", resp.StatusCode)
	}

	p.names("ana.lima@gmail.com", false)
	resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", ana, linkReturn(t, state, 1))
	if resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("ana's return: %d %s %s", resp.StatusCode, code, msg)
	}
	var created struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		State string `json:"state"`
	}
	decodeInto(t, resp, &created)
	if created.Email != "ana.lima@gmail.com" || created.State != "active" {
		t.Errorf("the mailbox the return created: %+v", created)
	}
	var link struct {
		State     string `json:"state"`
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
	}
	decodeInto(t, h.do(t, http.MethodGet, "/v1/accounts/oauth/links/"+id, ana, ""), &link)
	if link.State != "connected" || link.AccountID != created.ID || link.Email != "" {
		t.Errorf("ana's link once connected: %+v, want connected to %s with no address", link, created.ID)
	}
}

func TestALinksReturnCarriesItsKeyAndAConsentsNever(t *testing.T) {
	p := newLinkProvider(t)
	h := newLinkHarness(t, p, nil)
	authtest.NewUser(t, h.store, "ana@example.test", auth.RoleOwner)
	ana := authtest.SignIn(t, h.users, "ana@example.test")

	// A link's return with a code and no key is refused before its code is
	// taken: the same return with the key finishes it.
	_, state := h.startLink(t, ana)
	bare := fmt.Sprintf(`{"redirect_url":"http://localhost:5174/oauth/return?code=the-code&state=%s"}`, url.QueryEscape(state))
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", ana, bare); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a link's return without its key: %d, want 400", resp.StatusCode)
	}
	p.names("ana.lima@gmail.com", false)
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", ana, linkReturn(t, state, 2)); resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("the same return with its key: %d %s %s", resp.StatusCode, code, msg)
	}

	// A consent for a mailbox that exists carries none.
	added := h.do(t, http.MethodPost, "/v1/accounts", ana,
		jsonOf(t, contractLinkKey(3, map[string]any{"email": "ana@gmail.com"})))
	var result struct {
		Auth struct {
			AuthURL string `json:"auth_url"`
		} `json:"auth"`
	}
	decodeInto(t, added, &result)
	u, err := url.Parse(result.Auth.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, http.MethodPost, "/v1/accounts/oauth/callback", ana,
		linkReturn(t, u.Query().Get("state"), 4)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a consent's return carrying a key: %d, want 400", resp.StatusCode)
	}
}
