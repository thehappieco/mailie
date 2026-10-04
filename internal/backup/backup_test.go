package backup_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/backup"
	"github.com/thehappieco/mailie/internal/backup/backuptest"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

const (
	bucketName = "example-mail-backups"
	// The values the plaintext search looks for: a person's address and a
	// row the test writes itself.
	canaryEmail = "canary-4c1d9e@backup.example"
	canaryValue = "plaintext-canary-7b2e51f0c3"
	// smallChunks makes even a fresh database span dozens of chunks.
	smallChunks = 4 << 10
)

// writerEnv makes the test binary a writer process instead of a test run:
// the daemon's half of the point-in-time test, in a process of its own, as
// on the host.
const writerEnv = "MAILIE_BACKUP_TEST_WRITER"

func TestMain(m *testing.M) {
	if path := os.Getenv(writerEnv); path != "" {
		os.Exit(runWriter(path))
	}
	os.Exit(m.Run())
}

// runWriter commits to the ledger in a loop through the daemon's own store,
// until its standard input closes. Every transaction adds row n+1 and sets
// meta.ledger to n+1, so in any consistent state the row count, the highest
// n and the counter are one number.
func runWriter(path string) int {
	ctx := context.Background()
	db, err := store.Open(ctx, path, store.Options{SkipMigrate: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "writer:", err)
		return 1
	}
	//nolint:errcheck // the writer's exit is what the test waits on
	defer func() { _ = db.Close() }()
	stop := make(chan struct{})
	go func() {
		//nolint:errcheck // EOF is the signal
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stop)
	}()
	for ready := false; ; ready = true {
		select {
		case <-stop:
			return 0
		default:
		}
		err := db.Write(ctx, func(tx *sql.Tx) error {
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(n), 0) FROM ledger`).Scan(&n); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO ledger(n, pad) VALUES (?, randomblob(512))`, n+1); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'ledger'`, strconv.FormatInt(n+1, 10))
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "writer:", err)
			return 1
		}
		if !ready {
			fmt.Println("ready")
		}
	}
}

// liveDatabase is a migrated database with a person and a meta row in it,
// held open by a store the way the daemon holds it.
func liveDatabase(t *testing.T) (string, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mail.db")
	db := storetest.NewAt(t, path, nil)
	authtest.NewUser(t, db, canaryEmail, auth.RoleOwner)
	if err := db.SetMeta(context.Background(), "backup_canary", canaryValue); err != nil {
		t.Fatal(err)
	}
	return path, db
}

type fixture struct {
	kms    *backuptest.KMS
	bucket *backuptest.Bucket
	tmp    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{kms: backuptest.NewKMS(backuptest.KeyARN), bucket: backuptest.NewBucket(), tmp: t.TempDir()}
}

func (f *fixture) options(path string) backup.Options {
	return backup.Options{
		DatabasePath: path, Env: "prod", Bucket: bucketName, KMSKeyARN: backuptest.KeyARN,
		KMS: f.kms, Uploader: f.bucket, TempDir: f.tmp, ChunkSize: smallChunks,
		Logger: obs.NewLoggerTo(io.Discard, "info", "text"),
	}
}

// backup runs one backup and returns it with the object's bytes.
func (f *fixture) backup(t *testing.T, path string) (backup.Result, []byte) {
	t.Helper()
	res, err := backup.Run(context.Background(), f.options(path))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	obj := f.bucket.Object(bucketName, res.Key)
	if obj == nil {
		t.Fatalf("nothing stored at %s", res.Key)
	}
	return res, obj
}

// restore restores obj to out, trusting the fixture's key.
func (f *fixture) restore(obj []byte, out string) (backup.RestoreResult, error) {
	return backup.Restore(context.Background(), backup.RestoreOptions{
		Source: bytes.NewReader(obj), KMS: f.kms, KMSKeyARN: backuptest.KeyARN, Out: out,
	})
}

// dump reads every row of every table, sorted, through a read-only
// connection.
func dump(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := func(q string, row func(cols []string, vals []any)) {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			row(cols, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	var tables []string
	query(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`,
		func(_ []string, vals []any) { tables = append(tables, vals[0].(string)) })
	out := map[string][]string{}
	for _, table := range tables {
		out[table] = []string{}
		query(`SELECT * FROM "`+table+`"`, func(_ []string, vals []any) {
			out[table] = append(out[table], fmt.Sprintf("%#v", vals))
		})
		slices.Sort(out[table])
	}
	query(`PRAGMA user_version`, func(_ []string, vals []any) {
		out["PRAGMA user_version"] = []string{fmt.Sprint(vals[0])}
	})
	return out
}

func fileState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x size=%d mtime=%s", sum, len(b), st.ModTime().Format(time.RFC3339Nano))
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s should be empty, holds %v", dir, names)
	}
}

func TestABackupRestoresToTheSameRows(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	res, obj := f.backup(t, path)

	out := filepath.Join(t.TempDir(), "restored.db")
	got, err := f.restore(obj, out)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	live, restored := dump(t, path), dump(t, out)
	if !maps.EqualFunc(live, restored, slices.Equal) {
		for table := range live {
			if !slices.Equal(live[table], restored[table]) {
				t.Errorf("%s: live %d rows, restored %d", table, len(live[table]), len(restored[table]))
			}
		}
		t.Fatal("the restored database differs from the live one")
	}
	if len(live["users"]) != 1 {
		t.Fatalf("the fixture should hold one person, holds %d", len(live["users"]))
	}
	if got.Object != res.Key || got.Env != "prod" || got.KeyARN != backuptest.KeyARN || got.PlaintextBytes != res.PlaintextBytes {
		t.Fatalf("restore says %+v; the backup was %+v", got, res)
	}
	if got.Rows["users"] != 1 || got.SchemaVersion == 0 || got.SchemaVersion != res.SchemaVersion {
		t.Fatalf("restore inspection %+v, backup inspection %+v", got.Inspection, res.Inspection)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("the restored database is %v, want 0600", st.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the restore left %d entries beside its output", len(entries)-1)
	}
}

func TestTheUploadedObjectHoldsNoPlaintext(t *testing.T) {
	path, _ := liveDatabase(t)
	// The values are in the live database, or the search below proves
	// nothing.
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(append(live, wal...), []byte(canaryEmail)) || !bytes.Contains(append(live, wal...), []byte(canaryValue)) {
		t.Fatal("the canaries are not in the live database files")
	}

	f := newFixture(t)
	_, obj := f.backup(t, path)
	for _, needle := range []string{canaryEmail, canaryValue, "SQLite format 3", "CREATE TABLE"} {
		if bytes.Contains(obj, []byte(needle)) {
			t.Errorf("the uploaded object contains %q", needle)
		}
	}
}

func TestTheBackupNeverWritesToTheLiveDatabase(t *testing.T) {
	path, _ := liveDatabase(t)
	before := map[string]string{"db": fileState(t, path), "wal": fileState(t, path+"-wal")}
	if before["wal"] == "absent" {
		t.Fatal("the store should hold a -wal open, as the daemon does")
	}
	// Coarse mtimes would hide a write in the same instant.
	time.Sleep(20 * time.Millisecond)

	f := newFixture(t)
	f.backup(t, path)
	after := map[string]string{"db": fileState(t, path), "wal": fileState(t, path+"-wal")}
	if !maps.Equal(before, after) {
		t.Fatalf("the live files changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestTheSnapshotConnectionCannotWrite(t *testing.T) {
	path, _ := liveDatabase(t)
	db, err := sql.Open("sqlite", backup.SourceDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`UPDATE meta SET value = 'changed' WHERE key = 'backup_canary'`,
		`CREATE TABLE intruder(x)`,
		`DELETE FROM users`,
		`PRAGMA user_version = 999`,
	} {
		_, err := db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "readonly") {
			t.Errorf("%s: err = %v, want SQLITE_READONLY", stmt, err)
		}
	}
}

func TestTheWorkingDirectoryIsGoneAfterEveryOutcome(t *testing.T) {
	path, _ := liveDatabase(t)

	run := func(t *testing.T, f *fixture, ctx context.Context) error {
		t.Helper()
		_, err := backup.Run(ctx, f.options(path))
		return err
	}
	// sawDir records, from inside the upload, that the working directory
	// existed: otherwise "it is gone" would also pass if it never was.
	sawDir := func(t *testing.T, f *fixture, then func(ctx context.Context) error) {
		f.bucket.BeforePut = func(ctx context.Context, _ backup.PutInput) error {
			entries, err := os.ReadDir(f.tmp)
			if err != nil || len(entries) != 1 {
				t.Errorf("during the upload, %s holds %d entries (%v), want the working directory", f.tmp, len(entries), err)
			}
			return then(ctx)
		}
	}

	t.Run("success", func(t *testing.T) {
		f := newFixture(t)
		sawDir(t, f, func(context.Context) error { return nil })
		if err := run(t, f, context.Background()); err != nil {
			t.Fatal(err)
		}
		emptyDir(t, f.tmp)
	})
	t.Run("failure", func(t *testing.T) {
		f := newFixture(t)
		boom := errors.New("S3 said no")
		sawDir(t, f, func(context.Context) error { return boom })
		if err := run(t, f, context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		emptyDir(t, f.tmp)
	})
	t.Run("KMS refuses", func(t *testing.T) {
		f := newFixture(t)
		f.kms.Fail = errors.New("AccessDeniedException")
		if err := run(t, f, context.Background()); !errors.Is(err, f.kms.Fail) {
			t.Fatalf("err = %v", err)
		}
		emptyDir(t, f.tmp)
	})
	t.Run("cancelled during the upload", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sawDir(t, f, func(ctx context.Context) error {
			// What SIGTERM does: the signal context is cancelled.
			cancel()
			<-ctx.Done()
			return ctx.Err()
		})
		if err := run(t, f, ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		emptyDir(t, f.tmp)
	})
	t.Run("cancelled before the snapshot", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := run(t, f, ctx); err == nil {
			t.Fatal("a cancelled backup succeeded")
		}
		emptyDir(t, f.tmp)
	})
	t.Run("panic", func(t *testing.T) {
		f := newFixture(t)
		sawDir(t, f, func(context.Context) error { panic("the uploader panicked") })
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("the panic did not propagate")
				}
			}()
			//nolint:errcheck // it panics
			_ = run(t, f, context.Background())
		}()
		emptyDir(t, f.tmp)
	})
}

func TestThePlaintextDataKeyIsZeroedAfterUse(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	if _, err := f.restore(obj, filepath.Join(t.TempDir(), "restored.db")); err != nil {
		t.Fatal(err)
	}
	// And a restore that fails after KMS answered.
	damaged := bytes.Clone(obj)
	damaged[len(damaged)-1] ^= 1
	if _, err := f.restore(damaged, filepath.Join(t.TempDir(), "restored.db")); err == nil {
		t.Fatal("a damaged backup restored")
	}

	handed := f.kms.Handed()
	if len(handed) != 3 {
		t.Fatalf("KMS handed out %d plaintext keys, want one per backup and per restore", len(handed))
	}
	for i, key := range handed {
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Errorf("plaintext data key %d was not zeroed: %s", i, hex.EncodeToString(key))
		}
	}
}

func TestTheSnapshotAndTheBackupStayInAPrivateDirectory(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	f.bucket.BeforePut = func(_ context.Context, in backup.PutInput) error {
		file, ok := in.Body.(*os.File)
		if !ok {
			return fmt.Errorf("the body is a %T, not the file on disk", in.Body)
		}
		dir := filepath.Dir(file.Name())
		for p, want := range map[string]os.FileMode{dir: 0o700, file.Name(): 0o600} {
			st, err := os.Stat(p)
			if err != nil {
				return err
			}
			if st.Mode().Perm() != want {
				t.Errorf("%s is %v, want %v", p, st.Mode().Perm(), want)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 1 || entries[0].Name() != filepath.Base(file.Name()) {
			t.Errorf("during the upload the working directory holds %d entries; the plaintext snapshot should be gone", len(entries))
		}
		return nil
	}
	f.backup(t, path)
}

func TestABackupsContextIsExactlyServiceEnvPurposeAndItsObject(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	res, obj := f.backup(t, path)

	if !regexp.MustCompile(`^db/\d{8}T\d{6}Z-[0-9a-f]{8}\.mlbk$`).MatchString(res.Key) {
		t.Fatalf("object key %q", res.Key)
	}
	// Spelled out, not built from the package's constants: these are the
	// values a key policy names, so existing backups and policies keep
	// matching, and the object key is the whole of ref — a time and random
	// hex, nothing about anyone.
	want := map[string]string{"service": "mailie", "env": "prod", "purpose": "db-backup", "ref": res.Key}
	contexts := f.kms.Contexts()
	if len(contexts) != 1 || !maps.Equal(contexts[0], want) {
		t.Fatalf("data key generated with context %v, want %v", contexts, want)
	}
	// SplitHeader reads only a context whose keys are strictly ascending
	// (env, purpose, ref, service): one encoding per header.
	_, fields, err := backup.SplitHeader(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(fields.Context, want) || fields.KeyARN != backuptest.KeyARN || fields.ChunkSize != smallChunks {
		t.Fatalf("header %+v", fields)
	}
	if !maps.Equal(backup.EncryptionContext("prod", res.Key), want) {
		t.Fatalf("EncryptionContext = %v, want %v", backup.EncryptionContext("prod", res.Key), want)
	}
	puts := f.bucket.Puts()
	sum := sha256.Sum256(obj)
	if len(puts) != 1 || puts[0].Bucket != bucketName || puts[0].Key != res.Key ||
		!bytes.Equal(puts[0].SHA256, sum[:]) || puts[0].Size != int64(len(obj)) {
		t.Fatalf("uploads %+v", puts)
	}
	if res.CiphertextSHA256 != hex.EncodeToString(sum[:]) || res.CiphertextBytes != int64(len(obj)) {
		t.Fatalf("result %+v", res)
	}
	// The plaintext is the snapshot, which is smaller than the live files.
	if res.PlaintextBytes <= 0 || res.PlaintextBytes%4096 != 0 {
		t.Fatalf("plaintext of %d bytes is not a whole number of pages", res.PlaintextBytes)
	}
}

func TestTheContextCarriesTheEnvTheBackupIsTakenInAndABackupWithoutOneIsRefused(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	opts := f.options(path)
	opts.Env = "dev"
	res, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.kms.Contexts(); len(got) != 1 || got[0]["env"] != "dev" {
		t.Fatalf("contexts %v, want env=dev", got)
	}
	restored, err := f.restore(f.bucket.Object(bucketName, res.Key), filepath.Join(t.TempDir(), "mail.db"))
	if err != nil || restored.Env != "dev" || restored.Object != res.Key {
		t.Fatalf("restore: %+v, %v", restored, err)
	}

	opts.Env = ""
	if _, err := backup.Run(context.Background(), opts); err == nil {
		t.Fatal("a backup with no env ran")
	}
	if len(f.kms.Contexts()) != 1 || len(f.bucket.Puts()) != 1 {
		t.Fatal("a backup with no env reached KMS or S3")
	}
}

func TestABackupIsNamedNoLaterThanItsSnapshot(t *testing.T) {
	// The time in the name is a lower bound: nothing committed before it is
	// missing from the backup. A deletion committed after the snapshot, while
	// it is checked and sealed, is missing from it (the row is still there),
	// so the deletion must fall after that time.
	path, db := liveDatabase(t)
	f := newFixture(t)
	// Each reading of the clock is a minute after the one before: the check
	// and the encryption take time.
	base := time.Date(2026, 10, 1, 10, 28, 0, 0, time.UTC)
	var ticks atomic.Int64
	clock := func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * time.Minute) }
	var deletedAt time.Time
	defer backup.SetAfterSnapshot(func(string) {
		deletedAt = clock()
		err := db.Write(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(), `DELETE FROM meta WHERE key = 'backup_canary'`)
			return err
		})
		if err != nil {
			t.Error(err)
		}
	})()
	opts := f.options(path)
	opts.Now = clock
	res, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	stamp, _, _ := strings.Cut(strings.TrimPrefix(res.Key, "db/"), "-")
	named, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		t.Fatalf("object key %q: %v", res.Key, err)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if _, err := f.restore(f.bucket.Object(bucketName, res.Key), out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(dump(t, out)["meta"], "\n"), canaryValue) {
		t.Fatal("the deletion made after the snapshot is in the backup: the test proves nothing")
	}
	if named.After(deletedAt) {
		t.Fatalf("the backup is named %s, after a deletion at %s that it does not reflect: "+
			"its name dates before it a change it lacks", named.Format(time.RFC3339), deletedAt.Format(time.RFC3339))
	}
}

// breakSnapshot leaves an index that disagrees with its table: a file SQLite
// opens happily and PRAGMA integrity_check reports, row by row.
func breakSnapshot(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TABLE damaged(a, b)`,
		`CREATE INDEX damaged_a ON damaged(a)`,
		`INSERT INTO damaged VALUES (1, 2), (3, 4)`,
		`PRAGMA writable_schema = 1`,
		`UPDATE sqlite_schema SET sql = 'CREATE INDEX damaged_a ON damaged(b)' WHERE name = 'damaged_a'`,
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func TestADamagedSnapshotIsNeverUploaded(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	defer backup.SetAfterSnapshot(func(p string) { breakSnapshot(t, p) })()

	_, err := backup.Run(context.Background(), f.options(path))
	if !errors.Is(err, backup.ErrDamagedDatabase) || !strings.Contains(err.Error(), "missing from index damaged_a") {
		t.Fatalf("err = %v, want the integrity check to refuse it", err)
	}
	if len(f.bucket.Puts()) != 0 || len(f.kms.Contexts()) != 0 {
		t.Fatal("a damaged snapshot reached KMS or S3")
	}
	emptyDir(t, f.tmp)
}

func TestAnUnreadableFileFailsTheIntegrityCheck(t *testing.T) {
	path, _ := liveDatabase(t)
	out := filepath.Join(t.TempDir(), "snap.db")
	if err := backup.Snapshot(context.Background(), path, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Scribble over every page but the first: the schema still reads, the
	// trees it points at do not.
	for i := 4096; i < len(b); i++ {
		b[i] = byte(i * 31)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Inspect(context.Background(), out); !errors.Is(err, backup.ErrDamagedDatabase) {
		t.Fatalf("Inspect = %v, want ErrDamagedDatabase", err)
	}
}

// seedLedger makes a database the writer process can work on, with enough
// padding that a snapshot takes a while.
func seedLedger(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE ledger(n INTEGER PRIMARY KEY, pad BLOB NOT NULL)`,
			`CREATE TABLE padding(id INTEGER PRIMARY KEY, pad BLOB NOT NULL)`,
			`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 2500)
			 INSERT INTO padding(pad) SELECT randomblob(1024) FROM r`,
			`INSERT INTO meta(key, value) VALUES ('ledger', '0')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// startWriter runs the writer process on path and returns once it has
// committed.
func startWriter(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), writerEnv+"="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("writer: %v", err)
			}
		case <-time.After(20 * time.Second):
			//nolint:errcheck // giving up on it
			_ = cmd.Process.Kill()
			t.Error("the writer did not stop")
		}
	})
	ready := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		ready <- err == nil && line == "ready\n"
		//nolint:errcheck // nothing more is expected
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the writer did not start")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the writer did not commit within 30 s")
	}
}

// ledger reads the counter through a read-only connection.
func ledger(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", backup.SourceDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'ledger'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// readOnly makes the data directory and its files read-only, as the backup
// unit's ReadOnlyPaths does, until the test ends.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes; this case needs an unprivileged user")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o600) })
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
}

func TestTheSnapshotIsAPointInTimeWhileAnotherProcessWrites(t *testing.T) {
	for _, c := range []struct {
		name     string
		readOnly bool
	}{{"writable data directory", false}, {"read-only data directory, as the backup unit sees it", true}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mail.db")
			seedLedger(t, path)
			startWriter(t, path)
			if c.readOnly {
				readOnly(t, dir)
			}

			before := ledger(t, path)
			out := filepath.Join(t.TempDir(), "snap.db")
			start := time.Now()
			if err := backup.Snapshot(context.Background(), path, out); err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			took := time.Since(start)
			after := ledger(t, path)

			if _, err := backup.Inspect(context.Background(), out); err != nil {
				t.Fatalf("the snapshot: %v", err)
			}
			snap, err := sql.Open("sqlite", "file:"+out+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer snap.Close()
			var rows, highest int64
			var counter string
			if err := snap.QueryRow(`SELECT count(*), coalesce(max(n), 0), (SELECT value FROM meta WHERE key = 'ledger') FROM ledger`).
				Scan(&rows, &highest, &counter); err != nil {
				t.Fatal(err)
			}
			if strconv.FormatInt(rows, 10) != counter || rows != highest {
				t.Fatalf("a torn snapshot: %d rows, highest %d, counter %s", rows, highest, counter)
			}
			if rows < before || rows > after {
				t.Fatalf("the snapshot holds %d commits; before it %d, after it %d", rows, before, after)
			}
			// Otherwise the test proves nothing about concurrency.
			if after-before < 2 {
				t.Fatalf("the writer committed %d times during a %v snapshot; want it busy throughout", after-before, took)
			}
			t.Logf("snapshot of %d commits in %v while the writer went from %d to %d", rows, took, before, after)
		})
	}
}

func TestWithTheDaemonStoppedAReadOnlyDataDirectoryIsReportedAsSuch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mail.db")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a closed store should leave no -wal behind: %v", err)
	}
	readOnly(t, dir)

	f := newFixture(t)
	_, err = backup.Run(context.Background(), f.options(path))
	if err == nil || !strings.Contains(err.Error(), "is the daemon running against this data directory?") {
		t.Fatalf("err = %v, want it to say the daemon looks stopped", err)
	}
	emptyDir(t, f.tmp)
}

func TestTheLogLineSaysWhatWasUploadedAndNothingOfTheContents(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	var logs bytes.Buffer
	opts := f.options(path)
	opts.Logger = obs.NewLoggerTo(&logs, "debug", "json")
	res, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("want exactly one JSON log line, got %q: %v", logs.String(), err)
	}
	var keys []string
	for k := range line {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"ciphertext_bytes", "ciphertext_sha256", "duration_ms", "level", "msg", "object", "plaintext_bytes",
		"rows_accounts", "rows_messages", "rows_users", "schema_version", "time"}
	if !slices.Equal(keys, want) {
		t.Fatalf("log fields %v, want %v", keys, want)
	}
	if line["object"] != "s3://"+bucketName+"/"+res.Key || line["ciphertext_sha256"] != res.CiphertextSHA256 {
		t.Fatalf("log line %v", line)
	}
	for _, needle := range []string{canaryEmail, canaryValue} {
		if strings.Contains(logs.String(), needle) {
			t.Fatalf("the log carries %q", needle)
		}
	}
}
