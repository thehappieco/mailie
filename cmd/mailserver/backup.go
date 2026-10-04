package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thehappieco/mailie/internal/backup"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
)

// backupClients is KMS and S3, as the backup commands use them.
type backupClients interface {
	backup.KMS
	backup.Uploader
	backup.Downloader
}

// newBackupClients builds the AWS clients from the SDK's default credential
// chain. A variable so a test can hand the commands fakes.
var newBackupClients = func(ctx context.Context, region string) (backupClients, error) {
	return backup.NewAWS(ctx, region)
}

// backupCommand runs before config.Load, on a configuration of its own: the
// backup on the host needs none of the daemon's secrets, and a restore,
// wherever it runs, has none to give it.
func backupCommand(ctx context.Context, args []string) error {
	if len(args) > 0 {
		if args[0] == "restore" {
			return backupRestore(ctx, args[1:])
		}
		return fmt.Errorf("%w: unknown backup subcommand %q", errUsage, args[0])
	}
	cfg, err := config.LoadBackup()
	if err != nil {
		return err
	}
	logger := obs.NewLogger(cfg.Log.Level, cfg.Log.Format)
	ctx = obs.WithLogger(ctx, logger)

	clients, err := newBackupClients(ctx, cfg.Region)
	if err != nil {
		return err
	}
	// No lock, unlike every other command that opens the database: the
	// backup exists to run beside the daemon, and opens it read-only.
	_, err = backup.Run(ctx, backup.Options{
		DatabasePath: cfg.DatabasePath(),
		Env:          string(cfg.Env),
		Bucket:       cfg.Bucket,
		KMSKeyARN:    cfg.KMSKeyARN,
		KMS:          clients,
		Uploader:     clients,
		Logger:       logger,
	})
	return err
}

func backupRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	object := fs.String("object", "", "the backup in S3, as s3://BUCKET/db/NAME.mlbk")
	file := fs.String("file", "", "a backup already downloaded, instead of --object")
	out := fs.String("out", "", "the database file to create; it must not exist")
	keyARN := fs.String("kms-key-arn", "", "the ARN of the KMS key the backup must be under; "+
		"required unless MAIL_BACKUP_KMS_KEY_ARN is set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("%w: backup restore takes no arguments, got %q", errUsage, fs.Args())
	case (*object == "") == (*file == ""):
		return errors.New("backup restore: give exactly one of --object and --file")
	case *out == "":
		return errors.New("backup restore: --out is required")
	}
	bucket, key := "", ""
	if *object != "" {
		var err error
		if bucket, key, err = parseS3URL(*object); err != nil {
			return err
		}
	}

	cfg, err := config.LoadRestore()
	if err != nil {
		return err
	}
	// The key a backup names is in its header, which nothing authenticates
	// before KMS unwraps the data key: taking its word would accept a backup
	// sealed under any key the restoring credentials can use, in any account.
	pin := cfg.KMSKeyARN
	if *keyARN != "" {
		if _, err := config.KMSKeyRegion(*keyARN); err != nil {
			return fmt.Errorf("backup restore: --kms-key-arn: %w", err)
		}
		pin = *keyARN
	}
	if pin == "" {
		return errors.New("backup restore: --kms-key-arn is required: the ARN of the KMS key the backups are " +
			"encrypted under, never an alias (aws kms describe-key --key-id ALIAS shows the ARN an alias points at)")
	}
	// A KMS key answers only in its own region, and the restore talks to KMS
	// and S3 in one: the key's, which MAIL_BACKUP_REGION may only confirm.
	region, err := config.KMSKeyRegion(pin)
	if err != nil {
		return fmt.Errorf("backup restore: %w", err)
	}
	if cfg.Region != "" && region != cfg.Region {
		return fmt.Errorf("backup restore: --kms-key-arn: the key is in %s, but the restore runs in %s (MAIL_BACKUP_REGION)",
			region, cfg.Region)
	}
	clients, err := newBackupClients(ctx, region)
	if err != nil {
		return err
	}

	var src io.ReadCloser
	if *object != "" {
		if src, err = clients.GetObject(ctx, bucket, key); err != nil {
			return fmt.Errorf("backup restore: download %s: %w", *object, err)
		}
	} else if src, err = os.Open(*file); err != nil {
		return fmt.Errorf("backup restore: %w", err)
	}
	//nolint:errcheck // read-only; the restore's own error is the one to report
	defer func() { _ = src.Close() }()

	res, err := backup.Restore(ctx, backup.RestoreOptions{
		Source: src, Object: key, KMSKeyARN: pin, KMS: clients, Out: *out,
	})
	if err != nil {
		return err
	}
	counts := make([]string, 0, len(res.Rows))
	for _, table := range []string{"users", "accounts", "messages"} {
		if n, ok := res.Rows[table]; ok {
			counts = append(counts, fmt.Sprintf("%s %d", table, n))
		}
	}
	fmt.Printf("restored %s (env %s) to %s\n  %d bytes, schema version %d, %s\n", res.Object, res.Env, *out,
		res.PlaintextBytes, res.SchemaVersion, strings.Join(counts, ", "))
	fmt.Println("The mailbox credentials inside are still sealed with the MAIL_CREDENTIAL_KEY_HEX of the server it came from.")
	fmt.Println("It is the database in clear: keep it only where your data-protection rules allow, " +
		"and delete it as soon as it has served.")
	fmt.Println(restoredLacks)
	return nil
}

// restoredLacks is the last line a restore prints. It says what docs/backup.md
// says, and points at the section that says it: a restored database is the
// one in its backup, and nothing changed since, deletions included, is
// re-applied to it.
const restoredLacks = "It is the database as of the time in its name or a moment later: every change made after that, " +
	"deletions included, is missing from it and is not re-applied (docs/backup.md, \"What a restore lacks\")."

// parseS3URL splits s3://BUCKET/KEY.
func parseS3URL(v string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(v, "s3://")
	if ok {
		bucket, key, ok = strings.Cut(rest, "/")
	}
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("backup restore: --object: want s3://BUCKET/KEY, got %q", v)
	}
	return bucket, key, nil
}
