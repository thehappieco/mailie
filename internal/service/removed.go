package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Removed mailboxes (docs/workspaces.md, "Removed mailboxes").
//
// Removing a mailbox keeps it for 30 days (RemoveAccount), with everything it
// holds, found by nobody and used by nothing. Within them its workspace's
// owners see it in a list of their own, and recover it or delete it for good;
// after them the hourly sweep deletes it for good. Who:
//
//   - a personal mailbox: its person;
//   - a team mailbox: an active owner of the team, never an admin or a
//     member, who are told it does not exist; nothing is recovered in a
//     closed team, nor while nobody who could read it is left (someone
//     suspended on the server, who would read it once resumed, counts).
//     Its id alone shows to the team's admins, in the last_reader_of of a
//     member it keeps protecting (ListMembers), so that the protection is
//     said before it refuses a change of their people;
//   - an operator mailbox: the operator, an unrestricted instance admin key.
//
// The operator also lists and deletes for good any workspace's removed
// mailboxes, to erase one when its person asks them to, and never recovers
// one of a person or a team: that is their owners' to decide. A recovered
// mailbox comes back with its credentials, its index, its key, its consent
// and its readers who still qualify, and nothing a key held; its sync
// paused, where its consent stands, until someone who may resumes it
// (ResumeMailboxSync), which for a team records the team's agreement again,
// and it then catches up from the index it kept, or indexes the last 90
// days when it kept none; one whose consent is off comes back off, and a
// personal one the operator turned off held.

// RemovedMailbox is a removed mailbox, as its owners see it.
type RemovedMailbox struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider"`
	// RemovedAt is when it was removed, and RemovedBy who removed it:
	// "usr_…", "key:<prefix>" or "cli", absent once that person is deleted.
	RemovedAt int64  `json:"removed_at"`
	RemovedBy string `json:"removed_by,omitempty"`
	// PurgeAfter is when it is deleted for good, in unix seconds: it is
	// recovered until then.
	PurgeAfter int64 `json:"purge_after"`
	// IndexKept is an index it keeps, which a recovery catches up from;
	// false once its sync was turned off, which deleted it.
	IndexKept bool `json:"index_kept"`
	// Readers is how many people would read it again once recovered.
	Readers int `json:"readers"`
	// NoReader is a team mailbox nobody would read once recovered: it is not
	// recovered, but deleted for good and linked again. One that only a
	// person suspended on the server would read, once resumed, is not: it
	// waits for them, recovered paused, and resumed only once someone reads
	// it.
	NoReader bool `json:"no_reader"`
}

// PurgeAccountRequest names the mailbox again, as a guard against deleting
// the wrong one for good: Confirm must repeat its id.
type PurgeAccountRequest struct {
	Confirm string `json:"confirm"`
}

// Errors of removed mailboxes.
var (
	errRemovedOwnersOnly = E(CodeNotAuthorized,
		"only an owner of the workspace sees, recovers and deletes for good its removed mailboxes", nil)
	errOperatorNeverRecovers = E(CodeNotAuthorized,
		"the operator deletes a person's or a team's removed mailbox for good, and never recovers it; "+
			"its owners do", nil)
	errNotRemoved = E(CodeConflict, "this mailbox is not removed; remove it first", nil)
)

// errRemovedDuplicate is an address linked again where it is kept removed.
func errRemovedDuplicate(err error) error {
	until := ""
	var dup *account.RemovedDuplicateError
	if errors.As(err, &dup) && !dup.PurgeAfter.IsZero() {
		until = " until " + dup.PurgeAfter.UTC().Format(time.DateOnly)
	}
	return E(CodeConflict, "that address was removed from this workspace and is kept"+until+
		"; an owner can recover it, or delete it for good to link it again", err)
}

// ListRemovedMailboxes lists a workspace's removed mailboxes, the oldest
// removal first: for its owners — a personal workspace's person, a team's
// active owners — and for the operator, any workspace's.
func (s *Service) ListRemovedMailboxes(ctx context.Context, p Principal, workspaceID string) ([]RemovedMailbox, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	scope, err := s.removedScope(ctx, p, strings.TrimSpace(workspaceID))
	if err != nil {
		return nil, err
	}
	listed, err := s.accounts.Repo().ListRemoved(ctx, scope)
	if err != nil {
		return nil, E(CodeInternal, "listing the removed mailboxes failed", err)
	}
	out := make([]RemovedMailbox, 0, len(listed))
	for _, ra := range listed {
		out = append(out, RemovedMailbox{
			ID: ra.ID, WorkspaceID: ra.WorkspaceID, Email: ra.Email, DisplayName: ra.DisplayName,
			Provider: ra.ProviderName(), RemovedAt: ra.RemovedAt.Unix(), RemovedBy: ra.RemovedBy,
			PurgeAfter: ra.PurgeAfter.Unix(), IndexKept: ra.IndexKept, Readers: ra.Readers, NoReader: ra.NoReader,
		})
	}
	return out, nil
}

// removedScope is whose removed mailboxes the caller lists in a workspace: an
// owner signed in, a member of it; the operator, any workspace's. Everyone
// else is refused as for the workspace itself.
func (s *Service) removedScope(ctx context.Context, p Principal, workspaceID string) (account.RemovedScope, error) {
	if workspaceID == "" || s.workspaces == nil {
		return account.RemovedScope{}, errNoWorkspace
	}
	if isOperator(p) {
		if _, err := s.workspaces.Get(ctx, workspaceID); err != nil {
			return account.RemovedScope{}, fromWorkspace(err, "reading the workspace failed")
		}
		return account.RemovedScope{Operator: true, Workspace: workspaceID}, nil
	}
	if !p.IsSession() {
		return account.RemovedScope{}, errRemovedOwnersOnly
	}
	w, m, err := s.workspaceOf(ctx, p, workspaceID)
	if err != nil {
		return account.RemovedScope{}, err
	}
	if m.Role != workspace.RoleOwner {
		return account.RemovedScope{}, errRemovedOwnersOnly
	}
	return account.RemovedScope{UserID: p.UserID, Workspace: w.ID}, nil
}

// RecoverAccount recovers a removed mailbox within its 30 days, for its
// workspace's owners, and returns it: live again, its sync paused until
// someone who may resumes it, or off, or held, as the recovery found it. The
// engine is told, so that it keeps the paused mailbox's interactive
// connection between calls. Nothing a key held on it comes back. Refused,
// conflict, in a closed team, once its 30 days are over, and for a team
// mailbox nobody who could read it is left to read.
func (s *Service) RecoverAccount(ctx context.Context, p Principal, id string) (Account, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return Account{}, err
	}
	a, err := s.removedFor(ctx, p, id, false)
	if err != nil {
		return Account{}, err
	}
	recovered, err := s.accounts.Recover(ctx, id, p.Actor(), func(tx *sql.Tx) error {
		return s.ownsRemovedTx(ctx, tx, p, a)
	})
	if err != nil {
		return Account{}, fromRemoved(err, "recovering the mailbox failed")
	}
	s.accessChanged()
	s.log.Info("removed mailbox recovered", "account", a.ID, "workspace", a.WorkspaceID, "by", p.Actor(),
		"paused", !recovered.SyncPausedAt.IsZero(), "held", !recovered.SyncHeldAt.IsZero())
	s.reconcile(a.ID)
	return s.present(ctx, p, recovered)[0], nil
}

// PurgeAccount deletes a removed mailbox for good, before its 30 days are
// over: for its workspace's owners, and for the operator, any workspace's.
// The request repeats its id in Confirm, or nothing is deleted. A live
// mailbox is removed first (conflict). A team's owner is asked about first
// (FreshPurge), never the operator.
func (s *Service) PurgeAccount(ctx context.Context, p Principal, id string, req PurgeAccountRequest) error {
	if req.Confirm != id {
		return E(CodeBadRequest, "deleting a mailbox for good needs its id repeated in confirm; nothing was deleted", nil)
	}
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return err
	}
	a, err := s.removedFor(ctx, p, id, true)
	if CodeOf(err) == CodeNotFound {
		if _, live := s.authorizeAccount(ctx, p, auth.ScopeAdmin, id, needCard); live == nil {
			return errNotRemoved
		}
	}
	if err != nil {
		return err
	}
	if teamMailbox(a) && !isOperator(p) {
		// An owner deleting a team's mailbox before its 30 days are over:
		// refused first, read only, in a team withheld or closed, as the
		// write would; then asked about, outside the write, which checks
		// again that they own it, in a team that is open. The operator, who
		// deletes a closed team's too, and the sweep are nobody signed in.
		if err := s.removedTeamOpen(ctx, a); err != nil {
			return err
		}
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshPurge, WorkspaceID: a.WorkspaceID, AccountID: a.ID}); err != nil {
			return err
		}
	}
	err = s.accounts.Purge(ctx, id, p.Actor(), func(tx *sql.Tx) error { return s.ownsRemovedTx(ctx, tx, p, a) })
	if err != nil {
		return fromRemoved(err, "deleting the mailbox failed")
	}
	s.log.Info("removed mailbox deleted for good", "account", a.ID, "workspace", a.WorkspaceID, "by", p.Actor())
	s.forgetMoves([]string{a.ID})
	s.compact(ctx)
	return nil
}

// removedFor reads a removed mailbox for a caller who may recover it, or,
// with purge, delete it for good: an operator mailbox's operator; a personal
// one's person; an active owner of a team's; and, to delete one for good,
// the operator, any of them. Everyone else is told it does not exist, an
// admin and a member of its team included; the operator asking to recover a
// person's or a team's is told they never do.
func (s *Service) removedFor(ctx context.Context, p Principal, id string, purge bool) (account.Account, error) {
	if !p.MayAccess(id) {
		return account.Account{}, errNoAccount
	}
	a, err := s.accounts.Repo().GetRemoved(ctx, id)
	switch {
	case errors.Is(err, account.ErrNotFound):
		return account.Account{}, errNoAccount
	case err != nil:
		return account.Account{}, E(CodeInternal, "reading the mailbox failed", err)
	}
	switch {
	case a.WorkspaceID == workspace.OperatorID:
		if isOperator(p) {
			return a, nil
		}
	case isOperator(p):
		if purge {
			return a, nil
		}
		return account.Account{}, errOperatorNeverRecovers
	case !p.IsSession():
	case a.OwnerUserID != "":
		if a.OwnerUserID == p.UserID {
			return a, nil
		}
	case s.workspaces != nil:
		m, err := s.workspaces.Member(ctx, a.WorkspaceID, p.UserID)
		if err == nil && m.Active() && m.Role == workspace.RoleOwner {
			if err := s.mailboxWithheld(ctx, a); err != nil {
				return account.Account{}, err
			}
			return a, nil
		}
	}
	return account.Account{}, errNoAccount
}

// removedTeamOpen refuses, read only, a removed team mailbox's team that the
// write's own check (callerTx) would refuse: one an edition withholds
// (not_authorized) or a closed one (conflict). Read before the freshness
// hook is asked, so that it is asked only of an act the rest of the
// authorization lets through.
func (s *Service) removedTeamOpen(ctx context.Context, a account.Account) error {
	if s.workspaces == nil {
		return nil
	}
	w, err := s.workspaces.Get(ctx, a.WorkspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return errNoAccount
	case err != nil:
		return E(CodeInternal, "reading the workspace failed", err)
	}
	if err := s.teamWithheld(w); err != nil {
		return err
	}
	if !w.ClosedAt.IsZero() {
		return errTeamClosed
	}
	return nil
}

// ownsRemovedTx re-reads, inside the transaction that recovers or deletes a
// removed mailbox, that the caller removedFor admitted still may: its person
// still active, an active owner of its team, which is open; the operator's
// key does not change.
func (s *Service) ownsRemovedTx(ctx context.Context, tx *sql.Tx, p Principal, a account.Account) error {
	switch {
	case isOperator(p):
		return nil
	case a.OwnerUserID != "":
		if s.users != nil {
			if err := s.users.RequireActiveTx(ctx, tx, p.UserID); err != nil {
				return errNoAccount
			}
		}
		return nil
	}
	me, err := s.callerTx(ctx, tx, p, a.WorkspaceID)
	var se *Error
	switch {
	case errors.As(err, &se) && se.Code != CodeNotFound:
		return err
	case err != nil, me.Role != workspace.RoleOwner:
		return errNoAccount
	}
	return nil
}

// fromRemoved maps a failure recovering or deleting a removed mailbox onto
// the transport vocabulary.
func fromRemoved(err error, what string) error {
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, account.ErrNotFound):
		return E(CodeNotFound, "no such account", err)
	case errors.Is(err, account.ErrNotRemoved):
		return errNotRemoved
	case errors.Is(err, account.ErrRemovalOver):
		return E(CodeConflict, "this mailbox's 30 days are over: it is being deleted for good, and is no longer recovered", err)
	case errors.Is(err, account.ErrNoReaderLeft):
		return E(CodeConflict, "nobody in the team who could read this mailbox is left, and read passes only from "+
			"someone who reads it; delete it for good and link it again", err)
	case errors.Is(err, workspace.ErrClosed):
		return E(CodeConflict, errTeamClosed.Message, err)
	default:
		return E(CodeInternal, what, err)
	}
}

// SweepRemovedMailboxes deletes for good every removed mailbox whose 30 days
// are over, never one early, whatever its workspace, a closed team's
// included, journaled as the daemon's; and the mailbox journal's entries
// older than its retention, told when the next sweep is (every), so none
// outlives it by waiting for it. It reports how many mailboxes and how many
// entries it deleted: what the daemon's hourly housekeeping runs, at start
// too, so that a database restored from a backup deletes at once what is
// overdue.
func (s *Service) SweepRemovedMailboxes(ctx context.Context, every time.Duration) (mailboxes, entries int, err error) {
	now := s.now()
	purged, err := s.accounts.PurgeExpired(ctx, now, nil)
	if len(purged) > 0 {
		s.log.Info("removed mailboxes deleted for good after their 30 days", "accounts", purged)
		s.forgetMoves(purged)
		s.compact(ctx)
	}
	if err != nil {
		return len(purged), 0, err
	}
	if s.store == nil {
		return len(purged), 0, nil
	}
	n, err := s.store.SweepMailboxEvents(ctx, now.Add(every-store.MailboxJournalRetention).Unix())
	return len(purged), n, err
}
