package service

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
)

// Storage is what the caller's mailboxes take up in the index, as GET
// /v1/me/storage reports it.
type Storage struct {
	// Mailboxes are the caller's own, by address, each with what is indexed
	// for it. A mailbox never synced, or whose index was deleted (its owner
	// withdrew consent, or the operator switched its sync off), has zeros.
	// One whose account needs signing in again stops syncing but keeps its
	// index, and reports what was indexed before. A disabled person's
	// mailboxes keep their index too, until `user delete` removes it, but
	// nobody is told here: disabling ended their sessions and revoked their
	// keys, and no other credential sees a person's mailboxes.
	Mailboxes []MailboxStorage `json:"mailboxes"`
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
	Email     string `json:"email"`
	// Messages counts the indexed copies still in their folder. A message
	// in several folders — a Gmail message under several labels — counts
	// once in each.
	Messages int64 `json:"messages"`
	// Bytes sums those copies' sizes as the mail server reports them
	// (RFC822.SIZE), counted the same way: the mail's size on the server,
	// not what the index keeps of it, which is its metadata.
	Bytes int64 `json:"bytes"`
}

// StorageTotal is the sum of every mailbox's Messages and Bytes.
type StorageTotal struct {
	Messages int64 `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// Storage reports what the caller's own mailboxes take up.
//
// Whose they are follows the rule of listing accounts: a person's, and for
// an owner also the mailboxes nobody owns; a key restricted to some mailboxes
// sees only those. An instance key answers for the operator, whose mailboxes
// are the ones nobody owns — never a person's, which are theirs to count.
func (s *Service) Storage(ctx context.Context, p Principal) (Storage, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return Storage{}, err
	}
	v := visibility(p)
	if p.IsInstance() {
		v = account.Visibility{Unowned: true}
	}
	all, err := s.accounts.Repo().ListVisible(ctx, v)
	if err != nil {
		return Storage{}, E(CodeInternal, "listing accounts failed", err)
	}
	ids := make([]string, 0, len(all))
	out := Storage{Mailboxes: make([]MailboxStorage, 0, len(all))}
	for _, a := range all {
		if p.MayAccess(a.ID) {
			ids = append(ids, a.ID)
			out.Mailboxes = append(out.Mailboxes, MailboxStorage{AccountID: a.ID, Email: a.Email})
		}
	}
	slices.SortFunc(out.Mailboxes, func(a, b MailboxStorage) int {
		return cmp.Or(strings.Compare(a.Email, b.Email), strings.Compare(a.AccountID, b.AccountID))
	})
	if s.store == nil {
		// A service built without the database's own handle, as some
		// tools build it: nothing it can see is indexed.
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
	if p.IsSession() && p.UserRole == auth.RoleOwner {
		n, err := s.store.DiskBytes()
		if err != nil {
			return Storage{}, E(CodeInternal, "reading the size of the database failed", err)
		}
		out.DatabaseBytes = &n
	}
	return out, nil
}
