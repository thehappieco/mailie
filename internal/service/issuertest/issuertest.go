// Package issuertest is a key issuer for tests: an extension that makes API
// keys elsewhere, as service.KeyIssuer describes one, with a credential shape
// of its own that no real issuer has, and a way to store one of its keys in a
// database as an extension that mirrors its issuer would.
//
// It lives in its own package, as authtest does, so nothing in the daemon can
// import it. The core names no real issuer's format; these tests prove the
// seam with this one.
package issuertest

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The fake issuer's credential: Prefix, a selector of 16 lowercase hex, a
// dot, and a secret of 32 random bytes in unpadded base64url; Len bytes in
// all.
const (
	Issuer      = "https://issuer.test"
	Prefix      = "tst_"
	SelectorLen = 16
	SecretLen   = 43
	Len         = len(Prefix) + SelectorLen + 1 + SecretLen
)

// hashLabel is what a secret is hashed under, with one zero byte after it.
const hashLabel = "issuertest/secret"

// Fake is a key issuer: it claims every credential of its family, compares
// one with the hash held for it in constant time, and remembers every use it
// hears and every credential it was asked to claim.
type Fake struct {
	issuer        string
	mu            sync.Mutex
	uses          []service.IssuedKeyUse
	claimed       []string
	verifications int
}

// New is a Fake of Issuer.
func New() *Fake { return &Fake{issuer: Issuer} }

// NewFor is a Fake whose keys belong to issuer: the directory's, for a test
// that follows one.
func NewFor(issuer string) *Fake { return &Fake{issuer: issuer} }

// Issuer is the issuer the fake's keys belong to.
func (f *Fake) Issuer() string { return f.issuer }

// Claim claims a credential that, after leading spaces and tabs, begins with
// Prefix in any case and holds a dot or is not 43 base64url characters; its
// selector when it is exactly a credential of the family.
func (f *Fake) Claim(raw string) (string, bool) {
	f.mu.Lock()
	f.claimed = append(f.claimed, raw)
	f.mu.Unlock()
	w := strings.TrimLeft(raw, " \t")
	if len(w) < len(Prefix) || !strings.EqualFold(w[:len(Prefix)], Prefix) {
		return "", false
	}
	if !strings.Contains(w, ".") && len(w) == SecretLen {
		return "", false
	}
	selector, _, ok := parse(raw)
	if !ok {
		return "", true
	}
	return selector, true
}

// parse reads a credential exactly as presented: its selector and the hash
// of its secret.
func parse(raw string) (selector, hash string, ok bool) {
	if len(raw) != Len || raw[:len(Prefix)] != Prefix || raw[len(Prefix)+SelectorLen] != '.' {
		return "", "", false
	}
	selector = raw[len(Prefix) : len(Prefix)+SelectorLen]
	if _, err := hex.DecodeString(selector); err != nil || strings.ToLower(selector) != selector {
		return "", "", false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(raw[len(Prefix)+SelectorLen+1:])
	if err != nil || len(secret) != 32 {
		return "", "", false
	}
	return selector, Hash(secret), true
}

// Hash is a secret's hash, as a record carries it.
func Hash(secret []byte) string {
	h := sha256.New()
	h.Write([]byte(hashLabel))
	h.Write([]byte{0})
	h.Write(secret)
	return hex.EncodeToString(h.Sum(nil))
}

// Verify compares raw with hash in constant time, against a dummy when no
// hash is held or raw is not a credential of the family.
func (f *Fake) Verify(raw, selector, hash string) bool {
	f.mu.Lock()
	f.verifications++
	f.mu.Unlock()
	got, want := strings.Repeat("0", 64), strings.Repeat("f", 64)
	gotSelector, h, ok := parse(raw)
	if ok {
		got = h
	}
	if hash != "" {
		want = hash
	}
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	return ok && hash != "" && gotSelector == selector && match
}

// Used remembers a use.
func (f *Fake) Used(u service.IssuedKeyUse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uses = append(f.uses, u)
}

// Uses are the uses heard so far.
func (f *Fake) Uses() []service.IssuedKeyUse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]service.IssuedKeyUse(nil), f.uses...)
}

// Claimed are the credentials Claim was asked about, as it was asked.
func (f *Fake) Claimed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claimed...)
}

// Verifications counts the comparisons made.
func (f *Fake) Verifications() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifications
}

// Credential is a new credential of the family: its text, selector and hash.
func Credential(t *testing.T) (raw, selector, hash string) {
	t.Helper()
	sel := make([]byte, SelectorLen/2)
	secret := make([]byte, 32)
	if _, err := rand.Read(sel); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	selector = hex.EncodeToString(sel)
	return Prefix + selector + "." + base64.RawURLEncoding.EncodeToString(secret), selector, Hash(secret)
}

// Key is an issued key to store.
type Key struct {
	// WorkspaceID is the key's workspace, CreatorID the person who created
	// it (their "usr_…").
	WorkspaceID, CreatorID string
	// ExternalID is its id at the issuer; empty is one made up.
	ExternalID string
	// Created is its creation; zero is now. Expires is its expiry; zero is
	// 90 days after its creation.
	Created, Expires time.Time
	// Terms is the key terms revision its creator agreed to; empty is
	// "terms-test".
	Terms string
	// Grants are what it holds, each given by its creator.
	Grants []workspace.KeyGrant
	// Revoked revokes it at its creation, as its issuer would have.
	Revoked bool
	// Stops are this server's own stops of it: "operator", "creator".
	Stops []string
}

// Store writes k into db as an extension that mirrors its issuer would, and
// returns its credential and selector.
func Store(t *testing.T, db *store.Store, k Key) (raw, selector string) {
	t.Helper()
	raw, selector, hash := Credential(t)
	created := k.Created
	if created.IsZero() {
		created = time.Now()
	}
	expires := k.Expires
	if expires.IsZero() {
		expires = created.Add(90 * 24 * time.Hour)
	}
	terms := k.Terms
	if terms == "" {
		terms = "terms-test"
	}
	external := k.ExternalID
	if external == "" {
		external = "key-" + selector
	}
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
			workspace_id, terms_version, created_by, issuer) VALUES (?, ?, '', 'write', ?, ?, ?, ?, ?, ?)`,
			selector, hash, created.Unix(), expires.Unix(), k.WorkspaceID, terms, k.CreatorID, Issuer); err != nil {
			return fmt.Errorf("the key: %w", err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO issued_keys(prefix, issuer, external_id, version, listed,
			created_at_us, terms_ok) VALUES (?, ?, ?, 1, 1, ?, 1)`, selector, Issuer, external, created.UnixMicro()); err != nil {
			return fmt.Errorf("its record: %w", err)
		}
		for _, g := range k.Grants {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO key_access(key_prefix, account_id, workspace_id, read,
				act, send, granted_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, selector, g.AccountID,
				k.WorkspaceID, g.Read, g.Act, g.Send, k.CreatorID, created.Unix(), created.Unix()); err != nil {
				return fmt.Errorf("what it holds: %w", err)
			}
			for use, on := range map[string]bool{"read": g.Read, "act": g.Act, "send": g.Send} {
				if !on {
					continue
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO issued_key_grants(prefix, account_id, use, given_by,
					given_at_us, state, version) VALUES (?, ?, ?, ?, ?, 'held', 1)`, selector, g.AccountID, use,
					"sub-"+k.CreatorID, created.UnixMicro()); err != nil {
					return fmt.Errorf("its grant: %w", err)
				}
			}
		}
		if k.Revoked {
			if _, err := tx.ExecContext(t.Context(), `UPDATE api_keys SET revoked_at = ?, stop_reason = 'manual'
				WHERE prefix = ?`, created.Unix(), selector); err != nil {
				return fmt.Errorf("its revocation: %w", err)
			}
			if _, err := tx.ExecContext(t.Context(), `UPDATE issued_keys SET ended_at = ?, end_kind = 'revoked'
				WHERE prefix = ?`, created.Unix(), selector); err != nil {
				return fmt.Errorf("its end: %w", err)
			}
		}
		for _, cause := range k.Stops {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO issued_key_stops(prefix, cause, at) VALUES (?, ?, ?)`,
				selector, cause, created.Unix()); err != nil {
				return fmt.Errorf("its stop: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store an issued key: %v", err)
	}
	return raw, selector
}
