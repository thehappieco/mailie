package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/thehappieco/mailie/internal/store"
)

// The last reader (docs/workspaces.md, "Protections").
//
// A team mailbox someone can read is never left with nobody who can. A reader
// is who reads it by the one rule (store.ReaderSQL, docs/key-scheme.md section
// 12.13): an active member of the mailbox's workspace, active on the
// instance, holding read on it, and, on a mailbox that has a key, holding a
// grant at its current epoch. Keys never count, and neither does a role: an
// owner who reads nothing is no way back into a mailbox. Read passes only
// from someone who reads, and the key only from someone who holds it, so a
// mailbox left with no reader could never be read again; the only way out is
// removing it and linking it again. A member who holds the flag and waits for
// the key is no reader, and taking their flag leaves the readers as they were.
//
// Guarded: revoking the last reader's read, disabling or removing their
// membership (leaving included), disabling or deleting them on the instance
// without force, unless the team goes with them (a local team they are the
// only member of, GoesWithPersonSQL) — a suspended person included, for the
// mailboxes they would read once resumed, which closing them takes for
// good — and resetting their account key without
// force (the reset deletes their grants, so it takes read only on a mailbox
// that has a key). Not guarded: removing the mailbox itself, and role
// changes, which change no read. A removed mailbox has no reader while it is
// kept (store.LiveSQL), but it keeps protecting until it is deleted for
// good: who would read it once recovered (store.KeptReaderSQL, and the
// other Kept fragments for whom it waits or is held for) counts here as who
// reads a live one does, so that a recovery within its 30 days brings back
// a mailbox someone can still read. Its last such reader is refused what
// the last reader of a live one is, and a change no protection refuses
// strands it as it would a live one; deleting it for good ends that, with
// its grants. Each check runs inside the transaction that would break the
// rule; the database has one writer, so two changes that would each leave
// one reader are ordered, and the second is refused.

// lastReaders narrows lastReaderOf.
type lastReaders struct {
	// workspaceID, when not empty, keeps the mailboxes of that workspace.
	workspaceID string
	// accountIDs, when not nil, keeps those mailboxes.
	accountIDs []string
	// outlived keeps only the mailboxes of teams that outlive the person's
	// account: every team that does not go with them (GoesWithPersonSQL,
	// the same rule SoleMemberTeamsTx and the deletion follow), which
	// deleting the person leaves standing: a team with another member,
	// whatever that member's status, and a team from another source, even
	// when they are its last member here. A team whose remaining members
	// are all disabled, or that has none left here, still outlives them,
	// and its mailbox could never be read again, since read passes only
	// from a reader; closing the account of someone alone in a local team
	// takes the team with them instead.
	outlived bool
	// keyed keeps only the mailboxes that have a key: what deleting the
	// person's grants, and nothing else, takes read from.
	keyed bool
	// orSuspended counts, for the person asked about, the mailboxes they
	// would read once resumed (store.KeptSuspendedReaderSQL) as well as those
	// they read now: what closing a suspended person takes from them for
	// good, since a disable here deletes their grants and a deletion
	// everything of theirs. Only the person's own closure asks it
	// (blocksOn); anyone else's change, and every listing, counts a
	// suspended person as reading nothing, so they are nobody's other
	// reader.
	orSuspended bool
	// strand asks what a change would strand rather than what it must not
	// break (StrandCandidatesTx): the mailboxes the person reads, would read
	// once resumed or would read but for a hold, where nobody else is any of
	// the three. A mailbox another person waits for, resumed or released, is
	// not stranded: it waits for them.
	strand bool
}

// protectedSQL is the condition, over mailbox_access aliased alias, that the
// last-reader protection counts its holder as a reader: they read the
// mailbox, or, on a removed one, would read it once recovered
// (store.KeptReaderSQL). On a live mailbox it is the one rule
// (store.ReaderSQL); a removed one keeps protecting until it is deleted for
// good, which takes its grants with it.
func protectedSQL(alias string) string {
	return store.KeptReaderSQL(alias)
}

// anyReaderSQL is the condition, over mailbox_access aliased alias, that its
// holder reads the mailbox, would read it once resumed, or would read it but
// for a hold, on a removed mailbox as once it is recovered: whom a mailbox is
// kept for.
func anyReaderSQL(alias string) string {
	return `(` + protectedSQL(alias) + ` OR ` + store.KeptSuspendedReaderSQL(alias) + ` OR ` +
		store.KeptHeldReaderSQL(alias) + `)`
}

// lastReaderOf lists the team mailboxes userID is the only reader of, live
// or removed and kept (protectedSQL), oldest first, among those in narrows
// to.
func lastReaderOf(ctx context.Context, q querier, userID string, in lastReaders) ([]string, error) {
	reads, others := protectedSQL("g"), protectedSQL("o")
	switch {
	case in.strand:
		reads, others = anyReaderSQL("g"), anyReaderSQL("o")
	case in.orSuspended:
		reads = `(` + reads + ` OR ` + store.KeptSuspendedReaderSQL("g") + `)`
	}
	query := `SELECT a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		  JOIN mailbox_access g ON g.account_id = a.id AND g.user_id = ?1
		 WHERE ` + reads + `
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> ?1 AND ` + others + `)
		   AND (?2 = '' OR a.workspace_id = ?2)`
	if in.outlived {
		query += ` AND NOT ` + GoesWithPersonSQL("w", "?1")
	}
	if in.keyed {
		query += ` AND ` + store.HasKeySQL("a.id")
	}
	args := []any{userID, in.workspaceID}
	if in.accountIDs != nil {
		list, err := json.Marshal(in.accountIDs)
		if err != nil {
			return nil, fmt.Errorf("workspace: encode ids: %w", err)
		}
		query += ` AND a.id IN (SELECT value FROM json_each(?3))`
		args = append(args, string(list))
	}
	return listIDs(ctx, q, query+` ORDER BY a.created_at, a.rowid`, args...)
}

// StrandScope narrows StrandCandidatesTx.
type StrandScope struct {
	// WorkspaceID, when not empty, keeps the mailboxes of that workspace:
	// what ending a membership there strands.
	WorkspaceID string
	// Outlived keeps the mailboxes of teams that outlive the person
	// (GoesWithPersonSQL): what closing them strands, since a local team
	// they are alone in goes with them at their deletion, mailboxes and all.
	Outlived bool
	// Keyed keeps the mailboxes that have a key: what deleting the person's
	// sealed grants, and nothing else, strands (a reset of their account
	// key).
	Keyed bool
}

// StrandCandidatesTx lists, inside the caller's transaction and before the
// change, the team mailboxes a change that takes userID's reading would
// leave stranded (docs/workspaces.md, "Removed mailboxes"): those they read,
// would read once resumed (store.SuspendedReaderSQL) or would read but for a
// hold (store.HeldReaderSQL), where no other person is any of the three — a
// removed mailbox, until it is deleted for good, as once it is recovered
// (the Kept fragments). A mailbox another person's hold keeps strands once,
// when the last of those holds ends, naming that person. Oldest first. It
// refuses nothing: the caller journals what it returns (JournalStrandedTx)
// once the change is decided, while the person still exists.
func StrandCandidatesTx(ctx context.Context, tx *sql.Tx, userID string, in StrandScope) ([]string, error) {
	return lastReaderOf(ctx, tx, userID, lastReaders{
		workspaceID: in.WorkspaceID, outlived: in.Outlived, keyed: in.Keyed, strand: true,
	})
}

// LastReaderOfKeyedTx lists, inside the caller's transaction, the team
// mailboxes that have a key and that userID is the only reader of, in every
// team, a removed one they alone would read once recovered included: what
// deleting the person's grants while they stay, in teams that stay with
// them, must not leave behind (the reset of their account key,
// docs/key-scheme.md section 12.6). A mailbox without a key is left out: they
// keep reading it by the flag, which the reset does not take. Unlike
// BlocksTx's LastReaderOf, a local team whose only member is the person
// counts, since nothing takes it away with them.
func LastReaderOfKeyedTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	return lastReaderOf(ctx, tx, userID, lastReaders{keyed: true})
}

// HasReaderTx reports, inside the caller's transaction, whether a mailbox
// has a reader: whether anyone could ever read what syncing it would store.
func HasReaderTx(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND `+store.ReaderSQL("g"), accountID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read the mailbox's readers: %w", err)
	}
	return n > 0, nil
}

// AwaitsReaderTx reports, inside the caller's transaction, whether a mailbox
// no one reads now would be read by a person suspended by an extension once
// they are resumed (store.SuspendedReaderSQL): it waits for them, and is not
// one nobody can ever read again.
func AwaitsReaderTx(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND `+store.SuspendedReaderSQL("g"), accountID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read the mailbox's suspended readers: %w", err)
	}
	return n > 0, nil
}

// ReadsNowTx reports, inside the caller's transaction, whether a person reads
// a mailbox now, by the one rule: what giving read to someone else, or to a
// workspace key, and handing the mailbox's key on, are held to (the giver
// reads it themself, docs/key-scheme.md section 9.3).
func ReadsNowTx(ctx context.Context, tx *sql.Tx, accountID, userID string) (bool, error) {
	return readsNow(ctx, tx, accountID, userID)
}

func readsNow(ctx context.Context, q querier, accountID, userID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND g.user_id = ? AND `+store.ReaderSQL("g"), accountID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read whether the person reads the mailbox: %w", err)
	}
	return n > 0, nil
}

// requireAnotherReaderTx refuses, inside the caller's transaction, a change
// that takes read on accountIDs from userID when they are the last reader of
// one of them (ErrLastReader). Only a live mailbox's grant changes: nothing
// is given or taken on a removed one.
func requireAnotherReaderTx(ctx context.Context, tx *sql.Tx, userID string, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	last, err := lastReaderOf(ctx, tx, userID, lastReaders{accountIDs: accountIDs})
	if err != nil {
		return err
	}
	if len(last) > 0 {
		return fmt.Errorf("%w: %v", ErrLastReader, last)
	}
	return nil
}

// requireNotLastReaderInTx refuses, inside the caller's transaction, a change
// that takes userID's place in a workspace — disabling or removing their
// membership — while they are the last reader of one of its team mailboxes,
// live or removed and kept (ErrLastReader).
func requireNotLastReaderInTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string) error {
	last, err := lastReaderOf(ctx, tx, userID, lastReaders{workspaceID: workspaceID})
	if err != nil {
		return err
	}
	if len(last) > 0 {
		return fmt.Errorf("%w: %v", ErrLastReader, last)
	}
	return nil
}

// LastReaders maps each person to the team mailboxes of a workspace they are
// the only reader of, live or removed and kept (protectedSQL): what a
// listing marks in advance, so a console can explain before anyone tries.
func (r *Repository) LastReaders(ctx context.Context, workspaceID string) (map[string][]string, error) {
	return lastReadersIn(ctx, r.store.Reader(), workspaceID)
}

func lastReadersIn(ctx context.Context, q querier, workspaceID string) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT g.user_id, a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		  JOIN mailbox_access g ON g.account_id = a.id
		 WHERE a.workspace_id = ?1 AND `+protectedSQL("g")+`
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> g.user_id AND `+protectedSQL("o")+`)
		 ORDER BY a.created_at, a.rowid`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: read the last readers: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var userID, accountID string
		if err := rows.Scan(&userID, &accountID); err != nil {
			return nil, fmt.Errorf("workspace: read the last readers: %w", err)
		}
		out[userID] = append(out[userID], accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the last readers: %w", err)
	}
	return out, nil
}

// ReadBy lists the mailboxes a person holds the read flag on, in any
// workspace, whether or not it counts right now, and whether or not they hold
// the key: what may stop syncing when they go, for a team mailbox they were
// the last to read.
func (r *Repository) ReadBy(ctx context.Context, userID string) ([]string, error) {
	return listIDs(ctx, r.store.Reader(), `SELECT account_id FROM mailbox_access WHERE user_id = ? AND read = 1
		ORDER BY account_id`, userID)
}
