package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/backup"
	"github.com/thehappieco/mailie/internal/backup/backuptest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// testBucket is where the tests' backups go.
const testBucket = "example-mail-backups"

// fakeAWS is KMS and S3 from backuptest, as one set of clients.
type fakeAWS struct {
	*backuptest.KMS
	*backuptest.Bucket
}

// onlyEnv leaves the given MAIL_* variables and no other: the commands read
// os.Getenv, and the developer's shell must not decide what a test proves.
func onlyEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "MAIL_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// withFakeAWS installs fakes in place of the SDK clients and counts how
// often the commands asked for them.
func withFakeAWS(t *testing.T) (*fakeAWS, *int) {
	t.Helper()
	return withFakeAWSKey(t, backuptest.KeyARN)
}

// withFakeAWSKey is withFakeAWS with a fake KMS that holds keyARN, whose
// clients must be built for keyARN's region.
func withFakeAWSKey(t *testing.T, keyARN string) (*fakeAWS, *int) {
	t.Helper()
	fake := &fakeAWS{backuptest.NewKMS(keyARN), backuptest.NewBucket()}
	want, err := config.KMSKeyRegion(keyARN)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	prev := newBackupClients
	newBackupClients = func(_ context.Context, region string) (backupClients, error) {
		calls++
		if region != want {
			t.Errorf("clients built for %s, want the key's region %s", region, want)
		}
		return fake, nil
	}
	t.Cleanup(func() { newBackupClients = prev })
	return fake, &calls
}

func TestABackupAndARestoreNeedNoCredentialKey(t *testing.T) {
	dir := t.TempDir()
	storetest.NewAt(t, filepath.Join(dir, "mail.db"), nil)
	fake, _ := withFakeAWS(t)

	// The host: MAIL_ENV, its own three variables and the data directory,
	// nothing of the daemon's — no MAIL_CREDENTIAL_KEY_HEX among them.
	onlyEnv(t, map[string]string{
		"MAIL_ENV":                "prod",
		"MAIL_DATA_DIR":           dir,
		"MAIL_BACKUP_BUCKET":      testBucket,
		"MAIL_BACKUP_KMS_KEY_ARN": backuptest.KeyARN,
		"MAIL_BACKUP_REGION":      backuptest.Region,
		"MAIL_LOG_LEVEL":          "warn",
	})
	if err := run([]string{"backup"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	puts := fake.Puts()
	if len(puts) != 1 {
		t.Fatalf("%d uploads", len(puts))
	}
	// MAIL_ENV=prod is what a key policy's grant to the host can ask for.
	want := map[string]string{"service": "mailie", "env": "prod", "purpose": "db-backup", "ref": puts[0].Key}
	if got := fake.Contexts(); len(got) != 1 || !maps.Equal(got[0], want) {
		t.Fatalf("data key context %v, want %v", got, want)
	}
	object := "s3://" + puts[0].Bucket + "/" + puts[0].Key

	// Where a restore runs: no MAIL_* variable at all, only the key's ARN,
	// whose region is the restore's.
	onlyEnv(t, nil)
	out := filepath.Join(t.TempDir(), "from-s3.db")
	if err := run([]string{"backup", "restore", "--object", object, "--out", out, "--kms-key-arn", backuptest.KeyARN}); err != nil {
		t.Fatalf("restore --object: %v", err)
	}
	local := filepath.Join(t.TempDir(), "backup.mlbk")
	if err := os.WriteFile(local, fake.Object(puts[0].Bucket, puts[0].Key), 0o600); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(t.TempDir(), "from-file.db")
	if err := run([]string{"backup", "restore", "--file", local, "--out", out2, "--kms-key-arn", backuptest.KeyARN}); err != nil {
		t.Fatalf("restore --file: %v", err)
	}
	for _, p := range []string{out, out2} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestABackupWithoutItsConfigurationTouchesNothing(t *testing.T) {
	_, calls := withFakeAWS(t)
	onlyEnv(t, map[string]string{"MAIL_DATA_DIR": t.TempDir()})
	err := run([]string{"backup"})
	for _, missing := range []string{"MAIL_BACKUP_BUCKET", "MAIL_BACKUP_KMS_KEY_ARN", "MAIL_BACKUP_REGION"} {
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("err = %v, want every missing variable named, %s among them", err, missing)
		}
	}
	// An env the key policy knows nothing of goes no further either.
	onlyEnv(t, map[string]string{
		"MAIL_ENV": "staging", "MAIL_DATA_DIR": t.TempDir(), "MAIL_BACKUP_BUCKET": testBucket,
		"MAIL_BACKUP_KMS_KEY_ARN": backuptest.KeyARN, "MAIL_BACKUP_REGION": backuptest.Region,
	})
	if err := run([]string{"backup"}); err == nil || !strings.Contains(err.Error(), "MAIL_ENV") {
		t.Fatalf("MAIL_ENV=staging: err = %v", err)
	}
	if *calls != 0 {
		t.Fatal("AWS clients were built for a backup that could not run")
	}
}

func TestRestoreWantsOneSourceAndAnOutput(t *testing.T) {
	_, calls := withFakeAWS(t)
	onlyEnv(t, nil)
	out := filepath.Join(t.TempDir(), "x.db")
	for _, args := range [][]string{
		{"--out", out},
		{"--object", "s3://b/k", "--file", "f", "--out", out},
		{"--file", "f"},
		{"--object", "b/k", "--out", out},
		{"--object", "s3://b/", "--out", out},
		{"--object", "s3://b/k", "--out", out, "--kms-key-arn", "arn:aws:kms:" + backuptest.Region + ":111122223333:alias/example-backups"},
		{"--object", "s3://b/k", "--out", out, "--kms-key-arn", "alias/example-backups"},
		{"--object", "s3://b/k", "--out", out},
	} {
		if err := run(append([]string{"backup", "restore"}, args...)); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	// A region named for the restore that is not the key's: KMS would not
	// answer there.
	onlyEnv(t, map[string]string{"MAIL_BACKUP_REGION": "us-east-1"})
	err := run([]string{"backup", "restore", "--object", "s3://b/k", "--out", out, "--kms-key-arn", backuptest.KeyARN})
	if err == nil || !strings.Contains(err.Error(), "the key is in "+backuptest.Region+", but the restore runs in us-east-1") {
		t.Errorf("a key outside MAIL_BACKUP_REGION: err = %v", err)
	}
	if *calls != 0 {
		t.Fatal("AWS clients were built for a restore that could not run")
	}
}

func TestARestoreTrustsTheKeyItIsToldNotTheOneTheBackupNames(t *testing.T) {
	// A backup someone hands over, sealed in another account under a key
	// whose policy lets anyone decrypt. The fake KMS holds that key, as the
	// restoring credentials would reach it.
	const foreignARN = "arn:aws:kms:" + backuptest.Region + ":444455556666:key/00000000-0000-4000-8000-00000000000f"
	dir := t.TempDir()
	storetest.NewAt(t, filepath.Join(dir, "mail.db"), nil)
	fake, calls := withFakeAWSKey(t, foreignARN)
	onlyEnv(t, map[string]string{
		"MAIL_DATA_DIR":           dir,
		"MAIL_BACKUP_BUCKET":      testBucket,
		"MAIL_BACKUP_KMS_KEY_ARN": foreignARN,
		"MAIL_BACKUP_REGION":      backuptest.Region,
		"MAIL_LOG_LEVEL":          "warn",
	})
	if err := run([]string{"backup"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	puts := fake.Puts()
	local := filepath.Join(t.TempDir(), "handed-over.mlbk")
	if err := os.WriteFile(local, fake.Object(puts[0].Bucket, puts[0].Key), 0o600); err != nil {
		t.Fatal(err)
	}
	built := *calls

	onlyEnv(t, nil)
	out := filepath.Join(t.TempDir(), "mail.db")
	err := run([]string{"backup", "restore", "--file", local, "--out", out})
	if err == nil || !strings.Contains(err.Error(), "--kms-key-arn is required") {
		t.Fatalf("without a key: err = %v, want --kms-key-arn asked for", err)
	}
	if *calls != built {
		t.Fatal("AWS clients were built for a restore that named no key")
	}
	err = run([]string{"backup", "restore", "--file", local, "--out", out, "--kms-key-arn", backuptest.KeyARN})
	if !errors.Is(err, backup.ErrWrongKey) {
		t.Fatalf("pinned to the Mailie key: err = %v, want ErrWrongKey", err)
	}
	// MAIL_BACKUP_KMS_KEY_ARN pins it too.
	onlyEnv(t, map[string]string{"MAIL_BACKUP_KMS_KEY_ARN": backuptest.KeyARN})
	if err := run([]string{"backup", "restore", "--file", local, "--out", out}); !errors.Is(err, backup.ErrWrongKey) {
		t.Fatalf("pinned by MAIL_BACKUP_KMS_KEY_ARN: err = %v, want ErrWrongKey", err)
	}
	if fake.Decrypts() != 0 {
		t.Fatal("the foreign key was asked to unwrap a data key")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused restores left %s: %v", out, err)
	}
}

// captureStdout runs fn with os.Stdout going to a pipe, and returns what it
// printed. The tests here set the environment, so none runs in parallel.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	printed := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		printed <- string(b)
	}()
	defer func() { os.Stdout = prev }()
	ferr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-printed
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return out, ferr
}

func TestARestoreSaysDeletionsAfterItsBackupAreNotReappliedAsTheBackupDocDoes(t *testing.T) {
	dir := t.TempDir()
	storetest.NewAt(t, filepath.Join(dir, "mail.db"), nil)
	fake, _ := withFakeAWS(t)
	onlyEnv(t, map[string]string{
		"MAIL_ENV":                "prod",
		"MAIL_DATA_DIR":           dir,
		"MAIL_BACKUP_BUCKET":      testBucket,
		"MAIL_BACKUP_KMS_KEY_ARN": backuptest.KeyARN,
		"MAIL_BACKUP_REGION":      backuptest.Region,
		"MAIL_LOG_LEVEL":          "warn",
	})
	if err := run([]string{"backup"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	puts := fake.Puts()
	if len(puts) != 1 {
		t.Fatalf("%d uploads", len(puts))
	}

	onlyEnv(t, nil)
	out := filepath.Join(t.TempDir(), "restored.db")
	printed, err := captureStdout(t, func() error {
		return run([]string{"backup", "restore", "--object", "s3://" + puts[0].Bucket + "/" + puts[0].Key,
			"--out", out, "--kms-key-arn", backuptest.KeyARN})
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.HasPrefix(printed, "restored "+puts[0].Key+" (env prod) to "+out+"\n") {
		t.Fatalf("printed %q", printed)
	}
	// A restore re-applies nothing: the operator must not be told to redo
	// anything.
	lower := strings.ToLower(printed)
	if strings.Contains(lower, "redo") || !strings.Contains(lower, "deletions included") ||
		!strings.Contains(lower, "not re-applied") {
		t.Fatalf("printed %q, want later deletions said to be missing and not re-applied, and nothing to redo", printed)
	}

	// The section the restore points at exists, and says the same.
	m := regexp.MustCompile(`docs/backup\.md, "([^"]+)"`).FindStringSubmatch(printed)
	if m == nil {
		t.Fatalf("printed %q, which points at no section of docs/backup.md", printed)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "backup.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n### "+m[1]+"\n")
	if !ok {
		t.Fatalf("docs/backup.md has no section %q", m[1])
	}
	section, _, _ = strings.Cut(section, "\n#")
	if !strings.Contains(strings.Join(strings.Fields(section), " "), "deletions made after the backup are not re-applied") {
		t.Fatalf("docs/backup.md, %q, does not say that deletions after a backup are not re-applied", m[1])
	}
	// Nothing printed names a region: where a copy may be kept is the
	// operator's rule, not this program's.
	if region := regexp.MustCompile(`\b[a-z]{2}(-[a-z]+)+-\d\b`).FindString(printed); region != "" {
		t.Errorf("printed %q, which names the region %s", printed, region)
	}
}
