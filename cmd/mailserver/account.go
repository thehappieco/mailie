package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
		return fmt.Errorf("%w: account needs a subcommand: add, list, rename, authorize, folders, sync, remove, removed, "+
			"recover or purge", errUsage)
	}
	switch args[0] {
	case "add":
		return accountAdd(ctx, cfg, args[1:])
	case "list":
		return accountList(ctx, cfg, args[1:])
	case "rename":
		return accountRename(ctx, cfg, args[1:])
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
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	AuthKind    string `json:"auth_kind"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	SyncTier    string `json:"sync_tier"`
	LastError   string `json:"last_error"`
	Sync        struct {
		Enabled     bool  `json:"enabled"`
		Held        bool  `json:"held"`
		Paused      bool  `json:"paused"`
		PausedSince int64 `json:"paused_since"`
	} `json:"sync"`
}

// mailboxSyncNote says where a mailbox's sync stands, as the daemon
// answered it, and the command that starts it when it does not run: an
// operator mailbox syncs nothing until the operator switches it on.
func mailboxSyncNote(accountID string, a accountJSON) string {
	switch {
	case a.Sync.Held:
		// Never an operator mailbox's, said all the same.
		return "its sync is held until its person turns it on"
	case a.Sync.Paused:
		return fmt.Sprintf("its sync is paused until `mailserver account sync %s resume`", accountID)
	case a.Sync.Enabled:
		return fmt.Sprintf("its sync is on; `mailserver account sync %s status` shows how far it got", accountID)
	default:
		return fmt.Sprintf("its sync is off until `mailserver account sync %s on`", accountID)
	}
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
	password := fs.String("password", "", passwordFlagUsage)
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
	// Before anything is sent: a password given as an argument is refused
	// here, and one read from standard input is read once.
	secret, err := passwordArg(ctx, *email, *password)
	if err != nil {
		return err
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
		"provider": *providerName, "password": secret, "imap_host": *imapHost,
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

	if result.Auth == nil {
		// A password mailbox: the daemon logged in with it once before it
		// stored it. Its sync is whatever the answer says, which for an
		// operator mailbox is off until the operator switches it on.
		fmt.Fprintf(os.Stderr, "added %s as %s; %s\n", result.Account.Email, result.Account.ID,
			mailboxSyncNote(result.Account.ID, result.Account))
		return nil
	}
	fmt.Fprintf(os.Stderr, "registered %s as %s; it needs authorising\n", result.Account.Email, result.Account.ID)
	return finishAuthorization(ctx, cfg, result.Account.ID, result.Auth, flow)
}

// passwordFlagUsage is account add's --password help.
const passwordFlagUsage = "only -, which reads the IMAP password (an app-specific one for iCloud) from standard " +
	"input: typed at a prompt that does not show it, or the first line of what is piped in. Never the password " +
	"itself, which sudo, systemd-run and shell history record"

// maxPasswordBytes bounds a password read from standard input: a file piped
// in by mistake is refused rather than sent.
const maxPasswordBytes = 4096

var (
	// errPasswordArgument is a password given on the command line.
	errPasswordArgument = errors.New("account add: --password takes no password on the command line, which sudo, " +
		"systemd-run and shell history record; give --password - and pipe the password on standard input, " +
		"or type it at the prompt; nothing was added")
	// errNoPassword is --password - with nothing to read.
	errNoPassword = errors.New("account add: --password - read no password from standard input; nothing was added")
	// errPasswordTooLong is more than a password on standard input.
	errPasswordTooLong = fmt.Errorf("account add: the password read from standard input is longer than %d bytes; "+
		"nothing was added", maxPasswordBytes)
)

// passwordArg resolves account add's --password: empty is none, for a
// mailbox that signs in with its provider; "-" is the password, read from
// standard input; anything else is refused, before anything is sent, and
// never quoted back. An argument is seen by whatever records the command
// line — sudo's log, systemd-run's journal, shell history, ps — and a
// mailbox's password must reach nothing but the daemon, which seals it.
//
// At a terminal the password is typed without echo; otherwise it is the
// first line of standard input, read a byte at a time so nothing past it is
// consumed. Only the line ending is taken off it (a line feed, and a
// carriage return before one), never a space: a mail server may take a
// password that begins or ends with one, and inner spaces (as Google shows
// an app password) are the password's. A space a pipe added by mistake is
// harmless kept: the daemon logs in once with the password before it stores
// it, so a wrong one is refused, said and stored nowhere, where trimming
// would turn a right one into a wrong one with nothing to say why.
func passwordArg(ctx context.Context, email, value string) (string, error) {
	switch value {
	case "":
		return "", nil
	case "-":
	default:
		return "", errPasswordArgument
	}
	var (
		line []byte
		err  error
	)
	if stdinIsTerminal() {
		fmt.Fprintf(os.Stderr, "Password for %s (not shown): ", email)
		line, err = readHidden(ctx)
		fmt.Fprintln(os.Stderr) // the Return the terminal did not echo
	} else {
		line, err = readPasswordLine(stdin)
	}
	if err != nil {
		return "", fmt.Errorf("account add: reading the password: %w; nothing was added", err)
	}
	if len(line) > maxPasswordBytes {
		return "", errPasswordTooLong
	}
	password := strings.TrimSuffix(string(line), "\r")
	if password == "" {
		return "", errNoPassword
	}
	return password, nil
}

// readPasswordLine reads r up to its first line feed, a byte at a time so
// that nothing past it is consumed, and returns the line without the line
// feed; the end of input ends a line too. It stops one byte past
// maxPasswordBytes, which the caller refuses.
func readPasswordLine(r io.Reader) ([]byte, error) {
	var b [1]byte
	var line []byte
	for len(line) <= maxPasswordBytes {
		n, err := r.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				return line, nil
			}
			line = append(line, b[0])
		}
		if errors.Is(err, io.EOF) {
			return line, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return line, nil
}

// finishAuthorization walks the person through consent, and says where the
// mailbox's sync stands once it is authorised.
func finishAuthorization(ctx context.Context, cfg config.Config, accountID string, flow *authFlowJSON, kind string) error {
	authorised, err := authorize(ctx, cfg, accountID, flow, kind)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "authorised; %s\n", mailboxSyncNote(accountID, authorised))
	return nil
}

// authorize runs one consent flow to its end and returns the mailbox as the
// daemon answered then.
func authorize(ctx context.Context, cfg config.Config, accountID string, flow *authFlowJSON,
	kind string,
) (accountJSON, error) {
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
			return accountJSON{}, fmt.Errorf("account add: reading the pasted address: %w", err)
		}
		// The daemon answers with the mailbox it authorised.
		var authorised accountJSON
		if err := adminCall(ctx, cfg, "/v1/accounts/oauth/callback",
			map[string]any{"redirect_url": strings.TrimSpace(pasted)}, &authorised); err != nil {
			return accountJSON{}, err
		}
		return authorised, nil

	default:
		showConsentPage(ctx, flow)
		// The daemon is listening on the loopback port it advertised; it
		// completes the flow itself, and the account leaves pending_auth.
		return waitForState(ctx, cfg, accountID, "active", 10*time.Minute)
	}
}

// showConsentPage sends the person to the provider's consent page, in a
// browser on this machine where one opens: the redirect comes back to the
// daemon's loopback listener, whose page then says how the attempt ended.
func showConsentPage(ctx context.Context, flow *authFlowJSON) {
	fmt.Fprintln(os.Stderr, "\nOpening a browser to authorise this account. If nothing opens, visit:")
	fmt.Fprintf(os.Stderr, "\n  %s\n\n", flow.AuthURL)
	openBrowser(ctx, flow.AuthURL)
}

// pollInterval is how often the CLI asks whether consent has finished. Two
// seconds is quick enough that nobody waits on it after closing the browser
// tab, and slow enough to be nowhere near any limit.
const pollInterval = 2 * time.Second

// waitForState polls until an account reaches a state, fails, or the wait runs
// out, and returns the account as it was when it reached that state.
func waitForState(ctx context.Context, cfg config.Config, accountID, want string,
	timeout time.Duration,
) (accountJSON, error) {
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
			return accountJSON{}, err
		default:
			var a accountJSON
			if err := json.Unmarshal(raw, &a); err != nil {
				return accountJSON{}, fmt.Errorf("account: unexpected response: %w", err)
			}
			switch a.State {
			case want:
				return a, nil
			case "needs_reauth", "error":
				return accountJSON{}, fmt.Errorf("authorisation failed: %s",
					firstNonEmpty(a.StateReason, a.LastError, "no reason given"))
			}
		}
		if time.Now().After(deadline) {
			return accountJSON{}, fmt.Errorf("timed out waiting for authorisation; "+
				"run `mailserver account authorize %s` to try again", accountID)
		}
		select {
		case <-ctx.Done():
			return accountJSON{}, ctx.Err()
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

	// A mailbox that works stays active through a new consent: the daemon
	// replaces its grant only once a login with the new one worked, and a
	// consent declined, refused or run out leaves it as it was, recording
	// nothing on it. Its state then says nothing of how this attempt ends,
	// and waiting for it to be active would say "authorised" at once. The
	// pasted flow is answered with its own outcome; the others are not, so
	// they are told apart here, before anything starts.
	works := false
	if kind != "pasted" {
		var now accountJSON
		if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/accounts/"+url.PathEscape(accountID), nil, &now); err != nil {
			return err
		}
		works = now.State == "active"
	}
	if works && kind == "device" {
		return fmt.Errorf("account authorize: %s works, and stays as it is whichever way a new consent ends, so "+
			"nothing here could say how a device code ended: authorise it with --paste, which waits for the "+
			"outcome, or in a browser on the daemon's machine, whose page shows it; nothing was started", accountID)
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
	if works {
		showConsentPage(ctx, &flow)
		fmt.Fprintf(os.Stderr, "%s keeps working with the grant it has until a new one logs in: the browser's "+
			"page says how this consent ends, and nothing here waits for it\n", accountID)
		return nil
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
	return printAccounts(accounts)
}

// accountRename names a mailbox, or clears its name with "": the operator's
// own mailboxes, which only it manages. It prints the mailbox as account list
// does.
func accountRename(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("account rename", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%w: account rename takes an account id and its new name (\"\" clears it)", errUsage)
	}
	accountID, name := fs.Arg(0), fs.Arg(1)
	var renamed accountJSON
	if err := adminJSON(ctx, cfg, http.MethodPatch, "/v1/accounts/"+url.PathEscape(accountID),
		map[string]string{"display_name": name}, &renamed); err != nil {
		return err
	}
	if renamed.DisplayName == "" {
		fmt.Fprintf(os.Stderr, "%s has no name now\n", renamed.ID)
	} else {
		fmt.Fprintf(os.Stderr, "%s is named %q now\n", renamed.ID, renamed.DisplayName)
	}
	return printAccounts([]accountJSON{renamed})
}

// printAccounts lists mailboxes as a table, one a line, with a note on each.
func printAccounts(accounts []accountJSON) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "ID\tEMAIL\tPROVIDER\tAUTH\tSTATE\tNOTE")
	for _, a := range accounts {
		note := a.StateReason
		if note == "" {
			note = a.LastError
		}
		if note == "" && a.Sync.Held {
			note = "sync held: turned off until its person turns it on"
		}
		if note == "" && a.Sync.Paused {
			note = "sync paused since " + stamp(a.Sync.PausedSince)
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
// its 30 days. Its sync comes back paused until it is resumed (account sync
// ID resume), or off when the operator had switched it off (account sync ID
// on), as the answer says; a person's or a team's is recovered by its
// owners.
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
	switch {
	case recovered.Sync.Paused:
		fmt.Fprintf(os.Stderr, "recovered %s; its sync is paused until `mailserver account sync %s resume`\n",
			recovered.Email, accountID)
	case recovered.Sync.Held:
		// Never an operator mailbox's, said all the same.
		fmt.Fprintf(os.Stderr, "recovered %s; its sync is held until its person turns it on\n", recovered.Email)
	default:
		fmt.Fprintf(os.Stderr, "recovered %s; its sync is off until `mailserver account sync %s on`\n",
			recovered.Email, accountID)
	}
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
	Paused          bool   `json:"paused"`
	PausedSince     int64  `json:"paused_since"`
}

// accountSync shows an account's sync, asks for a pass, switches an
// operator mailbox's on, pauses or resumes an operator mailbox's sync, its
// index kept, or turns any mailbox's off. A person's mailbox syncs when
// they turn sync on, and a team's when its owners or admins do, and the
// daemon refuses to let anyone else turn it on for them; off is
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
		return fmt.Errorf("%w: account sync ID status|now|on|off|pause|resume", errUsage)
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
	case "pause":
		raw, err = adminDo(ctx, cfg, http.MethodPost, path+"/pause", nil)
	case "resume":
		// An operator mailbox takes no text to agree to.
		raw, err = adminDo(ctx, cfg, http.MethodPost, path+"/resume", nil)
	default:
		return fmt.Errorf("%w: account sync ID status|now|on|off|pause|resume", errUsage)
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
	if rest[1] == "pause" {
		fmt.Printf("sync paused; its index is kept, not updated, until `mailserver account sync %s resume`\n", accountID)
		return nil
	}
	if st.Held {
		// Naming no actor: a hold migration 0025 could not attribute may be
		// an older recovery's.
		fmt.Printf("sync held since %s: turned off until its person turns it on again\n", stamp(st.HeldSince))
		return nil
	}
	if st.Paused {
		fmt.Printf("sync paused since %s; its index is kept, not updated; `mailserver account sync %s resume` resumes it\n",
			stamp(st.PausedSince), accountID)
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

// openBrowser opens a page in this machine's browser: startBrowser, or what
// a test puts in its place, since a test opens no browser.
var openBrowser = startBrowser

// startBrowser is best effort: the URL is printed first, so a failure here
// costs a copy and paste rather than the flow.
func startBrowser(ctx context.Context, target string) {
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
