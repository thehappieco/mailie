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
// is how a person, or the operator for a mailbox nobody owns, changes the
// answer, and the deletion that goes with saying no.

// ErrNoSuchUser is a consent read or written for a person who does not exist
// or is disabled.
var ErrNoSuchUser = errors.New("store: no such active user")

// ErrNotInstanceAccount is sync switched on or off by the operator for an
// account that does not exist or that a person owns: only its owner decides
// for that one, by consenting.
var ErrNotInstanceAccount = errors.New("store: no such account without an owner")

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

// SyncConsentTx reads, inside the caller's transaction, a person's consent to
// sync and the revision it was given to; a person who is not active has none.
// A consent is the half of the eligibility rule a mailbox they link syncs
// under (syncPermitted). Taking a link over and linking into a team ask it
// where the link is made or changes hands, so a team mailbox never comes to
// sync under a consent whose text did not cover it.
func SyncConsentTx(ctx context.Context, tx *sql.Tx, userID string) (SyncConsent, error) {
	var c SyncConsent
	err := tx.QueryRowContext(ctx,
		`SELECT sync_consent_at, sync_consent_version FROM users WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SyncConsent{}, nil
	case err != nil:
		return SyncConsent{}, fmt.Errorf("store: read sync consent: %w", err)
	}
	return c, nil
}

// GrantSyncConsent records that a person agreed to version of the policy and
// returns the accounts they own, which may now start syncing.
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
// indexed for every account they own, in one transaction, and returns those
// accounts.
//
// The engine re-checks eligibility inside every transaction that writes to
// the index (RequireSyncEligibleTx), and the database has one writer, so a
// sync batch already on its way when this commits stores nothing: once the
// consent is gone, the index for these accounts stays empty until it is given
// again. Callers then compact the full-text index and scrub the WAL, outside
// the transaction, as the deletion of a person does.
func (s *Store) WithdrawSyncConsent(ctx context.Context, userID string) ([]string, error) {
	now := s.now().Unix()
	var owned []string
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
		if owned, err = ownedAccountsTx(ctx, tx, userID); err != nil {
			return err
		}
		return ForgetIndexTx(ctx, tx, owned)
	})
	if err != nil {
		return nil, err
	}
	s.moves.forget(owned)
	return owned, nil
}

// ActionsConsent is a person's standing answer to Mailie changing their
// mailboxes: when they allowed it and to which revision of the policy. A zero
// At is no consent, never given or withdrawn.
type ActionsConsent struct {
	At      int64
	Version string
}

// ActionsConsentOf reads a person's consent to actions. A person who does not
// exist or is disabled is ErrNoSuchUser: nobody can have allowed anything for
// them.
func (s *Store) ActionsConsentOf(ctx context.Context, userID string) (ActionsConsent, error) {
	var c ActionsConsent
	err := s.r.QueryRowContext(ctx,
		`SELECT actions_consent_at, actions_consent_version FROM users WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version)
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
// the sync consent does.
func (s *Store) GrantActionsConsent(ctx context.Context, userID, version string) (ActionsConsent, error) {
	now := s.now().Unix()
	var c ActionsConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users
			    SET actions_consent_at = CASE WHEN actions_consent_at = 0 OR actions_consent_version <> ?1
			                                  THEN ?2 ELSE actions_consent_at END,
			        actions_consent_version = ?1, updated_at = ?2
			  WHERE id = ?3 AND status = 'active'
			  RETURNING actions_consent_at, actions_consent_version`, version, now, userID,
		).Scan(&c.At, &c.Version)
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
func (s *Store) WithdrawActionsConsent(ctx context.Context, userID string) error {
	now := s.now().Unix()
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET actions_consent_at = 0, actions_consent_version = '', updated_at = ? WHERE id = ?`, now, userID)
		if err != nil {
			return fmt.Errorf("store: withdraw actions consent: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: withdraw actions consent: %w", err)
		} else if n == 0 {
			return ErrNoSuchUser
		}
		return nil
	})
}

// SendConsent is a person's standing answer to Mailie sending mail from
// their mailboxes when they ask: when they allowed it and to which revision of
// the policy. A zero At is no consent, never given or withdrawn.
type SendConsent struct {
	At      int64
	Version string
}

// SendConsentOf reads a person's consent to sending. A person who does not
// exist or is disabled is ErrNoSuchUser: nobody can have allowed anything for
// them.
func (s *Store) SendConsentOf(ctx context.Context, userID string) (SendConsent, error) {
	var c SendConsent
	err := s.r.QueryRowContext(ctx,
		`SELECT send_consent_at, send_consent_version FROM users WHERE id = ? AND status = 'active'`, userID,
	).Scan(&c.At, &c.Version)
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
// other consents do.
func (s *Store) GrantSendConsent(ctx context.Context, userID, version string) (SendConsent, error) {
	now := s.now().Unix()
	var c SendConsent
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`UPDATE users
			    SET send_consent_at = CASE WHEN send_consent_at = 0 OR send_consent_version <> ?1
			                               THEN ?2 ELSE send_consent_at END,
			        send_consent_version = ?1, updated_at = ?2
			  WHERE id = ?3 AND status = 'active'
			  RETURNING send_consent_at, send_consent_version`, version, now, userID,
		).Scan(&c.At, &c.Version)
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
func (s *Store) WithdrawSendConsent(ctx context.Context, userID string) error {
	now := s.now().Unix()
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET send_consent_at = 0, send_consent_version = '', updated_at = ? WHERE id = ?`, now, userID)
		if err != nil {
			return fmt.Errorf("store: withdraw send consent: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: withdraw send consent: %w", err)
		} else if n == 0 {
			return ErrNoSuchUser
		}
		return nil
	})
}

// SetInstanceSync switches sync on or off for an account nobody owns, and
// records who switched it on and when. Switching it off deletes what was
// indexed for it, in the same transaction, as withdrawing consent does for a
// person's mailboxes. It reports whether anything changed.
func (s *Store) SetInstanceSync(ctx context.Context, accountID string, on bool, by string) (bool, error) {
	now := s.now().Unix()
	changed := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var enabledAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT sync_enabled_at FROM accounts WHERE id = ? AND owner_user_id IS NULL`, accountID,
		).Scan(&enabledAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotInstanceAccount
		case err != nil:
			return fmt.Errorf("store: read instance sync: %w", err)
		}
		if on == (enabledAt != 0) {
			return nil
		}
		changed = true
		if on {
			_, err = tx.ExecContext(ctx,
				`UPDATE accounts SET sync_enabled_at = ?, sync_enabled_by = ?, updated_at = ? WHERE id = ?`,
				now, by, now, accountID)
			if err != nil {
				return fmt.Errorf("store: switch sync on: %w", err)
			}
			return nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '', updated_at = ? WHERE id = ?`, now, accountID)
		if err != nil {
			return fmt.Errorf("store: switch sync off: %w", err)
		}
		return ForgetIndexTx(ctx, tx, []string{accountID})
	})
	if err == nil && changed && !on {
		s.moves.forget([]string{accountID})
	}
	return changed, err
}

// SyncPermitted reports, for each account named, whether sync is allowed for
// it: its owner is active and consented, or, when nobody owns it, the
// operator switched it on. Whether the account is also active — the rest of
// the engine's rule — is the caller's to read from the account itself. An
// account that does not exist is absent from the map.
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

func ownedAccountsTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE owner_user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list owned accounts: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list owned accounts: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list owned accounts: %w", err)
	}
	return ids, nil
}
