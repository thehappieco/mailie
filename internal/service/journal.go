package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What an extension reads to report a followed workspace elsewhere: the
// journal of what was done to its mailboxes, as an outbox read per workspace
// from a cursor; the workspaces from the directory's source; which of them
// changed what a snapshot shows; and its own state, kept beside the data it
// describes. No transport calls any of it, and none takes a principal.

// JournalQuery asks for a workspace's entries after a seq.
type JournalQuery struct {
	WorkspaceID string
	After       int64
	// Limit is 1 to MaxJournalPage.
	Limit int
}

// MaxJournalPage is the most entries MailboxJournal returns at once.
const MaxJournalPage = store.MaxJournalPage

// JournalEntry is one entry of the journal of what was done to mailboxes.
type JournalEntry struct {
	Seq int64
	// ID is "mbe_" and 16 hex digits: the entry's own, once.
	ID string
	// Kind is removed, sync_paused, recovered, purged or stranded.
	Kind string
	// Cause is a stranding's (membership_ended, forgotten, disabled,
	// deleted, reset) and a sync turned off's (empty, or bound_stopped when
	// the consent bound to a person stopped with them); empty otherwise.
	Cause                               string
	WorkspaceID, AccountID, Label, Hint string
	At                                  time.Time
	// PurgeAfter is when a removed mailbox may be deleted for good; zero on
	// every other kind.
	PurgeAfter time.Time
	// Actor is who did it, raw: "usr_…", "key:<prefix>", "cli", "sweep",
	// "directory", or empty once forgotten.
	Actor string
	// ActorUserID is the person behind the actor: the person, or a key's
	// creator; empty for anyone else.
	ActorUserID string
	// LostUserID is the person a stranding lost; empty once forgotten.
	LostUserID string
	// Readers are who read the mailbox when it was done.
	Readers []string
}

// MailboxJournal lists a workspace's entries after q.After, oldest first, at
// most q.Limit, each with who read the mailbox then: the extension's outbox,
// read from its cursor. The entries stay for their retention whatever is
// read. A limit outside 1 to MaxJournalPage is bad_request.
func (s *Service) MailboxJournal(ctx context.Context, q JournalQuery) ([]JournalEntry, error) {
	if s.store == nil {
		return nil, nil
	}
	if q.Limit < 1 || q.Limit > MaxJournalPage {
		return nil, Ef(CodeBadRequest, nil, "a journal's page is 1 to %d entries", MaxJournalPage)
	}
	if q.After < 0 {
		return nil, E(CodeBadRequest, "a journal's cursor is never negative", nil)
	}
	rows, err := s.store.JournalOf(ctx, q.WorkspaceID, q.After, q.Limit)
	if err != nil {
		return nil, E(CodeInternal, "reading the journal failed", err)
	}
	out := make([]JournalEntry, 0, len(rows))
	for _, r := range rows {
		e := JournalEntry{
			Seq: r.Seq, ID: r.ID, Kind: string(r.Kind), Cause: r.Cause, WorkspaceID: r.WorkspaceID,
			AccountID: r.AccountID, Label: r.Label, Hint: r.Hint, At: time.Unix(r.At, 0).UTC(), Actor: r.Actor,
			ActorUserID: r.ActorUserID, LostUserID: r.LostUser, Readers: r.Readers,
		}
		if r.PurgeAfter != 0 {
			e.PurgeAfter = time.Unix(r.PurgeAfter, 0).UTC()
		}
		if e.Readers == nil {
			e.Readers = []string{}
		}
		out = append(out, e)
	}
	return out, nil
}

// MailboxJournalHead is the seq of the newest entry of the journal, 0 when
// it holds none.
func (s *Service) MailboxJournalHead(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, nil
	}
	seq, err := s.store.JournalHead(ctx)
	if err != nil {
		return 0, E(CodeInternal, "reading the journal failed", err)
	}
	return seq, nil
}

// FollowedWorkspace is a workspace from the directory's source.
type FollowedWorkspace struct {
	ID   string
	Kind workspace.Kind
	// PersonID is a personal workspace's person; empty for a team.
	PersonID string
	Closed   bool
}

// FollowedWorkspaces lists every workspace whose source is not the local
// one, the oldest first.
func (s *Service) FollowedWorkspaces(ctx context.Context) ([]FollowedWorkspace, error) {
	if s.store == nil {
		return nil, nil
	}
	listed, err := s.store.FollowedWorkspaces(ctx)
	if err != nil {
		return nil, E(CodeInternal, "listing the followed workspaces failed", err)
	}
	out := make([]FollowedWorkspace, 0, len(listed))
	for _, w := range listed {
		out = append(out, FollowedWorkspace{ID: w.ID, Kind: workspace.Kind(w.Kind), PersonID: w.PersonID, Closed: w.Closed})
	}
	return out, nil
}

// WorkspaceChange is a followed workspace's mark: the seq of the last change
// to what its snapshot shows.
type WorkspaceChange struct {
	WorkspaceID string
	Seq         int64
}

// WorkspaceChanges lists the followed workspaces whose marks are after
// after, in the order of their marks, at most limit, and the highest mark
// there is, read at one moment (store.WorkspaceChanges): what an extension
// sends a new snapshot of.
func (s *Service) WorkspaceChanges(ctx context.Context, after int64, limit int) ([]WorkspaceChange, int64, error) {
	if s.store == nil {
		return nil, 0, nil
	}
	if limit < 1 || limit > store.MaxWorkspaceChanges {
		return nil, 0, Ef(CodeBadRequest, nil, "a page of changes is 1 to %d workspaces", store.MaxWorkspaceChanges)
	}
	marks, head, err := s.store.WorkspaceChanges(ctx, after, limit)
	if err != nil {
		return nil, 0, E(CodeInternal, "reading the workspace changes failed", err)
	}
	out := make([]WorkspaceChange, 0, len(marks))
	for _, m := range marks {
		out = append(out, WorkspaceChange(m))
	}
	return out, head, nil
}

// ExtensionState reads one value of an extension's own state; false when
// the key holds none.
func (s *Service) ExtensionState(ctx context.Context, ext, key string) ([]byte, bool, error) {
	if s.store == nil {
		return nil, false, errNoState
	}
	value, ok, err := s.store.ExtensionState(ctx, ext, key)
	if err != nil {
		return nil, false, fromExtensionState(err, "reading the extension's state failed")
	}
	return value, ok, nil
}

// ExtensionStateKeys lists the keys of an extension's state that begin with
// prefix, sorted.
func (s *Service) ExtensionStateKeys(ctx context.Context, ext, prefix string) ([]string, error) {
	if s.store == nil {
		return nil, errNoState
	}
	keys, err := s.store.ExtensionStateKeys(ctx, ext, prefix)
	if err != nil {
		return nil, fromExtensionState(err, "listing the extension's state failed")
	}
	return keys, nil
}

// WriteExtensionState puts and deletes keys of an extension's state in one
// write: nothing at all when a name, a key or a value is out of bounds
// (bad_request).
func (s *Service) WriteExtensionState(ctx context.Context, ext string, put map[string][]byte, del []string) error {
	if s.store == nil {
		return errNoState
	}
	if err := s.store.WriteExtensionState(ctx, ext, put, del); err != nil {
		return fromExtensionState(err, "writing the extension's state failed")
	}
	return nil
}

var errNoState = E(CodeInternal, "this service has no database to keep an extension's state in", nil)

func fromExtensionState(err error, what string) error {
	if errors.Is(err, store.ErrExtensionState) {
		return E(CodeBadRequest, "an extension's name is 1 to 64 bytes, a key 1 to 256 and a value a mebibyte at most", err)
	}
	return E(CodeInternal, what, err)
}
