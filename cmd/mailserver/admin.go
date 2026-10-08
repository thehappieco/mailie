package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/thehappieco/mailie/internal/app"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func apikeyCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: apikey needs a subcommand: create, list or revoke", errUsage)
	}
	switch args[0] {
	case "create":
		return apikeyCreate(ctx, cfg, args[1:])
	case "list":
		return apikeyList(ctx, cfg, args[1:])
	case "revoke":
		return apikeyRevoke(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown apikey subcommand %q", errUsage, args[0])
	}
}

func apikeyCreate(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "read, write, send or admin")
	name := fs.String("name", "", "what this key is for, so it can be recognised later")
	accounts := fs.String("accounts", "", "comma-separated ids of operator mailboxes; empty means every one of them")
	days := fs.Int("days", 365, "how many days the key lives (at most 365)")
	bootstrap := fs.Bool("bootstrap", false, "write directly to the database, for the first key, when no daemon is running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scope, err := auth.ParseScope(*scopeFlag)
	if err != nil {
		return err
	}
	if *name == "" {
		return errors.New("apikey create: --name is required")
	}
	var accountIDs []string
	for _, id := range strings.Split(*accounts, ",") {
		if id = strings.TrimSpace(id); id != "" {
			accountIDs = append(accountIDs, id)
		}
	}
	req := auth.NewKeyRequest{
		Name: *name, Scope: scope, AccountIDs: accountIDs,
		TTL: time.Duration(*days) * 24 * time.Hour,
	}

	if *bootstrap {
		db, release, err := openExclusively(ctx, cfg)
		if err != nil {
			return err
		}
		defer release()

		req.CreatedBy = "cli"
		secret, key, err := auth.NewKeys(db).Issue(ctx, req)
		if err != nil {
			return err
		}
		printNewKey(secret, key.Prefix, key.Scope, key.ExpiresAt)
		return nil
	}

	body, err := adminPost(ctx, cfg, "/v1/apikeys", map[string]any{
		"name": req.Name, "scope": string(req.Scope),
		"account_ids": req.AccountIDs, "expires_in_days": *days,
	})
	if err != nil {
		return err
	}
	var created struct {
		Key       string `json:"key"`
		Prefix    string `json:"prefix"`
		Scope     string `json:"scope"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return fmt.Errorf("apikey create: unexpected response: %w", err)
	}
	printNewKey(created.Key, created.Prefix, auth.Scope(created.Scope), time.Unix(created.ExpiresAt, 0))
	return nil
}

func printNewKey(secret, prefix string, scope auth.Scope, expires time.Time) {
	fmt.Println(secret)
	fmt.Fprintf(os.Stderr, "\nprefix %s, scope %s, expires %s\n", prefix, scope, expires.Format(time.RFC3339))
	// Said once, because it is true once: only the Argon2id hash is stored.
	fmt.Fprintln(os.Stderr, "This is the only time the key is shown. Store it now.")
}

func apikeyList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, err := adminGet(ctx, cfg, "/v1/apikeys")
	if err != nil {
		return err
	}
	var keys []struct {
		Prefix      string   `json:"prefix"`
		Name        string   `json:"name"`
		Scope       string   `json:"scope"`
		WorkspaceID string   `json:"workspace_id"`
		AccountIDs  []string `json:"account_ids"`
		ExpiresAt   int64    `json:"expires_at"`
		RevokedAt   int64    `json:"revoked_at"`
		LastUsedAt  int64    `json:"last_used_at"`
	}
	if err := json.Unmarshal(body, &keys); err != nil {
		return fmt.Errorf("apikey list: unexpected response: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// The tabwriter buffers; Flush at the end is what reports a write error.
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "PREFIX\tNAME\tSCOPE\tWORKSPACE\tACCOUNTS\tEXPIRES\tLAST USED\tSTATE")
	for _, k := range keys {
		state := "active"
		if k.RevokedAt != 0 {
			state = "revoked"
		} else if k.ExpiresAt != 0 && time.Now().After(time.Unix(k.ExpiresAt, 0)) {
			state = "expired"
		}
		// An instance key with no restriction reaches every operator
		// mailbox; a workspace key reaches only the mailboxes it holds.
		accounts := strings.Join(k.AccountIDs, ",")
		switch {
		case accounts != "":
		case k.WorkspaceID == workspace.OperatorID:
			accounts = "all"
		default:
			accounts = "none"
		}
		ws := k.WorkspaceID
		if ws == "" {
			// Carried over from a person's key that reached several
			// workspaces (migration 0012).
			ws = "several"
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.Prefix, k.Name, k.Scope, ws, accounts,
			formatUnix(k.ExpiresAt), formatUnix(k.LastUsedAt), state)
	}
	return w.Flush()
}

func formatUnix(v int64) string {
	if v == 0 {
		return "-"
	}
	return time.Unix(v, 0).Format("2006-01-02")
}

func apikeyRevoke(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("apikey revoke: give exactly one key prefix")
	}
	if _, err := adminDo(ctx, cfg, http.MethodDelete, "/v1/apikeys/"+url.PathEscape(fs.Arg(0)), nil); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "revoked")
	return nil
}

func migrateCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be applied and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dryRun {
		db, release, err := openExclusivelyWithOptions(ctx, cfg, store.Options{SkipMigrate: true})
		if err != nil {
			return err
		}
		defer release()

		pending, err := db.PendingMigrations(ctx)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("schema is up to date")
			return nil
		}
		for _, m := range pending {
			if m.Rebuild {
				// Applied with the same guarantees as any other, but it
				// rewrites a table every mailbox hangs off: the moment for the
				// backup an upgrade deserves anyway.
				fmt.Println("pending:", m.Name, "(rebuilds a table; back up the data directory first)")
				continue
			}
			fmt.Println("pending:", m.Name)
		}
		return nil
	}

	db, release, err := openExclusively(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("schema is at version %d\n", version)
	return nil
}

func rewrapCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("rewrap-credentials", flag.ContinueOnError)
	newRoot := fs.Bool("new-send-hash-root", false,
		"replace a send-hash root no configured key opens (its key is lost) with a new one, and re-seal nothing")
	kmsKeyLost := fs.Bool("kms-key-lost", false,
		"with --new-send-hash-root: also replace a root of the configured KMS key's kind that it does not unwrap, "+
			"which the KMS key and MAIL_ENV that sealed it would still open; only when that key is lost for good")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kmsKeyLost && !*newRoot {
		return errors.New("rewrap-credentials: --kms-key-lost goes only with --new-send-hash-root")
	}

	sealer, err := app.NewSealer(ctx, cfg)
	if err != nil {
		return err
	}
	db, release, err := openExclusively(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()

	if *newRoot {
		if err := replaceSendHashRoot(ctx, db, sealer, *kmsKeyLost); err != nil {
			return app.ExplainSealed(err, sealer)
		}
		fmt.Println("replaced the send-hash root, sealed with", sealer.Describe())
		fmt.Fprintln(os.Stderr, "A send repeated from before the replacement is not recognised: one without "+
			"an idempotency key is sent again. Credentials no configured key opens stay as they are: "+
			"authorize their mailboxes again.")
		return nil
	}

	done, err := rewrapCredentials(ctx, db, sealer)
	if err != nil {
		return app.ExplainSealed(err, sealer)
	}
	switch {
	case done.credentials == 0 && !done.root:
		fmt.Println("every credential and the send-hash root are already sealed with", sealer.Describe())
	default:
		what := plural(done.credentials, "credential")
		if done.root {
			what += " and the send-hash root"
		}
		fmt.Printf("re-sealed %s with %s\n", what, sealer.Describe())
		switch {
		case cfg.Credentials.Sealer() == config.SealerAWSKMS:
			fmt.Fprintln(os.Stderr, "Nothing is sealed under a credential key any more: MAIL_CREDENTIAL_KEY_HEX, "+
				"MAIL_CREDENTIAL_KEY_ID and MAIL_CREDENTIAL_PREVIOUS_KEYS can now be removed from the environment. "+
				"Keep them elsewhere for as long as a backup taken before this rewrap is kept.")
		case cfg.Credentials.KMSOpensOnly:
			fmt.Fprintln(os.Stderr, "Nothing is sealed under the KMS key any more: MAIL_CREDENTIAL_KMS_KEY_ARN and "+
				"MAIL_CREDENTIAL_SEALER can now be removed from the environment. Keep the KMS key, and its policy, "+
				"for as long as a backup taken before this rewrap is kept.")
		default:
			fmt.Fprintln(os.Stderr,
				"The previous key can now be removed from MAIL_CREDENTIAL_PREVIOUS_KEYS.")
		}
	}
	return nil
}

// openExclusively opens the database for a command that must not run beside
// the daemon: both would be writing the same rows with no coordination.
func openExclusively(ctx context.Context, cfg config.Config) (*store.Store, func(), error) {
	return openExclusivelyWithOptions(ctx, cfg, store.Options{})
}

func openExclusivelyWithOptions(ctx context.Context, cfg config.Config, opts store.Options) (*store.Store, func(), error) {
	lock, err := lockfile.Acquire(cfg.LockPath())
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			return nil, nil, fmt.Errorf("the daemon is running on %s; stop it first, or use the REST API", cfg.DataDir)
		}
		return nil, nil, err
	}
	db, err := store.Open(ctx, cfg.DatabasePath(), opts)
	if err != nil {
		//nolint:errcheck // unwinding a failed open; the open error is the one to report
		_ = lock.Release()
		return nil, nil, err
	}
	return db, func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "mailserver: closing the database: %v\n", err)
		}
		if err := lock.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "mailserver: releasing the lock: %v\n", err)
		}
	}, nil
}

// --- the daemon's REST API, as a client -----------------------------------

func adminGet(ctx context.Context, cfg config.Config, path string) ([]byte, error) {
	return adminDo(ctx, cfg, http.MethodGet, path, nil)
}

func adminPost(ctx context.Context, cfg config.Config, path string, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return adminDo(ctx, cfg, http.MethodPost, path, encoded)
}

func adminDo(ctx context.Context, cfg config.Config, method, path string, body []byte) ([]byte, error) {
	if cfg.AdminKey == "" {
		return nil, errors.New("MAIL_ADMIN_KEY is not set; issue one with `mailserver apikey create --bootstrap --scope admin --name cli`")
	}
	endpoint := "http://" + cfg.HTTPAddr + path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Longer than any route's own deadline — adding a password account
	// spends up to thirty seconds logging in to its server — so the daemon's
	// answer, not this client, is what ends a slow request.
	client := &http.Client{Timeout: 70 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the daemon at %s: %w", cfg.HTTPAddr, err)
	}
	//nolint:errcheck // the body has been read; a close failure changes nothing
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		failure := &apiError{Status: resp.StatusCode, Message: resp.Status}
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			failure.RetryAfter = time.Duration(seconds) * time.Second
		}
		var wire struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &wire) == nil && wire.Message != "" {
			failure.Code, failure.Message = wire.Code, wire.Message
		}
		// Without MAIL_ADMIN_API the key routes are not mounted: a daemon
		// serving the console answers its JSON not_found, one without a
		// console the router's plain-text 404.
		if strings.Contains(path, "/v1/apikeys") && (failure.Code == "not_found" ||
			(failure.Code == "" && resp.StatusCode == http.StatusNotFound)) {
			return nil, fmt.Errorf("%w (is MAIL_ADMIN_API=true set on the daemon?)", failure)
		}
		return nil, failure
	}
	return payload, nil
}

// apiError is an error reply from the daemon, kept whole so a caller that can
// wait — a poll — can tell "slow down" from "no".
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return "the daemon answered " + e.Message
	}
	return e.Code + ": " + e.Message
}
