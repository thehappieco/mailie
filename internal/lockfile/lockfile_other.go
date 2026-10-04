//go:build !unix

package lockfile

import (
	"errors"
	"os"
)

// Platforms without flock get no exclusion. Refusing to start would be worse
// than the risk on a platform this daemon is not deployed to, but the caller
// should know it is unguarded.
var errUnsupported = errors.New("lockfile: advisory locking is not supported on this platform")

func tryLock(*os.File) error { return errUnsupported }
func unlock(*os.File) error  { return nil }
