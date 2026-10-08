package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
)

// resealed is what a rewrap sealed again.
type resealed struct {
	// credentials is how many credentials rows it re-sealed.
	credentials int
	// root is whether it re-sealed the send-hash root.
	root bool
}

// rewrapCredentials seals again, with the active sealer, every stored
// credential and the send-hash root whose envelope is not what that sealer
// writes now, opening each with whichever configured sealer knows it.
//
// Every row is looked at, and only those that are not current are opened: an
// envelope says what sealed it, which the keyid column beside a credential
// cannot for every kind of sealer (it holds 0 for any envelope no keyring key
// sealed). Everything happens in one transaction: a crash halfway would
// otherwise leave some rows readable only with a key the operator is about to
// delete, and a row that cannot be opened leaves every row as it was. Run
// again, it finds nothing to do.
func rewrapCredentials(ctx context.Context, db *store.Store, sealer secrets.Sealer) (resealed, error) {
	var done resealed
	err := db.Write(ctx, func(tx *sql.Tx) error {
		pending, err := staleCredentials(ctx, tx, sealer)
		if err != nil {
			return err
		}
		for _, c := range pending {
			sealed, err := secrets.Reseal(ctx, sealer, secrets.Credential(c.accountID, c.field), c.envelope)
			if err != nil {
				// Naming the account and field is what tells the operator
				// which previous key they still need to supply.
				return fmt.Errorf("rewrap: account %s, field %s: %w", c.accountID, c.field, err)
			}
			keyID, err := secrets.KeyID(sealed)
			if err != nil {
				return fmt.Errorf("rewrap: account %s, field %s: %w", c.accountID, c.field, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE credentials SET ciphertext = ?, keyid = ?, updated_at = ?
				  WHERE account_id = ? AND field = ?`,
				sealed, keyID, db.Now().Unix(), c.accountID, c.field,
			); err != nil {
				return fmt.Errorf("rewrap: update account %s, field %s: %w", c.accountID, c.field, err)
			}
		}
		done.credentials = len(pending)

		root, err := store.ResealSendHashRootTx(ctx, tx, sealer)
		if err != nil {
			return fmt.Errorf("rewrap: %w", err)
		}
		done.root = root
		return nil
	})
	if err != nil {
		return resealed{}, err
	}
	return done, nil
}

// storedCredential is one credentials row as a rewrap reads it.
type storedCredential struct {
	accountID string
	field     string
	keyID     int64
	envelope  []byte
}

// staleCredentials lists the credentials rows sealer would not write as they
// are, without opening any. A row whose keyid column disagrees with its
// envelope is listed too, so the column says which key each row needs.
func staleCredentials(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) ([]storedCredential, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT account_id, field, keyid, ciphertext FROM credentials ORDER BY account_id, field`)
	if err != nil {
		return nil, fmt.Errorf("rewrap: list credentials: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var stale []storedCredential
	for rows.Next() {
		var c storedCredential
		if err := rows.Scan(&c.accountID, &c.field, &c.keyID, &c.envelope); err != nil {
			return nil, fmt.Errorf("rewrap: scan credential: %w", err)
		}
		if id, err := secrets.KeyID(c.envelope); err == nil && sealer.Current(c.envelope) && int64(id) == c.keyID {
			continue
		}
		stale = append(stale, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rewrap: list credentials: %w", err)
	}
	return stale, nil
}

// replaceSendHashRoot puts a new send-hash root in place of one no configured
// key opens any more, and touches nothing else: a credential sealed under a
// lost key stays as it is until its mailbox is authorized again.
func replaceSendHashRoot(ctx context.Context, db *store.Store, sealer secrets.Sealer) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		if err := store.ReplaceSendHashRootTx(ctx, tx, sealer); err != nil {
			return fmt.Errorf("rewrap: %w", err)
		}
		return nil
	})
}
