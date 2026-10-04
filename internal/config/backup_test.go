package config_test

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

// The account is AWS's documentation account, which belongs to nobody.
const (
	backupKeyARN = "arn:aws:kms:us-east-2:111122223333:key/0b9e7a1c-2d3f-4a5b-8c6d-7e8f9a0b1c2d"
	backupRegion = "us-east-2"
	backupBucket = "example-mail-backups"
)

// backupEnv is a complete backup configuration, plus extra.
func backupEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{
		"MAIL_DATA_DIR": t.TempDir(), "MAIL_BACKUP_BUCKET": backupBucket,
		"MAIL_BACKUP_KMS_KEY_ARN": backupKeyARN, "MAIL_BACKUP_REGION": backupRegion,
	}
	maps.Copy(env, extra)
	return env
}

func TestABackupNeedsItsBucketKeyAndRegionButNoCredentialKey(t *testing.T) {
	dir := t.TempDir()
	setenv(t, backupEnv(t, map[string]string{"MAIL_DATA_DIR": dir}))
	b, err := config.LoadBackup()
	if err != nil {
		t.Fatalf("LoadBackup: %v", err)
	}
	if b.Region != backupRegion || b.DatabasePath() != filepath.Join(dir, "mail.db") || b.KMSKeyARN != backupKeyARN {
		t.Fatalf("got %+v", b)
	}

	setenv(t, map[string]string{"MAIL_DATA_DIR": dir})
	_, err = config.LoadBackup()
	for _, want := range []string{"MAIL_BACKUP_BUCKET is required", "MAIL_BACKUP_KMS_KEY_ARN is required", "MAIL_BACKUP_REGION is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestABackupHasNoDefaultRegion(t *testing.T) {
	// Where the copies of a database are kept is the operator's decision:
	// a backup with everything but the region goes nowhere.
	env := backupEnv(t, nil)
	delete(env, "MAIL_BACKUP_REGION")
	setenv(t, env)
	if _, err := config.LoadBackup(); err == nil || !strings.Contains(err.Error(), "MAIL_BACKUP_REGION is required") {
		t.Fatalf("err = %v", err)
	}
	// A restore runs in its key's region, which the command takes from the
	// key it is told to trust.
	b, err := config.LoadRestore()
	if err != nil || b.Region != "" {
		t.Fatalf("LoadRestore = %+v, %v; want no region of its own", b, err)
	}
}

func TestABackupsEnvIsMailEnvAsForTheDaemon(t *testing.T) {
	base := backupEnv(t, nil)
	for env, want := range map[string]config.Env{"": config.EnvDev, "dev": config.EnvDev, "prod": config.EnvProd} {
		vars := maps.Clone(base)
		if env != "" {
			vars["MAIL_ENV"] = env
		}
		setenv(t, vars)
		b, err := config.LoadBackup()
		if err != nil || b.Env != want {
			t.Errorf("MAIL_ENV=%q: env %q, %v; want %q", env, b.Env, err, want)
		}
	}
	base["MAIL_ENV"] = "production"
	setenv(t, base)
	if _, err := config.LoadBackup(); err == nil || !strings.Contains(err.Error(), "MAIL_ENV") {
		t.Errorf("MAIL_ENV=production: err = %v", err)
	}
}

func TestBackupAndRestoreTakeTheDaemonsLogSettingsAndRefuseAnInvalidOne(t *testing.T) {
	// docs/backup.md lists MAIL_LOG_LEVEL and MAIL_LOG_FORMAT with the
	// other variables the two commands read.
	base := backupEnv(t, nil)
	vars := maps.Clone(base)
	vars["MAIL_LOG_LEVEL"], vars["MAIL_LOG_FORMAT"] = "debug", "json"
	setenv(t, vars)
	for name, load := range map[string]func() (config.Backup, error){"backup": config.LoadBackup, "restore": config.LoadRestore} {
		b, err := load()
		if err != nil || b.Log.Level != "debug" || b.Log.Format != "json" {
			t.Errorf("%s: log %+v, %v; want debug/json", name, b.Log, err)
		}
	}
	setenv(t, base)
	for name, load := range map[string]func() (config.Backup, error){"backup": config.LoadBackup, "restore": config.LoadRestore} {
		b, err := load()
		if err != nil || b.Log.Level != "info" || b.Log.Format != "text" {
			t.Errorf("%s: log %+v, %v; want the defaults info/text outside prod", name, b.Log, err)
		}
	}
	for variable, value := range map[string]string{"MAIL_LOG_LEVEL": "verbose", "MAIL_LOG_FORMAT": "pretty"} {
		vars := maps.Clone(base)
		vars[variable] = value
		setenv(t, vars)
		for name, load := range map[string]func() (config.Backup, error){"backup": config.LoadBackup, "restore": config.LoadRestore} {
			if _, err := load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Errorf("%s with %s=%s: err = %v", name, variable, value, err)
			}
		}
	}
}

func TestARestoreNeedsNoConfigurationAtAll(t *testing.T) {
	setenv(t, nil)
	b, err := config.LoadRestore()
	if err != nil {
		t.Fatalf("LoadRestore: %v", err)
	}
	if b.Region != "" || b.DataDir != "" {
		t.Fatalf("got %+v", b)
	}
}

func TestTheBackupKeyIsAKeyARNInTheBackupRegion(t *testing.T) {
	for name, c := range map[string]struct{ arn, region, want string }{
		"an alias":           {"arn:aws:kms:us-east-2:111122223333:alias/example-backups", "", "an alias is not accepted"},
		"a bare alias":       {"alias/example-backups", "", "not a KMS key ARN"},
		"a bare key id":      {"0b9e7a1c-2d3f-4a5b-8c6d-7e8f9a0b1c2d", "", "not a KMS key ARN"},
		"another region":     {strings.Replace(backupKeyARN, backupRegion, "us-east-1", 1), "", "the key is in us-east-1"},
		"a region mismatch":  {backupKeyARN, "eu-central-1", "MAIL_BACKUP_REGION is eu-central-1"},
		"not a region":       {backupKeyARN, "ohio", "is not an AWS region"},
		"a truncated key id": {backupKeyARN[:len(backupKeyARN)-1], "", "not a KMS key ARN"},
	} {
		extra := map[string]string{"MAIL_BACKUP_KMS_KEY_ARN": c.arn}
		if c.region != "" {
			extra["MAIL_BACKUP_REGION"] = c.region
		}
		setenv(t, backupEnv(t, extra))
		for _, load := range []func() (config.Backup, error){config.LoadBackup, config.LoadRestore} {
			if _, err := load(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", name, err, c.want)
			}
		}
	}
}

func TestABucketNameS3WouldRefuseIsRefused(t *testing.T) {
	for _, bucket := range []string{"Example", "a", "-leading", "trailing-", "s3://" + backupBucket, "has_underscore"} {
		setenv(t, backupEnv(t, map[string]string{"MAIL_BACKUP_BUCKET": bucket}))
		if _, err := config.LoadBackup(); err == nil || !strings.Contains(err.Error(), "not an S3 bucket name") {
			t.Errorf("%q: err = %v", bucket, err)
		}
	}
}

func TestTheBackupStringSaysWhetherAKeyIsSetWithoutPrintingIt(t *testing.T) {
	setenv(t, backupEnv(t, map[string]string{
		// Present in the host's environment file; the backup never reads it.
		"MAIL_CREDENTIAL_KEY_HEX": validKey,
	}))
	b, err := config.LoadBackup()
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, never := range []string{backupKeyARN, "111122223333", validKey} {
		if strings.Contains(out, never) {
			t.Errorf("String() prints %q:\n%s", never, out)
		}
	}
	if !strings.Contains(out, "kms_key=set") || !strings.Contains(out, "bucket="+backupBucket) ||
		!strings.Contains(out, "region="+backupRegion) {
		t.Errorf("String() = %s", out)
	}
}
