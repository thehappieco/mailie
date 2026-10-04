package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// An API key is "<prefix>.<secret>": eight hex characters, a dot, and 32 random
// bytes in base64url.
//
// The split matters. The prefix is a lookup selector, not a secret: it is
// indexed, it appears in logs so a line can be traced to a caller, and it is
// what lets verification find the one row to compare against instead of
// hashing every key in the table. The secret is the only part that
// authenticates, and only its Argon2id hash is ever stored.
const (
	prefixLen = 8  // hex characters
	secretLen = 32 // raw bytes, base64url-encoded to 43 characters
)

// argonParams are the Argon2id cost factors a hash is made with. Verification
// never uses these: it reads them back out of the stored PHC string.
type argonParams struct {
	time    uint32
	memory  uint32 // KiB
	threads uint8
	keyLen  uint32
}

// keyParams are for API keys. Deliberately at the low end of what OWASP
// suggests: the input is 32 bytes from crypto/rand, not a password a human
// chose, so there is no dictionary to slow an attacker down through. What the
// hash buys is that a stolen database does not hand over working keys.
var keyParams = argonParams{time: 1, memory: 19 * 1024, threads: 1, keyLen: 32}

const saltLen = 16

// argon2id is Argon2id. A variable only so a test can observe how many
// derivations run at once; nothing in the daemon replaces it.
var argon2id = argon2.IDKey

// ErrInvalidKey is returned for every authentication failure.
//
// One error for all of them, on purpose. Distinguishing "no such prefix" from
// "wrong secret" from "revoked" from "expired" would hand a caller an oracle
// for enumerating which keys exist.
var ErrInvalidKey = errors.New("auth: invalid api key")

// Generate returns a new key and the PHC hash to store for it.
func Generate() (key, prefix, hash string, err error) {
	prefixBytes := make([]byte, prefixLen/2)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", "", "", fmt.Errorf("auth: read random: %w", err)
	}
	secret := make([]byte, secretLen)
	if _, err := rand.Read(secret); err != nil {
		return "", "", "", fmt.Errorf("auth: read random: %w", err)
	}

	prefix = hex.EncodeToString(prefixBytes)
	secretStr := base64.RawURLEncoding.EncodeToString(secret)
	hash, err = hashSecret(secretStr)
	if err != nil {
		return "", "", "", err
	}
	return prefix + "." + secretStr, prefix, hash, nil
}

// SplitKey separates a presented key into its prefix and secret.
func SplitKey(presented string) (prefix, secret string, ok bool) {
	prefix, secret, ok = strings.Cut(presented, ".")
	if !ok || len(prefix) != prefixLen || secret == "" {
		return "", "", false
	}
	if _, err := hex.DecodeString(prefix); err != nil {
		return "", "", false
	}
	return prefix, secret, true
}

// hashSecret derives the stored PHC string for a key's secret.
func hashSecret(secret string) (string, error) { return hashPHC(secret, keyParams) }

// hashPHC derives a PHC string under the given cost factors.
func hashPHC(secret string, p argonParams) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	digest := argon2id([]byte(secret), salt, p.time, p.memory, p.threads, p.keyLen)
	return formatPHC(p, salt, digest), nil
}

func formatPHC(p argonParams, salt, digest []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	)
}

// verifySecret checks a secret against a stored PHC string.
//
// The cost parameters are read from the stored hash rather than from the
// constants above, so raising them later does not invalidate keys that were
// issued under the old ones.
func verifySecret(secret, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	// A length taken from the stored hash, so a future parameter change does
	// not turn into a silent mismatch.
	//nolint:gosec // G115: the digest length is bounded by the stored hash
	got := argon2id([]byte(secret), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is what an unknown prefix is compared against.
//
// Without it, a miss would return as soon as the row lookup failed while a hit
// paid for an Argon2id derivation, and the difference — tens of milliseconds —
// is a free oracle for enumerating valid prefixes.
var dummyHash = mustHash("mQ8fVn0zJ7pR2tYxA4cLbE6hKdG1sWnO")

func mustHash(s string) string {
	h, err := hashSecret(s)
	if err != nil {
		panic("auth: cannot hash the timing-equalisation value: " + err.Error())
	}
	return h
}
