package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Per-account sync bookkeeping. These are columns on accounts, but they are
// the engine's: how the last pass went and when to try again. Each write
// touches an account only while sync is permitted for it, so a worker that
// has not yet heard of a withdrawal cannot write back what it reset. The account's
// state (active, needs_reauth, error...) is not here — it is changed through
// internal/account, which journals every change.

// RecordSyncOK notes a pass that finished without error: the failure count
// and backoff go back to zero.
func (s *Store) RecordSyncOK(ctx context.Context, accountID string, at time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE accounts AS a SET last_ok_at = ?, last_error = '', consecutive_failures = 0,
			next_retry_at = 0 WHERE a.id = ? AND `+syncPermitted, at.Unix(), accountID)
		if err != nil {
			return fmt.Errorf("store: record a sync pass: %w", err)
		}
		return nil
	})
}

// RecordSyncFailure notes a failed pass — class is provider.Class, a short
// fixed name, never the server's words — and when the engine will try again,
// and returns how many passes in a row have now failed.
func (s *Store) RecordSyncFailure(ctx context.Context, accountID, class string, nextRetry time.Time) (int, error) {
	var failures int
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE accounts AS a SET last_error = ?, consecutive_failures = consecutive_failures + 1,
			next_retry_at = ? WHERE a.id = ? AND `+syncPermitted+` RETURNING consecutive_failures`,
			class, unixOrZero(nextRetry), accountID).Scan(&failures)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: record a sync failure: %w", err)
		}
		return nil
	})
	return failures, err
}

// RecordIdleEvent notes when the connection parked in IDLE last heard from the
// server, for whoever is wondering whether it is still listening. The engine
// throttles this; it is observability, not state.
func (s *Store) RecordIdleEvent(ctx context.Context, accountID string, at time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE accounts AS a SET last_idle_event_at = ? WHERE a.id = ? AND `+syncPermitted,
			at.Unix(), accountID)
		if err != nil {
			return fmt.Errorf("store: record an idle event: %w", err)
		}
		return nil
	})
}

// SyncSummary is where an account's index stands, from the database alone.
// The engine adds what only it knows (whether a worker is running, backoff in
// memory) to make a status.
type SyncSummary struct {
	// FoldersTotal counts the folders that are synced at all: selectable, of
	// a role the provider syncs, and present in the last LIST.
	FoldersTotal int
	// FoldersSynced counts those whose initial sync has finished. A folder
	// whose last pass failed still counts when it had finished: the failure
	// is recorded on the folder, and it did not undo what is indexed.
	FoldersSynced int
	// Messages counts indexed rows that are live, label copies included.
	Messages int64
	// InitialTotal and InitialFetched sum the folders' initial windows and
	// how much of them is indexed.
	InitialTotal   int64
	InitialFetched int64

	Tier                string
	LastOKAt            time.Time
	LastError           string
	ConsecutiveFailures int
	NextRetryAt         time.Time
}

// InitialProgress is 0..100: how much of the initial windows is indexed, and
// 100 once every synced folder has finished its initial sync.
func (s SyncSummary) InitialProgress() int {
	if s.FoldersTotal > 0 && s.FoldersSynced >= s.FoldersTotal {
		return 100
	}
	if s.InitialTotal <= 0 {
		return 0
	}
	p := int(s.InitialFetched * 100 / s.InitialTotal)
	switch {
	case p < 0:
		return 0
	case p > 99:
		// Not 100 until every folder says it is done: the last batch can be
		// written before the folder's initial sync is closed.
		return 99
	}
	return p
}

// SyncSummary reads an account's sync position.
func (s *Store) SyncSummary(ctx context.Context, accountID string) (SyncSummary, error) {
	var (
		out               SyncSummary
		lastOK, nextRetry int64
	)
	err := s.r.QueryRowContext(ctx, `SELECT sync_tier_resolved, last_ok_at, last_error, consecutive_failures, next_retry_at
		FROM accounts WHERE id = ?`, accountID).Scan(&out.Tier, &lastOK, &out.LastError, &out.ConsecutiveFailures, &nextRetry)
	if err != nil {
		return SyncSummary{}, fmt.Errorf("store: read sync summary: %w", err)
	}
	out.LastOKAt, out.NextRetryAt = unixTime(lastOK), unixTime(nextRetry)
	err = s.r.QueryRowContext(ctx, `SELECT count(*),
			coalesce(sum(sync_state IN ('live', 'error') AND uidvalidity <> 0 AND backfill_cursor = 0), 0),
			coalesce(sum(initial_total), 0), coalesce(sum(min(initial_fetched, initial_total)), 0)
		FROM folders WHERE account_id = ? AND synced = 1 AND selectable = 1 AND missing_since = 0`, accountID).
		Scan(&out.FoldersTotal, &out.FoldersSynced, &out.InitialTotal, &out.InitialFetched)
	if err != nil {
		return SyncSummary{}, fmt.Errorf("store: read sync summary: %w", err)
	}
	err = s.r.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE account_id = ? AND vanished_at = 0`, accountID).
		Scan(&out.Messages)
	if err != nil {
		return SyncSummary{}, fmt.Errorf("store: read sync summary: %w", err)
	}
	return out, nil
}
