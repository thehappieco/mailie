package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Pausing one mailbox's sync (docs/workspaces.md, "Pausing a mailbox's sync";
// platform decision 0054's "stop sync", the owner's "Pausar só esta caixa"):
// its sync stops and its index is kept, read, searched and followed by
// actions, with no time limit, until someone who may resumes it. A recovery
// pauses a mailbox too. It is not a consent: turning sync off, which deletes
// the index, stays a separate act, and ends a pause; resuming a team mailbox
// gives the team's agreement to the current sync text again, as turning it on
// does. Readers, grants and every protection ignore it.

// SyncResumeRequest is resuming a mailbox's sync. Version, for a team
// mailbox, must be the current revision of the sync text, which the owner or
// admin was shown: a resume gives the team's agreement again. A personal or
// an operator mailbox takes none.
type SyncResumeRequest struct {
	Version string `json:"version,omitempty"`
}

// Errors of a pause and a resume.
var (
	errPauseNotOffered = E(CodeNotAuthorized, "pausing one mailbox's sync is not offered on this server", nil)
	errPauseHeld       = E(CodeConflict,
		"this mailbox's sync was turned off and is held until its person turns it on; there is nothing to pause", ErrHeld)
	errPauseSyncOff = E(CodeConflict, "sync is off for this mailbox; there is nothing to pause", ErrSyncOff)
	errTeamPause    = E(CodeNotAuthorized,
		"only an owner or an admin of the team pauses or resumes its mailboxes' sync, on the team's behalf", nil)
	errPersonalPause = E(CodeNotAuthorized, "only its person pauses or resumes a personal mailbox's sync", nil)
	errOperatorPause = E(CodeNotAuthorized,
		"pausing or resuming an operator mailbox's sync needs an unrestricted instance admin key", nil)
	errResumeNoVersion = E(CodeBadRequest,
		"a personal or an operator mailbox is resumed with no text to agree to; leave version out", nil)
	// errNoReaderResume: nobody reads it now. One a suspended member would
	// read once resumed waits for them; one nobody can read again
	// (no_reader) is linked again or turned off.
	errNoReaderResume = E(CodeConflict,
		"nobody in the team reads this mailbox now, so its sync cannot be resumed until someone does; one that "+
			"only a suspended member would read waits for them, and one nobody can be given read on again is "+
			"removed and linked again, or its sync turned off to delete what is still indexed", ErrNoReader)
	errSyncPaused = E(CodeConflict, "sync of this mailbox is paused; resume it to catch up", ErrPaused)
	// errPausedBehind replaces errMessageBusy on a paused mailbox, whose
	// engine does not run to catch up: a stale row, a folder renumbered or
	// gone on the server.
	errPausedBehind = E(CodeConflict,
		"this mailbox's sync is paused, and the mail server changed this folder since; resume sync to catch up", nil)
)

// PauseMailboxSync pauses a mailbox's sync: a personal mailbox's person,
// signed in; a team mailbox's owners and admins, signed in, on the team's
// behalf, after the freshness hook (FreshTeamSync); an operator mailbox's
// operator, with an unrestricted instance admin key. Never a key's own act. A
// mailbox whose consent does not stand, or whose sync the operator's
// turn-off holds, has nothing to pause (conflict). Pausing a paused one
// changes nothing. A team mailbox's other readers are told with a notice.
// Whoever does not see the mailbox is told it does not exist. An edition that
// does not offer the pause (Deps.SyncPauseOffered unset) refuses it to
// everyone, not_authorized.
func (s *Service) PauseMailboxSync(ctx context.Context, p Principal, accountID string) (AccountSync, error) {
	if err := refuseDelegated(p); err != nil {
		return AccountSync{}, err
	}
	if _, err := s.pauseSync(ctx, p, accountID, nil); err != nil {
		return AccountSync{}, err
	}
	return s.currentSync(ctx, accountID)
}

// pauseSync is PauseMailboxSync's act, for a request an extension carries
// for a person too (req, recorded in the pause's write, a mailbox paused
// already included), and returns what it changed.
func (s *Service) pauseSync(ctx context.Context, p Principal, accountID string, req *store.Request) (store.MailboxSyncSwitch, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return store.MailboxSyncSwitch{}, err
	}
	if !s.syncPauseOffered {
		return store.MailboxSyncSwitch{}, errPauseNotOffered
	}
	a, check, err := s.pauseAuthority(ctx, p, accountID)
	if err != nil {
		return store.MailboxSyncSwitch{}, err
	}
	switch {
	case !a.SyncPausedAt.IsZero():
		// For whoever may, nothing to do: no hook, no write but a
		// request's record, while it is still paused.
		done, err := s.noOp(ctx, req, a.ID, check, func(tx *sql.Tx) (bool, error) {
			paused, err := store.PausedTx(ctx, tx, a.ID)
			return paused, ignoreGone(err)
		})
		if err != nil || done || req == nil {
			return store.MailboxSyncSwitch{}, err
		}
		// Resumed since it was read: the act goes on, once.
	case !a.SyncHeldAt.IsZero():
		return store.MailboxSyncSwitch{}, errPauseHeld
	}
	if a.OwnerUserID != "" {
		c, err := s.store.SyncConsentOf(ctx, a.OwnerUserID)
		switch {
		case errors.Is(err, store.ErrNoSuchUser), err == nil && c.At == 0:
			return store.MailboxSyncSwitch{}, errPauseSyncOff
		case err != nil:
			return store.MailboxSyncSwitch{}, E(CodeInternal, "reading the person's consent to sync failed", err)
		}
	} else if a.SyncConsent.At == 0 {
		return store.MailboxSyncSwitch{}, errPauseSyncOff
	}
	if teamMailbox(a) {
		// The role was checked read-only (pauseAuthority); the write runs
		// the check again first, which stops an act the answer ended.
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshTeamSync, WorkspaceID: a.WorkspaceID, AccountID: a.ID}); err != nil {
			return store.MailboxSyncSwitch{}, err
		}
	}
	switched, err := s.store.PauseSyncFor(ctx, req, a.ID, p.Actor(), check)
	var se *Error
	switch {
	case errors.As(err, &se):
		return store.MailboxSyncSwitch{}, err
	case requestRefusal(err) != nil:
		return store.MailboxSyncSwitch{}, requestRefusal(err)
	case errors.Is(err, store.ErrNoSuchMailbox):
		return store.MailboxSyncSwitch{}, errNoAccount
	case errors.Is(err, store.ErrPauseHeld):
		return store.MailboxSyncSwitch{}, errPauseHeld
	case errors.Is(err, store.ErrPauseSyncOff):
		return store.MailboxSyncSwitch{}, errPauseSyncOff
	case err != nil:
		return store.MailboxSyncSwitch{}, E(CodeInternal, "pausing the mailbox's sync failed", err)
	}
	if switched.Changed {
		s.log.Info("mailbox sync paused", "account", a.ID, "workspace", a.WorkspaceID, "by", p.Actor(),
			"readers_told", len(switched.Told))
		s.reconcile(a.ID)
	}
	if len(switched.Told) > 0 {
		s.noticesChanged()
	}
	return switched, nil
}

// ResumeMailboxSync ends a mailbox's pause, a person's or a recovery's, by the
// same people, with the same freshness: it syncs again under its consent as
// it stands, catching up from the index it kept, and the pause's notices are
// withdrawn. A team mailbox's resume names the current sync text
// (req.Version) and records the team's agreement to it, by the caller, now,
// as turning it on does, and only while someone reads it (conflict
// otherwise). Resuming one that is not paused changes nothing. Offered in
// every edition, so that a pause never outlives the edition that offered it.
func (s *Service) ResumeMailboxSync(ctx context.Context, p Principal, accountID string, req SyncResumeRequest) (AccountSync, error) {
	if err := refuseDelegated(p); err != nil {
		return AccountSync{}, err
	}
	if _, err := s.resumeSync(ctx, p, accountID, req, nil); err != nil {
		return AccountSync{}, err
	}
	return s.currentSync(ctx, accountID)
}

// resumeSync is ResumeMailboxSync's act, for a request an extension carries
// for a person too (sreq, recorded in the resume's write, a mailbox not
// paused included; a team's agreement stamped with its first moment), and
// returns what it changed.
func (s *Service) resumeSync(ctx context.Context, p Principal, accountID string, req SyncResumeRequest,
	sreq *store.Request,
) (store.MailboxSyncSwitch, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return store.MailboxSyncSwitch{}, err
	}
	a, check, err := s.pauseAuthority(ctx, p, accountID)
	if err != nil {
		return store.MailboxSyncSwitch{}, err
	}
	team := teamMailbox(a)
	version := ""
	switch {
	case team && req.Version != s.consent.Sync:
		return store.MailboxSyncSwitch{}, errNotCurrentText(s.consent.Sync)
	case team:
		version = req.Version
	case req.Version != "":
		return store.MailboxSyncSwitch{}, errResumeNoVersion
	}
	if a.SyncPausedAt.IsZero() {
		done, err := s.noOp(ctx, sreq, a.ID, check, func(tx *sql.Tx) (bool, error) {
			paused, err := store.PausedTx(ctx, tx, a.ID)
			return !paused && err == nil, ignoreGone(err)
		})
		if err != nil || done || sreq == nil {
			return store.MailboxSyncSwitch{}, err
		}
		// Paused since it was read: the act goes on, once.
	}
	full := check
	if team {
		readable := func(tx *sql.Tx) error {
			// As the turn-on's check: what it would store nobody could read.
			ok, err := workspace.HasReaderTx(ctx, tx, a.ID)
			switch {
			case err != nil:
				return E(CodeInternal, "resuming the mailbox's sync failed", err)
			case !ok:
				return errNoReaderResume
			}
			return nil
		}
		if err := s.precheck(ctx, readable); err != nil {
			return store.MailboxSyncSwitch{}, err
		}
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshTeamSync, WorkspaceID: a.WorkspaceID, AccountID: a.ID}); err != nil {
			return store.MailboxSyncSwitch{}, err
		}
		full = func(tx *sql.Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			return readable(tx)
		}
	}
	switched, err := s.store.ResumeSyncFor(ctx, sreq, a.ID, p.Actor(), version, full)
	var se *Error
	switch {
	case errors.As(err, &se):
		return store.MailboxSyncSwitch{}, err
	case requestRefusal(err) != nil:
		return store.MailboxSyncSwitch{}, requestRefusal(err)
	case errors.Is(err, store.ErrNoSuchMailbox):
		return store.MailboxSyncSwitch{}, errNoAccount
	case err != nil:
		return store.MailboxSyncSwitch{}, E(CodeInternal, "resuming the mailbox's sync failed", err)
	}
	if switched.Changed {
		s.log.Info("mailbox sync resumed", "account", a.ID, "workspace", a.WorkspaceID, "by", p.Actor(),
			"version", version)
		s.reconcile(a.ID)
	}
	return switched, nil
}

// pauseAuthority reads the mailbox a pause or a resume is asked of, and
// refuses whoever may not, whatever the mailbox's state: for a team mailbox
// it runs the role check once, read-only (precheck), before the caller
// decides anything about the pause, the hold or the consent. It returns that
// check, which the write runs again first thing in its transaction (nil when
// nothing is re-read there).
func (s *Service) pauseAuthority(ctx context.Context, p Principal, accountID string) (account.Account, func(*sql.Tx) error, error) {
	if s.store == nil {
		return account.Account{}, nil, E(CodeInternal, "this service has no database to pause a mailbox's sync in", nil)
	}
	// not_found for a mailbox the caller does not see: another workspace's,
	// a removed one, a closed team's; a withheld team's as authorizeAccount
	// says.
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		return account.Account{}, nil, err
	}
	switch {
	case a.WorkspaceID == workspace.OperatorID:
		if !isOperator(p) {
			return account.Account{}, nil, errOperatorPause
		}
		return a, nil, nil
	case a.OwnerUserID != "":
		if !p.IsPerson() || p.UserID != a.OwnerUserID {
			return account.Account{}, nil, errPersonalPause
		}
		return a, nil, nil
	}
	if err := requireAPerson(p); err != nil {
		return account.Account{}, nil, err
	}
	check := func(tx *sql.Tx) error {
		// A team withheld is not_authorized and a closed one a conflict;
		// anyone who is no longer an active member does not see it.
		me, err := s.callerTx(ctx, tx, p, a.WorkspaceID)
		var se *Error
		switch {
		case errors.As(err, &se) && se.Code != CodeNotFound:
			return err
		case err != nil:
			return errNoAccount
		case !adminOf(me):
			return errTeamPause
		}
		return nil
	}
	// The role, before anything about the mailbox is decided: a member is
	// refused on a paused, a held or a switched-off mailbox alike, never
	// answered as if nothing had to change.
	if err := s.precheck(ctx, check); err != nil {
		return account.Account{}, nil, err
	}
	return a, check, nil
}
