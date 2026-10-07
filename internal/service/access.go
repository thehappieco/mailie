package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Access to a mailbox (docs/workspaces.md).
//
// Every mailbox belongs to its workspace. A person sees one as an active
// member of that workspace who holds a grant on it or, as an owner or an
// admin, manages it by their role: either shows its card — the account, its
// sync, never what it holds. Each flag of a grant opens one use: read its
// index, act on its messages, send from it; manage is the card and
// re-authorizing it, which owners and admins hold by their role and members
// only as a stored flag. No role reads, acts or sends. A workspace's API key
// sees a mailbox it holds something on (key_access), and each flag it holds
// there opens the same use, within its scope; a key never manages. An
// instance key reaches the operator workspace's mailboxes, and its scope is
// what limits it there.

// What an operation needs of the caller's grant.
var (
	// needCard is any grant: the account's card.
	needCard = workspace.Flags{}
	// needRead opens the index: folders, messages, events, storage.
	needRead = workspace.Flags{Read: true}
	// needSend is sending from the mailbox.
	needSend = workspace.Flags{Send: true}
	// needManage is re-authorising the mailbox.
	needManage = workspace.Flags{Manage: true}
)

// AccountAccess is what the caller may do with a mailbox: their grant, as far
// as the credential they came with reaches (a key's scope). For an instance
// key, on an operator mailbox, what its scope allows.
type AccountAccess struct {
	Read   bool `json:"read"`
	Act    bool `json:"act"`
	Send   bool `json:"send"`
	Manage bool `json:"manage"`
}

func presentAccess(f workspace.Flags) AccountAccess {
	return AccountAccess{Read: f.Read, Act: f.Act, Send: f.Send, Manage: f.Manage}
}

// scopeFlags is what a scope lets a credential do, as flags.
func scopeFlags(scope auth.Scope) workspace.Flags {
	return workspace.Flags{
		Read: scope.Covers(auth.ScopeRead), Act: scope.Covers(auth.ScopeWrite),
		Send: scope.Covers(auth.ScopeSend), Manage: scope.Covers(auth.ScopeAdmin),
	}
}

func intersect(a, b workspace.Flags) workspace.Flags {
	return workspace.Flags{Read: a.Read && b.Read, Act: a.Act && b.Act, Send: a.Send && b.Send, Manage: a.Manage && b.Manage}
}

// grantsOf reads what the caller may do with each mailbox named: a person's
// grant, counting only an active member's, with manage from it or from their
// role, owner or admin; what a workspace key holds, without send while this
// server's keys may not send; and every flag on each mailbox for an instance
// key, which sees only the operator's. A mailbox the caller neither holds a
// grant on nor manages by their role is absent.
func (s *Service) grantsOf(ctx context.Context, p Principal, accountIDs ...string) (map[string]workspace.Flags, error) {
	if p.IsInstance() {
		out := make(map[string]workspace.Flags, len(accountIDs))
		for _, id := range accountIDs {
			out[id] = workspace.AllFlags()
		}
		return out, nil
	}
	if s.workspaces == nil {
		return map[string]workspace.Flags{}, nil
	}
	if p.IsWorkspaceKey() {
		held, err := s.workspaces.KeyAccessOf(ctx, p.KeyPrefix, accountIDs)
		if err != nil {
			return nil, err
		}
		if s.keysMayNotSend {
			for id, f := range held {
				f.Send = false
				held[id] = f
			}
		}
		return held, nil
	}
	return s.workspaces.Access(ctx, p.UserID, accountIDs)
}

// requireFlags refuses a caller who sees a mailbox without the flags need
// names. An instance key's scope is checked by authorize, and is all that
// limits it on the operator's mailboxes.
func (s *Service) requireFlags(ctx context.Context, p Principal, a account.Account, need workspace.Flags) error {
	if !need.Any() || p.IsInstance() {
		return nil
	}
	held, err := s.grantsOf(ctx, p, a.ID)
	if err != nil {
		return E(CodeInternal, "reading the access to the mailbox failed", err)
	}
	if !held[a.ID].Covers(need) {
		return errMissingFlag(need)
	}
	return nil
}

// errMissingFlag says which use of a mailbox the caller's grant lacks.
func errMissingFlag(need workspace.Flags) error {
	switch {
	case need.Read:
		return errNoRead
	case need.Act:
		return errNoAct
	case need.Send:
		return errNoSendFlag
	default:
		return errNoManage
	}
}

// Errors of a grant without the flag an operation needs.
var (
	errNoRead = E(CodeNotAuthorized,
		"you do not have read access to this mailbox; an owner or an admin of its workspace who reads it can grant it", nil)
	errNoAct = E(CodeNotAuthorized,
		"you may not change this mailbox's messages; an owner or an admin of its workspace can grant it", nil)
	errNoSendFlag = E(CodeNotAuthorized,
		"you may not send from this mailbox; an owner or an admin of its workspace can grant it", nil)
	errNoManage = E(CodeNotAuthorized,
		"you do not manage this mailbox; its workspace's owners and admins do", nil)
)

// accessChanged records that read access may have changed for somebody. It
// is called once the change has committed: an event decided before it is one
// the caller could still read when it was decided.
func (s *Service) accessChanged() { s.accessEpoch.Add(1) }

// inWorkspace checks a ?workspace= a caller narrowed a listing to: a
// workspace they are an active member of; for a workspace key, its own, or,
// for a key carried over from a person's, one it holds a mailbox of; for an
// instance key, the operator workspace. Anything else does not exist for
// them. Empty is every workspace, and is always fine.
func (s *Service) inWorkspace(ctx context.Context, p Principal, workspaceID string) error {
	switch {
	case workspaceID == "":
		return nil
	case p.IsInstance():
		if workspaceID == workspace.OperatorID {
			return nil
		}
		return errNoWorkspace
	case s.workspaces == nil:
		return errNoWorkspace
	case p.IsWorkspaceKey() && p.WorkspaceID != "":
		if workspaceID == p.WorkspaceID {
			return nil
		}
		return errNoWorkspace
	case p.IsWorkspaceKey():
		reached, err := s.workspaces.KeyWorkspaces(ctx, p.KeyPrefix)
		if err != nil {
			return E(CodeInternal, "reading the workspace failed", err)
		}
		if slices.Contains(reached, workspaceID) {
			return nil
		}
		return errNoWorkspace
	}
	m, err := s.workspaces.Member(ctx, workspaceID, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNotMember):
		return errNoWorkspace
	case err != nil:
		return E(CodeInternal, "reading the workspace failed", err)
	case !m.Active():
		return errNoWorkspace
	}
	return nil
}

var errNoWorkspace = E(CodeNotFound, "no such workspace", nil)

// precheck runs a write's check once ahead of it, on a read-only transaction,
// so a caller who may not is refused before anything slow happens — a login
// to a mail server, an Argon2id hash. The write runs the same check again in
// its own transaction, which is the one that counts.
func (s *Service) precheck(ctx context.Context, check func(*sql.Tx) error) error {
	if check == nil {
		return nil
	}
	tx, err := s.store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return E(CodeInternal, "reading the workspace failed", err)
	}
	//nolint:errcheck // a read-only transaction; nothing to keep or lose
	defer func() { _ = tx.Rollback() }()
	return check(tx)
}

// fromWorkspace maps a failure from the workspace repository, or from the
// check a write ran in its transaction, onto the transport vocabulary. A
// check's own refusal is already one and goes through unchanged.
func fromWorkspace(err error, what string) error {
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, workspace.ErrNotFound):
		return E(CodeNotFound, "no such workspace", err)
	case errors.Is(err, workspace.ErrNoMailbox):
		return E(CodeNotFound, "no such account", err)
	case errors.Is(err, workspace.ErrNotMember), errors.Is(err, workspace.ErrNoSuchPerson):
		return E(CodeNotFound, "that person is not an active member of the workspace", err)
	case errors.Is(err, workspace.ErrNoGrant):
		return E(CodeNotFound, "that person has no access to the mailbox", err)
	case errors.Is(err, workspace.ErrAlreadyMember):
		return E(CodeConflict, "that person is already a member of the workspace", err)
	case errors.Is(err, workspace.ErrLastOwner):
		return E(CodeConflict, "that is the last active owner of the team; make another member an owner first", err)
	case errors.Is(err, workspace.ErrLastReader):
		return E(CodeConflict, "that is the last person who can read a mailbox of the workspace; "+
			"give another member read on it first, or remove the mailbox", err)
	case errors.Is(err, workspace.ErrHoldsMailboxes):
		return E(CodeConflict, "the workspace still holds mailboxes", err)
	case errors.Is(err, workspace.ErrManageByRole):
		return E(CodeBadRequest, "owners and admins manage every team mailbox by their role; manage is granted to members only", err)
	case errors.Is(err, workspace.ErrManagedElsewhere):
		return E(CodeConflict, "workspaces are managed in the account console", err)
	case errors.Is(err, workspace.ErrPersonal):
		return E(CodeBadRequest, "a personal workspace has its person as its only member", err)
	case errors.Is(err, workspace.ErrOperator):
		return E(CodeBadRequest, "the operator workspace has no members, grants or invites", err)
	case errors.Is(err, workspace.ErrActWithoutRead):
		return E(CodeBadRequest, "act needs read", err)
	case errors.Is(err, workspace.ErrNoKey):
		return errNoSuchKey
	case errors.Is(err, workspace.ErrNoKeyAccess):
		return E(CodeNotFound, "that key holds nothing on the mailbox", err)
	case errors.Is(err, workspace.ErrKeyNotLive):
		return E(CodeConflict, "that key is revoked or expired: it is given nothing more", err)
	case errors.Is(err, workspace.ErrKeyScope):
		return E(CodeBadRequest, "act needs a key of the write or send scope, and send a key of the send scope", err)
	case errors.Is(err, workspace.ErrCarriedOver):
		return E(CodeBadRequest, "a key carried over from a person's gains no mailbox and no flag; "+
			"create a key of the workspace instead", err)
	case errors.Is(err, workspace.ErrNoFlags):
		return E(CodeBadRequest, "a grant needs at least one of read, act, send and manage", err)
	case errors.Is(err, workspace.ErrInvalidName):
		return Ef(CodeBadRequest, err, "a team's name is 1 to %d characters with no control characters", workspace.MaxNameLength)
	case errors.Is(err, workspace.ErrInvalidRole):
		return E(CodeBadRequest, "a role is owner, admin or member", err)
	case errors.Is(err, workspace.ErrInvalidStatus):
		return E(CodeBadRequest, "a status is active or disabled", err)
	default:
		return E(CodeInternal, what, err)
	}
}
