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

// A sealed secret is a random key of the server's own, made once and kept in
// the meta table sealed like a credential: it opens wherever the credentials
// do, survives a rotation of the key (rewrap-credentials re-seals it with
// them) and never needs key material a sealer may not have to hand. Two are
// kept so: the send-hash root (sendhash.go) and the salt key (saltkey.go).
// This file is what they share; each keeps its own errors and names.
type sealedSecret struct {
	// key is the meta row that keeps the base64 of the envelope.
	key string
	// sealedWith is the meta row that says, in words, what sealed it (the
	// sealer's Describe when it did). Nothing authenticates it, so it only
	// ever goes into a message, and decides nothing.
	sealedWith string
	// binding is what the secret is sealed for.
	binding secrets.Binding
	// size is the secret's length.
	size int
	// what names it in messages.
	what string
	// notOpen is the secret the configured keys do not open; opens a
	// replacement asked for one they open; mayOpen a replacement asked,
	// without saying the key is lost, for one of the configured key
	// service's own kind that it does not unwrap.
	notOpen, opens, mayOpen error
}

// open opens the secret, and reports whether it made it now: the first time
// a database without one is opened. One the configured keys do not open is
// s.notOpen, never replaced: that would hide the wrong key until the first
// credential it cannot open. Any other error is returned as it is.
func (sec sealedSecret) open(ctx context.Context, s *Store, sealer secrets.Sealer) ([]byte, bool, error) {
	stored, err := s.Meta(ctx, sec.key)
	if err != nil {
		return nil, false, err
	}
	if stored != "" {
		value, err := sec.openStored(ctx, sealer, stored)
		if errors.Is(err, sec.notOpen) {
			err = sealedWith(err, s.metaOrEmpty(ctx, sec.sealedWith))
		}
		return value, false, err
	}

	fresh, err := sec.seal(ctx, sealer)
	if err != nil {
		return nil, false, err
	}
	created := false
	err = s.Write(ctx, func(tx *sql.Tx) error {
		// Kept only if no secret got there first, and the one that is there
		// is read back in the same transaction: there is only ever one.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, sec.key, fresh)
		if err != nil {
			return fmt.Errorf("store: keep the %s: %w", sec.what, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: keep the %s: %w", sec.what, err)
		}
		created = n == 1
		if created {
			if err := sec.recordSealedWithTx(ctx, tx, sealer); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, sec.key).Scan(&stored)
	})
	if err != nil {
		return nil, false, err
	}
	value, err := sec.openStored(ctx, sealer, stored)
	return value, created, err
}

// resealTx seals the secret again with sealer's active sealer when its
// envelope is not what that writes now, opening it with whichever sealer
// knows it, and reports whether it did. A database without one has nothing to
// re-seal: its daemon's next start makes one.
//
// One that is current is opened all the same, and one that does not open is
// an error, never "already sealed": under a key service the header names
// neither the key nor the env, so a secret sealed under another KMS key or
// another MAIL_ENV reads as current and opens only once they are put back.
func (sec sealedSecret) resealTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) (bool, error) {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, sec.key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read the %s: %w", sec.what, err)
	}
	envelope, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return false, sealedWith(fmt.Errorf("%w: %w", sec.notOpen, secrets.ErrMalformed),
			sec.sealedWithTx(ctx, tx))
	}
	// Opened, its size checked, before it is sealed again: a row that holds
	// something else is refused, not carried over.
	value, err := sec.openStored(ctx, sealer, stored)
	if errors.Is(err, sec.notOpen) {
		return false, sealedWith(err, sec.sealedWithTx(ctx, tx))
	}
	if err != nil {
		return false, err
	}
	if sealer.Current(envelope) {
		clear(value)
		return false, nil
	}
	resealed, err := sealer.Seal(ctx, sec.binding, value)
	clear(value)
	if err != nil {
		return false, fmt.Errorf("store: seal the %s: %w", sec.what, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = ?`,
		base64.StdEncoding.EncodeToString(resealed), sec.key); err != nil {
		return false, fmt.Errorf("store: keep the %s: %w", sec.what, err)
	}
	if err := sec.recordSealedWithTx(ctx, tx, sealer); err != nil {
		return false, err
	}
	return true, nil
}

// replaceTx puts a new secret in place of one the configured keys do not open
// (sec.notOpen), for a database whose key is lost for good. It refuses to
// replace one they open (sec.opens), and writes nothing when the sealer could
// not try to open it: a key service not reached, or that refuses to decrypt
// while it still seals, says nothing about the secret. One of the configured
// key service's own kind that it does not unwrap (secrets.ErrSealedElsewhere)
// is most likely under another KMS key or MAIL_ENV, which putting back opens:
// it is replaced only with kmsKeyLost, the operator's word that the key that
// sealed it is lost for good, and is otherwise sec.mayOpen.
func (sec sealedSecret) replaceTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer, kmsKeyLost bool) error {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, sec.key).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store: read the %s: %w", sec.what, err)
	default:
		value, err := sec.openStored(ctx, sealer, stored)
		if err == nil {
			clear(value)
			return sec.opens
		}
		if !errors.Is(err, sec.notOpen) {
			return err
		}
		if errors.Is(err, secrets.ErrSealedElsewhere) && !kmsKeyLost {
			return fmt.Errorf("%w: %w", sec.mayOpen, sealedWith(err, sec.sealedWithTx(ctx, tx)))
		}
	}
	fresh, err := sec.seal(ctx, sealer)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		sec.key, fresh); err != nil {
		return fmt.Errorf("store: keep the %s: %w", sec.what, err)
	}
	return sec.recordSealedWithTx(ctx, tx, sealer)
}

// recordSealedWithTx keeps, beside the secret just sealed, what sealed it.
func (sec sealedSecret) recordSealedWithTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		sec.sealedWith, sealer.Describe()); err != nil {
		return fmt.Errorf("store: record what sealed the %s: %w", sec.what, err)
	}
	return nil
}

// sealedWithTx is what the meta row says sealed the secret, or "" when no row
// says it (a secret made before the row was kept) or it cannot be read: it is
// only ever for a message.
func (sec sealedSecret) sealedWithTx(ctx context.Context, tx *sql.Tx) string {
	var v string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, sec.sealedWith).Scan(&v); err != nil {
		return ""
	}
	return v
}

// seal makes a new secret and seals it, as the meta row keeps it.
func (sec sealedSecret) seal(ctx context.Context, sealer secrets.Sealer) (string, error) {
	value := make([]byte, sec.size)
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(value)
	envelope, err := sealer.Seal(ctx, sec.binding, value)
	clear(value)
	if err != nil {
		return "", fmt.Errorf("store: seal the %s: %w", sec.what, err)
	}
	return base64.StdEncoding.EncodeToString(envelope), nil
}

// openStored opens the secret as the meta row keeps it. Only what says it
// does not open with the configured keys is sec.notOpen: a row that is not
// base64, an envelope the sealer reports as not opening here
// (secrets.DoesNotOpen), a plaintext of the wrong size. Every other error of
// the sealer is returned as it is.
func (sec sealedSecret) openStored(ctx context.Context, sealer secrets.Sealer, stored string) ([]byte, error) {
	envelope, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sec.notOpen, secrets.ErrMalformed)
	}
	value, err := sealer.Open(ctx, sec.binding, envelope)
	if secrets.DoesNotOpen(err) {
		return nil, fmt.Errorf("%w: %w", sec.notOpen, err)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open the %s: %w", sec.what, err)
	}
	if len(value) != sec.size {
		clear(value)
		return nil, fmt.Errorf("%w: it is %d bytes, not %d", sec.notOpen, len(value), sec.size)
	}
	return value, nil
}

// metaOrEmpty is Meta for a message: "" for a row that is missing or cannot
// be read.
func (s *Store) metaOrEmpty(ctx context.Context, key string) string {
	v, err := s.Meta(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// sealedWith adds to a secret that does not open what sealed it, when the
// meta row says so.
func sealedWith(err error, recorded string) error {
	if recorded == "" {
		return err
	}
	return fmt.Errorf("%w (it was sealed with %s)", err, recorded)
}
