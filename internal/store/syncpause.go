package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Pausing one mailbox's sync (migration 0025; docs/workspaces.md, "Pausing a
// mailbox's sync"): the engine syncs nothing of it (SyncEligible, the
// engine's rule) while what it indexed stays, read, searched and followed by
// actions (IndexFollows, the index rule), with no time limit, until it is
// resumed or its consent ends. A recovery pauses a mailbox too
// (account.recoverRemoved). A pause is not a consent: turning sync off,
// which deletes the index, stays a separate act, and ends a pause; a team
// mailbox's resume records the team's agreement to the sync text again.

// Errors of a pause and a resume.
var (
	// ErrPauseHeld is a pause asked of a mailbox the operator's turn-off
	// holds until its person turns it on: its index is gone, and there is
	// nothing to pause.
	ErrPauseHeld = errors.New("store: the mailbox's sync is held; there is nothing to pause")
	// ErrPauseSyncOff is a pause asked of a mailbox whose consent to sync does
	// not stand.
	ErrPauseSyncOff = errors.New("store: sync is off for the mailbox; there is nothing to pause")
	// ErrResumeAgreement is a resume that does not match its mailbox: a team
	// mailbox's records the team's agreement to a sync text, which it must
	// name, and no other mailbox's names one.
	ErrResumeAgreement = errors.New("store: a team mailbox's resume names the sync text it agrees to, and no other does")
)

// PauseSync pauses a live mailbox's sync, as actor ("usr_…", or the
// operator's "key:<prefix>" or "cli"), after check (nil checks nothing), run
// first in the transaction: the engine syncs nothing of it (SyncEligible)
// while its index stays, read, searched and followed by actions
// (IndexFollows), until it is resumed or its consent ends. Only a mailbox
// whose consent stands — its person's for a personal mailbox, its own
// (sync_enabled_at, a migrated one included) for a team's or an operator
// mailbox — and whose sync is not held is paused (ErrPauseSyncOff,
// ErrPauseHeld); one already paused is left as it is (Changed false). A team
// mailbox's pause is journaled (MailboxPaused) with who read it then, and
// each of them but the actor is told; a personal mailbox's, whose only reader
// is its person, and an operator mailbox's are not journaled, as their
// turn-offs are not. A mailbox nobody knows, or a removed one, is
// ErrNoSuchMailbox. A recovery pauses too, without this
// (account.recoverRemoved): no journal entry of its own, no notice.
func (s *Store) PauseSync(ctx context.Context, accountID, actor string, check func(*sql.Tx) error) (MailboxSyncSwitch, error) {
	now := s.now().Unix()
	var out MailboxSyncSwitch
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var team, removed, held, paused, stands bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = a.workspace_id AND w.kind = 'team'),
			       a.removed_at <> 0, a.sync_held_at <> 0, a.sync_paused_at <> 0, `+ConsentStandsSQL("a")+`
			  FROM accounts a WHERE a.id = ?`, accountID).Scan(&team, &removed, &held, &paused, &stands)
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
		switch {
		case removed:
			return fmt.Errorf("%w: %s", ErrNoSuchMailbox, accountID)
		case paused:
			return nil
		case held:
			return ErrPauseHeld
		case !stands:
			return ErrPauseSyncOff
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sync_paused_at = ?, sync_paused_by = ?, updated_at = ?
			WHERE id = ?`, now, actor, now, accountID); err != nil {
			return fmt.Errorf("store: pause the mailbox's sync: %w", err)
		}
		out.Changed = true
		if !team {
			return nil
		}
		out.Told, err = journalTellingTx(ctx, tx, MailboxEvent{
			Kind: MailboxPaused, AccountID: accountID, Actor: actor, At: now,
		}, true)
		return err
	})
	if err != nil {
		return MailboxSyncSwitch{}, err
	}
	return out, nil
}

// ResumeSync ends a mailbox's pause, a person's or a recovery's, as actor,
// after check (nil checks nothing), run first in the transaction: the engine
// syncs it again under its consent as it stands, catching up from the index
// it kept. A team mailbox's resume records the team's agreement to version,
// the current sync text, as switching it on does (agreeSyncTx:
// sync_enabled_at, _by and sync_consent_version written, a consent migration
// 0011 bound to its linker confirmed) — the caller checks that version is
// current; a personal or an operator mailbox's records nothing and names
// none (ErrResumeAgreement otherwise). The pause's notices are withdrawn
// (EndPauseTx); a team mailbox's resume is journaled (MailboxResumed) with
// who read it then, telling nobody. One that is not paused is left as it is
// (Changed false), its agreement too. A mailbox nobody knows, or a removed
// one, is ErrNoSuchMailbox.
func (s *Store) ResumeSync(ctx context.Context, accountID, actor, version string, check func(*sql.Tx) error) (MailboxSyncSwitch, error) {
	now := s.now().Unix()
	var out MailboxSyncSwitch
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var team, removed, paused bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = a.workspace_id AND w.kind = 'team'),
			       a.removed_at <> 0, a.sync_paused_at <> 0
			  FROM accounts a WHERE a.id = ?`, accountID).Scan(&team, &removed, &paused)
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
		switch {
		case removed:
			return fmt.Errorf("%w: %s", ErrNoSuchMailbox, accountID)
		case !paused:
			return nil
		case team != (version != ""):
			return ErrResumeAgreement
		}
		if team {
			// On every resume, even of a mailbox whose agreement stood at
			// this revision: the resumer is the one who agreed, now.
			if err := agreeSyncTx(ctx, tx, accountID, actor, version, now); err != nil {
				return err
			}
		}
		if err := EndPauseTx(ctx, tx, []string{accountID}, now); err != nil {
			return err
		}
		out.Changed = true
		if !team {
			return nil
		}
		_, err = journalTellingTx(ctx, tx, MailboxEvent{
			Kind: MailboxResumed, AccountID: accountID, Actor: actor, At: now,
		}, false)
		return err
	})
	if err != nil {
		return MailboxSyncSwitch{}, err
	}
	return out, nil
}

// EndPauseTx ends, inside the caller's transaction, the pause of each of
// these mailboxes that has one, and withdraws every notice of their pauses:
// what a resume does, and what everything that deletes a mailbox's index,
// withdraws or stops its consent, removes it, holds it or deletes it for
// good does first (docs/workspaces.md, "Pausing a mailbox's sync"). It
// journals nothing: the act that ends the pause journals itself.
func EndPauseTx(ctx context.Context, tx *sql.Tx, accountIDs []string, now int64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("store: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '', updated_at = ?
		WHERE id IN (SELECT value FROM json_each(?)) AND sync_paused_at <> 0`, now, string(list)); err != nil {
		return fmt.Errorf("store: end the mailboxes' pause: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notices WHERE event_seq IN
		(SELECT seq FROM mailbox_events WHERE kind = ? AND account_id IN (SELECT value FROM json_each(?)))`,
		string(MailboxPaused), string(list)); err != nil {
		return fmt.Errorf("store: withdraw the pauses' notices: %w", err)
	}
	return nil
}
