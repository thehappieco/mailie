// Package authtest creates people for tests.
//
// It lives in its own package, as storetest does, so nothing in the daemon can
// import it. What it offers is speed: a real sign-up hashes the password at
// 64 MiB and three passes, well over a second under the race detector, and a
// test that needs two users in two roles should not pay that to set the scene.
// The users made here carry a password hash at Argon2id's smallest cost, and
// since verification reads the cost back out of the stored hash, signing in as
// them goes through the production path unchanged — only cheaper.
package authtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Password is what every user made here signs in with.
const Password = "correct horse battery staple"

// NewUser inserts an active user, with the personal workspace signing up
// makes for them (the local source's), and returns it.
func NewUser(t *testing.T, db *store.Store, email string, role auth.Role) auth.User {
	t.Helper()
	now := db.Now().UTC().Truncate(time.Second)
	user := auth.User{
		ID: "usr_" + randomHex(t, 8), Email: email, Role: role,
		HasPassword: true, PasswordChangedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at)
			 VALUES (?, ?, '', ?, ?, 'active', ?, ?, ?)`,
			user.ID, email, cheapHash(t, Password), string(role), now.Unix(), now.Unix(), now.Unix())
		if err != nil {
			return err
		}
		return workspace.Local().PersonCreatedTx(t.Context(), tx, user.ID, now)
	})
	if err != nil {
		t.Fatalf("authtest: insert user: %v", err)
	}
	return user
}

// Personal is the id of a person's personal workspace.
func Personal(t *testing.T, db *store.Store, userID string) string {
	t.Helper()
	w, err := workspace.NewRepository(db, nil).PersonalOf(t.Context(), userID)
	if err != nil {
		t.Fatalf("authtest: personal workspace of %s: %v", userID, err)
	}
	return w.ID
}

// NewKey inserts an instance key, or with a userID a key acting as that user
// that somebody else made for them — which no route accepts — and returns the
// presented form. Verifying it costs next to nothing, so a test can make
// hundreds of requests with it.
func NewKey(t *testing.T, db *store.Store, scope auth.Scope, userID string) string {
	t.Helper()
	return newKey(t, db, scope, userID, "")
}

// NewPersonalKey inserts a key the user created in the console, agreeing to
// the key terms, at any scope, and returns the presented form.
func NewPersonalKey(t *testing.T, db *store.Store, scope auth.Scope, userID string) string {
	t.Helper()
	// Any revision: what the service checks is that the person agreed to one.
	return newKey(t, db, scope, userID, "authtest-terms")
}

func newKey(t *testing.T, db *store.Store, scope auth.Scope, userID, terms string) string {
	t.Helper()
	prefix := randomHex(t, 4)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	now := db.Now().UTC().Truncate(time.Second)
	var user any
	if userID != "" {
		user = userID
	}
	_, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, user_id, terms_version)
		 VALUES (?, ?, 'authtest', ?, ?, ?, ?, ?)`,
		prefix, cheapHash(t, secret), string(scope), now.Unix(), now.Add(auth.MaxLifetime).Unix(), user, terms)
	if err != nil {
		t.Fatalf("authtest: insert key: %v", err)
	}
	return prefix + "." + secret
}

// cheapHash is a PHC string at Argon2id's smallest cost: 8 KiB, one pass.
func cheapHash(t *testing.T, secret string) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	const memory, passes = 8, 1
	digest := argon2.IDKey([]byte(secret), salt, passes, memory, 1, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=1$%s$%s", argon2.Version, memory, passes,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// SignIn opens a session for a user made by NewUser and returns its token.
func SignIn(t *testing.T, users *auth.Users, email string) string {
	t.Helper()
	token, _, _, err := users.SignIn(t.Context(), email, Password, "authtest")
	if err != nil {
		t.Fatalf("authtest: sign in as %s: %v", email, err)
	}
	return token
}
