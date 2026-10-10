package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/netguard"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// liveListTimeout bounds a folder listing that talks to the mail server.
const liveListTimeout = 45 * time.Second

// Account is how an account is presented to a caller.
type Account struct {
	ID string `json:"id"`
	// WorkspaceID is the workspace the mailbox belongs to, which never
	// changes: a person's personal workspace, a team, or the operator's.
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider"`
	AuthKind    string `json:"auth_kind"`
	State       string `json:"state"`
	StateReason string `json:"state_reason,omitempty"`
	// SyncTier is what the server's capabilities chose, once known. Visible
	// because it explains how quickly flag changes are noticed.
	SyncTier     string `json:"sync_tier,omitempty"`
	SaveSentCopy bool   `json:"save_sent_copy"`
	LastOKAt     int64  `json:"last_ok_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	// Sync is where the account's sync stands, as GET
	// /v1/accounts/{id}/sync reports it.
	Sync AccountSync `json:"sync"`
	// Actions says whether the account has somewhere to archive to and a
	// trash folder, as its folder index shows: a console offers those
	// actions only where they can be done. Whether they may be done is the
	// owner's consent, which is theirs to read.
	Actions AccountActions `json:"actions"`
	// Send says whether the account can send for this caller, consent
	// aside: a console lists it as a From only when it can.
	Send AccountSend `json:"send"`
	// Access is what this caller may do with the mailbox: their grant, as
	// far as their credential's scope reaches.
	Access AccountAccess `json:"access"`
	// MailboxKey is the mailbox's key pair at its current epoch
	// (docs/key-scheme.md section 8), for a person signed in: what a grant of
	// it opens to, which their console checks. Absent for a mailbox without
	// a key, read by the flag alone, and for every API key, which seals and
	// opens nothing.
	MailboxKey *MailboxKeyPair `json:"mailbox_key,omitempty"`
}

// Folder is how a folder is presented.
type Folder struct {
	// ID is the folder's id in the index: what a message search filters
	// by. Only a listing that comes from the index has it; a live listing
	// has nothing indexed to filter.
	ID          int64  `json:"id,omitempty"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role,omitempty"`
	// RoleSource says how the role was decided, which is the difference
	// between "the server told us" and "we matched a localised name".
	RoleSource string `json:"role_source,omitempty"`
	Selectable bool   `json:"selectable"`
	Synced     bool   `json:"synced"`
	Messages   uint32 `json:"messages,omitempty"`
	Unseen     uint32 `json:"unseen,omitempty"`
	// SyncState is set when the listing comes from the index: "new",
	// "initial", "live", "resync", "error" or "disabled". The counts are then
	// what is indexed, not what the server holds, and exist only for folders
	// that are synced.
	SyncState string `json:"sync_state,omitempty"`
}

// AddAccountRequest registers an account.
type AddAccountRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Password    string `json:"password,omitempty"`
	IMAPHost    string `json:"imap_host,omitempty"`
	IMAPPort    int    `json:"imap_port,omitempty"`
	SMTPHost    string `json:"smtp_host,omitempty"`
	SMTPPort    int    `json:"smtp_port,omitempty"`
	SMTPTLS     string `json:"smtp_tls,omitempty"`
	LoginUser   string `json:"login_user,omitempty"`
	Flow        string `json:"flow,omitempty"`
	InitialDays int    `json:"initial_days,omitempty"`
	SaveSent    *bool  `json:"save_sent_copy,omitempty"`
	// WorkspaceID is where the mailbox goes: empty is the person's personal
	// workspace, or the operator workspace for an instance key. A team takes
	// a mailbox from its owners and admins.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// SyncConsentVersion, for a team mailbox, gives the team's consent to
	// sync it with the link, on the team's behalf: it must name the current
	// revision of the sync text (the console shows that text beside the
	// switch). Left out, the mailbox is linked with sync off, until an owner
	// or an admin turns it on (PUT /v1/accounts/{id}/sync). A personal
	// mailbox syncs under its person's own consent, and an operator mailbox
	// is switched on by the operator: neither takes it.
	SyncConsentVersion string `json:"sync_consent_version,omitempty"`
	// PublicKey, Namespace and Grant are the mailbox's first key, which the
	// browser of the person linking it made (docs/key-scheme.md sections 8
	// and 12.11): the public half of the key pair, base64url of 32 bytes; a
	// namespace, a lowercase UUIDv4 no other mailbox uses; and the linker's
	// own grant at epoch 1, base64url of 88 bytes. A person's link carries
	// all three, after a fresh step-up; an instance key's carries none, since
	// an operator mailbox has no key.
	PublicKey string `json:"public_key,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Grant     string `json:"grant,omitempty"`
}

// AuthFlow is what a caller needs to finish consent.
type AuthFlow struct {
	// Flow says what to do with the rest: "web" means send the browser to
	// AuthURL and expect it back at the console's /oauth/return; "loopback"
	// means open AuthURL and wait while the daemon catches the redirect;
	// "pasted" means paste back the address the browser ends on; "device"
	// means show UserCode and VerificationURI.
	Flow string `json:"flow"`
	// AuthURL is where the person must go. Empty for the device flow.
	AuthURL string `json:"auth_url,omitempty"`
	// State ties a pasted redirect back to this flow.
	State string `json:"state,omitempty"`
	// UserCode and VerificationURI are the device flow.
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
}

// AddAccountResult is an account and, when consent is needed, how to give it.
type AddAccountResult struct {
	Account Account   `json:"account"`
	Auth    *AuthFlow `json:"auth,omitempty"`
}

// ListAccounts returns the accounts this caller may see: every one they hold
// a grant on in a workspace they are an active member of, or, for an instance
// key, the operator workspace's. workspaceID narrows to one workspace; empty
// is every one.
func (s *Service) ListAccounts(ctx context.Context, p Principal, workspaceID string) ([]Account, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return nil, err
	}
	if err := s.inWorkspace(ctx, p, workspaceID); err != nil {
		return nil, err
	}
	v := visibility(p)
	v.Workspace = workspaceID
	all, err := s.accounts.Repo().ListVisible(ctx, v)
	if err != nil {
		return nil, E(CodeInternal, "listing accounts failed", err)
	}
	mayAccess := make([]account.Account, 0, len(all))
	for _, a := range all {
		if p.MayAccess(a.ID) {
			mayAccess = append(mayAccess, a)
		}
	}
	return s.present(ctx, p, mayAccess...), nil
}

// GetAccount returns one account: its card, which any grant shows.
func (s *Service) GetAccount(ctx context.Context, p Principal, id string) (Account, error) {
	a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, id, needCard)
	if err != nil {
		return Account{}, err
	}
	return s.present(ctx, p, a)[0], nil
}

// present renders accounts with their sync.
//
// A sync status that cannot be read shows the account as not syncing, with
// a warning in the log, rather than failing: a listing is how a person finds
// the account to fix, and an account just added or authorised must be
// returned whatever the engine says.
func (s *Service) present(ctx context.Context, p Principal, accounts ...account.Account) []Account {
	out := make([]Account, 0, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	permitted, err := s.syncPermitted(ctx, accounts)
	if err != nil {
		s.log.Warn("reading whether sync is on failed; showing it off", "err", err)
		permitted = map[string]bool{}
	}
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	grants, err := s.grantsOf(ctx, p, ids...)
	if err != nil {
		// Shown as no access, which is what the caller is then allowed:
		// every use asks again, and would refuse the same way.
		s.log.Warn("reading the caller's access failed; showing none", "err", err)
		grants = map[string]workspace.Flags{}
	}
	// The mailbox keys and who waits for one, for a person signed in only:
	// a key seals and opens nothing (docs/key-scheme.md section 12.15).
	keyPairs, waiting := s.keysOf(ctx, p, ids)
	// The sender's own name: a message goes out under the name of whoever
	// sends it, whoever linked the mailbox.
	fromName := s.fromName(ctx, p)
	for _, a := range accounts {
		shown := presentAccount(a)
		st, err := s.syncOf(ctx, a.ID, permitted[a.ID])
		if err != nil {
			s.log.Warn("reading an account's sync status failed; showing it off", "account", a.ID, "err", err)
			st = AccountSync{Enabled: permitted[a.ID], State: syncOff}
		}
		held := intersect(grants[a.ID], scopeFlags(p.Scope))
		shown.Sync = st
		shown.Actions = s.actionsOf(ctx, a)
		shown.Access = presentAccess(held)
		shown.Access.WaitingKey = waiting[a.ID]
		if k, ok := keyPairs[a.ID]; ok {
			pair := presentKeyPair(k)
			shown.MailboxKey = &pair
		}
		shown.Send = sendOf(a, held)
		if shown.Send.Available {
			shown.Send.FromName = fromName
		}
		out = append(out, shown)
	}
	return out
}

// keysOf reads, for a person signed in, the key pair at its current epoch of
// each mailbox named that has one, and those on which they wait for the key:
// they hold read, and no grant at its current epoch. Nothing for a key. One
// that cannot be read shows no key, with a warning in the log: every use of
// the mailbox asks again.
func (s *Service) keysOf(ctx context.Context, p Principal, ids []string) (map[string]workspace.KeyPair, map[string]bool) {
	if !p.IsSession() || s.workspaces == nil {
		return nil, nil
	}
	pairs, err := s.workspaces.CurrentKeys(ctx, ids)
	if err != nil {
		s.log.Warn("reading the mailbox keys failed; showing none", "err", err)
		pairs = nil
	}
	waiting, err := s.workspaces.WaitingForKey(ctx, p.UserID, ids)
	if err != nil {
		s.log.Warn("reading who waits for a mailbox key failed; showing nobody", "err", err)
		waiting = nil
	}
	return pairs, waiting
}

// AddAccount links a mailbox and starts consent where needed.
//
// It goes into the workspace the request names, or the person's personal
// workspace, or the operator workspace for an instance key (linkInto says who
// may link where). The person who links it gets read, act and send on it and
// manages it by their role; the consent flow is theirs. A personal mailbox
// syncs under its person's own consent; a team mailbox under its workspace's,
// which the link gives when it names the current sync text, and otherwise an
// owner or an admin gives later.
//
// A person links a mailbox with its key (docs/key-scheme.md sections 8 and
// 12.11): the key pair's public half, its namespace and their own grant,
// which their browser made, written in the transaction that creates the
// mailbox, on the password and the OAuth paths alike. That needs an account
// key of theirs (conflict without one) and a fresh step-up, asked before the
// mail server is dialled and again in that transaction. An instance key links
// an operator mailbox, which has no key.
func (s *Service) AddAccount(ctx context.Context, p Principal, req AddAccountRequest) (AddAccountResult, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return AddAccountResult{}, err
	}
	if len(p.AccountIDs) > 0 {
		// It could never see what it made: its restriction names accounts
		// that existed when it was issued.
		return AddAccountResult{}, E(CodeNotAuthorized,
			"a key restricted to some accounts cannot add new ones", nil)
	}
	add, err := s.checkAddRequest(p, req)
	if err != nil {
		return AddAccountResult{}, err
	}
	link, err := s.linkInto(ctx, p, strings.TrimSpace(req.WorkspaceID), req.SyncConsentVersion)
	if err != nil {
		return AddAccountResult{}, err
	}
	add.WorkspaceID, add.Check, add.SyncConsent = link.workspaceID, link.check, link.consent
	if p.IsSession() {
		// The person's account key, and their step-up before the login a
		// password account costs; the transaction that writes the key asks
		// for the step-up again, which is the one that counts.
		if err := s.requireAccountKey(ctx, p); err != nil {
			return AddAccountResult{}, err
		}
		if err := s.requireStepUp(ctx, p); err != nil {
			return AddAccountResult{}, err
		}
		check := link.check
		add.Check = func(tx *sql.Tx) error {
			if check != nil {
				if err := check(tx); err != nil {
					return err
				}
			}
			return s.stepUpTx(ctx, tx, p)
		}
	}

	created, flow, err := s.accounts.Add(ctx, add)
	if created.ID != "" {
		// The mailbox exists, and its linker reads it, whether or not its
		// consent could be started.
		s.accessChanged()
		if created.SyncConsent.At != 0 {
			s.log.Info("team mailbox linked with its workspace's consent to sync", "account", created.ID,
				"workspace", created.WorkspaceID, "by", p.Actor(), "version", created.SyncConsent.Version)
		}
	}
	if err != nil {
		// The account row may exist in pending_auth even when consent could
		// not be started; that is recoverable through oauth/start.
		return AddAccountResult{}, fromAccount(err, "adding the account failed")
	}

	result := AddAccountResult{Account: s.present(ctx, p, created)[0]}
	if flow != nil {
		result.Auth = presentFlow(flow)
	}
	return result, nil
}

// checkAddRequest validates what can be checked before anything is stored,
// and decides the provider and the consent flow.
//
// Here rather than in the registry so every message is one this layer wrote
// and knows is safe to show: a registry error can carry a database constraint
// or a server's banner, and those stay in the log.
func (s *Service) checkAddRequest(p Principal, req AddAccountRequest) (account.AddRequest, error) {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return account.AddRequest{}, E(CodeBadRequest, "an email address is required", nil)
	}
	if parsed, err := mail.ParseAddress(email); err != nil || parsed.Name != "" || parsed.Address != email {
		return account.AddRequest{}, E(CodeBadRequest, "that is not a valid email address", err)
	}
	kind, icloud, err := chooseProvider(email, req.Provider)
	if err != nil {
		return account.AddRequest{}, err
	}
	add := account.AddRequest{
		Email: email, DisplayName: req.DisplayName, Provider: kind, Password: req.Password,
		IMAPHost: strings.TrimSpace(req.IMAPHost), IMAPPort: req.IMAPPort,
		SMTPHost: strings.TrimSpace(req.SMTPHost), SMTPPort: req.SMTPPort, SMTPTLS: req.SMTPTLS,
		LoginUser: req.LoginUser, InitialDays: req.InitialDays, SaveSentCopy: req.SaveSent,
		LinkerID: p.UserID, LinkedBy: p.Actor(), ICloud: icloud,
	}
	if icloud {
		// Set at all, it is for an iCloud+ custom domain: Apple refuses that
		// address as a sign-in and wants the account's own iCloud address.
		add.LoginUser = strings.TrimSpace(req.LoginUser)
	}

	switch {
	case req.Password != "" && kind == provider.KindMicrosoft:
		// Not a limitation of this server: Exchange Online disabled basic
		// auth for IMAP in 2022 and it cannot be turned back on.
		return account.AddRequest{}, E(CodeBadRequest,
			"Microsoft no longer accepts a password for IMAP; this account must use OAuth", nil)
	case req.Password != "" && !passwordAllowed(p, kind):
		return account.AddRequest{}, E(CodeBadRequest,
			"Gmail accounts are connected with Google sign-in here, not with a password", nil)
	case icloud && req.Password == "":
		// Apple has no OAuth for this server to use: its XOAUTH2 is for its
		// partner programme.
		return account.AddRequest{}, E(CodeBadRequest,
			"an iCloud account needs an app-specific password, made at account.apple.com", nil)
	case icloud && add.NamesServers():
		return account.AddRequest{}, E(CodeBadRequest,
			"an iCloud account always uses Apple's servers; imap_host, imap_port, smtp_host, smtp_port "+
				"and smtp_tls cannot be set (name provider imap to use other servers)", nil)
	case icloud && add.LoginUser != "" && !bareAddress(add.LoginUser):
		// Apple's SMTP signs in only with a whole address, and one login
		// serves both servers: a name alone would pass IMAP and fail sending.
		return account.AddRequest{}, E(CodeBadRequest,
			"an iCloud login_user must be a whole address, such as name@icloud.com", nil)
	case kind == provider.KindIMAP && p.UserID != "" && googleMailbox(email, add.IMAPHost):
		// Gmail by another name — a Workspace domain, or "other IMAP" with an
		// app password: the same reasons apply, and so does Gmail's profile.
		return account.AddRequest{}, E(CodeBadRequest,
			"Gmail accounts are connected with Google sign-in here, not with a password", nil)
	case req.Password == "" && kind == provider.KindIMAP:
		return account.AddRequest{}, E(CodeBadRequest, "a generic IMAP account needs a password", nil)
	case !icloud && kind == provider.KindIMAP && (add.IMAPHost == "" || add.SMTPHost == ""):
		return account.AddRequest{}, E(CodeBadRequest, "a generic IMAP account needs imap_host and smtp_host", nil)
	}
	if p.UserID != "" && req.InitialDays != 0 && req.InitialDays != account.PersonInitialDays {
		// What a person, or a team, consented to is the last 90 days. Only
		// the operator, for an operator mailbox, may reach further back.
		return account.AddRequest{}, Ef(CodeBadRequest, nil,
			"a person's mailbox is synced back %d days; initial_days cannot be changed", account.PersonInitialDays)
	}
	switch req.SMTPTLS {
	case "", "implicit", "starttls":
	default:
		return account.AddRequest{}, E(CodeBadRequest, "smtp_tls must be implicit or starttls", nil)
	}
	for name, port := range map[string]int{"imap_port": req.IMAPPort, "smtp_port": req.SMTPPort} {
		if port < 0 || port > 65535 {
			return account.AddRequest{}, Ef(CodeBadRequest, nil, "%s must be between 1 and 65535", name)
		}
	}

	if req.Password == "" {
		flow, err := s.chooseFlow(p, kind, req.Flow)
		if err != nil {
			return account.AddRequest{}, err
		}
		add.Flow = flow
	}
	switch {
	case p.IsSession():
		key, err := linkKeyOf(req)
		if err != nil {
			return account.AddRequest{}, err
		}
		add.Key = key
	case req.PublicKey != "" || req.Namespace != "" || req.Grant != "":
		return account.AddRequest{}, errOperatorKey
	}
	return add, nil
}

// googleMailbox reports whether an address or an IMAP host is Google's.
func googleMailbox(email, imapHost string) bool {
	return account.GuessProvider(email) == provider.KindGmail || provider.IsGmailServer(provider.Caps{}, imapHost)
}

// bareAddress reports whether s is an address and nothing else: no display
// name, no angle brackets, no surrounding text.
func bareAddress(s string) bool {
	parsed, err := mail.ParseAddress(s)
	return err == nil && parsed.Name == "" && parsed.Address == s
}

// chooseProvider decides what an account is: the provider the request
// named, or the one its address implies. iCloud is not a provider.Kind — it
// is stored as generic IMAP on Apple's servers — so it comes back as imap and
// a flag.
func chooseProvider(email, named string) (kind provider.Kind, icloud bool, err error) {
	switch named {
	case "":
		if account.IsICloudAddress(email) {
			return provider.KindIMAP, true, nil
		}
		return account.GuessProvider(email), false, nil
	case account.ICloud:
		return provider.KindIMAP, true, nil
	}
	kind, err = provider.ParseKind(named)
	if err != nil {
		return "", false, E(CodeBadRequest, "provider must be gmail, microsoft, icloud or imap", err)
	}
	return kind, false, nil
}

// StartOAuth begins consent again, for an account that needs it: whoever
// manages it may re-authorise it, and the attempt is theirs alone.
func (s *Service) StartOAuth(ctx context.Context, p Principal, id, flowKind string) (*AuthFlow, error) {
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, id, needManage)
	if err != nil {
		return nil, err
	}
	if !a.UsesOAuth() {
		return nil, E(CodeBadRequest, "this account signs in with a password, not OAuth", nil)
	}
	kind, err := s.chooseFlow(p, a.Provider, flowKind)
	if err != nil {
		return nil, err
	}
	flow, err := s.accounts.StartAuth(ctx, id, kind, p.UserID)
	if err != nil {
		return nil, fromAccount(err, "starting consent failed")
	}
	return presentFlow(flow), nil
}

// CompleteOAuth finishes consent from the redirect the browser landed on:
// the console's /oauth/return page posting its own address, or a person
// pasting what their address bar ended on.
//
// Everything that decides whether this caller may finish this flow runs
// before the code is exchanged, and none of it consumes the flow. The flow
// must be one this caller started — a consent link sent to somebody else
// cannot be redeemed by whoever sent it — and its account one this caller may
// still administer. Either failing is not_found, like a flow that never
// existed, so a state cannot be probed for.
func (s *Service) CompleteOAuth(ctx context.Context, p Principal, redirectURL string) (Account, error) {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return Account{}, err
	}
	if strings.TrimSpace(redirectURL) == "" {
		return Account{}, E(CodeBadRequest, "paste the whole address the browser ended on", nil)
	}
	redirect, err := account.ParseRedirect(redirectURL)
	if err != nil {
		return Account{}, E(CodeBadRequest,
			"that is not the address the provider sent the browser back to; paste the whole address", err)
	}
	if redirect.State == "" {
		return Account{}, E(CodeBadRequest, "that address carries no state parameter; paste the whole address", nil)
	}

	pending, err := s.accounts.PendingFlow(ctx, redirect.State, p.UserID)
	if err != nil {
		return Account{}, fromCompletion(err)
	}
	if _, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, pending.AccountID, needManage); err != nil {
		return Account{}, err
	}

	a, err := s.accounts.CompleteFlow(ctx, redirect, p.UserID)
	if err != nil {
		return Account{}, fromCompletion(err)
	}
	return s.present(ctx, p, a)[0], nil
}

// fromCompletion renders a consent attempt that could not be finished. The
// account already records why; these messages say what to do next.
func fromCompletion(err error) error {
	switch {
	case errors.Is(err, account.ErrNotFound), errors.Is(err, account.ErrFlowExpired),
		errors.Is(err, account.ErrOwnerInactive), errors.Is(err, account.ErrStarterLostAccess):
		// A person disabled, or who stopped managing the mailbox,
		// mid-exchange has their attempts dropped, and this one is answered
		// as those are.
		return E(CodeNotFound, "that authorisation attempt is no longer open; start again", err)
	case errors.Is(err, account.ErrConsentDeclined):
		return E(CodeBadRequest, "consent was declined at the provider; start again to connect the account", err)
	case errors.Is(err, account.ErrClientRejected):
		return E(CodeInternal,
			"the provider rejected this server's OAuth client; its operator has to check the configuration", err)
	case errors.Is(err, account.ErrNotConfigured):
		return E(CodeConflict, "the OAuth client this attempt started with is no longer configured here", err)
	case errors.Is(err, account.ErrNoRefreshToken):
		return E(CodeBadRequest,
			"the provider issued no refresh token, so the account would stop within the hour; start again", err)
	case errors.Is(err, account.ErrScopeMissing):
		return E(CodeBadRequest,
			"the provider did not grant access to the mailbox; start again and allow access to email", err)
	case errors.Is(err, account.ErrMailboxRefused):
		return E(CodeBadRequest,
			"the mail server refused this authorization for the mailbox; "+
				"start again and sign in as the address being connected", err)
	case errors.Is(err, account.ErrExchangeFailed), errors.Is(err, account.ErrProviderRefused):
		return E(CodeBadRequest, "the provider did not accept the authorisation; start again", err)
	default:
		return E(CodeInternal, "completing the authorisation failed", err)
	}
}

// RemoveAccountRequest names the mailbox again, as a guard against removing
// the wrong one: Confirm must repeat its id.
type RemoveAccountRequest struct {
	Confirm string
}

// RemoveAccount forgets an account and everything indexed for it. The
// request repeats its id in Confirm, or nothing is removed. Who: an owner or
// an admin of its team, for a team mailbox; its person, for a personal one;
// an instance key with the admin scope, for an operator mailbox.
func (s *Service) RemoveAccount(ctx context.Context, p Principal, id string, req RemoveAccountRequest) error {
	if req.Confirm != id {
		return E(CodeBadRequest, "deleting a mailbox needs its id repeated in confirm; nothing was removed", nil)
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, id, needCard)
	if err != nil {
		return err
	}
	check := func(tx *sql.Tx) error { return mayRemoveTx(ctx, tx, p, a) }
	if err := s.precheck(ctx, check); err != nil {
		return err
	}
	err = s.accounts.RemoveChecked(ctx, id, check)
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, account.ErrNotFound):
		return E(CodeNotFound, "no such account", err)
	case err != nil:
		return E(CodeInternal, "removing the account failed", err)
	}
	s.log.Info("mailbox removed", "account", a.ID, "workspace", a.WorkspaceID, "by", p.Actor())
	s.accessChanged()
	s.compact(ctx)
	return nil
}

// mayRemoveTx re-reads, inside the transaction that removes a mailbox, that
// the caller may: an instance key for an operator mailbox (visibility let
// only that reach it), the person of a personal one, an active owner or
// admin of a team's.
func mayRemoveTx(ctx context.Context, tx *sql.Tx, p Principal, a account.Account) error {
	switch {
	case a.WorkspaceID == workspace.OperatorID:
		if p.IsInstance() {
			return nil
		}
		return errNoAccount
	case p.IsInstance():
		return errNoAccount
	case a.OwnerUserID != "":
		if a.OwnerUserID == p.UserID {
			return nil
		}
		return errNoAccount
	}
	me, err := callerTx(ctx, tx, p, a.WorkspaceID)
	if err != nil {
		return errNoAccount
	}
	if !adminOf(me) {
		return errRemoveTeamMailbox
	}
	return nil
}

var errRemoveTeamMailbox = E(CodeNotAuthorized, "only an owner or an admin of the team removes its mailboxes", nil)

// link is where a new mailbox goes, the check its transaction runs, and the
// workspace's consent to sync it, when the link gives one.
type link struct {
	workspaceID string
	check       func(*sql.Tx) error
	consent     store.MailboxConsent
}

// linkInto decides where a new mailbox goes and who may put it there.
//
// A person links into their personal workspace, and into a team they are an
// active owner or admin of: a link puts a mailbox's index in a space the
// team shares, so a member asks one of them. With syncVersion naming the
// current revision of the sync text, a link into a team also gives the
// team's consent to sync it, on the team's behalf; any other revision is
// refused, and none links it with sync off. An instance key links into the
// operator workspace only. The role is read again inside the transaction
// that creates the mailbox.
func (s *Service) linkInto(ctx context.Context, p Principal, workspaceID, syncVersion string) (link, error) {
	syncVersion = strings.TrimSpace(syncVersion)
	if p.IsInstance() {
		switch {
		case workspaceID != "" && workspaceID != workspace.OperatorID:
			return link{}, E(CodeNotAuthorized, "an instance key links mailboxes into the operator workspace only", nil)
		case syncVersion != "":
			return link{}, errSyncVersionNotTeam
		}
		return link{workspaceID: workspaceID}, nil
	}
	if workspaceID == "" {
		if syncVersion != "" {
			return link{}, errSyncVersionNotTeam
		}
		return link{}, nil
	}
	if s.workspaces == nil {
		return link{}, errNoWorkspace
	}
	w, err := s.workspaces.Get(ctx, workspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return link{}, errNoWorkspace
	case err != nil:
		return link{}, E(CodeInternal, "reading the workspace failed", err)
	}
	var consent store.MailboxConsent
	switch {
	case syncVersion == "":
	case w.Kind != workspace.KindTeam:
		return link{}, errSyncVersionNotTeam
	case syncVersion != s.consent.Sync:
		return link{}, errNotCurrentText(s.consent.Sync)
	default:
		consent = store.MailboxConsent{At: s.now().Unix(), By: p.Actor(), Version: syncVersion}
	}
	check := func(tx *sql.Tx) error {
		m, err := workspace.MemberTx(ctx, tx, w.ID, p.UserID)
		switch {
		case errors.Is(err, workspace.ErrNotMember):
			return errNoWorkspace
		case err != nil:
			return err
		case !m.Active():
			return errNoWorkspace
		case w.Kind == workspace.KindTeam && !adminOf(m):
			return errLinkTeam
		}
		return nil
	}
	if err := s.precheck(ctx, check); err != nil {
		return link{}, fromWorkspace(err, "reading the workspace failed")
	}
	return link{workspaceID: w.ID, check: check, consent: consent}, nil
}

var errSyncVersionNotTeam = E(CodeBadRequest,
	"sync_consent_version is a team's consent to sync a mailbox linked into it; a personal mailbox syncs under "+
		"its person's own consent, and an operator mailbox is switched on by the operator", nil)

var errLinkTeam = E(CodeNotAuthorized,
	"only an owner or an admin of the team links mailboxes into it; ask one of them", nil)

// ListFolders returns the account's folders with their roles resolved, for a
// caller who may read it.
//
// Once sync has listed the account's folders, the index answers, with its
// counts. Before that — sync not yet consented to, switched off, or not yet
// through its first listing — it asks the server, which is also how a person
// confirms an account works right after authorising it.
func (s *Service) ListFolders(ctx context.Context, p Principal, id string) ([]Folder, error) {
	a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, id, needRead)
	if err != nil {
		return nil, err
	}
	if indexed, err := s.indexedFolders(ctx, a); err != nil || indexed != nil {
		return indexed, err
	}
	if a.State == account.StateNeedsReauth {
		return nil, needsReauth(fmt.Errorf("%w: %s", provider.ErrNeedsReauth, a.StateReason))
	}
	if a.State == account.StatePendingAuth {
		return nil, E(CodeConflict, "this account has not been authorised yet", nil)
	}

	mailbox, err := s.accounts.Mailbox(ctx, id)
	if err != nil {
		return nil, fromMailbox(err)
	}

	listCtx, cancel := ctxWithDeadline(ctx, liveListTimeout)
	defer cancel()

	var (
		folders []provider.Folder
		caps    provider.Caps
		opened  bool
	)
	list := func(ctx context.Context, session provider.Session) error {
		opened = true
		caps = session.Caps()
		// Counters cost nothing extra where the server supports LIST-STATUS,
		// and are worth having the first time someone looks at an account.
		var err error
		folders, err = session.ListFolders(ctx, true)
		return err
	}
	if runner, ok := s.sync.(InteractiveRunner); ok {
		// The engine's connection, so the account stays within its budget.
		err = runner.Interactive(listCtx, id, list)
	} else {
		err = interactiveOnce(listCtx, mailbox, list)
	}
	if err != nil {
		var known *Error
		if errors.As(err, &known) {
			return nil, err
		}
		what := "listing the mailbox's folders failed"
		if !opened {
			what = "opening the mailbox to list its folders failed"
		}
		s.mailboxFailed(a, what, err)
		return nil, fromMailbox(err)
	}

	// As the sync engine reads it: a Gmail mailbox connected as generic IMAP
	// never syncs All Mail, Starred or Important either.
	profile := mailbox.Profile().ForServer(caps, a.IMAPHost)
	out := make([]Folder, 0, len(folders))
	for _, f := range folders {
		role, source := provider.ResolveRole(f, profile, a.FolderOverrides)
		entry := Folder{
			Name:        f.Name,
			DisplayName: displayName(f.Name, f.Delim),
			Role:        string(role),
			RoleSource:  source,
			Selectable:  f.Selectable,
			Synced:      profile.SyncsFolder(f, role),
		}
		if f.Status != nil {
			entry.Messages = f.Status.NumMessages
			entry.Unseen = f.Status.NumUnseen
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return folderLess(out[i], out[j]) })
	return out, nil
}

// mailboxFailed records a failure a person sees because of their mail server.
//
// At warn, not with the transport's debug line for a refused request: the
// person is told only which of seven codes it was, and whoever they ask has
// nothing else to go on. The account and a short class to filter by, this
// server's reading of the failure, and the server's own status line when it
// sent one; the logger masks both.
func (s *Service) mailboxFailed(a account.Account, what string, err error) {
	s.log.Warn(what, "account", a.ID, "provider", a.ProviderName(), "class", provider.Class(err),
		"err", err, "server", provider.ServerReply(err))
}

// indexedFolders is the folder listing from the index, or nil when the
// index cannot answer: sync is off for the account, or has not listed its
// folders yet.
func (s *Service) indexedFolders(ctx context.Context, a account.Account) ([]Folder, error) {
	if s.store == nil {
		return nil, nil
	}
	enabled, err := s.syncEnabled(ctx, a)
	if err != nil || !enabled {
		return nil, err
	}
	listed, err := s.store.FolderListSynced(ctx, a.ID)
	if err != nil {
		return nil, E(CodeInternal, "reading the folder index failed", err)
	}
	if !listed {
		return nil, nil
	}
	rows, err := s.store.Folders(ctx, a.ID)
	if err != nil {
		return nil, E(CodeInternal, "reading the folder index failed", err)
	}
	out := make([]Folder, 0, len(rows))
	for _, f := range rows {
		entry := Folder{
			ID: f.ID, Name: f.Name, DisplayName: f.DisplayName, Role: string(f.Role), RoleSource: f.RoleSource,
			Selectable: f.Selectable, Synced: f.Synced, SyncState: f.SyncState,
		}
		if f.Synced {
			entry.Messages, entry.Unseen = count32(f.LocalCount), count32(f.UnseenCount)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return folderLess(out[i], out[j]) })
	return out, nil
}

// count32 is a counter as the folder listing shows it.
func count32(n int) uint32 {
	switch {
	case n <= 0:
		return 0
	case uint64(n) > math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(n) //nolint:gosec // G115: bounded above
}

// displayName strips the provider's container prefix, so a person sees "Sent
// Mail" rather than "[Gmail]/Sent Mail".
func displayName(name string, delim rune) string {
	if delim == 0 {
		return name
	}
	if idx := strings.LastIndex(name, string(delim)); idx >= 0 && idx+1 < len(name) {
		return name[idx+1:]
	}
	return name
}

// folderLess orders folders the way a person expects: the inbox, then the
// other roles, then everything else alphabetically.
func folderLess(a, b Folder) bool {
	ra, rb := roleOrder(a.Role), roleOrder(b.Role)
	if ra != rb {
		return ra < rb
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}

func roleOrder(role string) int {
	switch provider.FolderRole(role) {
	case provider.RoleInbox:
		return 0
	case provider.RoleSent:
		return 1
	case provider.RoleDrafts:
		return 2
	case provider.RoleArchive, provider.RoleAll:
		return 3
	case provider.RoleJunk:
		return 4
	case provider.RoleTrash:
		return 5
	case provider.RoleNone:
		return 7
	default:
		return 6
	}
}

func parseFlow(s string) (account.FlowKind, error) {
	switch account.FlowKind(s) {
	case account.FlowLoopback, account.FlowPasted, account.FlowDevice, account.FlowWeb:
		return account.FlowKind(s), nil
	default:
		return "", Ef(CodeBadRequest, nil, "unknown flow %q (want web, loopback, pasted or device)", s)
	}
}

// fromAccount maps a registry failure onto the transport vocabulary. Anything
// it does not recognise is internal, with a fixed message: the registry's own
// text can quote a database constraint. A refusal this layer wrote — the check
// a link runs again inside the transaction that creates the mailbox — goes
// through unchanged.
func fromAccount(err error, what string) error {
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, account.ErrNoWorkspace), errors.Is(err, workspace.ErrNotMember):
		// The workspace, or the linker's place in it, went while the
		// request ran: what the check above would have said.
		return E(CodeNotFound, "no such workspace", err)
	case errors.Is(err, account.ErrNotFound):
		return E(CodeNotFound, "no such account", err)
	case errors.Is(err, account.ErrDuplicate):
		// Generic on purpose: the address may be connected by someone else,
		// and whose it is is not this caller's business.
		return E(CodeConflict, "that address is already connected", err)
	case errors.Is(err, account.ErrOwnerInactive):
		// Switched off while the request ran: the answer a revoked session
		// gets, which is what the transport's re-check will say anyway.
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	case errors.Is(err, provider.ErrNeedsReauth):
		return needsReauth(err)
	case errors.Is(err, account.ErrNotConfigured):
		return E(CodeConflict, "no OAuth client is configured for that provider on this server", err)
	case errors.Is(err, account.ErrFlowUnavailable):
		return E(CodeBadRequest, "that consent flow is not available on this server", err)
	case errors.Is(err, account.ErrNotOAuth):
		return E(CodeBadRequest, "this account signs in with a password, not OAuth", err)
	case errors.Is(err, account.ErrPrivateHost):
		return E(CodeBadRequest,
			"that mail server is on a loopback, private or link-local network, which this server does not connect to", err)
	case errors.Is(err, account.ErrLoginRefused):
		return E(CodeBadRequest, "the mail server refused that address and password", err)
	case errors.Is(err, account.ErrUnreachable):
		return E(CodeBadRequest, "the mail server could not be reached; check the host names and ports", err)
	case errors.Is(err, account.ErrClientRejected):
		return E(CodeInternal,
			"the provider rejected this server's OAuth client; its operator has to check the configuration", err)
	case errors.Is(err, account.ErrOperatorKey):
		return errOperatorKey
	default:
		// The mailbox key the link writes, refused in its transaction.
		return fromKeys(err, what)
	}
}

// fromMailbox renders a failure to reach an account's mailbox: the provider's
// own vocabulary, plus the two ways this server's configuration can be the
// cause.
func fromMailbox(err error) error {
	switch {
	case errors.Is(err, netguard.ErrPrivateAddress):
		return E(CodeConflict,
			"the account's mail server is on a loopback, private or link-local network, which this server does not connect to", err)
	case errors.Is(err, account.ErrNotConfigured):
		return E(CodeConflict,
			"the OAuth client that authorised this account is not configured here; re-authorise it", err)
	case errors.Is(err, account.ErrClientRejected):
		return E(CodeInternal,
			"the provider rejected this server's OAuth client; its operator has to check the configuration", err)
	case errors.Is(err, account.ErrNoCredentials):
		// An OAuth account whose first consent was declined, refused or
		// abandoned: in error, with no grant ever stored. Nothing about the
		// mail server is wrong, and nothing is for anyone but the person to
		// fix by authorising it.
		return E(CodeConflict, "this account has not been authorised yet", err)
	default:
		return fromProvider(err)
	}
}

func presentAccount(a account.Account) Account {
	out := Account{
		ID: a.ID, WorkspaceID: a.WorkspaceID, Email: a.Email, DisplayName: a.DisplayName,
		Provider: a.ProviderName(), AuthKind: a.AuthKind,
		State: string(a.State), StateReason: a.StateReason,
		SyncTier: a.SyncTierResolved, SaveSentCopy: a.SaveSentCopy,
		LastError: a.LastError,
	}
	if !a.LastOKAt.IsZero() {
		out.LastOKAt = a.LastOKAt.Unix()
	}
	if !a.CreatedAt.IsZero() {
		out.CreatedAt = a.CreatedAt.Unix()
	}
	return out
}

func presentFlow(f *account.AuthFlow) *AuthFlow {
	if f == nil {
		return nil
	}
	out := &AuthFlow{
		Flow:    string(f.Kind),
		AuthURL: f.AuthURL, State: f.State,
		UserCode: f.UserCode, VerificationURI: f.VerificationURI,
	}
	if !f.ExpiresAt.IsZero() {
		out.ExpiresAt = f.ExpiresAt.Unix()
	}
	return out
}

// interactiveOnce opens an interactive connection for one call and logs it
// out after.
func interactiveOnce(ctx context.Context, mailbox provider.Mailbox, fn func(context.Context, provider.Session) error) error {
	session, err := mailbox.Open(ctx, provider.RoleInteractive)
	if err != nil {
		return err
	}
	//nolint:errcheck // the call either succeeded or already failed
	defer func() { _ = session.Close() }()
	return fn(ctx, session)
}
