package account_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
)

// stateEvents reads the account.state rows journaled for an account.
func stateEvents(t *testing.T, repo *account.Repository, db interface{ Reader() *sql.DB }, id string) []account.StateChange {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT payload_json FROM events WHERE type = 'account.state' AND account_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []account.StateChange
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var c account.StateChange
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryStateChangeIsJournaledAsAccountState(t *testing.T) {
	// The console stops polling for an OAuth consent to finish when it can
	// hear the account become active; the sync engine hears it too.
	repo, db := newRepo(t)
	ctx := t.Context()
	a := seed(t, repo, "person@example.com", provider.KindGmail)

	if err := repo.SaveGrant(ctx, a.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}, account.ClientWeb); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetState(ctx, a.ID, account.StateActive, ""); err != nil {
		t.Fatal(err) // no change: nothing journaled
	}
	if moved, err := repo.Transition(ctx, a.ID, []account.State{account.StatePendingAuth}, account.StateError, "x"); err != nil || moved {
		t.Fatalf("a transition from a state the account is not in moved it (%t, %v)", moved, err)
	}
	if err := repo.MarkNeedsReauth(ctx, a.ID, "the refresh token is no longer valid"); err != nil {
		t.Fatal(err)
	}
	if moved, err := repo.Transition(ctx, a.ID, []account.State{account.StateNeedsReauth}, account.StatePendingAuth, ""); err != nil || !moved {
		t.Fatalf("an allowed transition did not move (%t, %v)", moved, err)
	}

	got := stateEvents(t, repo, db, a.ID)
	want := []account.StateChange{
		{AccountID: a.ID, State: "active", Previous: "pending_auth"},
		{AccountID: a.ID, State: "needs_reauth", Previous: "active", Reason: "the refresh token is no longer valid"},
		{AccountID: a.ID, State: "pending_auth", Previous: "needs_reauth"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("journaled %+v\nwant %+v", got, want)
	}
}

func TestAStateChangeIsPublishedOnlyAfterItCommits(t *testing.T) {
	repo, db := newRepo(t)
	bus := events.NewBus(events.NewJournal(db))
	repo.PublishTo(bus)
	a := seed(t, repo, "person@example.com", provider.KindGmail)

	sub, err := bus.Subscribe(t.Context(), 0, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// A change that fails publishes nothing.
	if err := repo.SetState(t.Context(), "acc_missing", account.StateActive, ""); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("SetState on a missing account: %v", err)
	}
	if err := repo.SetState(t.Context(), a.ID, account.StateActive, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != events.TypeAccountState || ev.AccountID != a.ID || ev.Seq == 0 {
			t.Fatalf("published %+v", ev)
		}
		// Anything published is already readable: the journal holds it.
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM events WHERE seq = ?`, ev.Seq).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the published event is not in the journal (%d, %v)", n, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the state change was never published")
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("a second event was published: %+v", ev)
	default:
	}
}

// recorder collects OnChange notifications.
type recorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recorder) note(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.ids
	r.ids = nil
	return out
}

func TestTheSyncEngineHearsAboutEveryAccountLifecycleChange(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	heard := &recorder{}
	registry.OnChange(heard.note)

	a := seed(t, repo, "person@example.com", provider.KindGmail)
	if got := heard.take(); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("creating: heard %v", got)
	}
	if err := repo.SaveGrant(ctx, a.ID, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}, account.ClientWeb); err != nil {
		t.Fatal(err)
	}
	if got := heard.take(); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("a grant: heard %v", got)
	}
	if err := repo.SetState(ctx, a.ID, account.StateActive, ""); err != nil {
		t.Fatal(err)
	}
	if got := heard.take(); len(got) != 0 {
		t.Fatalf("a state that did not change: heard %v", got)
	}
	if err := repo.SetState(ctx, a.ID, account.StateDisabled, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetFolderOverrides(ctx, a.ID, map[string]string{"sent": "Enviadas"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Remove(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := heard.take(); !slices.Equal(got, []string{a.ID, a.ID, a.ID}) {
		t.Fatalf("disable, overrides, remove: heard %v", got)
	}

	// A person's mailboxes, deleted with them.
	if _, err := db.Writer().ExecContext(ctx, `INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		VALUES ('usr_1', 'owner@example.com', 'x', 'owner', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	b, err := repo.Create(ctx, account.Account{
		ID: "acc_b", Email: "b@example.com", Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.example.com", IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 587,
		SMTPTLS: "starttls", LoginUser: "b@example.com", OwnerUserID: "usr_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	heard.take()
	n, err := registry.RemoveOwner(ctx, "usr_1", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `DELETE FROM users WHERE id = 'usr_1'`)
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("RemoveOwner: %d, %v", n, err)
	}
	if got := heard.take(); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("deleting the owner: heard %v", got)
	}
	// Removal journals nothing, and what was journaled for the account goes
	// with it.
	var left int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE account_id IN (?, ?)`, a.ID, b.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d events of removed accounts are left", left)
	}
}
