package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Consent to sync.
//
// The privacy policy says Mailie keeps nothing about a person's messages
// until they agree to it in the console, and that turning sync off deletes
// what it kept. Whose agreement it is depends on whose mailbox it is:
//
//   - a mailbox of a person's personal workspace syncs under that person's
//     consent, given once for all of them; withdrawing it deletes their
//     index, and touches no team's mailbox;
//   - a team mailbox syncs under its workspace's consent, which an owner or
//     an admin of the team gives on the team's behalf, to the current sync
//     text, when linking it or later, and any of them withdraws, deleting
//     its index for everyone who reads it (SetMailboxSync); a consent given
//     to an earlier text keeps it syncing, as a person's does;
//   - an operator mailbox, added with an instance key from the command line,
//     has no person to ask, and syncs only once the operator switches it on.
//
// A team mailbox whose consent the upgrade to this release copied from the
// person who linked it stays bound to that person until an owner or an admin
// confirms it at the current text: their withdrawal, or their being disabled
// or deleted, stops it and deletes its index, as the text they agreed to
// promised (store.StopBoundTx).

// DefaultSyncConsentVersion is the revision of the text describing sync that
// a person agrees to when the deployment configures no other
// (MAIL_CONSENT_VERSION_SYNC). A console asking for consent names the
// revision it showed, so an answer given to another text is not taken as
// agreement to the current one.
const DefaultSyncConsentVersion = config.DefaultSyncConsentVersion

// SyncConsent is a person's answer, as the console shows it.
type SyncConsent struct {
	// Consented is whether sync may store the person's mail.
	Consented bool `json:"consented"`
	// Version is the text revision they agreed to; empty when they have
	// not.
	Version string `json:"version,omitempty"`
	// ConsentedAt is when they agreed.
	ConsentedAt int64 `json:"consented_at,omitempty"`
	// CurrentVersion is the revision this server asks for. A console
	// compares the two to know whether to ask again. A consent to an earlier
	// revision still lets the person's mailboxes sync: eligibility reads
	// only that they consented (internal/store/eligibility.go).
	CurrentVersion string `json:"current_version"`
}

// SyncConsentRequest is agreeing to sync.
type SyncConsentRequest struct {
	// Version must be the current revision: the text the person was shown.
	Version string `json:"version"`
}

// withdrawTimeout bounds the clean-up after a withdrawal has committed:
// compacting the full-text index and emptying the WAL. The deletion itself
// has already happened by then.
const withdrawTimeout = 2 * time.Minute

// SyncConsent reports whether the person signed in agreed to sync. A key has
// no person to answer for.
func (s *Service) SyncConsent(ctx context.Context, p Principal) (SyncConsent, error) {
	if err := s.requirePerson(p); err != nil {
		return SyncConsent{}, err
	}
	c, err := s.store.SyncConsentOf(ctx, p.UserID)
	if err != nil {
		return SyncConsent{}, fromConsent(err, "reading the sync consent failed")
	}
	return s.presentConsent(c), nil
}

// GrantSyncConsent records that the signed-in person agreed to sync, and
// lets their mailboxes start.
//
// A signed-in person and nobody else: agreeing to a privacy policy is
// something a person does at a keyboard, not something a key does on their
// behalf.
func (s *Service) GrantSyncConsent(ctx context.Context, p Principal, version string) (SyncConsent, error) {
	if err := requireSession(p); err != nil {
		return SyncConsent{}, err
	}
	if version != s.consent.Sync {
		// A console showing an older text, or none: the person has not
		// seen what they would be agreeing to.
		return SyncConsent{}, errNotCurrentText(s.consent.Sync)
	}
	c, owned, err := s.store.GrantSyncConsent(ctx, p.UserID, version)
	if err != nil {
		return SyncConsent{}, fromConsent(err, "recording the sync consent failed")
	}
	s.log.Info("sync consent given", "user", p.UserID, "version", version, "accounts", len(owned))
	s.reconcile(owned...)
	return s.presentConsent(c), nil
}

// WithdrawSyncConsent takes the signed-in person's consent back: the
// mailboxes of their personal workspace stop syncing and everything indexed
// for them is deleted — the messages' metadata, their folders and the events
// about them — from the database files as well as its tables, and their
// pauses end. So does a team mailbox whose consent is still bound to theirs;
// any other team mailbox is the team's, and carries on.
//
// The withdrawal and the deletion are one transaction, and the engine
// re-checks consent inside each of its own, so nothing it was in the middle
// of storing survives. Withdrawing when there was no consent deletes
// whatever a previous withdrawal might have missed, and is not an error.
func (s *Service) WithdrawSyncConsent(ctx context.Context, p Principal) (SyncConsent, error) {
	if err := requireSession(p); err != nil {
		return SyncConsent{}, err
	}
	owned, err := s.store.WithdrawSyncConsent(ctx, p.UserID)
	if err != nil {
		return SyncConsent{}, fromConsent(err, "withdrawing the sync consent failed")
	}
	s.log.Info("sync consent withdrawn; index deleted", "user", p.UserID, "accounts", len(owned))
	s.reconcile(owned...)
	if len(owned) > 0 {
		// A team mailbox whose consent was bound to theirs tells its
		// other readers.
		s.noticesChanged()
	}
	s.compact(ctx)
	return s.presentConsent(store.SyncConsent{}), nil
}

// Consent to actions.
//
// Changing a mailbox — marking a message read, starring it, archiving,
// moving, putting it in the trash — is a use of it the sync consent does not
// cover, and the privacy policy promises to ask before a new use. So it has a
// consent of its own: a person's, given once for every mailbox they own, in
// the console. Nothing is deleted when it is withdrawn, because acting keeps
// nothing beyond what the index already holds; actions simply stop.

// DefaultActionsConsentVersion is the revision of the text describing actions
// on messages that a person agrees to when the deployment configures no
// other (MAIL_CONSENT_VERSION_ACTIONS).
const DefaultActionsConsentVersion = config.DefaultActionsConsentVersion

// ActionsConsent is a person's answer, as the console shows it: the same
// shape as SyncConsent.
type ActionsConsent struct {
	// Consented is whether Mailie may change the person's mailboxes when
	// they ask it to.
	Consented bool `json:"consented"`
	// Version is the text revision they agreed to; empty when they have
	// not.
	Version string `json:"version,omitempty"`
	// ConsentedAt is when they agreed.
	ConsentedAt int64 `json:"consented_at,omitempty"`
	// CurrentVersion is the revision this server asks for; until Version
	// matches it, actions are refused.
	CurrentVersion string `json:"current_version"`
	// WithdrawnAt is when the person last took this consent back, while
	// they have not given it again: absent for a person who consents, who
	// never answered, or whose withdrawal came before the server recorded
	// one. An edition that asks only the first time reads it to leave a
	// person who said no alone.
	WithdrawnAt int64 `json:"withdrawn_at,omitempty"`
}

// ActionsConsentRequest is agreeing to actions.
type ActionsConsentRequest struct {
	// Version must be the current revision: the text the person was shown.
	Version string `json:"version"`
}

// ActionsConsent reports whether the person signed in allowed actions. A key
// has no person to answer for.
func (s *Service) ActionsConsent(ctx context.Context, p Principal) (ActionsConsent, error) {
	if err := s.requirePerson(p); err != nil {
		return ActionsConsent{}, err
	}
	c, err := s.store.ActionsConsentOf(ctx, p.UserID)
	if err != nil {
		return ActionsConsent{}, fromConsent(err, "reading the actions consent failed")
	}
	return s.presentActionsConsent(c), nil
}

// GrantActionsConsent records that the signed-in person allows Mailie to
// change their mailboxes when they ask. A signed-in person and nobody else,
// as with sync.
func (s *Service) GrantActionsConsent(ctx context.Context, p Principal, version string) (ActionsConsent, error) {
	if err := requireSession(p); err != nil {
		return ActionsConsent{}, err
	}
	if version != s.consent.Actions {
		return ActionsConsent{}, errNotCurrentText(s.consent.Actions)
	}
	c, err := s.store.GrantActionsConsent(ctx, p.UserID, version)
	if err != nil {
		return ActionsConsent{}, fromConsent(err, "recording the actions consent failed")
	}
	s.log.Info("actions consent given", "user", p.UserID, "version", version)
	return s.presentActionsConsent(c), nil
}

// WithdrawActionsConsent takes the signed-in person's consent to actions
// back. Every action checks it when it is accepted and again before each
// command that changes the mailbox, so once this returns no further change
// is sent — not even by an action already waiting for the connection or
// part-way through its folders; a command already on the wire finishes.
// Nothing is deleted.
func (s *Service) WithdrawActionsConsent(ctx context.Context, p Principal) (ActionsConsent, error) {
	if err := requireSession(p); err != nil {
		return ActionsConsent{}, err
	}
	c, err := s.store.WithdrawActionsConsent(ctx, p.UserID)
	if err != nil {
		return ActionsConsent{}, fromConsent(err, "withdrawing the actions consent failed")
	}
	s.log.Info("actions consent withdrawn", "user", p.UserID)
	return s.presentActionsConsent(c), nil
}

func (s *Service) presentActionsConsent(c store.ActionsConsent) ActionsConsent {
	out := ActionsConsent{CurrentVersion: s.consent.Actions, WithdrawnAt: c.WithdrawnAt}
	if c.At != 0 {
		out.Consented = true
		out.Version = c.Version
		out.ConsentedAt = c.At
	}
	return out
}

// Consent to sending.
//
// Sending mail from a person's mailbox is a use of it neither the sync
// consent nor the actions consent covers, and the privacy policy promises to
// ask before a new use. So it has a consent of its own: a person's, given
// once for every mailbox they own, in the console. Nothing is deleted when it
// is withdrawn — a send keeps nothing of the message — and sends simply stop,
// including one that has not yet connected to the submission server.

// DefaultSendConsentVersion is the revision of the text describing sending
// that a person agrees to when the deployment configures no other
// (MAIL_CONSENT_VERSION_SEND).
const DefaultSendConsentVersion = config.DefaultSendConsentVersion

// SendConsent is a person's answer, as the console shows it: the same shape
// as the other consents.
type SendConsent struct {
	// Consented is whether Mailie may send mail from the person's mailboxes
	// when they ask it to.
	Consented bool `json:"consented"`
	// Version is the text revision they agreed to; empty when they have
	// not.
	Version string `json:"version,omitempty"`
	// ConsentedAt is when they agreed.
	ConsentedAt int64 `json:"consented_at,omitempty"`
	// CurrentVersion is the revision this server asks for; until Version
	// matches it, sends are refused.
	CurrentVersion string `json:"current_version"`
	// WithdrawnAt is when the person last took this consent back, while
	// they have not given it again: absent for a person who consents, who
	// never answered, or whose withdrawal came before the server recorded
	// one. An edition that asks only the first time reads it to leave a
	// person who said no alone.
	WithdrawnAt int64 `json:"withdrawn_at,omitempty"`
}

// SendConsentRequest is agreeing to sending.
type SendConsentRequest struct {
	// Version must be the current revision: the text the person was shown.
	Version string `json:"version"`
}

// SendConsent reports whether the person signed in allowed sending. A key
// has no person to answer for.
func (s *Service) SendConsent(ctx context.Context, p Principal) (SendConsent, error) {
	if err := s.requirePerson(p); err != nil {
		return SendConsent{}, err
	}
	c, err := s.store.SendConsentOf(ctx, p.UserID)
	if err != nil {
		return SendConsent{}, fromConsent(err, "reading the send consent failed")
	}
	return s.presentSendConsent(c), nil
}

// GrantSendConsent records that the signed-in person allows Mailie to send
// mail from their mailboxes when they ask. A signed-in person and nobody
// else, as with the other consents.
func (s *Service) GrantSendConsent(ctx context.Context, p Principal, version string) (SendConsent, error) {
	if err := requireSession(p); err != nil {
		return SendConsent{}, err
	}
	if version != s.consent.Send {
		return SendConsent{}, errNotCurrentText(s.consent.Send)
	}
	c, err := s.store.GrantSendConsent(ctx, p.UserID, version)
	if err != nil {
		return SendConsent{}, fromConsent(err, "recording the send consent failed")
	}
	s.log.Info("send consent given", "user", p.UserID, "version", version)
	return s.presentSendConsent(c), nil
}

// WithdrawSendConsent takes the signed-in person's consent to sending back.
// Every send checks it when it is accepted and again right before it connects
// to the submission server, so once this returns no send of theirs connects —
// not even one already accepted and waiting; one already talking to the
// server finishes. Nothing is deleted.
func (s *Service) WithdrawSendConsent(ctx context.Context, p Principal) (SendConsent, error) {
	if err := requireSession(p); err != nil {
		return SendConsent{}, err
	}
	c, err := s.store.WithdrawSendConsent(ctx, p.UserID)
	if err != nil {
		return SendConsent{}, fromConsent(err, "withdrawing the send consent failed")
	}
	s.log.Info("send consent withdrawn", "user", p.UserID)
	return s.presentSendConsent(c), nil
}

func (s *Service) presentSendConsent(c store.SendConsent) SendConsent {
	out := SendConsent{CurrentVersion: s.consent.Send, WithdrawnAt: c.WithdrawnAt}
	if c.At != 0 {
		out.Consented = true
		out.Version = c.Version
		out.ConsentedAt = c.At
	}
	return out
}

// MailboxSyncRequest switches sync on or off for a mailbox on its own
// consent: a team mailbox's, or an operator mailbox's. Enabled is required:
// switching off deletes the index, which no request should do by leaving a
// field out. Version, to switch a team mailbox's on, must be the current
// revision of the sync text, which the owner or admin was shown; an operator
// mailbox takes none. For a personal mailbox, only its person switches it on,
// with no version, and only to lift the operator's hold.
type MailboxSyncRequest struct {
	Enabled *bool  `json:"enabled"`
	Version string `json:"version,omitempty"`
}

// SetMailboxSync switches sync on or off for a mailbox on its own consent,
// recording who switched it on, when and to which revision. Switching it off
// deletes what was indexed for it, for everyone who read it, and tells each
// of them but whoever switched it off with a notice. Switching on records the
// agreement and leaves a pause, which a resume ends (ResumeMailboxSync);
// switching off ends it, as every turn-off does.
//
//   - A team mailbox: an owner or an admin of the team signed in, giving or
//     withdrawing the team's consent; on, to the current sync text, and only
//     while someone can read it. Turning on a mailbox whose consent the
//     upgrade copied from its linker confirms it, which detaches it from that
//     person. A removed team mailbox is turned off by an owner of the team,
//     which deletes the index it keeps, and never on.
//   - An operator mailbox: an unrestricted instance admin key, the
//     operator's decision, like closing someone's account.
//   - A personal mailbox has none: its person decides, by consenting in the
//     console, and nobody may decide for them. Its person turns it on only
//     to lift the hold the operator's turn-off put on it (StopMailboxSync),
//     after which it syncs under their consent as it stands, from no index.
//
// Whoever does not see the mailbox is told it does not exist.
func (s *Service) SetMailboxSync(ctx context.Context, p Principal, accountID string, req MailboxSyncRequest) (AccountSync, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return AccountSync{}, err
	}
	if req.Enabled == nil {
		return AccountSync{}, E(CodeBadRequest, "enabled is required: true or false", nil)
	}
	on := *req.Enabled
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		if !on && CodeOf(err) == CodeNotFound {
			if removed, ok := s.removedTeamMailbox(ctx, p, accountID); ok {
				return s.syncOffRemoved(ctx, p, removed)
			}
		}
		return AccountSync{}, err
	}
	var check func(*sql.Tx) error
	switch {
	case a.WorkspaceID == workspace.OperatorID:
		if !isOperator(p) {
			return AccountSync{}, E(CodeNotAuthorized,
				"switching sync for an operator mailbox needs an unrestricted instance admin key", nil)
		}
		if req.Version != "" {
			return AccountSync{}, E(CodeBadRequest, "an operator mailbox is switched on by the operator, with no text to agree to; leave version out", nil)
		}
	case a.OwnerUserID != "":
		return s.liftPersonalHold(ctx, p, a, on, req.Version)
	default:
		if err := requireSession(p); err != nil {
			return AccountSync{}, err
		}
		if on && req.Version != s.consent.Sync {
			return AccountSync{}, errNotCurrentText(s.consent.Sync)
		}
		if !on {
			req.Version = ""
		}
		check = func(tx *sql.Tx) error {
			me, err := s.callerTx(ctx, tx, p, a.WorkspaceID)
			switch {
			case err != nil:
				return errNoAccount
			case !adminOf(me):
				return errTeamSync
			case !on:
				return nil
			}
			// What it would store nobody could read, and nobody can be
			// given read on it: it can only be removed, or turned off.
			// Refused too for a consent migration 0011 bound to its
			// linker, which stays bound, so that deleting them still
			// deletes the index they alone read.
			readable, err := workspace.HasReaderTx(ctx, tx, a.ID)
			switch {
			case err != nil:
				return E(CodeInternal, "switching sync failed", err)
			case !readable:
				return errNoReaderSync
			}
			return nil
		}
		// On and off alike; the write runs check again.
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshTeamSync, WorkspaceID: a.WorkspaceID, AccountID: a.ID}); err != nil {
			return AccountSync{}, err
		}
		if err := s.precheck(ctx, check); err != nil {
			return AccountSync{}, err
		}
	}
	switched, err := s.store.SetMailboxSync(ctx, a.ID, on, p.Actor(), req.Version, check)
	var se *Error
	switch {
	case errors.As(err, &se):
		return AccountSync{}, err
	case errors.Is(err, store.ErrNoMailboxConsent):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "switching sync failed", err)
	}
	if switched.Changed {
		s.log.Info("mailbox sync switched", "account", a.ID, "workspace", a.WorkspaceID, "on", on,
			"by", p.Actor(), "version", req.Version, "readers_told", len(switched.Told))
		s.reconcile(a.ID)
		if !on {
			s.compact(ctx)
		}
	}
	if len(switched.Told) > 0 {
		s.noticesChanged()
	}
	return s.currentSync(ctx, a.ID)
}

// liftPersonalHold is SetMailboxSync for a personal mailbox: its person turns
// it on to lift the hold the operator's turn-off put on it
// (StopMailboxSync), and nothing else is theirs to switch here: a pause is
// ended by a resume.
func (s *Service) liftPersonalHold(ctx context.Context, p Principal, a account.Account, on bool, version string) (AccountSync, error) {
	if !on || !p.IsSession() || p.UserID != a.OwnerUserID {
		return AccountSync{}, E(CodeBadRequest,
			"a personal mailbox syncs under its person's own consent, which they turn on or off themselves", nil)
	}
	if version != "" {
		return AccountSync{}, E(CodeBadRequest,
			"a personal mailbox syncs under its person's own consent, with no text to agree to here; leave version out", nil)
	}
	lifted, err := s.store.LiftSyncHold(ctx, a.ID, nil)
	switch {
	case errors.Is(err, store.ErrNoSuchMailbox):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "switching sync failed", err)
	}
	if lifted {
		s.log.Info("a personal mailbox's sync turned on again by its person after the operator's turn-off",
			"account", a.ID, "by", p.Actor())
		s.reconcile(a.ID)
	}
	return s.currentSync(ctx, a.ID)
}

// removedTeamMailbox reads a removed team mailbox an active owner of its
// team asks about: the one thing they still do to it is turn its sync off,
// deleting the index it keeps.
func (s *Service) removedTeamMailbox(ctx context.Context, p Principal, id string) (account.Account, bool) {
	if !p.IsSession() || s.workspaces == nil || s.store == nil {
		return account.Account{}, false
	}
	a, err := s.accounts.Repo().GetRemoved(ctx, id)
	if err != nil || a.OwnerUserID != "" || a.WorkspaceID == workspace.OperatorID {
		return account.Account{}, false
	}
	m, err := s.workspaces.Member(ctx, a.WorkspaceID, p.UserID)
	if err != nil || !m.Active() || m.Role != workspace.RoleOwner {
		return account.Account{}, false
	}
	return a, true
}

// syncOffRemoved turns a removed team mailbox's sync off for an owner of its
// team: what it kept indexed is deleted, and a recovery finds none. A team
// withheld or closed is refused first, read only, as the write would; then
// the owner is asked about (FreshRemovedSyncOff), outside the write, which
// checks again that they own it, in a team that is open.
func (s *Service) syncOffRemoved(ctx context.Context, p Principal, a account.Account) (AccountSync, error) {
	if err := s.removedTeamOpen(ctx, a); err != nil {
		return AccountSync{}, err
	}
	if err := s.fresh(ctx, p, FreshAct{Kind: FreshRemovedSyncOff, WorkspaceID: a.WorkspaceID, AccountID: a.ID}); err != nil {
		return AccountSync{}, err
	}
	check := func(tx *sql.Tx) error {
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
	switched, err := s.store.SetMailboxSync(ctx, a.ID, false, p.Actor(), "", check)
	var se *Error
	switch {
	case errors.As(err, &se):
		return AccountSync{}, err
	case errors.Is(err, store.ErrNoMailboxConsent):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "switching sync failed", err)
	}
	s.log.Info("a removed team mailbox's sync turned off; its kept index deleted", "account", a.ID,
		"workspace", a.WorkspaceID, "by", p.Actor(), "changed", switched.Changed)
	s.compact(ctx)
	return AccountSync{State: syncOff}, nil
}

// StopMailboxSync turns a mailbox's sync off and deletes its index, in any
// workspace, live or removed, a closed team's included: the operator's (an
// unrestricted instance admin key), to erase a mailbox's index when asked
// to, or to stop one being abused, whatever its own people do. Nobody else:
// a person turns their own sync off (WithdrawSyncConsent), and a team's
// owners and admins a team mailbox's (SetMailboxSync).
//
// What it does is the store's (StopSync): a mailbox that names no person
// loses its consent, which a team's owners or admins may give again, to the
// current text, as after any turn-off; a personal one keeps its person's
// consent and is held, live or removed, until they turn it on again; a
// pause ends first. Everyone who read it is
// told with a notice, as of any turn-off, and the journal names the
// operator (store.ActorOperator), never a person or a key. Where an edition
// withholds teams it stops a team's mailbox all the same: it uses nothing of
// the team, and deletes what is the operator's to erase.
func (s *Service) StopMailboxSync(ctx context.Context, p Principal, accountID string) (AccountSync, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return AccountSync{}, err
	}
	if !isOperator(p) {
		return AccountSync{}, errOperatorStopsSync
	}
	if s.store == nil {
		return AccountSync{}, E(CodeInternal, "this service has no database to stop a mailbox's sync in", nil)
	}
	a, err := s.accounts.Repo().Get(ctx, accountID)
	removed := false
	if errors.Is(err, account.ErrNotFound) {
		a, err = s.accounts.Repo().GetRemoved(ctx, accountID)
		removed = true
	}
	switch {
	case errors.Is(err, account.ErrNotFound):
		return AccountSync{}, errNoAccount
	case err != nil:
		return AccountSync{}, E(CodeInternal, "reading the mailbox failed", err)
	}
	switched, err := s.store.StopSync(ctx, a.ID, store.ActorOperator)
	switch {
	case errors.Is(err, store.ErrNoSuchMailbox):
		return AccountSync{}, errNoAccount
	case err != nil:
		return AccountSync{}, E(CodeInternal, "stopping the mailbox's sync failed", err)
	}
	s.log.Info("the operator turned a mailbox's sync off and deleted its index", "account", a.ID,
		"workspace", a.WorkspaceID, "by", p.Actor(), "removed", removed, "changed", switched.Changed,
		"readers_told", len(switched.Told))
	if !removed {
		s.reconcile(a.ID)
	}
	s.compact(ctx)
	if len(switched.Told) > 0 {
		s.noticesChanged()
	}
	if removed {
		return AccountSync{State: syncOff}, nil
	}
	return s.currentSync(ctx, a.ID)
}

// currentSync reads an account's sync again, after a change to it.
func (s *Service) currentSync(ctx context.Context, id string) (AccountSync, error) {
	a, err := s.accounts.Repo().Get(ctx, id)
	switch {
	case errors.Is(err, account.ErrNotFound):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "reading the account failed", err)
	}
	enabled, err := s.syncEnabled(ctx, a)
	if err != nil {
		return AccountSync{}, err
	}
	return s.syncOf(ctx, a, enabled)
}

var (
	errOperatorStopsSync = E(CodeNotAuthorized,
		"only the operator, with an unrestricted instance admin key, stops a mailbox's sync this way; a person "+
			"turns their own sync off, and a team's owners and admins a team mailbox's", nil)
	errTeamSync = E(CodeNotAuthorized,
		"only an owner or an admin of the team turns its mailboxes' sync on or off, on the team's behalf", nil)
	errNoReaderSync = E(CodeConflict,
		"nobody in the team can read this mailbox, and read passes only from someone who reads it, so its sync "+
			"cannot be turned on; remove the mailbox and link it again, or turn its sync off to delete what is "+
			"still indexed", nil)
)

// requirePerson guards what answers for a person: a session. A key belongs
// to a workspace and acts as no person.
func (s *Service) requirePerson(p Principal) error {
	if !p.IsSession() || p.UserID == "" {
		return E(CodeNotAuthorized, "this is a person's setting; an API key has no person to answer for", nil)
	}
	return nil
}

// reconcile tells the engine these accounts may have changed eligibility.
func (s *Service) reconcile(ids ...string) {
	if s.sync == nil {
		return
	}
	for _, id := range ids {
		s.sync.Reconcile(id)
	}
}

// compact takes what a deletion of indexed mail just committed out of the
// full-text index's old segments and then out of the WAL. The deletion has
// happened either way, so failures are logged, not returned: the next
// compaction, or SQLite's own merges and checkpoints, finish the job.
func (s *Service) compact(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), withdrawTimeout)
	defer cancel()
	if s.store != nil {
		if err := s.store.CompactFullText(ctx); err != nil {
			s.log.Warn("compacting the full-text index after a deletion failed; a later merge will", "err", err)
		}
	}
	s.scrub(ctx)
}

// syncEnabled reports whether sync is permitted for one account.
func (s *Service) syncEnabled(ctx context.Context, a account.Account) (bool, error) {
	permitted, err := s.syncPermitted(ctx, []account.Account{a})
	if err != nil {
		return false, err
	}
	return permitted[a.ID], nil
}

func (s *Service) syncPermitted(ctx context.Context, accounts []account.Account) (map[string]bool, error) {
	if s.store == nil {
		// A service built without the database's own handle, as some
		// tools build it: nothing can have consented through it.
		return map[string]bool{}, nil
	}
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	permitted, err := s.store.SyncPermitted(ctx, ids)
	if err != nil {
		return nil, E(CodeInternal, "reading whether sync is on failed", err)
	}
	return permitted, nil
}

func (s *Service) presentConsent(c store.SyncConsent) SyncConsent {
	out := SyncConsent{CurrentVersion: s.consent.Sync}
	if c.At != 0 {
		out.Consented = true
		out.Version = c.Version
		out.ConsentedAt = c.At
	}
	return out
}

// errNotCurrentText refuses a consent to another revision than current: the
// console showed another text, or none.
func errNotCurrentText(current string) error {
	return Ef(CodeBadRequest, nil, "consent must name the current text version, %s; reload and review it", current)
}

func fromConsent(err error, what string) error {
	if errors.Is(err, store.ErrNoSuchUser) {
		// The session authenticated a moment ago; a person gone since is
		// answered as a revoked session is.
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	}
	return E(CodeInternal, what, err)
}
