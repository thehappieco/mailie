package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
	"github.com/thehappieco/mailie/internal/webui"
)

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	mcpStdio := fs.Bool("mcp-stdio", false, "also speak MCP over stdin and stdout, for a client that launches this binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mcpStdio {
		// Standard output is the MCP client's: one stray line there and the
		// client stops understanding the stream. Everything logged goes to
		// standard error.
		logger = obs.NewLoggerTo(os.Stderr, cfg.Log.Level, cfg.Log.Format)
		ctx = obs.WithLogger(ctx, logger)
		if cfg.MCPKey == "" {
			return errors.New("serve --mcp-stdio needs MAIL_MCP_KEY: an API key created in the console " +
				"(API keys & MCP) for the client that launches the daemon to act with")
		}
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

	db, err := store.Open(ctx, cfg.DatabasePath(), store.Options{})
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

	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		return err
	}

	metrics := obs.NewMetrics()
	keys := auth.NewKeys(db)
	users := auth.NewUsers(db)
	bus := events.NewBus(events.NewJournal(db))
	started := time.Now()

	var imapDebug io.Writer
	if cfg.IMAPDebug {
		imapDebug = debugWriter{logger}
	}
	// Every account state change is journaled as account.state in its own
	// transaction; the bus hears it once that has committed.
	repo := account.NewRepository(db, keyring)
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
		SpoolDir:     cfg.SpoolDir(),
		Debug:        imapDebug,
		Log:          logger,
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

	// What a send record keeps of a message is a hash under this key, so the
	// database, a backup or the log alone cannot confirm a guess of it.
	sendHashKey, err := keyring.DeriveKey("send-compose-hash")
	if err != nil {
		return err
	}
	svc := service.New(service.Deps{
		Accounts: accounts, Keys: keys, Users: users, Store: db, Bus: bus, Sync: engine, Log: logger,
		PublicURL: cfg.PublicURL, DownloadSpoolBytes: cfg.DownloadSpoolBytes, SpoolDir: cfg.SpoolDir(),
		SendHashKey: sendHashKey, ConsentVersions: cfg.Consent, MCPHTTP: cfg.MCPHTTP,
	})

	// One limiter for REST and MCP: a key has one budget whichever way it
	// comes in.
	limits := ratelimit.DefaultAuth(cfg.TrustedProxies)
	handler := &api.Handler{
		Service:      svc,
		Limits:       limits,
		SignInLimits: ratelimit.DefaultSignIn(cfg.TrustedProxies),
		Metrics:      metrics,
		Log:          logger,
		AdminAPI:     cfg.AdminAPI,
		Version:      version,
		Started:      started,
	}

	// MCP, for AI assistants and other tools: Streamable HTTP at /mcp, over
	// the same service, with the same keys and limits as REST — unless the
	// deployment switched it off, when only --mcp-stdio speaks MCP. The
	// stdio transport uses the same tools either way.
	tools := mcp.New(svc, logger, version)
	var mcpHTTP http.Handler
	mcpLog := []any{"mcp_http", "off"}
	if cfg.MCPHTTP {
		h := tools.HTTPHandler(mcp.HTTPOptions{Limits: limits, Metrics: metrics, PublicURL: cfg.PublicURL})
		storeBytes, storeAge := h.EventStore().Bounds()
		mcpLog = []any{"mcp_event_store_bytes", storeBytes, "mcp_event_store_age", storeAge.String()}
		mcpHTTP = h
	}
	var metricsRoute http.Handler
	if cfg.MetricsAddr == "" {
		metricsRoute = metrics.Handler()
	}
	var console http.Handler
	if cfg.WebDir != "" {
		web, err := webui.New(cfg.WebDir)
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

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: routes(handler, mcpHTTP, metricsRoute, console),
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
	if keyCount == 0 {
		// --bootstrap refuses to run beside a live daemon, so the hint has to
		// say so or the operator's first try fails. Only the command line and
		// other non-browser clients are shut out: a console session needs no
		// key, so with the console served this is information, not a warning.
		msg, hint := "no api keys exist yet; nothing can call this daemon",
			"stop the daemon and run: mailserver apikey create --bootstrap --scope admin --name cli"
		if cfg.WebDir != "" {
			logger.Info("no api keys exist yet; only console sessions can call this daemon", "hint", hint)
		} else {
			logger.Warn(msg, "hint", hint)
		}
	}
	if cfg.WebDir != "" || cfg.PublicURL != "" {
		userCount, err := users.Count(ctx)
		if err != nil {
			return err
		}
		if userCount == 0 {
			logger.Info("nobody can sign in to the console yet", "hint", firstInviteHint(cfg.PublicURL != "", keyCount > 0))
		}
	}
	logger.Info("starting", append([]any{"version", version, "config", cfg.String()}, mcpLog...)...)

	// The stdio client acts with MAIL_MCP_KEY, checked now as a bearer key
	// would be: a daemon that started without a key that works would only
	// fail at the client's first call.
	stdioDone := make(chan error, 1)
	if *mcpStdio {
		p, err := svc.AuthenticateTool(ctx, cfg.MCPKey, nil)
		if err != nil {
			return fmt.Errorf("MAIL_MCP_KEY: %s", service.MessageOf(err))
		}
		go func() { stdioDone <- tools.RunStdio(ctx, p) }()
	}

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
		housekeep(houseCtx, users, db.SweepSends, db.Scrub, logger, housekeepingInterval)
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
	case err := <-stdioDone:
		// The client that launched the daemon is gone, and it is what the
		// daemon was launched for.
		if err != nil {
			logger.Warn("the MCP client's session ended with an error; shutting down", "err", err)
		} else {
			logger.Info("the MCP client hung up; shutting down")
		}
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

// routes is everything the daemon's listener answers: the REST API, MCP over
// Streamable HTTP when mcpHTTP is not nil, /metrics when metrics is not nil
// (it has no listener of its own), and the console when there is one.
//
// With MCP over HTTP switched off, /mcp and everything under it answer the
// API's own 404, as an endpoint that does not exist does, whether or not a
// console is served behind it.
func routes(rest *api.Handler, mcpHTTP, metrics, console http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	rest.Mount(mux)
	if mcpHTTP != nil {
		mux.Handle("/mcp", mcpHTTP)
	} else {
		mux.Handle("/mcp", webui.NotFound())
		mux.Handle("/mcp/", webui.NotFound())
	}
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
	// Last, and only as a catch-all: the mux prefers every more specific
	// pattern above, so the console can never shadow an endpoint.
	if console != nil {
		mux.Handle("/", console)
	}
	return mux
}

// sweepSpool removes the sections the IMAP adapter spooled and nobody
// closed, and the attachments of a send that never ended: what a crash in the
// middle of a download or a send leaves. It returns how many it removed.
func sweepSpool(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || (!strings.HasPrefix(e.Name(), "part-") && !strings.HasPrefix(e.Name(), "send-")) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// sweepFlows removes expired consent attempts.
func sweepFlows(ctx context.Context, accounts *account.Registry, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := accounts.SweepFlows(ctx); err != nil {
				logger.Debug("sweeping expired authorisation attempts failed", "err", err)
			}
		}
	}
}

// housekeepingInterval is how often the retention sweep runs.
const housekeepingInterval = time.Hour

// housekeep enforces retention: now, and then every interval until ctx ends.
// Today that is the unused invites, deleted within auth.InviteRetention of
// expiring, and the send records and their send.finished notices, deleted
// store.SendRetention after their last change. scrub then takes the deleted
// rows out of the write-ahead log too.
func housekeep(ctx context.Context, users *auth.Users, sweepSends func(context.Context, time.Duration) (int, int, error),
	scrub func(context.Context) error, logger *slog.Logger, every time.Duration,
) {
	sweep := func() {
		// Told when the next sweep is, so nothing outlives its retention
		// by waiting for it.
		deleted := 0
		n, err := users.SweepInvites(ctx, every)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("sweeping expired invites failed", "err", err)
		case n > 0:
			logger.Info("deleted expired invites", "count", n)
			deleted += n
		}
		records, notices, err := sweepSends(ctx, every)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("sweeping old send records failed", "err", err)
		case records+notices > 0:
			logger.Info("deleted old send records", "count", records, "notices", notices)
			deleted += records + notices
		}
		if deleted > 0 {
			if err := scrub(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("emptying the write-ahead log after the sweep failed; a later checkpoint will", "err", err)
			}
		}
	}
	sweep()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
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
			"mailserver user invite --bootstrap --email ADDRESS"
	case !anyKey:
		return "stop the daemon and run: mailserver user invite --bootstrap --email ADDRESS"
	default:
		return "mailserver user invite --email ADDRESS (needs MAIL_ADMIN_KEY), " +
			"or with the daemon stopped: mailserver user invite --bootstrap --email ADDRESS"
	}
}
