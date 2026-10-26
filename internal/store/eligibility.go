package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotEligible is an account the engine may not store anything for: not
// active, or nobody has agreed to its mail being indexed, or, for the
// engine, its sync is paused.
var ErrNotEligible = errors.New("store: the account is not eligible to sync")

// syncEligible is the one rule for whether an account's mail may be indexed,
// as a WHERE fragment over accounts aliased a.
//
// An account is eligible when it is active and its consent stands:
//
//   - a personal mailbox (owner_user_id names its person): that person is
//     active and has consented to sync (the privacy policy promises nothing
//     is stored before they do);
//   - an operator mailbox (no person, the operator workspace): the operator
//     switched sync on for it;
//   - a team mailbox (no person, a team): an owner or an admin of the team
//     gave the workspace's consent, and someone reads it (ReaderSQL: an
//     active member, active on the instance, holding read, and on a mailbox
//     that has a key a grant at its current epoch). A mailbox nobody reads
//     indexes nothing more until someone does, or it is removed. A consent
//     migration 0011 copied from a person's own, still bound to them,
//     stands only while they are active: disabling or deleting them stops
//     it (StopBoundTx), and while an extension has them suspended it
//     indexes nothing more and keeps what it indexed, as their personal
//     mailboxes do.
//
// A personal mailbox has no reader term: its person may lose their grant (a
// reset) and write it a new key (docs/key-scheme.md section 12.12), and
// nothing it stores is sealed yet, so it keeps syncing meanwhile.
//
// A closed team's mailbox syncs nothing, whatever its consent and readers
// (OpenWorkspaceSQL): it keeps its index, its consent and its readers, and
// catches up once the team reopens.
//
// Nor does a removed mailbox (removed_at, migration 0020), kept with its
// index for a recovery, nor one the operator turned off (sync_held_at, a
// personal mailbox, held until its person turns it on); a recovered one is
// paused instead (sync_paused_at), which the engine's rule refuses and the
// index rule does not. Both a removal and a hold are in syncPermitted, so
// neither reads as switched on anywhere.
//
// A pause (sync_paused_at, migration 0025) is not part of this rule: the
// engine's rule adds it (engineRule), and the index stays writable for
// actions (indexRule).
//
// sync_enabled_at never stands in for a person's consent: nobody turns sync
// on for somebody else's personal mailbox, and a withdrawal stops it whatever
// that column says.
//
// Where the edition withholds teams (Options.TeamsWithheld), no team's
// mailbox is eligible either (notATeams): Store.indexRule adds it.
var syncEligible = `a.state = 'active' AND ` + OpenWorkspaceSQL("a.workspace_id") + ` AND ` + syncPermitted

// notATeams is that the mailbox, aliased a, is not a team's: what an
// edition that withholds teams adds to the rule, so that nothing of a team
// is used there, its mail included, and the index it has keeps until an
// edition that offers teams syncs it again.
var notATeams = `NOT EXISTS (SELECT 1 FROM workspaces tw WHERE tw.id = a.workspace_id AND tw.kind = 'team')`

// notPaused is that nobody paused the mailbox's sync, aliased a
// (sync_paused_at, migration 0025).
var notPaused = `a.sync_paused_at = 0`

// indexRule is the rule the index is written under for what someone asks
// now (an action's recording): syncEligible, and, where the edition
// withholds teams, not a team's mailbox. A pause does not change it.
func (s *Store) indexRule() string {
	if s.teamsWithheld {
		return syncEligible + ` AND ` + notATeams
	}
	return syncEligible
}

// engineRule is the rule the sync engine holds accounts to: the index rule,
// and not paused.
func (s *Store) engineRule() string { return s.indexRule() + ` AND ` + notPaused }

// ConsentStandsSQL is that the consent a mailbox, aliased a, syncs under
// stands: its person's for a personal mailbox (users.sync_consent_at,
// whatever their status), its own for a team's or an operator mailbox
// (sync_enabled_at, a consent migration 0011 bound to its linker included).
// A pause starts only where it stands (PauseSync, account.recoverRemoved),
// and every end of it ends the pause first, so a paused mailbox's consent
// always stands.
func ConsentStandsSQL(a string) string {
	return `CASE WHEN ` + a + `.owner_user_id IS NOT NULL THEN EXISTS (SELECT 1 FROM users u WHERE u.id = ` + a +
		`.owner_user_id AND u.sync_consent_at <> 0) ELSE ` + a + `.sync_enabled_at <> 0 END`
}

// syncPermitted is the consent half of the rule, as a boolean expression over
// accounts aliased a: a live mailbox whose sync nothing holds, under its
// consent. SyncPermitted reports it on its own; syncEligible adds that the
// account is active.
var syncPermitted = `a.removed_at = 0 AND a.sync_held_at = 0 AND CASE
	WHEN a.owner_user_id IS NOT NULL THEN EXISTS (SELECT 1 FROM users u WHERE u.id = a.owner_user_id
	                                                AND u.status = 'active' AND u.sync_consent_at <> 0)
	WHEN a.workspace_id = 'wsp_operator' THEN a.sync_enabled_at <> 0
	ELSE a.sync_enabled_at <> 0 AND ` + hasReader + ` AND NOT ` + boundToSomeoneOff + ` END`

// boundToSomeoneOff is a mailbox, aliased a, whose consent to sync is still
// the one migration 0011 copied from a person's own while that person is not
// active: suspended, since disabling or deleting them stops it.
var boundToSomeoneOff = `(a.sync_enabled_via = 'migration' AND EXISTS (SELECT 1 FROM users bound_u
	WHERE bound_u.id = a.sync_enabled_by AND bound_u.status <> 'active'))`

// Why a mailbox does not sync (PauseSQL), the first that holds, in this
// order.
const (
	// PauseRemoved is a removed mailbox, kept for a recovery.
	PauseRemoved = "removed"
	// PauseTeamClosed is a closed team's mailbox.
	PauseTeamClosed = "team_closed"
	// PauseWithheld is a team's mailbox where the edition withholds teams.
	PauseWithheld = "withheld"
	// PausePendingAuth is a mailbox waiting for its first consent.
	PausePendingAuth = "pending_auth"
	// PauseNeedsReauth is a mailbox whose grant died.
	PauseNeedsReauth = "needs_reauth"
	// PauseError is a mailbox in error, or disabled.
	PauseError = "error"
	// PauseHeld is a personal mailbox whose sync was turned off and is held
	// until its person turns it on: the operator's turn-off (StopSync), or a
	// hold migration 0025 could not attribute to a recovery.
	PauseHeld = "held"
	// PausePersonOff is a personal mailbox whose person is not active, or a
	// team mailbox whose consent migration 0011 bound to a person who is
	// not.
	PausePersonOff = "person_off"
	// PauseConsentOff is a mailbox whose consent to sync was never given or
	// was withdrawn: its person's, the operator's or its team's.
	PauseConsentOff = "consent_off"
	// PauseNoReader is a team mailbox nobody reads.
	PauseNoReader = "no_reader"
	// PausePaused is a mailbox whose sync is paused, its index kept: by
	// someone who may, or by its recovery.
	PausePaused = "paused"
)

// pausedOrNone is the last cause of every branch of PauseSQL: a pause, or
// nothing.
const pausedOrNone = `CASE WHEN a.sync_paused_at <> 0 THEN '` + PausePaused + `' ELSE '' END`

// PauseSQL is why a mailbox, aliased a, does not sync, as an expression: empty
// exactly when it may (the engine's rule, engineRule: syncEligible, with the
// edition's withholding of teams when withheld, and not paused), and
// otherwise the first of these that holds, in one order, from the same
// fragments: removed (PauseRemoved), a closed team (OpenWorkspaceSQL), a
// team where teams are withheld, the account's state (pending_auth,
// needs_reauth, error for error and disabled), its sync held by the
// operator's turn-off of a personal mailbox, and then its consent — a
// personal mailbox's person not active or not consenting, an operator
// mailbox not switched on, a team mailbox not switched on, bound to a
// person who is not active (boundToSomeoneOff) or with nobody who reads it
// (hasReader) — and last a pause (sync_paused_at), a person's or a
// recovery's: empty exactly when the engine's rule holds. What an extension
// reports of a mailbox it does not sync is this.
func PauseSQL(withheld bool) string {
	teams := ``
	if withheld {
		teams = `WHEN NOT ` + notATeams + ` THEN '` + PauseWithheld + `'`
	}
	return `(CASE
	WHEN a.removed_at <> 0 THEN '` + PauseRemoved + `'
	WHEN NOT ` + OpenWorkspaceSQL("a.workspace_id") + ` THEN '` + PauseTeamClosed + `'
	` + teams + `
	WHEN a.state = 'pending_auth' THEN '` + PausePendingAuth + `'
	WHEN a.state = 'needs_reauth' THEN '` + PauseNeedsReauth + `'
	WHEN a.state <> 'active' THEN '` + PauseError + `'
	WHEN a.sync_held_at <> 0 THEN '` + PauseHeld + `'
	WHEN a.owner_user_id IS NOT NULL THEN CASE
		WHEN NOT EXISTS (SELECT 1 FROM users u WHERE u.id = a.owner_user_id AND u.status = 'active')
			THEN '` + PausePersonOff + `'
		WHEN NOT EXISTS (SELECT 1 FROM users u WHERE u.id = a.owner_user_id AND u.sync_consent_at <> 0)
			THEN '` + PauseConsentOff + `'
		ELSE ` + pausedOrNone + ` END
	WHEN a.workspace_id = 'wsp_operator' THEN CASE WHEN a.sync_enabled_at = 0 THEN '` + PauseConsentOff + `' ELSE ` +
		pausedOrNone + ` END
	WHEN a.sync_enabled_at = 0 THEN '` + PauseConsentOff + `'
	WHEN ` + boundToSomeoneOff + ` THEN '` + PausePersonOff + `'
	WHEN NOT ` + hasReader + ` THEN '` + PauseNoReader + `'
	WHEN a.sync_paused_at <> 0 THEN '` + PausePaused + `'
	ELSE '' END)`
}

// hasReader is whether a mailbox, aliased a, has a reader, by the one rule
// (ReaderSQL).
var hasReader = `EXISTS (SELECT 1 FROM mailbox_access r WHERE r.account_id = a.id AND ` + ReaderSQL("r") + `)`

// hasSuspendedReader is whether a mailbox, aliased a, would have a reader
// once a suspended person is resumed (SuspendedReaderSQL).
var hasSuspendedReader = `EXISTS (SELECT 1 FROM mailbox_access r WHERE r.account_id = a.id AND ` +
	SuspendedReaderSQL("r") + `)`

// SyncEligibleAccounts lists every account the engine should be syncing,
// oldest first: the engine's rule, which a pause refuses.
func (s *Store) SyncEligibleAccounts(ctx context.Context) ([]string, error) {
	return s.accountsUnder(ctx, s.engineRule(), "list accounts eligible to sync")
}

// PausedAccounts lists the accounts whose index still follows (indexRule)
// and whose sync someone paused, oldest first: the ones the engine runs no
// worker for and keeps an interactive connection open for between calls
// (sync.Manager). Each is in IndexFollows and not in SyncEligibleAccounts.
func (s *Store) PausedAccounts(ctx context.Context) ([]string, error) {
	return s.accountsUnder(ctx, s.indexRule()+` AND a.sync_paused_at <> 0`, "list the paused accounts")
}

// accountsUnder lists the accounts, aliased a, a rule built from constants
// holds, oldest first.
func (s *Store) accountsUnder(ctx context.Context, rule, what string) ([]string, error) {
	//nolint:gosec // G202: the rules are built from constants alone (ReaderSQL's fragments); nothing is interpolated
	rows, err := s.r.QueryContext(ctx, `SELECT a.id FROM accounts a WHERE `+rule+` ORDER BY a.created_at, a.id`)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return ids, nil
}

// SyncEligible reports whether one account should be syncing: the engine's
// rule, which a pause refuses. An account that does not exist is not.
func (s *Store) SyncEligible(ctx context.Context, accountID string) (bool, error) {
	return syncEligibleOn(ctx, s.r, s.engineRule(), accountID)
}

// IndexFollows reports whether an account's index may be written for what
// someone asks now: the engine's rule but for a pause (indexRule). An action
// on a paused mailbox is recorded in its index; an account that does not
// exist does not.
func (s *Store) IndexFollows(ctx context.Context, accountID string) (bool, error) {
	return syncEligibleOn(ctx, s.r, s.indexRule(), accountID)
}

// RequireIndexFollowsTx fails with ErrNotEligible unless the account's index
// may be written for an action, inside the caller's transaction: the action
// recordings start with it, as every sync batch starts with
// RequireSyncEligibleTx.
func (s *Store) RequireIndexFollowsTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	return requireRule(ctx, tx, s.indexRule(), accountID)
}

// RequireSyncEligibleTx fails with ErrNotEligible unless the account may be
// synced, inside the caller's transaction: the engine's rule, which a pause
// refuses.
//
// Every transaction the engine writes to the index starts with this. The
// database has one writer, so a withdrawal of consent — or an account being
// disabled, removed or paused — and a sync batch are ordered: once the
// withdrawal commits, no batch that was already on its way can store another
// row, however late its worker notices it has been stopped.
func (s *Store) RequireSyncEligibleTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	return requireRule(ctx, tx, s.engineRule(), accountID)
}

func requireRule(ctx context.Context, tx *sql.Tx, rule, accountID string) error {
	ok, err := syncEligibleOn(ctx, tx, rule, accountID)
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

func syncEligibleOn(ctx context.Context, q queryRower, rule, accountID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM accounts a WHERE a.id = ? AND `+rule, accountID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check sync eligibility: %w", err)
	}
	return true, nil
}

// TeamSyncNotices lists what the daemon says at start about team mailboxes:
// those kept stopped with their index because the person whose consent
// migration 0011 would have copied was disabled (bound to them, off), which
// someone still reads and an owner or an admin turns on again at the current
// sync text, or off, or removes; and those nobody can read, kept stopped ones
// included, which index nothing more and can never be read again: they can
// only be removed (and linked again), or turned off to delete what is still
// indexed. A mailbox a suspended person would read once resumed is not one
// of those: it waits for them. Nor is a removed one, which is about to go and
// asks nothing of anyone.
func (s *Store) TeamSyncNotices(ctx context.Context) (keptStopped, noReader []string, err error) {
	keptStopped, err = s.teamIDs(ctx, teamsKeptStopped)
	if err != nil {
		return nil, nil, err
	}
	noReader, err = s.teamIDs(ctx, teamsNobodyReads)
	if err != nil {
		return nil, nil, err
	}
	return keptStopped, noReader, nil
}

// The queries of TeamSyncNotices.
var (
	teamsKeptStopped = `SELECT a.id FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		WHERE w.kind = 'team' AND a.removed_at = 0 AND a.sync_enabled_via = 'migration' AND a.sync_enabled_at = 0
		  AND ` + hasReader + `
		ORDER BY a.created_at, a.id`
	teamsNobodyReads = `SELECT a.id FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		WHERE w.kind = 'team' AND a.removed_at = 0 AND NOT ` + hasReader + ` AND NOT ` + hasSuspendedReader + `
		ORDER BY a.created_at, a.id`
)

// BoundTo lists the team mailboxes whose consent to sync is still the one
// migration 0011 copied from userID's own, oldest first: what syncs nothing
// while an extension has them suspended (boundToSomeoneOff), and catches up
// once they are resumed.
func (s *Store) BoundTo(ctx context.Context, userID string) ([]string, error) {
	return s.teamIDs(ctx, `SELECT a.id FROM accounts a WHERE a.sync_enabled_via = 'migration' AND a.sync_enabled_by = ?
		ORDER BY a.created_at, a.id`, userID)
}

// ClosedTeams lists the teams that are closed (migration 0019), oldest first:
// what the daemon says at start, since none of their mailboxes syncs until
// they reopen.
func (s *Store) ClosedTeams(ctx context.Context) ([]string, error) {
	return s.teamIDs(ctx, `SELECT id FROM workspaces WHERE closed_at <> 0 ORDER BY created_at, id`)
}

func (s *Store) teamIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list teams or team mailboxes: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list teams or team mailboxes: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list teams or team mailboxes: %w", err)
	}
	return ids, nil
}
