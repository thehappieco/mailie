package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/store"
)

// Consent to sync.
//
// The privacy policy says Mailie keeps nothing about a person's messages
// until they agree to it in the console, and that turning sync off deletes
// what it kept. Consent is the person's, given once for every mailbox they
// link, in whichever workspace: a mailbox syncs under the consent of whoever
// linked it, and withdrawing deletes the index of every one of them, team
// mailboxes others read included. A mailbox nobody linked — the operator
// workspace's, added with an instance key from the command line — has no
// person to ask, and syncs only once the operator switches it on.

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

// WithdrawSyncConsent takes the signed-in person's consent back: their
// mailboxes stop syncing and everything indexed for them is deleted — the
// messages' metadata, their folders and the events about them — from the
// database files as well as its tables.
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

// InstanceSyncRequest switches sync on or off for a mailbox of the operator
// workspace.
// Enabled is required: switching off deletes the index, which no request
// should do by leaving a field out.
type InstanceSyncRequest struct {
	Enabled *bool `json:"enabled"`
}

// EnableInstanceAccountSync switches sync on or off for a mailbox of the
// operator workspace, recording who switched it on and when. Switching it off
// deletes what was indexed for it.
//
// Only an unrestricted instance admin key: the operator's decision, like
// closing someone's account. A person's mailbox is not_found to it, as every
// mailbox outside the operator workspace is — its linker decides, by
// consenting in the console, and nobody may decide for them.
func (s *Service) EnableInstanceAccountSync(ctx context.Context, p Principal, accountID string, on bool) (AccountSync, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return AccountSync{}, err
	}
	if !p.IsInstance() || len(p.AccountIDs) > 0 {
		return AccountSync{}, E(CodeNotAuthorized,
			"switching sync for an instance account needs an unrestricted instance admin key", nil)
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		return AccountSync{}, err
	}
	if a.OwnerUserID != "" {
		// Unreachable while visibility holds (an instance key sees only the
		// operator's mailboxes, which nobody linked): a person's mailbox is
		// not_found above. Kept as a second wall, the same answer.
		return AccountSync{}, E(CodeNotFound, "no such account", nil)
	}
	changed, err := s.store.SetInstanceSync(ctx, a.ID, on, p.Actor())
	switch {
	case errors.Is(err, store.ErrNotInstanceAccount):
		return AccountSync{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return AccountSync{}, E(CodeInternal, "switching sync failed", err)
	}
	if changed {
		s.log.Info("instance account sync switched", "account", a.ID, "on", on, "by", p.Actor())
		s.reconcile(a.ID)
		if !on {
			s.compact(ctx)
		}
	}
	return s.syncOf(ctx, a.ID, on)
}

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
