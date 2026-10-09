package auth_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The ceremonies of docs/key-scheme.md section 12, as the server runs them.

// secretOf is base64url of 32 bytes made from a label: an auth key or a
// proof a browser could have derived.
func secretOf(label string) string {
	b := bytes.Repeat([]byte(label), 32)[:32]
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionOf authenticates a token and returns its session id.
func sessionOf(t *testing.T, users *auth.Users, token string) string {
	t.Helper()
	p, err := users.AuthenticateSession(t.Context(), token)
	if err != nil {
		t.Fatalf("the session does not authenticate: %v", err)
	}
	return p.SessionID
}

// userRow is what the ceremonies store of a person, as text.
func userRow(t *testing.T, db *store.Store, id string) string {
	t.Helper()
	var (
		hash, verifier, recovery, seal string
		pub, salt, wrap, rwrap         []byte
		m, tt, p, enrolled, replaced   int64
	)
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT password_hash, auth_verifier, recovery_verifier, seal_id,
		public_key, kdf_salt, password_wrap, recovery_wrap, kdf_m, kdf_t, kdf_p, zk_enrolled_at, key_replaced_at
		FROM users WHERE id = ?`, id).Scan(&hash, &verifier, &recovery, &seal, &pub, &salt, &wrap, &rwrap, &m, &tt, &p,
		&enrolled, &replaced); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%x|%x|%x|%x|%d|%d|%d|%d|%d", hash, verifier, recovery, seal, pub, salt, wrap, rwrap,
		m, tt, p, enrolled, replaced)
}

func liveSessions(t *testing.T, db *store.Store, userID string) int {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM sessions WHERE user_id = ? AND revoked_at = 0`, userID)
}

func TestTheServerStoresNeitherTheAuthKeyNorTheProofButTheirHashes(t *testing.T) {
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)
	req := signUpRequest(t, users, code, "ana@example.com")
	_, _, ana, err := users.SignUp(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !ana.Enrolled || !ana.HasPassword || !keyscheme.ValidSealID(ana.SealID) || !bytes.Equal(ana.PublicKey, req.Enrolment.PublicKey) {
		t.Fatalf("signed up %+v", ana)
	}
	var hash, verifier, recovery string
	var salt []byte
	var m, tt, p int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT password_hash, auth_verifier, recovery_verifier, kdf_salt,
		kdf_m, kdf_t, kdf_p FROM users WHERE id = ?`, ana.ID).Scan(&hash, &verifier, &recovery, &salt, &m, &tt, &p); err != nil {
		t.Fatal(err)
	}
	memory, passes := auth.VerifierCostForTest()
	prefix := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=1$", argon2.Version, memory, passes)
	if hash != "" || !strings.HasPrefix(verifier, prefix) || !strings.HasPrefix(recovery, prefix) {
		t.Errorf("stored hash %q, verifiers %q and %q: want no password hash and two verifiers at %s", hash, verifier,
			recovery, prefix)
	}
	if strings.Contains(verifier, req.Enrolment.AuthKey) || strings.Contains(recovery, req.Enrolment.RecoveryProof) {
		t.Error("a secret is stored in clear")
	}
	if n := count(t, db, `SELECT count(*) FROM users WHERE instr(CAST(auth_verifier AS BLOB), CAST(? AS BLOB)) > 0
		OR instr(CAST(recovery_verifier AS BLOB), CAST(? AS BLOB)) > 0`, req.Enrolment.AuthKey, req.Enrolment.RecoveryProof); n != 0 {
		t.Error("the database holds a secret in clear")
	}
	// The salt is the address's target, the parameters the default.
	want, err := keyscheme.DecoySalt(authtest.SaltKey, "ana@example.com")
	if err != nil || !bytes.Equal(salt, want) || (auth.KDF{M: m, T: tt, P: p}) != auth.DefaultKDF {
		t.Errorf("stored salt %x and m=%d t=%d p=%d, want the target %x and the default", salt, m, tt, p, want)
	}
}

func TestASignUpGivesThePersonTheSealIDOpeningItsInvitationAnswered(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	if _, err := users.OpenSignUp(t.Context(), code, "bob@example.com"); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("an invitation opened for another address: %v", err)
	}
	opened, err := users.OpenSignUp(t.Context(), code, " Ana@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !keyscheme.ValidSealID(opened.SealID) || !bytes.Equal(opened.Salt, mustSalt(t, authtest.SaltKey, "ana@example.com")) ||
		opened.KDF != auth.DefaultKDF {
		t.Fatalf("the invitation opened %+v; want a seal id and the address's target", opened)
	}
	// Opened again, in another tab, it answers the same: the wraps a browser
	// bound to either are the person's.
	again, err := users.OpenSignUp(t.Context(), code, "ana@example.com")
	if err != nil || again.SealID != opened.SealID {
		t.Fatalf("opened again: %q, %v; want %q", again.SealID, err, opened.SealID)
	}
	req := auth.SignUpRequest{Invite: code, Email: "ana@example.com", Enrolment: authtest.Enrolment(t), UserAgent: "test"}
	for name, seal := range map[string]string{"none": "", "another": keyscheme.NewSealID(), "malformed": "not-a-uuid"} {
		req.SealID = seal
		if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, auth.ErrSealIDNotOpened) {
			t.Errorf("a sign-up bound to %s seal id: %v", name, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM invites WHERE used_at = 0`); n != 1 {
		t.Fatalf("a refused sign-up spent the invitation")
	}
	req.SealID = opened.SealID
	_, _, ana, err := users.SignUp(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if ana.SealID != opened.SealID || !strings.Contains(userRow(t, db, ana.ID), opened.SealID) {
		t.Errorf("the person has the seal id %q, want %q", ana.SealID, opened.SealID)
	}
	if _, err := users.OpenSignUp(t.Context(), code, "ana@example.com"); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("a used invitation still opens: %v", err)
	}
}

func TestAChallengeAnswersAnAccountAtItsTargetExactlyAsAnAddressWithoutOne(t *testing.T) {
	users, db, _ := newUsers(t)
	nobody, err := users.Challenge(t.Context(), "ana@example.com")
	if err != nil || nobody.Upgrade || len(nobody.Salt) != 16 || nobody.KDF != auth.DefaultKDF {
		t.Fatalf("challenge for nobody = %+v, %v", nobody, err)
	}
	// The address's salt before its account exists is the salt it keeps,
	// however the address is typed.
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	for _, typed := range []string{"ana@example.com", " ANA@Example.com "} {
		got, err := users.Challenge(t.Context(), typed)
		if err != nil || got.Upgrade || !bytes.Equal(got.Salt, nobody.Salt) || got.KDF != nobody.KDF {
			t.Errorf("challenge for %q with an account = %+v, %v; want %+v", typed, got, err, nobody)
		}
	}
	// A disabled person's address answers as an unknown one, and another
	// address another salt.
	gone := authtest.NewUser(t, db, "gone@example.com", auth.RoleMember)
	if err := users.SetDisabled(t.Context(), gone.ID, true); err != nil {
		t.Fatal(err)
	}
	disabled, err := users.Challenge(t.Context(), "gone@example.com")
	if err != nil || disabled.Upgrade || bytes.Equal(disabled.Salt, nobody.Salt) {
		t.Errorf("challenge for a disabled person = %+v, %v", disabled, err)
	}
	if want, _ := keyscheme.DecoySalt(authtest.SaltKey, "gone@example.com"); !bytes.Equal(disabled.Salt, want) {
		t.Errorf("a disabled person's salt %x, want the address's %x", disabled.Salt, want)
	}
}

func TestAChallengeSaysUpgradeOnlyForAnActivePersonWithAnOldPassword(t *testing.T) {
	users, db, _ := newUsers(t)
	authtest.NewLegacyUser(t, db, "old@example.com", auth.RoleMember)
	gone := authtest.NewLegacyUser(t, db, "gone@example.com", auth.RoleMember)
	if err := users.SetDisabled(t.Context(), gone.ID, true); err != nil {
		t.Fatal(err)
	}
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	for email, want := range map[string]bool{
		"old@example.com": true, "gone@example.com": false, "ana@example.com": false, "nobody@example.com": false,
	} {
		got, err := users.Challenge(t.Context(), email)
		if err != nil || got.Upgrade != want {
			t.Errorf("challenge for %s: upgrade %v (%v), want %v", email, got.Upgrade, err, want)
		}
		if target, _ := keyscheme.DecoySalt(authtest.SaltKey, email); !bytes.Equal(got.Salt, target) {
			t.Errorf("challenge for %s answered a salt that is not the address's", email)
		}
	}
	if _, err := users.Challenge(t.Context(), "not an address"); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Errorf("a challenge for something that is not an address: %v", err)
	}
}

func TestASignInAnswersThePasswordWrapAndAStepUpTimeOfNow(t *testing.T) {
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	var wrap []byte
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT password_wrap FROM users WHERE id = ?`, ana.ID).Scan(&wrap); err != nil {
		t.Fatal(err)
	}
	login, err := users.Login(t.Context(), " Ana@Example.com", authtest.AuthKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(login.PasswordWrap, wrap) || login.User.SealID != ana.SealID || !bytes.Equal(login.User.PublicKey, ana.PublicKey) {
		t.Errorf("the sign-in answered %+v", login)
	}
	if login.Rederive != nil {
		t.Errorf("an account at its target was asked to re-derive: %+v", login.Rederive)
	}
	if !login.Session.AuthenticatedAt.Equal(clock.Truncate(time.Second)) {
		t.Errorf("step-up time %v, want the sign-in's %v", login.Session.AuthenticatedAt, *clock)
	}
	if err := users.RequireStepUp(t.Context(), ana.ID, login.Session.ID); err != nil {
		t.Errorf("a sign-in does not count as a step-up: %v", err)
	}
}

func TestASignInOffTargetNamesItAndItsReDerivationEndsNoSession(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	// Another salt key: what a replaced salt key, or an address changed by
	// the operator, leaves the account with.
	moved := auth.NewUsersWithClock(db, func() time.Time { return *clock })
	other := authtest.SignIn(t, users, "ana@example.com")
	before := userRow(t, db, ana.ID)

	login, err := moved.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	if login.Rederive == nil || bytes.Equal(login.Rederive.Salt, mustSalt(t, authtest.SaltKey, "ana@example.com")) ||
		login.Rederive.KDF != auth.DefaultKDF {
		t.Fatalf("an account off its target was answered %+v", login.Rederive)
	}
	sid := sessionOf(t, moved, login.Token)
	newKey := secretOf("re-derived")
	changed, err := moved.FinishPasswordChange(t.Context(), ana.ID, sid, auth.NewPassword{
		Ticket: login.Rederive.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: newKey, KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t),
	}, "test")
	if err != nil || changed.Rotated || changed.Token != "" {
		t.Fatalf("the re-derivation = %+v, %v; want nothing rotated", changed, err)
	}
	for _, token := range []string{login.Token, other} {
		if _, err := moved.AuthenticateSession(t.Context(), token); err != nil {
			t.Errorf("a re-derivation ended a session: %v", err)
		}
	}
	after := userRow(t, db, ana.ID)
	if after == before {
		t.Fatal("the re-derivation stored nothing")
	}
	// The target now, and the same account key, recovery and enrolment.
	again, err := moved.Login(t.Context(), "ana@example.com", newKey, "test")
	if err != nil || again.Rederive != nil || !bytes.Equal(again.User.PublicKey, ana.PublicKey) {
		t.Errorf("after the re-derivation: %+v, %v", again.Rederive, err)
	}
	if c, err := moved.Challenge(t.Context(), "ana@example.com"); err != nil || !bytes.Equal(c.Salt, login.Rederive.Salt) {
		t.Errorf("the challenge answers %x, want the target %x", c.Salt, login.Rederive.Salt)
	}
}

// countedKDF makes Argon2id cheap for the rest of the test, as cheapKDF does,
// and counts the derivations.
func countedKDF(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		n.Add(1)
		return argon2.IDKey(password, salt, 1, 8, 1, keyLen)
	})
	return &n
}

func TestATicketInAnAnswerFinishesOnlyWithTheAuthKeyThatEarnedIt(t *testing.T) {
	// A sign-in's re-derivation ticket rides in the same answer as the
	// session, and a password change's in the answer to its first step:
	// whoever saw only that answer (a proxy's log) holds the session and the
	// ticket, not the auth key. With them alone, it must set no password.
	derivations := countedKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	// Another salt key: an account off its target, whose sign-in names it.
	moved := auth.NewUsersWithClock(db, func() time.Time { return *clock })
	rederive := func() (string, string) {
		login, err := moved.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test")
		if err != nil || login.Rederive == nil {
			t.Fatalf("a sign-in off target: %+v, %v", login.Rederive, err)
		}
		return login.Rederive.Ticket, sessionOf(t, moved, login.Token)
	}
	change := func() (string, string) {
		laptop := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
		begun, err := users.BeginPasswordChange(t.Context(), ana.ID, laptop, authtest.AuthKey)
		if err != nil {
			t.Fatal(err)
		}
		return begun.Ticket, laptop
	}

	for _, c := range []struct {
		name  string
		start func() (ticket, session string)
		users *auth.Users
	}{
		{"a sign-in's re-derivation", rederive, moved},
		{"a password change", change, users},
	} {
		ticket, session := c.start()
		before := userRow(t, db, ana.ID)
		finish := auth.NewPassword{Ticket: ticket, AuthKey: secretOf("the proxy's own"), KDF: auth.DefaultKDF,
			PasswordWrap: authtest.Wrap(t)}
		for name, current := range map[string]string{
			"another auth key":   secretOf("guess"),
			"the new auth key":   finish.AuthKey,
			"the recovery proof": authtest.RecoveryProof,
			"no key at all":      "",
		} {
			finish.CurrentAuthKey = current
			derivations.Store(0)
			_, err := c.users.FinishPasswordChange(t.Context(), ana.ID, session, finish, "test")
			if current == "" {
				if !errors.Is(err, auth.ErrMalformedSecret) {
					t.Errorf("%s with no current auth key: %v", c.name, err)
				}
			} else if !errors.Is(err, auth.ErrTicketInvalid) {
				t.Errorf("%s finished with %s: %v", c.name, name, err)
			}
			if n := derivations.Load(); n != 0 {
				t.Errorf("%s refused with %s after %d derivations, want none", c.name, name, n)
			}
		}
		if userRow(t, db, ana.ID) != before {
			t.Fatalf("%s: a refused finish stored something", c.name)
		}
		// The ticket stays its own: the browser that earned it finishes.
		finish.CurrentAuthKey = authtest.AuthKey
		if _, err := c.users.FinishPasswordChange(t.Context(), ana.ID, session, finish, "test"); err != nil {
			t.Fatalf("%s with the auth key that earned it: %v", c.name, err)
		}
		// Back to Ana's own password, off target again, for the next case.
		if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET auth_verifier = ?, kdf_salt = ? WHERE id = ?`,
			mustVerifier(t, authtest.AuthKey), mustSalt(t, authtest.SaltKey, "ana@example.com"), ana.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestARecoveryOrAnEnrolmentWithATicketThatIsNotOneCostsNoDerivation(t *testing.T) {
	derivations := countedKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.NewLegacyUser(t, db, "old@example.com", auth.RoleMember)
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := users.LegacySignIn(t.Context(), "old@example.com", authtest.Password)
	if err != nil {
		t.Fatal(err)
	}
	made := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	recover := func(ticket string) error {
		return users.FinishRecovery(t.Context(), auth.RecoveryFinish{
			Ticket: ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: secretOf("x"), KDF: auth.DefaultKDF,
			PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("y"),
		})
	}
	enrol := func(ticket string) error {
		_, _, _, err := users.Enrol(t.Context(), ticket, authtest.Enrolment(t), "test")
		return err
	}
	for name, try := range map[string]func() error{
		"a recovery with a made-up ticket":      func() error { return recover(made) },
		"a recovery with an enrolment's ticket": func() error { return recover(upgrade.Ticket) },
		"an enrolment with a made-up ticket":    func() error { return enrol(made) },
		"an enrolment with a recovery's ticket": func() error { return enrol(opened.Ticket) },
	} {
		derivations.Store(0)
		if err := try(); !errors.Is(err, auth.ErrTicketInvalid) {
			t.Errorf("%s: %v", name, err)
		}
		if n := derivations.Load(); n != 0 {
			t.Errorf("%s was refused after %d derivations, want none", name, n)
		}
	}
	// Both tickets are still their own.
	if err := recover(opened.Ticket); err != nil {
		t.Errorf("the recovery's own ticket: %v", err)
	}
	if err := enrol(upgrade.Ticket); err != nil {
		t.Errorf("the enrolment's own ticket: %v", err)
	}
}

func TestARecoveryTicketFinishesOnlyWithTheProofThatEarnedIt(t *testing.T) {
	// The ticket rides in recover/open's answer, beside the recovery wrap:
	// whoever saw only that answer (a proxy's log) holds the ticket, not the
	// proof the request carried. With it alone, they must set no password
	// and no code of their own.
	derivations := countedKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	before := userRow(t, db, ana.ID)
	finish := auth.RecoveryFinish{
		Ticket: opened.Ticket, AuthKey: secretOf("the proxy's own"), KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t),
		RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("the proxy's code"),
	}
	for name, current := range map[string]string{
		"another proof":         secretOf("guess"),
		"the new code's proof":  finish.RecoveryProof,
		"the person's auth key": authtest.AuthKey,
		"no proof at all":       "",
	} {
		finish.CurrentRecoveryProof = current
		derivations.Store(0)
		err := users.FinishRecovery(t.Context(), finish)
		if current == "" {
			if !errors.Is(err, auth.ErrMalformedSecret) {
				t.Errorf("a recovery with no current proof: %v", err)
			}
		} else if !errors.Is(err, auth.ErrTicketInvalid) {
			t.Errorf("a recovery finished with %s: %v", name, err)
		}
		if n := derivations.Load(); n != 0 {
			t.Errorf("a recovery refused with %s after %d derivations, want none", name, n)
		}
	}
	if userRow(t, db, ana.ID) != before {
		t.Fatal("a refused recovery stored something")
	}
	// The ticket stays its own: the browser that opened the recovery finishes.
	finish.CurrentRecoveryProof = authtest.RecoveryProof
	if err := users.FinishRecovery(t.Context(), finish); err != nil {
		t.Fatalf("a recovery with the proof that opened it: %v", err)
	}
	if _, err := users.Login(t.Context(), "ana@example.com", finish.AuthKey, "test"); err != nil {
		t.Errorf("the new password after the recovery: %v", err)
	}
}

func mustSalt(t *testing.T, key []byte, email string) []byte {
	t.Helper()
	salt, err := keyscheme.DecoySalt(key, email)
	if err != nil {
		t.Fatal(err)
	}
	return salt
}

func TestAPasswordChangeNeedsTheCurrentAuthKeyAndEndsEverySession(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	phone := authtest.SignIn(t, users, "ana@example.com")
	sid := sessionOf(t, users, laptop)
	before := userRow(t, db, ana.ID)

	if _, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, secretOf("wrong")); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("a wrong current auth key began a change: %v", err)
	}
	begun, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, authtest.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(begun.PasswordWrap) != keyscheme.AccountWrapLen || begun.Ticket == "" || begun.KDF != auth.DefaultKDF {
		t.Fatalf("begin = %+v", begun)
	}
	// The ticket is this session's: the phone cannot finish the laptop's.
	newKey := secretOf("brand new")
	finish := auth.NewPassword{
		Ticket: begun.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: newKey, KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t),
	}
	if _, err := users.FinishPasswordChange(t.Context(), ana.ID, sessionOf(t, users, phone), finish, "test"); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("another session finished the change: %v", err)
	}
	if userRow(t, db, ana.ID) != before {
		t.Fatal("a refused change stored something")
	}
	*clock = clock.Add(time.Minute)
	changed, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, finish, "test")
	if err != nil || !changed.Rotated {
		t.Fatalf("finish = %+v, %v", changed, err)
	}
	for name, token := range map[string]string{"laptop": laptop, "phone": phone} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("the %s session survived the change: %v", name, err)
		}
	}
	// The new session keeps the old one's step-up time: a change opens no
	// window of its own.
	if _, err := users.AuthenticateSession(t.Context(), changed.Token); err != nil {
		t.Errorf("the new session does not work: %v", err)
	}
	if !changed.Session.AuthenticatedAt.Equal(clock.Add(-time.Minute).Truncate(time.Second)) {
		t.Errorf("the new session's step-up time is %v", changed.Session.AuthenticatedAt)
	}
	if _, err := users.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old auth key still signs in: %v", err)
	}
	login, err := users.Login(t.Context(), "ana@example.com", newKey, "test")
	if err != nil || !bytes.Equal(login.User.PublicKey, ana.PublicKey) || !bytes.Equal(login.PasswordWrap, finish.PasswordWrap) {
		t.Errorf("the new auth key: %+v, %v", login.User, err)
	}
	// Used once.
	if _, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, finish, "test"); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a ticket worked twice: %v", err)
	}
}

func TestATicketWorksOnceForItsPersonCeremonyAndTenMinutesOnly(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	// A recovery's ticket is no password change's.
	finish := auth.NewPassword{
		Ticket: opened.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: secretOf("x"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t),
	}
	if _, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, finish, "test"); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a recovery's ticket finished a password change: %v", err)
	}
	// Nor does it outlive its ten minutes.
	*clock = clock.Add(auth.TicketTTL + time.Second)
	if err := users.FinishRecovery(t.Context(), auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: secretOf("x"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("y"),
	}); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("an expired ticket finished a recovery: %v", err)
	}
	if n, err := users.SweepTickets(t.Context()); err != nil || n != 1 {
		t.Errorf("SweepTickets = %d, %v; want the expired ticket", n, err)
	}
}

func TestARecoveryKeepsTheAccountKeyAndEndsEverySession(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, users, "ana@example.com")
	var rwrap []byte
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT recovery_wrap FROM users WHERE id = ?`, ana.ID).Scan(&rwrap); err != nil {
		t.Fatal(err)
	}

	for name, try := range map[string]struct{ email, proof string }{
		"a wrong proof":      {"ana@example.com", secretOf("wrong")},
		"an unknown address": {"nobody@example.com", authtest.RecoveryProof},
	} {
		if _, err := users.OpenRecovery(t.Context(), try.email, try.proof); !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("%s opened a recovery: %v", name, err)
		}
	}
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened.RecoveryWrap, rwrap) || opened.SealID != ana.SealID || !bytes.Equal(opened.PublicKey, ana.PublicKey) ||
		!bytes.Equal(opened.Salt, mustSalt(t, authtest.SaltKey, "ana@example.com")) {
		t.Fatalf("open = %+v", opened)
	}
	newKey, newProof := secretOf("after recovery"), secretOf("new code")
	if err := users.FinishRecovery(t.Context(), auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: newKey, KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: newProof,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a session survived the recovery: %v", err)
	}
	login, err := users.Login(t.Context(), "ana@example.com", newKey, "test")
	if err != nil || !bytes.Equal(login.User.PublicKey, ana.PublicKey) || login.User.SealID != ana.SealID {
		t.Fatalf("after the recovery: %+v, %v; want the same account key", login.User, err)
	}
	// The old code no longer opens anything, the new one does.
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the replaced recovery code still opens: %v", err)
	}
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", newProof); err != nil {
		t.Errorf("the new recovery code does not open: %v", err)
	}
}

func TestReplacingTheRecoveryCodeNeedsTheCurrentAuthKey(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET auth_verifier = ? WHERE id = ?`,
		mustVerifier(t, secretOf("bea")), bea.ID); err != nil {
		t.Fatal(err)
	}
	// Right after the sign-in, the step-up window open: the session alone
	// still does not do it.
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	before := userRow(t, db, ana.ID)
	for name, key := range map[string]string{
		"a wrong auth key":            secretOf("guess"),
		"another person's auth key":   secretOf("bea"),
		"the recovery proof instead":  authtest.RecoveryProof,
		"the new code's proof itself": secretOf("new"),
	} {
		if err := users.ReplaceRecovery(t.Context(), ana.ID, sid, key, authtest.Wrap(t), secretOf("new")); !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("%s replaced the recovery code: %v", name, err)
		}
	}
	if err := users.ReplaceRecovery(t.Context(), ana.ID, sid, "", authtest.Wrap(t), secretOf("new")); !errors.Is(err, auth.ErrMalformedSecret) {
		t.Errorf("no auth key at all: %v", err)
	}
	if userRow(t, db, ana.ID) != before {
		t.Fatal("a refused replacement stored something")
	}
	// The key proves the password whenever it is presented: no step-up
	// window is asked for on top of it.
	*clock = clock.Add(auth.StepUpWindow + time.Hour)
	if err := users.ReplaceRecovery(t.Context(), ana.ID, sid, authtest.AuthKey, authtest.Wrap(t), secretOf("new")); err != nil {
		t.Fatalf("the current auth key could not replace the recovery code: %v", err)
	}
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", secretOf("new")); err != nil {
		t.Errorf("the new code does not open: %v", err)
	}
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old code still opens: %v", err)
	}
	// A session that has ended replaces nothing, whatever it presents.
	if err := users.EndSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	if err := users.ReplaceRecovery(t.Context(), ana.ID, sid, authtest.AuthKey, authtest.Wrap(t), secretOf("later")); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("an ended session replaced the recovery code: %v", err)
	}
}

func TestARecoveryOpenedWhileItsCodeIsReplacedGetsNoTicket(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	// Whoever holds the old code opens a recovery; while its proof is
	// hashed, the person replaces the code. No ticket was there to delete
	// yet: the recovery must not get one after the replacement committed.
	var once sync.Once
	var replaced error
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		if string(password) == authtest.RecoveryProof {
			once.Do(func() {
				replaced = users.ReplaceRecovery(t.Context(), ana.ID, sid, authtest.AuthKey, authtest.Wrap(t), secretOf("replaced"))
			})
		}
		return argon2.IDKey(password, salt, 1, 8, 1, keyLen)
	})
	_, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if replaced != nil {
		t.Fatalf("the replacement during the hash: %v", replaced)
	}
	if !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("a recovery opened with the code replaced while it was checked: %v", err)
	}
	var tickets int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM auth_tickets WHERE user_id = ? AND purpose = 'recover'`,
		ana.ID).Scan(&tickets); err != nil || tickets != 0 {
		t.Errorf("recover tickets after the replacement: %d, %v", tickets, err)
	}
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", secretOf("replaced")); err != nil {
		t.Errorf("the new code does not open: %v", err)
	}
}

func TestReplacingTheRecoveryCodeEndsARecoveryOpenedWithTheOldOne(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	// Whoever holds the old code opens a recovery; the person, signed in,
	// then replaces the code they no longer trust.
	opened, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof)
	if err != nil {
		t.Fatal(err)
	}
	// A password change in flight proved the password, which this does not
	// change.
	begun, err := users.BeginPasswordChange(t.Context(), ana.ID, sid, authtest.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.ReplaceRecovery(t.Context(), ana.ID, sid, authtest.AuthKey, authtest.Wrap(t), secretOf("replaced")); err != nil {
		t.Fatal(err)
	}
	before := userRow(t, db, ana.ID)
	err = users.FinishRecovery(t.Context(), auth.RecoveryFinish{
		Ticket: opened.Ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: secretOf("taken over"),
		KDF: auth.DefaultKDF, PasswordWrap: authtest.Wrap(t), RecoveryWrap: authtest.Wrap(t), RecoveryProof: secretOf("theirs"),
	})
	if !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("a recovery opened with the replaced code finished: %v", err)
	}
	if userRow(t, db, ana.ID) != before {
		t.Error("the refused recovery stored something")
	}
	if _, err := users.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test"); err != nil {
		t.Errorf("the person's own password stopped working: %v", err)
	}
	if _, err := users.FinishPasswordChange(t.Context(), ana.ID, sid, auth.NewPassword{
		Ticket: begun.Ticket, CurrentAuthKey: authtest.AuthKey, AuthKey: secretOf("changed"), KDF: auth.DefaultKDF,
		PasswordWrap: authtest.Wrap(t),
	}, "test"); err != nil {
		t.Errorf("a password change begun before the replacement could not finish: %v", err)
	}
}

func TestAStepUpProvesOnlyTheSessionsOwnPerson(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET auth_verifier = ? WHERE id = ?`,
		mustVerifier(t, secretOf("bea")), bea.ID); err != nil {
		t.Fatal(err)
	}
	sid := sessionOf(t, users, authtest.SignIn(t, users, "ana@example.com"))
	*clock = clock.Add(auth.StepUpWindow + time.Second)
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Fatalf("a session past its window: %v", err)
	}

	// Bea's own auth key does not step up Ana's session, nor does anything
	// on behalf of another person.
	if _, err := users.StepUp(t.Context(), ana.ID, sid, secretOf("bea")); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("another person's auth key stepped up the session: %v", err)
	}
	if _, err := users.StepUp(t.Context(), bea.ID, sid, secretOf("bea")); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a step-up for another person reached the session: %v", err)
	}
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Errorf("a refused step-up freshened the session: %v", err)
	}
	at, err := users.StepUp(t.Context(), ana.ID, sid, authtest.AuthKey)
	if err != nil || !at.Equal(clock.Truncate(time.Second)) {
		t.Fatalf("StepUp = %v, %v", at, err)
	}
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); err != nil {
		t.Errorf("a stepped-up session: %v", err)
	}
	*clock = clock.Add(auth.StepUpWindow)
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); err != nil {
		t.Errorf("at exactly ten minutes: %v", err)
	}
	*clock = clock.Add(time.Second)
	if err := users.RequireStepUp(t.Context(), ana.ID, sid); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Errorf("past ten minutes: %v", err)
	}
}

func mustVerifier(t *testing.T, secret string) string {
	t.Helper()
	v, err := auth.HashVerifierForTest(t.Context(), secret)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAnExternalStepUpNeedsAFreshMarkAndTheSessionsOwnIdentity(t *testing.T) {
	users, _, clock := newUsers(t)
	token, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	signInExternal(t, users, external("subject-of-dee", "dee@example.com"))
	sid := sessionOf(t, users, token)
	if err := users.RequireStepUp(t.Context(), cy.ID, sid); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Fatalf("a sign-in with no authentication time opened a window: %v", err)
	}
	now := clock.Truncate(time.Second)
	// No mark yet.
	if err := users.ExternalStepUp(t.Context(), cy.ID, sid, issuer, "subject-of-cy", now); !errors.Is(err, auth.ErrStepUpRefused) {
		t.Errorf("a step-up without a mark: %v", err)
	}
	mark, err := users.MarkExternalStepUp(t.Context(), cy.ID, sid)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	for name, c := range map[string]struct {
		subject string
		at      time.Time
	}{
		"another identity":                  {"subject-of-dee", clock.Truncate(time.Second)},
		"an authentication before the mark": {"subject-of-cy", mark.Add(-time.Second)},
		"an authentication at the mark":     {"subject-of-cy", mark},
		"an authentication after now":       {"subject-of-cy", clock.Add(time.Minute)},
	} {
		if err := users.ExternalStepUp(t.Context(), cy.ID, sid, issuer, c.subject, c.at); !errors.Is(err, auth.ErrStepUpRefused) {
			t.Errorf("%s: %v, want ErrStepUpRefused", name, err)
		}
	}
	if err := users.RequireStepUp(t.Context(), cy.ID, sid); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Fatalf("a refused step-up freshened the session: %v", err)
	}
	at := clock.Add(-10 * time.Second)
	if err := users.ExternalStepUp(t.Context(), cy.ID, sid, issuer, "subject-of-cy", at); err != nil {
		t.Fatalf("Cy's own step-up: %v", err)
	}
	s, err := users.Session(t.Context(), sid)
	if err != nil || !s.AuthenticatedAt.Equal(at.Truncate(time.Second)) {
		t.Errorf("the step-up time is %v (%v), want the provider's %v", s.AuthenticatedAt, err, at)
	}
	// The mark was used.
	if err := users.ExternalStepUp(t.Context(), cy.ID, sid, issuer, "subject-of-cy", clock.Truncate(time.Second)); !errors.Is(err, auth.ErrStepUpRefused) {
		t.Errorf("a mark worked twice: %v", err)
	}
	// And a mark older than ten minutes proves nothing.
	if _, err := users.MarkExternalStepUp(t.Context(), cy.ID, sid); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(auth.StepUpWindow + time.Second)
	if err := users.ExternalStepUp(t.Context(), cy.ID, sid, issuer, "subject-of-cy", clock.Truncate(time.Second)); !errors.Is(err, auth.ErrStepUpRefused) {
		t.Errorf("an old mark: %v", err)
	}
}

func TestAnExternalSignInsStepUpTimeIsTheProvidersNeverTheSignIns(t *testing.T) {
	users, _, clock := newUsers(t)
	// A silent sign-in answered from the provider's own session carries the
	// person's last authentication, an hour ago: no window.
	in := external("subject-of-cy", "cy@example.com")
	in.AuthTime = clock.Add(-time.Hour)
	_, session, cy := signInExternal(t, users, in)
	if !session.AuthenticatedAt.Equal(clock.Add(-time.Hour).Truncate(time.Second)) {
		t.Errorf("step-up time %v, want the provider's", session.AuthenticatedAt)
	}
	if err := users.RequireStepUp(t.Context(), cy.ID, session.ID); !errors.Is(err, auth.ErrStepUpNeeded) {
		t.Errorf("a silent sign-in opened a window: %v", err)
	}
	// A time after the server's now is taken as now, never later.
	in.AuthTime = clock.Add(time.Hour)
	_, session, _ = signInExternal(t, users, in)
	if !session.AuthenticatedAt.Equal(clock.Truncate(time.Second)) {
		t.Errorf("step-up time %v, want at most now", session.AuthenticatedAt)
	}
}

func TestTheUpgradeChecksTheOldPasswordOnceAndNeverAgain(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	old := authtest.NewLegacyUser(t, db, "old@example.com", auth.RoleMember)
	if _, err := users.LegacySignIn(t.Context(), "old@example.com", "not the password"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("a wrong password: %v", err)
	}
	upgrade, err := users.LegacySignIn(t.Context(), " OLD@example.com", authtest.Password)
	if err != nil {
		t.Fatal(err)
	}
	// A ticket, not a session, with what the browser binds the new wraps to
	// and derives under: the person's seal id and the address's target.
	if n := liveSessions(t, db, old.ID); n != 0 {
		t.Fatalf("the upgrade's check opened %d sessions", n)
	}
	if upgrade.SealID != old.SealID || !bytes.Equal(upgrade.Salt, mustSalt(t, authtest.SaltKey, "old@example.com")) ||
		upgrade.KDF != auth.DefaultKDF {
		t.Fatalf("the upgrade's ticket came with %q, %x, %+v", upgrade.SealID, upgrade.Salt, upgrade.KDF)
	}
	ticket := upgrade.Ticket
	in := authtest.Enrolment(t)
	token, session, user, err := users.Enrol(t.Context(), ticket, in, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !user.Enrolled || !bytes.Equal(user.PublicKey, in.PublicKey) || user.SealID != old.SealID || session.AuthenticatedAt.IsZero() {
		t.Errorf("enrolled %+v, session %+v", user, session)
	}
	if _, err := users.AuthenticateSession(t.Context(), token); err != nil {
		t.Errorf("the enrolment's session: %v", err)
	}
	var hash string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT password_hash FROM users WHERE id = ?`, old.ID).Scan(&hash); err != nil || hash != "" {
		t.Errorf("the old hash is still %q (%v)", hash, err)
	}
	// From then on the password in clear is refused, as a wrong one is,
	// and the challenge says nothing of an upgrade.
	if _, err := users.LegacySignIn(t.Context(), "old@example.com", authtest.Password); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old password was taken again: %v", err)
	}
	if c, err := users.Challenge(t.Context(), "old@example.com"); err != nil || c.Upgrade {
		t.Errorf("challenge after the upgrade = %+v, %v", c, err)
	}
	if _, err := users.Login(t.Context(), "old@example.com", in.AuthKey, "test"); err != nil {
		t.Errorf("the auth key does not sign in: %v", err)
	}
	// Used once.
	if _, _, _, err := users.Enrol(t.Context(), ticket, authtest.Enrolment(t), "test"); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("an enrolment ticket worked twice: %v", err)
	}
}

func TestEnrolmentIsOneWayAndAnEnrolledPersonHasNoPasswordHash(t *testing.T) {
	_, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	for name, stmt := range map[string]string{
		"enrolment undone":         `UPDATE users SET zk_enrolled_at = 0 WHERE id = ?`,
		"enrolment moved":          `UPDATE users SET zk_enrolled_at = zk_enrolled_at + 1 WHERE id = ?`,
		"a password hash put back": `UPDATE users SET password_hash = '$argon2id$v=19$m=65536,t=3,p=1$x$y' WHERE id = ?`,
		"a verifier taken away":    `UPDATE users SET auth_verifier = '' WHERE id = ?`,
		"a wrap taken away":        `UPDATE users SET recovery_wrap = NULL WHERE id = ?`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), stmt, ana.ID); err == nil {
			t.Errorf("%s: the schema took it", name)
		}
	}
}

func TestAnAccountPublicKeyAndASealIDAreWrittenOnce(t *testing.T) {
	_, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	other := authtest.PublicKey(t)
	for name, c := range map[string]struct {
		stmt string
		args []any
	}{
		"another public key":    {`UPDATE users SET public_key = ? WHERE id = ?`, []any{other, ana.ID}},
		"no public key":         {`UPDATE users SET public_key = NULL WHERE id = ?`, []any{ana.ID}},
		"another seal id":       {`UPDATE users SET seal_id = ? WHERE id = ?`, []any{keyscheme.NewSealID(), ana.ID}},
		"a seal id in capitals": {`UPDATE users SET seal_id = upper(seal_id) WHERE id = ?`, []any{ana.ID}},
		"a duplicate seal id": {`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at,
			updated_at, seal_id) VALUES ('usr_dup', 'dup@example.com', '', 'member', 0, 0, 0, ?)`, []any{ana.SealID}},
		"a seal id that is no UUIDv4": {`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at,
			updated_at, seal_id) VALUES ('usr_bad', 'bad@example.com', '', 'member', 0, 0, 0, 'not-a-uuid')`, nil},
	} {
		if _, err := db.Writer().ExecContext(t.Context(), c.stmt, c.args...); err == nil {
			t.Errorf("%s: the schema took it", name)
		}
	}
	// A row written without a seal id gets one drawn.
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO users(id, email, password_hash, role,
		password_changed_at, created_at, updated_at) VALUES ('usr_raw', 'raw@example.com', '', 'member', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	var seal string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT seal_id FROM users WHERE id = 'usr_raw'`).Scan(&seal); err != nil ||
		!keyscheme.ValidSealID(seal) || seal == ana.SealID {
		t.Errorf("a row written without a seal id has %q (%v)", seal, err)
	}
}

func TestAResetInvitationGivesANewAccountKeyAndEndsEverySession(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bob := authtest.NewUser(t, db, "bob@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	bobs := authtest.SignIn(t, users, "bob@example.com")

	code, reset, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if reset.Email != "ana@example.com" || !reset.ExpiresAt.Equal(clock.Add(auth.ResetTTL).Truncate(time.Second)) {
		t.Errorf("reset = %+v", reset)
	}
	// Making it changes nothing yet.
	if _, err := users.AuthenticateSession(t.Context(), laptop); err != nil {
		t.Fatalf("issuing a reset ended a session: %v", err)
	}
	in := authtest.EnrolmentWith(t, secretOf("after reset"), secretOf("new code"))
	for name, try := range map[string]struct{ code, email string }{
		"another address": {code, "bob@example.com"},
		"another code":    {base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "ana@example.com"},
	} {
		if _, _, _, err := users.CompleteReset(t.Context(), try.code, try.email, in, "test"); !errors.Is(err, auth.ErrResetInvalid) {
			t.Errorf("%s: %v, want ErrResetInvalid", name, err)
		}
	}
	*clock = clock.Add(time.Hour)
	token, session, user, err := users.CompleteReset(t.Context(), code, "Ana@Example.com", in, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(user.PublicKey, in.PublicKey) || user.SealID != ana.SealID || !user.Enrolled || session.AuthenticatedAt.IsZero() {
		t.Errorf("after the reset: %+v, %+v", user, session)
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a session survived the reset: %v", err)
	}
	for _, tk := range []string{token, bobs} {
		if _, err := users.AuthenticateSession(t.Context(), tk); err != nil {
			t.Errorf("a session that should work does not: %v", err)
		}
	}
	if _, err := users.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old auth key still signs in: %v", err)
	}
	if _, err := users.Login(t.Context(), "ana@example.com", in.AuthKey, "test"); err != nil {
		t.Errorf("the new auth key does not sign in: %v", err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); !errors.Is(err, auth.ErrResetInvalid) {
		t.Errorf("a reset worked twice: %v", err)
	}
	if n := liveSessions(t, db, bob.ID); n != 1 {
		t.Errorf("bob has %d sessions, want his one", n)
	}
}

func TestAResetOfATeamMailboxsLastReaderNeedsForce(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)

	var blocked *auth.BlockedError
	if _, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli"); !errors.As(err, &blocked) ||
		len(blocked.LastReaderOf) != 1 || blocked.LastReaderOf[0] != mailbox {
		t.Fatalf("a reset of the last reader: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM reset_invites`); n != 0 {
		t.Fatalf("a refused reset was stored")
	}
	// Issued while she was not the last reader, it checks again when used.
	grantRead(t, db, mailbox, bea.ID)
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	revokeRead(t, db, mailbox, bea.ID)
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); !errors.As(err, &blocked) {
		t.Fatalf("a reset that would take the last reader: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM reset_invites`); n != 1 {
		t.Fatalf("the refused reset was spent")
	}
	// With force it goes through, and says so when used.
	code, reset, err := users.CreateReset(t.Context(), ana.ID, true, "cli")
	if err != nil || !reset.Forced {
		t.Fatalf("a forced reset: %+v, %v", reset, err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatalf("a forced reset: %v", err)
	}
}

func TestAResetOfTheOnlyMemberOfATeamWhoReadsItsMailboxNeedsForce(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	// Alone in her team: closing her account would take the team with it,
	// but a reset leaves both, and the mailbox with nobody who can read it.
	team := teamOf(t, db, ana.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)

	var blocked *auth.BlockedError
	if _, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli"); !errors.As(err, &blocked) ||
		len(blocked.LastReaderOf) != 1 || blocked.LastReaderOf[0] != mailbox {
		t.Fatalf("a reset of the only member, the mailbox's last reader: %v", err)
	}
	code, _, err := users.CreateReset(t.Context(), ana.ID, true, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatalf("a forced reset: %v", err)
	}
}

func TestAResetLinkAnswersTheTargetItsResetStores(t *testing.T) {
	cheapKDF(t)
	_, db, clock := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	// Another salt key: ana's account is off its target, as a replaced salt
	// key or an address the operator corrected leaves it.
	users := auth.NewUsersWithClock(db, func() time.Time { return *clock })
	before, err := users.Challenge(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]struct{ code, email string }{
		"another address": {code, "bob@example.com"},
		"another code":    {base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "ana@example.com"},
		"no code":         {"", "ana@example.com"},
	} {
		if _, err := users.OpenReset(t.Context(), try.code, try.email); !errors.Is(err, auth.ErrResetInvalid) {
			t.Errorf("%s: %v, want ErrResetInvalid", name, err)
		}
	}
	target, err := users.OpenReset(t.Context(), code, " Ana@Example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if target.SealID != ana.SealID {
		t.Errorf("the reset link answered the seal id %q, want the person's own %q", target.SealID, ana.SealID)
	}
	// The challenge answers the salt the account stores now; the new
	// password is derived under the target, which is what the reset stores.
	if bytes.Equal(target.Salt, before.Salt) || target.KDF != auth.DefaultKDF {
		t.Fatalf("the reset link answered %x %+v; the challenge %x: want the target, not the stored salt", target.Salt,
			target.KDF, before.Salt)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatal(err)
	}
	after, err := users.Challenge(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Salt, target.Salt) || after.KDF != target.KDF {
		t.Errorf("after the reset the challenge answers %x %+v, want what the link answered, %x %+v", after.Salt,
			after.KDF, target.Salt, target.KDF)
	}
	login, err := users.Login(t.Context(), "ana@example.com", authtest.AuthKey, "test")
	if err != nil || login.Rederive != nil {
		t.Errorf("the next sign-in: %+v, %v; want one at the target", login.Rederive, err)
	}
	if _, err := users.OpenReset(t.Context(), code, "ana@example.com"); !errors.Is(err, auth.ErrResetInvalid) {
		t.Errorf("a used reset link still opens: %v", err)
	}
}

func TestAResetLinkRefusesTheLastReaderBeforeAPasswordIsChosen(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)
	grantRead(t, db, mailbox, bea.ID)
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	revokeRead(t, db, mailbox, bea.ID)
	var blocked *auth.BlockedError
	if _, err := users.OpenReset(t.Context(), code, "ana@example.com"); !errors.As(err, &blocked) ||
		len(blocked.LastReaderOf) != 1 || blocked.LastReaderOf[0] != mailbox {
		t.Fatalf("a reset link of the last reader opened: %v", err)
	}
	forced, _, err := users.CreateReset(t.Context(), ana.ID, true, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.OpenReset(t.Context(), forced, "ana@example.com"); err != nil {
		t.Errorf("a forced reset link did not open: %v", err)
	}
}

func TestAResetInvitationIsRefusedToADisabledPersonAndDisablingDropsIt(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.OpenRecovery(t.Context(), "ana@example.com", authtest.RecoveryProof); err != nil {
		t.Fatal(err)
	}
	if err := users.SetDisabled(t.Context(), ana.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT (SELECT count(*) FROM reset_invites) + (SELECT count(*) FROM auth_tickets)`); n != 0 {
		t.Errorf("disabling left %d reset invitations and tickets", n)
	}
	if err := users.SetDisabled(t.Context(), ana.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); !errors.Is(err, auth.ErrResetInvalid) {
		t.Errorf("a reset made before the disabling worked after it: %v", err)
	}
}

// teamMailbox links a mailbox into a team, with read for linkedBy.
func teamMailbox(t *testing.T, db *store.Store, teamID, linkedBy string) string {
	t.Helper()
	id := "acc_" + strings.ReplaceAll(keyscheme.NewSealID(), "-", "")[:16]
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			 smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, linked_by)
			 VALUES (?, ?, ?, 'imap', 'password', 'imap.example.com', 993, 'smtp.example.com', 465, 'implicit', ?, 0,
			         'active', 0, 0, 0, ?)`,
			id, teamID, id+"@mail.example", id+"@mail.example", linkedBy); err != nil {
			return err
		}
		return workspace.GrantLinkTx(t.Context(), tx, id, teamID, linkedBy, time.Now())
	})
	if err != nil {
		t.Fatalf("link a team mailbox: %v", err)
	}
	return id
}

func grantRead(t *testing.T, db *store.Store, accountID, userID string) {
	t.Helper()
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read,
		act, send, manage, granted_by, created_at, updated_at)
		SELECT id, workspace_id, ?, 1, 0, 0, 0, '', 0, 0 FROM accounts WHERE id = ?`, userID, accountID); err != nil {
		t.Fatal(err)
	}
}

func revokeRead(t *testing.T, db *store.Store, accountID, userID string) {
	t.Helper()
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`,
		accountID, userID); err != nil {
		t.Fatal(err)
	}
}

// addMember adds an active member to a team.
func addMember(t *testing.T, db *store.Store, teamID, userID string) {
	t.Helper()
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.NewRepository(db, nil).AddMemberTx(t.Context(), tx, teamID, userID, workspace.RoleMember, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
}
