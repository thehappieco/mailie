package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/backup"
	"github.com/thehappieco/mailie/internal/backup/backuptest"
)

// chunks splits a backup into its header and its sealed chunks.
func chunks(t *testing.T, obj []byte) ([]byte, [][]byte) {
	t.Helper()
	n, fields, err := backup.SplitHeader(obj)
	if err != nil {
		t.Fatal(err)
	}
	size := fields.ChunkSize + 16
	var out [][]byte
	for body := obj[n:]; len(body) > 0; {
		k := min(size, len(body))
		out = append(out, body[:k])
		body = body[k:]
	}
	return obj[:n], out
}

func join(header []byte, cs ...[]byte) []byte {
	return bytes.Join(append([][]byte{header}, cs...), nil)
}

// refused restores obj and fails the test unless the restore failed with
// want (any error if want is nil) and left nothing in the output directory.
func (f *fixture) refused(t *testing.T, name string, obj []byte, want error) {
	t.Helper()
	f.refusedUnder(t, name, obj, backuptest.KeyARN, want)
}

// refusedUnder is refused, with the restore trusting keyARN.
func (f *fixture) refusedUnder(t *testing.T, name string, obj []byte, keyARN string, want error) {
	t.Helper()
	dir := t.TempDir()
	_, err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: bytes.NewReader(obj), KMS: f.kms, KMSKeyARN: keyARN, Out: filepath.Join(dir, "restored.db"),
	})
	switch {
	case err == nil:
		t.Errorf("%s: restored", name)
	case want != nil && !errors.Is(err, want):
		t.Errorf("%s: err = %v, want %v", name, err, want)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("%s: the failed restore left %d files behind", name, len(entries))
	}
}

func TestATamperedBackupIsRefused(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	_, other := f.backup(t, path)
	header, cs := chunks(t, obj)
	otherHeader, otherChunks := chunks(t, other)
	if len(cs) < 4 {
		t.Fatalf("the backup has %d chunks; the cases below need at least 4", len(cs))
	}
	last := len(cs) - 1
	// The untouched backup restores, so every refusal below is the edit's.
	if _, err := f.restore(obj, filepath.Join(t.TempDir(), "ok.db")); err != nil {
		t.Fatalf("the untouched backup: %v", err)
	}

	t.Run("a flipped byte in any chunk", func(t *testing.T) {
		for i, c := range cs {
			at := len(header) + i*len(cs[0]) + (i*7919)%len(c)
			b := bytes.Clone(obj)
			b[at] ^= 0x01
			f.refused(t, fmt.Sprintf("chunk %d", i), b, backup.ErrCorrupt)
		}
	})
	t.Run("a flipped byte in a tag", func(t *testing.T) {
		b := bytes.Clone(obj)
		b[len(header)+len(cs[0])-1] ^= 0x80
		f.refused(t, "tag", b, backup.ErrCorrupt)
	})
	t.Run("truncated", func(t *testing.T) {
		f.refused(t, "after the header", header, backup.ErrCorrupt)
		f.refused(t, "inside a chunk", obj[:len(header)+len(cs[0])+100], backup.ErrCorrupt)
		f.refused(t, "at a chunk boundary", join(header, cs[:2]...), backup.ErrCorrupt)
		f.refused(t, "one byte short", obj[:len(obj)-1], backup.ErrCorrupt)
		f.refused(t, "inside the header", header[:len(header)-3], backup.ErrCorrupt)
		f.refused(t, "inside the fixed header", header[:10], backup.ErrNotABackup)
		f.refused(t, "empty", nil, backup.ErrNotABackup)
	})
	t.Run("the final chunk dropped", func(t *testing.T) {
		f.refused(t, "drop", join(header, cs[:last]...), backup.ErrCorrupt)
	})
	t.Run("a middle chunk dropped", func(t *testing.T) {
		f.refused(t, "drop", join(header, append(append([][]byte{}, cs[:2]...), cs[3:]...)...), backup.ErrCorrupt)
	})
	t.Run("chunks reordered", func(t *testing.T) {
		swapped := append([][]byte{}, cs...)
		swapped[1], swapped[2] = swapped[2], swapped[1]
		f.refused(t, "swap 1 and 2", join(header, swapped...), backup.ErrCorrupt)
		// The final chunk moved, with a full chunk now at the end.
		moved := append(append([][]byte{}, cs[:last-1]...), cs[last], cs[last-1])
		f.refused(t, "final chunk moved up", join(header, moved...), backup.ErrCorrupt)
	})
	t.Run("a chunk duplicated", func(t *testing.T) {
		dup := append(append(append([][]byte{}, cs[:2]...), cs[1]), cs[2:]...)
		f.refused(t, "chunk 1 twice", join(header, dup...), backup.ErrCorrupt)
		f.refused(t, "final chunk twice", join(header, append(append([][]byte{}, cs...), cs[last])...), backup.ErrCorrupt)
	})
	t.Run("a chunk swapped in from another backup", func(t *testing.T) {
		mixed := append([][]byte{}, cs...)
		mixed[1] = otherChunks[1]
		f.refused(t, "chunk 1 from the other", join(header, mixed...), backup.ErrCorrupt)
		f.refused(t, "the other's header on these chunks", join(otherHeader, cs...), backup.ErrCorrupt)
	})
	t.Run("data after the final chunk", func(t *testing.T) {
		f.refused(t, "one byte", append(bytes.Clone(obj), 0), backup.ErrCorrupt)
		f.refused(t, "a whole chunk", join(header, append(append([][]byte{}, cs...), cs[1])...), backup.ErrCorrupt)
	})
	t.Run("a wrong encryption context", func(t *testing.T) {
		for name, edit := range map[string]func(*backup.HeaderFields){
			"another object": func(h *backup.HeaderFields) { h.Context["ref"] = "db/20200101T000000Z-00000000.mlbk" },
			"another env":    func(h *backup.HeaderFields) { h.Context["env"] = "dev" },
			"an extra pair":  func(h *backup.HeaderFields) { h.Context["host"] = "elsewhere" },
			"no object":      func(h *backup.HeaderFields) { delete(h.Context, "ref") },
			"no env":         func(h *backup.HeaderFields) { delete(h.Context, "env") },
		} {
			b, err := backup.RewriteHeader(obj, edit)
			if err != nil {
				t.Fatal(err)
			}
			f.refused(t, name, b, backuptest.ErrRefused)
		}
	})
	t.Run("another key named", func(t *testing.T) {
		other := strings.Replace(backuptest.KeyARN, "0001", "0002", 1)
		b, err := backup.RewriteHeader(obj, func(h *backup.HeaderFields) { h.KeyARN = other })
		if err != nil {
			t.Fatal(err)
		}
		f.refused(t, "the pin", b, backup.ErrWrongKey)
		// Trusting the key the header now names, KMS is asked for it, and
		// refuses a blob that key never wrapped.
		f.refusedUnder(t, "KMS", b, other, backuptest.ErrRefused)
	})
	t.Run("any header byte changed", func(t *testing.T) {
		for i := range header {
			b := bytes.Clone(obj)
			b[i] ^= 0x20
			f.refused(t, "header byte", b, nil)
		}
	})
}

func TestAHeaderRewrappedUnderAnotherContextIsStillRefused(t *testing.T) {
	// Even with a wrapped key KMS accepts for the new context, the chunks
	// were sealed with the original header as associated data.
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	_, fields, err := backup.SplitHeader(obj)
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.kms.Decrypt(context.Background(), fields.KeyARN, fields.Wrapped, fields.Context)
	if err != nil {
		t.Fatal(err)
	}
	b, err := backup.RewriteHeader(obj, func(h *backup.HeaderFields) {
		h.Context["ref"] = "db/20200101T000000Z-00000000.mlbk"
		h.Wrapped = f.kms.Wrap(key, h.Context)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.refused(t, "rewrapped", b, backup.ErrCorrupt)
}

func TestRestoreRefusesToOverwrite(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	out := filepath.Join(t.TempDir(), "mail.db")
	if err := os.WriteFile(out, []byte("the live database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.restore(obj, out); !errors.Is(err, backup.ErrOutputExists) {
		t.Fatalf("err = %v, want ErrOutputExists", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "the live database" {
		t.Fatalf("the existing file now holds %q", b)
	}
	if f.kms.Decrypts() != 0 {
		t.Fatal("KMS was asked before the output was checked")
	}
	// A symlink at the destination is a file there too.
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.restore(obj, link); !errors.Is(err, backup.ErrOutputExists) {
		t.Fatalf("over a dangling symlink: err = %v, want ErrOutputExists", err)
	}
}

// appearing is a backup that, once its header has been read, makes a file
// appear at the restore's destination: the race the final link closes.
type appearing struct {
	r    io.Reader
	out  string
	done bool
}

func (a *appearing) Read(p []byte) (int, error) {
	if !a.done {
		a.done = true
		if err := os.WriteFile(a.out, []byte("arrived meanwhile"), 0o600); err != nil {
			return 0, err
		}
	}
	return a.r.Read(p)
}

func TestRestoreRefusesAFileThatAppearsWhileItRuns(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	dir := t.TempDir()
	out := filepath.Join(dir, "mail.db")
	_, err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: &appearing{r: bytes.NewReader(obj), out: out}, KMS: f.kms, KMSKeyARN: backuptest.KeyARN, Out: out,
	})
	if !errors.Is(err, backup.ErrOutputExists) {
		t.Fatalf("err = %v, want ErrOutputExists", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "arrived meanwhile" {
		t.Fatalf("the file that appeared now holds %d bytes of something else", len(b))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the restore left %d files beside the output", len(entries)-1)
	}
}

func TestARestoreThatDecryptsToADamagedDatabaseIsRefused(t *testing.T) {
	// Sealed correctly under a real data key: only the integrity check
	// stands between this plaintext and the destination.
	path, _ := liveDatabase(t)
	f := newFixture(t)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := backup.Snapshot(context.Background(), path, snap); err != nil {
		t.Fatal(err)
	}
	breakSnapshot(t, snap)
	damaged, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, 50_000)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}

	for name, plain := range map[string][]byte{"an index out of step": damaged, "not a database": noise} {
		encCtx := backup.EncryptionContext("prod", "db/20261001T000000Z-0000000a.mlbk")
		dk, err := f.kms.GenerateDataKey(context.Background(), backuptest.KeyARN, encCtx)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := backup.Seal(plain, dk.Plaintext, backup.HeaderFields{
			ChunkSize: smallChunks, KeyARN: dk.KeyARN, Wrapped: dk.Wrapped, Context: encCtx,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.refused(t, name, obj, backup.ErrDamagedDatabase)
	}
}

func TestARestoreFromS3RefusesAnObjectStoredUnderAnotherName(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	res, obj := f.backup(t, path)
	ctx := context.Background()

	// The object as it was written restores, named as it was written.
	body, err := f.bucket.GetObject(ctx, bucketName, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Restore(ctx, backup.RestoreOptions{
		Source: body, Object: res.Key, KMS: f.kms, KMSKeyARN: backuptest.KeyARN,
		Out: filepath.Join(t.TempDir(), "ok.db"),
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The same bytes under a newer name are not the newer backup.
	_, err = backup.Restore(ctx, backup.RestoreOptions{
		Source: bytes.NewReader(obj), Object: "db/20991231T235959Z-ffffffff.mlbk", KMS: f.kms,
		KMSKeyARN: backuptest.KeyARN, Out: filepath.Join(t.TempDir(), "renamed.db"),
	})
	if !errors.Is(err, backup.ErrCorrupt) || !strings.Contains(err.Error(), res.Key) {
		t.Fatalf("err = %v, want a refusal naming the object it was written as", err)
	}
}

func TestABackupUnderAnotherServicePurposeOrTheFirstDesignsContextIsRefused(t *testing.T) {
	// Each sealed exactly as Run seals, under a data key the fake KMS really
	// wrapped with that context: only the restore's check of the context
	// stands between it and KMS.
	path, _ := liveDatabase(t)
	f := newFixture(t)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := backup.Snapshot(context.Background(), path, snap); err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "db/20261001T000000Z-0000000b.mlbk"
	for name, encCtx := range map[string]map[string]string{
		// What this package wrote before its context had four pairs.
		"the first design's {purpose, object}": {"purpose": "mailie-db-backup", "object": ref},
		"another service":                      {"service": "wappie", "env": "prod", "purpose": "db-backup", "ref": ref},
		"another purpose":                      {"service": "mailie", "env": "prod", "purpose": "credentials", "ref": ref},
		"no service":                           {"env": "prod", "purpose": "db-backup", "ref": ref},
	} {
		dk, err := f.kms.GenerateDataKey(context.Background(), backuptest.KeyARN, encCtx)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := backup.Seal(plain, dk.Plaintext, backup.HeaderFields{
			ChunkSize: smallChunks, KeyARN: dk.KeyARN, Wrapped: dk.Wrapped, Context: encCtx,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.refused(t, name, obj, backup.ErrNotABackup)
		// Fetched from S3 under the name it gives, it is refused all the same.
		_, err = backup.Restore(context.Background(), backup.RestoreOptions{
			Source: bytes.NewReader(obj), Object: ref, KMS: f.kms, KMSKeyARN: backuptest.KeyARN,
			Out: filepath.Join(t.TempDir(), "mail.db"),
		})
		if !errors.Is(err, backup.ErrNotABackup) {
			t.Errorf("%s, fetched as %s: err = %v, want ErrNotABackup", name, ref, err)
		}
	}
	if n := f.kms.Decrypts(); n != 0 {
		t.Fatalf("KMS was asked to unwrap %d data keys of backups the restore refuses", n)
	}
}

func TestARestorePinnedToAKeyRefusesABackupUnderAnother(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	_, err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: bytes.NewReader(obj), KMS: f.kms, Out: filepath.Join(t.TempDir(), "x.db"),
		KMSKeyARN: strings.Replace(backuptest.KeyARN, "0001", "0002", 1),
	})
	if !errors.Is(err, backup.ErrWrongKey) {
		t.Fatalf("err = %v, want ErrWrongKey", err)
	}
	if f.kms.Decrypts() != 0 {
		t.Fatal("KMS was asked to unwrap a key the restore was told not to trust")
	}
}

func TestARestoreTrustsOnlyTheKeyItIsGivenNeverTheOneTheBackupNames(t *testing.T) {
	// A backup sealed in another account, under a key whose policy lets
	// anyone decrypt: every chunk authenticates under it, and the header
	// names it. Only the restore's own pin tells it from a Mailie backup.
	path, _ := liveDatabase(t)
	const foreignARN = "arn:aws:kms:us-east-2:444455556666:key/00000000-0000-4000-8000-00000000000f"
	foreign := backuptest.NewKMS(foreignARN)
	f := &fixture{kms: foreign, bucket: backuptest.NewBucket(), tmp: t.TempDir()}
	opts := f.options(path)
	opts.KMSKeyARN = foreignARN
	res, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	obj := f.bucket.Object(bucketName, res.Key)

	for name, pin := range map[string]string{"no pin": "", "the Mailie key": backuptest.KeyARN} {
		dir := t.TempDir()
		_, err := backup.Restore(context.Background(), backup.RestoreOptions{
			Source: bytes.NewReader(obj), KMS: foreign, KMSKeyARN: pin, Out: filepath.Join(dir, "mail.db"),
		})
		if err == nil {
			t.Errorf("%s: restored a backup sealed under %s", name, foreignARN)
		}
		emptyDir(t, dir)
	}
	if foreign.Decrypts() != 0 {
		t.Fatal("the foreign key was asked to unwrap a data key")
	}
	// The same backup restores when the restore is told to trust its key:
	// the refusals above are the pin's.
	if _, err := backup.Restore(context.Background(), backup.RestoreOptions{
		Source: bytes.NewReader(obj), KMS: foreign, KMSKeyARN: foreignARN, Out: filepath.Join(t.TempDir(), "mail.db"),
	}); err != nil {
		t.Fatalf("pinned to its own key: %v", err)
	}
}

// panicking is a backup whose reader panics once after bytes have been read,
// as a broken download might, after noting what the restore had written
// beside the output by then.
type panicking struct {
	r     io.Reader
	after int
	read  int
	dir   string
	seen  map[string]int64
}

func (p *panicking) Read(b []byte) (int, error) {
	if p.read >= p.after {
		entries, err := os.ReadDir(p.dir)
		if err != nil {
			return 0, err
		}
		p.seen = map[string]int64{}
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				p.seen[e.Name()] = info.Size()
			}
		}
		panic("the backup's reader panicked")
	}
	n, err := p.r.Read(b[:min(len(b), p.after-p.read)])
	p.read += n
	return n, err
}

func TestARestoreThatPanicsLeavesNoPlaintextBehind(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	header, cs := chunks(t, obj)
	dir := t.TempDir()
	src := &panicking{r: bytes.NewReader(obj), after: len(header) + 4*len(cs[0]), dir: dir}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic did not propagate")
			}
		}()
		//nolint:errcheck // it panics
		_, _ = backup.Restore(context.Background(), backup.RestoreOptions{
			Source: src, KMS: f.kms, KMSKeyARN: backuptest.KeyARN, Out: filepath.Join(dir, "mail.db"),
		})
	}()
	// Otherwise "nothing is left" would also pass if nothing was written.
	wrote := false
	for name, size := range src.seen {
		wrote = wrote || (strings.HasPrefix(name, ".mailie-restore-") && size > 0)
	}
	if !wrote {
		t.Fatalf("when the reader panicked the directory held %v, want a temporary file with plaintext in it", src.seen)
	}
	emptyDir(t, dir)
}

func TestARestoreIntoADirectoryItCannotWriteFailsBeforeAskingKMS(t *testing.T) {
	path, _ := liveDatabase(t)
	f := newFixture(t)
	_, obj := f.backup(t, path)
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		//nolint:errcheck // best effort, so TempDir can remove it
		_ = os.Chmod(readOnly, 0o700)
	})
	outs := map[string]string{"a missing directory": filepath.Join(t.TempDir(), "restore", "mail.db")}
	if os.Geteuid() != 0 {
		outs["a read-only directory"] = filepath.Join(readOnly, "mail.db")
	}
	for name, out := range outs {
		if _, err := f.restore(obj, out); err == nil {
			t.Errorf("%s: restored", name)
		}
	}
	if n := f.kms.Decrypts(); n != 0 {
		t.Fatalf("KMS was asked to unwrap %d data keys for restores that could not write their output", n)
	}
}
