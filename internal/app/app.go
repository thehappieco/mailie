// Package app assembles the daemon and runs it until its context ends: the
// data directory's lock, the store, the sealer and the send-hash root, the
// account registry and the sync engine, the service, REST, a console when
// one is given, metrics, the listeners, the startup hints, the sweeps that
// enforce retention, and a shutdown that gives the listeners fifteen seconds
// and waits for the rest. It serves no MCP: internal/mcp is a library no
// part of this assembly mounts, and /mcp answers the API's 404.
//
// `mailserver serve` is a thin caller of Run. Another binary that must run
// exactly the same server calls Run too, and adds what is its own through
// Options: routes beside the core's (Extension), which can never take a
// request a core route answers, with the background work an extension asks
// for (Deps.Go), awaited before the database closes; headers of its own on
// the console's page documents, and small files beside them (Pages); for a
// binary whose people sign in through such a route
// (service.SignInExternal), the switch that turns the core's passwords and
// invitations off (ExternalSignInOnly) and the one that has their names
// follow the provider's (NameFollowsIdentity); for an edition that does not
// offer team workspaces yet, the one that withholds them (TeamsWithheld);
// for one whose API keys belong to personal workspaces alone, the one that
// refuses a team's (KeysInPersonalWorkspaceOnly); for one whose sync text
// says what pausing one mailbox's sync does, the one that offers it
// (SyncPauseOffered, which `serve` sets); for a binary whose workspaces come from a directory elsewhere, the
// question asked before an act that widens access (Freshness); and, for one
// whose API keys an issuer makes elsewhere, that issuer (KeyIssuer, only
// beside ExternalSignInOnly), the end of this server's own workspace keys
// (KeysIssuedElsewhere) and the refusal of every issued key
// (IssuedKeysRefused). Nothing else
// of the assembly is open to a caller, because a server that differed from
// `mailserver serve` in what it checks, sweeps or refuses would be a second
// implementation of it.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
	"github.com/thehappieco/mailie/internal/webui"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Options is what the binary that runs the daemon decides beyond the
// configuration.
type Options struct {
	// Version is the build's version: reported by the health probe, and
	// logged at start.
	Version string
	// Extensions add routes to the daemon's listener, in this order, once
	// the service is built and before anything listens. See Extension.
	Extensions []Extension
	// WorkspaceSource is where workspaces and their memberships come from;
	// nil is workspace.Local(), the self-hosted edition's. Only a binary
	// that embeds the daemon sets it: there is no environment variable, and
	// `serve` never does.
	WorkspaceSource workspace.Source
	// ExternalSignInOnly says people sign in only through an extension,
	// which calls service.SignInExternal: the core refuses, not_authorized,
	// every route that signs in with a password, signs up or accepts an
	// invitation, changes a password, or creates an invitation, to the
	// instance or into a team, and the sessions the extension's sign-in
	// opens are sessions whatever the configuration says of passwords. The
	// service decides it, for every transport. Only a binary that embeds the
	// daemon sets it, and no environment variable does. Without it, whether
	// anybody signs in at all is the configuration's
	// (config.Config.PasswordSignIn, MAIL_PASSWORD_SIGN_IN): off by default,
	// so that `serve` runs a server with no people, whose operator acts with
	// keys made with its command line, and on, people with a password here.
	ExternalSignInOnly bool
	// NameFollowsIdentity says a person's name follows the identity they
	// sign in with through an extension: every sign-in
	// (service.SignInExternal) names them as the provider names them then,
	// an empty name leaving them none, and they never rename themselves here
	// (PUT /v1/auth/profile is a conflict for anyone linked to an identity).
	// A step-up never touches a name, nor does anything else. Someone who
	// signs in with a password names themselves as ever. The service decides
	// it, for every transport. Only a binary that embeds the daemon sets it:
	// there is no environment variable, and `serve` never does.
	NameFollowsIdentity bool
	// TeamsWithheld says this edition does not offer team workspaces yet:
	// the core refuses, not_authorized, every use of a team, by a person or
	// a key — linking a mailbox into one, its mailboxes, its keys, who holds
	// what on them, its members, invites and name, signing up or accepting
	// an invite into one, its consent to sync, writing or supplying a
	// mailbox key there — lists no team's mailbox, syncs none
	// (store.Options.TeamsWithheld), and lists every team marked withheld.
	// Nothing of a team is deleted or changed: whatever the database holds,
	// a restored one included, its index too, is kept and unused. Personal workspaces and the operator's are as they were. The
	// service decides it, for every transport, and the store for the sync
	// engine. Only a binary that embeds the daemon sets it: there is no
	// environment variable, and `serve` never does.
	TeamsWithheld bool
	// KeysInPersonalWorkspaceOnly says this edition's API keys belong to a
	// person's personal workspace alone, whatever teams it offers, as its key
	// terms say: the core refuses, not_authorized, creating a key in a team
	// and raising what a key holds on a team's mailbox, and lets no key reach
	// a team's mailbox, one a database still holds included, while listing a
	// team's keys, revoking them and taking what they hold away stay. The
	// service decides it, for every transport. Only a binary that embeds the
	// daemon sets it: there is no environment variable, and `serve` never
	// does.
	KeysInPersonalWorkspaceOnly bool
	// SyncPauseOffered offers a person's pause of one mailbox's sync (POST
	// /v1/accounts/{id}/sync/pause). `serve` sets it, for the open edition,
	// whose sync text a pause breaks nothing of; a binary that embeds the
	// daemon sets it only once the sync text its people agree to says what a
	// pause does, and left unset the core refuses every person's pause
	// (not_authorized), while a recovery still pauses and a resume still ends
	// a pause: an edition offers the pause by opting in, never by forgetting
	// to withhold it. The service decides it, for every transport; there is
	// no environment variable.
	SyncPauseOffered bool
	// Freshness, when set, is asked before an act of a person signed in that
	// widens who may read a mailbox or touches a team's shared data
	// (service.Freshness): an extension that follows the directory the
	// workspaces come from reads the actor's standing there again first. It
	// gates, never authorizes. Only a binary that embeds the daemon sets it:
	// there is no environment variable, and `serve` never does.
	Freshness service.Freshness
	// Pages is what the console's page documents carry besides the core's
	// own headers, document by document, and the small files served beside
	// them (webui.Pages): for pages of an edition's own that another site
	// may load ahead of a click, say, whose answer then depends on who asked
	// and is never stored. Nothing in it weakens what protects a page. A
	// file the console would not serve stops the daemon from starting
	// (webui.ErrPages); without a console (MAIL_WEB_DIR) it serves nothing.
	// Only a binary that embeds the daemon sets it: there is no environment
	// variable, and `serve` never does.
	Pages webui.Pages
	// KeyIssuer, when set, is an extension that makes API keys elsewhere,
	// for the people of a directory it follows, and has this server mirror
	// and enforce them as workspace keys (service.KeyIssuer): a credential
	// it claims is checked only against the keys it made. Refused at start
	// unless ExternalSignInOnly is set too (ErrKeyIssuer): no local password,
	// reset or recovery may exist beside keys a person's account elsewhere
	// answers for. Only a binary that embeds the daemon sets it: there is no
	// environment variable, and `serve` never does.
	KeyIssuer service.KeyIssuer
	// KeysIssuedElsewhere says this edition's API keys are made by their
	// issuer: the core refuses, not_authorized, creating a key of its own and
	// raising what one holds, before anything else is asked; refuses one of
	// its own outside the operator workspace; and revokes every live one at
	// start, before anything listens, logging how many (the operator's keys,
	// which the command line holds, are kept). Listing, revoking and taking
	// access away stay. The service decides it, for every transport. Only a
	// binary that embeds the daemon sets it: there is no environment
	// variable, and `serve` never does.
	KeysIssuedElsewhere bool
	// IssuedKeysRefused says this edition refuses every key its issuer made,
	// at authentication and before every answer, while their records still
	// apply: the way back from taking them, with nothing to undo once it is
	// lifted. Only a binary that embeds the daemon sets it: there is no
	// environment variable, and `serve` never does.
	IssuedKeysRefused bool
}

// ErrKeyIssuer is a KeyIssuer given without ExternalSignInOnly: keys an
// issuer makes stand on a person's account elsewhere, and a password, a
// reset or a recovery here would be a second account behind them.
var ErrKeyIssuer = errors.New("app: a key issuer needs people to sign in only through an extension (ExternalSignInOnly)")

// Run runs the daemon until ctx ends or a listener fails, and then shuts it
// down: its listeners within fifteen seconds, then, each waited for, the
// background work of its extensions, the sync engine, and the database.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	// What the binary asks of the console's pages is checked before
	// anything starts: a page it cannot have is its mistake, never the
	// console's absence.
	if err := opts.Pages.Check(); err != nil {
		return err
	}
	if opts.KeyIssuer != nil && !opts.ExternalSignInOnly {
		return ErrKeyIssuer
	}

	lock, err := lockfile.Acquire(cfg.LockPath())
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			if pid, ok := lockfile.Holder(cfg.LockPath()); ok {
				return fmt.Errorf("another mailserver is already running on %s (pid %d)", cfg.DataDir, pid)
			}
			return fmt.Errorf("another mailserver is already running on %s", cfg.DataDir)
		}
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			logger.Error("releasing the data directory lock failed", "err", err)
		}
	}()

	// A message section is spooled here only while a download is being
	// answered, and a send's attachments only while it runs; each is removed
	// when that ends. One a crash left behind would be a message's content
	// kept on disk, which nothing may keep. The lock says no other daemon is
	// using the directory.
	if n, err := sweepSpool(cfg.SpoolDir()); err != nil {
		logger.Warn("clearing the spool directory failed", "err", err)
	} else if n > 0 {
		logger.Info("removed message sections a previous run left in the spool directory", "files", n)
	}

	// Built before the database is opened: a KMS key the daemon cannot use
	// (DescribeKey refused, the instance role not reached) stops it here,
	// having changed nothing.
	sealer, err := NewSealer(ctx, cfg)
	if err != nil {
		return err
	}

	// An edition that withholds teams indexes none of their mailboxes: the
	// store's rule for what may sync says so, for the engine and for every
	// batch it writes.
	db, err := store.Open(ctx, cfg.DatabasePath(), store.Options{TeamsWithheld: opts.TeamsWithheld})
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("closing the database failed", "err", err)
		}
	}()

	// A send still sending was cut off by the stop of the daemon that ran
	// it, and nobody can say whether the server took the message: unknown,
	// before anything can send again, so its key is never used twice.
	if n, err := db.InterruptSends(ctx); err != nil {
		return fmt.Errorf("recording interrupted sends: %w", err)
	} else if n > 0 {
		logger.Warn("sends were interrupted by the last stop; their outcome is unknown", "sends", n)
	}

	// What a send record keeps of a message is a hash under this root, so
	// the database, a backup or the log alone cannot confirm a guess of it.
	// It is opened before anything runs.
	sendHashKey, created, err := openSendHashRoot(ctx, db, sealer)
	if err != nil {
		return err
	}
	if created {
		// The first start of this database, or the first of a version that
		// keeps the root on a database whose send hashes were derived from
		// the credential key: a send made before it and repeated now is not
		// recognised.
		logger.Info("created the send-hash root", "sealed_with", sealer.Describe())
	}

	// The salt every address is answered is the address's under this key
	// (docs/key-scheme.md section 5.3), so it is opened before anyone can
	// ask, where people sign in with a password here. Nobody asks on a
	// server with no people, nor where people sign in only through an
	// extension: no salt key is made there, and one a database holds from
	// before is left as it is, unopened.
	users := auth.NewUsers(db).WithWorkspaceSource(opts.WorkspaceSource)
	passwords := cfg.PasswordSignIn && !opts.ExternalSignInOnly
	if passwords {
		saltKey, made, err := openKDFSaltKey(ctx, db, sealer)
		if err != nil {
			return err
		}
		if made {
			logger.Info("created the salt key", "sealed_with", sealer.Describe())
		}
		users = users.WithSaltKey(saltKey)
		clear(saltKey)
	}

	metrics := obs.NewMetrics()
	keys := auth.NewKeys(db)
	bus := events.NewBus(events.NewJournal(db))
	started := time.Now()

	var imapDebug io.Writer
	if cfg.IMAPDebug {
		imapDebug = debugWriter{logger}
	}
	// Every account state change is journaled as account.state in its own
	// transaction; the bus hears it once that has committed.
	repo := account.NewRepository(db, sealer)
	repo.PublishTo(bus)
	// The registry holds one token source per account for as long as the
	// daemon runs: oauth2 reuses the context it was given for every later
	// refresh, so anything shorter-lived would stop refreshing silently.
	accounts := account.NewRegistry(ctx, repo, account.RegistryOptions{
		Google:    account.OAuthClient{ClientID: cfg.Google.ClientID, ClientSecret: cfg.Google.ClientSecret},
		Microsoft: account.OAuthClient{ClientID: cfg.Microsoft.ClientID, Tenant: cfg.Microsoft.Tenant},
		GoogleWeb: account.OAuthClient{ClientID: cfg.GoogleWeb.ClientID, ClientSecret: cfg.GoogleWeb.ClientSecret},
		MicrosoftWeb: account.OAuthClient{
			ClientID: cfg.MicrosoftWeb.ClientID, ClientSecret: cfg.MicrosoftWeb.ClientSecret, Tenant: cfg.MicrosoftWeb.Tenant,
		},
		PublicURL:    cfg.PublicURL,
		DeviceCode:   cfg.MicrosoftDeviceCode,
		AllowPrivate: cfg.AccountAllowPrivate,
		// A Google mailbox deleted for good has its grant revoked there.
		GoogleRevokeURL: account.GoogleRevokeEndpoint,
		SpoolDir:        cfg.SpoolDir(),
		Debug:           imapDebug,
		Log:             logger,
	})
	//nolint:contextcheck // shutdown paths make their own bounded contexts
	defer func() {
		if err := accounts.Close(); err != nil {
			logger.Error("closing the account registry failed", "err", err)
		}
	}()

	// The sync engine: one worker per account whose owner consented (or, for
	// an account nobody owns, that the operator switched on). The registry
	// tells it about every account created, removed or changing state; the
	// service tells it about consent. It stops before the registry and the
	// database close, logging every connection out.
	engine := syncengine.New(syncengine.Deps{
		Store: db, Accounts: repo, Mailboxes: accounts.Mailbox, Bus: bus, Log: logger,
	}, syncengine.Options{})
	accounts.OnChange(engine.Reconcile)
	// Proving a new grant for an account whose worker is running takes the
	// place of the engine's interactive connection: three, never four.
	accounts.CheckGrantsIn(engine.InPlaceOfInteractive)
	engineCtx, stopEngine := context.WithCancel(ctx)
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		engine.Run(engineCtx)
	}()
	defer func() {
		stopEngine()
		<-engineDone
	}()

	workspaces := workspace.NewRepository(db, opts.WorkspaceSource)
	if made, err := workspaces.RepairPersonal(ctx); err != nil {
		return err
	} else if made > 0 {
		// Only a binary from before migration 0008, run on this database
		// after it was migrated, creates a person without one.
		logger.Warn("people without a personal workspace were given one; a binary older than this database's "+
			"schema ran on it: never roll a binary back alone, restore the backup taken before the upgrade",
			"people", made)
	}
	kept, unread, err := db.TeamSyncNotices(ctx)
	if err != nil {
		return err
	}
	if len(kept) > 0 {
		// Migration 0011 copied no consent from a linker who was disabled:
		// these stay as they were, stopped with their index.
		logger.Warn("team mailboxes are stopped with their index kept, under the consent of a person who is "+
			"disabled; an owner or an admin of each team turns its sync on again, or off, or removes it",
			"accounts", kept)
	}
	if len(unread) > 0 {
		logger.Warn("team mailboxes nobody can read sync nothing, and nobody can be given read on them; "+
			"their owners and admins remove them (and link them again), or turn their sync off to delete "+
			"what is still indexed", "accounts", unread)
	}
	closedTeams, err := db.ClosedTeams(ctx)
	if err != nil {
		return err
	}
	if len(closedTeams) > 0 {
		logger.Info("teams closed: their mailboxes reach nobody and sync nothing until reopened",
			"count", len(closedTeams), "teams", closedTeams)
	}
	if opts.TeamsWithheld {
		logger.Info("team workspaces are withheld in this edition: nothing of a team is used, and nothing of one " +
			"is deleted")
	}
	if opts.KeysInPersonalWorkspaceOnly {
		logger.Info("API keys belong to personal workspaces only in this edition: none is created in a team, " +
			"none is given more on a team's mailbox, and none reaches one")
	}
	if !opts.SyncPauseOffered {
		logger.Info("pausing one mailbox's sync is not offered in this edition: a person's pause is refused; " +
			"a recovery still pauses, and a resume still ends a pause")
	}
	svc := service.New(service.Deps{
		Accounts: accounts, Keys: keys, Users: users, Store: db, Bus: bus, Sync: engine, Log: logger,
		Workspaces: workspaces,
		PublicURL:  cfg.PublicURL, DownloadSpoolBytes: cfg.DownloadSpoolBytes, SpoolDir: cfg.SpoolDir(),
		SendHashKey: sendHashKey, ConsentVersions: cfg.Consent,
		ExternalSignInOnly: opts.ExternalSignInOnly, PasswordSignIn: cfg.PasswordSignIn,
		KeysMayNotSend:             !cfg.KeysMaySend,
		KeysActUnderCreatorConsent: cfg.KeysActUnderCreatorConsent, TeamsWithheld: opts.TeamsWithheld,
		KeysInPersonalWorkspaceOnly: opts.KeysInPersonalWorkspaceOnly,
		Freshness:                   opts.Freshness, NameFollowsIdentity: opts.NameFollowsIdentity,
		SyncPauseOffered: opts.SyncPauseOffered,
		KeyIssuer:        opts.KeyIssuer, KeysIssuedElsewhere: opts.KeysIssuedElsewhere,
		IssuedKeysRefused: opts.IssuedKeysRefused,
	})
	if opts.KeysIssuedElsewhere {
		// Before any extension runs and anything listens: no key of this
		// server's own outside the operator workspace works past this start.
		// Every later start stops none.
		revoked, err := svc.RevokeOwnWorkspaceKeys(ctx)
		if err != nil {
			return err
		}
		logger.Info("workspace keys revoked: this edition takes keys from their issuer",
			"revoked", revoked.Revoked, "unexpired", revoked.Unexpired, "at", revoked.At.Format(time.RFC3339))
	}
	if opts.IssuedKeysRefused {
		logger.Info("keys an issuer made are refused in this edition: their records still apply, and every one " +
			"is refused until a release lifts this")
	}
	if err := personalReplacementHint(ctx, svc, workspaces.Source(), logger); err != nil {
		return err
	}
	// At every start, console or none: mailboxes people linked into their
	// own workspaces or a team's, which no instance key reaches. Where people
	// sign in only through an extension, those workspaces are the edition.
	if !opts.ExternalSignInOnly {
		warnWorkspaceMailboxes(ctx, db, logger)
	}

	// One limiter for bearer traffic, which an extension shares: a key has
	// one budget whichever way it comes in. Sign-in has its own, which an
	// extension that signs people in shares too.
	limits := ratelimit.DefaultAuth(cfg.TrustedProxies)
	signInLimits := ratelimit.DefaultSignIn(cfg.TrustedProxies)
	handler := &api.Handler{
		Service:      svc,
		Limits:       limits,
		SignInLimits: signInLimits,
		Metrics:      metrics,
		Log:          logger,
		AdminAPI:     cfg.AdminAPI,
		Version:      opts.Version,
		Started:      started,
	}

	var metricsRoute http.Handler
	if cfg.MetricsAddr == "" {
		metricsRoute = metrics.Handler()
	}
	var console http.Handler
	if cfg.WebDir != "" {
		web, err := webui.NewWith(cfg.WebDir, webui.Options{ConnectSrc: cfg.ConnectSrc, Pages: opts.Pages})
		if err != nil {
			logger.Warn("no console to serve; serving the API only", "dir", cfg.WebDir, "err", err)
		} else {
			defer func() {
				if err := web.Close(); err != nil {
					logger.Debug("closing the console directory failed", "err", err)
				}
			}()
			console = web
			logger.Info("serving the console", "dir", web.Dir())
		}
	}

	// What extensions add is collected now and mounted with the core's
	// routes, from none of which it may take a request. Their context ends
	// when the daemon stops, whatever stops it. The background work they ask
	// for starts once every route is mounted, and is waited for, without a
	// bound, before the engine, the registry and the database close.
	extensionCtx, stopExtensions := context.WithCancel(ctx)
	extensions := newExtensionRun(sealer, svc, logger)
	defer func() {
		stopExtensions()
		extensions.wait()
	}()
	extra, err := collect(extensionCtx, opts.Extensions, Deps{
		Config: cfg, Service: svc, Log: logger, Limits: limits, SignInLimits: signInLimits,
		Metrics: metrics.Registry(), Console: catchAll(console), run: extensions,
	})
	if err != nil {
		return err
	}
	mux, err := routes(handler, metricsRoute, console, extra...)
	if err != nil {
		return err
	}
	extensions.start(extensionCtx)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
		// No write timeout: the event stream and the long poll are supposed
		// to outlive any fixed deadline, and a server-wide one would cut them
		// off mid-stream. Each route carries its own timeout instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// net/http's own complaints — a handler that panicked, a client
		// that sent garbage — go through the same redaction as everything
		// else, instead of straight to stderr.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	keyCount, err := keys.Count(ctx)
	if err != nil {
		return err
	}
	if carried, err := keys.CarriedOverLive(ctx); err != nil {
		return err
	} else if carried > 0 {
		// Migration 0012 found people's keys reaching mailboxes of several
		// workspaces: they work on exactly those until they expire, within
		// a year, or lose their last one.
		logger.Info("api keys carried over from before keys belonged to a workspace are still live; each "+
			"workspace they reach lists them to its owners and admins, and the person who created each to them",
			"keys", carried)
	}
	if keyCount == 0 {
		// --bootstrap refuses to run beside a live daemon, so the hint has to
		// say so or the operator's first try fails. Where a console's people
		// sign in, only the command line and other non-browser clients are
		// shut out, so this is information, not a warning; on a server with
		// no people a key is the only way in.
		hint := "stop the daemon and run: mailserver apikey create --bootstrap --scope admin --name cli"
		if cfg.WebDir != "" && (cfg.PasswordSignIn || opts.ExternalSignInOnly) {
			logger.Info("no api keys exist yet; only console sessions can call this daemon", "hint", hint)
		} else {
			logger.Warn("no api keys exist yet; nothing can call this daemon", "hint", hint)
		}
	}
	// An invitation is how people arrive where they sign in with a password
	// here, through a console. On a server with no people nobody arrives,
	// and where people sign in only through an extension no command here
	// brings anyone, and nobody is an owner by arriving: nothing is said of
	// people then.
	if passwords && (cfg.WebDir != "" || cfg.PublicURL != "") {
		userCount, err := users.Count(ctx)
		if err != nil {
			return err
		}
		if userCount == 0 {
			logger.Info("nobody can sign in to the console yet", "hint", firstInviteHint(cfg.PublicURL != "", keyCount > 0))
		} else if orphaned, err := users.NeedsFirstOwner(ctx); err != nil {
			return err
		} else if orphaned {
			// People, and nobody to invite more or close an account: the
			// last owner deleted with --force, say, or a first sign-up that
			// an older release would have made an owner.
			logger.Warn("nobody administers this server: no active owner, and no owner invite waiting",
				"hint", firstInviteHint(cfg.PublicURL != "", keyCount > 0)+" (an address with no account yet)")
		}
	}
	if len(cfg.RetiredMCP) > 0 {
		// Named, never their values: MAIL_MCP_KEY is a key.
		logger.Warn("this server serves no MCP of its own, at /mcp or over standard input and output: MCP comes "+
			"through the hosted service once a server is connected to it; these variables ask for MCP and do nothing",
			"variables", cfg.RetiredMCP)
	}
	logger.Info("starting", "version", opts.Version, "config", cfg.String())

	// Abandoned consent attempts hold a usable PKCE verifier, so they are
	// swept rather than left to age out of nobody's attention — and each one
	// swept is recorded on its account, which is how a web flow nobody came
	// back from stops saying pending_auth.
	go sweepFlows(ctx, accounts, logger)

	// What the privacy policy says is kept only so long is deleted on a
	// schedule, on the daemon's context. Waited for on the way out, so the
	// database is never closed under a sweep.
	houseCtx, stopHousekeeping := context.WithCancel(ctx)
	housekeeping := make(chan struct{})
	go func() {
		defer close(housekeeping)
		housekeep(houseCtx, users, db.SweepSends, svc.SweepRemovedMailboxes, svc.SweepDirectory, svc, svc, db.Scrub, logger,
			housekeepingInterval)
	}()
	defer func() {
		stopHousekeeping()
		<-housekeeping
	}()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", metrics.Handler())
		metricsSrv = &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("serving metrics", "addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics server: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// A bounded shutdown: an IDLE connection that will not close politely must
	// not hold the process open forever.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if metricsSrv != nil {
		// The metrics listener going down noisily must not obscure the
		// shutdown of the listener that serves mail.
		//nolint:errcheck // secondary listener
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// routes is everything the daemon's listener answers: the core's routes (see
// coreRoutes), then the routes extensions added, which may take nothing a
// core route answers (see Extension), and last the console when there is one.
func routes(rest *api.Handler, metrics, console http.Handler, extra ...route) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	coreRoutes(mux, rest, metrics)
	if err := mountExtensions(mux, extra); err != nil {
		return nil, err
	}
	// Last, and only as a catch-all: the mux prefers every more specific
	// pattern above, so the console can never shadow an endpoint.
	if console != nil {
		mux.Handle("/", console)
	}
	return mux, nil
}

// catchAll is what answers a request routes leaves to its catch-all: the
// console, or, without one, the mux's own answer to a path no route takes.
// An extension hands back to it what it does not answer below a prefix it
// takes (Deps.Console).
func catchAll(console http.Handler) http.Handler {
	if console == nil {
		return http.NotFoundHandler()
	}
	return console
}

// coreRoutes mounts the REST API, /mcp and everything under it, and /metrics
// when metrics is not nil (it has no listener of its own).
//
// This server serves no MCP of its own: /mcp and everything under it answer
// the API's own 404, as an endpoint that does not exist does, for every
// method, whether or not a console is served behind it, so that a client
// still configured for an earlier release's learns nothing else.
func coreRoutes(mux *http.ServeMux, rest *api.Handler, metrics http.Handler) {
	rest.Mount(mux)
	mux.Handle("/mcp", webui.NotFound())
	mux.Handle("/mcp/", webui.NotFound())
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
}

// debugWriter sends the raw IMAP protocol to the log at debug level. The
// logger redacts the XOAUTH2 line on the way out; configuration refuses this
// outside development regardless.
type debugWriter struct{ logger *slog.Logger }

func (w debugWriter) Write(p []byte) (int, error) {
	w.logger.Debug("imap", "wire", strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}

// firstInviteHint is the command that can actually invite the first person on
// this daemon. The REST form needs an admin key to call it with and
// MAIL_PUBLIC_URL to build the link from, and answers 409 without the latter;
// the bootstrap form needs neither, but refuses to run beside a live daemon.
func firstInviteHint(publicURL, anyKey bool) string {
	switch {
	case !publicURL:
		return "set MAIL_PUBLIC_URL (invite links are built from it), then stop the daemon and run: " +
			"mailserver user invite --bootstrap --role owner --email ADDRESS"
	case !anyKey:
		return "stop the daemon and run: mailserver user invite --bootstrap --role owner --email ADDRESS"
	default:
		return "mailserver user invite --role owner --email ADDRESS (needs MAIL_ADMIN_KEY), " +
			"or with the daemon stopped: mailserver user invite --bootstrap --role owner --email ADDRESS"
	}
}

// personalReplacementHint says at start, under a source that mirrors a
// directory, which people's local personal workspaces a document of theirs
// will not replace while they hold mailboxes, live or removed, or live keys
// (service.PersonalReplacementBlocks): the directory's personal workspace
// waits for them, and the rest of each document applies. Under the local
// source it says nothing.
func personalReplacementHint(ctx context.Context, svc *service.Service, source workspace.Source, logger *slog.Logger) error {
	if source == nil || source.Name() == workspace.SourceLocal {
		return nil
	}
	blocks, err := svc.PersonalReplacementBlocks(ctx)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		logger.Warn("personal workspaces that a directory's document will not replace while they hold these",
			"user", b.UserID, "workspace", b.WorkspaceID, "accounts", b.Accounts, "live_keys", b.LiveKeys,
			"stopped_keys", b.StoppedKeys)
	}
	return nil
}
