// Package backup takes encrypted copies of the database off the host, and
// brings them back.
//
// A backup is a consistent snapshot of the live database (snapshot.go),
// checked with PRAGMA integrity_check, sealed in fixed-size chunks under a
// data key that AWS KMS generates for that one backup (format.go), and put
// in S3 as a new object. The design asks the host for exactly that much: its
// identity needs to generate data keys with the backup's encryption context
// and to put new objects under db/, and nothing else — it never lists, reads,
// deletes or decrypts a backup, its own included, so a compromised host
// cannot read or destroy the copies it made. Unwrapping a data key is for a
// separate principal, so a restore runs where that principal signs in and
// where the deployment's data-protection rules allow a copy of the database
// in clear. It needs nothing from the daemon's configuration: the backup
// carries its wrapped data key and its encryption context. It names its key
// too, but that is unauthenticated until KMS has answered, so the restore is
// told which key to trust instead. docs/backup.md has the IAM shape.
//
// KMS and S3 are behind the small interfaces below, so everything but the two
// SDK calls is tested offline; aws.go is the SDK side.
package backup

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Service and Purpose are two of the four pairs of every backup's encryption
// context, {service, env, purpose, ref}: a key policy can let the host
// generate data keys only with service=mailie and purpose=db-backup, so a
// key shared with other uses still gives the host nothing else.
const (
	Service = "mailie"
	Purpose = "db-backup"
)

// Encryption context keys. "env" is the deployment, MAIL_ENV, which a key
// policy can require to be "prod" for the host. "ref" is the S3 key the
// backup is written as, db/<time>-<8 random hex>.mlbk, which carries no
// personal data: KMS records the context of every call in CloudTrail, so each
// Decrypt there says which backup it opened, and a restore from S3 checks
// that the object it fetched is the one the backup says it is.
const (
	contextService = "service"
	contextEnv     = "env"
	contextPurpose = "purpose"
	contextRef     = "ref"
)

// EncryptionContext is the context of the data key of a backup taken in env
// and written as the object ref: exactly these four pairs.
func EncryptionContext(env, ref string) map[string]string {
	return map[string]string{contextService: Service, contextEnv: env, contextPurpose: Purpose, contextRef: ref}
}

// KMS is the part of AWS KMS a backup and a restore use.
type KMS interface {
	// GenerateDataKey returns a fresh AES-256 key, in plaintext and wrapped
	// under keyARN with the encryption context encCtx.
	GenerateDataKey(ctx context.Context, keyARN string, encCtx map[string]string) (DataKey, error)
	// Decrypt unwraps a data key. It fails unless keyARN wrapped it with
	// exactly encCtx.
	Decrypt(ctx context.Context, keyARN string, wrapped []byte, encCtx map[string]string) ([]byte, error)
}

// DataKey is what GenerateDataKey returns.
type DataKey struct {
	// KeyARN is the key that wrapped it, as KMS reports it.
	KeyARN string
	// Plaintext is the key itself. Whoever receives it zeroes it.
	Plaintext []byte
	// Wrapped is the KMS CiphertextBlob.
	Wrapped []byte
}

// Uploader puts one new object. The host needs no other permission, so an
// implementation must not depend on reading, listing or aborting anything.
type Uploader interface {
	PutObject(ctx context.Context, in PutInput) error
}

// PutInput is one upload.
type PutInput struct {
	Bucket string
	Key    string
	Body   io.ReadSeeker
	Size   int64
	// SHA256 is the digest of Body, which S3 checks on arrival.
	SHA256 []byte
}

// Downloader reads one object, where a restore runs.
type Downloader interface {
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
}

// Options configure Run.
type Options struct {
	// DatabasePath is the live database.
	DatabasePath string
	// Env is the deployment the backup is taken in, written into the
	// encryption context: MAIL_ENV, "prod" in production.
	Env       string
	Bucket    string
	KMSKeyARN string
	KMS       KMS
	Uploader  Uploader
	// TempDir is where the private working directory is made. Empty is
	// os.TempDir(), which a systemd unit's PrivateTmp makes its own.
	TempDir string
	// ChunkSize overrides DefaultChunkSize; tests use small chunks so a
	// backup spans many.
	ChunkSize int
	Logger    *slog.Logger
	Now       func() time.Time
	// Rand overrides crypto/rand, for the object name and the nonce prefix.
	Rand io.Reader
}

// Result describes an uploaded backup. Nothing in it comes from the
// database's contents.
type Result struct {
	Bucket           string
	Key              string
	PlaintextBytes   int64
	CiphertextBytes  int64
	CiphertextSHA256 string
	Duration         time.Duration
	Inspection
}

// afterSnapshot runs between the snapshot and its integrity check. Tests set
// it to damage the snapshot.
var afterSnapshot func(path string)

// Run takes one backup: snapshot, check, encrypt, upload.
//
// Everything it writes goes to a new 0700 directory, removed on the way out
// whatever the outcome — an error, a cancelled context, a panic. The
// plaintext snapshot is removed as soon as it is encrypted. The one exit no
// defer covers is SIGKILL; run under systemd with PrivateTmp, the directory
// is discarded when the unit stops, however it stopped.
func Run(ctx context.Context, opts Options) (res Result, err error) {
	if opts.DatabasePath == "" || opts.Env == "" || opts.Bucket == "" || opts.KMSKeyARN == "" ||
		opts.KMS == nil || opts.Uploader == nil {
		return res, errors.New("backup: incomplete options")
	}
	now, random, logger := opts.Now, opts.Rand, opts.Logger
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	if logger == nil {
		logger = slog.Default()
	}
	chunkSize := opts.ChunkSize
	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}
	start := now()

	dir, err := os.MkdirTemp(opts.TempDir, "mailie-backup-")
	if err != nil {
		return res, fmt.Errorf("backup: working directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("backup: remove the working directory %s: %w", dir, rerr))
		}
	}()

	plainPath := filepath.Join(dir, "snapshot.db")
	if err := snapshot(ctx, opts.DatabasePath, plainPath); err != nil {
		return res, err
	}
	if afterSnapshot != nil {
		afterSnapshot(plainPath)
	}
	if res.Inspection, err = inspect(ctx, plainPath); err != nil {
		return res, err
	}

	// Named for the moment before the snapshot began, not for now: the name
	// is the only date a backup carries, and it is a lower bound. Nothing
	// committed before that second is missing from the backup; anything from
	// it on may be. A later name, taken after the check and the encryption,
	// would date before it a change the backup does not hold.
	key, err := objectKey(start, random)
	if err != nil {
		return res, err
	}
	h := &header{
		chunkSize: uint32(chunkSize), //nolint:gosec // G115: marshal checks the range
		context:   EncryptionContext(opts.Env, key),
	}
	if _, err := io.ReadFull(random, h.noncePrefix[:]); err != nil {
		return res, fmt.Errorf("backup: nonce prefix: %w", err)
	}

	dk, err := opts.KMS.GenerateDataKey(ctx, opts.KMSKeyARN, h.context)
	if err != nil {
		return res, fmt.Errorf("backup: generate a data key: %w", err)
	}
	// Zeroed here whatever happens below, and again right after the cipher
	// has its copy.
	defer clear(dk.Plaintext)
	if dk.KeyARN != opts.KMSKeyARN {
		return res, fmt.Errorf("backup: KMS generated the data key under %q, not the configured key", dk.KeyARN)
	}
	h.keyARN, h.wrappedKey = dk.KeyARN, dk.Wrapped
	if _, err := h.marshal(); err != nil {
		return res, err
	}
	aead, err := newAEAD(dk.Plaintext)
	clear(dk.Plaintext)
	if err != nil {
		return res, err
	}

	sealedPath := filepath.Join(dir, "backup.mlbk")
	plain, sealed, sum, err := sealFile(ctx, plainPath, sealedPath, aead, h)
	if err != nil {
		return res, err
	}
	// The plaintext has done its job; it does not wait for the upload.
	if err := os.Remove(plainPath); err != nil {
		return res, fmt.Errorf("backup: remove the snapshot: %w", err)
	}

	body, err := os.Open(sealedPath) //nolint:gosec // G304: a path this function built
	if err != nil {
		return res, fmt.Errorf("backup: reopen the backup: %w", err)
	}
	//nolint:errcheck // read-only; removed with its directory
	defer func() { _ = body.Close() }()
	if err := opts.Uploader.PutObject(ctx, PutInput{
		Bucket: opts.Bucket, Key: key, Body: body, Size: sealed, SHA256: sum,
	}); err != nil {
		return res, fmt.Errorf("backup: upload s3://%s/%s: %w", opts.Bucket, key, err)
	}

	res.Bucket, res.Key = opts.Bucket, key
	res.PlaintextBytes, res.CiphertextBytes = plain, sealed
	res.CiphertextSHA256 = hex.EncodeToString(sum)
	res.Duration = now().Sub(start)
	attrs := []any{
		"object", "s3://" + opts.Bucket + "/" + key,
		"plaintext_bytes", plain, "ciphertext_bytes", sealed, "ciphertext_sha256", res.CiphertextSHA256,
		"duration_ms", res.Duration.Milliseconds(), "schema_version", res.SchemaVersion,
	}
	for _, table := range countedTables {
		if n, ok := res.Rows[table]; ok {
			attrs = append(attrs, "rows_"+table, n)
		}
	}
	logger.InfoContext(ctx, "backup uploaded", attrs...)
	return res, nil
}

// sealFile encrypts the snapshot at src into a new 0600 file at dst and
// returns the sizes and the SHA-256 of what it wrote.
func sealFile(ctx context.Context, src, dst string, aead cipher.AEAD, h *header) (plain, sealed int64, sum []byte, err error) {
	in, err := os.Open(src) //nolint:gosec // G304: a path Run built
	if err != nil {
		return 0, 0, nil, fmt.Errorf("backup: open the snapshot: %w", err)
	}
	//nolint:errcheck // read-only
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path Run built
	if err != nil {
		return 0, 0, nil, fmt.Errorf("backup: create the backup file: %w", err)
	}
	digest := sha256.New()
	plain, sealed, err = sealStream(ctx, io.MultiWriter(out, digest), in, aead, h)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("backup: write the backup file: %w", cerr)
	}
	if err != nil {
		return 0, 0, nil, err
	}
	return plain, sealed, digest.Sum(nil), nil
}

// objectKey names a backup: db/<UTC time>-<8 random hex>.mlbk, the time
// truncated to the second, so never later than t. The time sorts a listing;
// the random part keeps two runs in the same second apart, since the upload
// refuses to replace an object.
func objectKey(t time.Time, random io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("backup: object name: %w", err)
	}
	return "db/" + t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]) + ".mlbk", nil
}
