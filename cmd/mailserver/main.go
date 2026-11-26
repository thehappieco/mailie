// Command mailserver runs the mail daemon and its administrative commands.
//
// One binary, several subcommands. `serve` is the daemon: it holds the sync
// workers, the OAuth token sources and the loopback listener that completes a
// consent flow, so it is the only process that can add an account and have it
// start syncing. It serves a REST API, and no MCP of its own. The other
// subcommands are therefore thin REST clients of a running daemon — with a
// few exceptions that have to touch the database directly, because they run
// when no daemon is up or no key exists yet: `apikey create --bootstrap`,
// `user disable|delete --bootstrap`, `migrate` and `rewrap-credentials`.
// Those refuse to run while the daemon holds its lock.
//
// By default the server has no people (MAIL_PASSWORD_SIGN_IN): its operator
// acts with the keys `apikey create` makes, and nothing here invites anyone
// or sets a password. People, teams and what they hold on a mailbox exist
// where a database already holds them, one the open console left for one:
// `user`, `workspace`, `member` and `access` list them, rename, close and
// reopen a team, and take people and what they hold away; none of them
// creates or widens anything.
//
// `backup` is the one exception to the lock. It reads the database while the
// daemon runs — that is the point of it — through a read-only connection,
// and takes no lock; `backup restore` runs wherever the backups may be
// decrypted and needs none of the daemon's configuration (docs/backup.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			usage(os.Stderr)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "mailserver: %v\n", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

// errNoMCP answers what was the mcp command. The server serves no MCP of its
// own, so nothing on a client's machine relays to it or configures a client
// for it.
var errNoMCP = errors.New("mcp: this server serves no MCP of its own, so there is nothing to connect a client " +
	"to: its REST API stays, and MCP comes through the hosted service once a server is connected to it")

func run(argv []string) error {
	if len(argv) < 1 {
		return errUsage
	}
	command, args := argv[0], argv[1:]

	switch command {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	case "version", "-v", "--version":
		fmt.Println("mailserver", version)
		return nil
	}

	// Before the .env and the configuration: `mcp connect` and `mcp install`
	// ran on a person's machine, beside an MCP client, with none of the
	// daemon's variables, and a client's configuration there may still
	// launch them.
	if command == "mcp" {
		return errNoMCP
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A .env file is read before the configuration, and never overrides a
	// variable the environment already carries.
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}

	// Before config.Load: a backup needs none of the daemon's secrets, and a
	// restore runs where there are none.
	if command == "backup" {
		return backupCommand(ctx, args)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := commandLogger(command, cfg)
	ctx = obs.WithLogger(ctx, logger)

	switch command {
	case "serve":
		return serve(ctx, cfg, logger, args)
	case "account":
		return accountCommand(ctx, cfg, args)
	case "apikey":
		return apikeyCommand(ctx, cfg, args)
	case "user":
		return userCommand(ctx, cfg, args)
	case "workspace":
		return workspaceCommand(ctx, cfg, args)
	case "member":
		return memberCommand(ctx, cfg, args)
	case "access":
		return accessCommand(ctx, cfg, args)
	case "migrate":
		return migrateCommand(ctx, cfg, args)
	case "rewrap-credentials":
		return rewrapCommand(ctx, cfg, args)
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, command)
	}
}

// commandLogger is the logger a command logs through. The daemon's goes to
// standard output, where its service manager collects it. Every other
// command's standard output is its answer — a key, a table — which a script
// reads, or a person copies whole, so its log goes to standard error:
// a log line among them would be read as part of the answer.
func commandLogger(command string, cfg config.Config) *slog.Logger {
	if command == "serve" {
		return obs.NewLogger(cfg.Log.Level, cfg.Log.Format)
	}
	return obs.NewLoggerTo(os.Stderr, cfg.Log.Level, cfg.Log.Format)
}

func usage(w *os.File) {
	// Writing usage to a closed stream is not something to report on the
	// way out.
	//nolint:errcheck // usage text
	fmt.Fprint(w, `mailserver — a REST API over several mailboxes

Usage:
  mailserver serve                        Run the daemon (REST and SSE)
  mailserver account add --email ADDR [--password -]
                                          Register a mailbox and authorise it with its provider, or, with
                                          --password -, sign in to it with the password read from standard
                                          input (typed at a prompt that does not show it, or piped)
  mailserver account list                 List accounts and their state
  mailserver account rename ID NAME       Name a mailbox of the operator workspace ("" clears it)
  mailserver account folders ID           List an account's folders
  mailserver account authorize ID         Authorise again after a grant expires
  mailserver account sync ID status|now   Show an account's sync, or ask for a pass
  mailserver account sync ID on           Switch sync on for a mailbox of the operator workspace
  mailserver account sync ID off [--yes]  Switch any workspace's mailbox's sync off, removed ones too, and
                                          delete its index; its readers are told, and its team's owners
                                          and admins, or its person, turn it on again
  mailserver account sync ID pause|resume Pause or resume a mailbox of the operator workspace's sync: its
                                          index is kept, not updated, while paused
  mailserver account remove ID [--yes]    Remove a mailbox: kept 30 days, used by nothing and recoverable,
                                          then deleted for good; one that never connected goes at once
  mailserver account removed [--workspace ID]
                                          List a workspace's removed mailboxes (the operator's by default)
  mailserver account recover ID           Recover a removed mailbox of the operator workspace, its sync
                                          paused until account sync ID resume (off, if it was off, until
                                          account sync ID on)
  mailserver account purge ID [--yes]     Delete a removed mailbox for good, any workspace's
  mailserver apikey create --scope SCOPE --name NAME
                                          Issue an instance key (the daemon needs MAIL_ADMIN_API=true)
  mailserver apikey list                  List every key, with its workspace, revoked ones too (the same)
  mailserver apikey revoke PREFIX         Revoke any key (the same)
  mailserver apikey hold SELECTOR         Hold a key an extension's issuer made, refused here until released
  mailserver apikey release SELECTOR      Lift that hold, and nothing else
  mailserver user disable --email ADDR [--force] [--bootstrap]
                                          End a person's sessions and revoke the keys they created
  mailserver user delete --email ADDR [--force] [--bootstrap]
                                          Delete a person, the mailboxes they linked, sessions, keys and invites
  mailserver workspace list               List every workspace: personal, teams and the operator's
  mailserver workspace rename --workspace ID --name NAME
                                          Rename a team
  mailserver workspace close --workspace ID
                                          Close a team: its mailboxes reach nobody and sync nothing, kept,
                                          until it reopens
  mailserver workspace reopen --workspace ID
                                          Reopen a closed team as it was
  mailserver member list --workspace ID   List a workspace's members
  mailserver member remove --workspace ID --email ADDR
                                          Remove a member from a team, with their access there
  mailserver access list --workspace ID   List a workspace's mailboxes and who holds what on each
  mailserver access revoke --account ID --email ADDR [--read] [--act] [--send] [--manage]
                                          Take flags away from a member's grant; none named: every flag
  mailserver migrate [--dry-run]          Apply pending schema migrations
  mailserver rewrap-credentials           Re-seal the credentials, the send-hash root and the salt key under
                                          the active key (the KMS key, when MAIL_CREDENTIAL_KMS_KEY_ARN names one)
  mailserver rewrap-credentials [--new-send-hash-root] [--new-salt-key] [--kms-key-lost]
                                          Replace a send-hash root or a salt key no configured key opens (its key
                                          is lost); --kms-key-lost confirms the KMS key that sealed it is lost
  mailserver backup                       Encrypt a snapshot of the database and upload it to S3
  mailserver backup restore --object s3://BUCKET/KEY --kms-key-arn ARN --out PATH
                                          Download, verify and decrypt a backup (the decrypting AWS principal)
  mailserver backup restore --file FILE.mlbk --kms-key-arn ARN --out PATH
                                          The same, from a backup already downloaded
  mailserver version                      Print the version

Configuration comes from the environment, optionally seeded by a .env file in
the working directory. MAIL_CREDENTIAL_KEY_HEX is required, unless
MAIL_CREDENTIAL_KMS_KEY_ARN names an AWS KMS key to seal with instead: one or
the other encrypts every stored refresh token and IMAP password. Generate the
hex key with:

  openssl rand -hex 32

By default the server has no people: its operator acts with instance keys,
and this command line invites nobody and sets no password. The user,
workspace, member and access commands are for a database that already holds
people and teams, one an earlier release left: they list them, rename, close
and reopen a team, and take people and what they hold away; they create and
widen nothing.

The administrative subcommands talk to a running daemon over its REST API using
MAIL_ADMIN_KEY; apikey create, list and revoke use its /v1/apikeys routes, which
it mounts only with MAIL_ADMIN_API=true (apikey hold and release do not need
them). Before any key exists, or with those routes off, issue a key with the
daemon stopped:

  mailserver apikey create --bootstrap --scope admin --name cli

user disable and user delete also run with the daemon stopped, with
--bootstrap, which opens the database directly.

backup reads MAIL_ENV (the encryption context's env, dev when unset), and
MAIL_BACKUP_BUCKET, MAIL_BACKUP_KMS_KEY_ARN and MAIL_BACKUP_REGION, all three
required (there is no default region), and AWS credentials only from the SDK's
default chain. backup restore needs no MAIL_* variable: --kms-key-arn names the
key, and the restore runs in its region. See docs/backup.md.
`)
}
