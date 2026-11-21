package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Acts an extension runs for a person (docs/workspaces.md, "Acts an
// extension runs for a person").
//
// A person may ask for a change elsewhere, where an extension of the daemon
// takes the request, and the extension carries it here, with no session.
// ActFor is the one way in: it returns the acts an extension may run for
// that person, each the use case a person signed in runs, under the same
// authorization, the same checks in its write and the same freshness hook,
// for a mailbox of the workspace the request names. The principal is a
// person acting through an extension (auth.KindPerson): no session and no
// key. Every exported method that takes a principal refuses it first
// (refuseDelegated); only these acts reach the use cases, each through the
// unexported act a session's method calls too, with the request.
//
// Such a person never passes a step-up and never writes a mailbox key or a
// grant: those are a session's browser's. Each request runs once: the write
// that decides it records it, with what it did, in its own transaction
// (store.Request), and a request that ran answers that first outcome from
// every act, before any check. A narrowing never takes away what was given
// again at or after the second it was first asked (FirstIssued), and a
// widening is stamped with that moment, so the order in which people asked
// decides, not the order in which requests ran. No transport reaches it: it
// is called from Go, by an extension.

// Delegation is a person's request that an extension carries here with no
// session: made and checked elsewhere, run under this server's own rules
// for that person.
type Delegation struct {
	// Extension is the extension's name, 1 to 64 bytes.
	Extension string
	// ID is the request's id, its idempotency key: 1 to 64 of
	// [A-Za-z0-9_.-].
	ID string
	// UserID is the person.
	UserID string
	// WorkspaceID is the workspace an act on a mailbox is for: the mailbox
	// must be in it. Empty for a person's own act (their consents).
	WorkspaceID string
	// FirstIssued is the intent's first moment: a narrowing's bound, and a
	// widening's stamp.
	FirstIssued time.Time
	// KeepUntil is when the request's record goes: at most 31 days ahead.
	KeepUntil time.Time
}

// ActOutcome is what an act did.
type ActOutcome struct {
	// Changed is false when what it asked held already: nothing was written
	// but its record.
	Changed bool
	// Purged is a removal that deleted a link that never connected, for
	// good.
	Purged bool
	// SessionsEnded counts the live sessions a sign-out everywhere ended.
	SessionsEnded int
	// Dismissed counts the notices a dismissal deleted.
	Dismissed int
	// Ran is an outcome read from the request's record: the request ran
	// already, and this is what it did then.
	Ran bool
}

func outcomeOf(out store.RequestOutcome, ran bool) ActOutcome {
	return ActOutcome{
		Changed: out.Changed, Purged: out.Purged, SessionsEnded: out.SessionsEnded, Dismissed: out.Dismissed, Ran: ran,
	}
}

// Consent names one of a person's own consents.
type Consent string

// A person's consents.
const (
	ConsentSync    Consent = "sync"
	ConsentActions Consent = "actions"
	ConsentSend    Consent = "send"
)

// The acts, as a request's record names them: a closed list.
const (
	actPauseSync       = "pause_sync"
	actResumeSync      = "resume_sync"
	actLiftHold        = "lift_hold"
	actTeamSyncOn      = "team_sync_on"
	actTeamSyncOff     = "team_sync_off"
	actRemove          = "remove"
	actRecover         = "recover"
	actPurge           = "purge"
	actRaiseFlags      = "raise_flags"
	actDropFlags       = "drop_flags"
	actRemoveAccess    = "remove_access"
	actAgreeConsent    = "agree_consent"
	actWithdrawConsent = "withdraw_consent"
	actRemovedIndexOff = "removed_index_off"
	actSignOutEvery    = "sign_out_everywhere"
	actDismissNotices  = "dismiss_notices"
)

// PersonActs are what an extension may run for one person's request
// (ActFor). Each act runs the request at most once.
type PersonActs struct {
	s *Service
	d Delegation
	// p is the person acting through the extension.
	p Principal
	// ran is the record of a request that ran already.
	ran *store.RequestOutcome
	// suspended is a person an extension suspended, who may still make their
	// own narrowings.
	suspended bool
}

// Errors of a request an extension carries.
var (
	errBadDelegation = E(CodeBadRequest,
		"a request names its extension (1 to 64 bytes), its id (1 to 64 of A-Z, a-z, 0-9, '_', '.' and '-'), "+
			"its person, its first moment, and when its record goes, at most 31 days ahead", nil)
	errActorInactive = E(CodeNotAuthorized, "this person may no longer act here", ErrActorInactive)
	errItemWorkspace = E(CodeBadRequest,
		"an act on a mailbox names the workspace it is for, and a person's own act names none", nil)
	errAlreadyRemoved = E(CodeConflict, "this mailbox is removed already", ErrAlreadyRemoved)
	errNotTeamMailbox = E(CodeNotAuthorized, "only a team mailbox's sync is switched on or off on the team's behalf", nil)
	errSetSince       = E(CodeConflict,
		"what this would take away was given again since it was asked; nothing was taken away", ErrSetSince)
	errBadRaise = E(CodeBadRequest, "a raise adds flags to what the person holds, and never read", nil)
	errBadDrop  = E(CodeBadRequest, "a drop takes flags away, and adds none", nil)
	errConsent  = E(CodeBadRequest, "a person's consent is sync, actions or send", nil)
	errSignOut  = E(CodeBadRequest, "a sign-out everywhere names the moment it was asked", nil)
	errNotices  = E(CodeBadRequest,
		"a dismissal names 1 to 300 notices, each by its journal id of 1 to 64 bytes", nil)
	// errRecoveryClosed is a removed mailbox deleted for good since: nothing
	// recovers it.
	errRecoveryClosed = E(CodeConflict, "this mailbox was deleted for good, and is no longer recovered", ErrRecoveryOver)
)

// ActFor returns what an extension may run for d's person. The request's
// record is read first: one that ran answers its first outcome from every
// act, before any check. The person must exist and not be disabled here:
// not_authorized (ErrActorInactive) otherwise; a person an extension
// suspended acts only for their own narrowings (withdrawing a consent,
// signing out everywhere).
func (s *Service) ActFor(ctx context.Context, d Delegation) (*PersonActs, error) {
	if s.store == nil || s.users == nil {
		return nil, E(CodeInternal, "this service has no database to run a request in", nil)
	}
	probe := store.Request{Extension: d.Extension, ID: d.ID, UserID: d.UserID, Act: "probe", KeepUntil: d.KeepUntil}
	if err := probe.Check(s.now()); err != nil || d.FirstIssued.IsZero() {
		return nil, errBadDelegation
	}
	acts := &PersonActs{s: s, d: d}
	out, ran, err := s.store.RequestOutcomeOf(ctx, d.Extension, d.ID)
	switch {
	case err != nil:
		return nil, E(CodeInternal, "reading the request's record failed", err)
	case ran:
		acts.ran = &out
		return acts, nil
	}
	u, err := s.users.Get(ctx, d.UserID)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		return nil, errActorInactive
	case err != nil:
		return nil, E(CodeInternal, "reading the person failed", err)
	case u.Disabled && !u.Suspended:
		return nil, errActorInactive
	}
	acts.suspended = u.Suspended
	acts.p = Principal{Kind: auth.KindPerson, UserID: u.ID, UserRole: u.Role, Scope: auth.ScopeAdmin}
	return acts, nil
}

// RequestOutcome reads a request's record: what it did, and whether it ran.
func (s *Service) RequestOutcome(ctx context.Context, extension, id string) (ActOutcome, bool, error) {
	if s.store == nil {
		return ActOutcome{}, false, E(CodeInternal, "this service has no database to read a request in", nil)
	}
	out, ran, err := s.store.RequestOutcomeOf(ctx, extension, id)
	if err != nil {
		return ActOutcome{}, false, E(CodeInternal, "reading the request's record failed", err)
	}
	return outcomeOf(out, ran), ran, nil
}

// SweepRequests deletes the records of the requests whose time to be kept
// is over, and returns how many: what the daemon's hourly housekeeping runs.
func (s *Service) SweepRequests(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	return s.store.SweepRequests(ctx, s.now())
}

// run runs one act of the request: the first outcome of a request that ran;
// a suspended person refused all but their own narrowings; an act on a
// mailbox checked to name one of the request's workspace, live or removed,
// in a workspace that is not closed, before anything is authorized; then
// do, with the request its write records. What committed is what it did:
// an error after its write committed answers the record.
func (a *PersonActs) run(ctx context.Context, act, accountID string, own bool,
	do func(req *store.Request) (store.RequestOutcome, error),
) (ActOutcome, error) {
	if a.ran != nil {
		return outcomeOf(*a.ran, true), nil
	}
	if a.suspended && !own {
		return ActOutcome{}, errActorInactive
	}
	if accountID != "" {
		if err := a.item(ctx, accountID); err != nil {
			return ActOutcome{}, err
		}
	} else if a.d.WorkspaceID != "" {
		return ActOutcome{}, errItemWorkspace
	}
	req := &store.Request{
		Extension: a.d.Extension, ID: a.d.ID, UserID: a.d.UserID, Act: act, WorkspaceID: a.d.WorkspaceID,
		FirstIssued: a.d.FirstIssued, KeepUntil: a.d.KeepUntil, OwnNarrowing: own,
	}
	out, err := do(req)
	if err == nil {
		return outcomeOf(out, false), nil
	}
	var done *store.RequestDone
	if errors.As(err, &done) {
		return outcomeOf(done.Outcome, true), nil
	}
	recorded, ran, rerr := a.s.store.RequestOutcomeOf(context.WithoutCancel(ctx), a.d.Extension, a.d.ID)
	if rerr == nil && ran {
		return outcomeOf(recorded, true), nil
	}
	if accountID != "" && CodeOf(err) == CodeNotFound {
		// A team closed meanwhile hides its mailboxes: said as closed, which
		// an extension tries again once it reopens.
		if cerr := a.open(ctx); errors.Is(cerr, errTeamClosed) {
			return ActOutcome{}, cerr
		}
	}
	return ActOutcome{}, err
}

// item checks that accountID names a mailbox of the request's workspace,
// live or removed, and that the workspace is not closed.
func (a *PersonActs) item(ctx context.Context, accountID string) error {
	if a.d.WorkspaceID == "" {
		return errItemWorkspace
	}
	acc, err := a.s.accounts.Repo().Get(ctx, accountID)
	if errors.Is(err, account.ErrNotFound) {
		acc, err = a.s.accounts.Repo().GetRemoved(ctx, accountID)
	}
	switch {
	case errors.Is(err, account.ErrNotFound):
		return errNoAccount
	case err != nil:
		return E(CodeInternal, "reading the mailbox failed", err)
	case acc.WorkspaceID != a.d.WorkspaceID:
		return errNoAccount
	}
	return a.open(ctx)
}

// open refuses the request's workspace when it is closed (team_closed);
// one nobody knows is not found.
func (a *PersonActs) open(ctx context.Context) error {
	if a.s.workspaces == nil {
		return nil
	}
	w, err := a.s.workspaces.Get(ctx, a.d.WorkspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return errNoAccount
	case err != nil:
		return E(CodeInternal, "reading the workspace failed", err)
	case !w.ClosedAt.IsZero():
		return errTeamClosed
	}
	return nil
}

// SyncNow asks for a pass over the mailbox's folders soon (TriggerSync). It
// writes nothing, so nothing of the request is recorded: asking twice is
// harmless.
func (a *PersonActs) SyncNow(ctx context.Context, accountID string) (ActOutcome, error) {
	if a.ran != nil {
		return outcomeOf(*a.ran, true), nil
	}
	if a.suspended {
		return ActOutcome{}, errActorInactive
	}
	if err := a.item(ctx, accountID); err != nil {
		return ActOutcome{}, err
	}
	if err := a.s.triggerSync(ctx, a.p, accountID); err != nil {
		if CodeOf(err) == CodeNotFound {
			if cerr := a.open(ctx); errors.Is(cerr, errTeamClosed) {
				return ActOutcome{}, cerr
			}
		}
		return ActOutcome{}, err
	}
	return ActOutcome{Changed: true}, nil
}

// PauseSync pauses the mailbox's sync (PauseMailboxSync); one paused
// already is done.
func (a *PersonActs) PauseSync(ctx context.Context, accountID string) (ActOutcome, error) {
	return a.run(ctx, actPauseSync, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		sw, err := a.s.pauseSync(ctx, a.p, accountID, req)
		return store.RequestOutcome{Changed: sw.Changed}, err
	})
}

// ResumeSync ends the mailbox's pause (ResumeMailboxSync); a team mailbox's
// names the current sync text, version, and records the team's agreement at
// the request's first moment. One not paused is done.
func (a *PersonActs) ResumeSync(ctx context.Context, accountID, version string) (ActOutcome, error) {
	return a.run(ctx, actResumeSync, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		sw, err := a.s.resumeSync(ctx, a.p, accountID, SyncResumeRequest{Version: version}, req)
		return store.RequestOutcome{Changed: sw.Changed}, err
	})
}

// LiftHold lifts the hold the operator's turn-off put on the person's own
// personal mailbox (SetMailboxSync on); one not held is done.
func (a *PersonActs) LiftHold(ctx context.Context, accountID string) (ActOutcome, error) {
	return a.run(ctx, actLiftHold, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		acc, err := a.s.authorizeAccount(ctx, a.p, auth.ScopeAdmin, accountID, needCard)
		if err != nil {
			return store.RequestOutcome{}, err
		}
		lifted, err := a.s.liftHold(ctx, a.p, acc, req)
		return store.RequestOutcome{Changed: lifted}, err
	})
}

// TeamSync switches a team mailbox's sync on, to the current sync text
// version, or off, on the team's behalf (SetMailboxSync): on records the
// team's agreement at the request's first moment; off deletes its index for
// every reader, and takes nothing away from an agreement given at or after
// that moment.
func (a *PersonActs) TeamSync(ctx context.Context, accountID string, on bool, version string) (ActOutcome, error) {
	act := actTeamSyncOff
	if on {
		act = actTeamSyncOn
	}
	return a.run(ctx, act, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		if err := a.s.authorize(a.p, auth.ScopeAdmin); err != nil {
			return store.RequestOutcome{}, err
		}
		acc, err := a.s.authorizeAccount(ctx, a.p, auth.ScopeAdmin, accountID, needCard)
		if err != nil {
			// Removed since it was asked: turning it off is what a turn-off
			// asks of a removed team mailbox, its kept index deleted, by an
			// owner of the team; not found to anyone else.
			if !on && CodeOf(err) == CodeNotFound {
				if removed, ok := a.s.removedOwnMailbox(ctx, a.p, accountID); ok && teamMailbox(removed) {
					sw, err := a.s.syncOffRemoved(ctx, a.p, removed, req, false)
					return store.RequestOutcome{Changed: sw.Changed}, err
				}
			}
			return store.RequestOutcome{}, err
		}
		if !teamMailbox(acc) {
			return store.RequestOutcome{}, errNotTeamMailbox
		}
		sw, err := a.s.teamSync(ctx, a.p, acc, on, version, req)
		return store.RequestOutcome{Changed: sw.Changed}, err
	})
}

// Remove removes the mailbox (RemoveAccount): kept 30 days, or, a link that
// never connected, deleted for good at once (Purged). One removed already is
// said as such to whoever may recover it, and not found to anyone else.
func (a *PersonActs) Remove(ctx context.Context, accountID string) (ActOutcome, error) {
	return a.run(ctx, actRemove, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		r, err := a.s.removeAccount(ctx, a.p, accountID, req)
		if CodeOf(err) == CodeNotFound {
			if _, rerr := a.s.removedFor(ctx, a.p, accountID, false); rerr == nil {
				return store.RequestOutcome{}, errAlreadyRemoved
			}
		}
		return store.RequestOutcome{Changed: err == nil, Purged: r.Purged}, err
	})
}

// Recover recovers the removed mailbox (RecoverAccount): live again, its
// sync paused where its consent stands. To whoever may recover it, a
// mailbox live again in the request's workspace (recovered since it was
// asked) is done, with nothing changed; one the workspace's journal shows
// deleted for good is recovery_over (ErrRecoveryOver), as one past its 30
// days is; anything else is not found.
func (a *PersonActs) Recover(ctx context.Context, accountID string) (ActOutcome, error) {
	if a.ran == nil && !a.suspended && CodeOf(a.item(ctx, accountID)) == CodeNotFound && a.ownerHere(ctx) {
		purged, err := a.s.store.PurgedIn(ctx, a.d.WorkspaceID, accountID)
		switch {
		case err != nil:
			return ActOutcome{}, E(CodeInternal, "reading the mailbox's journal failed", err)
		case purged:
			return ActOutcome{}, errRecoveryClosed
		}
	}
	return a.run(ctx, actRecover, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		for range 2 {
			err := a.s.recoverAccount(ctx, a.p, accountID, req)
			if CodeOf(err) != CodeNotFound || !a.ownerHere(ctx) {
				return store.RequestOutcome{Changed: err == nil}, err
			}
			// Live again: recovered since it was asked.
			done, nerr := a.s.noOp(ctx, req, accountID, nil, liveTx(ctx, accountID))
			if nerr != nil || done {
				return store.RequestOutcome{}, nerr
			}
			// Removed again meanwhile: recovered as asked, once more.
		}
		return store.RequestOutcome{}, errNoAccount
	})
}

// Purge deletes the removed mailbox for good (PurgeAccount).
func (a *PersonActs) Purge(ctx context.Context, accountID string) (ActOutcome, error) {
	return a.run(ctx, actPurge, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		err := a.s.purgeAccount(ctx, a.p, accountID, req)
		return store.RequestOutcome{Changed: err == nil}, err
	})
}

// RemovedIndexOff deletes the index a removed mailbox keeps, at the word of
// whoever may: a personal mailbox's person (forgetRemovedIndex: their
// consent, its hold and its removal stay; nothing journaled), an active
// owner of a team's (syncOffRemoved: the team's agreement withdrawn and
// journaled, nobody told, a removed mailbox having no reader; after the
// freshness hook, FreshRemovedSyncOff). It never reaches a live mailbox:
// one recovered since it was asked is done, with nothing changed, to
// whoever may; and no widening gives a removed mailbox's index again, so it
// compares with no moment. Anyone else is told it does not exist.
func (a *PersonActs) RemovedIndexOff(ctx context.Context, accountID string) (ActOutcome, error) {
	return a.run(ctx, actRemovedIndexOff, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		if err := a.s.authorize(a.p, auth.ScopeAdmin); err != nil {
			return store.RequestOutcome{}, err
		}
		for range 2 {
			if removed, ok := a.s.removedOwnMailbox(ctx, a.p, accountID); ok {
				if removed.OwnerUserID != "" {
					forgot, err := a.s.forgetRemovedIndex(ctx, a.p, removed, req)
					return store.RequestOutcome{Changed: forgot}, err
				}
				sw, err := a.s.syncOffRemoved(ctx, a.p, removed, req.Unbounded(), true)
				return store.RequestOutcome{Changed: sw.Changed}, err
			}
			if !a.ownerHere(ctx) {
				break
			}
			// Live again: what it kept is the live mailbox's, never this
			// act's to delete.
			done, err := a.s.noOp(ctx, req, accountID, nil, liveTx(ctx, accountID))
			if err != nil || done {
				return store.RequestOutcome{}, err
			}
		}
		return store.RequestOutcome{}, errNoAccount
	})
}

// liveTx reads, inside a request's write, whether a mailbox is live.
func liveTx(ctx context.Context, accountID string) func(*sql.Tx) (bool, error) {
	return func(tx *sql.Tx) (bool, error) {
		var live bool
		err := tx.QueryRowContext(ctx, `SELECT removed_at = 0 FROM accounts WHERE id = ?`, accountID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return live, err
	}
}

// ownerHere reports whether the person owns the mailboxes of the request's
// workspace: its person, for a personal one; an active owner, for a team.
// Who may recover, delete for good or turn off a removed one there.
func (a *PersonActs) ownerHere(ctx context.Context) bool {
	if a.s.workspaces == nil || a.d.WorkspaceID == "" {
		return false
	}
	w, err := a.s.workspaces.Get(ctx, a.d.WorkspaceID)
	if err != nil {
		return false
	}
	switch w.Kind {
	case workspace.KindPersonal:
		return w.PersonID == a.p.UserID
	case workspace.KindTeam:
		m, err := a.s.workspaces.Member(ctx, w.ID, a.p.UserID)
		return err == nil && m.Active() && m.Role == workspace.RoleOwner
	}
	return false
}

// RaiseFlags gives userID the flags to holds beyond from, what the request
// saw them hold, on a team mailbox (SetAccess): refused unless they hold
// exactly from when it is written, so that it never gives back a flag taken
// away since. to is from with act, send or manage added, never read: read
// comes with a grant, which only a session's browser seals. The flags it
// adds are stamped with the request's first moment.
func (a *PersonActs) RaiseFlags(ctx context.Context, accountID, userID string, from, to GrantFlags) (ActOutcome, error) {
	f, t := from.flags(), to.flags()
	if t.Read != f.Read || !t.Covers(f) || t == f {
		return ActOutcome{}, errBadRaise
	}
	return a.run(ctx, actRaiseFlags, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		_, changed, err := a.s.setAccess(ctx, a.p, accountID, userID, GrantRequest{
			Read: &t.Read, Act: &t.Act, Send: &t.Send, Manage: &t.Manage, From: &from,
		}, req)
		return store.RequestOutcome{Changed: changed}, err
	})
}

// DropFlags takes the flags drop names away from userID's grant on a team
// mailbox (RevokeAccess); dropping read drops act too, and deletes their
// grants. A grant raised at or after the request's first second is left as
// it is (set_since); one that holds none of them is done.
func (a *PersonActs) DropFlags(ctx context.Context, accountID, userID string, drop GrantFlags) (ActOutcome, error) {
	d := drop.flags()
	if !d.Any() {
		return ActOutcome{}, errBadDrop
	}
	return a.run(ctx, actDropFlags, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		changed, err := a.s.revokeAccess(ctx, a.p, accountID, userID, d, req)
		return store.RequestOutcome{Changed: changed}, err
	})
}

// RemoveAccess takes every flag of userID's on a team mailbox away
// (RevokeAccess), as DropFlags does.
func (a *PersonActs) RemoveAccess(ctx context.Context, accountID, userID string) (ActOutcome, error) {
	return a.run(ctx, actRemoveAccess, accountID, false, func(req *store.Request) (store.RequestOutcome, error) {
		changed, err := a.s.revokeAccess(ctx, a.p, accountID, userID, workspace.AllFlags(), req)
		return store.RequestOutcome{Changed: changed}, err
	})
}

// AgreeConsent records the person's agreement to one of their consents at
// version, the current text (Grant*Consent), at the request's first moment;
// the same version agreed again keeps its first date.
func (a *PersonActs) AgreeConsent(ctx context.Context, which Consent, version string) (ActOutcome, error) {
	return a.run(ctx, actAgreeConsent, "", false, func(req *store.Request) (store.RequestOutcome, error) {
		var (
			changed bool
			err     error
		)
		switch which {
		case ConsentSync:
			_, changed, err = a.s.grantSyncConsent(ctx, a.p, version, req)
		case ConsentActions:
			_, changed, err = a.s.grantActionsConsent(ctx, a.p, version, req)
		case ConsentSend:
			_, changed, err = a.s.grantSendConsent(ctx, a.p, version, req)
		default:
			err = errConsent
		}
		return store.RequestOutcome{Changed: changed}, err
	})
}

// WithdrawConsent takes one of the person's consents back
// (Withdraw*Consent): withdrawing sync deletes their index and stops the
// team mailboxes bound to them. A consent agreed at or after the request's
// first second is left as it is (set_since); one off already is done. A
// person an extension suspended still withdraws their own.
func (a *PersonActs) WithdrawConsent(ctx context.Context, which Consent) (ActOutcome, error) {
	return a.run(ctx, actWithdrawConsent, "", true, func(req *store.Request) (store.RequestOutcome, error) {
		var (
			changed bool
			err     error
		)
		switch which {
		case ConsentSync:
			changed, err = a.s.withdrawSyncConsent(ctx, a.p, req)
		case ConsentActions:
			_, changed, err = a.s.withdrawActionsConsent(ctx, a.p, req)
		case ConsentSend:
			_, changed, err = a.s.withdrawSendConsent(ctx, a.p, req)
		default:
			err = errConsent
		}
		return store.RequestOutcome{Changed: changed}, err
	})
}

// SignOutEverywhere ends the person's live sessions that started at or
// before the moment they asked, before, counting them (SessionsEnded), and
// the tickets and links at the provider they started by then; and records
// that moment as their last sign-out everywhere, never moving it back.
// Sessions started after it stay, and keys too. A person an extension
// suspended still signs themselves out.
func (a *PersonActs) SignOutEverywhere(ctx context.Context, before time.Time) (ActOutcome, error) {
	if before.Unix() <= 0 {
		return ActOutcome{}, errSignOut
	}
	return a.run(ctx, actSignOutEvery, "", true, func(req *store.Request) (store.RequestOutcome, error) {
		ended, changed, err := a.s.users.SignOutEverywhereFor(ctx, req, a.p.UserID, before)
		switch {
		case requestRefusal(err) != nil:
			return store.RequestOutcome{}, requestRefusal(err)
		case err != nil:
			return store.RequestOutcome{}, E(CodeInternal, "signing out everywhere failed", err)
		}
		a.s.log.Info("a person signed out everywhere through an extension", "user", a.p.UserID, "sessions", ended)
		return store.RequestOutcome{Changed: changed, SessionsEnded: ended}, nil
	})
}

// DismissNotices deletes the person's notices of the journal entries ids
// names ("mbe_…"), 1 to 300 of them, in one write (Dismissed counts them):
// one they hold no notice of, dismissed already or gone, is done.
func (a *PersonActs) DismissNotices(ctx context.Context, ids []string) (ActOutcome, error) {
	if len(ids) == 0 || len(ids) > store.MaxNoticesDismissed {
		return ActOutcome{}, errNotices
	}
	for _, id := range ids {
		if id == "" || len(id) > 64 {
			return ActOutcome{}, errNotices
		}
	}
	return a.run(ctx, actDismissNotices, "", false, func(req *store.Request) (store.RequestOutcome, error) {
		n, err := a.s.store.DismissNoticesFor(ctx, req, a.p.UserID, ids)
		switch {
		case requestRefusal(err) != nil:
			return store.RequestOutcome{}, requestRefusal(err)
		case err != nil:
			return store.RequestOutcome{}, E(CodeInternal, "dismissing the notices failed", err)
		}
		return store.RequestOutcome{Changed: n > 0, Dismissed: n}, nil
	})
}

// requestRefusal maps what a request's record refuses in its write: nil for
// anything else. A request that ran already passes as it is, for run to
// answer its first outcome.
func requestRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrRequestDone):
		return err
	case errors.Is(err, store.ErrActorInactive):
		return E(CodeNotAuthorized, errActorInactive.Message, errors.Join(ErrActorInactive, err))
	case errors.Is(err, store.ErrNotInWorkspace):
		return E(CodeNotFound, errNoAccount.Message, errors.Join(ErrNotFound, err))
	case errors.Is(err, store.ErrSetSince):
		return E(CodeConflict, errSetSince.Message, errors.Join(ErrSetSince, err))
	}
	return nil
}

// noOp ends a request whose act found nothing to do before its write
// (store.RecordNoOp): in one write, the request's record begun, check run
// again (nil checks nothing) and holds read again; while it holds, the
// request is recorded as having changed nothing. It reports whether it did;
// false with no request, and false when holds no longer does, for the act to
// go on as usual, once.
func (s *Service) noOp(ctx context.Context, req *store.Request, accountID string, check func(*sql.Tx) error,
	holds func(*sql.Tx) (bool, error),
) (bool, error) {
	if req == nil {
		return false, nil
	}
	done, err := s.store.RecordNoOp(ctx, req, accountID, func(tx *sql.Tx) (bool, error) {
		if check != nil {
			if err := check(tx); err != nil {
				return false, err
			}
		}
		return holds(tx)
	})
	var se *Error
	switch {
	case err == nil:
		return done, nil
	case errors.As(err, &se):
		return false, err
	case requestRefusal(err) != nil:
		return false, requestRefusal(err)
	}
	return false, E(CodeInternal, "recording the request failed", err)
}

// ignoreGone is nil for a mailbox gone, whose act then says so in its own
// write.
func ignoreGone(err error) error {
	if errors.Is(err, store.ErrNoSuchMailbox) {
		return nil
	}
	return err
}
