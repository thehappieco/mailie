package account_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// A removed mailbox (migration 0020) is kept for a recovery and used by
// nothing meanwhile.

// removeBySQL removes a mailbox as the repository will, for 30 days.
func removeBySQL(t *testing.T, db *store.Store, accountID string) {
	t.Helper()
	exec(t, db, `UPDATE accounts SET removed_at = 900, removed_by = 'cli', purge_after = 900 + 2592000 WHERE id = ?`, accountID)
}

// recoverBySQL recovers a removed mailbox as the repository will, its sync
// held.
func recoverBySQL(t *testing.T, db *store.Store, accountID string) {
	t.Helper()
	exec(t, db, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0, sync_held_at = 950 WHERE id = ?`,
		accountID)
}

func exec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), query, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestARemovedMailboxIsNeverDialledNorRefreshed(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	var refreshes atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokens.Close)
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{
		Google: account.OAuthClient{ClientID: "google-client",
			Endpoint: oauth2.Endpoint{AuthURL: tokens.URL + "/auth", TokenURL: tokens.URL + "/token"}},
	})
	t.Cleanup(func() { _ = registry.Close() })
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	// A grant whose access token has expired: any use would refresh it first.
	if err := repo.SaveGrant(ctx, a.ID, &oauth2.Token{AccessToken: "old", RefreshToken: "first",
		Expiry: time.Now().Add(-time.Hour)}, account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	removeBySQL(t, db, a.ID)

	if _, err := registry.Mailbox(ctx, a.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the removed mailbox's provider mailbox: %v, want ErrNotFound", err)
	}
	if _, err := registry.StartAuth(ctx, a.ID, account.FlowLoopback, ""); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("consent started on the removed mailbox: %v, want ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, a.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("reading the removed mailbox: %v, want ErrNotFound", err)
	}
	if _, err := repo.GetVisible(ctx, a.ID, account.Visibility{All: true}); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the daemon's own read of the removed mailbox: %v, want ErrNotFound", err)
	}
	if all, err := repo.List(ctx); err != nil || len(all) != 0 {
		t.Errorf("the daemon lists %+v (%v), want no mailbox", all, err)
	}
	if n := refreshes.Load(); n != 0 {
		t.Errorf("the removed mailbox's grant was refreshed %d times", n)
	}
	// Its address stays taken while it is kept.
	if got, err := repo.GetByEmail(ctx, a.WorkspaceID, a.Email); err != nil || got.ID != a.ID {
		t.Errorf("its address: %+v, %v, want the removed mailbox", got, err)
	}
	// A refresh already under way when it was removed still records the
	// token the provider rotated, which a recovery needs.
	if err := repo.SaveToken(ctx, a.ID, &oauth2.Token{AccessToken: "late", RefreshToken: "rotated-late"}); err != nil {
		t.Fatalf("a rotated token recorded on the removed mailbox: %v", err)
	}
	if token, err := repo.Token(ctx, a.ID); err != nil || token.RefreshToken != "rotated-late" {
		t.Errorf("the removed mailbox keeps %+v (%v), want the rotated token", token, err)
	}

	// Recovered, it is built again.
	recoverBySQL(t, db, a.ID)
	if _, err := registry.Mailbox(ctx, a.ID); err != nil {
		t.Errorf("the recovered mailbox: %v", err)
	}
}

func TestAGrantFinishingAfterRemovalStoresNothing(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	if err := repo.SaveGrant(ctx, a.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "before"}, account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkNeedsReauth(ctx, a.ID, "the refresh token is no longer valid"); err != nil {
		t.Fatal(err)
	}
	before := stateEvents(t, repo, db, a.ID)
	removeBySQL(t, db, a.ID)

	err := repo.SaveGrant(ctx, a.ID, &oauth2.Token{AccessToken: "b", RefreshToken: "after"}, account.ClientWeb)
	if !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("a grant stored on the removed mailbox: %v, want ErrNotFound", err)
	}
	if token, err := repo.Token(ctx, a.ID); err != nil || token.RefreshToken != "before" {
		t.Errorf("the removed mailbox holds %+v (%v), want the grant it had", token, err)
	}
	var state, client string
	if err := db.Reader().QueryRowContext(context.Background(),
		`SELECT state, oauth_client FROM accounts WHERE id = ?`, a.ID).Scan(&state, &client); err != nil {
		t.Fatal(err)
	}
	if state != string(account.StateNeedsReauth) || client != account.ClientInstalled {
		t.Errorf("the removed mailbox is %s with the %s client, want it as it was", state, client)
	}
	if got := stateEvents(t, repo, db, a.ID); !slices.Equal(got, before) {
		t.Errorf("journaled %+v after the removal", got[len(before):])
	}
}
