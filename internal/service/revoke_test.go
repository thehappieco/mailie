package service_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestForgettingAPersonRevokesTheGoogleGrantsOfTheMailboxesThatGoWithThem(t *testing.T) {
	var (
		mu      sync.Mutex
		revoked []string
	)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			mu.Lock()
			revoked = append(revoked, r.PostForm.Get("token"))
			mu.Unlock()
		}
	}))
	t.Cleanup(google.Close)
	f := newFixtureWith(t, fixtureOptions{source: workspace.Platform(), registry: account.RegistryOptions{
		Google: account.OAuthClient{ClientID: "google-client"}, GoogleRevokeURL: google.URL,
	}})
	ctx := t.Context()
	f.followed(t, "ana@example.com")
	ana := f.apply(t, document("ana@example.com", 1)).UserID
	own, err := f.repo.Create(ctx, account.Account{
		ID: "acc_anas_gmail00001", WorkspaceID: personalOf("ana@example.com"), Email: "ana@gmail.com",
		Provider: provider.KindGmail, AuthKind: "oauth2", IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "ana@gmail.com",
	}, ana)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SaveGrant(ctx, own.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "refresh-ana",
		Expiry: time.Now().Add(time.Hour)}, account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	f.apply(t, closing("ana@example.com", 2))
	if got := f.forget(t, "ana@example.com", 2); got.AccountsRemoved != 1 {
		t.Fatalf("forgetting Ana removed %d mailboxes", got.AccountsRemoved)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(revoked, []string{"refresh-ana"}) {
		t.Errorf("revoked %q, want her mailbox's grant", revoked)
	}
}
