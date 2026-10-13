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
// sync_enabled_at never stands in for a person's consent: nobody turns sync
// on for somebody else's personal mailbox, and a withdrawal stops it whatever
// that column says.
//
// Where the edition withholds teams (Options.TeamsWithheld), no team's
// mailbox is eligible either (notATeams): Store.eligibleRule adds it.
var syncEligible = `a.state = 'active' AND ` + OpenWorkspaceSQL("a.workspace_id") + ` AND ` + syncPermitted

// notATeams is that the mailbox, aliased a, is not a team's: what an
// edition that withholds teams adds to the rule, so that nothing of a team
// is used there, its mail included, and the index it has keeps until an
// edition that offers teams syncs it again.
var notATeams = `NOT EXISTS (SELECT 1 FROM workspaces tw WHERE tw.id = a.workspace_id AND tw.kind = 'team')`

// eligibleRule is the rule this store holds accounts to: the one above, and,
// where the edition withholds teams, not a team's mailbox.
func (s *Store) eligibleRule() string {
	if s.teamsWithheld {
		return syncEligible + ` AND ` + notATeams
	}
	return syncEligible
}

// syncPermitted is the consent half of the rule, as a boolean expression over
// accounts aliased a. SyncPermitted reports it on its own; syncEligible adds
// that the account is active.
var syncPermitted = `CASE
	WHEN a.owner_user_id IS NOT NULL THEN EXISTS (SELECT 1 FROM users u WHERE u.id = a.owner_user_id
	                                                AND u.status = 'active' AND u.sync_consent_at <> 0)
	WHEN a.workspace_id = 'wsp_operator' THEN a.sync_enabled_at <> 0
	ELSE a.sync_enabled_at <> 0 AND ` + hasReader + ` AND NOT ` + boundToSomeoneOff + ` END`

// boundToSomeoneOff is a mailbox, aliased a, whose consent to sync is still
// the one migration 0011 copied from a person's own while that person is not
// active: suspended, since disabling or deleting them stops it.
var boundToSomeoneOff = `(a.sync_enabled_via = 'migration' AND EXISTS (SELECT 1 FROM users bound_u
	WHERE bound_u.id = a.sync_enabled_by AND bound_u.status <> 'active'))`

// hasReader is whether a mailbox, aliased a, has a reader, by the one rule
// (ReaderSQL).
var hasReader = `EXISTS (SELECT 1 FROM mailbox_access r WHERE r.account_id = a.id AND ` + ReaderSQL("r") + `)`

// hasSuspendedReader is whether a mailbox, aliased a, would have a reader
// once a suspended person is resumed (SuspendedReaderSQL).
var hasSuspendedReader = `EXISTS (SELECT 1 FROM mailbox_access r WHERE r.account_id = a.id AND ` +
	SuspendedReaderSQL("r") + `)`

// SyncEligibleAccounts lists every account the engine should be syncing,
// oldest first.
func (s *Store) SyncEligibleAccounts(ctx context.Context) ([]string, error) {
	//nolint:gosec // G202: syncEligible is built from constants alone (ReaderSQL's fragments); nothing is interpolated
	rows, err := s.r.QueryContext(ctx, `SELECT a.id FROM accounts a WHERE `+s.eligibleRule()+` ORDER BY a.created_at, a.id`)
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
	return syncEligibleOn(ctx, s.r, s.eligibleRule(), accountID)
}

// RequireSyncEligibleTx fails with ErrNotEligible unless the account may be
// synced, inside the caller's transaction.
//
// Every transaction that writes to the index starts with this. The database
// has one writer, so a withdrawal of consent — or an account being disabled
// or removed — and a sync batch are ordered: once the withdrawal commits, no
// batch that was already on its way can store another row, however late its
// worker notices it has been stopped.
func (s *Store) RequireSyncEligibleTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	ok, err := syncEligibleOn(ctx, tx, s.eligibleRule(), accountID)
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
// of those: it waits for them.
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
		WHERE w.kind = 'team' AND a.sync_enabled_via = 'migration' AND a.sync_enabled_at = 0 AND ` + hasReader + `
		ORDER BY a.created_at, a.id`
	teamsNobodyReads = `SELECT a.id FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		WHERE w.kind = 'team' AND NOT ` + hasReader + ` AND NOT ` + hasSuspendedReader + `
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
