package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A snapshot of who holds what on a followed workspace's mailboxes, for an
// extension that reports it to the directory its workspaces come from
// (docs/workspaces.md, "Following an external directory"). It decides
// nothing: each answer is the one reading rule's (store.ReaderSQL and its
// fragments), the last reader's and the one sync rule's (store.PauseSQL),
// read in one transaction. A removed mailbox is reported as for a live one
// until it is deleted for good: who would read it, wait for its key and
// hold its key once it is recovered, and its kept last readers as
// protected, which they are in what this server refuses too. No transport
// calls it.

// SnapshotPause is why a live mailbox does not sync, the first that holds;
// empty when it syncs, and on a removed mailbox.
type SnapshotPause string

// Why a live mailbox does not sync, in the order they are asked.
const (
	PauseTeamClosed  SnapshotPause = store.PauseTeamClosed
	PauseWithheld    SnapshotPause = store.PauseWithheld
	PausePendingAuth SnapshotPause = store.PausePendingAuth
	PauseNeedsReauth SnapshotPause = store.PauseNeedsReauth
	PauseError       SnapshotPause = store.PauseError
	PauseHeld        SnapshotPause = store.PauseHeld
	PauseConsentOff  SnapshotPause = store.PauseConsentOff
	PausePersonOff   SnapshotPause = store.PausePersonOff
	PauseNoReader    SnapshotPause = store.PauseNoReader
	PausePaused      SnapshotPause = store.PausePaused
)

// SnapshotPauses are every cause a snapshot reports: what an extension maps
// one by one (its tests hold every one to a reason).
func SnapshotPauses() []SnapshotPause {
	return []SnapshotPause{PauseTeamClosed, PauseWithheld, PausePendingAuth, PauseNeedsReauth, PauseError, PauseHeld,
		PauseConsentOff, PausePersonOff, PauseNoReader, PausePaused}
}

// The bounds of a snapshot (ErrSnapshotTooLarge past them).
const (
	SnapshotMaxMailboxes = workspace.SnapshotMaxMailboxes
	SnapshotMaxPeople    = workspace.SnapshotMaxPeople
)

// AccessSnapshot is a followed workspace's mailboxes and who holds what on
// them, measured at one moment.
type AccessSnapshot struct {
	WorkspaceID string
	Kind        workspace.Kind
	Closed      bool
	MeasuredAt  time.Time
	// Mailboxes are live and removed ones, until they are deleted for good,
	// the oldest first.
	Mailboxes []SnapshotMailbox
}

// SnapshotMailbox is one mailbox of a snapshot.
type SnapshotMailbox struct {
	ID string
	// Label is its name (store.MailboxLabel), empty when that would say its
	// address; Hint its address masked (store.MailboxHint). Never the
	// address.
	Label, Hint string
	// Removed is a mailbox kept until PurgeAfter.
	Removed    bool
	PurgeAfter time.Time
	// Pause is why a live mailbox does not sync; empty on a removed one.
	Pause SnapshotPause
	// People hold a flag on it or a sealed grant.
	People []SnapshotPerson
	// Protected are the people a team mailbox cannot lose: each the only
	// one who reads it, or on a removed one would read it once recovered.
	// None in a personal workspace.
	Protected []string
	// Keys are the live keys the registered issuer made (KeyIssuer) that
	// hold a flag on it: neither revoked, ended nor past their expiry; one
	// stopped here included, its stop said in IssuedKeysReport. Never in
	// People or Protected: a key reads nothing for anyone. Empty, never
	// nil, where there are none.
	Keys []SnapshotKey
}

// SnapshotKey is a live issued key holding a flag on a mailbox.
type SnapshotKey struct {
	// ExternalID is the key's id at its issuer.
	ExternalID string
	// Flags are what it holds there: read, and act only beside it.
	Flags workspace.Flags
	// Reads is the read flag: a key reads by its flag alone.
	Reads bool
}

// SnapshotPerson is what a person holds on a mailbox and what the reading
// rule says of them.
type SnapshotPerson struct {
	UserID string
	// Flags are the stored flags: read, act, send, manage.
	Flags workspace.Flags
	// Reads: they read it now. Waiting: they hold read without its key yet.
	// Enrolled: they have an account key. HoldsKey: they hold a sealed grant
	// at its current epoch, read or not, a suspended person included. On a
	// removed mailbox, each as once it is recovered.
	Reads, Waiting, Enrolled, HoldsKey bool
}

// ErrSnapshotTooLarge is a workspace with more mailboxes, or a mailbox with
// more people, than a snapshot holds: conflict.
var ErrSnapshotTooLarge = workspace.ErrSnapshotTooLarge

// AccessSnapshot reads a followed workspace's snapshot in one read-only
// transaction (store.ReadTx), for an extension; it takes no principal. A
// workspace that is not from the directory's source, or that nobody knows,
// is not_found; one larger than a snapshot holds, conflict; a server whose
// workspaces are local, conflict (ErrNotFollowing). A closed team's mailboxes
// are read as they are: the pause says the team is closed, and who reads
// them is computed without the closure.
func (s *Service) AccessSnapshot(ctx context.Context, workspaceID string) (AccessSnapshot, error) {
	if err := s.reportedWorkspace(ctx, workspaceID); err != nil {
		return AccessSnapshot{}, err
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return AccessSnapshot{}, E(CodeInternal, "reading the snapshot failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	out, err := s.accessSnapshotTx(ctx, tx, workspaceID, s.now())
	if errors.Is(err, workspace.ErrSnapshotTooLarge) {
		return AccessSnapshot{}, errSnapshotTooLarge(err)
	}
	return out, err
}

// reportedWorkspace refuses a workspace an extension reports nothing of: on
// a server that follows no directory (conflict, ErrNotFollowing), and one
// that is not from the directory's source or that nobody knows (not_found).
func (s *Service) reportedWorkspace(ctx context.Context, workspaceID string) error {
	if err := s.following(); err != nil {
		return err
	}
	return reportedSource(s.workspaces.Get(ctx, workspaceID))
}

// reportedSource refuses a workspace read as nobody knows it, or as one that
// is not from the directory's source (not_found): what reportedWorkspace
// asks of a workspace once the server follows a directory.
func reportedSource(w workspace.Workspace, err error) error {
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return errNoSnapshotWorkspace
	case err != nil:
		return E(CodeInternal, "reading the workspace failed", err)
	case w.Source == workspace.SourceLocal:
		return errNoSnapshotWorkspace
	}
	return nil
}

// accessSnapshotTx reads a workspace's snapshot in the caller's read,
// measured at measured. ErrSnapshotTooLarge past its bounds, as it is.
func (s *Service) accessSnapshotTx(ctx context.Context, tx *sql.Tx, workspaceID string, measured time.Time) (AccessSnapshot, error) {
	read, err := workspace.SnapshotTx(ctx, tx, workspaceID, workspace.SnapshotOptions{Withheld: s.teamsWithheld})
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return AccessSnapshot{}, errNoSnapshotWorkspace
	case errors.Is(err, workspace.ErrSnapshotTooLarge):
		return AccessSnapshot{}, err
	case err != nil:
		return AccessSnapshot{}, E(CodeInternal, "reading the snapshot failed", err)
	}
	out := AccessSnapshot{
		WorkspaceID: read.WorkspaceID, Kind: read.Kind, Closed: read.Closed, MeasuredAt: measured,
		Mailboxes: make([]SnapshotMailbox, 0, len(read.Mailboxes)),
	}
	keys, err := s.snapshotKeysTx(ctx, tx, workspaceID, measured)
	if err != nil {
		return AccessSnapshot{}, E(CodeInternal, "reading the snapshot failed", err)
	}
	for _, mb := range read.Mailboxes {
		m := SnapshotMailbox{
			ID: mb.ID, Label: mb.Label, Hint: mb.Hint, Removed: mb.Removed, Pause: SnapshotPause(mb.Pause),
			People: make([]SnapshotPerson, 0, len(mb.People)), Protected: mb.Protected,
			Keys: keys[mb.ID],
		}
		if m.Keys == nil {
			m.Keys = []SnapshotKey{}
		}
		if mb.Removed {
			m.PurgeAfter = time.Unix(mb.PurgeAfter, 0).UTC()
		}
		for _, p := range mb.People {
			m.People = append(m.People, SnapshotPerson{
				UserID: p.UserID, Flags: p.Flags, Reads: p.Reads, Waiting: p.Waiting, Enrolled: p.Enrolled,
				HoldsKey: p.HoldsKey,
			})
		}
		out.Mailboxes = append(out.Mailboxes, m)
	}
	return out, nil
}

// errSnapshotTooLarge is a snapshot past its bounds, as AccessSnapshot
// answers it.
func errSnapshotTooLarge(err error) error {
	return Ef(CodeConflict, err, "the workspace is larger than a snapshot holds: %d mailboxes, %d people on a mailbox",
		SnapshotMaxMailboxes, SnapshotMaxPeople)
}

var errNoSnapshotWorkspace = E(CodeNotFound, "no such workspace from the directory's source", nil)

// snapshotKeysTx reads, in the snapshot's read, the live keys the registered
// issuer made that hold a flag on each mailbox of a workspace, by mailbox.
func (s *Service) snapshotKeysTx(ctx context.Context, tx *sql.Tx, workspaceID string, measured time.Time,
) (map[string][]SnapshotKey, error) {
	out := map[string][]SnapshotKey{}
	if s.issuer == nil {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT x.account_id, i.external_id, x.read, x.act, x.send
		  FROM key_access x JOIN api_keys k ON k.prefix = x.key_prefix JOIN issued_keys i ON i.prefix = k.prefix
		 WHERE x.workspace_id = ? AND k.issuer = ? AND k.revoked_at = 0 AND i.ended_at = 0 AND k.expires_at > ?
		 ORDER BY x.account_id, i.external_id`, workspaceID, s.issuer.Issuer(), measured.Unix())
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			account string
			k       SnapshotKey
		)
		if err := rows.Scan(&account, &k.ExternalID, &k.Flags.Read, &k.Flags.Act, &k.Flags.Send); err != nil {
			return nil, err
		}
		k.Reads = k.Flags.Read
		out[account] = append(out[account], k)
	}
	return out, rows.Err()
}
