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
// No password in clear is checked here any more. The upgrade of a person
// who signed up before the key scheme (docs/key-scheme.md section 12.7)
// checked theirs, once, against the hash made when they chose it, in the
// release that brought the scheme only; it left in the next. A person who
// never enrolled keeps that hash in users.password_hash, which no sign-in
// checks now (it only still says they have a password, User.HasPassword) and
// the reset invitation clears: the reset is their way back.

// maxConcurrentHashes bounds how much memory checking people's secrets can
// hold: an auth key or a recovery proof at 19 MiB. An unbounded burst of
// sign-ins is a memory-exhaustion attack that the rate limiter only slows
// down; two at a time keeps the worst case at 38 MiB whatever arrives.
// maxHashWaiters bounds the queue behind them: at a few hundred milliseconds
// a hash, eight waiting is a second or two, and a ninth is told to come back
// rather than queued behind an attack for the whole of its route's deadline.
const (
	maxConcurrentHashes = 2
	maxHashWaiters      = 8
)

var hashSlots = newSlots(maxConcurrentHashes, maxHashWaiters)

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

// dummyVerifier is what an auth key or a recovery proof is checked against
// for an unknown address, a disabled person, a person with no password and a
// person not enrolled, so that a miss costs the same derivation as a wrong
// secret and the response time does not say which it was.
//
// A constant rather than a hash computed at start-up: verification reads the
// cost from the string, so a fixed salt and a digest nothing hashes to cost
// exactly what a real check costs, without every CLI invocation paying for
// one.
var dummyVerifier = formatPHC(keyParams,
	[]byte("mailie-dummy-ver"), []byte("no auth key hashes to this value"))
