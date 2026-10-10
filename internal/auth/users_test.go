package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

func newUsers(t *testing.T) (*auth.Users, *store.Store, *time.Time) {
	t.Helper()
	s := storetest.New(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := &now
	users := auth.NewUsersWithClock(s, func() time.Time { return *clock }).WithSaltKey(authtest.SaltKey)
	return users, s, clock
}

// signUpRequest is a sign-up request for an invite, with a fresh enrolment
// bound to the seal id opening it answers, as a browser's is; an invite that
// does not open for the address gets none, and is refused when used.
func signUpRequest(t *testing.T, users *auth.Users, code, email string) auth.SignUpRequest {
	t.Helper()
	req := auth.SignUpRequest{Invite: code, Email: email, Enrolment: authtest.Enrolment(t), UserAgent: "test"}
	if opened, err := users.OpenSignUp(t.Context(), code, email); err == nil {
		req.SealID = opened.SealID
	}
	return req
}

// cheapKDF stands in for Argon2id where a test is about what is hashed and
// when, not about the hash: a real sign-up hashes two verifiers at 19 MiB.
func cheapKDF(t *testing.T) {
	t.Helper()
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		return argon2.IDKey(password, salt, 1, 8, 1, keyLen)
	})
}

// invite makes an invite and returns its code.
func invite(t *testing.T, users *auth.Users, email string, role auth.Role) string {
	t.Helper()
	code, _, err := users.CreateInvite(t.Context(), auth.NewInvite{Email: email, Role: role, CreatedBy: "cli"})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return code
}

func TestHashingPeoplesSecretsIsBoundedToTwoAtATime(t *testing.T) {
	// 19 MiB a verifier: a burst of sign-ins with no bound is a way to run
	// the daemon out of memory that the rate limiter only slows down.
	var running, peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	auth.SetDeriveKeyForTest(t, func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		started <- struct{}{}
		<-release
		running.Add(-1)
		return make([]byte, keyLen)
	})

	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := auth.HashVerifierForTest(context.Background(), "correct horse battery"); err != nil {
				t.Errorf("hash: %v", err)
			}
		})
	}
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("a third hash started while two were running")
	case <-time.After(100 * time.Millisecond):
	}

	// Waiting honours the caller: a request whose client went away leaves
	// the queue instead of holding a place in it.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := auth.HashVerifierForTest(ctx, "correct horse battery"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller waiting for a slot got %v, want context.Canceled", err)
	}

	close(release)
	wg.Wait()
	if got := peak.Load(); got != 2 {
		t.Fatalf("at most %d hashes ran at once, want exactly 2", got)
	}
}

func TestEveryWayASignInFailsLooksTheSame(t *testing.T) {
	// Same error, and the same work: exactly one derivation at a verifier's
	// full cost for an address with no account, a disabled person, a person
	// who has not enrolled (one from before the key scheme, whose old hash no
	// sign-in checks) and a wrong auth key. A miss that returned early would
	// time out as a free oracle for which addresses have accounts.
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	gone := authtest.NewUser(t, db, "gone@example.com", auth.RoleMember)
	if err := users.SetDisabled(t.Context(), gone.ID, true); err != nil {
		t.Fatal(err)
	}
	authtest.NewLegacyUser(t, db, "old@example.com", auth.RoleMember)

	type call struct{ memory, passes uint32 }
	var calls []call
	var mu sync.Mutex
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		mu.Lock()
		calls = append(calls, call{memory, passes})
		mu.Unlock()
		return argon2.IDKey(password, salt, 1, 8, threads, keyLen)
	})

	memory, passes := auth.VerifierCostForTest()
	wrong := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, attempt := range []struct{ name, email, key string }{
		{"unknown address", "nobody@example.com", authtest.AuthKey},
		{"disabled person", "gone@example.com", authtest.AuthKey},
		{"person not enrolled", "old@example.com", authtest.AuthKey},
		{"wrong auth key", "ana@example.com", wrong},
	} {
		calls = nil
		_, err := users.Login(t.Context(), attempt.email, attempt.key, "test")
		if !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("%s: err = %v, want ErrBadCredentials", attempt.name, err)
		}
		if len(calls) != 1 {
			t.Errorf("%s: %d derivations, want exactly 1", attempt.name, len(calls))
			continue
		}
		// The test users carry a cheap verifier, so the wrong key is checked
		// at its stored cost; the others must be at the full one.
		if attempt.name != "wrong auth key" && (calls[0].memory != memory || calls[0].passes != passes) {
			t.Errorf("%s: derived at m=%d t=%d, want the real cost m=%d t=%d",
				attempt.name, calls[0].memory, calls[0].passes, memory, passes)
		}
	}
}

func TestAnInviteWorksOnceAndOnlyForItsEmail(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	req := signUpRequest(t, users, code, "mallory@example.com")
	if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("another address redeemed the invite: %v", err)
	}
	// Refusing the wrong address must not have spent it.
	req = signUpRequest(t, users, code, "Ana@Example.com")
	_, _, user, err := users.SignUp(t.Context(), req)
	if err != nil {
		t.Fatalf("the invited address could not sign up: %v", err)
	}
	if user.Email != "ana@example.com" || user.Role != auth.RoleMember {
		t.Errorf("user = %+v", user)
	}
	req.Email = "ana@example.com"
	if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("an invite worked twice: %v", err)
	}
}

func TestAnInviteIsForItsAddressExactlyNeverOneThatFoldsToIt(t *testing.T) {
	// Unicode's simple case folding takes U+017F (ſ) for an s, and the final
	// sigma for a sigma: a fold would let an invite for one address create an
	// account at another, and leave the invited address's other invites
	// waiting to create a second one (docs/key-scheme.md section 2).
	cheapKDF(t)
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	for invited, folded := range map[string]string{
		"sam@example.com": "\u017fam@example.com",
		"σx@example.com":  "ςx@example.com",
	} {
		code := invite(t, users, invited, auth.RoleMember)
		invite(t, users, invited, auth.RoleOwner)
		if _, err := users.OpenSignUp(t.Context(), code, folded); !errors.Is(err, auth.ErrInviteInvalid) {
			t.Errorf("an invite for %s opened for %q: %v", invited, folded, err)
		}
		req := auth.SignUpRequest{Invite: code, Email: folded, SealID: keyscheme.NewSealID(), Enrolment: authtest.Enrolment(t),
			UserAgent: "test"}
		if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, auth.ErrInviteInvalid) {
			t.Errorf("an invite for %s signed up %q: %v", invited, folded, err)
		}
		if n := count(t, db, `SELECT count(*) FROM users WHERE email = ?`, folded); n != 0 {
			t.Errorf("%q has an account", folded)
		}
		if n := count(t, db, `SELECT count(*) FROM invites WHERE email = ? AND used_at = 0`, invited); n != 2 {
			t.Errorf("%s has %d invites waiting, want both", invited, n)
		}
	}

	// Nor does a person whose address folds to a team invite's accept it.
	sam := authtest.NewUser(t, db, "\u017fam@example.com", auth.RoleMember)
	team := teamOf(t, db, owner.ID)
	code, _ := teamInvite(t, users, "sam@example.com", team.ID, workspace.RoleAdmin)
	if _, err := users.AcceptInvite(t.Context(), sam.ID, code); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("a team invite for sam@ was accepted by %q: %v", sam.Email, err)
	}
	if n := count(t, db, `SELECT count(*) FROM invites WHERE workspace_id = ? AND used_at = 0`, team.ID); n != 1 {
		t.Errorf("the refused acceptance spent the invite")
	}
}

func TestAnExpiredInviteIsRefused(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	*clock = clock.Add(auth.InviteTTL + time.Second)
	_, _, _, err := users.SignUp(t.Context(), signUpRequest(t, users, code, "ana@example.com"))
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("err = %v, want ErrInviteInvalid", err)
	}
}

func TestTheFirstSignUpIsNoOwnerUnlessInvitedAsOne(t *testing.T) {
	// The invite decides the role, when it is made; signing up first
	// decides nothing. An operator who invites a colleague as a member before
	// inviting themselves as the owner gets exactly that.
	cheapKDF(t)
	users, _, _ := newUsers(t)
	colleague := invite(t, users, "colleague@example.com", auth.RoleMember)
	owner := invite(t, users, "owner@example.com", auth.RoleOwner)

	signUp := func(code, email string) auth.User {
		t.Helper()
		_, _, user, err := users.SignUp(t.Context(), signUpRequest(t, users, code, email))
		if err != nil {
			t.Fatalf("sign up %s: %v", email, err)
		}
		return user
	}
	if got := signUp(colleague, "colleague@example.com"); got.Role != auth.RoleMember {
		t.Errorf("the first person to sign up, invited as a member, is %q", got.Role)
	}
	if got := signUp(owner, "owner@example.com"); got.Role != auth.RoleOwner {
		t.Errorf("the person invited as an owner is %q", got.Role)
	}
}

func TestAnEnrolmentOutOfShapeIsRefusedBeforeTheInviteIsSpent(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	lowOrder := make([]byte, 32) // the all-zero point: no secret is agreed with it
	for name, c := range map[string]struct {
		change func(*auth.Enrolment)
		want   error
	}{
		"an auth key of 31 bytes": {func(e *auth.Enrolment) {
			e.AuthKey = base64.RawURLEncoding.EncodeToString(make([]byte, 31))
		}, auth.ErrMalformedSecret},
		"an auth key with padding": {func(e *auth.Enrolment) { e.AuthKey += "=" }, auth.ErrMalformedSecret},
		"the password itself":      {func(e *auth.Enrolment) { e.AuthKey = authtest.Password }, auth.ErrMalformedSecret},
		"a proof of 33 bytes": {func(e *auth.Enrolment) {
			e.RecoveryProof = base64.RawURLEncoding.EncodeToString(make([]byte, 33))
		}, auth.ErrMalformedSecret},
		"cheaper parameters":       {func(e *auth.Enrolment) { e.KDF.M = 32768 }, auth.ErrKDFNotCurrent},
		"more passes":              {func(e *auth.Enrolment) { e.KDF.T = 4 }, auth.ErrKDFNotCurrent},
		"a low-order public key":   {func(e *auth.Enrolment) { e.PublicKey = lowOrder }, auth.ErrInvalidPublicKey},
		"a public key of 31 bytes": {func(e *auth.Enrolment) { e.PublicKey = e.PublicKey[:31] }, auth.ErrInvalidPublicKey},
		"a platform wrap's header": {func(e *auth.Enrolment) { e.PasswordWrap[0] = 0x03 }, auth.ErrInvalidWrap},
		"a recovery wrap of 60 bytes": {func(e *auth.Enrolment) {
			e.RecoveryWrap = e.RecoveryWrap[:60]
		}, auth.ErrInvalidWrap},
	} {
		req := signUpRequest(t, users, code, "ana@example.com")
		c.change(&req.Enrolment)
		if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if _, _, _, err := users.SignUp(t.Context(), signUpRequest(t, users, code, "ana@example.com")); err != nil {
		t.Fatalf("the invite did not survive the refused attempts: %v", err)
	}
}

func TestASessionTokenIsStoredOnlyAsItsHash(t *testing.T) {
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, users, "ana@example.com")

	if len(token) != 43 || strings.Contains(token, ".") {
		t.Fatalf("token %q: want 43 base64url characters with no dot, so it can never look like a key", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q does not decode to 32 bytes: %v", token, err)
	}
	sum := sha256.Sum256(raw)
	count := func(query string, v any) int {
		t.Helper()
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), query, v).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Keyed by the hash, found by the hash, and nothing else.
	if count(`SELECT count(*) FROM sessions WHERE token_hash = ?`, sum[:]) != 1 {
		t.Fatal("the session row is not keyed by the SHA-256 of the token")
	}
	if count(`SELECT count(*) FROM sessions WHERE token_hash = ?`, raw) != 0 {
		t.Fatal("the raw token bytes are in the database")
	}
	if count(`SELECT count(*) FROM sessions WHERE CAST(token_hash AS TEXT) LIKE '%' || ? || '%'`, token) != 0 {
		t.Fatal("the encoded token is in the database")
	}
}

func TestAnExpiredSessionIsRejected(t *testing.T) {
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, users, "ana@example.com")

	p, err := users.AuthenticateSession(t.Context(), token)
	if err != nil {
		t.Fatalf("a fresh session was refused: %v", err)
	}
	if !p.IsSession() || p.UserRole != auth.RoleMember || p.Scope != auth.ScopeAdmin {
		t.Errorf("principal = %+v", p)
	}

	// Absolute: using the session every day does not extend it.
	for range 13 {
		*clock = clock.Add(24 * time.Hour)
		if _, err := users.AuthenticateSession(t.Context(), token); err != nil {
			t.Fatalf("refused before the TTL ran out: %v", err)
		}
	}
	*clock = clock.Add(24 * time.Hour)
	if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("a session past its 14 days still works: %v", err)
	}
	if err := users.RecheckSession(t.Context(), p); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("recheck accepted an expired session: %v", err)
	}
}

func TestADisabledUsersSessionsStopWorking(t *testing.T) {
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, users, "ana@example.com")
	p, err := users.AuthenticateSession(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewKeys(db)
	personal := authtest.Personal(t, db, ana.ID)
	keySecret, _, err := keys.Issue(t.Context(), auth.NewKeyRequest{Name: "ana's agent", Scope: auth.ScopeRead,
		WorkspaceID: personal, CreatedBy: ana.ID, TermsVersion: "terms"})
	if err != nil {
		t.Fatal(err)
	}
	kp, err := keys.Authenticate(t.Context(), keySecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kp.UserID != "" || kp.IsInstance() || !kp.IsWorkspaceKey() || kp.WorkspaceID != personal || kp.CreatedBy != ana.ID {
		t.Fatalf("a key of Ana's workspace is %+v: it acts as nobody, and she answers for it", kp)
	}

	if err := users.SetDisabled(t.Context(), ana.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a disabled user's session still works: %v", err)
	}
	if err := users.RecheckSession(t.Context(), p); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("recheck accepted a disabled user's session: %v", err)
	}
	// Every way in, not only the browser: the keys they created stop.
	if _, err := keys.Authenticate(t.Context(), keySecret, nil); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("a key a disabled user created still works: %v", err)
	}
	if err := keys.Recheck(t.Context(), kp); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("recheck accepted a key a disabled user created: %v", err)
	}

	// Switching the account back on does not revive the sessions it had.
	if err := users.SetDisabled(t.Context(), ana.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("re-enabling brought an old session back: %v", err)
	}
	if _, err := keys.Authenticate(t.Context(), keySecret, nil); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("re-enabling brought a key back: %v", err)
	}
}

func TestSigningOutEndsOnlyThatSessionUnlessAskedForAll(t *testing.T) {
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	phone := authtest.SignIn(t, users, "ana@example.com")
	tablet := authtest.SignIn(t, users, "ana@example.com")

	p, err := users.AuthenticateSession(t.Context(), laptop)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.EndSession(t.Context(), p.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("the ended session still works: %v", err)
	}
	if _, err := users.AuthenticateSession(t.Context(), phone); err != nil {
		t.Fatalf("ending one session ended another: %v", err)
	}
	if err := users.EndAllSessions(t.Context(), ana.ID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{phone, tablet} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Fatalf("a session survived signing out everywhere: %v", err)
		}
	}
}

func TestAMalformedSessionTokenIsRefusedWithTheSameError(t *testing.T) {
	users, _, _ := newUsers(t)
	for _, token := range []string{"", "short", strings.Repeat("A", 43), "!!!!" + strings.Repeat("A", 39), strings.Repeat("A", 44)} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("token %q: err = %v", token, err)
		}
	}
}

func TestAnInviteLinkCarriesItsSecretsInTheFragment(t *testing.T) {
	// A fragment never reaches a server: not this one's access log, not a
	// proxy, not a Referer header.
	link := auth.InviteLink("https://console.example/", "c0de-_x", "a+b@example.com")
	if link != "https://console.example/#invite=c0de-_x&email=a%2Bb%40example.com" {
		t.Fatalf("link = %q", link)
	}
}

func TestAFullHashingQueueIsRefusedAtOnce(t *testing.T) {
	// Two hashing and a few waiting is a second or two. Anything beyond that
	// is an attack filling the queue, and a request behind it is told to come
	// back rather than held until its deadline.
	running, queue, _, _ := auth.HashLimitsForTest()
	var now atomic.Int32
	release := make(chan struct{})
	auth.SetDeriveKeyForTest(t, func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		now.Add(1)
		<-release
		now.Add(-1)
		return make([]byte, keyLen)
	})

	var wg sync.WaitGroup
	for range running + queue {
		wg.Go(func() {
			if _, err := auth.HashVerifierForTest(context.Background(), "correct horse battery"); err != nil {
				t.Errorf("a queued hash failed: %v", err)
			}
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting, _ := auth.HashWaitersForTest()
		if int(now.Load()) == running && waiting == queue {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d running and %d waiting; want %d and %d", now.Load(), waiting, running, queue)
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	if _, err := auth.HashVerifierForTest(t.Context(), "correct horse battery"); !errors.Is(err, auth.ErrHashBusy) {
		t.Errorf("a hash behind a full queue: %v, want ErrHashBusy", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the refusal took %v; it should not wait", waited)
	}
	close(release)
	wg.Wait()
}
