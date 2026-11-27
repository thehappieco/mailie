package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The link gate: an extension may be asked before a mailbox is linked into a
// workspace, and told once it was (Deps.LinkGate). A link is a mailbox
// added by its address (OAuth or password), a link at the provider, and the
// first authorization of a mailbox never authorized, which is when an
// address-first OAuth mailbox becomes one that works. A re-authorization of
// a mailbox that connected once is no link, and neither asks nor tells
// anything.
//
// It is asked after the act's own read-only authorization and the freshness
// hook, so it is never asked about a link that cannot happen, and before
// anything is dialled or stored; and told after the write that made the link
// committed. Both outside every write: the extension may write. A first
// authorization is told by whichever path stored its grant — a redirect the
// console posts or a person pastes, or a loopback listener or a device poll
// the daemon completes by itself — through the registry
// (account.Registry.OnFirstGrant), which reads in that write whether the
// grant is the mailbox's first credential: two attempts finishing together
// tell it once. A person acting through an extension (ActFor) links
// nothing, so neither is ever called for one. Neither names the mailbox: a
// mailbox that was never authorized has an id, and no more, and nothing of
// it reaches the gate.

// LinkWay is how a mailbox is linked.
type LinkWay string

// The ways a mailbox is linked.
const (
	// LinkByAddress: a mailbox added by its address, with a password or
	// through OAuth consent (AddAccount).
	LinkByAddress LinkWay = "address"
	// LinkAtProvider: a link at the provider, whose address the provider
	// names at its return (StartLink, CompleteOAuth).
	LinkAtProvider LinkWay = "provider"
	// LinkFirstAuthorization: the first authorization of a mailbox added by
	// its address that was never authorized: asked at StartOAuth, told once
	// its first grant is stored, on whichever path.
	LinkFirstAuthorization LinkWay = "first_authorization"
)

// LinkAttempt is a link about to happen, or one that happened.
type LinkAttempt struct {
	// ActorID is the person signed in who links, or empty for a key.
	ActorID string
	// WorkspaceID is where the mailbox goes, never empty: a person's
	// personal workspace, when the request names none, and the operator
	// workspace for an instance key.
	WorkspaceID string
	// Team says the workspace is a team's.
	Team bool
	// Way is how it is linked.
	Way LinkWay
}

// LinkGate is asked before a mailbox is linked into a workspace, and told
// once a link committed (see above). Nil asks and tells nothing.
//
// BeforeLink's answer is fresh's: nil lets the link go on; a *service.Error
// is the caller's answer as it is; a context's end is passed on; any other
// error says the gate could not be asked, and the caller is told to try
// again shortly (internal, "upstream: …", Retry-After). It is called before
// anything is dialled or stored, and may write.
//
// Linked is a notification: called once the write that linked the mailbox
// committed, with a context its caller's end does not cancel, and nothing it
// does changes what the caller is answered. It should return promptly: the
// caller waits for it.
type LinkGate interface {
	BeforeLink(ctx context.Context, link LinkAttempt) error
	Linked(ctx context.Context, link LinkAttempt)
}

// linkGateRetry is how long a caller waits when the gate could not be
// asked.
const linkGateRetry = 30 * time.Second

// errLinkGateUpstream is what a caller is told when the gate could not be
// asked.
const errLinkGateUpstream = "upstream: this server could not confirm that a mailbox may be linked here; " +
	"nothing was linked; try again shortly"

// errLinkGateUnasked is the cause of a link refused because its gate could
// not be asked, beside the gate's own error.
var errLinkGateUnasked = errors.New("service: the link gate could not be asked")

// beforeLink asks the link gate about a link by p into workspaceID, when
// there is a gate; nil otherwise. workspaceID is what linkInto answered: an
// empty one is resolved as the transaction that creates the mailbox resolves
// it. Never inside a write.
func (s *Service) beforeLink(ctx context.Context, p Principal, workspaceID string, team bool, way LinkWay) error {
	if s.linkGate == nil {
		return nil
	}
	id, err := s.linkWorkspace(ctx, p, workspaceID)
	if err != nil {
		return err
	}
	err = s.linkGate.BeforeLink(ctx, LinkAttempt{ActorID: linkActor(p), WorkspaceID: id, Team: team, Way: way})
	var se *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return &Error{Code: CodeInternal, Message: errLinkGateUpstream, Retry: linkGateRetry,
			Err: fmt.Errorf("%w: %w", errLinkGateUnasked, err)}
	}
}

// linked tells the link gate, when there is one, that a link by p of a
// mailbox now stored as a committed. Called after that commit, never inside
// a write, under a context the caller's end does not cancel: the link is
// done whether or not anybody still waits for its answer.
func (s *Service) linked(ctx context.Context, p Principal, a account.Account, way LinkWay) {
	s.tellLinked(ctx, linkActor(p), a, way)
}

// firstGranted is the registry's account.FirstGrant: a mailbox's first
// grant, stored, is its first authorization, linked, whichever path
// finished its consent. owner, who started the attempt, is the person who
// links (a person's user id), or nobody for a key, whose attempts carry none.
func (s *Service) firstGranted(ctx context.Context, a account.Account, owner string) {
	s.tellLinked(ctx, owner, a, LinkFirstAuthorization)
}

// tellLinked tells the link gate, when there is one, of a link by actor of
// a, as linked says.
func (s *Service) tellLinked(ctx context.Context, actor string, a account.Account, way LinkWay) {
	if s.linkGate == nil {
		return
	}
	s.linkGate.Linked(context.WithoutCancel(ctx), LinkAttempt{
		ActorID: actor, WorkspaceID: a.WorkspaceID, Team: teamMailbox(a), Way: way,
	})
}

// linkWorkspace is the workspace a link by p goes into: the one linkInto
// answered, or, where it answered none, the operator workspace for a caller
// who is nobody and the caller's personal workspace otherwise, as
// account.workspaceForTx decides for the transaction that creates the
// mailbox.
func (s *Service) linkWorkspace(ctx context.Context, p Principal, workspaceID string) (string, error) {
	switch {
	case workspaceID != "":
		return workspaceID, nil
	case p.UserID == "":
		return workspace.OperatorID, nil
	case s.workspaces == nil:
		return "", errNoWorkspace
	}
	w, err := s.workspaces.PersonalOf(ctx, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return "", errNoWorkspace
	case err != nil:
		return "", E(CodeInternal, "reading the workspace failed", err)
	}
	return w.ID, nil
}

// linkActor is the person who links, or nobody for a key.
func linkActor(p Principal) string {
	if p.IsPerson() {
		return p.UserID
	}
	return ""
}
