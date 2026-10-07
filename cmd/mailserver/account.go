package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/thehappieco/mailie/internal/config"
)

func accountCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: account needs a subcommand: add, list, authorize, folders, sync or remove", errUsage)
	}
	switch args[0] {
	case "add":
		return accountAdd(ctx, cfg, args[1:])
	case "list":
		return accountList(ctx, cfg, args[1:])
	case "authorize", "authorise":
		return accountAuthorize(ctx, cfg, args[1:])
	case "folders":
		return accountFolders(ctx, cfg, args[1:])
	case "remove":
		return accountRemove(ctx, cfg, args[1:])
	case "sync":
		return accountSync(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown account subcommand %q", errUsage, args[0])
	}
}

type accountJSON struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Provider    string `json:"provider"`
	AuthKind    string `json:"auth_kind"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	SyncTier    string `json:"sync_tier"`
	LastError   string `json:"last_error"`
}

type authFlowJSON struct {
	AuthURL         string `json:"auth_url"`
	State           string `json:"state"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresAt       int64  `json:"expires_at"`
}

type addAccountResponse struct {
	Account accountJSON   `json:"account"`
	Auth    *authFlowJSON `json:"auth"`
}

func accountAdd(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	email := fs.String("email", "", "the mailbox address")
	providerName := fs.String("provider", "", "gmail, microsoft, icloud or imap (guessed from the address when omitted)")
	displayName := fs.String("name", "", "how to label this account")
	paste := fs.Bool("paste", false, "print the URL and wait for the redirect to be pasted back, for a machine with no browser")
	device := fs.Bool("device", false, "show a code to type on another device (Microsoft only; usually blocked by tenant policy)")
	password := fs.String("password", "", "IMAP password, for a generic server, or an app-specific password for iCloud")
	imapHost := fs.String("imap-host", "", "override the IMAP host")
	imapPort := fs.Int("imap-port", 0, "override the IMAP port")
	smtpHost := fs.String("smtp-host", "", "override the SMTP host")
	smtpPort := fs.Int("smtp-port", 0, "override the SMTP port")
	smtpTLS := fs.String("smtp-tls", "", "implicit or starttls")
	loginUser := fs.String("login-user", "", "sign in as this instead of the address: the server's user name, "+
		"or the @icloud.com address for an iCloud+ custom domain")
	initialDays := fs.Int("initial-days", 0, "how far back the first sync reaches; 0 for the default (90), negative "+
		"for everything. Only for a mailbox nobody owns: a person's is always synced back 90 days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("account add: --email is required")
	}

	flow := "loopback"
	switch {
	case *device:
		flow = "device"
	case *paste:
		flow = "pasted"
	}

	body := map[string]any{
		"email": *email, "display_name": *displayName, "flow": flow,
	}
	for key, value := range map[string]any{
		"provider": *providerName, "password": *password, "imap_host": *imapHost,
		"smtp_host": *smtpHost, "smtp_tls": *smtpTLS, "login_user": *loginUser,
	} {
		if s, ok := value.(string); ok && s != "" {
			body[key] = s
		}
	}
	for key, value := range map[string]int{
		"imap_port": *imapPort, "smtp_port": *smtpPort, "initial_days": *initialDays,
	} {
		if value != 0 {
			body[key] = value
		}
	}

	raw, err := adminPost(ctx, cfg, "/v1/accounts", body)
	if err != nil {
		return err
	}
	var result addAccountResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("account add: unexpected response: %w", err)
	}

	fmt.Fprintf(os.Stderr, "registered %s as %s\n", result.Account.Email, result.Account.ID)
	if result.Auth == nil {
		fmt.Fprintln(os.Stderr, "ready to sync")
		return nil
	}
	return finishAuthorization(ctx, cfg, result.Account.ID, result.Auth, flow)
}

// finishAuthorization walks the person through consent.
func finishAuthorization(ctx context.Context, cfg config.Config, accountID string, flow *authFlowJSON, kind string) error {
	switch kind {
	case "device":
		fmt.Fprintf(os.Stderr, "\nOn any device, open %s\nand enter the code: %s\n",
			flow.VerificationURI, flow.UserCode)
		fmt.Fprintln(os.Stderr, "\nWaiting for approval...")
		return waitForState(ctx, cfg, accountID, "active", time.Until(time.Unix(flow.ExpiresAt, 0)))

	case "pasted":
		fmt.Fprintln(os.Stderr, "\nOpen this address in a browser:")
		fmt.Fprintf(os.Stderr, "\n  %s\n\n", flow.AuthURL)
		fmt.Fprintln(os.Stderr, "Then paste the address the browser ends on (it will look like a")
		fmt.Fprintln(os.Stderr, "connection error; that is expected) and press enter:")
		var pasted string
		if _, err := fmt.Scanln(&pasted); err != nil {
			return fmt.Errorf("account add: reading the pasted address: %w", err)
		}
		if _, err := adminPost(ctx, cfg, "/v1/accounts/oauth/callback",
			map[string]any{"redirect_url": strings.TrimSpace(pasted)}); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "authorised")
		return nil

	default:
		fmt.Fprintln(os.Stderr, "\nOpening a browser to authorise this account. If nothing opens, visit:")
		fmt.Fprintf(os.Stderr, "\n  %s\n\n", flow.AuthURL)
		openBrowser(ctx, flow.AuthURL)
		// The daemon is listening on the loopback port it advertised; it
		// completes the flow itself, and the account leaves pending_auth.
		return waitForState(ctx, cfg, accountID, "active", 10*time.Minute)
	}
}

// pollInterval is how often the CLI asks whether consent has finished. Two
// seconds is quick enough that nobody waits on it after closing the browser
// tab, and slow enough to be nowhere near any limit.
const pollInterval = 2 * time.Second

// waitForState polls until an account reaches a state, fails, or the wait runs
// out.
func waitForState(ctx context.Context, cfg config.Config, accountID, want string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		next := pollInterval
		raw, err := adminGet(ctx, cfg, "/v1/accounts/"+url.PathEscape(accountID))
		var failure *apiError
		switch {
		case errors.As(err, &failure) && failure.Code == "rate_limited":
			// Somebody else on this address is being throttled, or the
			// limits are tighter than they should be. Either way the person
			// is still in front of a consent screen; wait and ask again
			// rather than abandon the flow they are halfway through.
			next = max(failure.RetryAfter, pollInterval)
		case err != nil:
			return err
		default:
			var a accountJSON
			if err := json.Unmarshal(raw, &a); err != nil {
				return fmt.Errorf("account: unexpected response: %w", err)
			}
			switch a.State {
			case want:
				fmt.Fprintln(os.Stderr, "authorised")
				return nil
			case "needs_reauth", "error":
				return fmt.Errorf("authorisation failed: %s", firstNonEmpty(a.StateReason, a.LastError, "no reason given"))
			}
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for authorisation; " +
				"run `mailserver account authorize` to try again")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(next):
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func accountAuthorize(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account authorize", flag.ContinueOnError)
	paste := fs.Bool("paste", false, "print the URL and wait for the redirect to be pasted back")
	device := fs.Bool("device", false, "show a code to type on another device")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("account authorize: give exactly one account id")
	}
	accountID := fs.Arg(0)

	kind := "loopback"
	switch {
	case *device:
		kind = "device"
	case *paste:
		kind = "pasted"
	}

	raw, err := adminPost(ctx, cfg, "/v1/accounts/"+url.PathEscape(accountID)+"/oauth/start",
		map[string]any{"flow": kind})
	if err != nil {
		return err
	}
	var flow authFlowJSON
	if err := json.Unmarshal(raw, &flow); err != nil {
		return fmt.Errorf("account authorize: unexpected response: %w", err)
	}
	return finishAuthorization(ctx, cfg, accountID, &flow, kind)
}

func accountList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := adminGet(ctx, cfg, "/v1/accounts")
	if err != nil {
		return err
	}
	var accounts []accountJSON
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return fmt.Errorf("account list: unexpected response: %w", err)
	}
	if len(accounts) == 0 {
		fmt.Fprintln(os.Stderr, "no accounts yet; add one with `mailserver account add --email you@example.com`")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "ID\tEMAIL\tPROVIDER\tAUTH\tSTATE\tNOTE")
	for _, a := range accounts {
		note := a.StateReason
		if note == "" {
			note = a.LastError
		}
		if note == "" && a.SyncTier != "" {
			note = "sync: " + a.SyncTier
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Email, a.Provider, a.AuthKind, a.State, note)
	}
	return w.Flush()
}

type folderJSON struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	RoleSource  string `json:"role_source"`
	Synced      bool   `json:"synced"`
	Messages    uint32 `json:"messages"`
	Unseen      uint32 `json:"unseen"`
}

func accountFolders(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account folders", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("account folders: give exactly one account id")
	}
	raw, err := adminGet(ctx, cfg, "/v1/accounts/"+url.PathEscape(fs.Arg(0))+"/folders")
	if err != nil {
		return err
	}
	var folders []folderJSON
	if err := json.Unmarshal(raw, &folders); err != nil {
		return fmt.Errorf("account folders: unexpected response: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "FOLDER\tROLE\tFROM\tSYNCED\tMESSAGES\tUNSEEN")
	for _, f := range folders {
		synced := "no"
		if f.Synced {
			synced = "yes"
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\n",
			f.Name, orDash(f.Role), orDash(f.RoleSource), synced, f.Messages, f.Unseen)
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func accountRemove(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("account remove: give exactly one account id")
	}
	accountID := fs.Arg(0)

	if !*yes {
		// Removing an account discards its index and its stored credential;
		// re-adding it means authorising again and re-downloading everything.
		fmt.Fprintf(os.Stderr, "Remove %s, its stored credentials and everything indexed for it? [y/N] ", accountID)
		var answer string
		//nolint:errcheck // an unreadable answer is not a yes
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Fprintln(os.Stderr, "cancelled")
			return nil
		}
	}
	// The daemon removes nothing unless the request repeats the id.
	if _, err := adminDo(ctx, cfg, "DELETE",
		"/v1/accounts/"+url.PathEscape(accountID)+"?confirm="+url.QueryEscape(accountID), nil); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "removed")
	return nil
}

// syncJSON is an account's sync, as GET /v1/accounts/{id}/sync answers.
type syncJSON struct {
	Enabled         bool   `json:"enabled"`
	Running         bool   `json:"running"`
	State           string `json:"state"`
	Tier            string `json:"tier"`
	FoldersSynced   int    `json:"folders_synced"`
	FoldersTotal    int    `json:"folders_total"`
	Messages        int64  `json:"messages"`
	InitialProgress int    `json:"initial_progress"`
	LastSyncedAt    int64  `json:"last_synced_at"`
	ErrorClass      string `json:"error_class"`
}

// accountSync shows an account's sync, asks for a pass, or — for an account
// nobody owns, which has no person to consent — switches sync on or off. A
// person's mailbox syncs when they turn sync on in the console, and the
// daemon refuses to let anyone else decide for them.
func accountSync(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account sync", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation before switching sync off")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%w: account sync ID status|now|on|off", errUsage)
	}
	path := "/v1/accounts/" + url.PathEscape(fs.Arg(0)) + "/sync"

	var (
		raw []byte
		err error
	)
	switch fs.Arg(1) {
	case "status":
		raw, err = adminGet(ctx, cfg, path)
	case "now":
		raw, err = adminDo(ctx, cfg, http.MethodPost, path, nil)
	case "on":
		raw, err = adminDo(ctx, cfg, http.MethodPut, path, []byte(`{"enabled":true}`))
	case "off":
		if !*yes {
			// Switching off deletes the account's index, as withdrawing
			// consent does for a person's mailboxes.
			fmt.Fprintf(os.Stderr, "Switch sync off for %s and delete everything indexed for it? [y/N] ", fs.Arg(0))
			var answer string
			//nolint:errcheck // an unreadable answer is not a yes
			_, _ = fmt.Scanln(&answer)
			if !strings.EqualFold(strings.TrimSpace(answer), "y") {
				fmt.Fprintln(os.Stderr, "cancelled")
				return nil
			}
		}
		raw, err = adminDo(ctx, cfg, http.MethodPut, path, []byte(`{"enabled":false}`))
	default:
		return fmt.Errorf("%w: account sync ID status|now|on|off", errUsage)
	}
	if err != nil {
		return err
	}
	var st syncJSON
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("account sync: unexpected response: %w", err)
	}
	enabled := "off"
	if st.Enabled {
		enabled = "on"
	}
	last := "-"
	if st.LastSyncedAt != 0 {
		last = time.Unix(st.LastSyncedAt, 0).Format(time.RFC3339)
	}
	fmt.Printf("sync %s, %s", enabled, st.State)
	if st.Tier != "" {
		fmt.Printf(" (%s)", st.Tier)
	}
	fmt.Printf("; folders %d/%d, %d messages indexed, initial %d%%, last synced %s",
		st.FoldersSynced, st.FoldersTotal, st.Messages, st.InitialProgress, last)
	if st.ErrorClass != "" {
		fmt.Printf("; last error %s", st.ErrorClass)
	}
	fmt.Println()
	return nil
}

// openBrowser is best effort: the URL is printed first, so a failure here
// costs a copy and paste rather than the flow.
func openBrowser(ctx context.Context, target string) {
	// The URL is one this process built from the provider's own endpoint and
	// then printed; it is passed as a single argument to a fixed command, not
	// through a shell.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		//nolint:gosec // G204: fixed command, argument built by this process
		cmd = exec.CommandContext(ctx, "open", target)
	case "windows":
		//nolint:gosec // G204: fixed command, argument built by this process
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	default:
		//nolint:gosec // G204: fixed command, argument built by this process
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	}
	//nolint:errcheck // the URL was printed first; a failure costs a copy and paste
	_ = cmd.Start()
}
