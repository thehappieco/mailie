package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/thehappieco/mailie/internal/secrets"
)

// The send-hash root is the key of the hashes a send record keeps of what was
// composed, and of the idempotency key of a send that came without one
// (service.Deps.SendHashKey): without it, neither the record nor a log line
// can confirm a guess of a message.
//
// It is random, made once, and kept in the meta table sealed like a
// credential, so it opens wherever the credentials do, survives a rotation of
// the key (rewrap-credentials re-seals it with them) and never needs key
// material a sealer may not have to hand.
const (
	// MetaSendHashRoot is the meta row that keeps the root: the base64 of
	// its envelope.
	MetaSendHashRoot = "send_hash_root"
	// SendHashRootLen is the root's size.
	SendHashRootLen = 32
)

// SendHashRootBinding is what the root is sealed for: a credential's envelope
// moved into its row, or its envelope into a credential's, does not open.
var SendHashRootBinding = secrets.Binding{Purpose: secrets.PurposeSendHashRoot, Ref: "meta/" + MetaSendHashRoot}

// ErrSendHashRoot is a send-hash root the configured keys do not open: the
// key that sealed it is not given, the row was altered, or it is not a root
// (secrets.DoesNotOpen, or the row's own shape). A sealer that could not try
// (a key service not reached, or that refused the call, a context that
// ended) is never this: that says nothing about the root, which may open on
// the next try.
var ErrSendHashRoot = errors.New("store: the send-hash root does not open with the configured keys")

// ErrSendHashRootOpens is a replacement asked for a root the configured
// sealer opens, which nothing needs and which would forget every send record.
var ErrSendHashRootOpens = errors.New("store: the send-hash root opens with the configured keys; it is not replaced")

// SendHashRoot opens the database's send-hash root, and reports whether it
// made it now: the first time a database without one is opened. A root the
// configured keys do not open is ErrSendHashRoot, never replaced: that would
// hide the wrong key until the first credential it cannot open. Any other
// error is returned as it is.
func (s *Store) SendHashRoot(ctx context.Context, sealer secrets.Sealer) ([]byte, bool, error) {
	stored, err := s.Meta(ctx, MetaSendHashRoot)
	if err != nil {
		return nil, false, err
	}
	if stored != "" {
		root, err := openSendHashRoot(ctx, sealer, stored)
		return root, false, err
	}

	fresh, err := sealSendHashRoot(ctx, sealer)
	if err != nil {
		return nil, false, err
	}
	created := false
	err = s.Write(ctx, func(tx *sql.Tx) error {
		// Kept only if no root got there first, and the one that is there
		// is read back in the same transaction: there is only ever one.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, MetaSendHashRoot, fresh)
		if err != nil {
			return fmt.Errorf("store: keep the send-hash root: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: keep the send-hash root: %w", err)
		}
		created = n == 1
		return tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, MetaSendHashRoot).Scan(&stored)
	})
	if err != nil {
		return nil, false, err
	}
	root, err := openSendHashRoot(ctx, sealer, stored)
	return root, created, err
}

// ResealSendHashRootTx seals the root again with sealer's active sealer when
// its envelope is not what that writes now, opening it with whichever sealer
// knows it, and reports whether it did. A database without a root has nothing
// to re-seal: its daemon's next start makes one.
func ResealSendHashRootTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) (bool, error) {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, MetaSendHashRoot).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read the send-hash root: %w", err)
	}
	envelope, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrSendHashRoot, secrets.ErrMalformed)
	}
	if sealer.Current(envelope) {
		return false, nil
	}
	// Opened as a root, its size checked, before it is sealed again: a row
	// that holds something else is refused, not carried over.
	root, err := openSendHashRoot(ctx, sealer, stored)
	if err != nil {
		return false, err
	}
	resealed, err := sealer.Seal(ctx, SendHashRootBinding, root)
	clear(root)
	if err != nil {
		return false, fmt.Errorf("store: seal the send-hash root: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = ?`,
		base64.StdEncoding.EncodeToString(resealed), MetaSendHashRoot); err != nil {
		return false, fmt.Errorf("store: keep the send-hash root: %w", err)
	}
	return true, nil
}

// ReplaceSendHashRootTx puts a new root in place of one the configured keys
// do not open (ErrSendHashRoot), for a database whose key is lost for good.
// It refuses to replace one they open (ErrSendHashRootOpens), and writes
// nothing when the sealer could not try to open it: a key service not
// reached, or that refuses to decrypt while it still seals, says nothing
// about the root, and replacing a root that would still open forgets every
// send record. A send record made under the old root is no longer
// recognised: a key-less send repeated after the replacement is sent again,
// and one repeated with its idempotency key is refused as that key reused.
func ReplaceSendHashRootTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) error {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, MetaSendHashRoot).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store: read the send-hash root: %w", err)
	default:
		root, err := openSendHashRoot(ctx, sealer, stored)
		if err == nil {
			clear(root)
			return ErrSendHashRootOpens
		}
		if !errors.Is(err, ErrSendHashRoot) {
			return err
		}
	}
	fresh, err := sealSendHashRoot(ctx, sealer)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		MetaSendHashRoot, fresh); err != nil {
		return fmt.Errorf("store: keep the send-hash root: %w", err)
	}
	return nil
}

// sealSendHashRoot makes a new root and seals it, as the meta row keeps it.
func sealSendHashRoot(ctx context.Context, sealer secrets.Sealer) (string, error) {
	root := make([]byte, SendHashRootLen)
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(root)
	envelope, err := sealer.Seal(ctx, SendHashRootBinding, root)
	clear(root)
	if err != nil {
		return "", fmt.Errorf("store: seal the send-hash root: %w", err)
	}
	return base64.StdEncoding.EncodeToString(envelope), nil
}

// openSendHashRoot opens a root as the meta row keeps it. Only what says the
// root does not open with the configured keys is ErrSendHashRoot: a row that
// is not base64, an envelope the sealer reports as not opening here
// (secrets.DoesNotOpen), a plaintext of the wrong size. Every other error of
// the sealer is returned as it is.
func openSendHashRoot(ctx context.Context, sealer secrets.Sealer, stored string) ([]byte, error) {
	envelope, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSendHashRoot, secrets.ErrMalformed)
	}
	root, err := sealer.Open(ctx, SendHashRootBinding, envelope)
	if secrets.DoesNotOpen(err) {
		return nil, fmt.Errorf("%w: %w", ErrSendHashRoot, err)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open the send-hash root: %w", err)
	}
	if len(root) != SendHashRootLen {
		clear(root)
		return nil, fmt.Errorf("%w: it is %d bytes, not %d", ErrSendHashRoot, len(root), SendHashRootLen)
	}
	return root, nil
}
