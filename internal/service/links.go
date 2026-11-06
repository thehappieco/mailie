package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A link at the provider: a person connects a Google or Microsoft mailbox
// through the web flow without typing its address. They choose the account
// on the provider's own page, and the provider's answer names its address
// (internal/account, links.go and identity.go). Nothing of the mailbox
// exists before that answer: the start keeps where it goes and who started
// it, the return carries the mailbox's first key, made and sealed by the
// person's browser then (docs/key-scheme.md sections 8 and 12.11), and the
// mailbox, its key and its linker's grant are created in one transaction once
// one login as that address worked.
//
// Who may link where is AddAccount's rule (linkInto), asked at the start to
// fail early, again at the return before the code is taken, and once more in
// the transaction that creates the mailbox, which is the answer that counts;
// a team asks the freshness hook at the start and at the return. The
// address-first path (AddAccount) is unchanged and serves everything else.

// LinkRequest starts a link at the provider.
type LinkRequest struct {
	// Provider is gmail or microsoft.
	Provider string `json:"provider"`
	// Flow is web, the only flow a link takes; left out, it is the one.
	Flow string `json:"flow,omitempty"`
	// WorkspaceID is where the mailbox goes, as for AddAccount: empty is the
	// person's personal workspace; a team takes one from its owners and
	// admins.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// SyncConsentVersion, for a team, gives the team's consent to sync the
	// mailbox once it is linked, on the team's behalf, as AddAccount's does:
	// the current sync text's revision, checked again at the return, where
	// the agreement is recorded.
	SyncConsentVersion string `json:"sync_consent_version,omitempty"`
	// DisplayName is the mailbox's name, if the person gives one.
	DisplayName string `json:"display_name,omitempty"`
}

// Link is a link at the provider as its starter reads it: never the mailbox
// it created, which GET /v1/accounts/{id} shows to whoever may see it.
type Link struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// WorkspaceID is the team it links into; absent for the starter's
	// personal workspace.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// State is waiting (for the provider's return), finishing (the return
	// taken, the mailbox being created), connected or failed.
	State string `json:"state"`
	// ExpiresAt is when a link still waiting stops being taken, in unix
	// seconds.
	ExpiresAt int64 `json:"expires_at"`
	// AccountID is the mailbox it created, while connected and until that
	// mailbox is deleted for good.
	AccountID string `json:"account_id,omitempty"`
	// Email is the address the provider named, on a failed link, once it
	// named one; Reason is why it failed, a fixed English text a console
	// matches and never shows.
	Email  string `json:"email,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// LinkStarted is a link just started, and where to send the browser.
type LinkStarted struct {
	Link Link      `json:"link"`
	Auth *AuthFlow `json:"auth"`
}

// CompleteOAuthRequest finishes a consent from the address the browser
// landed on.
type CompleteOAuthRequest struct {
	// RedirectURL is the whole address the browser ended on: taking it
	// rather than the code is one paste instead of three, and it carries the
	// state that ties it back to its consent.
	RedirectURL string `json:"redirect_url"`
	// PublicKey, Namespace and Grant are the mailbox's first key, as
	// AddAccountRequest's: on a link at the provider's return with a code,
	// which creates the mailbox, all three, made by the browser then; never
	// on a consent for a mailbox that exists, and of no use to a return that
	// carries a refusal.
	PublicKey string `json:"public_key,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Grant     string `json:"grant,omitempty"`
}

// carriesKey reports whether the request carries any part of a mailbox key.
func (r CompleteOAuthRequest) carriesKey() bool {
	return r.PublicKey != "" || r.Namespace != "" || r.Grant != ""
}

const (
	// linkPrecheckTimeout bounds what a link's return asks before its code
	// is taken: who may link, the freshness hook, the step-up. Its finish
	// takes account.FinishTimeout more, and the callback route waits for
	// both.
	linkPrecheckTimeout = 20 * time.Second
	// linkStepUpSpare is what a link's return asks of the step-up beyond
	// its finish: still fresh once the code is exchanged and the login is
	// over, when the transaction that creates the mailbox asks again. The
	// take waits for the writer account.LinkTakeTimeout at most, and the
	// finish runs from the take; five seconds more are slack.
	linkStepUpSpare = account.LinkTakeTimeout + 5*time.Second
	// maxLinkName is the longest name a link keeps for its mailbox until its
	// return, in characters.
	maxLinkName = 200
)

var (
	errLinkProvider = E(CodeBadRequest, "a link at the provider is for gmail or microsoft; "+
		"other mailboxes are added with their address", nil)
	errLinkFlow = E(CodeBadRequest, "a link at the provider uses the web flow, which this server does not offer here; "+
		"add the mailbox with its address instead", nil)
	errLinkName = Ef(CodeBadRequest, nil, "a mailbox's name is at most %d characters", maxLinkName)
	errLinkKey  = E(CodeBadRequest, "public_key, namespace and grant come only with a link's return, "+
		"which creates the mailbox; a consent for a mailbox that exists carries none", nil)
)

const (
	// msgLinkDuplicate is one answer for a mailbox the workspace holds, live
	// or removed and kept: telling them apart would tell an admin who is not
	// an owner that a removed one exists.
	msgLinkDuplicate = "that address is already linked in this workspace, or was removed from it and is kept; " +
		"nothing was connected"
	msgLinkNoAddress = "the provider named no usable address for the account chosen on its page, " +
		"so nothing was connected; start again"
	msgLinkGone = "that authorisation attempt is no longer open; start again"
	// msgLinkSlow is a return whose checks before its code is taken ran out
	// of time: nothing was taken, and the same return may be posted again.
	msgLinkSlow = "upstream: checking that you may still finish this link took too long; nothing was taken, " +
		"so post the same return again shortly"
	// msgLinkBusy is a return that could not take its link in time, the
	// server busy writing something else: nothing was taken either.
	msgLinkBusy = "the server was too busy to take this return just now; nothing was taken, " +
		"so post the same return again shortly"
)

// StartLink starts a link at the provider for a person signed in: no
// address, the provider's account chooser, and the scopes that name the
// account's address. Nothing is dialled, and no mailbox, key or grant is
// written: the mailbox's key goes with the return (CompleteOAuth).
//
// Who may link where is AddAccount's rule, asked now to fail early; so are
// the person's account key and a fresh step-up. A person has at most
// account.MaxLinksWaiting links waiting (rate_limited until the oldest
// expires); a new one for the same provider and workspace replaces theirs.
func (s *Service) StartLink(ctx context.Context, p Principal, req LinkRequest) (LinkStarted, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return LinkStarted{}, err
	}
	// A link is a person's: its key is sealed to them, and its return is
	// theirs alone.
	if err := requireSession(p); err != nil {
		return LinkStarted{}, err
	}
	kind, err := provider.ParseKind(strings.TrimSpace(req.Provider))
	if err != nil || (kind != provider.KindGmail && kind != provider.KindMicrosoft) {
		return LinkStarted{}, errLinkProvider
	}
	if utf8.RuneCountInString(req.DisplayName) > maxLinkName {
		return LinkStarted{}, errLinkName
	}
	flow, err := s.chooseFlow(p, kind, req.Flow)
	if err != nil {
		return LinkStarted{}, err
	}
	if flow != account.FlowWeb {
		return LinkStarted{}, errLinkFlow
	}
	into, err := s.linkInto(ctx, p, strings.TrimSpace(req.WorkspaceID), req.SyncConsentVersion)
	if err != nil {
		return LinkStarted{}, err
	}
	if into.team {
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshLink, WorkspaceID: into.workspaceID}); err != nil {
			return LinkStarted{}, err
		}
	}
	if err := s.requireAccountKey(ctx, p); err != nil {
		return LinkStarted{}, err
	}
	if err := s.requireStepUp(ctx, p); err != nil {
		return LinkStarted{}, err
	}
	link, started, err := s.accounts.StartLink(ctx, account.LinkStart{
		Provider: kind, OwnerUserID: p.UserID, WorkspaceID: into.workspaceID, DisplayName: req.DisplayName,
		SyncConsentVersion: into.consent.Version,
	})
	if err != nil {
		return LinkStarted{}, fromLink(err)
	}
	return LinkStarted{Link: presentLink(link), Auth: presentFlow(started)}, nil
}

// GetLink reads a link at the provider for the person who started it:
// anybody else's, and one already forgotten (twenty minutes after it was
// started, at most), is not_found. A console reads it when a return went
// unanswered, and while it is finishing.
func (s *Service) GetLink(ctx context.Context, p Principal, id string) (Link, error) {
	if err := requireSession(p); err != nil {
		return Link{}, err
	}
	l, err := s.accounts.Repo().Link(ctx, id, p.UserID)
	switch {
	case errors.Is(err, account.ErrNotFound):
		return Link{}, E(CodeNotFound, "no such link", err)
	case err != nil:
		return Link{}, E(CodeInternal, "reading the link failed", err)
	}
	return presentLink(l), nil
}

// completeLink finishes a link at the provider from its return, for the
// person who started it. pending is the link as read before anything is
// taken.
//
// A return that carries the provider's refusal is recorded as it is, with no
// key and no step-up. One that carries a code is checked first, read only,
// with nothing taken, so a refusal here leaves the link for the same return
// posted again: the key's shapes, who may link into the workspace (and the
// team's sync text, if its agreement was asked), the freshness hook for a
// team, the person's account key, and a step-up still fresh once the finish
// is over. Then the registry takes the link, exchanges its code, learns the
// address, proves one login with it and creates the mailbox, its key and the
// person's grant in one transaction, which asks who may link and the step-up
// again under its own context. A take that could not get the writer in time
// has taken nothing either: internal, with a Retry, for the same return.
func (s *Service) completeLink(ctx context.Context, p Principal, rd account.Redirect, req CompleteOAuthRequest,
	pending account.PendingLink,
) (Account, error) {
	if rd.Error != "" {
		_, err := s.accounts.CompleteLink(ctx, rd, p.UserID, account.LinkCompletion{})
		return Account{}, fromLink(err)
	}
	key, err := linkKeyOf(req.PublicKey, req.Namespace, req.Grant)
	if err != nil {
		return Account{}, err
	}
	into, err := s.linkPrecheck(ctx, p, pending)
	if err != nil {
		return Account{}, err
	}
	check := into.check
	created, err := s.accounts.CompleteLink(ctx, rd, p.UserID, account.LinkCompletion{
		Key: key,
		// Under the context the write gives it: the finish's, detached from
		// this request, never the request's.
		Check: func(ctx context.Context, tx *sql.Tx) error {
			if check != nil {
				if err := check(ctx, tx); err != nil {
					return err
				}
			}
			return s.stepUpTx(ctx, tx, p)
		},
		SyncConsent: into.consent,
		LinkedBy:    p.Actor(),
	})
	if err != nil {
		return Account{}, fromLink(err)
	}
	s.accessChanged()
	if created.SyncConsent.At != 0 {
		s.log.Info("team mailbox linked with its workspace's consent to sync", "account", created.ID,
			"workspace", created.WorkspaceID, "by", p.Actor(), "version", created.SyncConsent.Version)
	}
	// As GET /v1/accounts/{id} would answer it: whatever changed while it
	// was created, the visibility rule decides what the person sees of it.
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, created.ID, needCard)
	if err != nil {
		return Account{}, err
	}
	return s.present(ctx, p, a)[0], nil
}

// linkPrecheck asks, read only and before a link's code is taken, whether
// its starter may still finish it, and returns where it goes, with the check
// its transaction runs again and the team's consent to record. Bounded by
// linkPrecheckTimeout: a link's return has its finish still to run.
func (s *Service) linkPrecheck(ctx context.Context, p Principal, pending account.PendingLink) (link, error) {
	checkCtx, cancel := context.WithTimeout(ctx, linkPrecheckTimeout)
	defer cancel()
	into, err := s.linkPrecheckIn(checkCtx, p, pending)
	if err != nil && ctx.Err() == nil && errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return link{}, &Error{Code: CodeInternal, Message: msgLinkSlow, Retry: freshRetry, Err: err}
	}
	return into, err
}

func (s *Service) linkPrecheckIn(ctx context.Context, p Principal, pending account.PendingLink) (link, error) {
	into, err := s.linkInto(ctx, p, pending.WorkspaceID, pending.SyncConsentVersion)
	if err != nil {
		return link{}, err
	}
	if into.team {
		if err := s.fresh(ctx, p, FreshAct{Kind: FreshLink, WorkspaceID: into.workspaceID}); err != nil {
			return link{}, err
		}
	}
	if err := s.requireAccountKey(ctx, p); err != nil {
		return link{}, err
	}
	// Fresh now, and still once the finish is over: a step-up that lapses
	// during the exchange would spend the code for nothing.
	if err := s.users.RequireStepUpFor(ctx, p.UserID, p.SessionID, account.FinishTimeout+linkStepUpSpare); err != nil {
		return link{}, fromUsers(err, "reading the step-up failed")
	}
	return into, nil
}

// fromLink renders a link at the provider that could not be started or
// finished. A refusal this layer wrote passes through as it is.
func fromLink(err error) error {
	var (
		se   *Error
		many *account.TooManyLinksError
	)
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, auth.ErrStepUpNeeded), errors.Is(err, auth.ErrInvalidSession):
		return fromUsers(err, "reading the step-up failed")
	case errors.Is(err, account.ErrNoLinkKey):
		return errLinkKeyNeeded
	case errors.Is(err, account.ErrLinkBusy):
		return &Error{Code: CodeInternal, Message: msgLinkBusy, Retry: freshRetry, Err: err}
	case errors.Is(err, account.ErrNotFound), errors.Is(err, account.ErrFlowExpired):
		return E(CodeNotFound, msgLinkGone, err)
	}
	if known := completionError(err); known != nil {
		return known
	}
	switch {
	case errors.Is(err, account.ErrNoAddress):
		return E(CodeBadRequest, msgLinkNoAddress, err)
	case errors.Is(err, account.ErrDuplicate), errors.Is(err, account.ErrRemovedDuplicate):
		return E(CodeConflict, msgLinkDuplicate, err)
	case errors.As(err, &many):
		wait := time.Until(many.Until).Round(time.Second)
		if wait < time.Second {
			wait = time.Second
		}
		return Retryable("too many links at the provider are waiting; wait until the oldest expires, "+
			"or finish one", wait, err)
	case errors.Is(err, account.ErrNoWorkspace), errors.Is(err, workspace.ErrNotMember):
		return E(CodeNotFound, "no such workspace", err)
	case errors.Is(err, workspace.ErrClosed):
		return E(CodeConflict, errTeamClosed.Message, err)
	case errors.Is(err, account.ErrUnknownOwner):
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	case errors.Is(err, account.ErrFlowUnavailable):
		return E(CodeBadRequest, "that consent flow is not available on this server", err)
	case errors.Is(err, account.ErrNotOAuth):
		return errLinkProvider
	case errors.Is(err, account.ErrBadRedirect):
		return E(CodeBadRequest, "that is not the address the provider sent the browser back to; "+
			"paste the whole address", err)
	}
	return fromKeys(err, "connecting the mailbox failed")
}

func presentLink(l account.Link) Link {
	out := Link{
		ID: l.ID, Provider: string(l.Provider), WorkspaceID: l.WorkspaceID, State: string(l.State),
		ExpiresAt: l.ExpiresAt.Unix(),
	}
	switch l.State {
	case account.LinkConnected:
		out.AccountID = l.AccountID
	case account.LinkFailed:
		out.Email, out.Reason = l.Email, l.Reason
	}
	return out
}
