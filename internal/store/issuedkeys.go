package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Keys an issuer makes (migration 0028; docs/workspaces.md, "Keys an issuer
// makes"): what an extension that mirrors them writes beside the directory
// it follows.

// The periods an issued key's traces are kept for (docs/workspaces.md, "Keys
// an issuer makes", "What is kept, and for how long").
const (
	// IssuedKeyRecordRetention is how long an issued key's full record stays
	// once it ended: counted from its own revocation or expiry, or the end
	// of the pass that did not list it.
	IssuedKeyRecordRetention = 30 * 24 * time.Hour
	// IssuedKeyEndRetention is how long what is left of an issued key (its
	// selector, how and when it ended) stays: until its creation and this
	// long.
	IssuedKeyEndRetention = 365 * 24 * time.Hour
	// IssuedKeyTellRetention is the longest anything owed to the issuer
	// waits for it to accept it.
	IssuedKeyTellRetention = 30 * 24 * time.Hour
	// IssuedKeyUseRetention is how long a day's refused uses are kept, at
	// most; a day before yesterday goes earlier, once a report carried it.
	IssuedKeyUseRetention = 30 * 24 * time.Hour
)

// DirectoryKeys is an issuer's key feed's place: the directory reset it
// counts under, where it was read up to, up to when the issuer said it
// published everything (microseconds), the highest of that trusted before
// the last reset, whether the reset's pass is open, and when a page was last
// applied (microseconds, 0 for never).
type DirectoryKeys struct {
	Issuer             string
	Reset              int64
	Cursor             int64
	PublishedThroughUS int64
	TrustedThroughUS   int64
	PassOpen           bool
	AppliedAtUS        int64
}

const directoryKeysColumns = `issuer, reset, cursor, published_through_us, trusted_through_us, pass_open, applied_at_us`

func scanDirectoryKeys(row interface{ Scan(...any) error }) (DirectoryKeys, error) {
	var k DirectoryKeys
	err := row.Scan(&k.Issuer, &k.Reset, &k.Cursor, &k.PublishedThroughUS, &k.TrustedThroughUS, &k.PassOpen,
		&k.AppliedAtUS)
	return k, err
}

// EnsureDirectoryKeysTx makes, inside the caller's transaction, the key
// feed's row of a directory already followed when it has none — at the
// directory's current reset, cursor 0, no pass open — and returns it as it
// is: what the first page of an issuer's keys, and a reset of its directory,
// do. ErrNoDirectory when the directory is not followed.
func EnsureDirectoryKeysTx(ctx context.Context, tx *sql.Tx, issuer string) (DirectoryKeys, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO directory_keys(issuer, reset)
		SELECT d.issuer, d.reset FROM directories d WHERE d.issuer = ?
		ON CONFLICT(issuer) DO NOTHING`, issuer); err != nil {
		return DirectoryKeys{}, fmt.Errorf("store: follow the issuer's keys: %w", err)
	}
	k, err := scanDirectoryKeys(tx.QueryRowContext(ctx, `SELECT `+directoryKeysColumns+`
		FROM directory_keys WHERE issuer = ?`, issuer))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DirectoryKeys{}, fmt.Errorf("%w: %s", ErrNoDirectory, issuer)
	case err != nil:
		return DirectoryKeys{}, fmt.Errorf("store: read the issuer's keys: %w", err)
	}
	return k, nil
}

// DirectoryKeysOf reads an issuer's key feed; false when it has none yet.
func (s *Store) DirectoryKeysOf(ctx context.Context, issuer string) (DirectoryKeys, bool, error) {
	return directoryKeysOn(ctx, s.r, issuer)
}

// DirectoryKeysTx is DirectoryKeysOf inside the caller's transaction.
func DirectoryKeysTx(ctx context.Context, tx *sql.Tx, issuer string) (DirectoryKeys, bool, error) {
	return directoryKeysOn(ctx, tx, issuer)
}

func directoryKeysOn(ctx context.Context, q querier, issuer string) (DirectoryKeys, bool, error) {
	k, err := scanDirectoryKeys(q.QueryRowContext(ctx, `SELECT `+directoryKeysColumns+`
		FROM directory_keys WHERE issuer = ?`, issuer))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DirectoryKeys{}, false, nil
	case err != nil:
		return DirectoryKeys{}, false, fmt.Errorf("store: read the issuer's keys: %w", err)
	}
	return k, true, nil
}

// SetDirectoryKeysCursorTx moves, inside the caller's transaction, an
// issuer's key feed to cursor, records when the page was applied, and, for
// the page that said it was the last (last), up to when the issuer published
// everything, which only rises, and the end of the reset's pass. It is a
// compare and set on the feed's reset and cursor: ErrDirectoryMoved when the
// feed counts under another reset or is already past cursor.
func SetDirectoryKeysCursorTx(ctx context.Context, tx *sql.Tx, issuer string, expectReset, cursor int64, last bool,
	publishedThroughUS, appliedAtUS int64,
) error {
	if cursor < 0 || publishedThroughUS < 0 {
		return fmt.Errorf("%w: a cursor and a moment are never negative", ErrDirectoryMoved)
	}
	if !last {
		publishedThroughUS = 0
	}
	res, err := tx.ExecContext(ctx, `UPDATE directory_keys
		SET cursor = ?, published_through_us = max(published_through_us, ?),
		    pass_open = CASE WHEN ? THEN 0 ELSE pass_open END, applied_at_us = ?
		WHERE issuer = ? AND reset = ? AND cursor <= ?`,
		cursor, publishedThroughUS, last, appliedAtUS, issuer, expectReset, cursor)
	if err != nil {
		return fmt.Errorf("store: move the issuer's key feed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: move the issuer's key feed: %w", err)
	}
	if n == 0 {
		return ErrDirectoryMoved
	}
	return nil
}

// resetIssuedKeysTx starts an issuer's keys again with its directory, inside
// ResetDirectoryTx's transaction, once the directory's own row moved: its
// feed, made when missing, counts under the new reset from 0, with what it
// trusted kept as the highest it ever trusted (never lowered by a second
// reset before the pass ended) and the reset's pass open; each of its keys
// waits to be listed again (version 0, not listed), and so does each grant;
// and every refusal owed goes, since it names versions the new generation
// never issued. Nothing else changes: revocations, expiries, grants held and
// waiting, tombstones, stops, and what is owed of a key held narrower, stay:
// a reset never widens a key. A directory just created has no keys and no
// pass to open (reset is false).
func resetIssuedKeysTx(ctx context.Context, tx *sql.Tx, issuer string, reset bool) error {
	if !reset {
		if _, err := tx.ExecContext(ctx, `INSERT INTO directory_keys(issuer, reset)
			SELECT d.issuer, d.reset FROM directories d WHERE d.issuer = ?
			ON CONFLICT(issuer) DO NOTHING`, issuer); err != nil {
			return fmt.Errorf("store: follow the issuer's keys: %w", err)
		}
		return nil
	}
	steps := []struct{ what, query string }{
		{"reset the issuer's key feed", `INSERT INTO directory_keys(issuer, reset, pass_open)
			SELECT d.issuer, d.reset, 1 FROM directories d WHERE d.issuer = ?1
			ON CONFLICT(issuer) DO UPDATE SET reset = excluded.reset, cursor = 0, published_through_us = 0,
			  trusted_through_us = max(trusted_through_us, published_through_us), pass_open = 1`},
		{"forget its keys' versions", `UPDATE issued_keys SET version = 0, listed = 0 WHERE issuer = ?1`},
		{"forget its grants' versions", `UPDATE issued_key_grants SET version = 0
			WHERE prefix IN (SELECT prefix FROM issued_keys WHERE issuer = ?1)`},
		{"drop the refusals owed", `DELETE FROM issued_key_tell WHERE issuer = ?1 AND kind = 'refusal'`},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, issuer); err != nil {
			return fmt.Errorf("store: %s: %w", step.what, err)
		}
	}
	return nil
}
