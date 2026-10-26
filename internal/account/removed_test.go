package account_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A removed mailbox (migration 0020) is kept for a recovery and used by
// nothing meanwhile.

// removeBySQL removes a mailbox as the repository will, for 30 days.
func removeBySQL(t *testing.T, db *store.Store, accountID string) {
	t.Helper()
	exec(t, db, `UPDATE accounts SET removed_at = 900, removed_by = 'cli', purge_after = 900 + 2592000 WHERE id = ?`, accountID)
}

// recoverBySQL recovers a removed mailbox as the repository will, its sync
// paused where its consent stands.
func recoverBySQL(t *testing.T, db *store.Store, accountID string) {
	t.Helper()
	exec(t, db, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
		sync_paused_at = CASE WHEN `+store.ConsentStandsSQL("accounts")+` THEN 950 ELSE 0 END WHERE id = ?`, accountID)
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

// gatedSealer is a keyring whose next Open, once armed, waits to be let go:
// a mailbox's build held between reading the account and opening its
// credentials.
type gatedSealer struct {
	secrets.Sealer
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedSealer) Open(ctx context.Context, b secrets.Binding, envelope []byte) ([]byte, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.Sealer.Open(ctx, b, envelope)
}

func TestAMailboxBeingBuiltWhenItsAccountIsRemovedIsNeverKept(t *testing.T) {
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedSealer{Sealer: keyring, entered: make(chan struct{}), release: make(chan struct{})}
	db := storetest.New(t)
	repo := account.NewRepository(db, gate)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	a, err := repo.Create(ctx, account.Account{
		ID: "acc_00000000000000a2", Email: "ops@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "ops@mail.example",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePassword(ctx, a.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}

	// A use of the mailbox reads the account, live, and is held opening its
	// password while the mailbox is removed.
	gate.armed.Store(true)
	built := make(chan error, 1)
	go func() {
		_, err := registry.Mailbox(ctx, a.ID)
		built <- err
	}()
	<-gate.entered
	if _, err := registry.Remove(ctx, a.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-built; !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the use under way when it was removed: %v, want ErrNotFound", err)
	}
	// Nothing was kept for it: every later use finds nothing either.
	if _, err := registry.Mailbox(ctx, a.ID); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("a use after the removal: %v, want ErrNotFound", err)
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

// connected seeds a mailbox that connected once: it holds a grant.
func connected(t *testing.T, repo *account.Repository, email string) account.Account {
	t.Helper()
	a := seed(t, repo, email, provider.KindGmail)
	if err := repo.SaveGrant(t.Context(), a.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "r"},
		account.ClientInstalled); err != nil {
		t.Fatal(err)
	}
	return a
}

// restrictionOf lists the mailboxes an instance key is restricted to.
func restrictionOf(t *testing.T, db *store.Store, prefix string) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT account_id FROM api_key_accounts WHERE key_prefix = ? ORDER BY account_id`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func countOf(t *testing.T, db *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestRemovingStopsAConsentAttemptAndItsRedirectFindsNothing(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{Google: account.OAuthClient{ClientID: "google-client"}})
	t.Cleanup(func() { _ = registry.Close() })
	// It connected once and its grant died: it is being authorized again.
	a := connected(t, repo, "person@example.com")
	if err := repo.MarkNeedsReauth(ctx, a.ID, "the refresh token is no longer valid"); err != nil {
		t.Fatal(err)
	}
	flow, err := registry.StartAuth(ctx, a.ID, account.FlowLoopback, "")
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(flow.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", redirect.Host); err != nil {
		t.Fatalf("the listener was not up to begin with: %v", err)
	} else {
		_ = conn.Close()
	}

	removal, err := registry.Remove(ctx, a.ID, "cli", nil)
	if err != nil || removal.Purged {
		t.Fatalf("removing it: %+v, %v, want it kept", removal, err)
	}
	if conn, err := net.Dial("tcp", redirect.Host); err == nil {
		_ = conn.Close()
		t.Error("the attempt's listener still answers after its mailbox was removed")
	}
	if n := countOf(t, db, `SELECT count(*) FROM oauth_pending WHERE account_id = ?`, a.ID); n != 0 {
		t.Errorf("%d consent attempts on the removed mailbox are still pending", n)
	}
	// The redirect, coming back later, finds nothing to finish.
	_, err = registry.CompleteFlow(ctx, account.Redirect{State: flow.State, Code: "the-code"}, "")
	if !errors.Is(err, account.ErrNotFound) {
		t.Errorf("the redirect after the removal: %v, want ErrNotFound", err)
	}
}

func TestRemovingDeletesWhatKeysHeldAndNeverWidensAnInstanceKey(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	keys := auth.NewKeys(db)
	issue := func(req auth.NewKeyRequest) string {
		t.Helper()
		req.Name = "removal test"
		_, key, err := keys.Issue(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return key.Prefix
	}
	// Two operator mailboxes: a key restricted to the first alone, and one
	// restricted to both.
	first := connected(t, repo, "first@example.com")
	second := connected(t, repo, "second@example.com")
	onlyFirst := issue(auth.NewKeyRequest{Scope: auth.ScopeRead, AccountIDs: []string{first.ID}})
	both := issue(auth.NewKeyRequest{Scope: auth.ScopeRead, AccountIDs: []string{first.ID, second.ID}})
	// A person's mailbox, and a key of their workspace holding it.
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	personal := authtest.Personal(t, db, ana.ID)
	own, err := repo.Create(ctx, account.Account{
		ID: "acc_00000000000000a1", Email: "ana@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "ana@mail.example",
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePassword(ctx, own.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}
	held := issue(auth.NewKeyRequest{Scope: auth.ScopeRead, WorkspaceID: personal, CreatedBy: ana.ID,
		TermsVersion: "terms", Mailboxes: []workspace.KeyGrant{{AccountID: own.ID, Flags: workspace.Flags{Read: true}}}})
	stopped := func(prefix string) (bool, string) {
		t.Helper()
		var revoked int64
		var reason string
		if err := db.Reader().QueryRowContext(ctx, `SELECT revoked_at, stop_reason FROM api_keys WHERE prefix = ?`,
			prefix).Scan(&revoked, &reason); err != nil {
			t.Fatal(err)
		}
		return revoked != 0, reason
	}

	for _, id := range []string{first.ID, own.ID} {
		if _, err := registry.Remove(ctx, id, "cli", nil); err != nil {
			t.Fatal(err)
		}
	}
	// The key restricted to the first alone stops, saying its last mailbox
	// went: restricted to nothing, it would reach every operator mailbox.
	if ok, reason := stopped(onlyFirst); !ok || reason != string(workspace.KeyStopRestrictionEmptied) {
		t.Errorf("the key restricted to the removed mailbox alone: stopped %v (%q)", ok, reason)
	}
	if ok, _ := stopped(both); ok {
		t.Error("the key restricted to both stopped")
	}
	if n := countOf(t, db, `SELECT count(*) FROM api_key_accounts WHERE account_id = ?`, first.ID) +
		countOf(t, db, `SELECT count(*) FROM key_access WHERE account_id = ?`, own.ID); n != 0 {
		t.Errorf("%d rows still give keys the removed mailboxes", n)
	}
	// A workspace key that held it holds nothing, and works on.
	if ok, _ := stopped(held); ok {
		t.Error("the workspace key that held the removed mailbox stopped")
	}

	// Recovered, nothing a key held comes back.
	for _, id := range []string{first.ID, own.ID} {
		if _, err := registry.Recover(ctx, id, "cli", nil); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := stopped(onlyFirst); !ok {
		t.Error("the stopped key works again after the recovery")
	}
	if restricted := restrictionOf(t, db, both); !slices.Equal(restricted, []string{second.ID}) {
		t.Errorf("the key restricted to both is restricted to %v after the recovery, want only %s", restricted, second.ID)
	}
	if n := countOf(t, db, `SELECT count(*) FROM key_access WHERE account_id = ?`, own.ID); n != 0 {
		t.Errorf("the workspace key holds the recovered mailbox again (%d rows)", n)
	}
}

func TestAMailboxThatNeverConnectedIsDeletedAtOnce(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	abandoned := seed(t, repo, "abandoned@example.com", provider.KindGmail)
	kept := connected(t, repo, "kept@example.com")
	var told []bool
	check := func(_ *sql.Tx, neverConnected bool) error {
		told = append(told, neverConnected)
		return nil
	}

	removal, err := registry.Remove(ctx, abandoned.ID, "cli", check)
	if err != nil || !removal.Purged || !removal.PurgeAfter.IsZero() {
		t.Fatalf("abandoning a link that never connected: %+v, %v, want it deleted at once", removal, err)
	}
	removal, err = registry.Remove(ctx, kept.ID, "cli", check)
	if err != nil || removal.Purged || removal.PurgeAfter.Sub(removal.RemovedAt) != account.RemovedKeptFor {
		t.Fatalf("removing one that connected: %+v, %v, want it kept 30 days", removal, err)
	}
	if !slices.Equal(told, []bool{true, false}) {
		t.Errorf("the check was told %v, want whether each never connected", told)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts WHERE id = ?`, abandoned.ID); n != 0 {
		t.Error("the link that never connected is still there")
	}
	// Nothing records it either: it goes as if it had never been linked.
	if n := countOf(t, db, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, abandoned.ID); n != 0 {
		t.Errorf("the journal keeps %d entries of the abandoned link", n)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, kept.ID); n != 1 {
		t.Error("the mailbox that connected is not kept removed")
	}
	// A refusal leaves either as it was.
	again := seed(t, repo, "again@example.com", provider.KindGmail)
	refused := errors.New("not yours")
	if _, err := registry.Remove(ctx, again.ID, "cli", func(*sql.Tx, bool) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused removal: %v", err)
	}
	if _, err := repo.Get(ctx, again.ID); err != nil {
		t.Errorf("a refused removal took the mailbox: %v", err)
	}
}

func TestTheSweepPurgesOnlyPastThirtyDays(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	var mu sync.Mutex
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	at := func(when time.Time) { mu.Lock(); now = when; mu.Unlock() }
	repo.WithClock(clock)
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	removedAt := now
	expiring := connected(t, repo, "expiring@example.com")
	recovered := connected(t, repo, "recovered@example.com")
	// A team's mailbox, removed before its team closed: its 30 days run all
	// the same.
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	team, err := ws.CreateTeam(ctx, "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := repo.Create(ctx, account.Account{
		ID: "acc_00000000000000c1", WorkspaceID: team.ID, Email: "support@mail.example", Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "support@mail.example",
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePassword(ctx, closed.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{expiring.ID, recovered.ID, closed.ID} {
		if _, err := registry.Remove(ctx, id, ana.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ws.SetClosed(ctx, team.ID, true); err != nil {
		t.Fatal(err)
	}
	at(removedAt.Add(24 * time.Hour))
	if _, err := registry.Recover(ctx, recovered.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}

	// A second early, nothing goes.
	at(removedAt.Add(account.RemovedKeptFor - time.Second))
	purged, err := registry.PurgeExpired(ctx, clock(), nil)
	if err != nil || len(purged) != 0 {
		t.Fatalf("a second before their 30 days were over the sweep deleted %v (%v)", purged, err)
	}
	// Once they are over, the removed ones go, the closed team's included;
	// the recovered one stays.
	at(removedAt.Add(account.RemovedKeptFor))
	var heard []string
	purged, err = registry.PurgeExpired(ctx, clock(), func(id string) { heard = append(heard, id) })
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(purged)
	want := []string{closed.ID, expiring.ID}
	slices.Sort(want)
	if !slices.Equal(purged, want) || !slices.Equal(heard, purged) {
		t.Errorf("the sweep deleted %v (told %v), want %v", purged, heard, want)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts WHERE id IN (?, ?)`, expiring.ID, closed.ID); n != 0 {
		t.Errorf("%d of the expired mailboxes are still there", n)
	}
	if _, err := repo.Get(ctx, recovered.ID); err != nil {
		t.Errorf("the sweep took the recovered mailbox: %v", err)
	}
	// Journaled as the daemon's, with the names forgotten.
	if n := countOf(t, db, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'purged'
		AND actor = ? AND label = '' AND hint = ''`, expiring.ID, store.ActorSweep); n != 1 {
		t.Error("the deletion for good is not journaled as the sweep's")
	}
	if n := countOf(t, db, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND (label <> '' OR hint <> '')`,
		expiring.ID); n != 0 {
		t.Error("the journal still names the mailbox deleted for good")
	}
	// Another sweep finds nothing more.
	if purged, err := registry.PurgeExpired(ctx, clock().Add(time.Hour), nil); err != nil || len(purged) != 0 {
		t.Errorf("a second sweep deleted %v (%v)", purged, err)
	}
}

func TestRelinkingARemovedAddressIsAConflict(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{Google: account.OAuthClient{ClientID: "google-client"}})
	t.Cleanup(func() { _ = registry.Close() })
	a := connected(t, repo, "person@gmail.com")
	removal, err := registry.Remove(ctx, a.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = registry.Add(ctx, account.AddRequest{Email: "PERSON@gmail.com", Provider: provider.KindGmail,
		Flow: account.FlowPasted})
	var dup *account.RemovedDuplicateError
	if !errors.As(err, &dup) || !errors.Is(err, account.ErrRemovedDuplicate) || dup.AccountID != a.ID ||
		!dup.PurgeAfter.Equal(removal.PurgeAfter) {
		t.Fatalf("linking the removed address again: %v, want it kept until %v", err, removal.PurgeAfter)
	}
	// The same answer where the insert is what finds it.
	_, err = repo.Create(ctx, account.Account{ID: "acc_again", Email: "person@gmail.com", Provider: provider.KindGmail,
		AuthKind: "oauth2", IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 587,
		SMTPTLS: "starttls", LoginUser: "person@gmail.com"}, "")
	if !errors.As(err, &dup) || dup.AccountID != a.ID {
		t.Fatalf("inserting the removed address again: %v", err)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts WHERE email = 'person@gmail.com' COLLATE NOCASE`); n != 1 {
		t.Errorf("%d mailboxes have the address", n)
	}
	// Deleted for good, it links again.
	if err := registry.Purge(ctx, a.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Add(ctx, account.AddRequest{Email: "person@gmail.com", Provider: provider.KindGmail,
		Flow: account.FlowPasted}); err != nil {
		t.Errorf("linking the address once the removed mailbox was deleted for good: %v", err)
	}
}

func TestANeverConnectedLinkRemovedInAFollowedWorkspaceIsJournaledPurged(t *testing.T) {
	// A team from another source, whose journal an extension reports
	// elsewhere: a link there that never connected is deleted at once all
	// the same, but its deletion is journaled, and its earlier entries are
	// kept with its names forgotten, as a removed mailbox's are.
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	const team = "0b4a3c5e-0000-4000-8000-000000000003"
	exec(t, db, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES (?, 'team', 'platform', 'Elsewhere', 100, 100)`, team)
	exec(t, db, `INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		VALUES (?, ?, 'owner', 'active', 100, 100)`, team, ana.ID)
	link := func(id, email string) {
		t.Helper()
		if _, err := repo.Create(ctx, account.Account{ID: id, WorkspaceID: team, Email: email, Provider: provider.KindGmail,
			AuthKind: "oauth2", IMAPHost: "imap.example.com", IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 587,
			SMTPTLS: "starttls", LoginUser: email, DisplayName: "Sales desk"}, ana.ID); err != nil {
			t.Fatal(err)
		}
	}
	const abandoned, deleted = "acc_00000000000000b1", "acc_00000000000000b2"
	link(abandoned, "orders@example.org")
	link(deleted, "billing@example.org")
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{Kind: store.MailboxSyncPaused, AccountID: abandoned,
			Actor: ana.ID, At: 500}, nil, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	removal, err := registry.Remove(ctx, abandoned, ana.ID, nil)
	if err != nil || !removal.Purged {
		t.Fatalf("abandoning a link that never connected: %+v, %v, want it deleted at once", removal, err)
	}
	if err := repo.Delete(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts WHERE id IN (?, ?)`, abandoned, deleted); n != 0 {
		t.Errorf("%d of the links are still there", n)
	}
	rows, err := db.Reader().QueryContext(ctx, `SELECT account_id || '|' || kind || '|' || actor || '|' || label || '|'
		|| hint || '|' || workspace_id FROM mailbox_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{
		abandoned + "|sync_paused|" + ana.ID + "|||" + team,
		abandoned + "|purged|" + ana.ID + "|||" + team,
		deleted + "|purged||||" + team,
	}; !slices.Equal(got, want) {
		t.Errorf("the journal holds\n%v\nwant\n%v", got, want)
	}
}
