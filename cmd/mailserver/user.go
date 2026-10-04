package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
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
		return fmt.Errorf("%w: user needs a subcommand: invite, disable or delete", errUsage)
	}
	switch args[0] {
	case "invite":
		return userInvite(ctx, cfg, args[1:])
	case "disable":
		return userDisable(ctx, cfg, args[1:])
	case "delete":
		return userDelete(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown user subcommand %q", errUsage, args[0])
	}
}

func userInvite(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	emailFlag := fs.String("email", "", "the address the invite is for; nobody else can use it"+emailFlagUsage)
	roleFlag := fs.String("role", string(auth.RoleMember), "owner or member (the first person on a daemon is always an owner)")
	bootstrap := fs.Bool("bootstrap", false, "write directly to the database, for the first person, when no daemon is running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	email, err := emailArg("user invite", *emailFlag)
	if err != nil {
		return err
	}
	role, err := auth.ParseRole(*roleFlag)
	if err != nil {
		return fmt.Errorf("user invite: --role: %w", err)
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
		code, invite, err := users.CreateInvite(ctx, auth.NewInvite{
			Email: email, Role: role, CreatedBy: "cli",
		})
		if err != nil {
			return err
		}
		printInvite(auth.InviteLink(base, code, invite.Email), invite.Email, string(invite.Role), invite.ExpiresAt)
		if n, err := users.Count(ctx); err == nil && n == 0 && invite.Role != auth.RoleOwner {
			// The invite says what was asked for; sign-up promotes whoever
			// arrives first, so which invite is used first decides it.
			fmt.Fprintln(os.Stderr, "Nobody has signed up yet: the first person to sign up on this daemon "+
				"becomes an owner, whatever their invite says.")
		}
		return nil
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
// is stopped: `user disable --email ADDRESS` first, which ends every session
// and revokes every key they hold at once, and then `user delete --email
// ADDRESS`, which removes their mailboxes with those mailboxes' credentials
// and everything indexed for them, their sessions, their keys and their
// invites, in one transaction. Disabling or deleting the last active owner
// needs --force: nobody would be left to invite people from the console.

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
	return nil
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
	fmt.Printf("deleted %s (%s): %s, %s, %s, %s\n", out.Email, out.ID,
		plural(out.AccountsRemoved, "mailbox"), plural(out.SessionsDeleted, "session"),
		plural(out.KeysDeleted, "API key"), plural(out.InvitesDeleted, "invite"))
	return nil
}

func parseCloseFlags(name string, args []string) (service.CloseUserRequest, bool, error) {
	fs := flag.NewFlagSet("user "+name, flag.ContinueOnError)
	email := fs.String("email", "", "the address the person signs in with"+emailFlagUsage)
	force := fs.Bool("force", false, "go ahead even if this is the last active owner, "+
		"which leaves nobody to invite people from the console")
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

// asOperator runs a use case straight against the database, as the operator.
//
// Whoever can open the database file and hold its lock administers the
// instance already, so the use case runs as an unrestricted instance admin
// key. It is the very method the REST route calls, lookup, rules and
// transaction included: --bootstrap must not be a second implementation that
// could drift from the first.
func asOperator(ctx context.Context, cfg config.Config, run func(*service.Service, service.Principal) error) error {
	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		return err
	}
	db, release, err := openExclusively(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()

	registry := account.NewRegistry(ctx, account.NewRepository(db, keyring), account.RegistryOptions{
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
	return run(svc, service.Principal{Kind: auth.KindKey, Scope: auth.ScopeAdmin})
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
