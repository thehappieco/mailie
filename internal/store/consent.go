package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Consent to sync, and what taking it back deletes.
//
// The privacy policy promises that nothing about a person's messages is
// stored until they agree to it in the console, and that turning sync off
// deletes what was. The rule the engine reads is in eligibility.go; this file
// is how the answer changes, and the deletion that goes with saying no:
//
//   - a person's own consent covers the mailboxes of their personal
//     workspace;
//   - a team mailbox has its workspace's consent, which an owner or an admin
//     gives or withdraws on the team's behalf, and the operator switches an
//     operator mailbox on or off: both are recorded on the mailbox
//     (SetMailboxSync).
//
// A team mailbox whose consent migration 0011 copied from the person who
// linked it stays bound to that person until the team confirms it: their
// withdrawal, or their being disabled or deleted, stops it (StopBoundTx).

// ErrNoSuchUser is a consent read or written for a person who does not exist
// or is disabled.
var ErrNoSuchUser = errors.New("store: no such active user")

// ErrNoMailboxConsent is sync switched on or off on a mailbox's own consent
// for an account that does not exist or that is a person's: a personal
// mailbox syncs under its person's consent, which only they give.
var ErrNoMailboxConsent = errors.New("store: no such mailbox with a consent of its own")

// SyncConsent is a person's standing answer: when they agreed and to which
// revision of the policy. A zero At is no consent, never given or withdrawn.
type SyncConsent struct {
	At      int64
	Version string
}

// SyncConsentOf reads a person's consent.
func (s *Store) SyncConsentOf(ctx context.Context, userID string) (SyncConsent, error) {
	var c SyncConsent
	err := s.r.QueryRowContext(ctx,
		`SELECT sync_consent_at, sync_consent_version FROM users WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SyncConsent{}, ErrNoSuchUser
	case err != nil:
		return SyncConsent{}, fmt.Errorf("store: read sync consent: %w", err)
	}
	return c, nil
}

// GrantSyncConsent records that a person agreed to version of the policy and
// returns the mailboxes of their personal workspace, which may now start
// syncing.
//
// Agreeing again to the same version keeps the first date: it is when the
// person said yes, not when a console last asked.
func (s *Store) GrantSyncConsent(ctx context.Context, userID, version string) (SyncConsent, []string, error) {
	now := s.now().Unix()
	var (
		c     SyncConsent
		owned []string
	)
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users
			    SET sync_consent_at = CASE WHEN sync_consent_at = 0 OR sync_consent_version <> ?1
			                               THEN ?2 ELSE sync_consent_at END,
			        sync_consent_version = ?1, updated_at = ?2
			  WHERE id = ?3 AND status = 'active'
			  RETURNING sync_consent_at, sync_consent_version`, version, now, userID,
		).Scan(&c.At, &c.Version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoSuchUser
		case err != nil:
			return fmt.Errorf("store: grant sync consent: %w", err)
		}
		owned, err = ownedAccountsTx(ctx, tx, userID)
		return err
	})
	if err != nil {
		return SyncConsent{}, nil, err
	}
	return c, owned, nil
}

// WithdrawSyncConsent takes a person's consent back and deletes everything
// indexed for the mailboxes of their personal workspace, in one transaction,
// together with the team mailboxes whose consent is still bound to theirs
// (StopBoundTx), and returns every mailbox it stopped. A team mailbox under
// its workspace's own consent is not theirs to stop: it is the team's.
//
// The engine re-checks eligibility inside every transaction that writes to
// the index (RequireSyncEligibleTx), and the database has one writer, so a
// sync batch already on its way when this commits stores nothing: once the
// consent is gone, the index for these accounts stays empty until it is given
// again. Callers then compact the full-text index and scrub the WAL, outside
// the transaction, as the deletion of a person does.
func (s *Store) WithdrawSyncConsent(ctx context.Context, userID string) ([]string, error) {
	now := s.now().Unix()
	var stopped []string
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET sync_consent_at = 0, sync_consent_version = '', updated_at = ? WHERE id = ?`, now, userID)
		if err != nil {
			return fmt.Errorf("store: withdraw sync consent: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: withdraw sync consent: %w", err)
		} else if n == 0 {
			return ErrNoSuchUser
		}
		owned, err := ownedAccountsTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := ForgetIndexTx(ctx, tx, owned); err != nil {
			return err
		}
		bound, err := StopBoundTx(ctx, tx, userID, now)
		if err != nil {
			return err
		}
		stopped = append(owned, bound...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.moves.forget(stopped)
	return stopped, nil
}

// StopBoundTx stops, inside the caller's transaction, the team mailboxes
// whose consent is still the one migration 0011 copied from userID's own:
// their sync goes off and everything indexed for them is deleted, as the text
// that person agreed to promised when they turn sync off, are disabled or are
// deleted. Each is journaled as a sync turned off by that person, whose cause
// says the consent stopped with them (CauseBoundStopped), and every other
// reader is told (JournalMailboxTx). It returns them. Callers compact and
// scrub once the transaction has committed (and ForgetMoves).
func StopBoundTx(ctx context.Context, tx *sql.Tx, userID string, now int64) ([]string, error) {
	ids, err := queryIDs(ctx, tx, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '',
		  sync_consent_version = '', sync_enabled_via = '', updated_at = ?
		WHERE sync_enabled_via = 'migration' AND sync_enabled_by = ? RETURNING id`, now, userID)
	if err != nil {
		return nil, fmt.Errorf("store: stop the team mailboxes bound to a person: %w", err)
	}
	for _, id := range ids {
		if _, err := journalSyncPausedTx(ctx, tx, id, userID, CauseBoundStopped, now); err != nil {
			return nil, err
		}
	}
	if err := ForgetIndexTx(ctx, tx, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// ForgetMoves drops what the index remembers in memory of moves on these
// accounts, once a transaction that deleted their index (ForgetIndexTx,
// StopBoundTx) has committed.
func (s *Store) ForgetMoves(accountIDs []string) { s.moves.forget(accountIDs) }

// ActionsConsent is a person's standing answer to Mailie changing their
// mailboxes: when they allowed it and to which revision of the policy. A zero
// At is no consent, never given or withdrawn; WithdrawnAt, when they last
// took it back, tells the two apart (migration 0024). It is 0 while they
// consent, and 0 for a person who never answered or whose withdrawal came
// before that migration.
type ActionsConsent struct {
	At          int64
	Version     string
	WithdrawnAt int64
}

// ActionsConsentOf reads a person's consent to actions. A person who does not
// exist or is disabled is ErrNoSuchUser: nobody can have allowed anything for
// them.
func (s *Store) ActionsConsentOf(ctx context.Context, userID string) (ActionsConsent, error) {
	var c ActionsConsent
	err := s.r.QueryRowContext(ctx,
		`SELECT actions_consent_at, actions_consent_version, actions_consent_withdrawn_at FROM users
		  WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version, &c.WithdrawnAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ActionsConsent{}, ErrNoSuchUser
	case err != nil:
		return ActionsConsent{}, fmt.Errorf("store: read actions consent: %w", err)
	}
	return c, nil
}

// GrantActionsConsent records that a person allowed actions under version of
// the policy. Allowing again under the same version keeps the first date, as
// the sync consent does; a withdrawal before it is cleared in the same
// UPDATE.
func (s *Store) GrantActionsConsent(ctx context.Context, userID, version string) (ActionsConsent, error) {
	now := s.now().Unix()
	var c ActionsConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users
			    SET actions_consent_at = CASE WHEN actions_consent_at = 0 OR actions_consent_version <> ?1
			                                  THEN ?2 ELSE actions_consent_at END,
			        actions_consent_version = ?1, actions_consent_withdrawn_at = 0, updated_at = ?2
			  WHERE id = ?3 AND status = 'active'
			  RETURNING actions_consent_at, actions_consent_version, actions_consent_withdrawn_at`, version, now, userID,
		).Scan(&c.At, &c.Version, &c.WithdrawnAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoSuchUser
		case err != nil:
			return fmt.Errorf("store: grant actions consent: %w", err)
		}
		return nil
	})
	if err != nil {
		return ActionsConsent{}, err
	}
	return c, nil
}

// WithdrawActionsConsent takes a person's consent to actions back. Nothing
// else is deleted: acting keeps nothing beyond what the index holds. The
// service checks the consent when an action is accepted and again before
// each command that changes the mailbox, so once this has committed no
// further change is sent; a command already on the wire finishes.
//
// The withdrawal's moment is written in the same UPDATE (WithdrawnAt), so a
// console can tell a person who said no from one never asked.
func (s *Store) WithdrawActionsConsent(ctx context.Context, userID string) (ActionsConsent, error) {
	now := s.now().Unix()
	var c ActionsConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users SET actions_consent_at = 0, actions_consent_version = '', actions_consent_withdrawn_at = ?1,
			        updated_at = ?1
			  WHERE id = ?2
			  RETURNING actions_consent_at, actions_consent_version, actions_consent_withdrawn_at`, now, userID,
		).Scan(&c.At, &c.Version, &c.WithdrawnAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoSuchUser
		case err != nil:
			return fmt.Errorf("store: withdraw actions consent: %w", err)
		}
		return nil
	})
	if err != nil {
		return ActionsConsent{}, err
	}
	return c, nil
}

// SendConsent is a person's standing answer to Mailie sending mail from
// their mailboxes when they ask: when they allowed it and to which revision of
// the policy. A zero At is no consent, never given or withdrawn; WithdrawnAt,
// when they last took it back, tells the two apart, as ActionsConsent's does.
type SendConsent struct {
	At          int64
	Version     string
	WithdrawnAt int64
}

// SendConsentOf reads a person's consent to sending. A person who does not
// exist or is disabled is ErrNoSuchUser: nobody can have allowed anything for
// them.
func (s *Store) SendConsentOf(ctx context.Context, userID string) (SendConsent, error) {
	var c SendConsent
	err := s.r.QueryRowContext(ctx,
		`SELECT send_consent_at, send_consent_version, send_consent_withdrawn_at FROM users
		  WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version, &c.WithdrawnAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SendConsent{}, ErrNoSuchUser
	case err != nil:
		return SendConsent{}, fmt.Errorf("store: read send consent: %w", err)
	}
	return c, nil
}

// GrantSendConsent records that a person allowed sending under version of the
// policy. Allowing again under the same version keeps the first date, as the
// other consents do; a withdrawal before it is cleared in the same UPDATE.
func (s *Store) GrantSendConsent(ctx context.Context, userID, version string) (SendConsent, error) {
	now := s.now().Unix()
	var c SendConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users
			    SET send_consent_at = CASE WHEN send_consent_at = 0 OR send_consent_version <> ?1
			                               THEN ?2 ELSE send_consent_at END,
			        send_consent_version = ?1, send_consent_withdrawn_at = 0, updated_at = ?2
			  WHERE id = ?3 AND status = 'active'
			  RETURNING send_consent_at, send_consent_version, send_consent_withdrawn_at`, version, now, userID,
		).Scan(&c.At, &c.Version, &c.WithdrawnAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoSuchUser
		case err != nil:
			return fmt.Errorf("store: grant send consent: %w", err)
		}
		return nil
	})
	if err != nil {
		return SendConsent{}, err
	}
	return c, nil
}

// WithdrawSendConsent takes a person's consent to sending back. Nothing else
// is deleted: the send records are about the mailbox, and go on their own
// schedule. The service checks the consent when a send is accepted and again
// right before it connects to the submission server, so once this has
// committed no send of theirs connects; one already talking to the server
// finishes.
//
// The withdrawal's moment is written in the same UPDATE (WithdrawnAt), as
// WithdrawActionsConsent does.
func (s *Store) WithdrawSendConsent(ctx context.Context, userID string) (SendConsent, error) {
	now := s.now().Unix()
	var c SendConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users SET send_consent_at = 0, send_consent_version = '', send_consent_withdrawn_at = ?1,
			        updated_at = ?1
			  WHERE id = ?2
			  RETURNING send_consent_at, send_consent_version, send_consent_withdrawn_at`, now, userID,
		).Scan(&c.At, &c.Version, &c.WithdrawnAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoSuchUser
		case err != nil:
			return fmt.Errorf("store: withdraw send consent: %w", err)
		}
		return nil
	})
	if err != nil {
		return SendConsent{}, err
	}
	return c, nil
}

// MailboxConsent is a mailbox's own consent to sync: a team mailbox's, given
// by an owner or an admin on its workspace's behalf, or an operator
// mailbox's switch. A zero At is off.
type MailboxConsent struct {
	At int64
	// By is who gave it: "usr_…", "key:<prefix>" or "cli"; empty once that
	// person is deleted.
	By string
	// Version is the sync text's revision it was given to; empty for an
	// operator mailbox.
	Version string
	// Migrated is a consent migration 0011 copied from its linker's own (By),
	// still bound to that person until an owner or an admin confirms it.
	Migrated bool
}

// MailboxSyncSwitch is what switching a mailbox's own sync did.
type MailboxSyncSwitch struct {
	// Changed is anything changed: a consent recorded or withdrawn, or a
	// recovery's hold lifted.
	Changed bool
	// Told are the readers a team mailbox's sync turned off told, with a
	// notice: everyone who read it then but whoever turned it off.
	Told []string
}

// SetMailboxSync switches sync on or off for a mailbox that names no person —
// a team's, under its workspace's consent, or the operator's — and records
// who switched it on, when and to which revision. check runs first in the
// transaction (nil checks nothing): where the caller's authority is re-read.
//
// Switching on a mailbox that is already on records the new consent when its
// revision changes, it is a migrated one being confirmed, which detaches it
// from the person it was bound to, or a recovery holds its sync, which it
// lifts; otherwise nothing changes. A removed mailbox is never switched on
// (ErrNoMailboxConsent). Switching off deletes what was indexed for it, in
// the same transaction, as withdrawing consent does for a person's mailboxes,
// a migrated consent's kept index included, and lifts a hold, which an off
// mailbox does not need; a team mailbox's that was on, or bound to a person,
// is journaled, and every reader but whoever turned it off is told
// (JournalMailboxTx). It reports what changed.
func (s *Store) SetMailboxSync(ctx context.Context, accountID string, on bool, by, version string, check func(*sql.Tx) error) (MailboxSyncSwitch, error) {
	now := s.now().Unix()
	var out MailboxSyncSwitch
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var (
			c                   MailboxConsent
			held, removed, team bool
		)
		err := tx.QueryRowContext(ctx,
			`SELECT a.sync_enabled_at, a.sync_consent_version, a.sync_enabled_via = 'migration', a.sync_held_at <> 0,
			        a.removed_at <> 0, EXISTS (SELECT 1 FROM workspaces w WHERE w.id = a.workspace_id AND w.kind = 'team')
			   FROM accounts a WHERE a.id = ? AND a.owner_user_id IS NULL`, accountID,
		).Scan(&c.At, &c.Version, &c.Migrated, &held, &removed, &team)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoMailboxConsent
		case err != nil:
			return fmt.Errorf("store: read the mailbox's sync: %w", err)
		}
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		if on {
			switch {
			case removed:
				return ErrNoMailboxConsent
			case c.At != 0 && c.Version == version && !c.Migrated && !held:
				return nil
			}
			out.Changed = true
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET sync_enabled_at = ?, sync_enabled_by = ?, sync_consent_version = ?,
				   sync_enabled_via = '', sync_held_at = 0, updated_at = ? WHERE id = ?`,
				now, by, version, now, accountID); err != nil {
				return fmt.Errorf("store: switch sync on: %w", err)
			}
			return nil
		}
		wasOn := c.At != 0 || c.Migrated
		out.Changed = wasOn || held
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '', sync_consent_version = '',
			   sync_enabled_via = '', sync_held_at = 0, updated_at = ? WHERE id = ?`, now, accountID); err != nil {
			return fmt.Errorf("store: switch sync off: %w", err)
		}
		if err := ForgetIndexTx(ctx, tx, []string{accountID}); err != nil {
			return err
		}
		if !wasOn || !team {
			return nil
		}
		out.Told, err = journalSyncPausedTx(ctx, tx, accountID, by, "", now)
		return err
	})
	if err == nil && !on {
		s.moves.forget([]string{accountID})
	}
	return out, err
}

// journalSyncPausedTx journals, inside the caller's transaction, a mailbox's
// sync turned off by actor, for cause ("" or CauseBoundStopped), with who
// read it then, and tells each of them but the actor; it returns who it
// told.
func journalSyncPausedTx(ctx context.Context, tx *sql.Tx, accountID, actor, cause string, now int64) ([]string, error) {
	readers, err := MailboxReadersTx(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	if _, _, err := JournalMailboxTx(ctx, tx, MailboxEvent{
		Kind: MailboxSyncPaused, AccountID: accountID, Actor: actor, At: now, Cause: cause,
	}, readers, true); err != nil {
		return nil, err
	}
	told := make([]string, 0, len(readers))
	for _, id := range readers {
		if id != actor {
			told = append(told, id)
		}
	}
	return told, nil
}

// LiftSyncHold lifts the hold a recovery put on a live mailbox's sync
// (sync_held_at), which then syncs again under its consent as it stands: a
// personal mailbox's person's, which only they lift it for. check runs first
// in the transaction (nil checks nothing). It reports whether a hold was
// lifted; a mailbox nobody knows, or a removed one, is ErrNoSuchMailbox.
func (s *Store) LiftSyncHold(ctx context.Context, accountID string, check func(*sql.Tx) error) (bool, error) {
	now := s.now().Unix()
	lifted := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var held bool
		err := tx.QueryRowContext(ctx, `SELECT sync_held_at <> 0 FROM accounts WHERE id = ? AND removed_at = 0`,
			accountID).Scan(&held)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrNoSuchMailbox, accountID)
		case err != nil:
			return fmt.Errorf("store: read the mailbox's sync: %w", err)
		}
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		if !held {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sync_held_at = 0, updated_at = ? WHERE id = ?`,
			now, accountID); err != nil {
			return fmt.Errorf("store: lift the sync's hold: %w", err)
		}
		lifted = true
		return nil
	})
	return lifted, err
}

// SyncPermitted reports, for each account named, whether sync is allowed for
// it: a personal mailbox's person is active and consented; a team mailbox
// has its workspace's consent and a reader; an operator mailbox was switched
// on. A removed mailbox is not, and neither is one whose sync a recovery
// holds until someone turns it on. Whether the account is also active — the rest of the engine's rule —
// is the caller's to read from the account itself. An account that does not
// exist is absent from the map.
func (s *Store) SyncPermitted(ctx context.Context, accountIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("store: encode ids: %w", err)
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT a.id, `+syncPermitted+`
		   FROM accounts a WHERE a.id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return nil, fmt.Errorf("store: read sync permission: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id        string
			permitted bool
		)
		if err := rows.Scan(&id, &permitted); err != nil {
			return nil, fmt.Errorf("store: read sync permission: %w", err)
		}
		out[id] = permitted
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read sync permission: %w", err)
	}
	return out, nil
}

// ForgetIndexTx deletes everything sync stored for the accounts, inside the
// caller's transaction, and leaves the accounts themselves: their settings,
// credentials and state.
//
// Folders go with their messages, and the account's sync bookkeeping (tier,
// last pass, last failure) is reset. Their names are the person's labels, kept
// only because sync listed them; the engine rediscovers them from scratch if
// sync is turned on again, and until then the folder listing asks the server.
// Messages cascade to their parts and bodies, and the full-text index loses
// them through its trigger. The events of these accounts go too — they quote
// subjects and senders — with any webhook delivery of them.
func ForgetIndexTx(ctx context.Context, tx *sql.Tx, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("store: encode ids: %w", err)
	}
	const in = `(SELECT value FROM json_each(?))`
	for _, step := range []struct{ what, query string }{
		{"delete deliveries of their events", `DELETE FROM webhook_deliveries
		   WHERE event_seq IN (SELECT seq FROM events WHERE account_id IN ` + in + `)`},
		{"delete their events", `DELETE FROM events WHERE account_id IN ` + in},
		{"delete their messages", `DELETE FROM messages WHERE account_id IN ` + in},
		{"delete their folders", `DELETE FROM folders WHERE account_id IN ` + in},
		// And where sync stood: an account that is off shows nothing of the
		// sync it no longer has, and a later consent starts from scratch.
		{"reset their sync bookkeeping", `UPDATE accounts SET sync_tier_resolved = '', last_ok_at = 0, last_error = '',
		   consecutive_failures = 0, next_retry_at = 0, last_idle_event_at = 0 WHERE id IN ` + in},
	} {
		if _, err := tx.ExecContext(ctx, step.query, string(list)); err != nil {
			return fmt.Errorf("store: forget the index: %s: %w", step.what, err)
		}
	}
	return nil
}

// CompactFullText rewrites the full-text index without what was deleted from
// it.
//
// FTS5 records a deletion as a marker and keeps the deleted terms in its
// older segments until they are merged, so a subject deleted from messages
// would still sit in the file. Merging every segment into one drops them, and
// secure_delete zeroes the pages that frees. It rewrites the whole index, so
// it runs after a deletion that has to leave nothing behind, outside that
// deletion's transaction, and before Scrub.
func (s *Store) CompactFullText(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES ('optimize')`); err != nil {
			return fmt.Errorf("store: compact the full-text index: %w", err)
		}
		return nil
	})
}

// ownedAccountsTx lists the mailboxes of a person's personal workspace: the
// ones that name them (owner_user_id), which their own consent covers.
func ownedAccountsTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	ids, err := queryIDs(ctx, tx, `SELECT id FROM accounts WHERE owner_user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list owned accounts: %w", err)
	}
	return ids, nil
}

// queryIDs runs a statement returning one text column and reads every value,
// closing its cursor before it returns.
func queryIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
