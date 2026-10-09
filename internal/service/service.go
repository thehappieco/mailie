package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Service is the use-case layer.
//
// A concrete struct, not an interface. There is one implementation and there
// will be one implementation; an interface here would buy nothing except a
// second place to keep the method set in step, and would push the transports
// towards testing against a fake service — which is exactly how REST and MCP
// drift apart. They are tested against this, wired to a temporary database and
// a fake mailbox.
type Service struct {
	accounts   *account.Registry
	keys       *auth.Keys
	users      *auth.Users
	workspaces *workspace.Repository
	store      *store.Store
	bus        *events.Bus
	sync       SyncController
	log        *slog.Logger
	now        func() time.Time

	// accessEpoch counts the commits that may have given or taken read
	// access to a mailbox from somebody: a grant set or revoked, a
	// membership disabled or removed, a mailbox linked or removed, a person
	// disabled or deleted. One daemon writes the database, so a counter in
	// memory sees every one; an event subscription reads again what its
	// caller may read whenever it moved (access.go).
	accessEpoch atomic.Int64

	// publicURL is the console's origin: the base of every invite link.
	publicURL string
	// localConsole is whether a browser using the console runs on this
	// machine, which is the only place a loopback OAuth listener can be
	// reached from.
	localConsole bool
	// downloads bounds what originals and attachments hold in the spool.
	downloads *downloadBudget

	// spoolDir is where a send's attachments are held while it runs.
	spoolDir string
	// sendSpool bounds what sends in flight hold there.
	sendSpool *downloadBudget
	// sendRetry is how long to wait before each new attempt at a send the
	// server said to try later.
	sendRetry []time.Duration
	// pacer keeps each account under its provider's submission rate.
	pacer *sendPacer
	// sendHashKey keys the hash that identifies a message to its send
	// record (composeHash).
	sendHashKey []byte
	// consent names the revisions of the texts a person agrees to, every
	// one set.
	consent config.ConsentVersions
	// mcpHTTP is whether this server answers MCP over HTTP at /mcp.
	mcpHTTP bool
	// keysMayNotSend refuses the send scope to a new workspace key and every
	// send by one (MAIL_KEYS_MAY_SEND=false).
	keysMayNotSend bool
	// keysActUnderCreator holds every workspace key's actions to its
	// creator's actions consent (MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT).
	keysActUnderCreator bool
	// externalSignInOnly is whether people sign in only through an
	// extension (external.go): every password and invitation route is
	// refused.
	externalSignInOnly bool
}

// Deps is what the service needs.
type Deps struct {
	Accounts *account.Registry
	Keys     *auth.Keys
	Users    *auth.Users
	// Workspaces is the repository of workspaces, their members and the
	// grants on their mailboxes, with the workspace source the daemon runs
	// with. nil is one over Store with the local source.
	Workspaces *workspace.Repository
	// Store is the database, for what belongs to no repository of its own:
	// consent to sync and the index it governs.
	Store *store.Store
	Bus   *events.Bus
	// Sync is the sync engine. nil runs without one: every account's sync
	// is off, and asking for a pass says sync is unavailable.
	Sync SyncController
	Log  *slog.Logger
	Now  func() time.Time
	// PublicURL is MAIL_PUBLIC_URL, already validated. Empty means the
	// console, if there is one, is only ever reached on this machine.
	PublicURL string
	// DownloadSpoolBytes bounds what downloads in flight may hold in the
	// spool at once (MAIL_DOWNLOAD_SPOOL_MAX_BYTES); 0 is
	// DefaultDownloadSpoolBytes. DownloadsPerCaller is how many one caller
	// may have in flight; 0 is DefaultDownloadsPerCaller.
	DownloadSpoolBytes int64
	DownloadsPerCaller int
	// SpoolDir is where a send's attachments are held while it runs
	// (<data>/tmp, beside the provider's spooled sections); the daemon's
	// start removes any a crash left. Empty is the system's temporary
	// directory.
	SpoolDir string
	// SendSpoolBytes bounds what sends in flight may hold there at once; 0
	// is DefaultSendSpoolBytes.
	SendSpoolBytes int64
	// SendHashKey keys the hash a send record keeps of what was composed,
	// so that neither the record nor a log line holding a key made from it
	// can confirm a guess of the message without it. The daemon gives it
	// the send-hash root, made once and kept in the database sealed like a
	// credential (store.SendHashRoot), so it opens only with a key that
	// never sits beside the database. Empty is a random key for this
	// process: a record is then recognised only until it restarts.
	SendHashKey []byte
	// ConsentVersions are the revisions of the texts a person agrees to
	// (MAIL_CONSENT_VERSION_*); an empty one is its default.
	ConsentVersions config.ConsentVersions
	// MCPHTTP is whether the daemon serves MCP over Streamable HTTP at /mcp
	// (MAIL_MCP_HTTP), which the service only reports (MCPAccess): the
	// transport decides what it mounts.
	MCPHTTP bool
	// KeysMayNotSend is MAIL_KEYS_MAY_SEND=false: an edition whose key terms
	// do not cover sending. A workspace key is then never created with the
	// send scope, and every send by one is refused. False, the default, lets
	// a key send where it holds the send flag, with the send scope.
	KeysMayNotSend bool
	// KeysActUnderCreatorConsent is MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT=true:
	// a workspace key acts only while its creator allows actions.
	KeysActUnderCreatorConsent bool
	// ExternalSignInOnly says people sign in only through an extension of
	// the daemon (SignInExternal): the challenge, signing in with a
	// password, signing up or accepting an invitation, changing or
	// recovering a password, the password's step-up, the upgrade, a reset
	// invitation and creating an invitation are refused, not_authorized.
	// False, the default, changes nothing. Only internal/app sets it, from
	// its Options.
	ExternalSignInOnly bool
}

// New builds the service.
func New(d Deps) *Service {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	local := d.PublicURL == ""
	if u, err := url.Parse(d.PublicURL); err == nil && d.PublicURL != "" {
		local = config.IsLoopbackHost(u.Hostname())
	}
	if d.Accounts != nil && d.Users != nil {
		// Who may still have a grant or an account stored for them is a
		// rule about people, which the registry cannot know by itself. It
		// runs in the transaction that stores, so a consent finishing while
		// its person is being disabled stores nothing.
		d.Accounts.CheckOwnersWith(d.Users.RequireActiveTx)
	}
	if d.Accounts != nil {
		// And who may finish a consent attempt: whoever started it must
		// still manage the mailbox when its grant is stored, which nothing
		// else re-checks for an attempt the daemon completes by itself.
		d.Accounts.CheckFlowsWith(func(ctx context.Context, tx *sql.Tx, accountID, owner string) error {
			return workspace.ManagesTx(ctx, tx, accountID, owner)
		})
	}
	hashKey := d.SendHashKey
	if len(hashKey) == 0 {
		hashKey = make([]byte, 32)
		//nolint:errcheck // crypto/rand.Read never returns an error
		_, _ = rand.Read(hashKey)
	}
	workspaces := d.Workspaces
	if workspaces == nil && d.Store != nil {
		workspaces = workspace.NewRepository(d.Store, nil)
	}
	return &Service{
		accounts: d.Accounts, keys: d.Keys, users: d.Users, workspaces: workspaces,
		store: d.Store, bus: d.Bus, sync: d.Sync, log: log, now: now,
		publicURL: d.PublicURL, localConsole: local,
		downloads: newDownloadBudget(d.DownloadSpoolBytes, d.DownloadsPerCaller),
		spoolDir:  d.SpoolDir, sendSpool: newSendBudget(d.SendSpoolBytes),
		sendRetry: defaultSendRetry, pacer: newSendPacer(), sendHashKey: hashKey,
		consent: d.ConsentVersions.OrDefaults(), mcpHTTP: d.MCPHTTP, externalSignInOnly: d.ExternalSignInOnly,
		keysMayNotSend:      d.KeysMayNotSend,
		keysActUnderCreator: d.KeysActUnderCreatorConsent,
	}
}

// Principal is the authenticated caller.
type Principal = auth.Principal

// Authenticate identifies the caller behind a bearer token.
//
// Here rather than in a transport so REST and MCP accept exactly the same
// credentials. The shape of the token picks the check — a key has a dot, a
// session token never does — so a session costs a SHA-256, never the Argon2id a
// key's secret needs.
//
// unknownKey is asked before a key whose prefix matches nothing is hashed:
// the transport's per-address limit on checks that can only fail. nil admits
// every one.
func (s *Service) Authenticate(ctx context.Context, token string, unknownKey auth.Gate) (Principal, error) {
	var (
		p   Principal
		err error
	)
	switch {
	case auth.IsAPIKey(token):
		p, err = s.keys.Authenticate(ctx, token, unknownKey)
	case s.users != nil:
		p, err = s.users.AuthenticateSession(ctx, token)
	default:
		err = auth.ErrInvalidSession
	}
	if err != nil {
		return Principal{}, fromCredential(err)
	}
	if p.IsWorkspaceKey() && p.TermsVersion == "" {
		// A workspace key reaches mail only if the person who created it
		// agreed to the key terms, which say what a tool holding it can do.
		// Migration 0012 revoked every person's key nobody agreed to them
		// through; this refuses one on every transport, not only on the
		// tools', should one ever be live.
		return Principal{}, errKeyNotAgreed
	}
	return p, nil
}

// AuthenticateTool identifies the caller behind a bearer token presented by a
// tool: the MCP server's credential check.
//
// Stricter than Authenticate, because a tool is somebody's AI assistant or
// script, and the privacy promise is that a tool reaches a mailbox only
// through a key of its workspace, holding what a reader of it gave:
//
//   - A console session is not a tool's credential. It stands for a person
//     at a keyboard, and copying one out of a browser into an assistant's
//     configuration must not work.
//   - A workspace key reaches the mailboxes it holds something on, as over
//     REST, and works only if the person who created it agreed to the key
//     terms — here as everywhere (Authenticate).
//   - An instance key sees only the operator workspace's mailboxes, here as
//     over REST, never a person's (see visibility).
func (s *Service) AuthenticateTool(ctx context.Context, token string, unknownKey auth.Gate) (Principal, error) {
	if !auth.IsAPIKey(token) {
		return Principal{}, errToolNeedsKey
	}
	p, err := s.Authenticate(ctx, token, unknownKey)
	if err != nil {
		return Principal{}, err
	}
	p.Tool = true
	return p, nil
}

// Errors of Authenticate and AuthenticateTool.
var (
	errToolNeedsKey = E(CodeUnauthorized,
		"the MCP server takes an API key, which an owner or an admin of a workspace creates in the console, "+
			"not a console session", nil)
	errKeyNotAgreed = E(CodeNotAuthorized,
		"this key was not created by a person who agreed to the key terms; an owner or an admin of the workspace "+
			"creates one in the console for a tool to use", nil)
)

// Recheck re-reads a caller that already authenticated, immediately before a
// response is written: a key revoked or a session ended while the request ran
// must not see its answer.
func (s *Service) Recheck(ctx context.Context, p Principal) error {
	var err error
	if p.IsSession() {
		err = s.users.RecheckSession(ctx, p)
	} else {
		err = s.keys.Recheck(ctx, p)
	}
	if err != nil {
		return fromCredential(err)
	}
	return nil
}

// fromCredential renders an authentication failure. One message per kind of
// credential and never which check failed: saying "revoked" rather than
// "unknown" would confirm to a thief that the thing they hold was once real.
func fromCredential(err error) error {
	var throttled *auth.ThrottledError
	switch {
	case errors.As(err, &throttled):
		// The same words as every other limit on this path: which limit bit
		// is not the caller's business.
		return Retryable("too many requests", throttled.RetryAfter, err)
	case errors.Is(err, auth.ErrHashBusy), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The check never ran, so this says nothing about the credential.
		return Retryable("the server is busy checking other credentials; try again shortly", hashWaitRetry, err)
	case errors.Is(err, auth.ErrInvalidKey):
		return E(CodeUnauthorized, "invalid or expired api key", err)
	case errors.Is(err, auth.ErrKeyNarrowed):
		// The key works; what was opened with it no longer matches it. Not
		// unauthorized, which tells a client its key is dead: it connects
		// again, and goes on with what the key still names.
		return E(CodeConflict, "this key no longer reaches a mailbox it was authenticated with; "+
			"authenticate again: reconnect the stream, or restart the session", err)
	case errors.Is(err, auth.ErrInvalidSession):
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	default:
		return E(CodeInternal, "checking the credential failed", err)
	}
}

// authorize checks the scope and returns the error a transport should render.
//
// Authorisation lives here rather than in the transports so that REST and MCP
// cannot disagree about who may do what — which they would, eventually, if
// each carried its own copy of the rule.
func (s *Service) authorize(p Principal, need auth.Scope) error {
	if !p.Scope.Covers(need) {
		return Ef(CodeNotAuthorized, nil, "this key has %s scope; %s is required", p.Scope, need)
	}
	return nil
}

// authorizeAccount checks the scope and that the caller may touch this
// account with the flags need names, and returns the account.
//
// Not found rather than forbidden, so that nobody can learn which account ids
// exist by probing: a key restricted to other accounts, a mailbox of a
// workspace the caller is not an active member of, or one they hold no grant
// on. A mailbox the caller does see, without a flag the operation needs, is
// not_authorized: they already know it exists. See visibility.
func (s *Service) authorizeAccount(ctx context.Context, p Principal, scope auth.Scope, accountID string, need workspace.Flags) (account.Account, error) {
	if err := s.authorize(p, scope); err != nil {
		return account.Account{}, err
	}
	if !p.MayAccess(accountID) {
		return account.Account{}, E(CodeNotFound, "no such account", nil)
	}
	a, err := s.accounts.Repo().GetVisible(ctx, accountID, visibility(p))
	switch {
	case errors.Is(err, account.ErrNotFound):
		return account.Account{}, E(CodeNotFound, "no such account", err)
	case err != nil:
		return account.Account{}, E(CodeInternal, "reading the account failed", err)
	}
	if err := s.requireFlags(ctx, p, a, need); err != nil {
		return account.Account{}, err
	}
	return a, nil
}

// visibility is the rule for which mailboxes a caller may know exist, as the
// repository applies it in SQL, the same for listing and for fetching one.
//
// An instance key — the operator's, over REST and MCP alike — sees the
// operator workspace's mailboxes and nothing else. A person signed in sees a
// mailbox when they are an active member of its workspace and hold a grant
// on it or manage it by their role, owner or admin. A workspace key sees the
// mailboxes it holds something on, read live, whoever holds the principal and
// for however long. A role reaches no mailbox's index: an owner of a team or
// of the instance reads nothing by being one.
func visibility(p Principal) account.Visibility {
	switch {
	case p.IsInstance():
		return account.Visibility{Unowned: true}
	case p.IsSession():
		return account.Visibility{UserID: p.UserID}
	}
	return account.Visibility{Key: p.KeyPrefix}
}

// readable is visibility narrowed to the mailboxes whose index the caller may
// open: a person's or a key's read flag. An instance key reads every mailbox
// it sees.
func readable(p Principal) account.Visibility {
	v := visibility(p)
	v.Need = workspace.Flags{Read: true}
	return v
}

// requireSession guards what only a signed-in person may do: their own
// profile, password and sessions, and every key: a key never mints, lists or
// changes a key, so a leaked one cannot keep access after it is revoked.
func requireSession(p Principal) error {
	if !p.IsSession() {
		return E(CodeNotAuthorized, "this needs a signed-in user; an API key cannot use it", nil)
	}
	return nil
}

// fromProvider maps a provider failure onto the transport vocabulary. The
// provider's error always goes along as the cause: the caller's message is
// this layer's, and what the server actually said belongs in the log.
func fromProvider(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provider.ErrNeedsReauth):
		return needsReauth(err)
	case errors.Is(err, provider.ErrNotConnected):
		return E(CodeConflict,
			"the server authenticated the account but refused to open the mailbox; "+
				"check that IMAP is enabled for it", err)
	case errors.Is(err, provider.ErrAuthFailed):
		return E(CodeConflict, "the mail server rejected the stored credentials", err)
	case errors.Is(err, provider.ErrTooManyConnections):
		return Retryable("the account has too many open connections to its mail server", time.Minute, err)
	case errors.Is(err, provider.ErrRateLimited):
		return Retryable("the mail server is throttling this account", time.Minute, err)
	case errors.Is(err, provider.ErrFolderNotFound):
		return E(CodeNotFound, "no such folder", err)
	case errors.Is(err, provider.ErrMessageGone):
		return E(CodeNotFound, "that message no longer exists on the server", err)
	case errors.Is(err, provider.ErrTooLarge):
		return E(CodeBadRequest, "the message is larger than the provider accepts", err)
	case errors.Is(err, provider.ErrUnsupported):
		return E(CodeBadRequest, "the mail server does not support that operation", err)
	default:
		// Deliberately "upstream": the fault is not ours, and a caller reading
		// the message should be able to tell.
		return E(CodeInternal, "upstream: the mail server could not be reached", err)
	}
}

// ctxWithDeadline bounds a provider call so a slow server cannot hold a
// request open past its own timeout.
func ctxWithDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < d {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
