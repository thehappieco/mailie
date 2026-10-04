// Package store is the SQLite persistence layer.
//
// Two connection pools over one file, and the split is not a tuning knob:
//
//   - The writer opens its transactions with BEGIN IMMEDIATE (_txlock=immediate)
//     and is capped at one connection. A deferred transaction that reads and
//     then writes takes SQLITE_BUSY_SNAPSHOT under concurrency, and busy_timeout
//     does not help, because the snapshot it holds can never become writable.
//     Serialising writes in Go avoids the whole class; a sync batch commits in
//     well under a millisecond per message, so there is nothing to gain by
//     trying to be cleverer.
//
//   - The reader carries query_only, so a handler that means to read cannot
//     write by accident, and WAL lets it run while the writer works.
//
// Everything here is pure Go: modernc.org/sqlite, no cgo, which is what lets
// the binary be built and shipped without a C toolchain. Its libc dependency is
// pinned exactly; the two must move together.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store owns the two pools. It is safe for concurrent use.
type Store struct {
	w *sql.DB // writer: one connection, BEGIN IMMEDIATE
	r *sql.DB // readers: query_only, several connections

	path string
	now  func() time.Time
	// moves is what the index remembers, for a few minutes and in memory
	// only, about moves a person asked for (moves.go).
	moves *moveMemory
}

// Options configure Open. The zero value is usable.
type Options struct {
	// ReadConns bounds the reader pool. Zero picks a sensible default.
	ReadConns int
	// Now overrides the clock, for tests.
	Now func() time.Time
	// SkipMigrate leaves the schema alone, for the `migrate` subcommand which
	// wants to report what it is about to do.
	SkipMigrate bool
}

const defaultReadConns = 4

// Open prepares the database file and both pools, applying any pending
// migrations. The file and its WAL sidecars are tightened to 0600 after the
// driver has created them: they hold sealed credentials and every subject line
// in the index, and the process umask is not something to rely on.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}

	readConns := opts.ReadConns
	if readConns <= 0 {
		readConns = defaultReadConns
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	// From here on, a failure closes what was already opened; those close
	// errors are not reported because the open error is the one that matters.
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	// One writer connection, always. More would only produce lock contention
	// that SQLite resolves by failing.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	if err := w.PingContext(ctx); err != nil {
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = w.Close()
		return nil, fmt.Errorf("store: connect writer: %w", err)
	}
	if err := chmodDatabase(path); err != nil {
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = w.Close()
		return nil, err
	}

	s := &Store{w: w, path: path, now: now, moves: newMoveMemory()}
	if !opts.SkipMigrate {
		if err := s.Migrate(ctx); err != nil {
			//nolint:errcheck // unwinding a failed open; the open error is the one to report
			_ = w.Close()
			return nil, err
		}
	}

	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	r.SetMaxOpenConns(readConns)
	r.SetMaxIdleConns(readConns)
	r.SetConnMaxLifetime(0)
	if err := r.PingContext(ctx); err != nil {
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = w.Close()
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = r.Close()
		return nil, fmt.Errorf("store: connect reader: %w", err)
	}
	s.r = r
	return s, nil
}

// dsn builds the connection string. The pragmas are fixed here rather than
// taken from configuration: modernc executes _pragma values verbatim, so a
// DSN assembled from settings would be an injection point for anyone who can
// edit the environment.
func dsn(path string, writer bool) string {
	v := url.Values{}
	v.Add("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "busy_timeout(5000)")
	v.Add("_pragma", "foreign_keys(1)")
	v.Add("_pragma", "synchronous(NORMAL)")
	if writer {
		// Deleted content is overwritten with zeros rather than left in free
		// pages until something reuses them: deleting a person or a mailbox
		// has to remove their address from the file, not only from the
		// tables. Scrub then takes the old copies out of the WAL.
		v.Add("_pragma", "secure_delete(1)")
	}
	q := v.Encode()
	if writer {
		// Every writing transaction takes the write lock at BEGIN.
		return "file:" + path + "?" + q + "&_txlock=immediate"
	}
	// A real guarantee, not a convention: a write on this pool fails with
	// SQLITE_READONLY rather than sneaking through.
	return "file:" + path + "?" + q + "&_pragma=query_only(1)"
}

// chmodDatabase tightens the database and its WAL sidecars.
func chmodDatabase(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: chmod %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// Close shuts both pools down.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		if err := s.r.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close reader: %w", err))
		}
	}
	if s.w != nil {
		if err := s.w.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close writer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Reader is the query-only pool. Use it for everything that does not write.
func (s *Store) Reader() *sql.DB { return s.r }

// Writer is the single-connection pool. Prefer Write, which wraps a
// transaction; this exists for the few statements that must run outside one.
func (s *Store) Writer() *sql.DB { return s.w }

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.now() }

// Write runs fn inside an immediate transaction and commits it.
//
// Every write goes through here so that the "read then write" shape, which is
// what produces SQLITE_BUSY_SNAPSHOT, cannot be written by accident: the
// transaction already holds the write lock before fn sees it.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ErrScrubBusy is a Scrub that could not empty the WAL, because a reader was
// still using what it holds. The next one, or SQLite's own checkpoints, finish
// the job.
var ErrScrubBusy = errors.New("store: a reader still holds the write-ahead log")

// Scrub moves everything in the write-ahead log into the database file and
// truncates the log to nothing.
//
// secure_delete zeroes what a transaction deletes, but in WAL mode the zeroed
// pages are appended to the log while the earlier frames, which still hold the
// deleted rows, stay in it until a checkpoint lets SQLite start the log over
// and something happens to overwrite them. After deleting a person, "deleted"
// should mean gone from the files on disk, so the callers that delete people
// and mailboxes call this once they have committed.
func (s *Store) Scrub(ctx context.Context) error {
	var busy, frames, done int
	if err := s.w.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	if busy != 0 {
		return ErrScrubBusy
	}
	return nil
}

// Read runs fn against the read-only pool.
func (s *Store) Read(ctx context.Context, fn func(context.Context, *sql.DB) error) error {
	return fn(ctx, s.r)
}

// Meta reads a value from the meta table. A missing key yields "".
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read meta %q: %w", key, err)
	}
	return value, nil
}

// SetMeta writes a value to the meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value)
		if err != nil {
			return fmt.Errorf("store: write meta %q: %w", key, err)
		}
		return nil
	})
}
