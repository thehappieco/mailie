package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// An extension's own state (migration 0022): a few keys and values an
// extension of the daemon keeps beside the data it describes, so a restore
// of the database takes it back with that data. Each extension has its own
// keys; what they mean is the extension's.

// Bounds of an extension's state, which the schema holds too.
const (
	// MaxExtensionName is the longest name of an extension, in bytes.
	MaxExtensionName = 64
	// MaxExtensionStateKey is the longest key, in bytes.
	MaxExtensionStateKey = 256
	// MaxExtensionStateValue is the largest value: a mebibyte.
	MaxExtensionStateValue = 1 << 20
)

// ErrExtensionState is an extension's name, a key or a value out of bounds:
// a name of 1 to 64 bytes, a key of 1 to 256, a value of a mebibyte at most.
var ErrExtensionState = errors.New("store: an extension's state is out of bounds")

func checkExtensionName(ext string) error {
	if len(ext) == 0 || len(ext) > MaxExtensionName {
		return fmt.Errorf("%w: an extension's name has 1 to %d bytes", ErrExtensionState, MaxExtensionName)
	}
	return nil
}

func checkExtensionKey(key string) error {
	if len(key) == 0 || len(key) > MaxExtensionStateKey {
		return fmt.Errorf("%w: a key has 1 to %d bytes", ErrExtensionState, MaxExtensionStateKey)
	}
	return nil
}

// ExtensionState reads one value of an extension's state; false when the key
// holds none.
func (s *Store) ExtensionState(ctx context.Context, ext, key string) ([]byte, bool, error) {
	if err := checkExtensionName(ext); err != nil {
		return nil, false, err
	}
	if err := checkExtensionKey(key); err != nil {
		return nil, false, err
	}
	var value []byte
	err := s.r.QueryRowContext(ctx, `SELECT value FROM extension_state WHERE extension = ? AND key = ?`, ext, key).
		Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: read the extension's state: %w", err)
	}
	if value == nil {
		value = []byte{}
	}
	return value, true, nil
}

// ExtensionStateKeys lists the keys of an extension's state that begin with
// prefix, every one for "", sorted by their bytes.
func (s *Store) ExtensionStateKeys(ctx context.Context, ext, prefix string) ([]string, error) {
	if err := checkExtensionName(ext); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT key FROM extension_state
		WHERE extension = ? AND substr(CAST(key AS BLOB), 1, length(CAST(? AS BLOB))) = CAST(? AS BLOB)`,
		ext, prefix, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: list the extension's state: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: list the extension's state: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the extension's state: %w", err)
	}
	sort.Strings(keys)
	return keys, nil
}

// WriteExtensionState writes an extension's state in one transaction: every
// key of put holds its value, a nil value as an empty one, and every key of
// del holds none, the deletions first. Nothing is written when any name, key
// or value is out of bounds (ErrExtensionState).
func (s *Store) WriteExtensionState(ctx context.Context, ext string, put map[string][]byte, del []string) error {
	if err := checkExtensionName(ext); err != nil {
		return err
	}
	for _, key := range del {
		if err := checkExtensionKey(key); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(put))
	for key, value := range put {
		if err := checkExtensionKey(key); err != nil {
			return err
		}
		if len(value) > MaxExtensionStateValue {
			return fmt.Errorf("%w: a value holds a mebibyte at most", ErrExtensionState)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	now := s.now().Unix()
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, key := range del {
			if _, err := tx.ExecContext(ctx, `DELETE FROM extension_state WHERE extension = ? AND key = ?`,
				ext, key); err != nil {
				return fmt.Errorf("store: write the extension's state: %w", err)
			}
		}
		for _, key := range keys {
			value := put[key]
			if value == nil {
				value = []byte{}
			}
			// coalesce: an empty value is still a value, never NULL.
			if _, err := tx.ExecContext(ctx, `INSERT INTO extension_state(extension, key, value, updated_at)
				VALUES (?, ?, coalesce(?, x''), ?)
				ON CONFLICT(extension, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				ext, key, value, now); err != nil {
				return fmt.Errorf("store: write the extension's state: %w", err)
			}
		}
		return nil
	})
}
