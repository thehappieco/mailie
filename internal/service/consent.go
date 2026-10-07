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

// SyncConsent reports whether the caller agreed to sync. A person's key may
// read it; an instance key has no person to answer for.
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
// about them — from the database files as well as its tables. So does a team
// mailbox whose consent is still bound to theirs; any other team mailbox is
// the team's, and carries on.
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
}

// ActionsConsentRequest is agreeing to actions.
type ActionsConsentRequest struct {
	// Version must be the current revision: the text the person was shown.
	Version string `json:"version"`
}

// ActionsConsent reports whether the caller allowed actions. A person's key
// may read it; an instance key has no person to answer for.
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
	if err := s.store.WithdrawActionsConsent(ctx, p.UserID); err != nil {
		return ActionsConsent{}, fromConsent(err, "withdrawing the actions consent failed")
	}
	s.log.Info("actions consent withdrawn", "user", p.UserID)
	return s.presentActionsConsent(store.ActionsConsent{}), nil
}

func (s *Service) presentActionsConsent(c store.ActionsConsent) ActionsConsent {
	out := ActionsConsent{CurrentVersion: s.consent.Actions}
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
}

// SendConsentRequest is agreeing to sending.
type SendConsentRequest struct {
	// Version must be the current revision: the text the person was shown.
	Version string `json:"version"`
}

// SendConsent reports whether the caller allowed sending. A person's key may
// read it; an instance key has no person to answer for.
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
	if err := s.store.WithdrawSendConsent(ctx, p.UserID); err != nil {
		return SendConsent{}, fromConsent(err, "withdrawing the send consent failed")
	}
	s.log.Info("send consent withdrawn", "user", p.UserID)
	return s.presentSendConsent(store.SendConsent{}), nil
}

func (s *Service) presentSendConsent(c store.SendConsent) SendConsent {
	out := SendConsent{CurrentVersion: s.consent.Send}
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
// mailbox takes none.
type MailboxSyncRequest struct {
	Enabled *bool  `json:"enabled"`
	Version string `json:"version,omitempty"`
}

// SetMailboxSync switches sync on or off for a mailbox on its own consent,
// recording who switched it on, when and to which revision. Switching it off
// deletes what was indexed for it, for everyone who read it.
//
//   - A team mailbox: an owner or an admin of the team signed in, giving or
//     withdrawing the team's consent; on, to the current sync text, and only
//     while someone can read it. Turning on a mailbox whose consent the
//     upgrade copied from its linker confirms it, which detaches it from that
//     person.
//   - An operator mailbox: an unrestricted instance admin key, the
//     operator's decision, like closing someone's account.
//   - A personal mailbox has none: its person decides, by consenting in the
//     console, and nobody may decide for them.
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
		return AccountSync{}, E(CodeBadRequest,
			"a personal mailbox syncs under its person's own consent, which they turn on or off themselves", nil)
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
			me, err := callerTx(ctx, tx, p, a.WorkspaceID)
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
		if err := s.precheck(ctx, check); err != nil {
			return AccountSync{}, err
		}
	}
	changed, err := s.store.SetMailboxSync(ctx, a.ID, on, p.Actor(), req.Version, check)
	var se *Error
	switch {
	case errors.As(err, &se):
		return AccountSync{}, err
	case errors.Is(err, store.ErrNoMailboxConsent):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "switching sync failed", err)
	}
	if changed {
		s.log.Info("mailbox sync switched", "account", a.ID, "workspace", a.WorkspaceID, "on", on,
			"by", p.Actor(), "version", req.Version)
		s.reconcile(a.ID)
		if !on {
			s.compact(ctx)
		}
	}
	enabled, err := s.syncEnabled(ctx, a)
	if err != nil {
		return AccountSync{}, err
	}
	return s.syncOf(ctx, a.ID, enabled)
}

var (
	errTeamSync = E(CodeNotAuthorized,
		"only an owner or an admin of the team turns its mailboxes' sync on or off, on the team's behalf", nil)
	errNoReaderSync = E(CodeConflict,
		"nobody in the team can read this mailbox, and read passes only from someone who reads it, so its sync "+
			"cannot be turned on; remove the mailbox and link it again, or turn its sync off to delete what is "+
			"still indexed", nil)
)

// requirePerson guards what answers for a person: a session, or a key issued
// to act as one.
func (s *Service) requirePerson(p Principal) error {
	if p.UserID == "" {
		return E(CodeNotAuthorized, "this is a person's setting; an instance key has no person to answer for", nil)
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
