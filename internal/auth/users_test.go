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
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func newUsers(t *testing.T) (*auth.Users, *store.Store, *time.Time) {
	t.Helper()
	s := storetest.New(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := &now
	return auth.NewUsersWithClock(s, func() time.Time { return *clock }), s, clock
}

// cheapKDF stands in for Argon2id where a test is about what is hashed and
// when, not about the hash: a real sign-up costs over a second under -race.
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

func TestPasswordHashingIsBoundedToTwoAtATime(t *testing.T) {
	// 64 MiB a hash: a burst of sign-ins with no bound is a way to run the
	// daemon out of memory that the rate limiter only slows down.
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
			if _, err := auth.HashPasswordForTest(context.Background(), "correct horse battery"); err != nil {
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
	if _, err := auth.HashPasswordForTest(ctx, "correct horse battery"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller waiting for a slot got %v, want context.Canceled", err)
	}

	close(release)
	wg.Wait()
	if got := peak.Load(); got != 2 {
		t.Fatalf("at most %d hashes ran at once, want exactly 2", got)
	}
}

func TestAnUnknownEmailAnswersLikeAWrongPassword(t *testing.T) {
	// Same error, and the same work: exactly one derivation at the full cost
	// for an address with no account, a wrong password, and a disabled
	// account. A miss that returned early would time out as a free oracle for
	// which addresses have accounts.
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	gone := authtest.NewUser(t, db, "gone@example.com", auth.RoleMember)
	if err := users.SetDisabled(t.Context(), gone.ID, true); err != nil {
		t.Fatal(err)
	}

	type call struct{ memory, passes uint32 }
	var calls []call
	var mu sync.Mutex
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		mu.Lock()
		calls = append(calls, call{memory, passes})
		mu.Unlock()
		return argon2.IDKey(password, salt, 1, 8, threads, keyLen)
	})

	memory, passes := auth.PasswordCostForTest()
	for _, attempt := range []struct{ name, email, password string }{
		{"unknown address", "nobody@example.com", authtest.Password},
		{"disabled account", "gone@example.com", authtest.Password},
		{"wrong password", "ana@example.com", "not the password at all"},
	} {
		calls = nil
		_, _, _, err := users.SignIn(t.Context(), attempt.email, attempt.password, "test")
		if !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("%s: err = %v, want ErrBadCredentials", attempt.name, err)
		}
		if len(calls) != 1 {
			t.Errorf("%s: %d derivations, want exactly 1", attempt.name, len(calls))
			continue
		}
		// The test users carry a cheap hash, so the wrong password is checked
		// at their stored cost; the other two must be at the full one.
		if attempt.name != "wrong password" && (calls[0].memory != memory || calls[0].passes != passes) {
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

	req := auth.SignUpRequest{Invite: code, Email: "mallory@example.com", Name: "M", Password: "long enough password"}
	if _, _, _, err := users.SignUp(t.Context(), req); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("another address redeemed the invite: %v", err)
	}
	// Refusing the wrong address must not have spent it.
	req.Email = "Ana@Example.com"
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

func TestAnExpiredInviteIsRefused(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	*clock = clock.Add(auth.InviteTTL + time.Second)
	_, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{
		Invite: code, Email: "ana@example.com", Password: "long enough password",
	})
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("err = %v, want ErrInviteInvalid", err)
	}
}

func TestTheFirstUserIsAlwaysAnOwner(t *testing.T) {
	// Otherwise a daemon whose first invite said "member" would have nobody
	// able to invite anyone else.
	cheapKDF(t)
	users, _, _ := newUsers(t)
	code, inv, err := users.CreateInvite(t.Context(), auth.NewInvite{Email: "first@example.com", Role: auth.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Role != auth.RoleMember {
		t.Errorf("the first invite says %q, want the member it was asked for", inv.Role)
	}
	_, _, first, err := users.SignUp(t.Context(), auth.SignUpRequest{
		Invite: code, Email: "first@example.com", Password: "long enough password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != auth.RoleOwner {
		t.Fatalf("first user role = %q, want owner", first.Role)
	}

	code = invite(t, users, "second@example.com", auth.RoleMember)
	_, _, second, err := users.SignUp(t.Context(), auth.SignUpRequest{
		Invite: code, Email: "second@example.com", Password: "long enough password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != auth.RoleMember {
		t.Fatalf("second user role = %q, want the member the invite asked for", second.Role)
	}
}

func TestOnlyTheFirstPersonIsPromotedToOwner(t *testing.T) {
	// An operator who invites themselves and a colleague before anyone has
	// signed up asked for one owner, not two: the promotion belongs to
	// whoever signs up first, not to every invite written before then.
	cheapKDF(t)
	users, _, _ := newUsers(t)
	first := invite(t, users, "first@example.com", auth.RoleMember)
	colleague := invite(t, users, "colleague@example.com", auth.RoleMember)

	signUp := func(code, email string) auth.User {
		t.Helper()
		_, _, user, err := users.SignUp(t.Context(), auth.SignUpRequest{
			Invite: code, Email: email, Password: "long enough password",
		})
		if err != nil {
			t.Fatalf("sign up %s: %v", email, err)
		}
		return user
	}
	if got := signUp(first, "first@example.com"); got.Role != auth.RoleOwner {
		t.Errorf("the first person is %q, want owner", got.Role)
	}
	if got := signUp(colleague, "colleague@example.com"); got.Role != auth.RoleMember {
		t.Errorf("the colleague invited as a member before anyone signed up is %q", got.Role)
	}
}

func TestAWeakPasswordIsRefusedBeforeTheInviteIsSpent(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	code := invite(t, users, "ana@example.com", auth.RoleMember)

	for _, password := range []string{"short", strings.Repeat("x", auth.MaxPasswordBytes+1)} {
		_, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{Invite: code, Email: "ana@example.com", Password: password})
		if !errors.Is(err, auth.ErrPasswordTooShort) && !errors.Is(err, auth.ErrPasswordTooLong) {
			t.Fatalf("password of %d bytes: err = %v", len(password), err)
		}
	}
	if _, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{
		Invite: code, Email: "ana@example.com", Password: "long enough password",
	}); err != nil {
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
	keySecret, _, err := keys.Issue(t.Context(), auth.NewKeyRequest{Name: "ana's agent", Scope: auth.ScopeRead, UserID: ana.ID})
	if err != nil {
		t.Fatal(err)
	}
	kp, err := keys.Authenticate(t.Context(), keySecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kp.UserID != ana.ID || kp.IsInstance() {
		t.Fatalf("a key issued for a user does not act as them: %+v", kp)
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
	// Every way in, not only the browser.
	if _, err := keys.Authenticate(t.Context(), keySecret, nil); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("a disabled user's key still works: %v", err)
	}
	if err := keys.Recheck(t.Context(), kp); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("recheck accepted a disabled user's key: %v", err)
	}

	// Switching the account back on does not revive the sessions it had.
	if err := users.SetDisabled(t.Context(), ana.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("re-enabling brought an old session back: %v", err)
	}
}

func TestAPasswordChangeEndsEverySessionAndIssuesANewOne(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	phone := authtest.SignIn(t, users, "ana@example.com")

	if _, _, err := users.ChangePassword(t.Context(), ana.ID, "wrong", "a brand new password", "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("a wrong current password was accepted: %v", err)
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); err != nil {
		t.Fatalf("a refused change still ended a session: %v", err)
	}

	fresh, _, err := users.ChangePassword(t.Context(), ana.ID, authtest.Password, "a brand new password", "test")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	for name, token := range map[string]string{"laptop": laptop, "phone": phone} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("the %s session survived the change: %v", name, err)
		}
	}
	if _, err := users.AuthenticateSession(t.Context(), fresh); err != nil {
		t.Errorf("the new session does not work: %v", err)
	}
	if _, _, _, err := users.SignIn(t.Context(), "ana@example.com", authtest.Password, "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old password still signs in: %v", err)
	}
	if _, _, _, err := users.SignIn(t.Context(), "ana@example.com", "a brand new password", "test"); err != nil {
		t.Errorf("the new password does not sign in: %v", err)
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

func TestAFullPasswordQueueIsRefusedAtOnce(t *testing.T) {
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
			if _, err := auth.HashPasswordForTest(context.Background(), "correct horse battery"); err != nil {
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
	if _, err := auth.HashPasswordForTest(t.Context(), "correct horse battery"); !errors.Is(err, auth.ErrHashBusy) {
		t.Errorf("a hash behind a full queue: %v, want ErrHashBusy", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the refusal took %v; it should not wait", waited)
	}
	close(release)
	wg.Wait()
}
