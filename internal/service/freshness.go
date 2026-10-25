package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Freshness: where workspaces and their memberships come from a directory
// elsewhere, an act that widens who may read a mailbox, or that touches a
// team's shared data, is asked about first by the extension that follows it
// (Deps.Freshness), which may read the actor's standing there again, and
// apply what it reads, before the act goes on. It gates; it never
// authorizes: the act's own checks run as they always do, and the write
// that makes the act checks again, in its transaction, who may — so an
// answer that ended the actor's membership stops the act there, and it
// changes nothing.
//
// It is asked only of a person signed in, never of a key's own acts nor of
// the operator's, and never inside a write: the extension may apply a
// document, which writes. Reading, removing, recovering and taking access
// away never ask; deleting a removed mailbox for good, and turning a removed
// one's sync off, ask only of a team's mailbox, whose owners do it before
// its 30 days are over: what it deletes is the team's, read by others too.
// Each act asks after its own read-only authorization — a team withheld or
// closed refused first, as its write would — so the extension is never
// asked about an act that cannot happen.

// FreshKind is the kind of act a freshness question is about.
type FreshKind string

// The acts that ask.
const (
	// FreshShareKey: a mailbox's key sealed to someone (giving read with a
	// grant, supplying the key, a first key, a new key).
	FreshShareKey FreshKind = "share_key"
	// FreshRaiseFlags: flags raised for a person, or for a key.
	FreshRaiseFlags FreshKind = "raise_flags"
	// FreshCreateKey: a workspace key created.
	FreshCreateKey FreshKind = "create_key"
	// FreshLink: a mailbox linked into a team.
	FreshLink FreshKind = "link"
	// FreshReauthorize: a team mailbox's consent started again.
	FreshReauthorize FreshKind = "reauthorize"
	// FreshTeamSync: a team mailbox's sync switched on or off.
	FreshTeamSync FreshKind = "team_sync"
	// FreshActions: an action on a team mailbox's messages. (FreshAct is
	// the act itself.)
	FreshActions FreshKind = "act"
	// FreshSend: a send from a team mailbox.
	FreshSend FreshKind = "send"
	// FreshPurge: a removed team mailbox deleted for good by an owner of
	// its team, before its 30 days are over.
	FreshPurge FreshKind = "purge"
	// FreshRemovedSyncOff: a removed team mailbox's sync turned off by an
	// owner of its team, which deletes the index it keeps.
	FreshRemovedSyncOff FreshKind = "removed_sync_off"
)

// FreshAct is an act about to happen.
type FreshAct struct {
	Kind FreshKind
	// ActorID is the person signed in.
	ActorID string
	// RecipientIDs are the people a key is sealed to, or whose flags rise.
	RecipientIDs []string
	// WorkspaceID and AccountID are what the act is on; an account is empty
	// for a workspace's key.
	WorkspaceID, AccountID string
}

// Freshness is asked before an act goes on (see above). A nil error lets it
// go on. A *service.Error is the caller's answer as it is; a context's end
// is passed on; any other error says the directory could not be asked, and
// the caller is told to try again shortly (internal, "upstream: …",
// Retry-After). It is called outside every write, and may write.
type Freshness interface {
	Fresh(ctx context.Context, act FreshAct) error
}

// freshRetry is how long a caller waits when the directory could not be
// asked.
const freshRetry = 30 * time.Second

// errFreshUpstream is what a caller is told when the directory could not be
// asked.
const errFreshUpstream = "upstream: this server could not confirm with the directory its workspaces come from " +
	"that you may do this; try again shortly"

// fresh asks the freshness hook about an act by p, when there is one and p
// is a person signed in; nil otherwise. Never inside a write.
func (s *Service) fresh(ctx context.Context, p Principal, act FreshAct) error {
	if s.freshness == nil || !p.IsSession() {
		return nil
	}
	act.ActorID = p.UserID
	err := s.freshness.Fresh(ctx, act)
	var se *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return &Error{Code: CodeInternal, Message: errFreshUpstream, Retry: freshRetry, Err: err}
	}
}

// asksFresh reports whether an act by p asks the freshness hook: a person
// signed in, with a hook.
func (s *Service) asksFresh(p Principal) bool { return s.freshness != nil && p.IsSession() }

// teamMailbox reports whether a mailbox is a team's: neither a person's nor
// the operator's.
func teamMailbox(a account.Account) bool {
	return a.OwnerUserID == "" && a.WorkspaceID != workspace.OperatorID
}

// raised reports whether to holds a flag from does not.
func raised(from, to workspace.Flags) bool {
	return (to.Read && !from.Read) || (to.Act && !from.Act) || (to.Send && !from.Send) || (to.Manage && !from.Manage)
}
