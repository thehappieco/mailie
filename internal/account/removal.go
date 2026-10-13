package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Removing a mailbox keeps it (migration 0020; docs/workspaces.md, "Removed
// mailboxes").
//
// A removed mailbox stays, with its credentials, folders, messages, drafts,
// sends, key, grants and consent, for RemovedKeptFor, used by nothing
// meanwhile: it is not found by anyone (Get, Visibility), dials nothing and
// refreshes nothing (Registry.Mailbox), syncs nothing and has no reader
// (store.LiveSQL). What keys held on it goes at once — what an instance key
// was restricted to and what a workspace key held — and so do the consent
// attempts under way on it; nothing of that comes back. Within those days it
// may be recovered, which clears the removal and holds its sync until
// someone who may turns it on; after them it is deleted for good by the
// hourly sweep (PurgeExpired), or before by whoever may. A mailbox that never
// connected — nothing was ever stored to authenticate with — is deleted for
// good at once instead: there is nothing in it to keep. Who may do which is
// the service's: each takes a check run in its transaction.
//
// The removal, the recovery and the deletion for good are journaled in the
// transaction that makes them (store.JournalMailboxTx); every reader of a
// mailbox removed but whoever removed it is told.

// RemovedKeptFor is how long a removed mailbox is kept, recoverable, before
// it may be deleted for good: 30 days, on the repository's clock, which the
// schema holds every removal to.
const RemovedKeptFor = 30 * 24 * time.Hour

// Removal is what removing a mailbox did.
type Removal struct {
	AccountID string
	// Purged is a mailbox that never connected, deleted for good at once.
	Purged bool
	// RemovedAt is when a kept mailbox was removed, and PurgeAfter when it
	// may be deleted for good; both zero for one purged.
	RemovedAt  time.Time
	PurgeAfter time.Time
	// Told are the readers given a notice of the removal: everyone who read
	// the mailbox but whoever removed it.
	Told []string
}

// RemoveCheck re-reads, inside the transaction that removes a mailbox, that
// the caller may: neverConnected says the mailbox never connected, which is
// deleted for good at once, and which more people may abandon.
type RemoveCheck func(tx *sql.Tx, neverConnected bool) error

// remove removes a live mailbox, as actor ("usr_…", "key:<prefix>", "cli"),
// after check (nil checks nothing): kept for RemovedKeptFor, or deleted for
// good at once when it never connected. ErrNotFound for a mailbox nobody
// knows, or one already removed.
func (r *Repository) remove(ctx context.Context, id, actor string, check RemoveCheck) (Removal, error) {
	at := r.now().UTC().Truncate(time.Second)
	out := Removal{AccountID: id}
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var neverConnected bool
		err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM credentials c WHERE c.account_id = a.id)
			FROM accounts a WHERE a.id = ? AND a.removed_at = 0`, id).Scan(&neverConnected)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("account: read the mailbox to remove: %w", err)
		}
		if check != nil {
			if err := check(tx, neverConnected); err != nil {
				return err
			}
		}
		// Its consent attempts end either way: a redirect that comes back
		// later finds nothing, and the registry stops what it runs for them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending WHERE account_id = ?`, id); err != nil {
			return fmt.Errorf("account: drop the mailbox's pending flows: %w", err)
		}
		if neverConnected {
			out.Purged = true
			_, err := purgeTx(ctx, tx, []string{id}, purge{forget: true, at: at})
			return err
		}
		// Read before the removal: a removed mailbox has no reader.
		readers, err := store.MailboxReadersTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out.RemovedAt, out.PurgeAfter = at, at.Add(RemovedKeptFor)
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = ?, removed_by = ?, purge_after = ?,
			  updated_at = ? WHERE id = ? AND removed_at = 0`,
			at.Unix(), actor, out.PurgeAfter.Unix(), at.Unix(), id); err != nil {
			return fmt.Errorf("account: remove: %w", err)
		}
		// What keys held on it goes now, and never comes back with a
		// recovery: an instance key restricted to it alone is revoked first,
		// since one restricted to nothing reaches every operator mailbox; a
		// carried-over key that held nothing else is revoked by the schema.
		if err := revokeKeysLimitedToTx(ctx, tx, []string{id}, at.Unix()); err != nil {
			return err
		}
		for _, step := range []struct{ what, query string }{
			{"take it from the keys restricted to it", `DELETE FROM api_key_accounts WHERE account_id = ?`},
			{"take it from the keys that held it", `DELETE FROM key_access WHERE account_id = ?`},
		} {
			if _, err := tx.ExecContext(ctx, step.query, id); err != nil {
				return fmt.Errorf("account: remove: %s: %w", step.what, err)
			}
		}
		if _, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
			Kind: store.MailboxRemoved, AccountID: id, Actor: actor, At: at.Unix(),
		}, readers, true); err != nil {
			return err
		}
		for _, userID := range readers {
			if userID != actor {
				out.Told = append(out.Told, userID)
			}
		}
		return nil
	})
	if err != nil {
		return Removal{}, err
	}
	r.committed(nil, id)
	return out, nil
}

// recoverRemoved recovers a removed mailbox, as actor, after check (nil
// checks nothing): its removal is cleared and its sync held (sync_held_at)
// until someone who may turns it on. It comes back with everything it kept —
// credentials, index, key, consent, and its readers who still qualify — and
// without what keys held on it. ErrNotFound for a mailbox nobody knows,
// ErrNotRemoved for a live one, ErrRemovalOver once its 30 days are over,
// workspace.ErrClosed in a closed team, and ErrNoReaderLeft for a team
// mailbox nobody who could read it is left to read.
func (r *Repository) recoverRemoved(ctx context.Context, id, actor string, check func(*sql.Tx) error) error {
	at := r.now().UTC().Truncate(time.Second)
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var (
			workspaceID, kind     string
			removedAt, purgeAfter int64
		)
		err := tx.QueryRowContext(ctx, `SELECT a.workspace_id, w.kind, a.removed_at, a.purge_after
			FROM accounts a JOIN workspaces w ON w.id = a.workspace_id WHERE a.id = ?`, id).
			Scan(&workspaceID, &kind, &removedAt, &purgeAfter)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("account: read the removed mailbox: %w", err)
		case removedAt == 0:
			return ErrNotRemoved
		}
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		if purgeAfter <= at.Unix() {
			return ErrRemovalOver
		}
		if err := workspace.RequireOpenTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
			  sync_held_at = ?, updated_at = ? WHERE id = ? AND removed_at <> 0`, at.Unix(), at.Unix(), id); err != nil {
			return fmt.Errorf("account: recover: %w", err)
		}
		// A team mailbox syncs for its readers, and read passes only from
		// someone who reads it: with none left, it is deleted for good and
		// linked again instead.
		if workspace.Kind(kind) == workspace.KindTeam {
			readable, err := workspace.HasReaderTx(ctx, tx, id)
			switch {
			case err != nil:
				return err
			case !readable:
				return ErrNoReaderLeft
			}
		}
		// Told it was removed, its readers need not be told any longer.
		if _, err := tx.ExecContext(ctx, `DELETE FROM notices WHERE event_seq IN
			(SELECT seq FROM mailbox_events WHERE account_id = ? AND kind = ?)`, id, string(store.MailboxRemoved)); err != nil {
			return fmt.Errorf("account: recover: withdraw the removal's notices: %w", err)
		}
		readers, err := store.MailboxReadersTx(ctx, tx, id)
		if err != nil {
			return err
		}
		_, _, err = store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
			Kind: store.MailboxRecovered, AccountID: id, Actor: actor, At: at.Unix(),
		}, readers, false)
		return err
	})
	if err != nil {
		return err
	}
	r.committed(nil, id)
	return nil
}

// purgeRemoved deletes a removed mailbox for good, as actor, after check (nil
// checks nothing), journaled: what its owner, or the operator, decides before
// its 30 days are over. ErrNotFound for a mailbox nobody knows, ErrNotRemoved
// for a live one: removing comes first.
func (r *Repository) purgeRemoved(ctx context.Context, id, actor string, check func(*sql.Tx) error) error {
	at := r.now()
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var removedAt int64
		err := tx.QueryRowContext(ctx, `SELECT removed_at FROM accounts WHERE id = ?`, id).Scan(&removedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("account: read the removed mailbox: %w", err)
		case removedAt == 0:
			return ErrNotRemoved
		}
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		_, err = purgeTx(ctx, tx, []string{id}, purge{actor: actor, at: at})
		return err
	})
	if err != nil {
		return err
	}
	r.committed(nil, id)
	return nil
}

// ExpiredRemoved lists the removed mailboxes whose 30 days are over at now:
// what the sweep deletes for good. Never one early, and whatever its
// workspace: a closed team's removed mailboxes go as every other.
func (r *Repository) ExpiredRemoved(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT id FROM accounts
		WHERE removed_at <> 0 AND purge_after <= ? ORDER BY purge_after, id`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("account: list the removed mailboxes to delete: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("account: list the removed mailboxes to delete: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list the removed mailboxes to delete: %w", err)
	}
	return ids, nil
}

// purgeExpired deletes one removed mailbox for good if its 30 days are over
// at now, journaled as the sweep's, and reports whether it did: one recovered
// meanwhile, or already gone, is left alone.
func (r *Repository) purgeExpired(ctx context.Context, id string, now time.Time) (bool, error) {
	purged := false
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var due bool
		err := tx.QueryRowContext(ctx, `SELECT removed_at <> 0 AND purge_after <= ? FROM accounts WHERE id = ?`,
			now.Unix(), id).Scan(&due)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("account: read the removed mailbox: %w", err)
		case !due:
			return nil
		}
		purged = true
		_, err = purgeTx(ctx, tx, []string{id}, purge{actor: store.ActorSweep, at: now})
		return err
	})
	if err != nil || !purged {
		return false, err
	}
	r.committed(nil, id)
	return true, nil
}

// GetRemoved reads one removed mailbox: a live one, or one nobody knows, is
// ErrNotFound. Only what asks for a removed mailbox by name reads it:
// listing, recovering and deleting the removed ones.
func (r *Repository) GetRemoved(ctx context.Context, id string) (Account, error) {
	row := r.store.Reader().QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = ? AND removed_at <> 0`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// RemovedScope is whose removed mailboxes a caller may list, built by
// internal/service and turned into SQL here.
type RemovedScope struct {
	// UserID sees the removed mailboxes of the open workspaces they are an
	// active owner of, their personal one included.
	UserID string
	// Operator sees every workspace's, to delete one for good when its
	// person asks the operator to erase it.
	Operator bool
	// Workspace narrows to one workspace's; empty is every one.
	Workspace string
}

// RemovedAccount is a removed mailbox as its owners see it.
type RemovedAccount struct {
	Account
	// IndexKept is an index it keeps: recovered, it catches up from it.
	IndexKept bool
	// Readers is how many people would read it again once recovered
	// (store.KeptReaderSQL): a team mailbox with none is not recovered.
	Readers int
}

// ListRemoved lists the removed mailboxes scope may see, the oldest removal
// first. A scope naming nobody sees nothing.
func (r *Repository) ListRemoved(ctx context.Context, scope RemovedScope) ([]RemovedAccount, error) {
	where, args := "accounts.removed_at <> 0", []any(nil)
	switch {
	case scope.Operator:
	case scope.UserID != "":
		where += ` AND EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id = m.user_id
		           WHERE m.workspace_id = accounts.workspace_id AND m.user_id = ? AND m.status = 'active'
		             AND m.role = 'owner' AND u.status = 'active' AND ` + store.OpenWorkspaceSQL("accounts.workspace_id") + `)`
		args = append(args, scope.UserID)
	default:
		return nil, nil
	}
	if scope.Workspace != "" {
		where += ` AND accounts.workspace_id = ?`
		args = append(args, scope.Workspace)
	}
	//nolint:gosec // G202: built from constants and the rule's fragments; every value is bound
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT `+accountColumns+`,
		  EXISTS (SELECT 1 FROM folders f WHERE f.account_id = accounts.id),
		  (SELECT count(*) FROM mailbox_access g WHERE g.account_id = accounts.id AND `+store.KeptReaderSQL("g")+`)
		  FROM accounts WHERE `+where+` ORDER BY removed_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("account: list the removed mailboxes: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []RemovedAccount
	for rows.Next() {
		var ra RemovedAccount
		ra.Account, err = scanAccount(removedRow{rows: rows, extra: []any{&ra.IndexKept, &ra.Readers}})
		if err != nil {
			return nil, err
		}
		out = append(out, ra)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list the removed mailboxes: %w", err)
	}
	return out, nil
}

// removedRow scans an account's columns and then the extra ones a listing
// of removed mailboxes adds.
type removedRow struct {
	rows  *sql.Rows
	extra []any
}

func (r removedRow) Scan(dest ...any) error { return r.rows.Scan(append(dest, r.extra...)...) }

// Remove removes a mailbox, as actor ("usr_…", "key:<prefix>", "cli"), after
// check, which the service re-reads who may in (nil checks nothing): kept
// for RemovedKeptFor, recoverable, or deleted for good at once when it never
// connected. Once that has committed, the registry lets go of everything it
// holds for it: a consent attempt still waiting, whose listener would still
// take a redirect and say something about it, its token source and its
// cached mailbox.
func (r *Registry) Remove(ctx context.Context, accountID, actor string, check RemoveCheck) (Removal, error) {
	removal, err := r.repo.remove(ctx, accountID, actor, check)
	if err != nil {
		return Removal{}, err
	}
	r.stopWaiting(accountID)
	r.forget(accountID)
	return removal, nil
}

// Recover recovers a removed mailbox, as actor, after check (nil checks
// nothing), and returns it: live again, its sync held until someone who may
// turns it on. See recoverRemoved for what refuses it.
func (r *Registry) Recover(ctx context.Context, accountID, actor string, check func(*sql.Tx) error) (Account, error) {
	if err := r.repo.recoverRemoved(ctx, accountID, actor, check); err != nil {
		return Account{}, err
	}
	return r.repo.Get(ctx, accountID)
}

// Purge deletes a removed mailbox for good, as actor, after check (nil checks
// nothing), journaled. A live one is ErrNotRemoved: it is removed first.
func (r *Registry) Purge(ctx context.Context, accountID, actor string, check func(*sql.Tx) error) error {
	if err := r.repo.purgeRemoved(ctx, accountID, actor, check); err != nil {
		return err
	}
	r.stopWaiting(accountID)
	r.forget(accountID)
	return nil
}

// PurgeExpired deletes for good every removed mailbox whose 30 days are over
// at now, one transaction each, journaled as the sweep's, and returns those
// it deleted; perMailbox, when not nil, is told each once its transaction has
// committed. A failure stops it, with what it deleted until then: the next
// sweep goes on.
func (r *Registry) PurgeExpired(ctx context.Context, now time.Time, perMailbox func(accountID string)) ([]string, error) {
	ids, err := r.repo.ExpiredRemoved(ctx, now)
	if err != nil {
		return nil, err
	}
	var purged []string
	for _, id := range ids {
		done, err := r.repo.purgeExpired(ctx, id, now)
		if err != nil {
			return purged, err
		}
		if !done {
			continue
		}
		r.stopWaiting(id)
		r.forget(id)
		purged = append(purged, id)
		if perMailbox != nil {
			perMailbox(id)
		}
	}
	return purged, nil
}
