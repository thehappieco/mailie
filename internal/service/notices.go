package service

import (
	"context"
	"errors"
	"sync"

	"github.com/thehappieco/mailie/internal/store"
)

// A person's notices (docs/workspaces.md, "Removed mailboxes").
//
// When a mailbox is removed, or a team mailbox's sync is turned off — by an
// owner or an admin, or by the person its consent was still bound to — every
// person who read it then, but whoever did it, is told, with a notice they
// see in the console until they dismiss it, or until its journal entry goes
// (store.MailboxJournalRetention, 90 days). A notice names the mailbox by its
// address while it is kept, and by a masked hint of it ("a…@example.com")
// once it is deleted for good, and says where it stands now: removed, until
// when it is recovered; deleted; or live again. A recovery withdraws the
// notices of the removal it undoes. A notice is a person's: a key has none.

// PersonNotice is one of a person's notices.
type PersonNotice struct {
	// ID names it, to dismiss it.
	ID string `json:"id"`
	// Kind is what happened: "removed", a mailbox removed; "sync_off", a
	// team mailbox's sync turned off, its index deleted.
	Kind string `json:"kind"`
	// WorkspaceID and WorkspaceName are where the mailbox was: the name is
	// absent for a personal workspace, and once the team is gone.
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	AccountID     string `json:"account_id"`
	// Mailbox is the mailbox's address while it is kept, and a masked hint
	// of it, or its name, once it is deleted for good.
	Mailbox string `json:"mailbox"`
	// At is when it happened, in unix seconds.
	At int64 `json:"at"`
	// State is where the mailbox stands now: "removed", "deleted" or "live".
	State string `json:"state"`
	// RecoverableUntil is, for a removed mailbox, when it is deleted for
	// good: its workspace's owners recover it until then.
	RecoverableUntil int64 `json:"recoverable_until,omitempty"`
}

// Notice kinds and mailbox states, as PersonNotice says them.
const (
	noticeRemoved = "removed"
	noticeSyncOff = "sync_off"

	mailboxRemoved = "removed"
	mailboxDeleted = "deleted"
	mailboxLive    = "live"
)

// Notices lists the signed-in person's notices, the newest first.
func (s *Service) Notices(ctx context.Context, p Principal) ([]PersonNotice, error) {
	if err := s.requirePerson(p); err != nil {
		return nil, err
	}
	if s.store == nil {
		return []PersonNotice{}, nil
	}
	listed, err := s.store.Notices(ctx, p.UserID)
	if err != nil {
		return nil, E(CodeInternal, "listing the notices failed", err)
	}
	out := make([]PersonNotice, 0, len(listed))
	for _, n := range listed {
		out = append(out, presentNotice(n))
	}
	return out, nil
}

func presentNotice(n store.Notice) PersonNotice {
	out := PersonNotice{
		ID: n.Event.ID, Kind: noticeRemoved, WorkspaceID: n.Event.WorkspaceID, WorkspaceName: n.WorkspaceName,
		AccountID: n.Event.AccountID, At: n.Event.At,
	}
	if n.Event.Kind == store.MailboxSyncPaused {
		out.Kind = noticeSyncOff
	}
	switch {
	case !n.Kept:
		out.State = mailboxDeleted
		out.Mailbox = n.Event.Hint
		if out.Mailbox == "" {
			out.Mailbox = n.Event.Label
		}
	case n.RemovedAt != 0:
		out.State, out.Mailbox, out.RecoverableUntil = mailboxRemoved, n.Email, n.PurgeAfter
	default:
		out.State, out.Mailbox = mailboxLive, n.Email
	}
	return out
}

// DismissNotice deletes one of the signed-in person's notices; one they do
// not have is not_found.
func (s *Service) DismissNotice(ctx context.Context, p Principal, id string) error {
	if err := s.requirePerson(p); err != nil {
		return err
	}
	if s.store == nil {
		return errNoNotice
	}
	err := s.store.DismissNotice(ctx, p.UserID, id)
	switch {
	case errors.Is(err, store.ErrNoNotice):
		return errNoNotice
	case err != nil:
		return E(CodeInternal, "dismissing the notice failed", err)
	}
	return nil
}

var errNoNotice = E(CodeNotFound, "no such notice", nil)

// noticesChanged records that somebody may have been given a notice. It is
// called once the change has committed.
func (s *Service) noticesChanged() { s.noticeEpoch.Add(1) }

// noticeWatch tells a person's event stream when they are given a notice: it
// reads their newest notice again only when the notice epoch moved, and
// reports it when it is newer than the last it saw.
type noticeWatch struct {
	s      *Service
	userID string

	mu    sync.Mutex
	epoch int64
	last  int64
}

// newNoticeWatch watches a person's notices from now on, nil for a key: what
// they hold already is what their console reads when it starts.
func (s *Service) newNoticeWatch(ctx context.Context, p Principal) (*noticeWatch, error) {
	if !p.IsSession() || p.UserID == "" || s.store == nil {
		return nil, nil
	}
	w := &noticeWatch{s: s, userID: p.UserID, epoch: s.noticeEpoch.Load()}
	last, err := s.store.LatestNotice(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	w.last = last
	return w, nil
}

// check reports whether the person was given a notice since it last looked.
// The epoch is read before the notices, so a notice committed meanwhile is
// either read now or makes the next check read again.
func (w *noticeWatch) check(ctx context.Context) (bool, error) {
	if w == nil {
		return false, nil
	}
	epoch := w.s.noticeEpoch.Load()
	w.mu.Lock()
	defer w.mu.Unlock()
	if epoch == w.epoch {
		return false, nil
	}
	latest, err := w.s.store.LatestNotice(ctx, w.userID)
	if err != nil {
		return false, err
	}
	w.epoch = epoch
	if latest <= w.last {
		return false, nil
	}
	w.last = latest
	return true, nil
}
