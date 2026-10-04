package account

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/netguard"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// flowTTL is how long a consent flow stays open. Long enough to find a
// password manager, short enough that an abandoned attempt does not sit in the
// database with a usable PKCE verifier in it.
const flowTTL = 10 * time.Minute

// loginCheckTimeout bounds proving a password account works before it is
// stored: resolving its hosts, dialing and logging in.
const loginCheckTimeout = 30 * time.Second

// stopWait bounds how long removing an account or restarting its consent
// waits for the previous attempt's listener to close.
const stopWait = 5 * time.Second

// Registry owns the accounts: registration, consent, credentials, and the
// mailbox objects the rest of the server talks to.
//
// It holds one token source per account, kept alive for as long as the account
// exists. That lifetime is not incidental: oauth2 uses the context it was
// given at construction for every later refresh, so a source built inside a
// request stops refreshing the moment that request ends.
type Registry struct {
	repo         *Repository
	google       OAuthClient
	microsoft    OAuthClient
	googleWeb    OAuthClient
	microsoftWeb OAuthClient
	publicURL    string
	deviceCode   bool
	allowPrivate bool
	spoolDir     string
	debug        io.Writer
	insecure     bool
	log          *slog.Logger
	// baseCtx outlives every request and is cancelled only when the daemon
	// stops.
	baseCtx context.Context

	mu        sync.Mutex
	mailboxes map[string]provider.Mailbox
	sources   map[string]*tokenSource
	// grantMu orders storing a new grant against recording that the mail
	// server refused the old one, so a refusal noticed late cannot stop an
	// account whose new grant was stored meanwhile. Taken before mu, never
	// while holding it.
	grantMu sync.Mutex
	// waiting is the consent attempt the daemon is completing by itself for
	// each account — a loopback listener or a device-code poll — so that
	// removing the account or starting again can stop it. One per account:
	// two would each hold a listener and each get a say in the account's
	// state.
	waiting map[string]*waiter

	// ownerCheck is who may still have something stored for them; see
	// CheckOwnersWith. Guarded by mu.
	ownerCheck OwnerCheck
	// grantSlot is where a new grant's login check runs; see CheckGrantsIn.
	// Guarded by mu.
	grantSlot ConnectionSlot

	// redirect dials another IMAP address in place of an account's. Nothing
	// in the daemon sets it: the package's own tests do, through
	// export_test.go, to stand an in-process server in for a provider whose
	// servers are constants and cannot be overridden, such as iCloud's.
	redirect map[string]string
}

// waiter is one attempt the daemon is completing by itself.
type waiter struct {
	// state is the attempt's OAuth state, which tells it apart from a later
	// attempt on the same account.
	state  string
	cancel context.CancelFunc
	// done is closed once the attempt's goroutine has returned and its
	// listener, if it had one, is closed.
	done chan struct{}
}

// RegistryOptions configure the registry.
type RegistryOptions struct {
	// Google and Microsoft are the installed (public) clients the loopback,
	// pasted and device flows use.
	Google    OAuthClient
	Microsoft OAuthClient
	// GoogleWeb and MicrosoftWeb are the console's confidential clients,
	// which redirect to PublicURL. A web client without a PublicURL offers
	// nothing.
	GoogleWeb    OAuthClient
	MicrosoftWeb OAuthClient
	PublicURL    string
	// DeviceCode offers Microsoft's device-code flow. Off by default: Entra
	// security defaults block it, and a flow that fails for most tenants is
	// worse than no flow.
	DeviceCode bool
	// AllowPrivate lets accounts point at loopback, private, link-local and
	// CGNAT addresses. Off, every IMAP and SMTP connection is checked at dial
	// time, on the resolved address.
	AllowPrivate bool
	SpoolDir     string
	// Debug tees the raw IMAP protocol. It carries the XOAUTH2 line, so
	// configuration refuses it outside development.
	Debug io.Writer
	// AllowInsecureAuth permits authenticating over an unencrypted
	// connection. Nothing in the daemon sets this: it exists so tests can
	// drive the in-process IMAP server, which speaks plain TCP.
	AllowInsecureAuth bool
	// Log receives what the background consent paths cannot return to
	// anyone: why a loopback or device flow failed. Nil discards it.
	Log *slog.Logger
	// RedirectIMAP dials another address in place of an account's IMAP
	// server, keyed by host:port. Nothing in the daemon sets it: it exists so
	// tests can stand an in-process server in for a provider whose servers
	// are constants — Gmail's, Microsoft's, Apple's — which finishing a
	// consent now dials, to prove the grant opens the mailbox.
	RedirectIMAP map[string]string
}

// NewRegistry builds the registry. ctx bounds the daemon, not a request.
func NewRegistry(ctx context.Context, repo *Repository, opts RegistryOptions) *Registry {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var redirect map[string]string
	if len(opts.RedirectIMAP) > 0 {
		redirect = make(map[string]string, len(opts.RedirectIMAP))
		for from, to := range opts.RedirectIMAP {
			redirect[from] = to
		}
	}
	return &Registry{
		redirect:     redirect,
		repo:         repo,
		google:       opts.Google,
		microsoft:    opts.Microsoft,
		googleWeb:    opts.GoogleWeb,
		microsoftWeb: opts.MicrosoftWeb,
		publicURL:    strings.TrimSuffix(opts.PublicURL, "/"),
		deviceCode:   opts.DeviceCode,
		allowPrivate: opts.AllowPrivate,
		spoolDir:     opts.SpoolDir,
		debug:        opts.Debug,
		insecure:     opts.AllowInsecureAuth,
		log:          log,
		baseCtx:      ctx,
		mailboxes:    map[string]provider.Mailbox{},
		sources:      map[string]*tokenSource{},
		waiting:      map[string]*waiter{},
	}
}

// OwnerCheck reports, inside a transaction, why a person may no longer have
// anything stored on their behalf — disabled, or gone — or nil if they may.
type OwnerCheck func(ctx context.Context, tx *sql.Tx, userID string) error

// CheckOwnersWith installs the rule for who is still a person things may be
// stored for. The rule is not this package's: people belong to internal/auth,
// and internal/service, which knows both, installs it once, before anything
// can run. Without one, nothing is checked.
//
// It runs in the same transaction that stores a consent's grant or creates a
// person's account. Disabling someone is a transaction too, and the database
// has one writer, so the two are ordered: a code that was being exchanged
// while the person was switched off stores nothing, where a check made before
// the exchange would have let it through.
func (r *Registry) CheckOwnersWith(check OwnerCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ownerCheck = check
}

// ownerStillActive is the check for one person, for a repository write to
// run in its transaction; nil when there is nobody to check.
func (r *Registry) ownerStillActive(ctx context.Context, userID string) func(*sql.Tx) error {
	r.mu.Lock()
	check := r.ownerCheck
	r.mu.Unlock()
	if userID == "" || check == nil {
		return nil
	}
	return func(tx *sql.Tx) error {
		if err := check(ctx, tx, userID); err != nil {
			return fmt.Errorf("%w: %w", ErrOwnerInactive, err)
		}
		return nil
	}
}

// ConnectionSlot runs check in place of an account's interactive connection:
// that connection is logged out first, and nothing else takes its place
// until check returns.
type ConnectionSlot func(ctx context.Context, accountID string, check func(context.Context) error) error

// CheckGrantsIn installs where proving a new grant runs. The daemon hands it
// the sync engine's interactive slot, so that re-authorising an account whose
// worker holds its sync and idle connections logs the engine's interactive
// connection out first: the check is the account's third connection, never a
// fourth. It cannot run on that connection — it has to log in with the new
// grant. Without a slot, the check simply runs.
func (r *Registry) CheckGrantsIn(slot ConnectionSlot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantSlot = slot
}

// Repo exposes the repository for read-only callers.
func (r *Registry) Repo() *Repository { return r.repo }

// OnChange registers fn to hear about every account created, removed, or
// changing state or folder overrides, once the change has committed; see
// Repository.OnChange for what fn must not do. The daemon points this at the
// sync engine, which is how a consent finishing in the background or a grant
// found dead starts or stops a worker without anyone polling.
func (r *Registry) OnChange(fn func(accountID string)) { r.repo.OnChange(fn) }

// Availability is which consent flows a provider can offer on this daemon.
// Which of them a given caller is offered is decided in internal/service.
type Availability struct {
	// Installed is the CLI's installed client: the loopback and pasted
	// flows.
	Installed bool
	// Web is the console's web client together with a public origin to
	// redirect to.
	Web bool
	// Device is the device-code flow: Microsoft only, and only when the
	// deployment turned it on.
	Device bool
}

// OAuth reports whether any flow at all can authorise an account.
func (a Availability) OAuth() bool { return a.Installed || a.Web || a.Device }

// Availability reports what the configured clients allow for a provider.
func (r *Registry) Availability(kind provider.Kind) Availability {
	switch kind {
	case provider.KindGmail:
		return Availability{
			Installed: r.google.ClientID != "",
			Web:       r.googleWeb.ClientID != "" && r.publicURL != "",
		}
	case provider.KindMicrosoft:
		return Availability{
			Installed: r.microsoft.ClientID != "",
			Web:       r.microsoftWeb.ClientID != "" && r.publicURL != "",
			Device: r.deviceCode && r.microsoft.ClientID != "" &&
				provider.ProfileFor(kind).DeviceCodeSupported,
		}
	}
	return Availability{}
}

// Errors adding a password account, all found before anything is stored.
var (
	// ErrLoginRefused is a mail server that refused the address and password.
	ErrLoginRefused = errors.New("account: the mail server refused the login")
	// ErrUnreachable is a mail server that could not be found or reached.
	ErrUnreachable = errors.New("account: the mail server could not be reached")
	// ErrPrivateHost is a host on a loopback, private or link-local network,
	// which this daemon does not connect to unless told it may.
	ErrPrivateHost = errors.New("account: the mail server is on a private network")
)

// AddRequest describes an account to register.
type AddRequest struct {
	Email       string
	DisplayName string
	Provider    provider.Kind
	// Password is for generic IMAP, iCloud's app-specific password included;
	// Microsoft refuses it outright and Gmail only accepts it for accounts
	// that have turned off modern auth.
	Password string
	// IMAPHost and the rest default per provider.
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
	SMTPTLS  string
	// LoginUser overrides the SASL identity, which on some generic servers is
	// not the address — nor on iCloud for an iCloud+ custom domain, where
	// Apple accepts only the account's own iCloud address.
	LoginUser string
	// Flow is how consent will be collected.
	Flow FlowKind
	// OwnerUserID is the person connecting the account; empty for an
	// instance key. The consent flow is bound to them too.
	OwnerUserID string
	// InitialDays is the initial sync window; zero means the default.
	InitialDays int
	// SaveSentCopy overrides the provider default. Nil keeps it.
	SaveSentCopy *bool
	// ICloud makes the account iCloud Mail: generic IMAP, with a password,
	// on Apple's servers. It names no servers of its own.
	ICloud bool
}

// Add registers an account and, for OAuth providers, starts consent.
//
// A password account is proved first: its hosts are checked, and it logs in,
// before anything is written. An account that is stored "active" and then
// never syncs because of a typo in the password is a worse answer than a
// refusal while the person is still looking at the form.
//
// An OAuth account's row is written first, in pending_auth. An account that
// exists but has not been authorised is a state the API can describe and a
// person can retry; a consent flow with nothing behind it is not.
func (r *Registry) Add(ctx context.Context, req AddRequest) (Account, *AuthFlow, error) {
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return Account{}, nil, fmt.Errorf("account: %q is not a valid email address", req.Email)
	}
	kind := req.Provider
	if kind == "" {
		kind = GuessProvider(req.Email)
	}
	// Without a password it would be stored and sent to OAuth consent, which
	// Apple does not offer this server.
	if req.ICloud && (kind != provider.KindIMAP || req.NamesServers() || req.Password == "") {
		return Account{}, nil, errors.New(
			"account: an iCloud account is generic IMAP on Apple's servers, with a password, and names no others")
	}
	profile := provider.ProfileFor(kind)

	a := Account{
		ID:           newAccountID(),
		Email:        strings.TrimSpace(req.Email),
		DisplayName:  req.DisplayName,
		Provider:     kind,
		LoginUser:    orDefault(req.LoginUser, strings.TrimSpace(req.Email)),
		OwnerUserID:  req.OwnerUserID,
		InitialDays:  req.InitialDays,
		SaveSentCopy: profile.SaveSentDefault,
	}
	if req.SaveSentCopy != nil {
		a.SaveSentCopy = *req.SaveSentCopy
	}
	applyServerDefaults(&a, req)

	if kind == provider.KindMicrosoft {
		a.Tenant = orDefault(r.microsoft.Tenant, "common")
	}

	switch {
	case req.Password != "":
		if kind == provider.KindMicrosoft {
			// Not a limitation of this server: Exchange Online disabled basic
			// auth for IMAP in 2022 and it cannot be turned back on.
			return Account{}, nil, errors.New(
				"account: Microsoft no longer accepts a password for IMAP; this account must use OAuth")
		}
		a.AuthKind = "password"
	default:
		a.AuthKind = "oauth2"
	}

	// A duplicate would be refused by the insert anyway; asking first saves
	// dialing somebody's mail server to find that out.
	if _, err := r.repo.GetByEmail(ctx, a.Email); err == nil {
		return Account{}, nil, ErrDuplicate
	}

	checkCtx, cancel := context.WithTimeout(ctx, loginCheckTimeout)
	defer cancel()
	// Only hosts somebody typed. The providers' own are constants, and
	// resolving them here would make adding a Gmail account depend on DNS.
	if err := r.checkHosts(checkCtx, req.IMAPHost, req.SMTPHost); err != nil {
		return Account{}, nil, err
	}
	if a.AuthKind == "password" {
		if err := r.checkLogin(checkCtx, a, req.Password); err != nil {
			return Account{}, nil, err
		}
	}

	// The login check above can take seconds; the person may have been
	// switched off meanwhile.
	created, err := r.repo.create(ctx, a, r.ownerStillActive(ctx, a.OwnerUserID))
	if err != nil {
		return Account{}, nil, err
	}

	if created.AuthKind == "password" {
		if err := r.repo.SavePassword(ctx, created.ID, req.Password); err != nil {
			return Account{}, nil, err
		}
		if err := r.repo.SetState(ctx, created.ID, StateActive, ""); err != nil {
			return Account{}, nil, err
		}
		created.State = StateActive
		return created, nil, nil
	}

	flow, err := r.StartAuth(ctx, created.ID, req.Flow, req.OwnerUserID)
	if err != nil {
		// Leave the account in pending_auth rather than deleting it: the
		// person can retry consent without retyping everything, and an
		// account that vanished on a transient failure is worse.
		return created, nil, err
	}
	return created, flow, nil
}

// checkHosts refuses hosts on private networks, early and by name, so the
// answer can say which. The dial-time check is the one that holds; this one
// only makes the refusal readable.
func (r *Registry) checkHosts(ctx context.Context, hosts ...string) error {
	if r.allowPrivate {
		return nil
	}
	for _, host := range hosts {
		if strings.TrimSpace(host) == "" {
			continue
		}
		switch err := netguard.CheckHost(ctx, nil, host); {
		case errors.Is(err, netguard.ErrPrivateAddress):
			return fmt.Errorf("%w: %s", ErrPrivateHost, host)
		case err != nil:
			return fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
	}
	return nil
}

// checkLogin dials the account's IMAP server and logs in with the password,
// through exactly the code that will later sync it.
func (r *Registry) checkLogin(ctx context.Context, a Account, password string) error {
	mb, err := imapprovider.New(r.providerConfig(a, provider.Credentials{User: a.LoginUser, Password: password}))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	session, err := mb.Open(ctx, provider.RoleInteractive)
	if err != nil {
		// The person sees only which of three answers it was; what the
		// server said is for whoever has to explain it.
		r.log.Warn("the mail server did not accept a new password account",
			"account", a.ID, "provider", a.ProviderName(), "class", provider.Class(err),
			"err", err, "server", provider.ServerReply(err))
	}
	switch {
	case errors.Is(err, netguard.ErrPrivateAddress):
		return fmt.Errorf("%w: %w", ErrPrivateHost, err)
	case errors.Is(err, provider.ErrAuthFailed), errors.Is(err, provider.ErrNeedsReauth),
		errors.Is(err, provider.ErrNotConnected):
		return fmt.Errorf("%w: %w", ErrLoginRefused, err)
	case err != nil:
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	//nolint:errcheck // the login worked, which is all this was for
	_ = session.Close()
	return nil
}

// StartAuth begins or restarts consent for an account.
//
// A new attempt replaces whatever attempt the account had: its listener or
// poll is stopped and its pending row dropped, so exactly one flow at a time
// decides where the account stands. owner is who is starting it — empty for
// an instance key — and is the only caller the redirect will be accepted from.
func (r *Registry) StartAuth(ctx context.Context, accountID string, kind FlowKind, owner string) (*AuthFlow, error) {
	a, err := r.repo.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !a.UsesOAuth() {
		return nil, fmt.Errorf("%w: %s authenticates with a password", ErrNotOAuth, a.Email)
	}
	if kind == "" {
		kind = FlowLoopback
	}

	state, err := randomState()
	if err != nil {
		return nil, err
	}
	if err := r.supersede(ctx, accountID); err != nil {
		return nil, err
	}

	var flow *AuthFlow
	switch kind {
	case FlowDevice:
		flow, err = r.startDeviceFlow(ctx, a, state, owner)
	case FlowWeb:
		flow, err = r.startWebFlow(ctx, a, state, owner)
	case FlowLoopback, FlowPasted:
		flow, err = r.startBrowserFlow(ctx, a, kind, state, owner)
	default:
		err = fmt.Errorf("%w: unknown flow %q", ErrFlowUnavailable, kind)
	}
	if err != nil {
		return nil, err
	}

	// An account that failed or lost its grant is waiting on this attempt
	// now, and says so; one that works keeps working while it runs.
	if _, err := r.repo.Transition(ctx, accountID, []State{StateNeedsReauth, StateError}, StatePendingAuth, ""); err != nil {
		return nil, err
	}
	return flow, nil
}

// startWebFlow sends the browser to the provider with the console's web
// client, and back to the console. Nothing waits for it here: the console
// posts the redirect to the callback endpoint, with its own credential.
func (r *Registry) startWebFlow(ctx context.Context, a Account, state, owner string) (*AuthFlow, error) {
	if !r.Availability(a.Provider).Web {
		return nil, fmt.Errorf("%w: the web flow for %s needs a web client and MAIL_PUBLIC_URL", ErrFlowUnavailable, a.Provider)
	}
	redirect := r.publicURL + webRedirectPath
	config, err := r.clientConfig(a, ClientWeb, redirect)
	if err != nil {
		return nil, err
	}
	flow, err := startAuthCode(a.Provider, config, state, a.Email)
	if err != nil {
		return nil, err
	}
	flow.AccountID = a.ID
	flow.Kind = FlowWeb
	flow.ExpiresAt = r.repo.now().Add(flowTTL)

	if err := r.repo.SaveFlow(ctx, PendingFlow{
		State: state, AccountID: a.ID, OwnerUserID: owner, Flow: FlowWeb, Verifier: flow.Verifier,
		RedirectURI: redirect, ExpiresAt: flow.ExpiresAt,
	}); err != nil {
		return nil, err
	}
	return flow, nil
}

func (r *Registry) startBrowserFlow(ctx context.Context, a Account, kind FlowKind, state, owner string) (*AuthFlow, error) {
	// A real listener in both cases. The two flows differ only in how the
	// caller waits: with a browser on this machine the redirect arrives here
	// and the flow finishes by itself; with a browser elsewhere the
	// connection fails, the address bar holds the code, and the person pastes
	// it back. Inventing a redirect nothing listens on would give the
	// provider a URI with no port, which Google rejects.
	listener, err := listenLoopback(a.Provider, "/oauth/callback", state)
	if err != nil {
		return nil, err
	}
	redirect := listener.RedirectURI()

	config, err := r.clientConfig(a, ClientInstalled, redirect)
	if err != nil {
		r.closeLoopback(listener)
		return nil, err
	}
	flow, err := startAuthCode(a.Provider, config, state, a.Email)
	if err != nil {
		r.closeLoopback(listener)
		return nil, err
	}
	flow.AccountID = a.ID
	flow.Kind = kind
	flow.ExpiresAt = r.repo.now().Add(flowTTL)

	if err := r.repo.SaveFlow(ctx, PendingFlow{
		State: state, AccountID: a.ID, OwnerUserID: owner, Flow: kind, Verifier: flow.Verifier,
		RedirectURI: redirect, ExpiresAt: flow.ExpiresAt,
	}); err != nil {
		r.closeLoopback(listener)
		return nil, err
	}

	// Outlives this request: the person still has to sign in.
	//nolint:contextcheck // bounded by the flow's own deadline, not the request's
	r.launch(a.ID, state, flow.ExpiresAt, func(ctx context.Context) {
		r.awaitLoopback(ctx, a.ID, state, listener)
	})
	return flow, nil
}

// awaitLoopback completes a browser flow when its redirect arrives.
//
// A redirect with somebody else's state never reaches here: the listener
// answers it itself and keeps waiting. What does arrive is this flow's
// answer, whatever it is, and every way it can end is written to the
// account, because nobody is waiting on this goroutine to hear it: the
// person is looking at a browser tab, and the console at the account.
func (r *Registry) awaitLoopback(ctx context.Context, accountID, state string, listener *loopbackServer) {
	// Close makes its own bounded shutdown context: this one may be spent.
	//nolint:errcheck,contextcheck // the flow is over either way
	defer func() { _ = listener.Close() }()

	result, err := listener.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			//nolint:contextcheck // the flow's context is spent; recording the expiry needs its own
			r.expire(accountID, state)
		}
		// Cancelled: the account was removed, consent was restarted, or the
		// daemon is stopping. None of those is the attempt failing.
		return
	}

	// The person has answered. From here the flow's own deadline no longer
	// applies — an exchange that straddles it must not be cut off and then
	// reported as an expiry — and neither does a cancellation: the code is
	// single-use, and throwing it away halfway through storing the grant
	// would cost the person their consent.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	pending, err := r.repo.TakeFlow(ctx, state)
	switch {
	case errors.Is(err, ErrNotFound):
		// Finished or replaced another way — a pasted redirect, a restart —
		// while this tab was on its way back.
		result.outcome <- err
		return
	case err != nil:
		r.fail(ctx, accountID, err)
		result.outcome <- err
		return
	}
	if result.err != nil {
		r.fail(ctx, accountID, result.err)
		result.outcome <- result.err
		return
	}
	_, err = r.finish(ctx, pending, result.redirect)
	result.outcome <- err
}

func (r *Registry) startDeviceFlow(ctx context.Context, a Account, state, owner string) (*AuthFlow, error) {
	if !r.Availability(a.Provider).Device {
		return nil, fmt.Errorf("%w: the device-code flow is not available for %s here "+
			"(Microsoft only, and only with MAIL_MICROSOFT_DEVICE_CODE=true)", ErrFlowUnavailable, a.Provider)
	}
	config, err := r.clientConfig(a, ClientInstalled, "")
	if err != nil {
		return nil, err
	}
	flow, err := startDevice(ctx, config)
	if err != nil {
		return nil, err
	}
	flow.AccountID = a.ID
	flow.Kind = FlowDevice
	flow.State = state
	if flow.ExpiresAt.IsZero() {
		flow.ExpiresAt = r.repo.now().Add(flowTTL)
	}

	if err := r.repo.SaveFlow(ctx, PendingFlow{
		State: state, AccountID: a.ID, OwnerUserID: owner, Flow: FlowDevice,
		DeviceCode: flow.DeviceCode, ExpiresAt: flow.ExpiresAt,
	}); err != nil {
		return nil, err
	}
	// Outlives this request: the person is typing a code on another device.
	//nolint:contextcheck // bounded by the device code's own expiry, not the request's
	r.launch(a.ID, state, flow.ExpiresAt, func(ctx context.Context) {
		r.awaitDevice(ctx, a.ID, state, config, flow)
	})
	return flow, nil
}

// awaitDevice polls until the person approves on the other device, and
// records every other ending too.
func (r *Registry) awaitDevice(ctx context.Context, accountID, state string, config *oauth2.Config, flow *AuthFlow) {
	token, err := pollDevice(ctx, config, flow)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		//nolint:contextcheck // the flow's context is spent; recording the expiry needs its own
		r.expire(accountID, state)
		return
	case errors.Is(err, context.Canceled):
		return
	}
	// As for a loopback redirect: the answer is in, and recording it — the
	// grant proved against the mailbox, then stored — must not depend on the
	// poll's deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err != nil {
		// Declined, expired at the provider, or refused: the attempt is
		// over either way, and its row goes with it.
		if _, takeErr := r.repo.TakeFlow(ctx, state); errors.Is(takeErr, ErrNotFound) {
			return
		}
		r.fail(ctx, accountID, err)
		return
	}
	pending, err := r.repo.TakeFlow(ctx, state)
	if err != nil && !errors.Is(err, ErrFlowExpired) {
		// Replaced or removed while the person was approving: this grant
		// belongs to an attempt nobody is waiting for any more.
		return
	}
	a, err := r.repo.Get(ctx, pending.AccountID)
	if err != nil {
		r.fail(ctx, accountID, err)
		return
	}
	if token, err = r.checkGrant(ctx, a, config, token); err != nil {
		r.fail(ctx, accountID, err)
		return
	}
	if err := r.storeGrant(ctx, pending.AccountID, token, ClientInstalled, pending.OwnerUserID); err != nil {
		r.fail(ctx, accountID, err)
	}
}

// launch runs an attempt the daemon completes by itself, on a context of the
// daemon's bounded by the attempt's deadline, and registers it so it can be
// stopped. An attempt the account already had is cancelled: there is only
// ever one.
func (r *Registry) launch(accountID, state string, deadline time.Time, run func(context.Context)) {
	ctx, cancel := context.WithDeadline(r.baseCtx, deadline)
	w := &waiter{state: state, cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	previous := r.waiting[accountID]
	r.waiting[accountID] = w
	r.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}

	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			if r.waiting[accountID] == w {
				delete(r.waiting, accountID)
			}
			r.mu.Unlock()
			close(w.done)
		}()
		run(ctx)
	}()
}

// stopWaiting ends the attempt an account's daemon-side flow is running, and
// waits for its listener to close, so that once an account is removed nothing
// is left answering on its behalf.
func (r *Registry) stopWaiting(accountID string) {
	r.mu.Lock()
	w := r.waiting[accountID]
	delete(r.waiting, accountID)
	r.mu.Unlock()
	w.stop()
}

// stopAttempt is stopWaiting for one attempt: if the account has moved on to
// a newer one meanwhile, that one is somebody else's and keeps running.
func (r *Registry) stopAttempt(accountID, state string) {
	r.mu.Lock()
	w := r.waiting[accountID]
	if w == nil || w.state != state {
		r.mu.Unlock()
		return
	}
	delete(r.waiting, accountID)
	r.mu.Unlock()
	w.stop()
}

// stop cancels the attempt and waits, a bounded while, for its listener to
// close.
func (w *waiter) stop() {
	if w == nil {
		return
	}
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(stopWait):
	}
}

// supersede ends an account's current attempt before a new one starts.
func (r *Registry) supersede(ctx context.Context, accountID string) error {
	r.stopWaiting(accountID)
	return r.repo.DeleteFlows(ctx, accountID)
}

// PendingFlow reads a flow for the caller completing it, without consuming
// it, so the caller's other permissions can be checked against the account
// before anything is exchanged. A flow somebody else started is not found.
func (r *Registry) PendingFlow(ctx context.Context, state, owner string) (PendingFlow, error) {
	pending, err := r.repo.OwnedFlow(ctx, state, owner)
	if errors.Is(err, ErrFlowExpired) {
		// Expired is final: take it, so the account says so.
		if _, takeErr := r.repo.TakeOwnedFlow(ctx, state, owner); !errors.Is(takeErr, ErrNotFound) {
			r.fail(ctx, pending.AccountID, err)
		}
	}
	return pending, err
}

// CompleteFlow finishes a flow from the address its browser landed on, for
// the caller who started it — the console's /oauth/return page, or somebody
// pasting what the address bar ended on.
//
// The owner is matched in the same statement that consumes the row, before
// any code is exchanged. A redirect from a flow someone else started is not
// found and leaves the flow untouched, which is the whole defence against a
// consent link sent to a victim: their approval cannot be redeemed by anyone
// but the person the flow belongs to.
func (r *Registry) CompleteFlow(ctx context.Context, rd Redirect, owner string) (Account, error) {
	if rd.State == "" {
		return Account{}, fmt.Errorf("%w: it carries no state parameter; paste the whole address", ErrBadRedirect)
	}
	pending, err := r.repo.TakeOwnedFlow(ctx, rd.State, owner)
	switch {
	case errors.Is(err, ErrFlowExpired):
		r.fail(ctx, pending.AccountID, err)
		return Account{}, err
	case err != nil:
		return Account{}, err
	}
	// A pasted flow also has a listener waiting in case the browser was on
	// this machine after all. The row is gone, so it could do nothing now;
	// stopping it just returns the port.
	r.stopWaiting(pending.AccountID)

	// Detached from the request: the consent has happened and the code is
	// single-use, so a caller that stops waiting must not be able to throw
	// the grant away halfway through storing it.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	return r.finish(finishCtx, pending, rd)
}

// finish ends a flow whose row has been taken: a refusal is recorded against
// the account, a code is exchanged for a grant with the client the flow ran
// under.
func (r *Registry) finish(ctx context.Context, pending PendingFlow, rd Redirect) (Account, error) {
	if rd.Error != "" {
		err := rd.refusal()
		r.fail(ctx, pending.AccountID, err)
		return Account{}, err
	}
	a, err := r.repo.Get(ctx, pending.AccountID)
	if err != nil {
		return Account{}, err
	}
	client := pending.Flow.client()
	config, err := r.clientConfig(a, client, pending.RedirectURI)
	if err != nil {
		r.fail(ctx, a.ID, err)
		return Account{}, err
	}
	token, err := exchange(ctx, config, rd.Code, pending.Verifier)
	if err != nil {
		r.fail(ctx, a.ID, err)
		return Account{}, err
	}
	token, err = r.checkGrant(ctx, a, config, token)
	if err != nil {
		r.fail(ctx, a.ID, err)
		return Account{}, err
	}
	if err := r.storeGrant(ctx, a.ID, token, client, pending.OwnerUserID); err != nil {
		return Account{}, err
	}
	return r.repo.Get(ctx, a.ID)
}

// grantCheckTimeout bounds proving a new grant opens its mailbox: dialing,
// authenticating, and one refresh and retry if the first token is refused.
const grantCheckTimeout = 30 * time.Second

// finishTimeout bounds everything after a consent is answered, detached from
// whoever is waiting: exchanging the code, the grant check, and storing the
// grant, for which the check always leaves persistTimeout. The callback route
// gives its caller 45 s, and the loopback page waits a little longer than
// this, so both hear how it ended.
const finishTimeout = grantCheckTimeout + persistTimeout

// checkGrant proves a new grant opens the mailbox it was given for, before
// the account is called connected, and returns the token to store — which a
// refresh during the check may have replaced.
//
// A token endpoint that issues a grant says nothing about whether the mail
// server will take it. The person may have picked another account on the
// provider's page than the address typed here — the token is then someone
// else's, and XOAUTH2 names the typed address — or the mailbox may not take
// OAuth at all (IMAP turned off for it); either way the grant refreshes
// happily forever and every login fails. So the check logs in once, through
// exactly the code that will later sync the account, as the address the
// account will log in as.
//
// Only a refusal fails the consent: ErrMailboxRefused, and nothing is stored
// — an unusable grant kept around would only make the account look
// connected. Anything else, a server that is down or slow or throttling, is
// not the grant's fault, and the grant is kept: a mail server's bad minute
// must not cost a person their consent. Either way the log says why.
//
// ctx is already detached from the request: the code has been spent. The
// check leaves the time the grant still needs to be stored.
func (r *Registry) checkGrant(ctx context.Context, a Account, config *oauth2.Config, token *oauth2.Token) (*oauth2.Token, error) {
	deadline := time.Now().Add(grantCheckTimeout)
	if outer, ok := ctx.Deadline(); ok && outer.Add(-persistTimeout).Before(deadline) {
		deadline = outer.Add(-persistTimeout)
	}
	checkCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	held := &heldGrant{token: token}
	// Built on the check's context, and gone with it: nothing keeps this
	// source once the check is over.
	source := newTokenSource(checkCtx, a.ID, config, token, held)
	check := func(ctx context.Context) error {
		return r.openOnce(ctx, a, provider.Credentials{User: a.LoginUser, Tokens: source})
	}
	r.mu.Lock()
	slot := r.grantSlot
	r.mu.Unlock()
	var err error
	if slot != nil {
		err = slot(checkCtx, a.ID, check)
	} else {
		err = check(checkCtx)
	}
	switch {
	case err == nil:
		return held.latest(), nil
	case errors.Is(err, provider.ErrNeedsReauth), errors.Is(err, provider.ErrAuthFailed):
		r.log.Warn("the mail server refused a new authorization; nothing is stored",
			"account", a.ID, "provider", a.ProviderName(), "class", provider.Class(err),
			"err", err, "server", provider.ServerReply(err))
		return nil, fmt.Errorf("%w: %w", ErrMailboxRefused, err)
	default:
		r.log.Warn("could not prove a new authorization opens the mailbox; keeping it",
			"account", a.ID, "provider", a.ProviderName(), "class", provider.Class(err),
			"err", err, "server", provider.ServerReply(err))
		return held.latest(), nil
	}
}

// openOnce opens one interactive session with some credentials and closes it.
func (r *Registry) openOnce(ctx context.Context, a Account, creds provider.Credentials) error {
	mb, err := imapprovider.New(r.providerConfig(a, creds))
	if err != nil {
		return err
	}
	session, err := mb.Open(ctx, provider.RoleInteractive)
	if err != nil {
		return err
	}
	//nolint:errcheck // the login worked, which is all this was for
	_ = session.Close()
	return nil
}

// heldGrant is where a grant under check keeps a token refreshed meanwhile:
// in memory, until the check decides whether it is stored at all. A dead
// grant found by the refresh is the check's refusal to report, not a state
// to write on an account that may still have a working grant of its own.
type heldGrant struct {
	mu    sync.Mutex
	token *oauth2.Token
}

var _ TokenStore = (*heldGrant)(nil)

func (h *heldGrant) SaveToken(_ context.Context, _ string, token *oauth2.Token) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
	return nil
}

func (h *heldGrant) MarkNeedsReauth(context.Context, string, string) error { return nil }

func (h *heldGrant) latest() *oauth2.Token {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.token
}

// expire records an attempt that ran out of time, if it is still the
// account's attempt: whoever takes the expired row — this, the sweeper, a
// late callback — is the one that records it.
func (r *Registry) expire(accountID, state string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.baseCtx), persistTimeout)
	defer cancel()
	if _, err := r.repo.TakeFlow(ctx, state); errors.Is(err, ErrNotFound) {
		return
	}
	r.fail(ctx, accountID, fmt.Errorf("%w before consent was given", ErrFlowExpired))
}

// SweepFlows removes expired consent attempts and records each as failed.
func (r *Registry) SweepFlows(ctx context.Context) error {
	expired, err := r.repo.SweepFlows(ctx)
	if err != nil {
		return err
	}
	for _, accountID := range expired {
		r.fail(ctx, accountID, ErrFlowExpired)
	}
	return nil
}

// fail records a failed consent attempt on an account that was waiting for
// it. Only pending_auth moves: an account that works keeps working when a
// re-consent is declined, and one that already failed keeps its first reason.
// A cancellation is not a failure — the attempt was replaced or removed on
// purpose — and is not recorded.
func (r *Registry) fail(ctx context.Context, accountID string, cause error) {
	if errors.Is(cause, context.Canceled) {
		return
	}
	// Detached: the account's state has changed whether or not the request
	// that noticed is still around to hear about it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	reason := failureReason(cause)
	moved, err := r.repo.Transition(writeCtx, accountID, []State{StatePendingAuth}, StateError, reason)
	if err != nil {
		r.log.Error("recording a failed authorization failed", "account", accountID, "err", err)
		return
	}
	if moved {
		r.log.Warn("authorization failed", "account", accountID, "reason", reason, "err", cause)
	}
}

// storeGrant stores what a consent produced, unless owner — who started it —
// has been disabled or deleted since.
func (r *Registry) storeGrant(ctx context.Context, accountID string, token *oauth2.Token, client, owner string) error {
	r.grantMu.Lock()
	defer r.grantMu.Unlock()
	// The cached mailbox, if any, holds a token source built around the old
	// token — and possibly the old client. It is dropped before the new
	// grant is written, so a connection still opening with the old one
	// cannot write a refreshed token over it, and again after, so the next
	// use picks up both.
	r.forget(accountID)
	if err := r.repo.saveGrant(ctx, accountID, token, client, r.ownerStillActive(ctx, owner)); err != nil {
		return err
	}
	r.forget(accountID)
	return nil
}

// Mailbox returns the provider mailbox for an account, building it once.
func (r *Registry) Mailbox(ctx context.Context, accountID string) (provider.Mailbox, error) {
	r.mu.Lock()
	if mb, ok := r.mailboxes[accountID]; ok {
		r.mu.Unlock()
		return mb, nil
	}
	r.mu.Unlock()

	a, err := r.repo.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if a.State == StateNeedsReauth {
		return nil, fmt.Errorf("%w: %s", provider.ErrNeedsReauth, a.StateReason)
	}

	creds, err := r.credentials(ctx, a)
	if err != nil {
		return nil, err
	}
	imapMailbox, err := imapprovider.New(r.providerConfig(a, creds))
	if err != nil {
		return nil, err
	}
	var mb provider.Mailbox = imapMailbox
	if a.UsesOAuth() {
		mb = &guardedMailbox{Mailbox: imapMailbox, registry: r, accountID: a.ID}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Another goroutine may have built one meanwhile; one mailbox per account
	// keeps the connection budget meaningful.
	if existing, ok := r.mailboxes[accountID]; ok {
		return existing, nil
	}
	if source, ok := creds.Tokens.(*tokenSource); ok && source.retired.Load() {
		// The grant it was built on was replaced while it was being built:
		// good for this one use, not worth keeping.
		return mb, nil
	}
	r.mailboxes[accountID] = mb
	return mb, nil
}

// guardedMailbox is an OAuth account's mailbox, which notices when its grant
// stops working.
//
// The token source records a refresh token that died at the identity
// provider. It cannot see the other way a grant dies: the token refreshes
// fine and the mail server refuses it, a freshly refreshed one included.
// Only the mailbox sees that, and without this the account would stay active
// while every use of it failed — with the console offering nothing to fix
// it.
//
// The IMAP server's refusal, not the submission server's: a Microsoft 365
// mailbox with SMTP AUTH turned off refuses every token at submission while
// IMAP takes the same grant, and parking the account there would take its
// reading away each time its owner tried to send. A send the submission
// server refuses fails on its own; the account's state stays IMAP's to say.
type guardedMailbox struct {
	provider.Mailbox
	registry  *Registry
	accountID string
}

func (m *guardedMailbox) Open(ctx context.Context, role provider.Role) (provider.Session, error) {
	session, err := m.Mailbox.Open(ctx, role)
	if err != nil && errors.Is(err, provider.ErrNeedsReauth) {
		m.registry.grantRefused(ctx, m, err)
	}
	return session, err
}

// grantRefused moves an active account whose grant the mail server refused to
// needs_reauth, so the only thing that can fix it — the person consenting
// again — is what the account asks for. Only active moves: an account the
// token source already marked keeps the reason it was given.
//
// And only while m is still the account's mailbox. A consent stored while
// this refusal was on its way replaced the grant m was built on and dropped
// m; the refusal is about a grant that is no longer there.
func (r *Registry) grantRefused(ctx context.Context, m *guardedMailbox, cause error) {
	accountID := m.accountID
	r.grantMu.Lock()
	defer r.grantMu.Unlock()
	r.mu.Lock()
	current := r.mailboxes[accountID] == provider.Mailbox(m)
	r.mu.Unlock()
	if !current {
		r.log.Warn("the mail server refused an authorization the account no longer uses",
			"account", accountID, "provider", string(m.Kind()), "class", provider.Class(cause),
			"err", cause, "server", provider.ServerReply(cause))
		return
	}
	// Detached: the account's state has changed whether or not the caller
	// that noticed is still waiting.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	moved, err := r.repo.Transition(writeCtx, accountID, []State{StateActive}, StateNeedsReauth, ReasonTokenRejected)
	if err != nil {
		r.log.Error("recording a refused authorization failed", "account", accountID, "err", err)
		return
	}
	if moved {
		r.log.Warn("the mail server refused the account's authorization; it needs consent again",
			"account", accountID, "provider", string(m.Kind()), "class", provider.Class(cause),
			"err", cause, "server", provider.ServerReply(cause))
		// The token source is spent; the next consent builds a new one.
		r.forget(accountID)
	}
}

// providerConfig is how to reach an account's servers with some credentials.
func (r *Registry) providerConfig(a Account, creds provider.Credentials) provider.Config {
	imapAddr := a.IMAPAddr()
	if to, ok := r.redirect[imapAddr]; ok {
		imapAddr = to
	}
	return provider.Config{
		Kind:              a.Provider,
		IMAPAddr:          imapAddr,
		SMTPHost:          a.SMTPHost,
		SMTPPort:          a.SMTPPort,
		SMTPImplicitTLS:   a.SMTPTLS == "implicit",
		SMTPHelo:          r.heloName(),
		Credentials:       creds,
		SpoolDir:          r.spoolDir,
		DebugWriter:       r.debug,
		AllowInsecureAuth: r.insecure,
		DialControl:       r.dialControl(),
	}
}

// heloName is the host of the deployment's public URL, the name SMTP
// submissions introduce themselves with; empty without one.
func (r *Registry) heloName() string {
	u, err := url.Parse(r.publicURL)
	if err != nil || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil {
		return ""
	}
	return u.Hostname()
}

// dialControl is the address check every IMAP and SMTP connection runs, or
// nil when the deployment allows private hosts.
func (r *Registry) dialControl() func(network, address string, c syscall.RawConn) error {
	if r.allowPrivate {
		return nil
	}
	return netguard.Control
}

func (r *Registry) credentials(ctx context.Context, a Account) (provider.Credentials, error) {
	if !a.UsesOAuth() {
		password, err := r.repo.Password(ctx, a.ID)
		if err != nil {
			return provider.Credentials{}, err
		}
		return provider.Credentials{User: a.LoginUser, Password: password}, nil
	}

	token, err := r.repo.Token(ctx, a.ID)
	if err != nil {
		return provider.Credentials{}, err
	}
	// The client that issued the grant, never simply the default one: a
	// refresh token only works with the registration it came from, and the
	// console's web client and the CLI's installed client are two.
	config, err := r.clientConfig(a, a.OAuthClient, "")
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			// Its client was taken out of the environment. The account can
			// do nothing until it is put back or the account re-authorised,
			// and it says so rather than failing each use in its own words.
			//nolint:errcheck // the error below is what the caller acts on
			_, _ = r.repo.Transition(context.WithoutCancel(ctx), a.ID, []State{StateActive}, StateError, ReasonClientMissing)
		}
		return provider.Credentials{}, err
	}
	if a.State == StateError && a.StateReason == ReasonClientMissing {
		// And put back: nothing else would ever clear this reason.
		//nolint:errcheck // a stale reason is cosmetic; the credentials are good
		_, _ = r.repo.Transition(ctx, a.ID, []State{StateError}, StateActive, "")
	}

	r.mu.Lock()
	source, ok := r.sources[a.ID]
	if !ok {
		// Built on the daemon's context, not the caller's: oauth2 reuses that
		// context for every future refresh, so a request-scoped one would
		// make refreshes fail the moment that request ended.
		//nolint:contextcheck // the daemon's lifetime is the correct scope
		source = newTokenSource(r.baseCtx, a.ID, config, token, r.repo)
		r.sources[a.ID] = source
	}
	r.mu.Unlock()

	return provider.Credentials{User: a.LoginUser, Tokens: source}, nil
}

// Remove deletes an account and everything cached for it, including a
// consent attempt still waiting: a listener left open after its account is
// gone would still take a redirect and say something about it.
func (r *Registry) Remove(ctx context.Context, accountID string) error {
	if err := r.repo.Delete(ctx, accountID); err != nil {
		return err
	}
	r.stopWaiting(accountID)
	r.forget(accountID)
	return nil
}

// RemoveOwner deletes a person's mailboxes — every account they own, with
// everything Remove deletes for one — and drops every consent attempt they
// started anywhere, in one transaction that also then completes: the caller
// deletes the person there, so either all of it happens or none does. Once it
// has committed, the registry lets go of those accounts as Remove does: their
// token sources and cached mailboxes, and any listener or device poll still
// waiting on their behalf. It reports how many accounts it removed.
func (r *Registry) RemoveOwner(ctx context.Context, userID string, also func(*sql.Tx) error) (int, error) {
	removed, attempts, err := r.repo.deleteOwner(ctx, userID, also)
	if err != nil {
		return 0, err
	}
	for _, a := range attempts {
		r.stopAttempt(a.accountID, a.state)
	}
	for _, id := range removed {
		r.stopWaiting(id)
		r.forget(id)
	}
	return len(removed), nil
}

// Scrub empties the database's write-ahead log, so that what a removal just
// deleted is gone from the files on disk and not only from the tables. Callers
// run it once the deletion has committed; see store.Scrub.
func (r *Registry) Scrub(ctx context.Context) error { return r.repo.store.Scrub(ctx) }

// StopFlowsBy ends every consent attempt a person started: its row, so its
// redirect is refused, and the listener or poll the daemon runs for it. Used
// when the person is disabled, who must not finish connecting a mailbox
// afterwards.
func (r *Registry) StopFlowsBy(ctx context.Context, userID string) error {
	attempts, err := r.repo.deleteFlowsBy(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		r.stopAttempt(a.accountID, a.state)
	}
	return nil
}

func (r *Registry) forget(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mb, ok := r.mailboxes[accountID]; ok {
		//nolint:errcheck // dropping a cached factory, nothing to report
		_ = mb.Close()
		delete(r.mailboxes, accountID)
	}
	// Every caller is done with this grant: replaced, refused or removed.
	// A connection still opening with it must not write back over the next.
	if source, ok := r.sources[accountID]; ok {
		source.retire()
	}
	delete(r.sources, accountID)
}

// Close releases every cached mailbox and stops every consent attempt,
// waiting for their listeners to close.
func (r *Registry) Close() error {
	r.mu.Lock()
	for id, mb := range r.mailboxes {
		//nolint:errcheck // shutting down
		_ = mb.Close()
		delete(r.mailboxes, id)
	}
	waiting := make([]*waiter, 0, len(r.waiting))
	for id, w := range r.waiting {
		waiting = append(waiting, w)
		delete(r.waiting, id)
	}
	r.mu.Unlock()

	for _, w := range waiting {
		w.cancel()
	}
	for _, w := range waiting {
		select {
		case <-w.done:
		case <-time.After(stopWait):
		}
	}
	return nil
}

// closeLoopback unwinds a listener whose flow could not be started.
//
// Close makes its own bounded shutdown context on purpose: the caller's may
// already be cancelled, and the listener still has to stop.
//
//nolint:contextcheck // see above
func (r *Registry) closeLoopback(l *loopbackServer) {
	if l != nil {
		//nolint:errcheck // unwinding a failed start
		_ = l.Close()
	}
}

// clientConfig builds the OAuth configuration for one of an account's
// provider's clients: ClientInstalled or ClientWeb.
func (r *Registry) clientConfig(a Account, client, redirect string) (*oauth2.Config, error) {
	var registration OAuthClient
	switch a.Provider {
	case provider.KindGmail:
		registration = r.google
		if client == ClientWeb {
			registration = r.googleWeb
		}
	case provider.KindMicrosoft:
		registration = r.microsoft
		if client == ClientWeb {
			registration = r.microsoftWeb
		}
		if a.Tenant != "" {
			registration.Tenant = a.Tenant
		}
	default:
		return nil, fmt.Errorf("%w: %s accounts do not use OAuth", ErrNotOAuth, a.Provider)
	}
	if client != ClientWeb {
		client = ClientInstalled
	}
	return configFor(a.Provider, registration, client, redirect)
}

// applyServerDefaults fills in the hosts a provider uses, so a person adding a
// Gmail account does not have to know them.
func applyServerDefaults(a *Account, req AddRequest) {
	switch {
	case req.ICloud:
		// Apple's servers, and only those: they are what makes the account
		// iCloud, and Add has already refused a request that named others.
		a.IMAPHost, a.IMAPPort = icloudIMAPHost, icloudIMAPPort
		a.SMTPHost, a.SMTPPort, a.SMTPTLS = icloudSMTPHost, icloudSMTPPort, icloudSMTPTLS
		return
	case a.Provider == provider.KindGmail:
		a.IMAPHost, a.IMAPPort = "imap.gmail.com", 993
		a.SMTPHost, a.SMTPPort, a.SMTPTLS = "smtp.gmail.com", 465, "implicit"
	case a.Provider == provider.KindMicrosoft:
		a.IMAPHost, a.IMAPPort = "outlook.office365.com", 993
		a.SMTPHost, a.SMTPPort, a.SMTPTLS = "smtp.office365.com", 587, "starttls"
	}
	// Explicit values win, so an unusual deployment is still reachable.
	if req.IMAPHost != "" {
		a.IMAPHost = req.IMAPHost
	}
	if req.IMAPPort != 0 {
		a.IMAPPort = req.IMAPPort
	}
	if req.SMTPHost != "" {
		a.SMTPHost = req.SMTPHost
	}
	if req.SMTPPort != 0 {
		a.SMTPPort = req.SMTPPort
	}
	if req.SMTPTLS != "" {
		a.SMTPTLS = req.SMTPTLS
	}
	if a.SMTPTLS == "" {
		a.SMTPTLS = "starttls"
	}
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	if a.SMTPPort == 0 {
		a.SMTPPort = 587
	}
}

// GuessProvider picks a provider from the address, so the common cases need no
// flag.
func GuessProvider(email string) provider.Kind {
	_, domain, _ := strings.Cut(strings.ToLower(email), "@")
	switch domain {
	case "gmail.com", "googlemail.com":
		return provider.KindGmail
	case "outlook.com", "hotmail.com", "live.com", "msn.com":
		return provider.KindMicrosoft
	default:
		return provider.KindIMAP
	}
}

func newAccountID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail in practice, and an account id that is
		// not unique would attach one mailbox's mail to another.
		panic("account: cannot read random bytes: " + err.Error())
	}
	return "acc_" + hex.EncodeToString(raw)
}

func randomState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("account: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
