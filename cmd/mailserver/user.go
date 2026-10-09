package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/app"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// stdin is where --email - reads the address from. A variable so a test can
// hand it one.
var stdin io.Reader = os.Stdin

// emailFlagUsage is the --email help for the commands that take a person's
// address.
const emailFlagUsage = "; - reads it from standard input instead, which keeps it off the command line " +
	"(sudo, systemd-run and shell history record that)"

// emailArg resolves --email. "-" is one line read from standard input: on a
// server, running this binary against the daemon's data directory usually
// means sudo, and often systemd-run as the daemon's user, and each of them
// logs the whole command line, so an address passed as an argument ends up
// unmasked in the host's journal — the very place a request to delete that
// person's data should not leave it.
func emailArg(command, value string) (string, error) {
	if value != "-" {
		if value == "" {
			return "", fmt.Errorf("%s: --email is required", command)
		}
		return value, nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s: reading the address from standard input: %w", command, err)
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", fmt.Errorf("%s: --email - read no address from standard input", command)
	}
	return line, nil
}

// devConsoleURL is where `npm run dev` serves the console. A bootstrap invite
// needs some origin to put in its link, and on a machine with no
// MAIL_PUBLIC_URL yet this is the one that will be open.
const devConsoleURL = "http://localhost:5174"

func userCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: user needs a subcommand: invite, disable, delete or password", errUsage)
	}
	switch args[0] {
	case "invite":
		return userInvite(ctx, cfg, args[1:])
	case "disable":
		return userDisable(ctx, cfg, args[1:])
	case "delete":
		return userDelete(ctx, cfg, args[1:])
	case "password":
		return userPassword(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown user subcommand %q", errUsage, args[0])
	}
}

func userInvite(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	emailFlag := fs.String("email", "", "the address the invite is for; nobody else can use it"+emailFlagUsage)
	roleFlag := fs.String("role", "",
		"without --workspace, owner or member, the person's role on this server: member by default, and with "+
			"--bootstrap owner while the server has no active owner and no owner invite waiting; "+
			"with --workspace, owner, admin or member, the role in that team (member by default)")
	workspaceFlag := fs.String("workspace", "", "a team's id: invite into that team (an existing person accepts "+
		"it signed in; anyone else signs up with it)")
	bootstrap := fs.Bool("bootstrap", false, "write directly to the database, for the first person, when no daemon is running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("user invite: unexpected argument %q", fs.Arg(0))
	}
	email, err := emailArg("user invite", *emailFlag)
	if err != nil {
		return err
	}
	team := strings.TrimSpace(*workspaceFlag)
	var (
		role     auth.Role
		teamRole workspace.Role
	)
	switch {
	case team != "":
		teamRole = workspace.RoleMember
		if *roleFlag != "" {
			if teamRole, err = workspace.ParseRole(*roleFlag); err != nil {
				return fmt.Errorf("user invite: --role: %w", err)
			}
		}
	case *roleFlag != "":
		if role, err = auth.ParseRole(*roleFlag); err != nil {
			return fmt.Errorf("user invite: --role: %w", err)
		}
	}

	if *bootstrap {
		db, release, err := openExclusively(ctx, cfg)
		if err != nil {
			return err
		}
		defer release()

		base := cfg.PublicURL
		if base == "" {
			base = devConsoleURL
			fmt.Fprintf(os.Stderr, "MAIL_PUBLIC_URL is not set; the link points at %s, "+
				"where `npm run dev` serves the console.\n", devConsoleURL)
		}

		users := auth.NewUsers(db)
		if team == "" && role == "" {
			// The invite decides the role, at creation; signing up never
			// does. The first person invited from here, with no owner yet
			// and none on the way, is the one who administers the server.
			first, err := users.NeedsFirstOwner(ctx)
			if err != nil {
				return err
			}
			role = auth.RoleMember
			if first {
				role = auth.RoleOwner
			}
		}
		code, invite, err := users.CreateInvite(ctx, auth.NewInvite{
			Email: email, Role: role, WorkspaceID: team, WorkspaceRole: teamRole, CreatedBy: "cli",
		})
		if err != nil {
			return err
		}
		shown := string(invite.Role)
		if team != "" {
			shown = string(invite.WorkspaceRole) + " of the team " + team
		}
		printInvite(auth.InviteLink(base, code, invite.Email), invite.Email, shown, invite.ExpiresAt)
		return nil
	}

	if team != "" {
		var created service.TeamInvite
		if err := adminCall(ctx, cfg, "/v1/workspaces/"+url.PathEscape(team)+"/invites",
			map[string]any{"email": email, "role": string(teamRole)}, &created); err != nil {
			return err
		}
		printInvite(created.URL, created.Email, created.Role+" of the team "+created.WorkspaceID,
			time.Unix(created.ExpiresAt, 0))
		return nil
	}
	if role == "" {
		role = auth.RoleMember
	}
	body, err := adminPost(ctx, cfg, "/v1/users/invites", map[string]any{"email": email, "role": string(role)})
	if err != nil {
		return err
	}
	var created struct {
		Email     string `json:"email"`
		Role      string `json:"role"`
		URL       string `json:"url"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return fmt.Errorf("user invite: unexpected response: %w", err)
	}
	printInvite(created.URL, created.Email, created.Role, time.Unix(created.ExpiresAt, 0))
	return nil
}

func printInvite(link, email, role string, expires time.Time) {
	fmt.Println(link)
	fmt.Fprintf(os.Stderr, "\ninvite for %s as %s, expires %s\n", email, role, expires.Local().Format(time.RFC3339))
	// The code is in the fragment, which browsers never send to a server; the
	// link itself is still a credential until it is used.
	fmt.Fprintln(os.Stderr, "Send the link to that person only. It works once, for that address.")
}

// Closing a person's account, when they ask for it, is two steps, each run
// through the daemon with an admin key, or with --bootstrap while the daemon
// is stopped: `user disable --email ADDRESS` first, which ends every session,
// revokes every API key they created and expires the invites they made, at
// once; and then `user delete --email ADDRESS`, which removes the mailboxes
// of their personal workspace with those mailboxes' credentials and
// everything indexed for them, their sessions, their name on the keys they
// created, their invites, their personal workspace and every team they were
// alone in, in one transaction. A team
// mailbox of a team others are in is the team's, and stays. Disabling or
// deleting the last active owner needs --force: nobody would be left to
// invite people from the console. So does someone their teams depend on — a
// team's last active owner, the last person who can read a team mailbox, or
// the person whose own consent a team mailbox someone else reads still syncs
// under since the upgrade — and the daemon's refusal names those teams and
// mailboxes.

func userDisable(ctx context.Context, cfg config.Config, args []string) error {
	req, bootstrap, err := parseCloseFlags("disable", args)
	if err != nil {
		return err
	}
	var out service.DisabledUser
	if bootstrap {
		err = asOperator(ctx, cfg, func(svc *service.Service, operator service.Principal) error {
			var err error
			out, err = svc.DisableUser(ctx, operator, req)
			return err
		})
	} else {
		err = adminCall(ctx, cfg, "/v1/users/disable", closeBody(req), &out)
	}
	if err != nil {
		return err
	}
	fmt.Printf("disabled %s (%s): %s ended, %s revoked\n",
		out.Email, out.ID, plural(out.SessionsEnded, "session"), plural(out.KeysRevoked, "API key"))
	printStoppedTeamSyncs(out.TeamSyncsStopped)
	return nil
}

// printStoppedTeamSyncs names the team mailboxes a closure stopped, whose
// consent to sync was still the person's own: their index went with it, and
// an owner or an admin of each team turns it on again.
func printStoppedTeamSyncs(ids []string) {
	if len(ids) > 0 {
		fmt.Printf("stopped the sync of team mailboxes that synced under their consent, and deleted their index: %s\n",
			strings.Join(ids, ", "))
	}
}

func userDelete(ctx context.Context, cfg config.Config, args []string) error {
	req, bootstrap, err := parseCloseFlags("delete", args)
	if err != nil {
		return err
	}
	var out service.DeletedUser
	if bootstrap {
		err = asOperator(ctx, cfg, func(svc *service.Service, operator service.Principal) error {
			var err error
			out, err = svc.DeleteUser(ctx, operator, req)
			return err
		})
	} else {
		err = adminCall(ctx, cfg, "/v1/users/delete", closeBody(req), &out)
	}
	if err != nil {
		return err
	}
	if out.ID == "" {
		fmt.Printf("%s had no account; deleted %s\n", out.Email, plural(out.InvitesDeleted, "invite"))
		return nil
	}
	fmt.Printf("deleted %s (%s): %s, %s, %s, %s, %s\n", out.Email, out.ID,
		plural(out.AccountsRemoved, "mailbox"), plural(out.SessionsDeleted, "session"),
		plural(out.KeysDeleted, "API key"), plural(out.InvitesDeleted, "invite"), plural(out.TeamsDeleted, "team"))
	printStoppedTeamSyncs(out.TeamSyncsStopped)
	return nil
}

func parseCloseFlags(name string, args []string) (service.CloseUserRequest, bool, error) {
	fs := flag.NewFlagSet("user "+name, flag.ContinueOnError)
	email := fs.String("email", "", "the address the person signs in with"+emailFlagUsage)
	force := fs.Bool("force", false, "go ahead even if this is the last active owner, "+
		"which leaves nobody to invite people from the console, or someone teams depend on: "+
		"the last active owner of a team (give it another with `member role`), the last person "+
		"who can read a team mailbox (nobody can read it then, and its owners and admins remove it), "+
		"or the person whose own consent a team mailbox still syncs under since the upgrade "+
		"(its sync stops and its index is deleted until the team turns it on again)")
	bootstrap := fs.Bool("bootstrap", false, "write directly to the database, when no daemon is running")
	if err := fs.Parse(args); err != nil {
		return service.CloseUserRequest{}, false, err
	}
	if fs.NArg() > 0 {
		return service.CloseUserRequest{}, false, fmt.Errorf("user %s: unexpected argument %q", name, fs.Arg(0))
	}
	address, err := emailArg("user "+name, *email)
	if err != nil {
		return service.CloseUserRequest{}, false, err
	}
	return service.CloseUserRequest{Email: address, Force: *force}, *bootstrap, nil
}

// closeBody is the request as the daemon's route reads it, built as the other
// commands build theirs. The address is in the body, never in the URL.
func closeBody(req service.CloseUserRequest) map[string]any {
	body := map[string]any{"email": req.Email}
	if req.Force {
		body["force"] = true
	}
	return body
}

// A person who lost both their password and their recovery code gets a reset
// invitation from the operator, with the daemon stopped: `user password
// --bootstrap --email ADDRESS` (docs/key-scheme.md section 12.6). Nothing
// that reaches the daemon over the network makes one, by design, so
// --bootstrap is not optional here; whoever can open the database file and
// hold its lock administers the instance already.
//
// The command sets no password: the server never knows one. It prints a
// single-use link, valid for seven days, with the code in its fragment, which
// the person opens to choose a new password; their browser makes a new
// account key and recovery code, and in one transaction the server replaces
// their public key, deletes every grant sealed to the old one and ends their
// sessions. It is refused, naming the mailboxes, while the person is the last
// reader of a team mailbox, unless --force, which the invitation records.

func userPassword(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("user password", flag.ContinueOnError)
	emailFlag := fs.String("email", "", "the address the person signs in with"+emailFlagUsage)
	force := fs.Bool("force", false, "go ahead even if the person is the last who can read a team mailbox: "+
		"the reset takes read from them on every mailbox that has a key, and nobody can be given it again "+
		"on such a team mailbox (give another member read first, if the old key is not truly lost)")
	bootstrap := fs.Bool("bootstrap", false, "write directly to the database, when no daemon is running "+
		"(required: no route makes a reset invitation)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// Not quoted back: a stray argument here may well be a password.
		return errors.New("user password: unexpected argument; no password is ever given here: " +
			"the command prints a reset link, and the person chooses a new password with it")
	}
	if !*bootstrap {
		return errors.New("user password: needs --bootstrap, with the daemon stopped: " +
			"no route makes a reset invitation, by design")
	}
	// The lock first, and then who it is for.
	db, release, err := openExclusively(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()

	address, err := emailArg("user password", *emailFlag)
	if err != nil {
		return err
	}
	address, err = auth.NormalizeEmail(address)
	if err != nil {
		return fmt.Errorf("user password: %w", err)
	}
	users := auth.NewUsers(db)
	user, err := users.GetByEmail(ctx, address)
	if errors.Is(err, auth.ErrUserNotFound) {
		return errors.New("user password: no person has that address; nothing was made")
	}
	if err != nil {
		return err
	}
	base := cfg.PublicURL
	if base == "" {
		base = devConsoleURL
		fmt.Fprintf(os.Stderr, "MAIL_PUBLIC_URL is not set; the link points at %s, "+
			"where `npm run dev` serves the console.\n", devConsoleURL)
	}
	code, reset, err := users.CreateReset(ctx, user.ID, *force, "cli")
	var blocked *auth.BlockedError
	if errors.As(err, &blocked) {
		return fmt.Errorf("user password: %s is the last person who can read the team mailboxes %s: "+
			"a reset takes read from them; have another member given read first, or pass --force; "+
			"nothing was made", user.Email, strings.Join(blocked.LastReaderOf, ", "))
	}
	if err != nil {
		return fmt.Errorf("user password: %w; nothing was made", err)
	}
	obs.LoggerFrom(ctx).Info("reset invitation made by the operator",
		"user", user.ID, "forced", reset.Forced, "disabled", user.Disabled)
	fmt.Println(auth.ResetLink(base, code, reset.Email))
	fmt.Fprintf(os.Stderr, "\nreset invitation for %s (%s), expires %s\n", reset.Email, user.ID,
		reset.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintln(os.Stderr, "Send the link to that person only. It works once, for that address, and replaces "+
		"any earlier one. With it they choose a new password and get a new recovery code and account key; "+
		"their sessions end, and their personal mailboxes need a new key before they open again.")
	if user.Disabled {
		fmt.Fprintln(os.Stderr, "This person is disabled: the link works only once they are enabled again.")
	}
	return nil
}

// stdinIsTerminal and readHidden stand for the terminal, which `go test`
// never has; variables so that a test can be one. `mcp install` reads a key
// with them.
var (
	stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readHidden      = readHiddenLine
)

// hiddenTerminal is a terminal a secret is typed at. hide switches its echo
// off and returns what switches it back on; readLine reads one line and never
// touches the terminal's settings.
type hiddenTerminal interface {
	hide() (show func(), err error)
	readLine() ([]byte, error)
}

// errEntryStopped is a hidden entry ended by quit or hangup.
var errEntryStopped = errors.New("stopped by a signal")

// readHiddenLine reads one line from the terminal on standard input without
// echoing it.
func readHiddenLine(ctx context.Context) ([]byte, error) {
	return readHiddenFrom(ctx, newStdinTerminal())
}

// readHiddenFrom reads one line from t without echoing it, and leaves the
// terminal echoing again whichever way it ends: Return, an interrupt or a
// termination (ctx), or quit or hangup.
//
// Echo goes off here, before anything waits, and the deferred call puts it
// back, last. The read runs in a goroutine, because it is the one step a
// signal does not end (the terminal still waits for Return), and it never
// touches the terminal's settings: a process that gives up leaves it blocked
// and exits, and nothing switches echo off again after it was put back. Quit
// (Ctrl-\, which the terminal still sends) and hangup would otherwise end the
// process at once, past every deferred call, so they are listened for while
// the entry is open.
func readHiddenFrom(ctx context.Context, t hiddenTerminal) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var listen []os.Signal
	for _, sig := range entrySignals {
		// An ignored signal ends nothing, and listening would stop
		// ignoring it (nohup's hangup).
		if !signal.Ignored(sig) {
			listen = append(listen, sig)
		}
	}
	stopped := make(chan os.Signal, 1)
	if len(listen) > 0 { // none would mean every signal
		signal.Notify(stopped, listen...)
		defer signal.Stop(stopped) // after show: echo is back before quit can kill again
	}
	show, err := t.hide()
	if err != nil {
		return nil, err
	}
	defer show()

	type read struct {
		line []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := t.readLine()
		done <- read{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case sig := <-stopped:
		return nil, fmt.Errorf("%w (%s); nothing was changed", errEntryStopped, sig)
	}
}

// readTerminalLine reads one line of r a byte at a time, so that nothing past
// it is consumed, as x/term's ReadPassword does: a backspace that reaches it
// removes the byte before, a carriage return is dropped, and the end of input
// ends a line that has something in it.
func readTerminalLine(r io.Reader) ([]byte, error) {
	var b [1]byte
	var line []byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			switch b[0] {
			case '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			case '\n':
				return line, nil
			case '\r':
			default:
				line = append(line, b[0])
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
	}
}

// asOperator runs a use case straight against the database, as the operator.
//
// Whoever can open the database file and hold its lock administers the
// instance already, so the use case runs as an unrestricted instance admin
// key. It is the very method the REST route calls, lookup, rules and
// transaction included: --bootstrap must not be a second implementation that
// could drift from the first.
func asOperator(ctx context.Context, cfg config.Config, run func(*service.Service, service.Principal) error) error {
	sealer, err := app.NewSealer(ctx, cfg)
	if err != nil {
		return err
	}
	db, release, err := openExclusively(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()

	registry := account.NewRegistry(ctx, account.NewRepository(db, sealer), account.RegistryOptions{
		SpoolDir: cfg.SpoolDir(),
	})
	//nolint:contextcheck // shutdown makes its own bounded context
	defer func() {
		//nolint:errcheck // nothing was started that could fail to stop
		_ = registry.Close()
	}()
	svc := service.New(service.Deps{
		Accounts: registry, Keys: auth.NewKeys(db), Users: auth.NewUsers(db), Store: db,
		Log: obs.LoggerFrom(ctx), PublicURL: cfg.PublicURL, ConsentVersions: cfg.Consent,
	})
	return run(svc, service.Principal{Kind: auth.KindKey, Scope: auth.ScopeAdmin, WorkspaceID: workspace.OperatorID})
}

// adminCall posts a request to the daemon and decodes its answer into out.
func adminCall(ctx context.Context, cfg config.Config, path string, body, out any) error {
	payload, err := adminPost(ctx, cfg, path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s: unexpected response: %w", path, err)
	}
	return nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if noun == "mailbox" {
		return fmt.Sprintf("%d mailboxes", n)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
