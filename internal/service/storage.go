package service

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Storage is what the caller's mailboxes hold in this server's database, as
// GET /v1/me/storage reports it.
type Storage struct {
	// RuleVersion is the version of the rule every Bytes is counted by
	// (store.RuleVersion): a logical, deterministic measure of what the
	// database holds for a mailbox, the same on every server for the same
	// rows. A client compares figures only under one version.
	RuleVersion int `json:"rule_version"`
	// Mailboxes are the live ones the caller may read and the removed ones
	// they may list, by address, each with what is indexed for it. A
	// mailbox never synced, or whose index was deleted (its consent was
	// withdrawn, or the operator switched its sync off), has zero messages,
	// and only its folders' bytes if it kept them.
	// One whose account needs signing in again stops syncing but keeps its
	// index, and reports what was indexed before. A disabled person's
	// mailboxes keep their index too, until `user delete` removes it, but
	// nobody is told here: disabling ended their sessions and revoked their
	// keys, and no other credential sees a person's mailboxes.
	Mailboxes []MailboxStorage `json:"mailboxes"`
	// Workspaces sums Mailboxes per workspace, in the order the workspaces
	// first appear there: never a mailbox the caller cannot read or list
	// removed, so not a workspace's whole usage.
	Workspaces []WorkspaceStorage `json:"workspaces"`
	// Total is the sum over Mailboxes, the removed ones included.
	Total StorageTotal `json:"total"`
	// DatabaseBytes is the size of the database file and its write-ahead
	// log on disk: everything this daemon keeps, for every person, in
	// SQLite's pages. Only the operator is told: an unrestricted instance
	// key with the admin scope.
	DatabaseBytes *int64 `json:"database_bytes,omitempty"`
}

// MailboxStorage is one mailbox's share.
type MailboxStorage struct {
	AccountID string `json:"account_id"`
	// WorkspaceID is the workspace the mailbox belongs to.
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
	// Removed is a mailbox removed and kept until it is deleted for good
	// (docs/workspaces.md, "Removed mailboxes"): what it keeps still
	// counts until then.
	Removed bool `json:"removed"`
	// Messages counts the indexed copies still in their folder. A message
	// in several folders — a Gmail message under several labels — counts
	// once in each.
	Messages int64 `json:"messages"`
	// Bytes is what the database holds for the mailbox under RuleVersion:
	// its live messages' rows, their parts, its folders and its open
	// drafts, each value counted by its bytes. A message in several folders
	// counts in each.
	Bytes int64 `json:"bytes"`
	// ProviderBytes sums its indexed copies' sizes as the mail server
	// reports them (RFC822.SIZE), counted the same way: the mail's size on
	// the server, not what the index keeps of it.
	ProviderBytes int64 `json:"provider_bytes"`
}

// WorkspaceStorage is the sum over the caller's mailboxes of one workspace.
type WorkspaceStorage struct {
	WorkspaceID string `json:"workspace_id"`
	// Mailboxes counts them, the removed ones included.
	Mailboxes     int   `json:"mailboxes"`
	Messages      int64 `json:"messages"`
	Bytes         int64 `json:"bytes"`
	ProviderBytes int64 `json:"provider_bytes"`
}

// StorageTotal is the sum of every mailbox's Messages, Bytes and
// ProviderBytes.
type StorageTotal struct {
	Messages      int64 `json:"messages"`
	Bytes         int64 `json:"bytes"`
	ProviderBytes int64 `json:"provider_bytes"`
}

// Storage reports what the caller's mailboxes hold.
//
// The live ones are those whose index the caller may read: a person's read
// grant, as an active member of its workspace — a grant without read shows
// the mailbox, but not what its index holds; a workspace key's read on the
// mailboxes it holds. An instance key answers for the operator workspace's
// mailboxes (those it is restricted to, when it is), never a person's,
// which are theirs to count.
//
// A removed mailbox counts until it is deleted for good (platform decision
// 0042): it is listed, Removed, to whoever lists it on the removed
// mailboxes' own paths (ListRemovedMailboxes), here as there — a personal
// workspace's person and a team's active owners signed in, of their open
// workspaces, never a key of theirs — and to the operator for the operator
// workspace's alone, as its live ones. workspaceID narrows to one workspace
// the caller reaches; empty is every one.
func (s *Service) Storage(ctx context.Context, p Principal, workspaceID string) (Storage, error) {
	if err := refuseDelegated(p); err != nil {
		return Storage{}, err
	}
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return Storage{}, err
	}
	if err := s.inWorkspace(ctx, p, workspaceID); err != nil {
		return Storage{}, err
	}
	v := s.readable(p)
	v.Workspace = workspaceID
	all, err := s.accounts.Repo().ListVisible(ctx, v)
	if err != nil {
		return Storage{}, E(CodeInternal, "listing accounts failed", err)
	}
	removed, err := s.removedToCount(ctx, p, workspaceID)
	if err != nil {
		return Storage{}, err
	}
	ids := make([]string, 0, len(all)+len(removed))
	out := Storage{
		RuleVersion: store.RuleVersion,
		Mailboxes:   make([]MailboxStorage, 0, len(all)+len(removed)),
		Workspaces:  []WorkspaceStorage{},
	}
	for _, a := range all {
		if p.MayAccess(a.ID) {
			ids = append(ids, a.ID)
			out.Mailboxes = append(out.Mailboxes, MailboxStorage{AccountID: a.ID, WorkspaceID: a.WorkspaceID, Email: a.Email})
		}
	}
	for _, a := range removed {
		ids = append(ids, a.ID)
		out.Mailboxes = append(out.Mailboxes, MailboxStorage{
			AccountID: a.ID, WorkspaceID: a.WorkspaceID, Email: a.Email, Removed: true,
		})
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
		m := &out.Mailboxes[i]
		m.Messages, m.Bytes, m.ProviderBytes = u.Messages, u.Bytes, u.ProviderBytes
		out.Total.Messages += u.Messages
		out.Total.Bytes += u.Bytes
		out.Total.ProviderBytes += u.ProviderBytes
	}
	out.Workspaces = perWorkspace(out.Mailboxes)
	if isOperator(p) {
		n, err := s.store.DiskBytes()
		if err != nil {
			return Storage{}, E(CodeInternal, "reading the size of the database failed", err)
		}
		out.DatabaseBytes = &n
	}
	return out, nil
}

// removedToCount is the removed mailboxes Storage counts for the caller, in
// workspaceID when it names one (inWorkspace has checked the caller reaches
// it): the ones ListRemovedMailboxes lists them, with its scope (admin, which
// a session holds), a person's of the open workspaces they are an active
// owner of, the operator's of the operator workspace. A team's is left out
// where teams are withheld, as its live ones are.
func (s *Service) removedToCount(ctx context.Context, p Principal, workspaceID string) ([]account.RemovedAccount, error) {
	if !p.Scope.Covers(auth.ScopeAdmin) {
		return nil, nil
	}
	var scope account.RemovedScope
	switch {
	case isOperator(p):
		scope = account.RemovedScope{Operator: true, Workspace: workspace.OperatorID}
	case p.IsSession():
		scope = account.RemovedScope{UserID: p.UserID, Workspace: workspaceID}
	default:
		// A key of a workspace never lists its removed mailboxes.
		return nil, nil
	}
	listed, err := s.accounts.Repo().ListRemoved(ctx, scope)
	if err != nil {
		return nil, E(CodeInternal, "listing the removed mailboxes failed", err)
	}
	out := listed[:0]
	for _, a := range listed {
		err := s.mailboxWithheld(ctx, a.Account)
		switch {
		case errors.Is(err, ErrTeamWithheld):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, a)
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
		out[i].ProviderBytes += m.ProviderBytes
	}
	return out
}
