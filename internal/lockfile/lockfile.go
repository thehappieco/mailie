// Package lockfile marks a running daemon.
//
// Two daemons over one database would each open their own IMAP sessions,
// advance the same UID watermarks and hand out the same message ids for
// different rows. SQLite would keep the file consistent and the mailbox index
// would still be wrong, so the exclusion has to happen a level up.
//
// An advisory lock on a file rather than a PID file: the kernel releases it
// when the process dies, so a daemon killed with SIGKILL does not leave a
// stale marker that a human has to clear before the service can start again.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("lockfile: already held by another process")

// Lock is a held advisory lock.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes the lock, or returns ErrLocked.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("lockfile: create directory: %w", err)
	}
	//nolint:gosec // G304: the path comes from configuration, not from a request
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lockfile: open %s: %w", path, err)
	}
	if err := tryLock(f); err != nil {
		//nolint:errcheck // the open failed to become a lock; the close error adds nothing
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lockfile: lock %s: %w", path, err)
	}

	// The pid is for a human reading the directory; the lock itself is what
	// enforces anything.
	if err := f.Truncate(0); err == nil {
		// Best effort: the pid is a courtesy to whoever reads the directory.
		// The lock is what excludes, and it is already held.
		//nolint:errcheck // advisory content, not the guarantee
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f, path: path}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	if err != nil {
		return fmt.Errorf("lockfile: release %s: %w", l.path, err)
	}
	return nil
}

// Path is the file the lock is held on.
func (l *Lock) Path() string { return l.path }

// Holder reports the pid recorded in the lock file, for an error message that
// tells the operator which process to look at. It is advisory: the pid may
// already be gone, which is why the lock and not this is what excludes.
func Holder(path string) (int, bool) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: configured path
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(trimSpace(body)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
