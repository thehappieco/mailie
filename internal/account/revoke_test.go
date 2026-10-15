package account_test

import (
	"bytes"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
)

// A Google mailbox deleted for good has its grant revoked at Google, best
// effort, once the deletion commits (revoke.go).

// revocations is a stand-in for Google's revocation endpoint: it keeps every
// token it is sent, and answers status.
type revocations struct {
	mu     sync.Mutex
	tokens []string
	status int
	srv    *httptest.Server
}

func newRevocations(t *testing.T) *revocations {
	t.Helper()
	r := &revocations{status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		if err := req.ParseForm(); err != nil {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.tokens = append(r.tokens, req.PostForm.Get("token"))
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *revocations) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tokens)
}

func (r *revocations) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// lockedLog is a log a registry writes from its own goroutines.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// revokingRegistry is a registry that revokes Google grants at endpoint,
// allowed to reach it on loopback when allowPrivate says so.
func revokingRegistry(t *testing.T, repo *account.Repository, endpoint string, allowPrivate bool) (*account.Registry, *lockedLog) {
	t.Helper()
	log := &lockedLog{}
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		Google:          account.OAuthClient{ClientID: "google-client"},
		Microsoft:       account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
		GoogleRevokeURL: endpoint, AllowPrivate: allowPrivate,
		Log: slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	t.Cleanup(func() { _ = registry.Close() })
	return registry, log
}

// granted seeds a mailbox of kind holding a grant whose refresh token is
// refresh.
func granted(t *testing.T, repo *account.Repository, email string, kind provider.Kind, refresh string) account.Account {
	t.Helper()
	a := seed(t, repo, email, kind)
	if err := repo.SaveGrant(t.Context(), a.ID, &oauth2.Token{AccessToken: "access-" + refresh, RefreshToken: refresh,
		Expiry: time.Now().Add(time.Hour)}, account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAGoogleMailboxDeletedForGoodHasItsGrantRevoked(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	google := newRevocations(t)
	registry, log := revokingRegistry(t, repo, google.srv.URL, true)

	// Its owners, or the operator, deleting a removed mailbox early.
	early := granted(t, repo, "early@gmail.com", provider.KindGmail, "refresh-early")
	removeBySQL(t, db, early.ID)
	if err := registry.Purge(ctx, early.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	if got := google.sent(); !slices.Equal(got, []string{"refresh-early"}) {
		t.Fatalf("revoked %q, want the early deletion's grant", got)
	}

	// The sweep, once its 30 days are over.
	late := granted(t, repo, "late@gmail.com", provider.KindGmail, "refresh-late")
	removeBySQL(t, db, late.ID)
	if purged, err := registry.PurgeExpired(ctx, time.Unix(900+30*24*60*60, 0), nil); err != nil || len(purged) != 1 {
		t.Fatalf("the sweep: %v (%v)", purged, err)
	}
	if got := google.sent(); !slices.Equal(got, []string{"refresh-early", "refresh-late"}) {
		t.Fatalf("revoked %q, want the sweep's grant too", got)
	}

	// The person their mailboxes go with, deleted or forgotten.
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	own, err := repo.Create(ctx, account.Account{
		ID: "acc_anas_own", WorkspaceID: authtest.Personal(t, db, ana.ID), Email: "ana@gmail.com",
		Provider: provider.KindGmail, AuthKind: "oauth2", IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "ana@gmail.com",
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveGrant(ctx, own.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "refresh-ana"},
		account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	if n, err := registry.RemoveOwner(ctx, ana.ID, nil, func(*sql.Tx) error { return nil }); err != nil || n != 1 {
		t.Fatalf("deleting Ana's mailboxes: %d (%v)", n, err)
	}
	if got := google.sent(); !slices.Equal(got, []string{"refresh-early", "refresh-late", "refresh-ana"}) {
		t.Fatalf("revoked %q, want Ana's grant too", got)
	}
	for _, token := range []string{"refresh-early", "refresh-late", "refresh-ana", "access-"} {
		if strings.Contains(log.String(), token) {
			t.Errorf("the log holds %q: %s", token, log.String())
		}
	}
	if n := strings.Count(log.String(), "revoked a deleted mailbox's grant at Google"); n != 3 {
		t.Errorf("%d revocations logged, want 3: %s", n, log.String())
	}
}

func TestOnlyAGoogleGrantNoOtherMailboxHoldsIsRevoked(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	google := newRevocations(t)
	registry, _ := revokingRegistry(t, repo, google.srv.URL, true)

	// The same Google address linked twice, through the same client: one
	// grant at Google, which the second mailbox still holds.
	first := granted(t, repo, "shared@gmail.com", provider.KindGmail, "refresh-one")
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	second, err := repo.Create(ctx, account.Account{
		ID: "acc_shared_again", WorkspaceID: authtest.Personal(t, db, ana.ID), Email: "Shared@Gmail.com",
		Provider: provider.KindGmail, AuthKind: "oauth2", IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "shared@gmail.com",
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveGrant(ctx, second.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "refresh-two"},
		account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	// A Microsoft mailbox and a password one have nothing revoked.
	microsoft := granted(t, repo, "person@outlook.com", provider.KindMicrosoft, "refresh-ms")
	password := seed(t, repo, "person@mail.example.com", provider.KindIMAP)
	exec(t, db, `UPDATE accounts SET auth_kind = 'password' WHERE id = ?`, password.ID)
	if err := repo.SavePassword(ctx, password.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []account.Account{first, microsoft, password} {
		removeBySQL(t, db, a.ID)
		if err := registry.Purge(ctx, a.ID, "cli", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := google.sent(); len(got) != 0 {
		t.Fatalf("revoked %q: a grant another mailbox holds, Microsoft's or a password", got)
	}
	// Once the last mailbox holding it goes, the grant is revoked.
	removeBySQL(t, db, second.ID)
	if err := registry.Purge(ctx, second.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	if got := google.sent(); !slices.Equal(got, []string{"refresh-two"}) {
		t.Errorf("revoked %q, want the last holder's grant", got)
	}

	// A registry with no revocation endpoint revokes nothing.
	quiet, _ := revokingRegistry(t, repo, "", true)
	other := granted(t, repo, "other@gmail.com", provider.KindGmail, "refresh-other")
	removeBySQL(t, db, other.ID)
	if err := quiet.Purge(ctx, other.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	if got := google.sent(); len(got) != 1 {
		t.Errorf("revoked %q without an endpoint", got)
	}
}

func TestAFailedRevocationNeverStopsTheDeletionNorLogsTheToken(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	google := newRevocations(t)
	google.answer(http.StatusInternalServerError)
	registry, log := revokingRegistry(t, repo, google.srv.URL, true)

	refused := granted(t, repo, "refused@gmail.com", provider.KindGmail, "refresh-refused")
	removeBySQL(t, db, refused.ID)
	if err := registry.Purge(ctx, refused.ID, "cli", nil); err != nil {
		t.Fatalf("the deletion, its revocation refused: %v", err)
	}
	// A token that does not open: nothing is sent for it, and the deletion
	// goes on.
	broken := granted(t, repo, "broken@gmail.com", provider.KindGmail, "refresh-broken")
	exec(t, db, `UPDATE credentials SET ciphertext = randomblob(80) WHERE account_id = ?`, broken.ID)
	removeBySQL(t, db, broken.ID)
	if err := registry.Purge(ctx, broken.ID, "cli", nil); err != nil {
		t.Fatalf("the deletion, its token unopened: %v", err)
	}
	// An address the deployment does not allow: nothing is sent.
	guarded, guardedLog := revokingRegistry(t, repo, google.srv.URL, false)
	private := granted(t, repo, "private@gmail.com", provider.KindGmail, "refresh-private")
	removeBySQL(t, db, private.ID)
	if err := guarded.Purge(ctx, private.ID, "cli", nil); err != nil {
		t.Fatalf("the deletion, its revocation refused at dial time: %v", err)
	}

	if got := google.sent(); !slices.Equal(got, []string{"refresh-refused"}) {
		t.Errorf("sent %q, want the refused one alone", got)
	}
	var rows int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM accounts`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d mailboxes left (%v)", rows, err)
	}
	all := log.String() + guardedLog.String()
	for _, want := range []string{"Google refused to revoke", "its token did not open", "revoking a deleted mailbox's grant at Google failed"} {
		if !strings.Contains(all, want) {
			t.Errorf("the log says nothing of %q: %s", want, all)
		}
	}
	for _, token := range []string{"refresh-refused", "refresh-broken", "refresh-private"} {
		if strings.Contains(all, token) {
			t.Errorf("the log holds %q", token)
		}
	}
}
