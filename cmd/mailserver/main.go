// Command mailserver runs the mail daemon and its administrative commands.
//
// One binary, several subcommands. `serve` is the daemon: it holds the sync
// workers, the OAuth token sources and the loopback listener that completes a
// consent flow, so it is the only process that can add an account and have it
// start syncing. The other subcommands are therefore thin REST clients of a
// running daemon — with a few exceptions that have to touch the database
// directly, because they run when no daemon is up or no key exists yet:
// `apikey create --bootstrap`, `user invite|disable|delete --bootstrap`,
// `user password --bootstrap` (which has no REST form at all), `migrate` and
// `rewrap-credentials`. Those refuse to run while the daemon holds its lock.
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

	// A .env file is read before the configuration, and never overrides a
	// variable the environment already carries.
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before config.Load: a backup needs none of the daemon's secrets, and a
	// restore runs where there are none.
	if command == "backup" {
		return backupCommand(ctx, args)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := obs.NewLogger(cfg.Log.Level, cfg.Log.Format)
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
	case "migrate":
		return migrateCommand(ctx, cfg, args)
	case "rewrap-credentials":
		return rewrapCommand(ctx, cfg, args)
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, command)
	}
}

func usage(w *os.File) {
	// Writing usage to a closed stream is not something to report on the
	// way out.
	//nolint:errcheck // usage text
	fmt.Fprint(w, `mailserver — REST and MCP access to several mailboxes

Usage:
  mailserver serve [--mcp-stdio]          Run the daemon (REST, SSE and MCP)
  mailserver account add --email ADDR      Register a mailbox and authorise it
  mailserver account list                 List accounts and their state
  mailserver account folders ID           List an account's folders
  mailserver account authorize ID         Authorise again after a grant expires
  mailserver account sync ID status|now   Show an account's sync, or ask for a pass
  mailserver account sync ID on|off       Switch sync for an account nobody owns
  mailserver account remove ID            Forget an account and its index
  mailserver apikey create --scope SCOPE --name NAME
                                          Issue an instance key (the daemon needs MAIL_ADMIN_API=true)
  mailserver apikey list                  List instance keys, including revoked ones (the same)
  mailserver apikey revoke PREFIX         Revoke an instance key (the same)
  mailserver user invite --email ADDR     Invite a person to the web console
  mailserver user disable --email ADDR    End a person's sessions and revoke their keys
  mailserver user delete --email ADDR     Delete a person, their mailboxes, sessions, keys and invite
  mailserver user password --bootstrap --email ADDR
                                          Set a forgotten console password and end every session
                                          (daemon stopped; typed twice at a terminal, or one line piped)
  mailserver migrate [--dry-run]          Apply pending schema migrations
  mailserver rewrap-credentials           Re-encrypt credentials under the active key
  mailserver backup                       Encrypt a snapshot of the database and upload it to S3
  mailserver backup restore --object s3://BUCKET/KEY --kms-key-arn ARN --out PATH
                                          Download, verify and decrypt a backup (the decrypting AWS principal)
  mailserver backup restore --file FILE.mlbk --kms-key-arn ARN --out PATH
                                          The same, from a backup already downloaded
  mailserver version                      Print the version

Configuration comes from the environment, optionally seeded by a .env file in
the working directory. MAIL_CREDENTIAL_KEY_HEX is required: it encrypts every
stored refresh token and IMAP password. Generate one with:

  openssl rand -hex 32

serve answers MCP over HTTP at /mcp unless MAIL_MCP_HTTP=false; --mcp-stdio
speaks it on standard input and output either way.

The administrative subcommands talk to a running daemon over its REST API using
MAIL_ADMIN_KEY; apikey create, list and revoke use its /v1/apikeys routes, which
it mounts only with MAIL_ADMIN_API=true. Before any key exists, or with those
routes off, issue a key with the daemon stopped:

  mailserver apikey create --bootstrap --scope admin --name cli

and, for the web console, invite its first person (always an owner) with:

  mailserver user invite --bootstrap --email you@example.com

A forgotten console password is set again only that way, with the daemon
stopped, since no route sets a password; it ends every session the person has:

  mailserver user password --bootstrap --email you@example.com

backup reads MAIL_ENV (the encryption context's env, dev when unset), and
MAIL_BACKUP_BUCKET, MAIL_BACKUP_KMS_KEY_ARN and MAIL_BACKUP_REGION, all three
required (there is no default region), and AWS credentials only from the SDK's
default chain. backup restore needs no MAIL_* variable: --kms-key-arn names the
key, and the restore runs in its region. See docs/backup.md.
`)
}
