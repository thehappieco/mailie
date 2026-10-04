package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
)

// rewrapCredentials re-seals every stored credential under the active key.
//
// It selects on the keyid column beside each envelope rather than opening every
// row to look, so a database with nothing to do costs one indexed query. The
// rows it does touch are opened and re-sealed inside one transaction: a crash
// halfway would otherwise leave some accounts readable only with a key the
// operator is about to delete.
func rewrapCredentials(ctx context.Context, db *store.Store, keyring *secrets.Keyring) (int, error) {
	type row struct {
		accountID string
		field     string
		envelope  []byte
	}

	var pending []row
	err := db.Read(ctx, func(ctx context.Context, r *sql.DB) error {
		rows, err := r.QueryContext(ctx,
			`SELECT account_id, field, ciphertext FROM credentials WHERE keyid <> ?`, keyring.ActiveKeyID())
		if err != nil {
			return fmt.Errorf("rewrap: list credentials: %w", err)
		}
		//nolint:errcheck // read-only query
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c row
			if err := rows.Scan(&c.accountID, &c.field, &c.envelope); err != nil {
				return fmt.Errorf("rewrap: scan credential: %w", err)
			}
			pending = append(pending, c)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	err = db.Write(ctx, func(tx *sql.Tx) error {
		for _, c := range pending {
			sealed, err := keyring.Rewrap(c.accountID, c.field, c.envelope)
			if err != nil {
				// Naming the account and field is what tells the operator
				// which previous key they still need to supply.
				return fmt.Errorf("rewrap: account %s, field %s: %w", c.accountID, c.field, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE credentials SET ciphertext = ?, keyid = ?, updated_at = ?
				  WHERE account_id = ? AND field = ?`,
				sealed, keyring.ActiveKeyID(), db.Now().Unix(), c.accountID, c.field,
			); err != nil {
				return fmt.Errorf("rewrap: update account %s, field %s: %w", c.accountID, c.field, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(pending), nil
}
