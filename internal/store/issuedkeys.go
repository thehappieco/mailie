package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Keys an issuer makes (migration 0028; docs/workspaces.md, "Keys an issuer
// makes"): what an extension that mirrors them writes beside the directory
// it follows.

// DirectoryKeys is an issuer's key feed's place: the directory reset it
// counts under, where it was read up to, up to when the issuer said it
// published everything (microseconds), the highest of that trusted before
// the last reset, and whether the reset's pass is open.
type DirectoryKeys struct {
	Issuer             string
	Reset              int64
	Cursor             int64
	PublishedThroughUS int64
	TrustedThroughUS   int64
	PassOpen           bool
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
	var k DirectoryKeys
	err := tx.QueryRowContext(ctx, `SELECT issuer, reset, cursor, published_through_us, trusted_through_us, pass_open
		FROM directory_keys WHERE issuer = ?`, issuer).
		Scan(&k.Issuer, &k.Reset, &k.Cursor, &k.PublishedThroughUS, &k.TrustedThroughUS, &k.PassOpen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DirectoryKeys{}, fmt.Errorf("%w: %s", ErrNoDirectory, issuer)
	case err != nil:
		return DirectoryKeys{}, fmt.Errorf("store: read the issuer's keys: %w", err)
	}
	return k, nil
}
