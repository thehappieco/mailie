// Package authtest creates people for tests.
//
// It lives in its own package, as storetest does, so nothing in the daemon can
// import it. What it offers is speed: a real sign-up hashes the auth key and
// the recovery proof at 19 MiB each, and a test that needs two users in two
// roles should not pay that to set the scene. The users made here are
// enrolled in the key scheme with verifiers at Argon2id's smallest cost, and
// since verification reads the cost back out of the stored hash, signing in
// as them goes through the production path unchanged — only cheaper.
package authtest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Password is what every user NewLegacyUser makes signs in with at the
// upgrade, as people did before the key scheme.
const Password = "correct horse battery staple"

// AuthKey is the auth key every user NewUser makes signs in with, and
// RecoveryProof the proof of their recovery code: base64url of 32 bytes, as a
// browser derives them. Neither is derived from anything here; the server
// never sees what they would be derived from.
var (
	AuthKey       = encoded("authtest auth key")
	RecoveryProof = encoded("authtest recovery proof")
)

// SaltKey is a salt key a test may give its auth.Users (WithSaltKey), so that
// the users made here are at their target and a sign-in names no
// re-derivation (docs/key-scheme.md section 5.3).
var SaltKey = bytes.Repeat([]byte{0x5a}, keyscheme.KeyLen)

func encoded(label string) string {
	sum := sha256.Sum256([]byte(label))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Enrolment is what a browser would send to enrol a person: AuthKey,
// RecoveryProof, the default parameters, and a fresh account public key with
// wraps of the right shape. The wraps open nothing: the server only ever
// checks their shape.
func Enrolment(t *testing.T) auth.Enrolment {
	t.Helper()
	return EnrolmentWith(t, AuthKey, RecoveryProof)
}

// EnrolmentWith is Enrolment with another auth key and recovery proof.
func EnrolmentWith(t *testing.T, authKey, proof string) auth.Enrolment {
	t.Helper()
	return auth.Enrolment{
		AuthKey: authKey, KDF: auth.DefaultKDF, PublicKey: PublicKey(t),
		PasswordWrap: Wrap(t), RecoveryWrap: Wrap(t), RecoveryProof: proof,
	}
}

// PublicKey is the public half of a fresh account key.
func PublicKey(t *testing.T) []byte {
	t.Helper()
	priv := make([]byte, keyscheme.KeyLen)
	if _, err := rand.Read(priv); err != nil {
		t.Fatal(err)
	}
	pub, err := keyscheme.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// Wrap is 61 random bytes starting with 0x02: an account wrap's shape.
func Wrap(t *testing.T) []byte {
	t.Helper()
	w := make([]byte, keyscheme.AccountWrapLen)
	if _, err := rand.Read(w); err != nil {
		t.Fatal(err)
	}
	w[0] = keyscheme.AccountWrapHeader
	return w
}

// NewUser inserts an active user, enrolled in the key scheme with AuthKey
// and RecoveryProof at their target under SaltKey, with the personal
// workspace signing up makes for them (the local source's), and returns it.
func NewUser(t *testing.T, db *store.Store, email string, role auth.Role) auth.User {
	t.Helper()
	now := db.Now().UTC().Truncate(time.Second)
	in := Enrolment(t)
	user := auth.User{
		ID: "usr_" + randomHex(t, 8), Email: email, Role: role, HasPassword: true, Enrolled: true,
		SealID: keyscheme.NewSealID(), PublicKey: in.PublicKey, PasswordChangedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	salt, err := keyscheme.DecoySalt(SaltKey, email)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at,
			                   seal_id, public_key, auth_verifier, kdf_salt, kdf_m, kdf_t, kdf_p, password_wrap,
			                   recovery_wrap, recovery_verifier, zk_enrolled_at)
			 VALUES (?, ?, '', '', ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user.ID, email, string(role), now.Unix(), now.Unix(), now.Unix(), user.SealID, in.PublicKey,
			cheapHash(t, in.AuthKey), salt, in.KDF.M, in.KDF.T, in.KDF.P, in.PasswordWrap, in.RecoveryWrap,
			cheapHash(t, in.RecoveryProof), now.Unix())
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

// NewLegacyUser inserts an active user who signed up before the key scheme:
// a password the server checks itself (Password), no account key. Their next
// sign-in is the upgrade's.
func NewLegacyUser(t *testing.T, db *store.Store, email string, role auth.Role) auth.User {
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
		if err := tx.QueryRowContext(t.Context(), `SELECT seal_id FROM users WHERE id = ?`, user.ID).Scan(&user.SealID); err != nil {
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

// NewKey inserts an instance key — a key of the operator workspace, which
// reaches its mailboxes — and returns the presented form. Verifying it costs
// next to nothing, so a test can make hundreds of requests with it.
func NewKey(t *testing.T, db *store.Store, scope auth.Scope) string {
	t.Helper()
	return newKey(t, db, scope, workspace.OperatorID, "cli", "")
}

// NewWorkspaceKey inserts a key of a workspace that createdBy created,
// agreeing to the key terms, holding what grants name on mailboxes of that
// workspace, given by createdBy, and returns the presented form. Who may give
// what is not checked: the scene is set as a test needs it.
func NewWorkspaceKey(t *testing.T, db *store.Store, scope auth.Scope, workspaceID, createdBy string,
	grants ...workspace.KeyGrant,
) string {
	t.Helper()
	presented := newKey(t, db, scope, workspaceID, createdBy, "authtest-terms")
	prefix, _, _ := strings.Cut(presented, ".")
	now := db.Now().UTC().Truncate(time.Second)
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		for _, g := range grants {
			if err := workspace.PutKeyAccessTx(t.Context(), tx, prefix, workspaceID, g, createdBy, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("authtest: give the key its mailboxes: %v", err)
	}
	return presented
}

// Prefix is the prefix of a presented key.
func Prefix(presented string) string {
	prefix, _, _ := strings.Cut(presented, ".")
	return prefix
}

func newKey(t *testing.T, db *store.Store, scope auth.Scope, workspaceID, createdBy, terms string) string {
	t.Helper()
	prefix := randomHex(t, 4)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	now := db.Now().UTC().Truncate(time.Second)
	_, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, terms_version, created_by)
		 VALUES (?, ?, 'authtest', ?, ?, ?, ?, ?, ?)`,
		prefix, cheapHash(t, secret), string(scope), now.Unix(), now.Add(auth.MaxLifetime).Unix(), workspaceID, terms,
		createdBy)
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

// SignIn opens a session for a user made by NewUser, with AuthKey through
// the sign-in every browser uses, and returns its token. Its step-up time is
// now.
func SignIn(t *testing.T, users *auth.Users, email string) string {
	t.Helper()
	login, err := users.Login(t.Context(), email, AuthKey, "authtest")
	if err != nil {
		t.Fatalf("authtest: sign in as %s: %v", email, err)
	}
	return login.Token
}
