package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrOutputExists is a restore asked to write over a file. A restore never
// replaces anything: the file at the destination may be the live database.
var ErrOutputExists = errors.New("backup: the output file already exists")

// ErrWrongKey is a backup that names a KMS key other than the one the
// restore was told to trust.
var ErrWrongKey = errors.New("backup: the backup names another KMS key")

// RestoreOptions configure Restore.
type RestoreOptions struct {
	// Source is the backup, from its first byte.
	Source io.Reader
	// Object is the S3 key the backup was fetched from, when it was. The
	// backup's ref must name the same object: one copied or renamed over
	// another name is refused. Empty for a local file, which whoever
	// downloaded it may have renamed.
	Object string
	// KMSKeyARN is the only key the backup may name. It is required: the
	// key ARN is part of the backup's header, which nothing authenticates
	// until KMS has unwrapped the data key, so a restore that took the
	// backup's word for it would authenticate a backup sealed under any key
	// the caller's credentials can use, in any account.
	KMSKeyARN string
	KMS       KMS
	// Out is the database file to create. It must not exist.
	Out string
}

// RestoreResult describes a restored database.
type RestoreResult struct {
	// Object and Env are the backup's ref and env, as its encryption context
	// gives them.
	Object         string
	Env            string
	KeyARN         string
	PlaintextBytes int64
	Inspection
}

// Restore decrypts a backup into a new database file at opts.Out.
//
// The plaintext goes to a 0600 temporary file in the destination's directory,
// chunk by chunk as each authenticates. Only when the last chunk has
// authenticated and the file passes PRAGMA integrity_check is it linked into
// place, which fails if something appeared at the destination meanwhile. On
// any other outcome, a panic included, the temporary file is removed: nothing
// partial is left. The one exit no defer covers is SIGKILL.
func Restore(ctx context.Context, opts RestoreOptions) (res RestoreResult, err error) {
	if opts.Source == nil || opts.KMS == nil || opts.Out == "" {
		return res, errors.New("backup: incomplete restore options")
	}
	if opts.KMSKeyARN == "" {
		return res, errors.New("backup: a restore needs the ARN of the KMS key the backup must be under")
	}
	if _, err := os.Lstat(opts.Out); err == nil {
		return res, fmt.Errorf("%w: %s", ErrOutputExists, opts.Out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("backup: check the output path: %w", err)
	}

	// The temporary file comes before anything reaches KMS: a destination
	// that cannot take it fails here, not after a Decrypt that CloudTrail
	// records.
	tmp, err := os.CreateTemp(filepath.Dir(opts.Out), ".mailie-restore-*.db")
	if err != nil {
		return res, fmt.Errorf("backup: create a temporary file beside the output: %w", err)
	}
	tmpPath := tmp.Name()
	// Keyed on committed, never on err: while a panic unwinds, err is still
	// nil, and the file holds the database in plaintext.
	committed := false
	defer func() {
		if committed {
			return
		}
		//nolint:errcheck // a second close after the explicit one below is expected to fail
		_ = tmp.Close()
		err = errors.Join(err, removeWithSidecars(tmpPath))
	}()

	h, err := readHeader(opts.Source)
	if err != nil {
		return res, err
	}
	// A file of this format with another service's or purpose's context, or
	// with the first design's context (purpose and object, no service), is
	// not one of these backups: refused before KMS is asked.
	// The rest of the context is KMS's to check, since the data key unwraps
	// only with exactly the context it was generated with.
	if s, p := h.context[contextService], h.context[contextPurpose]; s != Service || p != Purpose {
		return res, fmt.Errorf("%w: its encryption context has service %q and purpose %q, not %q and %q",
			ErrNotABackup, s, p, Service, Purpose)
	}
	res.Object, res.Env, res.KeyARN = h.context[contextRef], h.context[contextEnv], h.keyARN
	if opts.Object != "" && res.Object != opts.Object {
		return res, fmt.Errorf("%w: fetched as %q, but it was written as %q", ErrCorrupt, opts.Object, res.Object)
	}
	if h.keyARN != opts.KMSKeyARN {
		return res, fmt.Errorf("%w: %q, not the expected %q", ErrWrongKey, h.keyARN, opts.KMSKeyARN)
	}

	// The key ARN goes to KMS as well as the blob: KMS then refuses a blob
	// wrapped under any other key, rather than unwrapping whatever it names.
	key, err := opts.KMS.Decrypt(ctx, h.keyARN, h.wrappedKey, h.context)
	if err != nil {
		return res, fmt.Errorf("backup: KMS did not unwrap the data key "+
			"(the credentials, the key and the backup's encryption context must all match): %w", err)
	}
	defer clear(key)
	aead, err := newAEAD(key)
	clear(key)
	if err != nil {
		return res, err
	}

	if res.PlaintextBytes, err = openStream(ctx, tmp, opts.Source, aead, h); err != nil {
		return res, err
	}
	if err = tmp.Sync(); err != nil {
		return res, fmt.Errorf("backup: write the restored database: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return res, fmt.Errorf("backup: write the restored database: %w", err)
	}
	if res.Inspection, err = inspect(ctx, tmpPath); err != nil {
		return res, err
	}
	// A hard link fails if the name exists, where a rename would replace
	// it: the check above was a courtesy, this is the guarantee.
	if err = os.Link(tmpPath, opts.Out); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("%w: %s", ErrOutputExists, opts.Out)
			return res, err
		}
		err = fmt.Errorf("backup: put the restored database in place: %w", err)
		return res, err
	}
	if err = removeWithSidecars(tmpPath); err != nil {
		return res, err
	}
	committed = true
	syncDir(filepath.Dir(opts.Out))
	return res, nil
}

// removeWithSidecars removes the temporary file and whatever SQLite put
// beside it. The check opens it read-only and a backup is never in WAL mode,
// so there should be nothing, but a crafted one could be.
func removeWithSidecars(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("backup: remove %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// syncDir makes the new directory entry durable. Best effort: the file is
// already complete and in place, and some filesystems refuse to sync a
// directory.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the directory the operator asked to restore into
	if err != nil {
		return
	}
	//nolint:errcheck // best effort, see above
	_ = d.Sync()
	//nolint:errcheck // read-only handle
	_ = d.Close()
}
