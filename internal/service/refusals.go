package service

import (
	"errors"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Typed refusals (docs/workspaces.md, "Acts an extension runs for a person").
//
// What an extension calls keeps the cause of each refusal for errors.Is: the
// *Error a transport renders carries, as its cause, one of the sentinels
// below, or one of the repositories' own, and its code and message are what
// they were. RefusalOf reads the cause back as one word of a closed list,
// which an extension maps to its own vocabulary one by one.

// Refusal is why an act was refused, from a closed list.
type Refusal string

// The refusals.
const (
	RefusalNone             Refusal = "none" // anything else: internal, a store fault
	RefusalNotFound         Refusal = "not_found"
	RefusalNotAuthorized    Refusal = "not_authorized"
	RefusalOwnerOnly        Refusal = "owner_only"   // a team mailbox removed by an admin
	RefusalOwnerAccess      Refusal = "owner_access" // an admin changing an owner's access
	RefusalActorInactive    Refusal = "actor_inactive"
	RefusalTargetInactive   Refusal = "target_inactive"
	RefusalTeamClosed       Refusal = "team_closed"
	RefusalTeamWithheld     Refusal = "team_withheld"
	RefusalNotCurrentText   Refusal = "not_current_text"
	RefusalNoReader         Refusal = "no_reader"
	RefusalLastReader       Refusal = "last_reader"
	RefusalRecoveryOver     Refusal = "recovery_over"
	RefusalNotRemoved       Refusal = "not_removed"
	RefusalAlreadyRemoved   Refusal = "already_removed"
	RefusalPaused           Refusal = "paused"
	RefusalSyncOff          Refusal = "sync_off"
	RefusalHeld             Refusal = "held"
	RefusalNeedsReauth      Refusal = "needs_reauth"
	RefusalPendingAuth      Refusal = "pending_auth"
	RefusalEngineNotRunning Refusal = "engine_not_running"
	RefusalGrantNeeded      Refusal = "grant_needed"
	RefusalStateChanged     Refusal = "state_changed" // a compare in the write
	RefusalSetSince         Refusal = "set_since"     // a narrowing of what was given again since
	RefusalManageByRole     Refusal = "manage_by_role"
	RefusalPersonalGrant    Refusal = "personal_grant"
	RefusalNeedsOwnSession  Refusal = "needs_own_session"
	RefusalFreshUnconfirmed Refusal = "fresh_unconfirmed"
	RefusalRequestDone      Refusal = "request_done"
)

// Refusals lists every refusal but RefusalNone, in a fixed order.
func Refusals() []Refusal {
	return []Refusal{
		RefusalNotFound, RefusalNotAuthorized, RefusalOwnerOnly, RefusalOwnerAccess, RefusalActorInactive,
		RefusalTargetInactive, RefusalTeamClosed, RefusalTeamWithheld, RefusalNotCurrentText, RefusalNoReader,
		RefusalLastReader, RefusalRecoveryOver, RefusalNotRemoved, RefusalAlreadyRemoved, RefusalPaused,
		RefusalSyncOff, RefusalHeld, RefusalNeedsReauth, RefusalPendingAuth, RefusalEngineNotRunning,
		RefusalGrantNeeded, RefusalStateChanged, RefusalSetSince, RefusalManageByRole, RefusalPersonalGrant,
		RefusalNeedsOwnSession, RefusalFreshUnconfirmed, RefusalRequestDone,
	}
}

// refusalCause is a sentinel cause a refusal's *Error carries.
type refusalCause struct{ refusal Refusal }

func (c *refusalCause) Error() string { return "service: refused: " + string(c.refusal) }

func cause(r Refusal) error { return &refusalCause{refusal: r} }

// The causes an *Error carries, one per refusal but needs_reauth, which is
// ErrNeedsReauth itself. ErrActorInactive and
// ErrFreshUnconfirmed are also what a freshness hook carries in the *Error it
// returns (Freshness): the actor may no longer act where the hook asked, and
// the hook could not confirm the act now.
var (
	ErrNotFound         = cause(RefusalNotFound)
	ErrNotAuthorized    = cause(RefusalNotAuthorized)
	ErrOwnerOnly        = cause(RefusalOwnerOnly)
	ErrOwnerAccess      = cause(RefusalOwnerAccess)
	ErrActorInactive    = cause(RefusalActorInactive)
	ErrTargetInactive   = cause(RefusalTargetInactive)
	ErrTeamClosed       = cause(RefusalTeamClosed)
	ErrTeamWithheld     = cause(RefusalTeamWithheld)
	ErrNotCurrentText   = cause(RefusalNotCurrentText)
	ErrNoReader         = cause(RefusalNoReader)
	ErrLastReader       = cause(RefusalLastReader)
	ErrRecoveryOver     = cause(RefusalRecoveryOver)
	ErrNotRemoved       = cause(RefusalNotRemoved)
	ErrAlreadyRemoved   = cause(RefusalAlreadyRemoved)
	ErrPaused           = cause(RefusalPaused)
	ErrSyncOff          = cause(RefusalSyncOff)
	ErrHeld             = cause(RefusalHeld)
	ErrPendingAuth      = cause(RefusalPendingAuth)
	ErrEngineNotRunning = cause(RefusalEngineNotRunning)
	ErrGrantNeeded      = cause(RefusalGrantNeeded)
	ErrStateChanged     = cause(RefusalStateChanged)
	ErrSetSince         = cause(RefusalSetSince)
	ErrManageByRole     = cause(RefusalManageByRole)
	ErrPersonalGrant    = cause(RefusalPersonalGrant)
	// ErrNeedsOwnSession is an act that needs a person signed in here, or a
	// key: a person acting through an extension (auth.KindPerson) is refused
	// it, not_authorized.
	ErrNeedsOwnSession  = cause(RefusalNeedsOwnSession)
	ErrFreshUnconfirmed = cause(RefusalFreshUnconfirmed)
	ErrRequestDone      = cause(RefusalRequestDone)
)

// lowerCauses are the repositories' own sentinels, as a refusal names them.
var lowerCauses = []struct {
	err     error
	refusal Refusal
}{
	{store.ErrRequestDone, RefusalRequestDone},
	{store.ErrActorInactive, RefusalActorInactive},
	{store.ErrNoSuchUser, RefusalActorInactive},
	{store.ErrSetSince, RefusalSetSince},
	{store.ErrNotInWorkspace, RefusalNotFound},
	{store.ErrPauseHeld, RefusalHeld},
	{store.ErrPauseSyncOff, RefusalSyncOff},
	{workspace.ErrGrantChanged, RefusalStateChanged},
	{workspace.ErrManageByRole, RefusalManageByRole},
	{workspace.ErrPersonal, RefusalPersonalGrant},
	{workspace.ErrLastReader, RefusalLastReader},
	{workspace.ErrNotMember, RefusalTargetInactive},
	{workspace.ErrSealedGrantNeeded, RefusalGrantNeeded},
	{workspace.ErrClosed, RefusalTeamClosed},
	{account.ErrRemovalOver, RefusalRecoveryOver},
	{account.ErrNotRemoved, RefusalNotRemoved},
	{account.ErrNoReaderLeft, RefusalNoReader},
}

// RefusalOf names why err refused an act: the sentinel it carries first, then
// a repository's, then its code (not_found; not_authorized for a caller who
// may not, or a credential that ended); none for anything else.
func RefusalOf(err error) Refusal {
	if err == nil {
		return RefusalNone
	}
	var c *refusalCause
	if errors.As(err, &c) {
		return c.refusal
	}
	if errors.Is(err, ErrNeedsReauth) {
		return RefusalNeedsReauth
	}
	for _, lc := range lowerCauses {
		if errors.Is(err, lc.err) {
			return lc.refusal
		}
	}
	var se *Error
	if !errors.As(err, &se) {
		return RefusalNone
	}
	switch se.Code {
	case CodeNotFound:
		return RefusalNotFound
	case CodeNotAuthorized, CodeUnauthorized:
		return RefusalNotAuthorized
	}
	return RefusalNone
}

// errNeedsOwnSession refuses a person acting through an extension what needs
// a person signed in here, or a key.
var errNeedsOwnSession = E(CodeNotAuthorized,
	"this needs a person signed in here, or a key: a request carried for a person from elsewhere cannot do it",
	ErrNeedsOwnSession)

// refuseDelegated refuses a person acting through an extension
// (auth.KindPerson): the first line of every exported method that takes a
// principal. What such a person may do is only what ActFor returns, each act
// the use case a session runs, with the request recorded in its write.
func refuseDelegated(p Principal) error {
	if p.IsDelegated() {
		return errNeedsOwnSession
	}
	return nil
}
