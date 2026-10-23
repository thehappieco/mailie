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
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

func accountCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: account needs a subcommand: add, list, authorize, folders, sync, remove, removed, "+
			"recover or purge", errUsage)
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
	case "removed":
		return accountRemoved(ctx, cfg, args[1:])
	case "recover":
		return accountRecover(ctx, cfg, args[1:])
	case "purge":
		return accountPurge(ctx, cfg, args[1:])
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
	Sync        struct {
		Held bool `json:"held"`
	} `json:"sync"`
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
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("account authorize: give exactly one account id")
	}
	accountID := rest[0]

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
		if note == "" && a.Sync.Held {
			note = "sync held since it was recovered"
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

// accountRemove removes a mailbox. The daemon keeps it 30 days, used by
// nothing, recoverable by its workspace's owners — for an operator mailbox,
// the operator — and then deletes it for good; one that never connected,
// which holds nothing, it deletes at once.
func accountRemove(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("account remove: give exactly one account id")
	}
	accountID := rest[0]

	if !*yes && !confirmed(fmt.Sprintf("Remove %s? It is kept 30 days, used by nothing and recoverable, "+
		"and then deleted for good with its stored credentials and everything indexed for it.", accountID)) {
		return nil
	}
	// The daemon removes nothing unless the request repeats the id.
	var removal service.Removal
	if err := adminJSON(ctx, cfg, http.MethodDelete,
		"/v1/accounts/"+url.PathEscape(accountID)+"?confirm="+url.QueryEscape(accountID), nil, &removal); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, removalSummary(accountID, removal))
	return nil
}

// removalSummary says what removing a mailbox did, and how to undo it or
// finish it while it is kept.
func removalSummary(accountID string, r service.Removal) string {
	if r.Purged {
		return "deleted: it never connected, so nothing was kept"
	}
	return fmt.Sprintf("removed; kept until %s (`mailserver account recover %s` brings it back, "+
		"`mailserver account purge %s` deletes it for good now)", stamp(r.PurgeAfter), accountID, accountID)
}

// accountRemoved lists a workspace's removed mailboxes: the operator's own by
// default, and any workspace's by --workspace, which the operator may delete
// for good (account purge) and never recovers.
func accountRemoved(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account removed", flag.ContinueOnError)
	id := fs.String("workspace", workspace.OperatorID, "the workspace's id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("account removed: unexpected argument %q", fs.Arg(0))
	}
	var removed []service.RemovedMailbox
	if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces/"+url.PathEscape(*id)+"/removed-mailboxes",
		nil, &removed); err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintf(os.Stderr, "no removed mailboxes in %s\n", *id)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "ID\tEMAIL\tPROVIDER\tREMOVED\tBY\tKEPT UNTIL\tINDEX\tREADERS")
	for _, m := range removed {
		index := "kept"
		if !m.IndexKept {
			index = "none"
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", m.ID, m.Email, m.Provider, stamp(m.RemovedAt),
			orDash(m.RemovedBy), stamp(m.PurgeAfter), index, m.Readers)
	}
	return w.Flush()
}

// accountRecover recovers a removed mailbox of the operator workspace within
// its 30 days. Its sync stays held until it is switched on again (account
// sync ID on); a person's or a team's is recovered by its owners, in the
// console.
func accountRecover(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account recover", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("account recover: give exactly one account id")
	}
	accountID := fs.Arg(0)
	var recovered accountJSON
	if err := adminJSON(ctx, cfg, http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/recover",
		nil, &recovered); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recovered %s; its sync is held until `mailserver account sync %s on`\n",
		recovered.Email, accountID)
	return nil
}

// accountPurge deletes a removed mailbox for good, of any workspace: the
// operator's way to erase one when its person asks. A live mailbox is
// removed first.
func accountPurge(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account purge", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("account purge: give exactly one account id")
	}
	accountID := rest[0]
	if !*yes && !confirmed(fmt.Sprintf("Delete the removed mailbox %s for good, with its stored credentials and "+
		"everything indexed for it? Nothing brings it back.", accountID)) {
		return nil
	}
	// The daemon deletes nothing unless the request repeats the id.
	if err := adminJSON(ctx, cfg, http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/purge",
		service.PurgeAccountRequest{Confirm: accountID}, nil); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "deleted for good")
	return nil
}

// parseInterspersed parses args with fs, its flags before or after the
// arguments — `account remove ID --yes` as well as `account remove --yes
// ID` — and returns the arguments. The flag package stops at the first
// argument, which would leave a flag written after an id unread.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// confirmed asks a yes-or-no question on the terminal; anything but y is a
// no, which it says.
func confirmed(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	var answer string
	//nolint:errcheck // an unreadable answer is not a yes
	_, _ = fmt.Scanln(&answer)
	if strings.EqualFold(strings.TrimSpace(answer), "y") {
		return true
	}
	fmt.Fprintln(os.Stderr, "cancelled")
	return false
}

// stamp is a moment in unix seconds as the command line prints one.
func stamp(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
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
	Held            bool   `json:"held"`
	HeldSince       int64  `json:"held_since"`
}

// accountSync shows an account's sync, asks for a pass, switches an
// operator mailbox's on — which also lifts the hold of one recovered after
// it was removed — or turns any mailbox's off. A person's mailbox syncs when
// they turn sync on in the console, and a team's when its owners or admins
// do, and the daemon refuses to let anyone else turn it on for them; off is
// the operator's turn-off (POST /v1/accounts/{id}/sync/stop), of a mailbox of
// any workspace, live or removed, which deletes its index and tells its
// readers: how the operator erases a mailbox's index when asked to, or stops
// one being abused. A team's owners or admins may turn it on again, and a
// person, whose mailbox it holds.
func accountSync(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account sync", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation before switching sync off")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("%w: account sync ID status|now|on|off", errUsage)
	}
	accountID := rest[0]
	path := "/v1/accounts/" + url.PathEscape(accountID) + "/sync"

	var raw []byte
	switch rest[1] {
	case "status":
		raw, err = adminGet(ctx, cfg, path)
	case "now":
		raw, err = adminDo(ctx, cfg, http.MethodPost, path, nil)
	case "on":
		raw, err = adminDo(ctx, cfg, http.MethodPut, path, []byte(`{"enabled":true}`))
	case "off":
		// Switching off deletes the account's index, as withdrawing consent
		// does for a person's mailboxes, whichever workspace it is in.
		if !*yes && !confirmed(fmt.Sprintf("Switch sync off for %s and delete everything indexed for it? "+
			"Whoever reads it is told.", accountID)) {
			return nil
		}
		raw, err = adminDo(ctx, cfg, http.MethodPost, path+"/stop", nil)
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
	if rest[1] == "off" {
		if st.Held {
			fmt.Println("sync off, its index deleted; held until its person turns it on again")
			return nil
		}
		fmt.Println("sync off, its index deleted")
		return nil
	}
	if st.Held {
		fmt.Printf("sync held since %s, when it was recovered; `mailserver account sync %s on` turns it on\n",
			stamp(st.HeldSince), accountID)
		return nil
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
