package storetest_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// schemaOf is a database's whole schema, as SQLite keeps it, with its
// version and the pragmas a file carries.
func schemaOf(t *testing.T, db *store.Store) string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var kind, name, table, text string
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		b.WriteString(kind + " " + name + " " + table + "\n" + text + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, pragma := range []string{"user_version", "journal_mode", "auto_vacuum", "page_size"} {
		var v string
		if err := db.Reader().QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&v); err != nil {
			t.Fatal(err)
		}
		b.WriteString(pragma + "=" + v + "\n")
	}
	var operator int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM workspaces WHERE id = 'wsp_operator'`).Scan(&operator); err != nil {
		t.Fatal(err)
	}
	b.WriteString("operator workspaces=" + strconv.Itoa(operator) + "\n")
	return b.String()
}

func TestADatabaseFromTheTemplateIsTheOneMigratingMakes(t *testing.T) {
	copied := storetest.New(t)
	fresh, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "mail.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if got, want := schemaOf(t, copied), schemaOf(t, fresh); got != want {
		t.Fatalf("the template's copy:\n%s\nmigrating:\n%s", got, want)
	}
	pending, err := copied.PendingMigrations(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("the copy has migrations pending: %v %v", pending, err)
	}
}

func TestEachDatabaseIsACopyOfItsOwn(t *testing.T) {
	a, b := storetest.New(t), storetest.New(t)
	if err := a.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO meta(key, value) VALUES ('storetest', 'a')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM meta WHERE key = 'storetest'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a row written in one database is in another: %d %v", n, err)
	}
	// And none of it reached the template the next one is copied from.
	c := storetest.New(t)
	if err := c.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM meta WHERE key = 'storetest'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a row written in one database is in a later one: %d %v", n, err)
	}
}

func TestNewAtOpensADatabaseThatIsThereAsItIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mail.db")
	first, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO meta(key, value) VALUES ('storetest', 'kept')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again := storetest.NewAt(t, path, nil)
	var v string
	if err := again.Reader().QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key = 'storetest'`).Scan(&v); err != nil || v != "kept" {
		t.Fatalf("the reopened database lost its row: %q %v", v, err)
	}
}

func TestNewAtMakesTheFileAsTheStoreWould(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	storetest.NewAt(t, filepath.Join(dir, "mail.db"), nil)
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "mail.db"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
}
