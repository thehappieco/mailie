package account

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A link at the provider (migration 0026): a Google or Microsoft mailbox
// connected through the web flow without its address being typed here. The
// person picks the account on the provider's page, its account chooser
// always shown, and the provider's answer names the address (identity.go).
// Nothing of the mailbox exists before that answer: a link holds its flow's
// state and PKCE verifier, who started it and into which workspace, until
// its return, and no key and no address. At the return, its starter alone
// takes it, once, in the statement that sets taken_at, before its code is
// exchanged; then the address is read, a mailbox the workspace already holds
// under it refused, the grant proved by one login as that address
// (checkGrant), and the mailbox, its key and its linker's grant created in
// one transaction, which also finishes the link. Every other ending is
// recorded on the link, with a fixed reason and the address once the
// provider named one, for its starter's console to say why.
//
// The address-first path (Add) is unchanged and stays what everything else
// uses: iCloud and IMAP, the CLI and instance keys, the installed client's
// flows, and the Microsoft mailboxes whose address only the person knows (a
// shared mailbox, or a tenant whose administrator consented to the mail
// permissions alone).

// LinkState is where a link at the provider stands, as its starter reads it.
type LinkState string

const (
	// LinkWaiting is a link started and not yet back from the provider.
	LinkWaiting LinkState = "waiting"
	// LinkFinishing is a link whose return was taken and is being finished:
	// its code exchanged, its address read, its grant proved, its mailbox
	// created.
	LinkFinishing LinkState = "finishing"
	// LinkConnected is a link that created its mailbox.
	LinkConnected LinkState = "connected"
	// LinkFailed is a link that ended without one, for its Reason.
	LinkFailed LinkState = "failed"
)

// MaxLinksWaiting is how many links one person may have waiting at once: a
// new one for the same provider and workspace replaces theirs instead.
const MaxLinksWaiting = 5

const (
	// linkKeptAfter is how long a link is kept past its expiry, however it
	// ended, for its starter's console to read how: 20 minutes in all, at
	// most.
	linkKeptAfter = 10 * time.Minute
	// linkCutOff is how long after its take a link still finishing is one a
	// restart cut off: its finish ends finishTimeout after the take, which
	// was recorded to the second, and recording it failed takes
	// persistTimeout more.
	linkCutOff = finishTimeout + persistTimeout
)

// LinkTakeTimeout bounds how long a link's return waits for the writer to
// take its link.
const LinkTakeTimeout = 5 * time.Second

// Errors about links at the provider.
var (
	// ErrTooManyLinks is a person with MaxLinksWaiting links waiting
	// already. Its error is a *TooManyLinksError, which says until when.
	ErrTooManyLinks = errors.New("account: too many links at the provider are waiting")
	// ErrNoLinkKey is a link's return with a code and without the mailbox's
	// key, which a person's link always carries (docs/key-scheme.md section
	// 12.11): refused before the link is taken, which stays for a return
	// that carries one.
	ErrNoLinkKey = errors.New("account: a link's return carries the mailbox's key")
	// ErrLinkBusy is a link's return that could not take its link within
	// LinkTakeTimeout.
	ErrLinkBusy = errors.New("account: the link could not be taken in time")
)

// TooManyLinksError is ErrTooManyLinks for one person: a link is started
// again once the oldest of theirs waiting expires, at Until.
type TooManyLinksError struct {
	Until time.Time
}

func (e *TooManyLinksError) Error() string {
	return ErrTooManyLinks.Error() + " until " + e.Until.UTC().Format(time.RFC3339)
}

// Is makes errors.Is(err, ErrTooManyLinks) hold.
func (e *TooManyLinksError) Is(target error) bool { return target == ErrTooManyLinks }

// linkFailureReason is the reason a failed link records: failureReason's,
// and the two only a link has.
func linkFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrNoAddress):
		return ReasonNoAddress
	case errors.Is(err, ErrDuplicate), errors.Is(err, ErrRemovedDuplicate):
		// One reason for a live mailbox and a removed one: telling them
		// apart would tell an admin who is not an owner that a removed
		// mailbox exists.
		return ReasonDuplicate
	default:
		return failureReason(err)
	}
}

// LinkStart is what a link at the provider starts with.
type LinkStart struct {
	// Provider is gmail or microsoft.
	Provider provider.Kind
	// OwnerUserID is the person who starts it, the only one who finishes it,
	// and the mailbox's linker: a link is always a person's.
	OwnerUserID string
	// WorkspaceID is the team it links into; empty, or the person's own
	// personal workspace, is their personal workspace, which the transaction
	// that creates the mailbox resolves. Who may link where is the service's
	// to decide, at the start and again at the return.
	WorkspaceID string
	// DisplayName is the mailbox's name, if the person gave one.
	DisplayName string
	// SyncConsentVersion is the team's sync text the person agreed to on
	// the team's behalf, or empty: kept for the return, where the service
	// checks it is still the current one and records the agreement.
	SyncConsentVersion string
}

// Link is a link at the provider as its starter reads it.
type Link struct {
	ID       string
	Provider provider.Kind
	// WorkspaceID is the team it links into; empty for the starter's
	// personal workspace.
	WorkspaceID string
	State       LinkState
	ExpiresAt   time.Time
	// AccountID is the mailbox it created, while connected and until that
	// mailbox is deleted for good.
	AccountID string
	// Email is the address the provider named, while failed, once it named
	// one; Reason why it failed, one of the fixed Reason texts.
	Email  string
	Reason string
}

// PendingLink is a link waiting for its return, as the caller completing it
// reads it before anything is taken (OwnedLink): what its checks need.
type PendingLink struct {
	ID string
	// State is the flow's state, which its return carries.
	State              string
	OwnerUserID        string
	WorkspaceID        string
	Provider           provider.Kind
	DisplayName        string
	SyncConsentVersion string
	ExpiresAt          time.Time

	verifier    string
	redirectURI string
}

// LinkCompletion is what the transaction that creates a link's mailbox
// writes beside it, or checks first.
type LinkCompletion struct {
	// Key is the mailbox's first key pair and the linker's grant at epoch 1,
	// made by their browser at the return (docs/key-scheme.md sections 8
	// and 12.11). Required with a code (ErrNoLinkKey); a return carrying an
	// error needs none.
	Key *workspace.LinkKey
	// Check runs first in the transaction that creates the mailbox, after
	// the check that the person is still active, under that write's own
	// context: where the service re-reads that its starter may still link
	// into the workspace, and that their step-up is still fresh. nil checks
	// nothing more.
	Check func(ctx context.Context, tx *sql.Tx) error
	// SyncConsent is the team's consent to sync the mailbox, given at the
	// return and recorded in that transaction; zero links it with sync off.
	SyncConsent store.MailboxConsent
	// LinkedBy is who links it, for attribution; empty is the starter.
	LinkedBy string
}

// StartLink starts a link at the provider for a person: a state and a PKCE
// pair, the provider's authorization URL for the web client with its
// account chooser and the scopes that name the account's address, and the
// link's row, waiting for its return for flowTTL. A person's waiting link
// for the same provider and workspace is replaced by it, and one with
// MaxLinksWaiting links waiting already gets ErrTooManyLinks. Nothing is
// dialled, and no mailbox, key or grant is written.
func (r *Registry) StartLink(ctx context.Context, s LinkStart) (Link, *AuthFlow, error) {
	if s.OwnerUserID == "" {
		return Link{}, nil, fmt.Errorf("%w: a link at the provider is a person's", ErrFlowUnavailable)
	}
	switch s.Provider {
	case provider.KindGmail, provider.KindMicrosoft:
	default:
		return Link{}, nil, fmt.Errorf("%w: only Gmail and Microsoft mailboxes are linked at the provider", ErrNotOAuth)
	}
	if !r.Availability(s.Provider).Web {
		return Link{}, nil, fmt.Errorf("%w: the web flow for %s needs a web client and MAIL_PUBLIC_URL",
			ErrFlowUnavailable, s.Provider)
	}
	registration, err := r.registration(s.Provider, ClientWeb, r.linkTenant(s.Provider))
	if err != nil {
		return Link{}, nil, err
	}
	redirect := r.publicURL + webRedirectPath
	config, err := configFor(s.Provider, registration, ClientWeb, redirect, true)
	if err != nil {
		return Link{}, nil, err
	}
	state, err := randomState()
	if err != nil {
		return Link{}, nil, err
	}
	flow, err := startAuthCode(s.Provider, config, state, "", true)
	if err != nil {
		return Link{}, nil, err
	}
	now := r.repo.now().UTC().Truncate(time.Second)
	flow.Kind = FlowWeb
	flow.ExpiresAt = now.Add(flowTTL)

	link, err := r.repo.insertLink(ctx, s, newLinkID(), state, flow.Verifier, redirect, now)
	if err != nil {
		return Link{}, nil, err
	}
	return link, flow, nil
}

// linkTenant is the tenant a link of kind is authorized under: the one Add
// gives a Microsoft mailbox, which its grant is then refreshed under.
func (r *Registry) linkTenant(kind provider.Kind) string {
	if kind == provider.KindMicrosoft {
		return r.microsoftTenant()
	}
	return ""
}

// OwnedLink reads a link waiting for its return for the caller completing
// it, without taking it, so the caller's permissions can be checked before
// anything is exchanged. A link somebody else started, one already taken or
// finished, and a state nobody knows are ErrNotFound alike, and nothing of
// them changes; one of the caller's that ran out of time is ErrFlowExpired,
// and is recorded so.
func (r *Registry) OwnedLink(ctx context.Context, state, owner string) (PendingLink, error) {
	link, err := r.repo.ownedLink(ctx, state, owner)
	if errors.Is(err, ErrFlowExpired) {
		if _, expireErr := r.repo.expireLink(context.WithoutCancel(ctx), state, owner); expireErr != nil {
			r.log.Error("recording an expired link at the provider failed", "link", link.ID, "err", expireErr)
		}
	}
	return link, err
}

// CompleteLink finishes a link at the provider from the address its browser
// landed on, for the person who started it, and returns the mailbox it
// created.
//
// The owner is matched in the same statement that takes the link, before
// anything is exchanged: a return from a link somebody else started is not
// found, and leaves it takeable by its starter. A return with a code needs
// the mailbox's key (ErrNoLinkKey), checked before the take too. The take
// waits for the writer LinkTakeTimeout at most: one that could not take the
// link by then has taken nothing (ErrLinkBusy), and the same return may be
// posted again. Once taken, everything runs detached from the request,
// bounded by finishTimeout from the take: the code is single-use. A return
// carrying an error records the refusal. One carrying a code is exchanged
// with the scopes that name the address; the address read (ErrNoAddress); a
// mailbox the workspace holds under it, live or removed, under any spelling
// of one Gmail account, refused (ErrDuplicate, or a *RemovedDuplicateError)
// with nothing stored or revoked; the grant proved by one login as that
// address (checkGrant); and the mailbox, its credential, its key, its
// linker's grant and the team's consent created in one transaction, which
// checks again that the person is active, runs c.Check, and finishes the
// link: one that ended meanwhile (EndLinksTx) creates nothing
// (ErrFlowExpired). Every ending but that one is recorded on the link, with
// the address once the provider named it.
func (r *Registry) CompleteLink(ctx context.Context, rd Redirect, owner string, c LinkCompletion) (Account, error) {
	if rd.State == "" {
		return Account{}, fmt.Errorf("%w: it carries no state parameter; paste the whole address", ErrBadRedirect)
	}
	if rd.Error == "" {
		if c.Key == nil {
			return Account{}, ErrNoLinkKey
		}
		if err := workspace.CheckLinkKey(*c.Key); err != nil {
			return Account{}, err
		}
	}
	link, err := r.takeLink(ctx, rd.State, owner)
	if err != nil {
		return Account{}, err
	}
	// Detached from the request: the link is taken and its code single-use,
	// so a caller that stops waiting must not throw the mailbox away halfway
	// through creating it.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	created, address, err := r.finishLink(finishCtx, link, rd, c)
	if err != nil {
		r.failLink(finishCtx, link, address, err)
		return Account{}, err
	}
	return created, nil
}

// finishLink ends a link whose row has been taken, and returns the mailbox
// it created and the address the provider named, once it named one.
func (r *Registry) finishLink(ctx context.Context, link PendingLink, rd Redirect, c LinkCompletion) (Account, string, error) {
	if rd.Error != "" {
		return Account{}, "", rd.refusal()
	}
	kind, tenant := link.Provider, r.linkTenant(link.Provider)
	registration, err := r.registration(kind, ClientWeb, tenant)
	if err != nil {
		return Account{}, "", err
	}
	config, err := configFor(kind, registration, ClientWeb, link.redirectURI, true)
	if err != nil {
		return Account{}, "", err
	}
	token, err := exchange(ctx, config, rd.Code, link.verifier)
	if err != nil {
		return Account{}, "", err
	}
	// The ID token's times are on the provider's clock, which is the wall
	// clock, whatever the repository's says.
	address, err := identityOf(kind, registration, token, time.Now())
	if err != nil {
		return Account{}, "", err
	}

	profile := provider.ProfileFor(kind)
	a := Account{
		ID:           newAccountID(),
		Email:        address,
		LoginUser:    address,
		DisplayName:  link.DisplayName,
		Provider:     kind,
		AuthKind:     "oauth2",
		OAuthClient:  ClientWeb,
		Tenant:       tenant,
		LinkedBy:     c.LinkedBy,
		WorkspaceID:  link.WorkspaceID,
		SyncConsent:  c.SyncConsent,
		SaveSentCopy: profile.SaveSentDefault,
	}
	applyServerDefaults(&a, AddRequest{})

	// The address is known now: a mailbox the workspace holds already is
	// refused before its server is dialled, and the transaction below asks
	// again, which is the answer that counts.
	ws, err := r.repo.WorkspaceFor(ctx, link.WorkspaceID, link.OwnerUserID)
	if err != nil {
		return Account{}, address, err
	}
	if err := r.repo.duplicateOf(ctx, ws, kind, address); err != nil {
		return Account{}, address, err
	}
	token, err = r.proveGrant(ctx, a, config, token, "link", link.ID)
	if err != nil {
		return Account{}, address, err
	}
	created, err := r.repo.createLinked(ctx, a, link, c.Key, both(r.ownerStillActive(link.OwnerUserID), c.Check), token)
	if err != nil {
		return Account{}, address, err
	}
	return created, address, nil
}

// failLink records a link that ended without its mailbox, unless it ended
// meanwhile another way: why, by a fixed reason, and the address the
// provider named, if it named one (for a duplicate, only while the
// workspace still holds a mailbox under it: Repository.failLink).
func (r *Registry) failLink(ctx context.Context, link PendingLink, address string, cause error) {
	// Detached: the link has ended whether or not the finish that noticed
	// has time left.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	reason := linkFailureReason(cause)
	moved, err := r.repo.failLink(writeCtx, link, address, reason)
	if err != nil {
		r.log.Error("recording a failed link at the provider failed", "link", link.ID, "err", err)
		return
	}
	if moved {
		r.log.Warn("a link at the provider failed", "link", link.ID, "provider", string(link.Provider),
			"reason", reason, "err", cause)
	}
}

// insertLink writes a link waiting for its return, in one transaction: its
// workspace checked (the starter's personal one, stored as none, or a team
// that is open), the starter's waiting link for the same provider and
// workspace replaced, and refused while they have MaxLinksWaiting others.
func (r *Repository) insertLink(ctx context.Context, s LinkStart, id, state, verifier, redirect string, now time.Time,
) (Link, error) {
	expires := now.Add(flowTTL)
	link := Link{ID: id, Provider: s.Provider, State: LinkWaiting, ExpiresAt: expires}
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		into, err := linkWorkspaceTx(ctx, tx, s.WorkspaceID, s.OwnerUserID)
		if err != nil {
			return err
		}
		link.WorkspaceID = into
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_links
			WHERE owner_user_id = ? AND provider = ? AND coalesce(workspace_id, '') = ? AND taken_at = 0 AND finished_at = 0`,
			s.OwnerUserID, string(s.Provider), into); err != nil {
			return fmt.Errorf("account: replace the waiting link: %w", err)
		}
		var (
			waiting int
			oldest  sql.NullInt64
		)
		if err := tx.QueryRowContext(ctx, `SELECT count(*), min(expires_at) FROM oauth_links
			WHERE owner_user_id = ? AND taken_at = 0 AND finished_at = 0 AND expires_at >= ?`,
			s.OwnerUserID, now.Unix()).Scan(&waiting, &oldest); err != nil {
			return fmt.Errorf("account: count the waiting links: %w", err)
		}
		if waiting >= MaxLinksWaiting {
			// The oldest stops counting the second after it expires.
			return &TooManyLinksError{Until: time.Unix(oldest.Int64+1, 0).UTC()}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_links(id, state, owner_user_id, workspace_id, provider,
			oauth_client, pkce_verifier, redirect_uri, display_name, sync_consent_version, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, state, s.OwnerUserID, nullable(into), string(s.Provider), ClientWeb, verifier, redirect,
			s.DisplayName, s.SyncConsentVersion, now.Unix(), expires.Unix()); err != nil {
			if store.IsForeignKey(err) {
				return ErrUnknownOwner
			}
			return fmt.Errorf("account: record the link: %w", err)
		}
		return nil
	})
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

// linkWorkspaceTx is the workspace a link goes into, as its row names it:
// none for the starter's personal workspace, named or not, which the
// transaction that creates the mailbox resolves then, and a team's id for a
// team, open. Another person's personal workspace and the operator's are
// ErrNoWorkspace; whether the starter may link into the team is the
// service's.
func linkWorkspaceTx(ctx context.Context, tx *sql.Tx, workspaceID, owner string) (string, error) {
	w, err := workspaceForTx(ctx, tx, workspaceID, owner)
	if err != nil {
		return "", err
	}
	switch w.Kind {
	case workspace.KindPersonal:
		if w.PersonID != owner {
			return "", ErrNoWorkspace
		}
		return "", nil
	case workspace.KindTeam:
		if err := workspace.RequireOpenTx(ctx, tx, w.ID); err != nil {
			return "", err
		}
		return w.ID, nil
	default:
		return "", ErrNoWorkspace
	}
}

const pendingLinkColumns = `id, state, owner_user_id, coalesce(workspace_id, ''), provider, display_name,
	sync_consent_version, expires_at, pkce_verifier, redirect_uri`

func scanPendingLink(row scanner) (PendingLink, error) {
	var (
		l       PendingLink
		kind    string
		expires int64
	)
	err := row.Scan(&l.ID, &l.State, &l.OwnerUserID, &l.WorkspaceID, &kind, &l.DisplayName, &l.SyncConsentVersion,
		&expires, &l.verifier, &l.redirectURI)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingLink{}, ErrNotFound
	}
	if err != nil {
		return PendingLink{}, fmt.Errorf("account: read the link: %w", err)
	}
	l.Provider = provider.Kind(kind)
	l.ExpiresAt = time.Unix(expires, 0).UTC()
	return l, nil
}

// ownedLink reads a link of owner's waiting for its return, without taking
// it: ErrNotFound for any other, ErrFlowExpired, with the link, for one that
// ran out of time.
func (r *Repository) ownedLink(ctx context.Context, state, owner string) (PendingLink, error) {
	link, err := scanPendingLink(r.store.Reader().QueryRowContext(ctx, `SELECT `+pendingLinkColumns+`
		FROM oauth_links WHERE state = ? AND owner_user_id = ? AND taken_at = 0 AND finished_at = 0`, state, owner))
	if err != nil {
		return PendingLink{}, err
	}
	if r.now().After(link.ExpiresAt) {
		return link, fmt.Errorf("%w at %s; start again", ErrFlowExpired, link.ExpiresAt.Format(time.RFC3339))
	}
	return link, nil
}

// takeLink takes a link of owner's for its return (Repository.takeLink),
// waiting for the writer linkTakeTimeout at most: a take that could not get
// it by then, while the caller still waits, is ErrLinkBusy, and has taken
// nothing.
//
// The bound keeps what the caller checked before the take true until the
// finish is over: a link's starter's step-up is checked, before the take,
// to be fresh for LinkTakeTimeout and finishTimeout more
// (auth.Users.RequireStepUpFor), and the transaction that creates the
// mailbox asks again. It also keeps the route that posts the return within
// its deadline.
func (r *Registry) takeLink(ctx context.Context, state, owner string) (PendingLink, error) {
	takeCtx, cancel := context.WithTimeout(ctx, r.linkTakeTimeout)
	defer cancel()
	link, err := r.repo.takeLink(takeCtx, state, owner)
	if err != nil && ctx.Err() == nil && errors.Is(takeCtx.Err(), context.DeadlineExceeded) {
		return PendingLink{}, fmt.Errorf("%w: %w", ErrLinkBusy, err)
	}
	return link, err
}

// takeLink takes a link of owner's waiting for its return, once
// (takeLinkTx). One of theirs that ran out of time is recorded so, and is
// ErrFlowExpired; anything else is ErrNotFound, and stays as it was.
//
// The moment of the take is read inside its transaction, once the writer is
// this take's: its finish is bounded from then (finishTimeout), and a link
// still finishing is taken for one a restart cut off only linkCutOff after
// it (Link, sweepLinks), however long the take waited for the writer.
func (r *Repository) takeLink(ctx context.Context, state, owner string) (PendingLink, error) {
	var (
		link    PendingLink
		expired bool
	)
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		now := r.now().Unix()
		var err error
		link, err = takeLinkTx(ctx, tx, state, owner, now)
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if expired, err = expireLinkTx(ctx, tx, state, owner, now); err != nil {
			return err
		}
		if !expired {
			return ErrNotFound
		}
		return nil
	})
	switch {
	case err != nil:
		return PendingLink{}, err
	case expired:
		return PendingLink{}, fmt.Errorf("%w; start again", ErrFlowExpired)
	}
	return link, nil
}

// takeLinkTx takes, inside the caller's transaction, a link waiting for its
// return: owner's, matched in the statement that takes it, untaken,
// unfinished and unexpired at now. Its taken_at is now: no return takes it
// again. Any other is ErrNotFound and is left exactly as it was.
func takeLinkTx(ctx context.Context, tx *sql.Tx, state, owner string, now int64) (PendingLink, error) {
	return scanPendingLink(tx.QueryRowContext(ctx, `UPDATE oauth_links SET taken_at = ?1
		WHERE state = ?2 AND owner_user_id = ?3 AND taken_at = 0 AND finished_at = 0 AND expires_at >= ?1
		RETURNING `+pendingLinkColumns, now, state, owner))
}

// expireLink records a link of owner's that ran out of time before its
// return was taken, and reports whether it did.
func (r *Repository) expireLink(ctx context.Context, state, owner string) (bool, error) {
	var expired bool
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		expired, err = expireLinkTx(ctx, tx, state, owner, r.now().Unix())
		return err
	})
	return expired, err
}

func expireLinkTx(ctx context.Context, tx *sql.Tx, state, owner string, now int64) (bool, error) {
	n, err := execCount(ctx, tx, `UPDATE oauth_links SET outcome = 'failed', reason = ?1, finished_at = ?2,
		pkce_verifier = '', display_name = '', sync_consent_version = ''
		WHERE state = ?3 AND owner_user_id = ?4 AND taken_at = 0 AND finished_at = 0 AND expires_at < ?2`,
		ReasonExpired, now, state, owner)
	if err != nil {
		return false, fmt.Errorf("account: record the expired link: %w", err)
	}
	return n == 1, nil
}

// failLink records a link taken and not finished as failed, for reason,
// with the address the provider named, if any, and reports whether it did:
// one that ended meanwhile another way stays as it ended.
//
// A duplicate names its address only while the link's workspace still holds
// a mailbox under it, asked in this transaction: one deleted for good since
// it was found took its failed links with it (forgetFailedLinksTx) only if
// they were failed then, and this one was still finishing. Either way, no
// failed link outlives the mailbox it named.
func (r *Repository) failLink(ctx context.Context, link PendingLink, address, reason string) (bool, error) {
	var moved bool
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		named := address
		if reason == ReasonDuplicate && named != "" {
			held, err := heldUnderTx(ctx, tx, link, named)
			if err != nil {
				return err
			}
			if !held {
				named = ""
			}
		}
		n, err := execCount(ctx, tx, `UPDATE oauth_links SET outcome = 'failed', reason = ?, email = ?, finished_at = ?,
			pkce_verifier = '', display_name = '', sync_consent_version = '' WHERE id = ? AND finished_at = 0`,
			reason, named, r.now().Unix(), link.ID)
		moved = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("account: record the failed link: %w", err)
	}
	return moved, nil
}

// heldUnderTx reports, inside the caller's transaction, whether the
// workspace a link goes into holds a mailbox, live or removed and kept, under
// address, as duplicateOfTx counts one. A workspace the link can no longer
// go into holds none for it.
func heldUnderTx(ctx context.Context, tx *sql.Tx, link PendingLink, address string) (bool, error) {
	w, err := workspaceForTx(ctx, tx, link.WorkspaceID, link.OwnerUserID)
	switch {
	case errors.Is(err, ErrNoWorkspace):
		return false, nil
	case err != nil:
		return false, err
	}
	err = duplicateOfTx(ctx, tx, w.ID, link.Provider, address)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, ErrDuplicate), errors.Is(err, ErrRemovedDuplicate):
		return true, nil
	}
	return false, err
}

// createLinked creates a link's mailbox with its grant: the token sealed
// first, outside the transaction, and then, in one, the mailbox with its
// key and its linker's grant (create, which runs check first), its
// credential, its move to active, and the link finished (finishLinkTx). A
// link that ended meanwhile rolls it all back.
func (r *Repository) createLinked(ctx context.Context, a Account, link PendingLink, key *workspace.LinkKey,
	check func(context.Context, *sql.Tx) error, token *oauth2.Token,
) (Account, error) {
	//nolint:gosec // G117: the result is encrypted before it reaches disk; an ID token is not kept (raw)
	encoded, err := json.Marshal(token)
	if err != nil {
		return Account{}, fmt.Errorf("account: encode token: %w", err)
	}
	sealed, err := r.sealer.Seal(ctx, secrets.Credential(a.ID, "oauth_token"), encoded)
	if err != nil {
		return Account{}, fmt.Errorf("account: seal oauth_token: %w", err)
	}
	a.State = StatePendingAuth
	return r.create(ctx, a, link.OwnerUserID, key, check, func(tx *sql.Tx, at time.Time) ([]events.Event, error) {
		if err := r.putCredential(ctx, tx, a.ID, "oauth_token", sealed, at.Unix()); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET state = ?, state_reason = '', state_changed_at = ?, updated_at = ?
			  WHERE id = ?`, string(StateActive), at.Unix(), at.Unix(), a.ID); err != nil {
			return nil, fmt.Errorf("account: set state: %w", err)
		}
		evs, err := r.journalState(ctx, tx, a.ID, StatePendingAuth, StateActive, "", at)
		if err != nil {
			return nil, err
		}
		return evs, finishLinkTx(ctx, tx, link.ID, a.ID, at)
	})
}

// finishLinkTx records, inside the transaction that created its mailbox, a
// link connected to it: what it kept for its return goes, and it names the
// mailbox and no address. A link that is no longer taken and unfinished,
// ended meanwhile, is ErrFlowExpired, which rolls the mailbox back.
func finishLinkTx(ctx context.Context, tx *sql.Tx, id, accountID string, at time.Time) error {
	n, err := execCount(ctx, tx, `UPDATE oauth_links SET outcome = 'connected', account_id = ?, finished_at = ?,
		pkce_verifier = '', display_name = '', sync_consent_version = '' WHERE id = ? AND taken_at <> 0 AND finished_at = 0`,
		accountID, at.Unix(), id)
	switch {
	case err != nil:
		return fmt.Errorf("account: finish the link: %w", err)
	case n != 1:
		return fmt.Errorf("%w: the link ended while it was finishing", ErrFlowExpired)
	}
	return nil
}

// Link reads a link at the provider for its starter: anybody else's, and one
// already forgotten, is ErrNotFound. Its state is read at now: an untaken
// link past its expiry, or one still finishing past what its finish could
// take, has failed, as the sweep records it.
func (r *Repository) Link(ctx context.Context, id, owner string) (Link, error) {
	var (
		l              Link
		kind, outcome  string
		expires, taken int64
	)
	err := r.store.Reader().QueryRowContext(ctx, `SELECT id, provider, coalesce(workspace_id, ''), expires_at, taken_at,
		outcome, coalesce(account_id, ''), email, reason FROM oauth_links WHERE id = ? AND owner_user_id = ?`,
		id, owner).Scan(&l.ID, &kind, &l.WorkspaceID, &expires, &taken, &outcome, &l.AccountID, &l.Email, &l.Reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Link{}, ErrNotFound
	case err != nil:
		return Link{}, fmt.Errorf("account: read the link: %w", err)
	}
	l.Provider = provider.Kind(kind)
	l.ExpiresAt = time.Unix(expires, 0).UTC()
	now := r.now().Unix()
	switch {
	case outcome == string(LinkConnected):
		l.State = LinkConnected
	case outcome == string(LinkFailed):
		l.State = LinkFailed
	case (taken == 0 && expires < now) || (taken != 0 && taken < now-int64(linkCutOff/time.Second)):
		l.State, l.Reason = LinkFailed, ReasonExpired
	case taken == 0:
		l.State = LinkWaiting
	default:
		l.State = LinkFinishing
	}
	return l, nil
}

// sweepLinks records the links that ended without an answer — untaken past
// their expiry, or taken by a finish a restart cut off — as failed, for
// ReasonExpired, and deletes every link linkKeptAfter past its expiry.
func (r *Repository) sweepLinks(ctx context.Context) error {
	now := r.now().Unix()
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_links SET outcome = 'failed', reason = ?1, finished_at = ?2,
			pkce_verifier = '', display_name = '', sync_consent_version = ''
			WHERE finished_at = 0 AND ((taken_at = 0 AND expires_at < ?2) OR (taken_at <> 0 AND taken_at < ?2 - ?3))`,
			ReasonExpired, now, int64(linkCutOff/time.Second)); err != nil {
			return fmt.Errorf("account: sweep the links: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_links WHERE expires_at < ?`,
			now-int64(linkKeptAfter/time.Second)); err != nil {
			return fmt.Errorf("account: sweep the links: %w", err)
		}
		return nil
	})
}

// forgetFailedLinksTx deletes, inside the transaction that deletes these
// mailboxes for good, the failed links that name one of their addresses,
// under any spelling of one Gmail account: a link refused as a duplicate of
// a mailbox names its address, and never outlives it.
func forgetFailedLinksTx(ctx context.Context, tx *sql.Tx, list string) error {
	mailboxes, err := addressesTx(ctx, tx, `SELECT '', provider, email FROM accounts
		WHERE id IN (SELECT value FROM json_each(?))`, list)
	if err != nil || len(mailboxes) == 0 {
		return err
	}
	failed, err := addressesTx(ctx, tx, `SELECT id, provider, email FROM oauth_links WHERE outcome = 'failed' AND email <> ''`)
	if err != nil {
		return err
	}
	var named []string
	for _, l := range failed {
		for _, m := range mailboxes {
			if sameMailbox(l.kind, l.email, m.kind, m.email) {
				named = append(named, l.id)
				break
			}
		}
	}
	if len(named) == 0 {
		return nil
	}
	ids, err := json.Marshal(named)
	if err != nil {
		return fmt.Errorf("account: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_links WHERE id IN (SELECT value FROM json_each(?))`, string(ids)); err != nil {
		return fmt.Errorf("account: delete the failed links naming the mailboxes: %w", err)
	}
	return nil
}

// address is a row's id, provider and address, as addressesTx reads them.
type address struct {
	id    string
	kind  provider.Kind
	email string
}

// addressesTx runs a query of an id, a provider and an address inside tx.
func addressesTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]address, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("account: read addresses: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var out []address
	for rows.Next() {
		var (
			a    address
			kind string
		)
		if err := rows.Scan(&a.id, &kind, &a.email); err != nil {
			return nil, fmt.Errorf("account: read addresses: %w", err)
		}
		a.kind = provider.Kind(kind)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: read addresses: %w", err)
	}
	return out, nil
}

// execCount runs a statement inside tx and reports how many rows it changed.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func newLinkID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// As for an account id: crypto/rand does not fail in practice.
		panic("account: cannot read random bytes: " + err.Error())
	}
	return "lnk_" + hex.EncodeToString(raw)
}
