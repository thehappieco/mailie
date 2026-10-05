package service

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/thehappieco/mailie/internal/auth"
)

// Storage is what the caller's mailboxes take up in the index, as GET
// /v1/me/storage reports it.
type Storage struct {
	// Mailboxes are the ones the caller may read, by address, each with what
	// is indexed for it. A mailbox never synced, or whose index was deleted
	// (whoever linked it withdrew consent, or the operator switched its sync
	// off), has zeros.
	// One whose account needs signing in again stops syncing but keeps its
	// index, and reports what was indexed before. A disabled person's
	// mailboxes keep their index too, until `user delete` removes it, but
	// nobody is told here: disabling ended their sessions and revoked their
	// keys, and no other credential sees a person's mailboxes.
	Mailboxes []MailboxStorage `json:"mailboxes"`
	// Workspaces sums Mailboxes per workspace, in the order the workspaces
	// first appear there: never a mailbox the caller cannot read, so not a
	// workspace's whole usage.
	Workspaces []WorkspaceStorage `json:"workspaces"`
	// Total is the sum over Mailboxes.
	Total StorageTotal `json:"total"`
	// DatabaseBytes is the size of the database file and its write-ahead
	// log on disk: everything this daemon keeps, for every person. Only an
	// owner signed in to the console is told.
	DatabaseBytes *int64 `json:"database_bytes,omitempty"`
}

// MailboxStorage is one mailbox's share.
type MailboxStorage struct {
	AccountID string `json:"account_id"`
	// WorkspaceID is the workspace the mailbox belongs to.
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
	// Messages counts the indexed copies still in their folder. A message
	// in several folders — a Gmail message under several labels — counts
	// once in each.
	Messages int64 `json:"messages"`
	// Bytes sums those copies' sizes as the mail server reports them
	// (RFC822.SIZE), counted the same way: the mail's size on the server,
	// not what the index keeps of it, which is its metadata.
	Bytes int64 `json:"bytes"`
}

// WorkspaceStorage is the sum over the caller's readable mailboxes of one
// workspace.
type WorkspaceStorage struct {
	WorkspaceID string `json:"workspace_id"`
	// Mailboxes counts them.
	Mailboxes int   `json:"mailboxes"`
	Messages  int64 `json:"messages"`
	Bytes     int64 `json:"bytes"`
}

// StorageTotal is the sum of every mailbox's Messages and Bytes.
type StorageTotal struct {
	Messages int64 `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// Storage reports what the mailboxes the caller may read take up.
//
// Which they are is the rule of reading a mailbox's index: a person's read
// grant, as an active member of its workspace — a grant without read shows
// the mailbox, but not what its index holds; a key restricted to some
// mailboxes counts only those. An instance key answers for the operator
// workspace's mailboxes, never a person's, which are theirs to count.
// workspaceID narrows to one workspace the caller is an active member of;
// empty is every one.
func (s *Service) Storage(ctx context.Context, p Principal, workspaceID string) (Storage, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return Storage{}, err
	}
	if err := s.inWorkspace(ctx, p, workspaceID); err != nil {
		return Storage{}, err
	}
	v := readable(p)
	v.Workspace = workspaceID
	all, err := s.accounts.Repo().ListVisible(ctx, v)
	if err != nil {
		return Storage{}, E(CodeInternal, "listing accounts failed", err)
	}
	ids := make([]string, 0, len(all))
	out := Storage{Mailboxes: make([]MailboxStorage, 0, len(all)), Workspaces: []WorkspaceStorage{}}
	for _, a := range all {
		if p.MayAccess(a.ID) {
			ids = append(ids, a.ID)
			out.Mailboxes = append(out.Mailboxes, MailboxStorage{AccountID: a.ID, WorkspaceID: a.WorkspaceID, Email: a.Email})
		}
	}
	slices.SortFunc(out.Mailboxes, func(a, b MailboxStorage) int {
		return cmp.Or(strings.Compare(a.Email, b.Email), strings.Compare(a.AccountID, b.AccountID))
	})
	if s.store == nil {
		// A service built without the database's own handle, as some
		// tools build it: nothing it can see is indexed.
		out.Workspaces = perWorkspace(out.Mailboxes)
		return out, nil
	}
	usage, err := s.store.Usage(ctx, ids)
	if err != nil {
		return Storage{}, E(CodeInternal, "reading what the index holds failed", err)
	}
	for i := range out.Mailboxes {
		u := usage[out.Mailboxes[i].AccountID]
		out.Mailboxes[i].Messages, out.Mailboxes[i].Bytes = u.Messages, u.Bytes
		out.Total.Messages += u.Messages
		out.Total.Bytes += u.Bytes
	}
	out.Workspaces = perWorkspace(out.Mailboxes)
	if p.IsSession() && p.UserRole == auth.RoleOwner {
		n, err := s.store.DiskBytes()
		if err != nil {
			return Storage{}, E(CodeInternal, "reading the size of the database failed", err)
		}
		out.DatabaseBytes = &n
	}
	return out, nil
}

// perWorkspace sums mailboxes per workspace, in the order each workspace
// first appears.
func perWorkspace(mailboxes []MailboxStorage) []WorkspaceStorage {
	out := []WorkspaceStorage{}
	index := map[string]int{}
	for _, m := range mailboxes {
		i, ok := index[m.WorkspaceID]
		if !ok {
			i = len(out)
			index[m.WorkspaceID] = i
			out = append(out, WorkspaceStorage{WorkspaceID: m.WorkspaceID})
		}
		out[i].Mailboxes++
		out[i].Messages += m.Messages
		out[i].Bytes += m.Bytes
	}
	return out
}
