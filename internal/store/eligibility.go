package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotEligible is an account the engine may not store anything for: not
// active, or nobody has agreed to its mail being indexed.
var ErrNotEligible = errors.New("store: the account is not eligible to sync")

// syncEligible is the one rule for whether an account's mail may be indexed,
// as a WHERE fragment over accounts aliased a.
//
// An account is eligible when it is active and either
//
//   - it has an owner, that person is active, and they have consented to
//     sync (the privacy policy promises nothing is stored before they do); or
//   - it has no owner (an instance key created it) and the operator switched
//     sync on for it.
//
// sync_enabled_at never stands in for a person's consent: an operator cannot
// turn on sync for somebody else's mailbox, and a withdrawal stops the
// mailbox whatever that column says.
const syncEligible = `a.state = 'active' AND ` + syncPermitted

// syncPermitted is the consent half of the rule, as a boolean expression over
// accounts aliased a: the owner is active and consented or, for an account
// nobody owns, the operator switched sync on. SyncPermitted reports it on its
// own; syncEligible adds that the account is active.
const syncPermitted = `CASE WHEN a.owner_user_id IS NULL THEN a.sync_enabled_at <> 0
	ELSE EXISTS (SELECT 1 FROM users u WHERE u.id = a.owner_user_id
	               AND u.status = 'active' AND u.sync_consent_at <> 0) END`

// SyncEligibleAccounts lists every account the engine should be syncing,
// oldest first.
func (s *Store) SyncEligibleAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT a.id FROM accounts a WHERE `+syncEligible+` ORDER BY a.created_at, a.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts eligible to sync: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list accounts eligible to sync: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list accounts eligible to sync: %w", err)
	}
	return ids, nil
}

// SyncEligible reports whether one account should be syncing. An account
// that does not exist is not.
func (s *Store) SyncEligible(ctx context.Context, accountID string) (bool, error) {
	return syncEligibleOn(ctx, s.r, accountID)
}

// RequireSyncEligibleTx fails with ErrNotEligible unless the account may be
// synced, inside the caller's transaction.
//
// Every transaction that writes to the index starts with this. The database
// has one writer, so a withdrawal of consent — or an account being disabled
// or removed — and a sync batch are ordered: once the withdrawal commits, no
// batch that was already on its way can store another row, however late its
// worker notices it has been stopped.
func RequireSyncEligibleTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	ok, err := syncEligibleOn(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotEligible, accountID)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func syncEligibleOn(ctx context.Context, q queryRower, accountID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM accounts a WHERE a.id = ? AND `+syncEligible, accountID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check sync eligibility: %w", err)
	}
	return true, nil
}
