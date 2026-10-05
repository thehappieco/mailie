package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
	// rebuild is a migration that recreates a table other tables refer to:
	// its first line is rebuildMarker, and it runs through applyRebuild.
	rebuild bool
}

// rebuildMarker, as the first line of a migration, says that it rebuilds a
// table: creates a new one, copies every row, drops the old one and renames
// the new one into its place. SQLite removes a constraint, or changes a
// column's, only that way.
//
// Such a migration cannot run like the others. They run in a transaction on a
// connection with foreign keys on, where DROP TABLE is an implicit DELETE that
// cascades into every child: dropping accounts there would delete every
// credential, folder and message. PRAGMA foreign_keys cannot change inside a
// transaction, so applyRebuild turns it off before it begins one.
const rebuildMarker = "-- migration: rebuild"

// ErrRebuildRefused is a rebuild the runner will not apply, or one that would
// have broken the database and was rolled back.
var ErrRebuildRefused = errors.New("store: the rebuild was refused")

// ErrSchemaTooNew is a database a newer binary migrated past the last
// migration this one embeds. This binary does not know that schema: it would
// read and write it by rules that no longer hold (a binary from before 0008,
// say, creates people with no personal workspace), so it refuses to open it.
// A binary is never rolled back alone: going back is the backup taken before
// the upgrade, restored with the binary of its time.
var ErrSchemaTooNew = errors.New("store: the database was migrated by a newer binary")

// Migrate applies every migration newer than PRAGMA user_version.
//
// Each migration runs inside the transaction that also bumps user_version, so a
// crash halfway leaves the database at the previous version rather than in a
// shape no code knows how to read. There is no migrations table: user_version
// is a single integer SQLite maintains for exactly this, and a table would be
// one more thing to keep consistent with it.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	current, err := s.userVersion(ctx)
	if err != nil {
		return err
	}
	if err := knows(migrations, current); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// PendingMigration is a migration Migrate would apply.
type PendingMigration struct {
	Name string
	// Rebuild is a migration that rebuilds a table (see rebuildMarker): an
	// upgrade an operator backs up before, as before any other.
	Rebuild bool
}

// PendingMigrations reports the migrations that Migrate would apply.
func (s *Store) PendingMigrations(ctx context.Context) ([]PendingMigration, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	current, err := s.userVersion(ctx)
	if err != nil {
		return nil, err
	}
	if err := knows(migrations, current); err != nil {
		return nil, err
	}
	var out []PendingMigration
	for _, m := range migrations {
		if m.version > current {
			out = append(out, PendingMigration{Name: m.name, Rebuild: m.rebuild})
		}
	}
	return out, nil
}

// knows refuses a schema version past the last migration embedded here
// (ErrSchemaTooNew).
func knows(migrations []migration, current int) error {
	latest := 0
	if len(migrations) > 0 {
		latest = migrations[len(migrations)-1].version
	}
	if current > latest {
		return fmt.Errorf("%w: it is at schema %d, and this binary knows up to %d; "+
			"run the newer binary, or restore the backup taken before the upgrade with the binary of its time",
			ErrSchemaTooNew, current, latest)
	}
	return nil
}

// SchemaVersion is the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) { return s.userVersion(ctx) }

func (s *Store) userVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	if m.rebuild {
		return s.applyRebuild(ctx, m)
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
		// PRAGMA does not accept a bound parameter, and the value is an int
		// parsed from a file name that ships inside the binary.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("store: migration %s: set user_version: %w", m.name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(numStr)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("store: migration %q: %q is not a version number", e.Name(), numStr)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		first, _, _ := strings.Cut(string(body), "\n")
		out = append(out, migration{
			version: version, name: e.Name(), sql: string(body),
			rebuild: strings.TrimSpace(first) == rebuildMarker,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// applyRebuild applies a migration that rebuilds a table, through SQLite's
// own procedure for "other kinds of table schema changes", and leaves the
// database exactly as it was when anything goes wrong.
//
//  1. One connection for the whole procedure, so the pragmas and the
//     transaction run on the same one. The writer pool has one connection,
//     and at Open and in `mailserver migrate` nothing else is connected: the
//     reader pool opens after migrating, and the lock keeps a daemon away.
//  2. Outside any transaction, foreign keys off, read back; legacy_alter_table
//     read back as off, without which a rename would not repoint the
//     references of triggers and views; and no view in the schema, since the
//     runner cannot tell which tables a view names.
//  3. BEGIN IMMEDIATE (the writer's _txlock), and the rows of every table
//     counted, the full-text index's own tables included.
//  4. The migration: create the new table, copy every row with every column
//     named, drop the old one, rename the new one into its place, recreate
//     indexes and triggers. Never the other order, renaming the old table
//     aside first: with modern rename rules that repoints every child's
//     foreign key at the renamed table, and dropping it orphans them all.
//  5. PRAGMA foreign_key_check: any row fails the migration.
//  6. The rows counted again: every table that was there holds exactly as
//     many. A rebuild adds tables, and rows to them; it never loses a row, nor
//     adds one to a table that existed.
//  7. user_version, in the same transaction, and COMMIT; any failure before
//     it is a ROLLBACK, and the database is as it was.
//  8. Whatever happened, foreign keys back on, read back, before the
//     connection goes back to the pool; a connection where that fails is
//     discarded, and a new one gets foreign_keys(1) from the DSN. A process
//     that dies midway loses nothing: the transaction never committed.
func (s *Store) applyRebuild(ctx context.Context, m migration) (err error) {
	conn, err := s.w.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migration %s: take the writer: %w", m.name, err)
	}
	defer func() {
		if fkErr := setForeignKeys(context.WithoutCancel(ctx), conn, true); fkErr != nil {
			err = errors.Join(err, fmt.Errorf("store: migration %s: %w", m.name, fkErr))
			// Never back into the pool with foreign keys off.
			//nolint:errcheck // the error returned is the one that discards the connection
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, fmt.Errorf("store: migration %s: release the writer: %w", m.name, closeErr))
		}
	}()

	if err := setForeignKeys(ctx, conn, false); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	var legacy, views int
	if err := conn.QueryRowContext(ctx, `PRAGMA legacy_alter_table`).Scan(&legacy); err != nil {
		return fmt.Errorf("store: migration %s: read legacy_alter_table: %w", m.name, err)
	}
	if legacy != 0 {
		return fmt.Errorf("%w: %s: legacy_alter_table is on", ErrRebuildRefused, m.name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'view'`).Scan(&views); err != nil {
		return fmt.Errorf("store: migration %s: look for views: %w", m.name, err)
	}
	if views != 0 {
		return fmt.Errorf("%w: %s: the schema has views, which a rebuild does not carry over", ErrRebuildRefused, m.name)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %s: begin: %w", m.name, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("store: migration %s: rollback: %w", m.name, rbErr))
		}
	}()

	before, err := rowCounts(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRebuildRefused, m.name, err)
	}
	after, err := rowCounts(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	if err := sameRows(before, after); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRebuildRefused, m.name, err)
	}
	// As in applyMigration: an int parsed from a file name in the binary.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("store: migration %s: set user_version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %s: commit: %w", m.name, err)
	}
	committed = true
	return nil
}

// setForeignKeys switches foreign keys on one connection, outside any
// transaction (inside one SQLite ignores it), and reads the setting back.
func setForeignKeys(ctx context.Context, conn *sql.Conn, on bool) error {
	want := 0
	if on {
		want = 1
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA foreign_keys = %d", want)); err != nil {
		return fmt.Errorf("set foreign_keys to %d: %w", want, err)
	}
	var got int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&got); err != nil {
		return fmt.Errorf("read foreign_keys back: %w", err)
	}
	if got != want {
		return fmt.Errorf("%w: foreign_keys is %d, want %d", ErrRebuildRefused, got, want)
	}
	return nil
}

// rowCounts counts the rows of every table in the schema. A virtual table is
// left out: its rows are another table's (the full-text index's are
// messages'), and its own storage is in shadow tables, which are counted. So
// are SQLite's statistics (sqlite_stat*, if anyone ever ran ANALYZE): they
// describe indexes, and dropping a table drops its indexes' lines.
// sqlite_sequence is counted: it is what keeps an AUTOINCREMENT id from ever
// being handed out twice.
func rowCounts(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	names, err := countedTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(names))
	for _, name := range names {
		var n int64
		// The name comes from sqlite_schema, quoted as an identifier.
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}

func countedTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema
		WHERE type = 'table' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' AND name NOT LIKE 'sqlite\_stat%' ESCAPE '\'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("list tables: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return names, nil
}

// sameRows refuses a table that went missing or whose row count changed.
func sameRows(before, after map[string]int64) error {
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		n, ok := after[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s is gone", name))
		case n != before[name]:
			problems = append(problems, fmt.Sprintf("%s held %d rows and holds %d", name, before[name], n))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// foreignKeyCheck fails on any row whose foreign key names nothing.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var problems []string
	for rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			fk            int64
		)
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			return fmt.Errorf("foreign_key_check: %w", err)
		}
		if len(problems) < 5 {
			problems = append(problems, fmt.Sprintf("%s row %d names a missing %s", table, rowid.Int64, parent))
		} else if len(problems) == 5 {
			problems = append(problems, "and more")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("foreign keys broken: %s", strings.Join(problems, "; "))
	}
	return nil
}
