package service

import (
	"context"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What each mailbox of a followed workspace holds under usage rule 1
// (store.RuleVersion), for an extension that reports it to the directory its
// workspaces come from: read from the totals the store keeps as a mailbox's
// rows change, never a scan of the rows, beside what a snapshot says of the
// mailbox, in one read. GET /v1/me/storage keeps its own scan (storage.go),
// the reference those totals are checked against. No transport calls it.

// MailboxBytes is what one mailbox of a followed workspace holds.
type MailboxBytes struct {
	ID string
	// Removed is a mailbox kept until PurgeAfter.
	Removed    bool
	PurgeAfter time.Time
	// Pause is why a live mailbox does not sync; empty on a removed one, as
	// a snapshot says it.
	Pause SnapshotPause
	// Pending is a mailbox never authorized: it holds no credential yet,
	// and stops being pending at its first.
	Pending bool
	// Bytes is its figure under the rule: DatedSince and the rest of its
	// message rows and their parts, and Fixed.
	Bytes int64
	// DatedSince is, of Bytes, what its message rows and their parts hold
	// whose internal_date falls on since's UTC day or after it, a date
	// ahead of today included.
	DatedSince int64
	// Fixed is, of Bytes, what its folders rows and open drafts hold, which
	// take no date.
	Fixed int64
}

// WorkspaceBytes is what every mailbox of a followed workspace holds, read
// at one moment.
type WorkspaceBytes struct {
	WorkspaceID string
	// Mailboxes are live and removed ones, until they are deleted for good,
	// the oldest first, as a snapshot lists them. Empty, never nil, where
	// there are none.
	Mailboxes []MailboxBytes
}

// WorkspaceBytes reads, in one read-only transaction (store.ReadTx), every
// mailbox of a followed workspace, live and removed until deleted for good,
// with its kept totals; since is a moment whose UTC day starts the dated
// count (DatedSince), whatever its hour. It takes no principal. It refuses
// what AccessSnapshot refuses: a workspace not from the directory's source,
// or that nobody knows, not even any more, is not_found, read in the same
// transaction as its mailboxes; a server whose workspaces are local,
// conflict (ErrNotFollowing).
func (s *Service) WorkspaceBytes(ctx context.Context, workspaceID string, since time.Time) (WorkspaceBytes, error) {
	if err := s.following(); err != nil {
		return WorkspaceBytes{}, err
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return WorkspaceBytes{}, E(CodeInternal, "reading the workspace's bytes failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	if err := reportedSource(workspace.GetTx(ctx, tx, workspaceID)); err != nil {
		return WorkspaceBytes{}, err
	}
	read, err := s.store.WorkspaceBytesTx(ctx, tx, workspaceID, store.UTCDay(since), s.teamsWithheld)
	if err != nil {
		return WorkspaceBytes{}, E(CodeInternal, "reading the workspace's bytes failed", err)
	}
	out := WorkspaceBytes{WorkspaceID: workspaceID, Mailboxes: make([]MailboxBytes, 0, len(read))}
	for _, mb := range read {
		m := MailboxBytes{
			ID: mb.AccountID, Removed: mb.Removed, Pause: SnapshotPause(mb.Pause), Pending: mb.Pending,
			Bytes: mb.Bytes, DatedSince: mb.DatedSince, Fixed: mb.Fixed,
		}
		if mb.Removed {
			m.PurgeAfter = time.Unix(mb.PurgeAfter, 0).UTC()
		}
		out.Mailboxes = append(out.Mailboxes, m)
	}
	return out, nil
}

// FollowedWorkspaceBytes is what each followed workspace holds under usage
// rule 1, by workspace: the kept totals of every mailbox of it, live and
// removed alike until deleted for good, summed in one statement; 0 for one
// that holds nothing. It takes no principal. A server whose workspaces are
// local follows none, and has none to say.
func (s *Service) FollowedWorkspaceBytes(ctx context.Context) (map[string]int64, error) {
	if s.store == nil {
		return map[string]int64{}, nil
	}
	out, err := s.store.FollowedWorkspaceBytes(ctx)
	if err != nil {
		return nil, E(CodeInternal, "reading the followed workspaces' bytes failed", err)
	}
	return out, nil
}
