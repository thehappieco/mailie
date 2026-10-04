package config

import (
	"errors"
	"fmt"
	"regexp"
)

// Backup is what `mailserver backup` and `mailserver backup restore` read.
//
// It is loaded on its own rather than as part of Config. The backup runs on
// the host beside the daemon and needs none of the daemon's secrets, so it
// must not fail because an OAuth client is half configured; a restore runs
// wherever the backups may be decrypted, where there is no credential key and
// no data directory at all. AWS credentials are never configuration here,
// under any name: the SDK's default chain finds the host's own identity (an
// instance role, say) or the restoring person's sign-in, and nothing else.
type Backup struct {
	// Env is MAIL_ENV, as for the daemon: dev when unset. A backup writes it
	// into its data key's encryption context, where a key policy can tell a
	// production backup from any other.
	Env Env
	// DataDir holds the live database the backup reads. Unused by a restore.
	DataDir string
	// Bucket receives the encrypted backups, under db/.
	Bucket string
	// KMSKeyARN is the key every backup's data key is generated under.
	// Always the key's ARN: an alias can be pointed at another key by
	// whoever may update aliases. For a restore it is the key the backup must
	// name; without it the restore needs --kms-key-arn.
	KMSKeyARN string
	// Region is where the bucket and the key live, MAIL_BACKUP_REGION. A
	// backup requires it: there is no default, because where the copies of
	// the database are kept is the operator's decision. A restore may leave
	// it empty and runs in the region of the key it is told to trust.
	Region string
	Log    Log
}

var (
	// kmsKeyARNRE is a key ARN — never an alias ARN — with its region and
	// account. Multi-Region keys carry an mrk- id.
	kmsKeyARNRE = regexp.MustCompile(`^arn:aws(?:-[a-z]+)*:kms:([a-z]{2}(?:-[a-z]+)+-\d+):(\d{12}):key/(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)
	regionRE    = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+$`)
	// bucketRE is S3's naming rule for general purpose buckets, without the
	// rarer exclusions (an IP address, the xn-- prefix), which S3 itself
	// refuses at creation.
	bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// LoadBackup reads the configuration of `mailserver backup`: the data
// directory, and the bucket, the key and their region, which are required.
func LoadBackup() (Backup, error) { return loadBackup(true) }

// LoadRestore reads the configuration of `mailserver backup restore`. Nothing
// is required here: the object names its own bucket, the key the backup must
// be under may come from the command line instead, and the region is that
// key's unless MAIL_BACKUP_REGION says otherwise.
func LoadRestore() (Backup, error) { return loadBackup(false) }

func loadBackup(upload bool) (Backup, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	env := Env(str("MAIL_ENV", string(EnvDev)))
	if env != EnvDev && env != EnvProd {
		bad("MAIL_ENV: want %q or %q, got %q", EnvDev, EnvProd, env)
	}
	b := Backup{
		Env:       env,
		Bucket:    str("MAIL_BACKUP_BUCKET", ""),
		KMSKeyARN: str("MAIL_BACKUP_KMS_KEY_ARN", ""),
		Region:    str("MAIL_BACKUP_REGION", ""),
		Log:       Log{Level: str("MAIL_LOG_LEVEL", "info"), Format: str("MAIL_LOG_FORMAT", defaultLogFormat(env))},
	}
	if upload {
		b.DataDir = dataDir(&errs)
	}
	errs = append(errs, b.Log.validate()...)

	switch {
	case b.Region == "" && upload:
		bad("MAIL_BACKUP_REGION is required: the AWS region of the bucket and the key, such as us-east-1")
	case b.Region != "" && !regionRE.MatchString(b.Region):
		bad("MAIL_BACKUP_REGION: %q is not an AWS region", b.Region)
	}
	switch {
	case b.Bucket == "" && upload:
		bad("MAIL_BACKUP_BUCKET is required: the S3 bucket the encrypted backups go to")
	case b.Bucket != "" && !bucketRE.MatchString(b.Bucket):
		bad("MAIL_BACKUP_BUCKET: %q is not an S3 bucket name", b.Bucket)
	}
	switch region, err := KMSKeyRegion(b.KMSKeyARN); {
	case b.KMSKeyARN == "" && upload:
		bad("MAIL_BACKUP_KMS_KEY_ARN is required: the ARN of the KMS key the backups are encrypted under")
	case b.KMSKeyARN == "":
	case err != nil:
		bad("MAIL_BACKUP_KMS_KEY_ARN: %v", err)
	case b.Region != "" && region != b.Region:
		// A KMS key answers only in its own region, and the backup talks to
		// KMS and S3 in one.
		bad("MAIL_BACKUP_KMS_KEY_ARN: the key is in %s, but MAIL_BACKUP_REGION is %s", region, b.Region)
	}

	if len(errs) > 0 {
		return Backup{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return b, nil
}

// KMSKeyRegion returns the region of a KMS key ARN, refusing anything that is
// not one, aliases included: an alias resolves to whatever key it points at
// when it is used, and whoever may update aliases could point it elsewhere.
func KMSKeyRegion(arn string) (string, error) {
	m := kmsKeyARNRE.FindStringSubmatch(arn)
	if m == nil {
		return "", fmt.Errorf("%q is not a KMS key ARN (arn:aws:kms:<region>:<account>:key/<id>; "+
			"an alias is not accepted)", arn)
	}
	return m[1], nil
}

// DatabasePath is the live SQLite file the backup reads.
func (b Backup) DatabasePath() string { return Config{DataDir: b.DataDir}.DatabasePath() }

// String renders the backup configuration for logs. Nothing in it is a
// secret today; it says whether the key is set rather than printing it so the
// account id stays out of log aggregators, as Config.String does for clients.
func (b Backup) String() string {
	return fmt.Sprintf("env=%s data=%s bucket=%s region=%s kms_key=%s log=%s/%s",
		b.Env, orDefault(b.DataDir, "unset"), orDefault(b.Bucket, "unset"), orDefault(b.Region, "unset"),
		configured(b.KMSKeyARN != ""), b.Log.Level, b.Log.Format)
}

// validate checks the level and format, as Load does.
func (l Log) validate() []error {
	var errs []error
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("MAIL_LOG_LEVEL: want debug|info|warn|error, got %q", l.Level))
	}
	switch l.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("MAIL_LOG_FORMAT: want json|text, got %q", l.Format))
	}
	return errs
}
