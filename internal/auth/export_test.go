package auth

import (
	"context"
	"testing"
)

// Test seams for the hashing of people's secrets, which has to be observed
// from outside without making anything in the daemon able to change it.

// Argon2 is Argon2id's signature.
type Argon2 func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte

// SetDeriveKeyForTest replaces Argon2id for the rest of the test.
func SetDeriveKeyForTest(t *testing.T, fn Argon2) {
	t.Helper()
	previous := argon2id
	argon2id = fn
	t.Cleanup(func() { argon2id = previous })
}

// HashVerifierForTest hashes an auth key's or a proof's text the way an
// enrolment does.
func HashVerifierForTest(ctx context.Context, secret string) (string, error) {
	return hashPersonSecret(ctx, secret)
}

// VerifierCostForTest is the cost an auth key and a proof are hashed at.
func VerifierCostForTest() (memoryKiB, passes uint32) {
	return keyParams.memory, keyParams.time
}

// PasswordCostForTest is the cost a password was hashed at before the key
// scheme, which the upgrade's check costs.
func PasswordCostForTest() (memoryKiB, passes uint32) {
	return passwordParams.memory, passwordParams.time
}

// HashWaitersForTest is how many checks of people's secrets and of keys are
// queued for a hashing slot right now.
func HashWaitersForTest() (people, keys int) {
	return int(hashSlots.waiting.Load()), int(keySlots.waiting.Load())
}

// HashLimitsForTest is how many derivations of people's secrets and of keys
// may run at once, and how many may wait for each.
func HashLimitsForTest() (people, peopleQueue, keys, keyQueue int) {
	return maxConcurrentHashes, maxHashWaiters, maxConcurrentKeyHashes, maxKeyHashWaiters
}
