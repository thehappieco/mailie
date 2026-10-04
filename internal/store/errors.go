package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLite reports what went wrong through extended result codes. The interesting
// ones here are the unique-constraint violation — which is how an idempotent
// upsert and the send reservation both detect "somebody got there first" —
// and the busy codes, which tell a writer to retry rather than to give up.

// IsUnique reports whether err is a UNIQUE constraint violation.
func IsUnique(err error) bool {
	return hasCode(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

// IsForeignKey reports whether err is a foreign-key violation.
func IsForeignKey(err error) bool { return hasCode(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY) }

// IsBusy reports whether err is a lock conflict, including
// SQLITE_BUSY_SNAPSHOT. Seeing one of these on the writer pool means a
// transaction was opened without the immediate lock; the fix is the call site,
// not a retry loop.
func IsBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// IsReadOnly reports whether err is a write attempted on the read-only pool.
func IsReadOnly(err error) bool { return hasCode(err, sqlite3.SQLITE_READONLY) }

func hasCode(err error, codes ...int) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range codes {
		if e.Code() == c {
			return true
		}
	}
	return false
}
