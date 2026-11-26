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

func userCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: user needs a subcommand: disable or delete", errUsage)
	}
	switch args[0] {
	case "disable":
		return userDisable(ctx, cfg, args[1:])
	case "delete":
		return userDelete(ctx, cfg, args[1:])
	case "invite", "password":
		// Named, since a script may still call them: the server has no
		// people of its own to invite or to reset (cmd/mailserver's doc).
		return fmt.Errorf("user %s: the command line invites nobody and resets no password: "+
			"the operator acts with instance keys, which apikey create makes", args[0])
	default:
		return fmt.Errorf("%w: unknown user subcommand %q", errUsage, args[0])
	}
}

// Closing a person's account, when they ask for it, is two steps, each run
// through the daemon with an admin key, or with --bootstrap while the daemon
// is stopped: `user disable --email ADDRESS` first, which ends every session,
// revokes every API key they created and expires the invites they made, at
// once; and then `user delete --email ADDRESS`, which removes the mailboxes
// of their personal workspace with those mailboxes' credentials and
// everything indexed for them, their sessions, their name on the keys they
// created, their invites, their personal workspace and every local team they
// were alone in, in one transaction. A team mailbox of a team others are in,
// or of a team from another source, is the team's, and stays. Disabling or
// deleting the last active owner needs --force, and so does someone their
// teams depend on — a team's last active owner, the last person who can read
// a team mailbox (or would read a removed one once recovered, until it is
// deleted for good), or the person whose own consent a team mailbox someone
// else reads still syncs under since the upgrade — and the daemon's refusal
// names those teams and mailboxes. The command line makes nobody: the people
// these close are ones a database already holds, one the open console left
// for one, and moving such a database to the operator's mailboxes ends with
// deleting them, members before owners, with --force.

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
// consent to sync was still the person's own: their index went with it, until
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
	email := fs.String("email", "", "the person's address"+emailFlagUsage)
	force := fs.Bool("force", false, "go ahead even if this is the last active owner, or someone teams "+
		"depend on: the last active owner of a team, the last person who can read a team mailbox "+
		"(nobody can read it then), or the person whose own consent a team mailbox still syncs under "+
		"since the upgrade (its sync stops and its index is deleted)")
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

// googleRevokeURL is where a mailbox deleted for good has its Google grant
// revoked: the daemon's endpoint, which a test points at its own.
var googleRevokeURL = account.GoogleRevokeEndpoint

// asOperator runs a use case straight against the database, as the operator.
//
// Whoever can open the database file and hold its lock administers the
// instance already, so the use case runs as an unrestricted instance admin
// key, which no key authenticated: it has no prefix, and what it does is
// recorded as the command line's ("cli"). It is the very method the REST
// route calls, lookup, rules and transaction included: --bootstrap must not
// be a second implementation that could drift from the first.
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

	// A person deleted here takes their mailboxes with them for good, as
	// over REST: their Google grants are revoked as the daemon revokes them.
	registry := account.NewRegistry(ctx, account.NewRepository(db, sealer), account.RegistryOptions{
		AllowPrivate: cfg.AccountAllowPrivate, GoogleRevokeURL: googleRevokeURL, SpoolDir: cfg.SpoolDir(),
		Log: obs.LoggerFrom(ctx),
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
