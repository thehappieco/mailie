package account_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// nameOf is a mailbox's name as the database holds it, removed or not.
func nameOf(t *testing.T, db *store.Store, id string) string {
	t.Helper()
	var name string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT display_name FROM accounts WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestARenameWhoseCheckRefusesChangesNothing(t *testing.T) {
	repo, db := newRepo(t)
	a := seed(t, repo, "ana@example.com", provider.KindGmail)
	refused := errors.New("not yours")
	if _, err := repo.Rename(t.Context(), a.ID, "Vendas", func(*sql.Tx) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused rename: %v", err)
	}
	if got := nameOf(t, db, a.ID); got != "" {
		t.Errorf("a refused rename named it %q", got)
	}
	renamed, err := repo.Rename(t.Context(), a.ID, "Vendas", func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if renamed.DisplayName != "Vendas" || renamed.ID != a.ID || renamed.Email != a.Email {
		t.Errorf("the rename answered %+v", renamed)
	}
	if got, err := repo.Get(t.Context(), a.ID); err != nil || got.DisplayName != "Vendas" {
		t.Errorf("read again: %+v, %v", got, err)
	}
}

func TestARemovedMailboxIsNotFoundToARename(t *testing.T) {
	repo, db := newRepo(t)
	a := seed(t, repo, "ana@example.com", provider.KindGmail)
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE accounts SET removed_at = 1, removed_by = 'cli', purge_after = 2592001 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Rename(t.Context(), a.ID, "Vendas", nil); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("renaming a removed mailbox: %v", err)
	}
	if _, err := repo.Rename(t.Context(), "acc_0000000000000000", "Vendas", nil); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("renaming no mailbox: %v", err)
	}
	if got := nameOf(t, db, a.ID); got != "" {
		t.Errorf("the removed mailbox is named %q", got)
	}
}
