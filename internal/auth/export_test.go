package auth

import (
	"context"
	"testing"
)

// Test seams for the password hashing, which has to be observed from outside
// without making anything in the daemon able to change it.

// KDF is Argon2id's signature.
type KDF func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte

// SetDeriveKeyForTest replaces Argon2id for the rest of the test.
func SetDeriveKeyForTest(t *testing.T, fn KDF) {
	t.Helper()
	previous := argon2id
	argon2id = fn
	t.Cleanup(func() { argon2id = previous })
}

// HashPasswordForTest hashes a new password the way sign-up does.
func HashPasswordForTest(ctx context.Context, password string) (string, error) {
	return hashPassword(ctx, password)
}

// PasswordCostForTest is the cost a new password is hashed at.
func PasswordCostForTest() (memoryKiB, passes uint32) {
	return passwordParams.memory, passwordParams.time
}

// HashWaitersForTest is how many password checks and key checks are queued
// for a hashing slot right now.
func HashWaitersForTest() (passwords, keys int) {
	return int(hashSlots.waiting.Load()), int(keySlots.waiting.Load())
}

// HashLimitsForTest is how many password and key derivations may run at once,
// and how many may wait for each.
func HashLimitsForTest() (passwords, passwordQueue, keys, keyQueue int) {
	return maxConcurrentHashes, maxHashWaiters, maxConcurrentKeyHashes, maxKeyHashWaiters
}
