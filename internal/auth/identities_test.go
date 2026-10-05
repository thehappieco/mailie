package auth_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// issuer is the identity provider the tests below sign people in through.
const issuer = "https://accounts.example.com"

// external is an identity seen by the provider, with a verified address and
// a session of a day.
func external(subject, email string) auth.ExternalSignIn {
	return auth.ExternalSignIn{
		Issuer: issuer, Subject: subject, Email: email, EmailVerified: true,
		Name: "Cy Lima", UserAgent: "test", TTL: 24 * time.Hour,
	}
}

// signInExternal signs in through the provider, failing the test on a refusal.
func signInExternal(t *testing.T, users *auth.Users, in auth.ExternalSignIn) (string, auth.Session, auth.User) {
	t.Helper()
	token, session, user, err := users.SignInExternal(t.Context(), in)
	if err != nil {
		t.Fatalf("SignInExternal(%s, %s): %v", in.Subject, in.Email, err)
	}
	return token, session, user
}

func count(t *testing.T, db *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnExternalSessionNeverOutlivesItsTTL(t *testing.T) {
	users, db, clock := newUsers(t)
	start := clock.Truncate(time.Second)
	token, session, _ := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	if !session.CreatedAt.Equal(start) || !session.ExpiresAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("session from %v to %v, want a day from %v", session.CreatedAt, session.ExpiresAt, start)
	}

	// Used every few hours, right up to the last second: nothing renews it.
	for _, at := range []time.Duration{time.Hour, 6 * time.Hour, 12 * time.Hour, 24*time.Hour - time.Second} {
		*clock = start.Add(at)
		if _, err := users.AuthenticateSession(t.Context(), token); err != nil {
			t.Fatalf("refused %v in, before its day was up: %v", at, err)
		}
	}
	*clock = start.Add(24 * time.Hour)
	p, err := users.AuthenticateSession(t.Context(), token)
	if !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("a day-long session still works a day later: %v, %+v", err, p)
	}

	// Nor can anything write it a longer life: the schema refuses to move
	// an expiry later, whatever does the writing. An update; an upsert; and
	// a replace, which deletes the row it collides with, under the
	// session's id or its token, and would not fire an update trigger.
	later := `SELECT %s, user_id, token_hash, user_agent, created_at, last_seen_at, expires_at + 3600, revoked_at
	            FROM sessions WHERE id = ?`
	for _, stmt := range []string{
		`UPDATE sessions SET expires_at = expires_at + 3600 WHERE id = ?`,
		`UPDATE OR REPLACE sessions SET expires_at = expires_at + 3600 WHERE id = ?`,
		`INSERT OR REPLACE INTO sessions ` + fmt.Sprintf(later, "id"),
		`REPLACE INTO sessions ` + fmt.Sprintf(later, "id"),
		`INSERT OR REPLACE INTO sessions ` + fmt.Sprintf(later, "'ses_another'"),
		`INSERT INTO sessions ` + fmt.Sprintf(later, "id") + ` ON CONFLICT(id) DO UPDATE SET expires_at = excluded.expires_at`,
	} {
		_, err = db.Writer().ExecContext(t.Context(), stmt, session.ID)
		if err == nil || !strings.Contains(err.Error(), "never lasts longer") {
			t.Errorf("%s: an expiry was moved later (%v)", stmt, err)
		}
	}
	stored, err := users.Session(t.Context(), session.ID)
	if err != nil || !stored.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("stored expiry %v (%v), want %v", stored.ExpiresAt, err, session.ExpiresAt)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("%d sessions, want the one started", n)
	}
	// Ending one sooner is still everybody's right.
	if _, err := db.Writer().ExecContext(t.Context(),
		`UPDATE sessions SET expires_at = expires_at - 60 WHERE id = ?`, session.ID); err != nil {
		t.Fatalf("an expiry could not be brought forward: %v", err)
	}
}

func TestAnExternalSessionLastsMoreThanNothingAndNoLongerThanAPasswordSession(t *testing.T) {
	users, db, _ := newUsers(t)
	for _, ttl := range []time.Duration{0, -time.Hour, auth.SessionTTL + time.Second, 30 * 24 * time.Hour} {
		in := external("subject-of-cy", "cy@example.com")
		in.TTL = ttl
		if _, _, _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrInvalidSessionTTL) {
			t.Errorf("a session of %v: %v, want ErrInvalidSessionTTL", ttl, err)
		}
	}
	// Refused before anything was written: no person, no identity.
	if n := count(t, db, `SELECT count(*) FROM users`) + count(t, db, `SELECT count(*) FROM user_identities`); n != 0 {
		t.Fatalf("a refused sign-in left %d rows", n)
	}
	in := external("subject-of-cy", "cy@example.com")
	in.TTL = auth.SessionTTL
	if _, session, _ := signInExternal(t, users, in); session.ExpiresAt.Sub(session.CreatedAt) != auth.SessionTTL {
		t.Errorf("a session asked for the longest gets %v", session.ExpiresAt.Sub(session.CreatedAt))
	}
}

func TestANewExternalPersonIsAMemberWithTheirWorkspaceAndNoPassword(t *testing.T) {
	users, db, _ := newUsers(t)
	// An owner invitation waiting for the address, and a team's: the
	// instance's is spent with the sign-in, as signing up spends it, and the
	// team's waits to be accepted.
	invite(t, users, "cy@example.com", auth.RoleOwner)
	bob := authtest.NewUser(t, db, "bob@example.com", auth.RoleOwner)
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", bob.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := users.CreateInvite(t.Context(), auth.NewInvite{
		Email: "cy@example.com", WorkspaceID: team.ID, WorkspaceRole: workspace.RoleMember, CreatedBy: bob.ID,
	}); err != nil {
		t.Fatal(err)
	}

	in := external("subject-of-cy", "Cy@Example.com")
	in.Name = "  Cy Lima  "
	_, _, cy := signInExternal(t, users, in)
	// Never an owner, whatever was waiting for the address.
	if cy.Role != auth.RoleMember || cy.Email != "cy@example.com" || cy.Name != "Cy Lima" || cy.HasPassword ||
		!cy.PasswordChangedAt.IsZero() || cy.Disabled {
		t.Fatalf("created %+v", cy)
	}
	if got, err := users.Get(t.Context(), cy.ID); err != nil || got != cy {
		t.Fatalf("stored %+v (%v), want %+v", got, err, cy)
	}
	if _, err := workspace.NewRepository(db, nil).PersonalOf(t.Context(), cy.ID); err != nil {
		t.Fatalf("no personal workspace: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM invites WHERE email = 'cy@example.com' AND workspace_id IS NULL`); n != 0 {
		t.Errorf("%d instance invitations still wait for an address that has an account", n)
	}
	if n := count(t, db, `SELECT count(*) FROM invites WHERE email = 'cy@example.com' AND workspace_id = ?`, team.ID); n != 1 {
		t.Errorf("the team's invitation went with the sign-in (%d left)", n)
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities WHERE issuer = ? AND subject = 'subject-of-cy' AND user_id = ?`,
		issuer, cy.ID); n != 1 {
		t.Errorf("the identity is linked %d times", n)
	}

	// The next sign-in is the same person, not another one.
	_, _, again := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	if again.ID != cy.ID || count(t, db, `SELECT count(*) FROM users`) != 2 {
		t.Errorf("signing in again made %+v", again)
	}
}

func TestALinkedAddressNeedsAVerifiedEmail(t *testing.T) {
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)

	// Neither linked to the person who has the address, nor a new person:
	// an address nobody verified proves nothing about whose it is.
	for _, email := range []string{"Ana@Example.com", "nobody@example.com"} {
		in := external("subject-of-someone", email)
		in.EmailVerified = false
		if _, _, _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrEmailNotVerified) {
			t.Errorf("%s, unverified: %v, want ErrEmailNotVerified", email, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities`); n != 0 {
		t.Fatalf("an unverified address linked %d identities", n)
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("an unverified address created a person (%d people)", n)
	}

	// Verified, the address is the person's, compared as sign-in compares it.
	token, _, user := signInExternal(t, users, external("subject-of-ana", "ANA@example.com"))
	if user.ID != ana.ID || !user.HasPassword {
		t.Fatalf("signed in %+v, want ana, who keeps her password", user)
	}
	if p, err := users.AuthenticateSession(t.Context(), token); err != nil || p.UserID != ana.ID {
		t.Fatalf("the session is %+v (%v)", p, err)
	}
	// She still signs in with her password too.
	if _, _, _, err := users.SignIn(t.Context(), "ana@example.com", authtest.Password, "test"); err != nil {
		t.Errorf("linking an identity took her password away: %v", err)
	}

	// Once linked, the identity is hers whatever the provider says of the
	// address now: verified or not, changed or not.
	later := external("subject-of-ana", "ana.new@example.com")
	later.EmailVerified = false
	if _, _, again := signInExternal(t, users, later); again.ID != ana.ID || again.Email != "ana@example.com" {
		t.Errorf("the linked identity signed in %+v", again)
	}
}

func TestAPersonSignsInWithOneSubjectPerIssuer(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-one", "cy@example.com"))

	// Another subject from the same issuer, claiming the same address: it is
	// not the identity she signs in with, and it does not become a second.
	if _, _, _, err := users.SignInExternal(t.Context(), external("subject-two", "cy@example.com")); !errors.Is(err, auth.ErrIdentityConflict) {
		t.Fatalf("a second subject for her address: %v, want ErrIdentityConflict", err)
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities WHERE user_id = ?`, cy.ID); n != 1 {
		t.Fatalf("she has %d identities", n)
	}

	// Another issuer is another provider: one subject there too.
	other := external("subject-one", "cy@example.com")
	other.Issuer = "https://login.example.org"
	if _, _, user := signInExternal(t, users, other); user.ID != cy.ID {
		t.Errorf("the other provider's identity signed in %s, want %s", user.ID, cy.ID)
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities WHERE user_id = ?`, cy.ID); n != 2 {
		t.Errorf("she has %d identities, want one per issuer", n)
	}
}

func TestADisabledPersonIsRefusedAnExternalSignInAndNothingIsLinked(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	for _, id := range []string{cy.ID, ana.ID} {
		if err := users.SetDisabled(t.Context(), id, true); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, _, err := users.SignInExternal(t.Context(), external("subject-of-cy", "cy@example.com")); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("a disabled person's identity: %v, want ErrUserDisabled", err)
	}
	if _, _, _, err := users.SignInExternal(t.Context(), external("subject-of-ana", "ana@example.com")); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("a disabled person's address: %v, want ErrUserDisabled", err)
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities WHERE user_id = ?`, ana.ID); n != 0 {
		t.Errorf("a refused sign-in linked an identity to her")
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE revoked_at = 0`); n != 0 {
		t.Errorf("%d sessions outlive the disabling or were started after it", n)
	}
}

func TestAPasswordlessPersonCannotSignInWithAnyPassword(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	var hash string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT password_hash FROM users WHERE id = ?`, cy.ID).Scan(&hash); err != nil || hash != "" {
		t.Fatalf("stored password hash %q (%v), want none", hash, err)
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
	// Every one refused, as an unknown address is: one error, and exactly one
	// derivation at the full cost, so the answer does not tell a guesser
	// that this address signs in another way.
	for _, password := range []string{"", " ", authtest.Password, "no password hashes to this value", "mailie-dummy-sal"} {
		calls = nil
		_, _, _, err := users.SignIn(t.Context(), "cy@example.com", password, "test")
		if !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("password %q signed in: %v", password, err)
		}
		if len(calls) != 1 || calls[0].memory != memory || calls[0].passes != passes {
			t.Errorf("password %q: derivations %+v, want exactly one at m=%d t=%d", password, calls, memory, passes)
		}
	}
	// Nor is there a current password to prove for a change.
	calls = nil
	if _, _, err := users.ChangePassword(t.Context(), cy.ID, "", "a brand new password", "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("a password was set by proving none: %v", err)
	}
	if len(calls) != 1 || calls[0].memory != memory {
		t.Errorf("refusing the change cost %+v, want one derivation at full cost", calls)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE user_id = ? AND revoked_at = 0`, cy.ID); n != 1 {
		t.Errorf("%d live sessions, want the one the provider's sign-in started", n)
	}
}

func TestTheOperatorCanGiveAPasswordlessPersonAPassword(t *testing.T) {
	cheapKDF(t)
	users, _, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	if _, err := users.SetPassword(t.Context(), cy.ID, "a brand new password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	_, _, user, err := users.SignIn(t.Context(), "cy@example.com", "a brand new password", "test")
	if err != nil || !user.HasPassword || user.PasswordChangedAt.IsZero() {
		t.Fatalf("signing in with the password set: %+v, %v", user, err)
	}
	// And the provider still signs her in.
	if _, _, again := signInExternal(t, users, external("subject-of-cy", "cy@example.com")); again.ID != cy.ID || !again.HasPassword {
		t.Errorf("the provider signed in %+v", again)
	}
}

func TestAPinIsNeverReplaced(t *testing.T) {
	users, db, _ := newUsers(t)
	first := []byte("the first key seen")
	pinned, inserted, err := users.PinKey(t.Context(), issuer, "subject-of-cy", "key-1", first)
	if err != nil || !inserted || !bytes.Equal(pinned, first) {
		t.Fatalf("first pin: %q, %v, %v", pinned, inserted, err)
	}
	// The caller's copies, both ways: what it changes afterwards changes no pin.
	first[0], pinned[0] = 'X', 'Y'

	pinned, inserted, err = users.PinKey(t.Context(), issuer, "subject-of-cy", "key-1", []byte("another key"))
	if err != nil || inserted || string(pinned) != "the first key seen" {
		t.Fatalf("a second key under the same id: %q, inserted %v, %v; want the first one back", pinned, inserted, err)
	}
	// Another key id, or the same id for another identity, is another pin.
	for _, pin := range []struct{ subject, keyID string }{{"subject-of-cy", "key-2"}, {"subject-of-dee", "key-1"}} {
		if _, inserted, err := users.PinKey(t.Context(), issuer, pin.subject, pin.keyID, []byte("k")); err != nil || !inserted {
			t.Errorf("%s/%s: inserted %v, %v", pin.subject, pin.keyID, inserted, err)
		}
	}

	// Nothing that writes the table replaces one either: an update is
	// refused, and an insert under the same key id changes nothing, even
	// one that asks to replace (which would not fire a DELETE trigger).
	for _, stmt := range []string{
		`UPDATE identity_key_pins SET key = x'00' WHERE key_id = 'key-1'`,
		`UPDATE identity_key_pins SET pinned_at = 0`,
		`UPDATE OR REPLACE identity_key_pins SET key_id = 'key-1' WHERE key_id = 'key-2'`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "never replaced") {
			t.Errorf("the schema let this through: %s (%v)", stmt, err)
		}
	}
	for _, stmt := range []string{
		`INSERT OR REPLACE INTO identity_key_pins(issuer, subject, key_id, key, pinned_at)
		 VALUES ('` + issuer + `', 'subject-of-cy', 'key-1', x'00', 0)`,
		`REPLACE INTO identity_key_pins(issuer, subject, key_id, key, pinned_at)
		 VALUES ('` + issuer + `', 'subject-of-cy', 'key-1', x'00', 0)`,
		`INSERT INTO identity_key_pins(issuer, subject, key_id, key, pinned_at)
		 VALUES ('` + issuer + `', 'subject-of-cy', 'key-1', x'00', 0)
		 ON CONFLICT DO UPDATE SET key = excluded.key`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), stmt); err != nil {
			t.Errorf("%s: %v", stmt, err)
		}
		if got, _, _ := users.PinKey(t.Context(), issuer, "subject-of-cy", "key-1", []byte("x")); string(got) != "the first key seen" {
			t.Fatalf("after %s the pin is %q", stmt, got)
		}
	}

	// Bounds, before anything is written.
	for name, attempt := range map[string]func() error{
		"no key id": func() error { _, _, err := users.PinKey(t.Context(), issuer, "s", "", []byte("k")); return err },
		"a long key id": func() error {
			_, _, err := users.PinKey(t.Context(), issuer, "s", strings.Repeat("k", 256), []byte("k"))
			return err
		},
		"no key":          func() error { _, _, err := users.PinKey(t.Context(), issuer, "s", "k", nil); return err },
		"a key too large": func() error { _, _, err := users.PinKey(t.Context(), issuer, "s", "k", make([]byte, 4097)); return err },
	} {
		if err := attempt(); !errors.Is(err, auth.ErrInvalidPin) {
			t.Errorf("%s: %v, want ErrInvalidPin", name, err)
		}
	}
	if _, _, err := users.PinKey(t.Context(), "https://Accounts.example.com", "s", "k", []byte("k")); !errors.Is(err, auth.ErrInvalidIssuer) {
		t.Errorf("an issuer spelt another way: %v", err)
	}
}

func TestAProviderNameNeverKeepsAPersonOut(t *testing.T) {
	users, db, _ := newUsers(t)
	long := strings.Repeat("n", auth.MaxNameLength+1)

	// A new person is created with what the provider holds made into a name
	// this server takes: control characters and broken bytes become one
	// space, and a name too long is cut.
	for _, c := range []struct{ subject, email, name, want string }{
		{"subject-of-cy", "cy@example.com", "Cy\tLima\r\n", "Cy Lima"},
		{"subject-of-dee", "dee@example.com", "  Dee \xff Moraes  ", "Dee Moraes"},
		{"subject-of-eve", "eve@example.com", long, long[:auth.MaxNameLength]},
		{"subject-of-flo", "flo@example.com", strings.Repeat("é", auth.MaxNameLength) + "x", strings.Repeat("é", auth.MaxNameLength)},
	} {
		in := external(c.subject, c.email)
		in.Name = c.name
		_, _, user := signInExternal(t, users, in)
		if user.Name != c.want {
			t.Errorf("%s: created as %q, want %q", c.email, user.Name, c.want)
		}
		if _, err := auth.NormalizeName(user.Name); err != nil {
			t.Errorf("%s: %q is a name this server refuses: %v", c.email, user.Name, err)
		}
	}

	// Somebody who already exists is signed in whatever the provider now
	// calls them, and keeps their name: through a linked identity, and
	// through a verified address the identity is linked by.
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	for _, c := range []struct{ subject, email, keeps string }{
		{"subject-of-cy", "cy@example.com", "Cy Lima"},
		{"subject-of-ana", "ana@example.com", ana.Name},
	} {
		for _, name := range []string{long, "Cy\tLima", "\xff"} {
			in := external(c.subject, c.email)
			in.Name = name
			_, _, user, err := users.SignInExternal(t.Context(), in)
			if err != nil || user.Name != c.keeps {
				t.Errorf("%s, provider name %q: %+v, %v; want them signed in as %q", c.email, name, user, err, c.keeps)
			}
		}
	}
}

func TestALookalikeAddressNeverLinksToAnotherPerson(t *testing.T) {
	users, db, _ := newUsers(t)
	karen := authtest.NewUser(t, db, "karen@example.com", auth.RoleOwner)
	authtest.NewUser(t, db, "ida@example.com", auth.RoleMember)

	// The provider verified these mailboxes as it wrote them: the Kelvin
	// sign is not the letter K, nor the dotted capital I the letter i, nor
	// the Ohm sign the omega. Lower case would make each the address of
	// somebody here, or of somebody to come; neither is linked nor created.
	for _, email := range []string{"\u212Aaren@example.com", "\u0130da@example.com", "\u2126mega@example.com"} {
		in := external("subject-of-"+email, email)
		if _, _, user, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrInvalidEmail) {
			t.Errorf("%s signed in %+v (%v), want ErrInvalidEmail", email, user, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM user_identities`) + count(t, db, `SELECT count(*) FROM users`); n != 2 {
		t.Fatalf("lookalike addresses linked or created %d rows", n-2)
	}

	// A letter whose lower case is its own pair is the same address, as it
	// always was: Karen's, and a new person's with a letter beyond ASCII.
	if _, _, user := signInExternal(t, users, external("subject-of-karen", "KAREN@example.com")); user.ID != karen.ID {
		t.Errorf("her own address signed in %s, want %s", user.ID, karen.ID)
	}
	if _, _, user := signInExternal(t, users, external("subject-of-ase", "\u00C5se@example.com")); user.Email != "\u00E5se@example.com" {
		t.Errorf("an address with \u00C5 was created as %q", user.Email)
	}

	// Once linked, an identity signs its person in whatever the provider
	// now says of the address, a lookalike included.
	if _, _, user := signInExternal(t, users, external("subject-of-karen", "\u212Aaren@example.com")); user.ID != karen.ID {
		t.Errorf("her linked identity signed in %s", user.ID)
	}
}

func TestAPinGoesOnlyWithItsPerson(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	_, _, dee := signInExternal(t, users, external("subject-of-dee", "dee@example.com"))
	for _, pin := range []struct{ subject, keyID string }{
		{"subject-of-cy", "k1"}, {"subject-of-cy", "k2"}, {"subject-of-dee", "k1"}, {"subject-of-nobody", "k1"},
	} {
		if _, _, err := users.PinKey(t.Context(), issuer, pin.subject, pin.keyID, []byte(pin.subject+pin.keyID)); err != nil {
			t.Fatal(err)
		}
	}
	pins := func(subject string) int {
		return count(t, db, `SELECT count(*) FROM identity_key_pins WHERE subject = ?`, subject)
	}

	// Not on its own while its identity signs somebody in.
	if _, err := db.Writer().ExecContext(t.Context(),
		`DELETE FROM identity_key_pins WHERE subject = 'subject-of-cy'`); err == nil || !strings.Contains(err.Error(), "goes only with") {
		t.Fatalf("a pin was deleted on its own: %v", err)
	}
	// Nor when its person is only switched off: they may be switched on again.
	if _, err := users.Disable(t.Context(), cy.ID, false); err != nil {
		t.Fatal(err)
	}
	if pins("subject-of-cy") != 2 {
		t.Fatal("disabling her took her pins")
	}

	// Deleted with her, in the transaction that deletes her.
	var removed auth.Removed
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		removed, err = users.DeleteTx(t.Context(), tx, cy.ID, false)
		return err
	}); err != nil {
		t.Fatalf("DeleteTx: %v", err)
	}
	if removed.Identities != 1 || pins("subject-of-cy") != 0 ||
		count(t, db, `SELECT count(*) FROM user_identities WHERE subject = 'subject-of-cy'`) != 0 {
		t.Fatalf("after deleting her: %+v, %d pins left", removed, pins("subject-of-cy"))
	}
	// Nobody else's went with her.
	if pins("subject-of-dee") != 1 || pins("subject-of-nobody") != 1 ||
		count(t, db, `SELECT count(*) FROM user_identities WHERE user_id = ?`, dee.ID) != 1 {
		t.Error("deleting her took somebody else's identity or pins")
	}
	// A pin whose identity links nobody names nobody, and may go.
	if _, err := db.Writer().ExecContext(t.Context(),
		`DELETE FROM identity_key_pins WHERE subject = 'subject-of-nobody'`); err != nil {
		t.Errorf("a pin no identity links could not be deleted: %v", err)
	}
}

func TestAPinForASignInThatWasRefusedIsSweptSoon(t *testing.T) {
	users, db, clock := newUsers(t)
	start := *clock
	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	signInExternal(t, users, external("subject-of-ana", "ana@example.com"))
	pin := func(subject string) {
		t.Helper()
		if _, _, err := users.PinKey(t.Context(), issuer, subject, "k1", []byte("key of "+subject)); err != nil {
			t.Fatal(err)
		}
	}
	pins := func(subject string) int {
		return count(t, db, `SELECT count(*) FROM identity_key_pins WHERE subject = ?`, subject)
	}

	// An extension pins before it signs in, and the sign-in is refused: an
	// address nobody verified, another subject for an address that has one.
	pin("subject-of-ana")
	pin("subject-of-unverified")
	unverified := external("subject-of-unverified", "someone@example.com")
	unverified.EmailVerified = false
	if _, _, _, err := users.SignInExternal(t.Context(), unverified); !errors.Is(err, auth.ErrEmailNotVerified) {
		t.Fatal(err)
	}
	pin("subject-of-another-ana")
	if _, _, _, err := users.SignInExternal(t.Context(), external("subject-of-another-ana", "ana@example.com")); !errors.Is(err, auth.ErrIdentityConflict) {
		t.Fatal(err)
	}

	// While a sign-in may still be under way, nothing goes.
	*clock = start.Add(auth.UnlinkedPinGrace - time.Second)
	if n, err := users.SweepUnlinkedPins(t.Context()); err != nil || n != 0 {
		t.Fatalf("a sweep within the grace deleted %d (%v)", n, err)
	}
	// After it, the pins of the refused sign-ins go: they are the provider's
	// id and key for people who have no account here. Ana's stays, however
	// old, while her identity signs her in.
	*clock = start.Add(auth.UnlinkedPinGrace + time.Second)
	if n, err := users.SweepUnlinkedPins(t.Context()); err != nil || n != 2 {
		t.Fatalf("the sweep deleted %d (%v), want the 2 pins of refused sign-ins", n, err)
	}
	if pins("subject-of-unverified")+pins("subject-of-another-ana") != 0 || pins("subject-of-ana") != 1 {
		t.Errorf("after the sweep: %d, %d and %d pins", pins("subject-of-unverified"), pins("subject-of-another-ana"), pins("subject-of-ana"))
	}
	*clock = start.Add(365 * 24 * time.Hour)
	if n, err := users.SweepUnlinkedPins(t.Context()); err != nil || n != 0 || pins("subject-of-ana") != 1 {
		t.Errorf("a year on the sweep deleted %d (%v), and %d of ana's pins are left", n, err, pins("subject-of-ana"))
	}
}

func TestAnIssuerIsExactlyAnOrigin(t *testing.T) {
	for _, ok := range []string{
		"https://accounts.example.com", "https://accounts.example.com:8443", "https://203.0.113.7",
		"http://localhost:8290", "http://accounts.example.localhost:8290", "http://127.0.0.1:9000", "http://[::1]:9000",
	} {
		if err := auth.CheckIssuer(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "accounts.example.com", "https:accounts.example.com", "https://", "wss://accounts.example.com",
		"https://Accounts.example.com", "https://accounts.example.com/", "https://accounts.example.com/realm",
		"https://accounts.example.com:443", "http://localhost:80", "https://accounts.example.com:", "https://accounts.example.com:0443",
		"https://accounts.example.com:99999", "https://user@accounts.example.com", "https://accounts.example.com?x=1",
		"https://accounts.example.com#x", "https://accounts.example.com?", "https://accounts_example.com",
		"https://accounts.example.com.", "http://accounts.example.com", "http://localhost.example.com", "http://10.0.0.1",
		"https://[fe80::1%25en0]", "https://[::FFFF:7f00:1]", "https://" + strings.Repeat("a.", 150) + "com",
	} {
		if err := auth.CheckIssuer(bad); !errors.Is(err, auth.ErrInvalidIssuer) {
			t.Errorf("%q: %v, want ErrInvalidIssuer", bad, err)
		}
	}
}

func TestAnExternalSignInRefusesWhatCannotNameAPerson(t *testing.T) {
	users, db, _ := newUsers(t)
	for name, change := range map[string]struct {
		edit func(*auth.ExternalSignIn)
		want error
	}{
		"no subject":            {func(in *auth.ExternalSignIn) { in.Subject = "" }, auth.ErrInvalidSubject},
		"a subject too long":    {func(in *auth.ExternalSignIn) { in.Subject = strings.Repeat("s", 256) }, auth.ErrInvalidSubject},
		"a subject with a line": {func(in *auth.ExternalSignIn) { in.Subject = "a\nb" }, auth.ErrInvalidSubject},
		"a subject not UTF-8":   {func(in *auth.ExternalSignIn) { in.Subject = "\xff" }, auth.ErrInvalidSubject},
		"no issuer":             {func(in *auth.ExternalSignIn) { in.Issuer = "" }, auth.ErrInvalidIssuer},
		"an address with a name": {func(in *auth.ExternalSignIn) { in.Email = "Cy <cy@example.com>" },
			auth.ErrInvalidEmail},
		"no address": {func(in *auth.ExternalSignIn) { in.Email = "" }, auth.ErrInvalidEmail},
	} {
		in := external("subject-of-cy", "cy@example.com")
		change.edit(&in)
		if _, _, _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, change.want) {
			t.Errorf("%s: %v, want %v", name, err, change.want)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 0 {
		t.Errorf("refusals created %d people", n)
	}
	// The longest subject there is still names someone.
	if _, _, _, err := users.SignInExternal(t.Context(), external(strings.Repeat("s", auth.MaxSubjectLength), "cy@example.com")); err != nil {
		t.Errorf("a subject of %d bytes: %v", auth.MaxSubjectLength, err)
	}
}
