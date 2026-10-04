package lockfile_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/thehappieco/mailie/internal/lockfile"
)

func TestASecondDaemonCannotTakeTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailserver.lock")

	first, err := lockfile.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = first.Release() }()

	// Two daemons over one database would drive the same accounts from two
	// sets of IMAP sessions and disagree about every UID watermark.
	if _, err := lockfile.Acquire(path); !errors.Is(err, lockfile.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
}

func TestTheLockIsAvailableAgainAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailserver.lock")

	first, err := lockfile.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := lockfile.Acquire(path)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestTheLockFileNamesItsHolder(t *testing.T) {
	// So an operator reading the data directory knows which process to look
	// at, without the pid ever being what enforces the exclusion.
	path := filepath.Join(t.TempDir(), "mailserver.lock")
	l, err := lockfile.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = l.Release() }()

	if pid, ok := lockfile.Holder(path); !ok || pid <= 0 {
		t.Fatalf("Holder = %d, %v", pid, ok)
	}
}

func TestReleasingTwiceIsHarmless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailserver.lock")
	l, err := lockfile.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}
