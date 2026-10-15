package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A snapshot of who holds what on a followed workspace's mailboxes, for an
// extension that reports it to the directory its workspaces come from
// (docs/workspaces.md, "Following an external directory"). It decides
// nothing: each answer is the one reading rule's (store.ReaderSQL and its
// fragments) and the one sync rule's (store.PauseSQL), read in one
// transaction. No transport calls it.

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
)

// The bounds of a snapshot (ErrSnapshotTooLarge past them).
const (
	SnapshotMaxMailboxes = workspace.SnapshotMaxMailboxes
	SnapshotMaxPeople    = workspace.SnapshotMaxPeople
)

// RemovedInSnapshot is what a snapshot says of a removed mailbox's people:
// the one switch between the two rules a directory may ask for.
type RemovedInSnapshot int

const (
	// RemovedAsKept, the default, reports a removed mailbox's people as for
	// a live one: who would read it, wait for its key and hold its key once
	// it is recovered, and its kept last readers as protected, until it is
	// deleted for good. A removed mailbox here still has no reader and
	// protects nobody in what this server refuses: only what the snapshot
	// says follows the directory's rule.
	RemovedAsKept RemovedInSnapshot = iota
	// RemovedAsNobody reports a removed mailbox as the rules here see it:
	// nobody reads it, waits for its key, holds its key or is protected.
	RemovedAsNobody
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
	// one who reads it. None in a personal workspace.
	Protected []string
}

// SnapshotPerson is what a person holds on a mailbox and what the reading
// rule says of them.
type SnapshotPerson struct {
	UserID string
	// Flags are the stored flags: read, act, send, manage.
	Flags workspace.Flags
	// Reads: they read it now. Waiting: they hold read without its key yet.
	// Enrolled: they have an account key. HoldsKey: they hold a sealed grant
	// at its current epoch, read or not, a suspended person included.
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
	if err := s.following(); err != nil {
		return AccessSnapshot{}, err
	}
	w, err := s.workspaces.Get(ctx, workspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return AccessSnapshot{}, errNoSnapshotWorkspace
	case err != nil:
		return AccessSnapshot{}, E(CodeInternal, "reading the workspace failed", err)
	case w.Source == workspace.SourceLocal:
		return AccessSnapshot{}, errNoSnapshotWorkspace
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return AccessSnapshot{}, E(CodeInternal, "reading the snapshot failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	measured := s.now()
	read, err := workspace.SnapshotTx(ctx, tx, workspaceID, workspace.SnapshotOptions{
		RemovedAsLive: s.removedInSnapshot == RemovedAsKept, Withheld: s.teamsWithheld,
	})
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return AccessSnapshot{}, errNoSnapshotWorkspace
	case errors.Is(err, workspace.ErrSnapshotTooLarge):
		return AccessSnapshot{}, Ef(CodeConflict, err,
			"the workspace is larger than a snapshot holds: %d mailboxes, %d people on a mailbox",
			SnapshotMaxMailboxes, SnapshotMaxPeople)
	case err != nil:
		return AccessSnapshot{}, E(CodeInternal, "reading the snapshot failed", err)
	}
	out := AccessSnapshot{
		WorkspaceID: read.WorkspaceID, Kind: read.Kind, Closed: read.Closed, MeasuredAt: measured,
		Mailboxes: make([]SnapshotMailbox, 0, len(read.Mailboxes)),
	}
	for _, mb := range read.Mailboxes {
		m := SnapshotMailbox{
			ID: mb.ID, Label: mb.Label, Hint: mb.Hint, Removed: mb.Removed, Pause: SnapshotPause(mb.Pause),
			People: make([]SnapshotPerson, 0, len(mb.People)), Protected: mb.Protected,
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

var errNoSnapshotWorkspace = E(CodeNotFound, "no such workspace from the directory's source", nil)
