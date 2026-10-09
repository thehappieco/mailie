package auth

import (
	"context"
)

// A person's password never reaches the server: their browser derives an auth
// key from it with Argon2id at 64 MiB (docs/key-scheme.md section 5), and the
// server keeps a hash of the auth key's text, the auth verifier, as it keeps a
// hash of an API key's secret, and a hash of the recovery proof beside it.
// What it hashes is already the output of a 64 MiB Argon2id of the password,
// and the password wrap kept beside it is an equally good offline oracle, so
// its own hash adds no cost to a guess: its job is to stop a copy of the
// database from being replayed as an auth key. It is keyParams', 19 MiB, read
// back from the stored string so it can be raised.
//
// One password is still checked here, once: that of a person who signed up
// before the key scheme and has not signed in since, at their upgrade
// (LegacySignIn), against the hash made when they chose it. Their old hash is
// cleared in the transaction that enrols them, and nothing makes a new one.
// That path exists in the release that brings the scheme only.

// passwordParams are what a password was hashed at before the key scheme: 64
// MiB and three passes, Wappie's cost in the browser and OWASP's first
// recommendation for Argon2id. Nothing hashes a password any more; a legacy
// check reads the parameters back from the stored hash, and the dummy below
// costs what such a check costs.
var passwordParams = argonParams{time: 3, memory: 64 * 1024, threads: 1, keyLen: 32}

// MaxPasswordBytes bounds a password the upgrade checks, as it bounded every
// password before: a sign-in cannot ask the server to hash a megabyte.
const MaxPasswordBytes = 1024 // bytes

// maxConcurrentHashes bounds how much memory checking people's secrets can
// hold: a legacy password at 64 MiB, an auth key or a recovery proof at 19
// MiB. An unbounded burst of sign-ins is a memory-exhaustion attack that the
// rate limiter only slows down; two at a time keeps the worst case at 128 MiB
// whatever arrives. maxHashWaiters bounds the queue behind them: at a few
// hundred milliseconds a hash, eight waiting is a second or two, and a ninth
// is told to come back rather than queued behind an attack for the whole of
// its route's deadline.
const (
	maxConcurrentHashes = 2
	maxHashWaiters      = 8
)

var hashSlots = newSlots(maxConcurrentHashes, maxHashWaiters)

// verifyPassword checks a password against a stored PHC string, or against
// dummyPasswordHash when there is nothing real to compare with. Either way it
// costs one full derivation, which is the point.
//
// An empty phc is a person who has no password the server checks: nothing
// matches it, whatever is presented, the empty password included, and
// finding that out costs the derivation a wrong password costs, so the
// refusal says nothing about how the person signs in.
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

// hashPersonSecret hashes an auth key's or a recovery proof's text for
// storing, in one of the hashing slots.
func hashPersonSecret(ctx context.Context, secret string) (string, error) {
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return hashPHC(secret, keyParams)
}

// verifyPersonSecret checks an auth key's or a recovery proof's text against
// a stored verifier, or against dummyVerifier when phc is empty: an unknown
// address, a disabled person, a person not enrolled. Either way it costs one
// full derivation, and nothing matches the dummy.
func verifyPersonSecret(ctx context.Context, secret, phc string) (bool, error) {
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if phc == "" {
		_ = verifySecret(secret, dummyVerifier)
		return false, nil
	}
	return verifySecret(secret, phc), nil
}

// acquireHashSlot waits for one of the hashing slots, or for the caller to
// give up, or refuses with ErrHashBusy when the queue is already full. A
// request whose client has gone should not keep a place in the queue it is no
// longer waiting on.
func acquireHashSlot(ctx context.Context) (func(), error) { return hashSlots.acquire(ctx) }

// dummyPasswordHash is what the upgrade checks a password against for an
// unknown address, a disabled account, an enrolled person or a person with
// no password, so that a miss costs the same derivation as a wrong password
// and the response time does not say which it was.
//
// A constant rather than a hash computed at start-up: verification reads the
// cost from the string, so a fixed salt and a digest nothing hashes to cost
// exactly what a real check costs, without every CLI invocation paying 64 MiB
// to build one.
var dummyPasswordHash = formatPHC(passwordParams,
	[]byte("mailie-dummy-sal"), []byte("no password hashes to this value"))

// dummyVerifier is dummyPasswordHash for an auth key or a recovery proof: at
// a verifier's cost.
var dummyVerifier = formatPHC(keyParams,
	[]byte("mailie-dummy-ver"), []byte("no auth key hashes to this value"))
