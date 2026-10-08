package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func newRepo(t *testing.T) (*account.Repository, *store.Store) {
	t.Helper()
	db := storetest.New(t)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xA1)})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return account.NewRepository(db, keyring), db
}

func bytesOf(b byte) []byte {
	out := make([]byte, secrets.KeyLen)
	for i := range out {
		out[i] = b
	}
	return out
}

func seed(t *testing.T, repo *account.Repository, email string, kind provider.Kind) account.Account {
	t.Helper()
	a, err := repo.Create(t.Context(), account.Account{
		ID: "acc_" + strings.ReplaceAll(email, "@", "_"), Email: email, Provider: kind,
		AuthKind: "oauth2", IMAPHost: "imap.example.com", IMAPPort: 993,
		SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPTLS: "starttls", LoginUser: email,
	}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func TestAnAccountRoundTripsThroughTheDatabase(t *testing.T) {
	repo, _ := newRepo(t)
	created := seed(t, repo, "person@example.com", provider.KindGmail)

	got, err := repo.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != created.Email || got.Provider != provider.KindGmail {
		t.Fatalf("round trip changed the account: %+v", got)
	}
	if got.State != account.StatePendingAuth {
		t.Errorf("state = %q; a new account has nothing to authenticate with yet", got.State)
	}
	if got.InitialDays != 90 {
		t.Errorf("initial window = %d, want the 90 day default", got.InitialDays)
	}
}

func TestAnAddressIsLinkedOnceInAWorkspace(t *testing.T) {
	// Two rows for one mailbox in one workspace would each open their own
	// IMAP sessions and index the same messages under different ids, for the
	// same people. (In another workspace it is another mailbox: see
	// TestTheSameAddressInTwoWorkspacesIsTwoIndependentMailboxes.)
	repo, _ := newRepo(t)
	seed(t, repo, "person@example.com", provider.KindGmail)

	_, err := repo.Create(t.Context(), account.Account{
		ID: "acc_other", Email: "Person@Example.com", Provider: provider.KindGmail,
		AuthKind: "oauth2", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 587,
		SMTPTLS: "starttls", LoginUser: "person@example.com",
	}, "")
	if !errors.Is(err, account.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate for an address differing only in case, got %v", err)
	}
}

func TestAStoredTokenIsEncryptedAndComesBackIntact(t *testing.T) {
	repo, db := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindMicrosoft)

	want := &oauth2.Token{
		AccessToken:  "ya29.access",
		RefreshToken: "1//0eXaMpLe-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	if err := repo.SaveToken(t.Context(), a.ID, want); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	var ciphertext []byte
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT ciphertext FROM credentials WHERE account_id = ? AND field = 'oauth_token'`, a.ID).
		Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if strings.Contains(string(ciphertext), "0eXaMpLe") {
		t.Fatal("the refresh token is readable in the database")
	}

	got, err := repo.Token(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got.RefreshToken != want.RefreshToken || got.AccessToken != want.AccessToken {
		t.Fatalf("token did not round trip: %+v", got)
	}
}

func TestDecryptingWithTheWrongKeySaysWhatToCheck(t *testing.T) {
	// Losing the credential key is a real operational event, and the error
	// has to point at it rather than at "decryption failed".
	db := storetest.New(t)
	original, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, original)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	if err := repo.SavePassword(t.Context(), a.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}

	wrong, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xB2)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = account.NewRepository(db, wrong).Password(t.Context(), a.ID)
	// Named by what the sealer says it seals with, whatever its kind.
	for _, says := range []string{wrong.Describe(), "is the key it was sealed with"} {
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Fatalf("the error should name the key to check (%q), got %v", says, err)
		}
	}
}

func TestDeletingAnAccountTakesItsCredentialsWithIt(t *testing.T) {
	repo, db := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	if err := repo.SavePassword(t.Context(), a.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(t.Context(), a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d credentials survived the account", n)
	}
}

func TestAPendingFlowCanOnlyBeUsedOnce(t *testing.T) {
	// The redirect URL carries the code in a query string, which ends up in
	// browser history and sometimes in a chat window.
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	flow := account.PendingFlow{
		State: "state-1", AccountID: a.ID, Flow: account.FlowLoopback,
		Verifier: "verifier", RedirectURI: "http://127.0.0.1:1234/oauth/callback",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := repo.SaveFlow(t.Context(), flow); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.TakeFlow(t.Context(), "state-1"); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := repo.TakeFlow(t.Context(), "state-1"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("a replayed redirect should find nothing, got %v", err)
	}
}

func TestAnExpiredFlowIsRefusedWithAnInstruction(t *testing.T) {
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	if err := repo.SaveFlow(t.Context(), account.PendingFlow{
		State: "old", AccountID: a.ID, Flow: account.FlowPasted,
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.TakeFlow(t.Context(), "old")
	if !errors.Is(err, account.ErrFlowExpired) || !strings.Contains(err.Error(), "start again") {
		t.Fatalf("want an expiry message telling the person what to do, got %v", err)
	}
}

func TestACodeIsReadOutOfAWholeRedirectURL(t *testing.T) {
	// Pasting the address the browser landed on is one action; finding the
	// code inside it is three.
	rd, err := account.ParseRedirect(
		"http://127.0.0.1:53682/oauth/callback?state=abc&code=4/0Adeu5&scope=https://mail.google.com/")
	if err != nil {
		t.Fatalf("ParseRedirect: %v", err)
	}
	if rd.Code != "4/0Adeu5" || rd.State != "abc" {
		t.Fatalf("redirect = %+v", rd)
	}
}

func TestADeclinedConsentIsReadWithItsState(t *testing.T) {
	// The state still arrives with a refusal, and it is what ties the refusal
	// to the flow whose account should say so.
	rd, err := account.ParseRedirect(
		"http://127.0.0.1:1/oauth/callback?error=access_denied&error_description=The+user+denied+the+request&state=abc")
	if err != nil {
		t.Fatalf("ParseRedirect: %v", err)
	}
	if rd.Error != "access_denied" || rd.State != "abc" || rd.Code != "" {
		t.Fatalf("redirect = %+v", rd)
	}
}

func TestAProviderFailureOnTheRedirectIsNotRecordedAsADecline(t *testing.T) {
	// A registration the provider will not serve, or the provider having an
	// outage, used to be recorded as the person saying no, and the operator
	// never learned that the configuration was broken.
	for _, c := range []struct {
		code   string
		want   error
		reason string
	}{
		{"access_denied", account.ErrConsentDeclined, account.ReasonDeclined},
		{"consent_required", account.ErrConsentDeclined, account.ReasonDeclined},
		{"login_required", account.ErrConsentDeclined, account.ReasonDeclined},
		{"unauthorized_client", account.ErrClientRejected, account.ReasonClientRejected},
		{"invalid_client", account.ErrClientRejected, account.ReasonClientRejected},
		{"invalid_scope", account.ErrClientRejected, account.ReasonClientRejected},
		{"server_error", account.ErrProviderRefused, account.ReasonRefused},
		{"temporarily_unavailable", account.ErrProviderRefused, account.ReasonRefused},
		{"something_new", account.ErrProviderRefused, account.ReasonRefused},
	} {
		reason, err := account.RefusalForTest(c.code, "Some <b>provider</b> text")
		if !errors.Is(err, c.want) || reason != c.reason {
			t.Errorf("error=%s: %v, reason %q; want %v, %q", c.code, err, reason, c.want, c.reason)
		}
		if !errors.Is(c.want, account.ErrConsentDeclined) && errors.Is(err, account.ErrConsentDeclined) {
			t.Errorf("error=%s is recorded as a decline", c.code)
		}
		// The provider's own words stay in the log; the reason a caller may
		// show is one of this package's fixed phrases.
		if strings.Contains(reason, "provider</b>") {
			t.Errorf("error=%s: the reason quotes the redirect: %q", c.code, reason)
		}
	}
}

func TestARedirectWithoutACodeSaysWhatToPaste(t *testing.T) {
	_, err := account.ParseRedirect("http://127.0.0.1:1/oauth/callback")
	if !errors.Is(err, account.ErrBadRedirect) || !strings.Contains(err.Error(), "paste") {
		t.Fatalf("want guidance, got %v", err)
	}
}

// --- the token source ------------------------------------------------------

// fakeIDP is a token endpoint that rotates refresh tokens the way Entra does.
type fakeIDP struct {
	server      *httptest.Server
	mu          chan struct{}
	issued      int
	failWith    string
	lastRefresh string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{mu: make(chan struct{}, 1)}
	idp.mu <- struct{}{}
	idp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-idp.mu
		defer func() { idp.mu <- struct{}{} }()

		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		idp.lastRefresh = r.Form.Get("refresh_token")

		if idp.failWith != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			body := `{"error":"` + idp.failWith + `"}`
			if idp.failWith == "invalid_grant" {
				// Entra attaches the code that says which kind of dead.
				body = `{"error":"invalid_grant","error_codes":[70008],` +
					`"error_description":"AADSTS70008: The refresh token has expired due to inactivity"}`
			}
			_, _ = w.Write([]byte(body))
			return
		}

		idp.issued++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-" + itoa(idp.issued),
			// Rotated on every use, exactly as Entra does.
			"refresh_token": "refresh-" + itoa(idp.issued),
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(idp.server.Close)
	return idp
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func (f *fakeIDP) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: "client",
		Endpoint: oauth2.Endpoint{TokenURL: f.server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams},
	}
}

func TestARotatedRefreshTokenIsPersistedBeforeItIsUsed(t *testing.T) {
	// Microsoft returns a new refresh token every time and does not revoke the
	// old one immediately, so a daemon that forgets to write it down works
	// until it restarts and then cannot authenticate at all — weeks later,
	// with no obvious cause.
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindMicrosoft)
	idp := newFakeIDP(t)

	expired := &oauth2.Token{
		AccessToken: "old-access", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}
	if err := repo.SaveToken(t.Context(), a.ID, expired); err != nil {
		t.Fatal(err)
	}

	source := account.NewTokenSourceForTest(t.Context(), a.ID, idp.config(), expired, repo)
	token, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "access-1" {
		t.Fatalf("access token = %q, want the refreshed one", token)
	}

	stored, err := repo.Token(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh-1" {
		t.Fatalf("the rotated refresh token was not persisted: %q", stored.RefreshToken)
	}
}

func TestInvalidateForcesARefreshEvenWhenTheTokenLooksValid(t *testing.T) {
	// A server can refuse a token the library still believes in: clock skew,
	// an early server-side expiry, a grant revoked and restored. Without this,
	// the retry after a rejected login presents the same credential again.
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	idp := newFakeIDP(t)

	valid := &oauth2.Token{
		AccessToken: "still-valid", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(time.Hour),
	}
	if err := repo.SaveToken(t.Context(), a.ID, valid); err != nil {
		t.Fatal(err)
	}
	source := account.NewTokenSourceForTest(t.Context(), a.ID, idp.config(), valid, repo)

	first, err := source.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first != "still-valid" {
		t.Fatalf("a valid token should be reused, got %q", first)
	}

	source.Invalidate()
	second, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token after Invalidate: %v", err)
	}
	if second == first {
		t.Fatal("Invalidate did not force a refresh; the retry would present the rejected token again")
	}
}

func TestADeadGrantStopsTheAccountAndSaysWhy(t *testing.T) {
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindMicrosoft)
	idp := newFakeIDP(t)
	idp.failWith = "invalid_grant"

	expired := &oauth2.Token{
		AccessToken: "old", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}
	source := account.NewTokenSourceForTest(t.Context(), a.ID, idp.config(), expired, repo)

	_, err := source.Token(t.Context())
	if !errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("want ErrNeedsReauth, got %v", err)
	}

	got, err := repo.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != account.StateNeedsReauth {
		t.Fatalf("account state = %q, want needs_reauth", got.State)
	}
	// The AADSTS code is the one part of an Entra error worth showing someone.
	if !strings.Contains(got.StateReason, "AADSTS70008") {
		t.Errorf("state reason = %q, want it to carry the provider's code", got.StateReason)
	}
}

func TestAnInvalidClientIsAConfigErrorNotADeadGrant(t *testing.T) {
	// A rotated or expired client secret refuses every account's refresh at
	// once. Marking each one for re-consent would send every person through a
	// consent screen to fix a setting the operator changes in one place.
	for name, body := range map[string]string{
		"google":                     `{"error":"invalid_client","error_description":"The OAuth client was not found."}`,
		"microsoft":                  `{"error":"invalid_client","error_codes":[7000222],"error_description":"AADSTS7000222: The provided client secret keys are expired."}`,
		"wrong client for the grant": `{"error":"unauthorized_client"}`,
	} {
		repo, _ := newRepo(t)
		a := seed(t, repo, "person@example.com", provider.KindMicrosoft)
		if err := repo.SetState(t.Context(), a.ID, account.StateActive, ""); err != nil {
			t.Fatal(err)
		}
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(idp.Close)
		config := &oauth2.Config{
			ClientID: "client",
			Endpoint: oauth2.Endpoint{TokenURL: idp.URL + "/token", AuthStyle: oauth2.AuthStyleInParams},
		}
		expired := &oauth2.Token{
			AccessToken: "old", RefreshToken: "refresh-0",
			TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
		}

		_, err := account.NewTokenSourceForTest(t.Context(), a.ID, config, expired, repo).Token(t.Context())
		if errors.Is(err, provider.ErrNeedsReauth) {
			t.Errorf("%s: a refused client was taken for a dead grant: %v", name, err)
		}
		if !errors.Is(err, account.ErrClientRejected) || !errors.Is(err, provider.ErrTemporary) {
			t.Errorf("%s: err = %v, want ErrClientRejected, retried as temporary", name, err)
		}
		got, err := repo.Get(t.Context(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != account.StateActive {
			t.Errorf("%s: the account became %s over the server's configuration", name, got.State)
		}
	}
}

func TestAnIdentityProviderHiccupDoesNotStopTheAccount(t *testing.T) {
	// Treating every refresh failure as fatal would park a working account
	// over a network blip.
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	idp := newFakeIDP(t)
	idp.failWith = "temporarily_unavailable"

	expired := &oauth2.Token{
		AccessToken: "old", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}
	source := account.NewTokenSourceForTest(t.Context(), a.ID, idp.config(), expired, repo)

	_, err := source.Token(t.Context())
	if !errors.Is(err, provider.ErrTemporary) {
		t.Fatalf("want ErrTemporary, got %v", err)
	}
	got, err := repo.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == account.StateNeedsReauth {
		t.Fatal("a temporary failure must not ask the user to re-consent")
	}
}

func TestTheTokenSourceOutlivesTheRequestThatBuiltIt(t *testing.T) {
	// oauth2 reuses the context it was given for every later refresh, so a
	// source built on a request context stops refreshing the moment that
	// request ends — hours later, with nothing in the logs to connect them.
	repo, _ := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	idp := newFakeIDP(t)

	daemonCtx := context.Background()
	requestCtx, cancel := context.WithCancel(context.Background())

	expired := &oauth2.Token{
		AccessToken: "old", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}
	source := account.NewTokenSourceForTest(daemonCtx, a.ID, idp.config(), expired, repo)

	cancel() // the request that happened to trigger the first refresh is over
	if _, err := source.Token(requestCtx); err != nil {
		t.Fatalf("a refresh must not depend on the request context: %v", err)
	}
}

func TestARefreshedTokenIsSealedEvenWhenTheRequestThatAskedForItIsCancelled(t *testing.T) {
	// A sealer may call a key service, under the context it is given. By the
	// time a refreshed token is sealed the provider has rotated it, and the
	// old refresh token is spent: the request's cancellation must not reach
	// that call, which runs under the persist's own deadline.
	db := storetest.New(t)
	kms := secretstest.New("kms")
	repo := account.NewRepository(db, kms)
	a := seed(t, repo, "person@example.com", provider.KindMicrosoft)
	idp := newFakeIDP(t)
	expired := &oauth2.Token{
		AccessToken: "old", RefreshToken: "refresh-0",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}
	if err := repo.SaveToken(t.Context(), a.ID, expired); err != nil {
		t.Fatal(err)
	}
	source := account.NewTokenSourceForTest(t.Context(), a.ID, idp.config(), expired, repo)

	requestCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.Token(requestCtx); err != nil {
		t.Fatalf("the refreshed token was not persisted: %v", err)
	}
	stored, err := repo.Token(t.Context(), a.ID)
	if err != nil || stored.RefreshToken != "refresh-1" {
		t.Fatalf("stored %+v, %v; want the rotated refresh token", stored, err)
	}
	// An envelope no keyring key sealed is kept under key id 0.
	var keyID int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT keyid FROM credentials WHERE account_id = ?`, a.ID).Scan(&keyID); err != nil || keyID != 0 {
		t.Fatalf("keyid = %d (%v), want 0", keyID, err)
	}
}

func TestOnlyACredentialThatDoesNotOpenAsksWhetherItsKeyIsConfigured(t *testing.T) {
	// The hint sends the operator after a key; a key service that did not
	// answer is no reason to, and the hint names the sealer, whatever kind.
	db := storetest.New(t)
	kms := secretstest.New("kms")
	repo := account.NewRepository(db, kms)
	a := seed(t, repo, "person@example.com", provider.KindIMAP)
	if err := repo.SavePassword(t.Context(), a.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}

	unreachable := errors.New("kms: the key service did not answer")
	kms.FailOpens(unreachable)
	_, err := repo.Password(t.Context(), a.ID)
	if !errors.Is(err, unreachable) || secrets.DoesNotOpen(err) || strings.Contains(err.Error(), "configured keys") {
		t.Fatalf("an unreachable key service: %v", err)
	}
	kms.FailOpens(nil)

	other := account.NewRepository(db, secretstest.New("another"))
	_, err = other.Password(t.Context(), a.ID)
	if !secrets.DoesNotOpen(err) || !strings.Contains(err.Error(), "configured keys") ||
		!strings.Contains(err.Error(), "test sealer another") {
		t.Fatalf("a password no configured key opens: %v", err)
	}
}

func TestTheGoogleFlowAsksForOfflineAccessAndAFreshConsent(t *testing.T) {
	// Without offline access there is no refresh token and the account dies
	// within the hour; without prompt=consent a second authorisation of the
	// same account silently returns none.
	url := account.AuthURLForTest(t, provider.KindGmail, account.OAuthClient{ClientID: "client-id"},
		"http://127.0.0.1:9999/oauth/callback", "state-1")

	for _, want := range []string{"access_type=offline", "prompt=consent", "code_challenge_method=S256", "mail.google.com"} {
		if !strings.Contains(url, want) {
			t.Errorf("the authorisation URL lacks %q:\n%s", want, url)
		}
	}
}

func TestTheMicrosoftFlowAsksForTheOutlookScopesAndNoSecret(t *testing.T) {
	raw := account.AuthURLForTest(t, provider.KindMicrosoft, account.OAuthClient{ClientID: "client-id", Tenant: "common"},
		"http://localhost/oauth/callback", "state-1")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	scope := parsed.Query().Get("scope")
	for _, want := range []string{
		"https://outlook.office.com/IMAP.AccessAsUser.All",
		"https://outlook.office.com/SMTP.Send",
		"offline_access",
	} {
		if !strings.Contains(scope, want) {
			t.Errorf("scope %q lacks %q", scope, want)
		}
	}
	// Mixing Graph scopes in makes the token endpoint issue one for the wrong
	// resource, which then fails at AUTHENTICATE with nothing to explain it.
	if strings.Contains(scope, "graph.microsoft.com") {
		t.Error("Graph scopes must not be mixed with the Outlook resource")
	}
	if !strings.Contains(parsed.Host, "login.microsoftonline.com") {
		t.Errorf("host = %q", parsed.Host)
	}
	if !strings.Contains(parsed.Path, "/common/") {
		t.Errorf("path = %q, want the common tenant so personal accounts work too", parsed.Path)
	}
}

func TestAnAccountWithoutAConfiguredClientSaysWhichVariableIsMissing(t *testing.T) {
	_, err := account.ConfigForTest(provider.KindGmail, account.OAuthClient{}, "http://127.0.0.1/cb")
	if err == nil || !strings.Contains(err.Error(), "MAIL_GOOGLE_CLIENT_ID") {
		t.Fatalf("want the variable named, got %v", err)
	}
}

func TestTheGrantedScopeIsReadTheWayEachProviderWritesIt(t *testing.T) {
	// Google's granular consent lets a person untick mail access, and an
	// Entra admin can consent to less than was asked. Either grant refreshes
	// forever and fails every login, so a response that says it lacks the
	// mailbox is refused. A response that says nothing, or writes the same
	// scope another way, is not.
	const (
		imapScope = "https://outlook.office.com/IMAP.AccessAsUser.All"
		smtpScope = "https://outlook.office.com/SMTP.Send"
	)
	for _, c := range []struct {
		name    string
		kind    provider.Kind
		extra   map[string]any
		missing string // empty when the grant is accepted
	}{
		{"google, as asked", provider.KindGmail, map[string]any{"scope": "https://mail.google.com/"}, ""},
		{"google, without the slash", provider.KindGmail, map[string]any{"scope": "https://mail.google.com"}, ""},
		{"google, with more", provider.KindGmail, map[string]any{"scope": "openid https://mail.google.com/"}, ""},
		{"google, no scope field", provider.KindGmail, map[string]any{}, ""},
		{"google, an empty scope field", provider.KindGmail, map[string]any{"scope": ""}, ""},
		{"google, mail unticked", provider.KindGmail,
			map[string]any{"scope": "openid https://www.googleapis.com/auth/userinfo.email"}, "https://mail.google.com/"},
		// Entra never echoes offline_access; the refresh token is its proof.
		{"microsoft, as Entra writes it", provider.KindMicrosoft,
			map[string]any{"scope": imapScope + " " + smtpScope}, ""},
		{"microsoft, the other outlook host and other case", provider.KindMicrosoft,
			map[string]any{"scope": "https://outlook.office365.com/imap.accessasuser.all https://OUTLOOK.office365.com/SMTP.Send"}, ""},
		{"microsoft, unqualified", provider.KindMicrosoft,
			map[string]any{"scope": "IMAP.AccessAsUser.All SMTP.Send User.Read"}, ""},
		{"microsoft, percent-encoded as its documentation shows", provider.KindMicrosoft,
			map[string]any{"scope": "https%3A%2F%2Foutlook.office.com%2FIMAP.AccessAsUser.All " + smtpScope}, ""},
		{"microsoft, the whole field percent-encoded", provider.KindMicrosoft,
			map[string]any{"scope": "https%3A%2F%2Foutlook.office.com%2FIMAP.AccessAsUser.All%20https%3A%2F%2Foutlook.office.com%2FSMTP.Send"}, ""},
		{"microsoft, without sending", provider.KindMicrosoft,
			map[string]any{"scope": imapScope + " offline_access"}, smtpScope},
		{"microsoft, for another resource", provider.KindMicrosoft,
			map[string]any{"scope": "https://graph.microsoft.com/IMAP.AccessAsUser.All https://graph.microsoft.com/SMTP.Send"},
			imapScope + " " + smtpScope},
	} {
		reason, err := account.ScopeCheckForTest(t, c.kind, c.extra)
		if c.missing == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, account.ErrScopeMissing) {
			t.Errorf("%s: want ErrScopeMissing, got %v", c.name, err)
			continue
		}
		if reason != account.ReasonScopeMissing {
			t.Errorf("%s: state_reason %q, want %q", c.name, reason, account.ReasonScopeMissing)
		}
		if !strings.Contains(err.Error(), "lacks "+c.missing) {
			t.Errorf("%s: the error does not say what is missing: %v", c.name, err)
		}
	}
}
