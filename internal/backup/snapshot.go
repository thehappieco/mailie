package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrDamagedDatabase is a snapshot, or a restored file, that fails
// PRAGMA integrity_check. It is never uploaded, and never put in place.
var ErrDamagedDatabase = errors.New("backup: the database fails its integrity check")

// sqliteURI is a SQLite URI filename for path. The driver passes "file:"
// names to SQLite as URIs, where '?' starts the parameters, '#' ends the name
// and '%' escapes, so those three are escaped in the path.
func sqliteURI(path, params string) string {
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path) + "?" + params
}

// sourceDSN opens the live database for the snapshot.
//
// mode=ro is SQLITE_OPEN_READONLY: the database file is opened O_RDONLY, so
// no statement on this connection can change it, and SQLite refuses every
// write with SQLITE_READONLY. In WAL mode a reader still needs the -wal and
// the -shm the daemon holds open: it reads frames from the first and takes
// its read lock in the second. With write access to the -shm it records a
// read mark there, as any reader does. Without it (the backup unit's
// ReadOnlyPaths), SQLite 3.22 and later still read: they lock a read mark
// already set to the right frame, or copy the WAL index into private memory
// and hold read lock 0, which keeps the daemon's checkpoints from moving
// pages into the database file until the snapshot is done.
//
// No query_only: it also refuses VACUUM INTO, whose only write is to the new
// file. No lock file either. The daemon's lock keeps a second daemon or a
// --bootstrap command off the database; this connection writes nothing, so
// it has nothing to be kept from.
func sourceDSN(path string) string {
	return sqliteURI(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// snapshot copies the live database at live into a new file at out, as it
// was at one instant.
//
// VACUUM INTO runs as one statement in one read transaction, so the copy is
// the database as of the moment that transaction began, whatever the daemon
// commits meanwhile — WAL readers see a fixed snapshot. It writes a fresh,
// compact file in rollback-journal mode: no -wal or -shm to carry along, and
// none of the free pages that held deleted rows.
func snapshot(ctx context.Context, live, out string) error {
	if _, err := os.Stat(live); err != nil {
		return fmt.Errorf("backup: the live database: %w", err)
	}
	db, err := sql.Open("sqlite", sourceDSN(live))
	if err != nil {
		return fmt.Errorf("backup: open the live database: %w", err)
	}
	//nolint:errcheck // read-only connection; the snapshot's own error is the one that matters
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		//nolint:errcheck // a partial copy is about to be removed with its directory
		_ = os.Remove(out)
		if _, werr := os.Stat(live + "-wal"); errors.Is(werr, os.ErrNotExist) {
			// A backup run with the data directory read-only cannot
			// create the -wal and -shm SQLite wants for a database nobody
			// has open. They exist whenever the daemon runs.
			return fmt.Errorf("backup: snapshot (the database has no -wal file: is the daemon running "+
				"against this data directory? the backup reads the database the daemon holds open): %w", err)
		}
		return fmt.Errorf("backup: snapshot: %w", err)
	}
	if err := os.Chmod(out, 0o600); err != nil {
		return fmt.Errorf("backup: snapshot: %w", err)
	}
	return nil
}

// Inspection is what a check of a snapshot or a restored file found.
type Inspection struct {
	// SchemaVersion is PRAGMA user_version: the migration the database is at.
	SchemaVersion int
	// Rows counts a few tables, for the log line. A table this schema does
	// not have yet is left out.
	Rows map[string]int64
}

// integrityCheck returns what PRAGMA integrity_check reports: "ok" alone for
// a sound database.
func integrityCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // read-only; rows.Err reports what matters
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// countedTables are counted for the log: enough to tell a backup of an empty
// database from a real one at a glance, and cheap.
var countedTables = []string{"users", "accounts", "messages"}

// inspect runs PRAGMA integrity_check on the database file at path and reads
// the schema version and a few row counts. A file that fails the check is
// ErrDamagedDatabase.
func inspect(ctx context.Context, path string) (Inspection, error) {
	var in Inspection
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro"))
	if err != nil {
		return in, fmt.Errorf("backup: open %s: %w", path, err)
	}
	//nolint:errcheck // read-only connection
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	problems, err := integrityCheck(ctx, db)
	if err != nil {
		return in, fmt.Errorf("%w: %w", ErrDamagedDatabase, err)
	}
	if len(problems) != 1 || problems[0] != "ok" {
		// The messages name tables, indexes, pages and row ids, not row contents.
		if len(problems) > 3 {
			problems = append(problems[:3], fmt.Sprintf("and %d more", len(problems)-3))
		}
		return in, fmt.Errorf("%w: %s", ErrDamagedDatabase, strings.Join(problems, "; "))
	}

	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&in.SchemaVersion); err != nil {
		return in, fmt.Errorf("backup: read the schema version: %w", err)
	}
	in.Rows = map[string]int64{}
	for _, table := range countedTables {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return in, fmt.Errorf("backup: look for table %s: %w", table, err)
		}
		if n == 0 {
			continue
		}
		var rowsIn int64
		// The name comes from countedTables, never from input.
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&rowsIn); err != nil {
			return in, fmt.Errorf("backup: count %s: %w", table, err)
		}
		in.Rows[table] = rowsIn
	}
	return in, nil
}
