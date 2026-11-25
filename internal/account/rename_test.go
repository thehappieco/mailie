package account_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
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

func TestARenameARequestDecidesIsRecordedInItsWriteAndANameHeldAlreadyAsChangingNothing(t *testing.T) {
	repo, db := newRepo(t)
	ctx := t.Context()
	a := seed(t, repo, "ana@example.com", provider.KindGmail)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	request := func(id string) *store.Request {
		return &store.Request{Extension: "ext", ID: id, UserID: ana.ID, Act: "rename", FirstIssued: time.Now(),
			KeepUntil: time.Now().Add(time.Hour)}
	}
	renamed, changed, err := repo.RenameFor(ctx, request("r1"), a.ID, "Vendas", nil)
	if err != nil || !changed || renamed.DisplayName != "Vendas" {
		t.Fatalf("a rename a request decides: %+v, changed %v, %v", renamed, changed, err)
	}
	if out, ran, err := db.RequestOutcomeOf(ctx, "ext", "r1"); err != nil || !ran || !out.Changed {
		t.Errorf("its record: %+v, ran %v, %v", out, ran, err)
	}
	// Run again, the request does nothing but answer what it did.
	if _, err := repo.Rename(ctx, a.ID, "Compras", nil); err != nil {
		t.Fatal(err)
	}
	var done *store.RequestDone
	if _, _, err := repo.RenameFor(ctx, request("r1"), a.ID, "Vendas", nil); !errors.As(err, &done) || !done.Outcome.Changed {
		t.Errorf("the request again: %v, want its first outcome", err)
	}
	if got := nameOf(t, db, a.ID); got != "Compras" {
		t.Errorf("the request again named it %q", got)
	}
	// The name it holds already: recorded, as having changed nothing.
	if _, changed, err := repo.RenameFor(ctx, request("r2"), a.ID, "Compras", nil); err != nil || changed {
		t.Fatalf("a rename to the name it holds: changed %v, %v", changed, err)
	}
	if out, ran, err := db.RequestOutcomeOf(ctx, "ext", "r2"); err != nil || !ran || out.Changed {
		t.Errorf("the no-op's record: %+v, ran %v, %v", out, ran, err)
	}
	// A refused rename records nothing.
	refused := errors.New("not yours")
	if _, _, err := repo.RenameFor(ctx, request("r3"), a.ID, "Mine", func(*sql.Tx) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused rename: %v", err)
	}
	if _, ran, err := db.RequestOutcomeOf(ctx, "ext", "r3"); err != nil || ran {
		t.Errorf("the refused rename was recorded: %v, %v", ran, err)
	}
}
