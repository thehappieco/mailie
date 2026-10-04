package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
)

// Closing a person's account, on their request, is the operator's job: the
// console cannot do it yet. It is two steps, as the privacy policy promises.
// Disabling ends every session and revokes every key the person holds;
// deleting removes their mailboxes and those mailboxes' credentials, their
// sessions, their keys and their invite, in one transaction.
//
// Both are for an unrestricted instance admin key and nobody else. An owner
// signed in to the console administers people, but closing an account is the
// answer to a request that arrives by email and has to be verified first, and
// that is the operator's.

// CloseUserRequest names the person whose account is being closed. Their
// address travels in the body, never in the URL, where every proxy on the way
// would log it.
type CloseUserRequest struct {
	Email string `json:"email"`
	// Force allows disabling or deleting the last active owner, which leaves
	// nobody able to invite people or see the instance's own accounts from
	// the console.
	Force bool `json:"force,omitempty"`
}

// DisabledUser is what disabling a person ended.
type DisabledUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	SessionsEnded int    `json:"sessions_ended"`
	KeysRevoked   int    `json:"keys_revoked"`
}

// DeletedUser is what deleting a person removed. ID is empty when the address
// had no account, only invites.
type DeletedUser struct {
	ID              string `json:"id,omitempty"`
	Email           string `json:"email"`
	AccountsRemoved int    `json:"accounts_removed"`
	SessionsDeleted int    `json:"sessions_deleted"`
	KeysDeleted     int    `json:"keys_deleted"`
	InvitesDeleted  int    `json:"invites_deleted"`
}

// DisableUser switches a person off: every session they have ends, every key
// issued for them is revoked, and every consent attempt they started stops.
// Disabling someone already disabled is not an error.
func (s *Service) DisableUser(ctx context.Context, p Principal, req CloseUserRequest) (DisabledUser, error) {
	user, err := s.closing(ctx, p, req.Email)
	if err != nil {
		return DisabledUser{}, err
	}
	ended, err := s.users.Disable(ctx, user.ID, req.Force)
	if err != nil {
		return DisabledUser{}, fromClosure(err, "disabling the user failed")
	}
	// After the commit rather than in it: the person can no longer start
	// or finish anything, and a listener still open for them would take a
	// redirect nobody may complete.
	if err := s.accounts.StopFlowsBy(ctx, user.ID); err != nil {
		return DisabledUser{}, E(CodeInternal, "the user is disabled, but stopping their pending authorisations failed", err)
	}
	// Their mailboxes stop syncing: a disabled person's mail is not indexed
	// any further. The accounts themselves did not change, so the registry
	// has not told the engine; this does.
	owned, err := s.accounts.Repo().ListVisible(ctx, account.Visibility{UserID: user.ID})
	if err != nil {
		return DisabledUser{}, E(CodeInternal, "the user is disabled, but listing their mailboxes to stop them failed", err)
	}
	for _, a := range owned {
		s.reconcile(a.ID)
	}
	return DisabledUser{
		ID: user.ID, Email: user.Email, SessionsEnded: ended.Sessions, KeysRevoked: ended.Keys,
	}, nil
}

// DeleteUser deletes a person and everything kept about them: the accounts
// they own with those accounts' credentials, folders and everything else
// indexed for them; their sessions; their keys; every invite for their
// address. One transaction, so an interruption leaves the person whole rather
// than half deleted.
//
// An address with no account but with invites — somebody invited who never
// signed up — has those invites deleted.
func (s *Service) DeleteUser(ctx context.Context, p Principal, req CloseUserRequest) (DeletedUser, error) {
	user, err := s.closing(ctx, p, req.Email)
	var missing *Error
	if errors.As(err, &missing) && missing.Code == CodeNotFound {
		return s.deleteInvitesOnly(ctx, req.Email)
	}
	if err != nil {
		return DeletedUser{}, err
	}

	var removed auth.Removed
	accounts, err := s.accounts.RemoveOwner(ctx, user.ID, func(tx *sql.Tx) error {
		// The records of what they sent from a mailbox nobody owns stay
		// with that mailbox, without saying who asked.
		if err := store.ForgetSenderTx(ctx, tx, user.ID); err != nil {
			return err
		}
		var err error
		removed, err = s.users.DeleteTx(ctx, tx, user.ID, req.Force)
		return err
	})
	if err != nil {
		return DeletedUser{}, fromClosure(err, "deleting the user failed")
	}
	s.compact(ctx)
	s.log.Info("user deleted", "user", user.ID, "accounts", accounts,
		"sessions", removed.Sessions, "keys", removed.Keys, "invites", removed.Invites)
	return DeletedUser{
		ID: user.ID, Email: user.Email, AccountsRemoved: accounts,
		SessionsDeleted: removed.Sessions, KeysDeleted: removed.Keys, InvitesDeleted: removed.Invites,
	}, nil
}

func (s *Service) deleteInvitesOnly(ctx context.Context, email string) (DeletedUser, error) {
	email, err := auth.NormalizeEmail(email)
	if err != nil {
		return DeletedUser{}, fromUsers(err, "deleting the invites failed")
	}
	n, err := s.users.DeleteInvites(ctx, email)
	switch {
	case err != nil:
		return DeletedUser{}, E(CodeInternal, "deleting the invites failed", err)
	case n == 0:
		return DeletedUser{}, E(CodeNotFound, "no account and no invite has that address", nil)
	}
	s.scrub(ctx)
	return DeletedUser{Email: email, InvitesDeleted: n}, nil
}

// scrub takes what a deletion just committed out of the database's
// write-ahead log as well, where the rows as they were before would otherwise
// sit until SQLite happened to overwrite them. The deletion has happened
// either way, so a failure is logged rather than returned: the next
// successful scrub, or SQLite's own checkpoints, finish the job.
func (s *Service) scrub(ctx context.Context) {
	if err := s.accounts.Scrub(context.WithoutCancel(ctx)); err != nil {
		s.log.Warn("emptying the write-ahead log after a deletion failed; a later checkpoint will", "err", err)
	}
}

// closing authorises closing an account and finds the person by address.
func (s *Service) closing(ctx context.Context, p Principal, email string) (auth.User, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return auth.User{}, err
	}
	if !p.IsInstance() || len(p.AccountIDs) > 0 {
		return auth.User{}, E(CodeNotAuthorized, "closing an account needs an unrestricted instance admin key", nil)
	}
	email, err := auth.NormalizeEmail(email)
	if err != nil {
		return auth.User{}, fromUsers(err, "reading the user failed")
	}
	user, err := s.users.GetByEmail(ctx, email)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		return auth.User{}, E(CodeNotFound, "no account has that address", err)
	case err != nil:
		return auth.User{}, E(CodeInternal, "reading the user failed", err)
	}
	return user, nil
}

// fromClosure maps a failure closing an account onto the transport
// vocabulary.
func fromClosure(err error, what string) error {
	switch {
	case errors.Is(err, auth.ErrLastOwner):
		return E(CodeConflict, "that is the last active owner; nobody would be left to invite people "+
			"or see the instance's own accounts. Pass force to do it anyway", err)
	case errors.Is(err, auth.ErrUserNotFound):
		return E(CodeNotFound, "no account has that address", err)
	default:
		return E(CodeInternal, what, err)
	}
}
