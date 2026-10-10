package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
)

// Closing a person's account, on their request, is two steps, as the privacy
// policy promises. Disabling ends every session, revokes every API key the
// person created, in whichever workspace, and expires the invites they made;
// deleting removes the mailboxes of their personal workspace and of every
// team they were alone in, with those mailboxes' credentials, index and keys,
// their sessions, their name on the keys they created in other teams, their
// invite, the identities they sign in with through a provider with the keys
// pinned for them, and their personal workspace, in one transaction. A team
// mailbox of a team others are in is the team's: closing a person never stops
// or removes it, but for one whose consent to sync migration 0011 copied from
// theirs and nobody has confirmed since, which stops with its index deleted,
// as the text they agreed to promised — and, while someone else reads it,
// only when the caller insists.
//
// Both are for an unrestricted instance admin key — the operator, answering a
// request that arrived by email and was verified — and for an owner of the
// instance signed in to the console, who administers its people, never on
// themselves. A person their teams depend on — a team's last active owner,
// the last person who can read a mailbox of a team that outlives them, or the
// person whose consent a team mailbox someone else reads still syncs under —
// is refused unless the caller insists (force), as the last owner of the
// instance is.

// CloseUserRequest names the person whose account is being closed. Their
// address travels in the body, never in the URL, where every proxy on the way
// would log it.
type CloseUserRequest struct {
	Email string `json:"email"`
	// Force allows disabling or deleting the last active owner of the
	// instance, which leaves nobody able to invite people from the console,
	// and a person their teams depend on: a team can then be left without
	// an owner; a team mailbox without anyone who can read it, which syncs
	// nothing more and is marked so for the team's owners and admins until
	// they remove it; and a team mailbox whose consent to sync was still
	// the person's own (migration 0011) stopped, its index deleted, until an
	// owner or an admin of the team turns its sync on again.
	Force bool `json:"force,omitempty"`
}

// DisabledUser is what disabling a person ended.
type DisabledUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	SessionsEnded int    `json:"sessions_ended"`
	KeysRevoked   int    `json:"keys_revoked"`
	// TeamSyncsStopped are the team mailboxes whose consent to sync was
	// still the person's own (migration 0011): stopped, their index deleted.
	TeamSyncsStopped []string `json:"team_syncs_stopped,omitempty"`
}

// DeletedUser is what deleting a person removed. ID is empty when the address
// had no account, only invites.
type DeletedUser struct {
	ID              string `json:"id,omitempty"`
	Email           string `json:"email"`
	AccountsRemoved int    `json:"accounts_removed"`
	SessionsDeleted int    `json:"sessions_deleted"`
	// KeysDeleted counts the API keys they created: gone with their
	// personal workspace and the teams they were alone in, or revoked and
	// kept, without their name, by a team that stays.
	KeysDeleted    int `json:"keys_deleted"`
	InvitesDeleted int `json:"invites_deleted"`
	// TeamsDeleted counts the teams whose only member they were, deleted
	// with them.
	TeamsDeleted int `json:"teams_deleted"`
	// TeamSyncsStopped are the team mailboxes whose consent to sync was
	// still the person's own (migration 0011): stopped, their index deleted.
	// The mailboxes stay, the team's.
	TeamSyncsStopped []string `json:"team_syncs_stopped,omitempty"`
}

// DisableUser switches a person off: every session they have ends, every API
// key they created is revoked, in whichever workspace, every invite they made expires, and every
// consent attempt they started stops. Their memberships and flags stay, and
// count for nothing while they are off; the mailbox keys sealed to them go
// (docs/key-scheme.md section 12.13), so that on a mailbox that has a key
// they would wait for it again; the mailboxes of their personal
// workspace stop syncing, and so does a team mailbox whose consent was still
// bound to theirs, its index deleted (refused without force while somebody
// else reads it). Other team mailboxes carry on.
// Disabling someone already disabled is not an error.
func (s *Service) DisableUser(ctx context.Context, p Principal, req CloseUserRequest) (DisabledUser, error) {
	user, err := s.closing(ctx, p, req.Email)
	if err != nil {
		return DisabledUser{}, err
	}
	read := s.readBy(ctx, user.ID)
	ended, err := s.users.Disable(ctx, user.ID, req.Force)
	if err != nil {
		return DisabledUser{}, fromClosure(err, "disabling the user failed")
	}
	// Whatever they could read, they no longer can.
	s.accessChanged()
	// After the commit rather than in it: the person can no longer start
	// or finish anything, and a listener still open for them would take a
	// redirect nobody may complete.
	if err := s.accounts.StopFlowsBy(ctx, user.ID); err != nil {
		return DisabledUser{}, E(CodeInternal, "the user is disabled, but stopping their pending authorisations failed", err)
	}
	// Their personal mailboxes stop syncing: nothing is indexed any further
	// under the consent of a person who is off. The accounts themselves did
	// not change, so the registry has not told the engine; this does. A
	// team mailbox whose consent was bound to theirs stopped, and its index
	// went, in the same transaction; a team mailbox they alone read stops
	// too, read by nobody now.
	owned, err := s.accounts.Repo().OwnedBy(ctx, user.ID)
	if err != nil {
		return DisabledUser{}, E(CodeInternal, "the user is disabled, but listing their mailboxes to stop them failed", err)
	}
	for _, a := range owned {
		s.reconcile(a.ID)
	}
	s.reconcile(ended.Stopped...)
	s.reconcile(read...)
	if len(ended.Stopped) > 0 {
		s.log.Info("team mailboxes whose consent to sync was bound to a disabled person stopped; index deleted",
			"user", user.ID, "accounts", ended.Stopped)
		s.forgetMoves(ended.Stopped)
		s.compact(ctx)
	}
	return DisabledUser{
		ID: user.ID, Email: user.Email, SessionsEnded: ended.Sessions, KeysRevoked: ended.Keys,
		TeamSyncsStopped: ended.Stopped,
	}, nil
}

// DeleteUser deletes a person and everything kept about them: the mailboxes
// of their personal workspace, with those mailboxes' credentials, folders and
// everything else indexed for them, since the consent they synced under goes
// with them; their sessions; every invite for their address; the identities
// they signed in with through a provider, and the keys pinned for those
// identities; their personal workspace, and every team they were the only
// member of, with its mailboxes and API keys. Their name goes from what they
// did for others — the grants they gave, what they gave keys, the mailboxes
// they linked, the team consents they gave, the keys they created in a team
// that stays, revoked — which stays. A team mailbox whose consent was still bound to
// theirs stops, its index deleted. One transaction, so an interruption leaves
// the person whole rather than half deleted.
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
	read := s.readBy(ctx, user.ID)
	// Their teams are asked about before anything of theirs goes.
	blocks := func(tx *sql.Tx) error { return s.users.RequireNoBlocksTx(ctx, tx, user.ID, req.Force) }
	accounts, err := s.accounts.RemoveOwner(ctx, user.ID, blocks, func(tx *sql.Tx) error {
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
	s.accessChanged()
	s.reconcile(removed.Stopped...)
	s.reconcile(read...)
	s.forgetMoves(removed.Stopped)
	s.compact(ctx)
	s.log.Info("user deleted", "user", user.ID, "accounts", accounts,
		"sessions", removed.Sessions, "keys", removed.Keys, "invites", removed.Invites, "teams", removed.Teams,
		"identities", removed.Identities, "team_syncs_stopped", len(removed.Stopped))
	return DeletedUser{
		ID: user.ID, Email: user.Email, AccountsRemoved: accounts,
		SessionsDeleted: removed.Sessions, KeysDeleted: removed.Keys, InvitesDeleted: removed.Invites,
		TeamsDeleted: removed.Teams, TeamSyncsStopped: removed.Stopped,
	}, nil
}

// forgetMoves drops what the index remembers of moves on accounts whose index
// a committed transaction deleted.
func (s *Service) forgetMoves(ids []string) {
	if s.store != nil && len(ids) > 0 {
		s.store.ForgetMoves(ids)
	}
}

// readBy lists the mailboxes a person reads, before they are disabled or
// deleted: a team mailbox they were the last to read (a forced closure)
// stops syncing, and the engine hears of it at once rather than at its next
// periodic check. A failure only delays that.
func (s *Service) readBy(ctx context.Context, userID string) []string {
	if s.workspaces == nil {
		return nil
	}
	ids, err := s.workspaces.ReadBy(ctx, userID)
	if err != nil {
		s.log.Warn("listing what a person reads failed; the engine notices on its own", "err", err)
		return nil
	}
	return ids
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

// closing authorises closing an account and finds the person by address: the
// operator, or an owner of the instance signed in, never on themselves.
func (s *Service) closing(ctx context.Context, p Principal, email string) (auth.User, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return auth.User{}, err
	}
	if !isOperator(p) && !isInstanceOwner(p) {
		return auth.User{}, E(CodeNotAuthorized,
			"closing an account needs an owner of this server signed in, or an unrestricted instance admin key", nil)
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
	if p.IsSession() && user.ID == p.UserID {
		return auth.User{}, E(CodeBadRequest,
			"you cannot close your own account here; another owner, or the operator, can", nil)
	}
	return user, nil
}

// isOperator reports whether the caller is the operator: an unrestricted
// instance admin key, what the command line holds.
func isOperator(p Principal) bool {
	return p.IsInstance() && len(p.AccountIDs) == 0 && p.Scope.Covers(auth.ScopeAdmin)
}

// isInstanceOwner reports whether the caller is an owner of the instance,
// signed in: the self-hosted server's own administration. It reaches no
// mailbox by being one.
func isInstanceOwner(p Principal) bool {
	return p.IsSession() && p.UserRole == auth.RoleOwner
}

// fromClosure maps a failure closing an account onto the transport
// vocabulary.
func fromClosure(err error, what string) error {
	var blocked *auth.BlockedError
	switch {
	case errors.As(err, &blocked):
		// The ids, so whoever is closing the account can see to each first:
		// make another member an owner, give another member read, or insist.
		var parts []string
		if len(blocked.LastOwnerOf) > 0 {
			parts = append(parts, "the last active owner of the teams "+strings.Join(blocked.LastOwnerOf, ", "))
		}
		if len(blocked.LastReaderOf) > 0 {
			parts = append(parts, "the last person who can read the team mailboxes "+strings.Join(blocked.LastReaderOf, ", "))
		}
		if len(blocked.BoundTo) > 0 {
			parts = append(parts, "the person under whose own consent the team mailboxes "+strings.Join(blocked.BoundTo, ", ")+
				" still sync since the upgrade, which closing them stops, deleting their index for everyone who reads them")
		}
		return E(CodeConflict, "that person is "+strings.Join(parts, ", and ")+
			"; make another member an owner, give another member read, or have an owner or an admin of the team "+
			"turn the mailbox's sync on for the team, first, or pass force to do it anyway", err)
	case errors.Is(err, auth.ErrLastOwner):
		return E(CodeConflict, "that is the last active owner; nobody would be left to invite people "+
			"from the console. Pass force to do it anyway", err)
	case errors.Is(err, auth.ErrUserNotFound):
		return E(CodeNotFound, "no account has that address", err)
	default:
		return E(CodeInternal, what, err)
	}
}
