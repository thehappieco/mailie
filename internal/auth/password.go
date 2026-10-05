package auth

import (
	"context"
	"errors"
	"unicode/utf8"
)

// Passwords are hashed here, on the server. Wappie derives a key in the
// browser first, but only because its archive is end-to-end encrypted and the
// password has to unwrap something the server must never see. Nothing here is
// like that: this daemon holds every mailbox credential anyway, so a
// client-side derivation would add a second implementation of the same hash
// and protect nothing. The password travels over TLS and stops here.

// passwordParams are for passwords a person chose, which is the opposite case
// from an API key: a dictionary exists, so the cost has to be real. 64 MiB and
// three passes is what Wappie pays in the browser, and OWASP's first
// recommendation for Argon2id.
var passwordParams = argonParams{time: 3, memory: 64 * 1024, threads: 1, keyLen: 32}

// Password length limits. The minimum is the console's; the maximum exists so
// that a sign-in cannot ask the server to hash a megabyte.
const (
	MinPasswordLength = 10   // characters
	MaxPasswordBytes  = 1024 // bytes
)

// maxConcurrentHashes bounds how much memory password hashing can hold. At
// 64 MiB each, an unbounded burst of sign-ins is a memory-exhaustion attack
// that the rate limiter only slows down; two at a time keeps the worst case at
// 128 MiB whatever arrives. maxHashWaiters bounds the queue behind them: at a
// few hundred milliseconds a hash, eight waiting is a second or two, and a
// ninth is told to come back rather than queued behind an attack for the
// whole of its route's deadline.
const (
	maxConcurrentHashes = 2
	maxHashWaiters      = 8
)

var hashSlots = newSlots(maxConcurrentHashes, maxHashWaiters)

var (
	// ErrPasswordTooShort is a new password under MinPasswordLength.
	ErrPasswordTooShort = errors.New("auth: a password needs at least 10 characters")
	// ErrPasswordTooLong is a new password over MaxPasswordBytes.
	ErrPasswordTooLong = errors.New("auth: a password may be at most 1024 bytes")
	// ErrPasswordNotUTF8 is a new password that is not valid UTF-8.
	ErrPasswordNotUTF8 = errors.New("auth: a password must be valid UTF-8 text")
)

// CheckPassword applies the rules a new password must meet.
//
// It has to be valid UTF-8 because that is all a sign-in can ever present: the
// console sends UTF-8, and a JSON body's invalid bytes arrive as U+FFFD. Bytes
// in another encoding — a password piped from a Latin-1 file to `user
// password`, or typed at a terminal that is not set to UTF-8 — would hash to
// something no sign-in reproduces.
func CheckPassword(password string) error {
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	if !utf8.ValidString(password) {
		return ErrPasswordNotUTF8
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	return nil
}

// hashPassword derives the stored PHC string for a new password.
func hashPassword(ctx context.Context, password string) (string, error) {
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return hashPHC(password, passwordParams)
}

// verifyPassword checks a password against a stored PHC string, or against
// dummyPasswordHash when there is nothing real to compare with. Either way it
// costs one full derivation, which is the point.
//
// An empty phc is a person who has no password (they sign in through an
// identity provider, SignInExternal): nothing matches it, whatever is
// presented, the empty password included, and finding that out costs the
// derivation a wrong password costs, so the refusal says nothing about how
// the person signs in.
func verifyPassword(ctx context.Context, password, phc string) (bool, error) {
	if len(password) > MaxPasswordBytes {
		// Never a password anyone could have set. Refusing it without
		// hashing says nothing about the account, since the length is the
		// caller's own choice.
		return false, nil
	}
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if phc == "" {
		_ = verifySecret(password, dummyPasswordHash)
		return false, nil
	}
	return verifySecret(password, phc), nil
}

// acquireHashSlot waits for one of the hashing slots, or for the caller to
// give up, or refuses with ErrHashBusy when the queue is already full. A
// request whose client has gone should not keep a place in the queue it is no
// longer waiting on.
func acquireHashSlot(ctx context.Context) (func(), error) { return hashSlots.acquire(ctx) }

// dummyPasswordHash is what an unknown address, a disabled account or a
// person with no password is checked against, so that a miss costs the same
// derivation as a wrong password and the response time does not say which it
// was.
//
// A constant rather than a hash computed at start-up: verification reads the
// cost from the string, so a fixed salt and a digest nothing hashes to cost
// exactly what a real check costs, without every CLI invocation paying 64 MiB
// to build one.
var dummyPasswordHash = formatPHC(passwordParams,
	[]byte("mailie-dummy-sal"), []byte("no password hashes to this value"))
