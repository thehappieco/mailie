package auth_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

// The account key of a person who signs in through an identity provider
// (docs/key-scheme.md sections 6.3, 12.8 and 12.10).

// withKey is in asking the provider for the product key the tests' provider
// delivers, pinned for the identity first, as an extension pins it.
func withKey(t *testing.T, users *auth.Users, in auth.ExternalSignIn) auth.ExternalSignIn {
	t.Helper()
	authtest.PinProductKey(t, users, in.Issuer, in.Subject)
	in.WantsKey, in.ProductKeyID = true, authtest.ProductKeyID
	return in
}

// ticketOf signs in asking for the key, as a person without an account key,
// and returns what it answered, failing on anything but a ticket.
func ticketOf(t *testing.T, users *auth.Users, in auth.ExternalSignIn) auth.ExternalSignedIn {
	t.Helper()
	signed, err := users.SignInExternal(t.Context(), withKey(t, users, in))
	if err != nil || signed.Ticket == "" || signed.Token != "" {
		t.Fatalf("a first sign-in asking for the key answered %+v (%v), want a ticket and no session", signed, err)
	}
	return signed
}

// enrolment is what a page sends with ticket: a fresh public key and a
// platform wrap's shape under the tests' product key id.
func enrolment(t *testing.T, ticket string) auth.ExternalEnrolment {
	t.Helper()
	return auth.ExternalEnrolment{
		Ticket: ticket, PublicKey: authtest.PublicKey(t), PlatformWrap: authtest.PlatformWrap(t),
		ProductKeyID: authtest.ProductKeyID, UserAgent: "test",
	}
}

func TestAPersonWithoutAnAccountKeyIsGivenNoSessionThroughAProvider(t *testing.T) {
	users, db, clock := newUsers(t)
	before := rowsOf(t, db)

	// Not asking for the key: refused, and the person it would have created
	// is not, nor is the identity linked.
	if _, err := users.SignInExternal(t.Context(), external("subject-of-cy", "cy@example.com")); !errors.Is(err, auth.ErrAccountKeyNeeded) {
		t.Fatalf("a first sign-in for the identity alone: %v, want ErrAccountKeyNeeded", err)
	}
	if after := rowsOf(t, db); !slices.Equal(after, before) {
		t.Fatalf("a refused sign-in wrote:\nbefore %v\nafter  %v", before, after)
	}

	// Asking for it: the person is created and linked, and answered a
	// ticket and their seal id, never a session.
	in := external("subject-of-cy", "cy@example.com")
	in.AuthTime = clock.Add(-time.Minute)
	signed := ticketOf(t, users, in)
	if signed.User.SealID == "" || signed.User.PublicKey != nil || signed.Session != (auth.Session{}) ||
		!signed.TicketExpiresAt.Equal(clock.Truncate(time.Second).Add(auth.EnrolmentTicketTTL)) || signed.PlatformWrap != nil {
		t.Fatalf("answered %+v", signed)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions for a person without an account key", n)
	}
	// Stored as its hash, bound to the person, the identity and the pinned
	// product key id, with what the session it opens gets.
	raw, err := base64.RawURLEncoding.DecodeString(signed.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if n := count(t, db, `SELECT count(*) FROM external_enrolments WHERE hash = ? AND user_id = ? AND issuer = ? AND subject = ?
		AND product_key_id = ? AND auth_time = ? AND session_ttl = ?`, sum[:], signed.User.ID, issuer, "subject-of-cy",
		authtest.ProductKeyID, clock.Add(-time.Minute).Unix(), int64((24 * time.Hour).Seconds())); n != 1 {
		t.Fatalf("the ticket is stored %d times as its hash and binding", n)
	}
	if n := count(t, db, `SELECT count(*) FROM external_enrolments WHERE hash = ?`, raw); n != 0 {
		t.Fatal("the ticket is stored as itself")
	}

	// The person exists now, keyless, and is still given no session without
	// the key: their next sign-in for the identity alone is refused too.
	if _, err := users.SignInExternal(t.Context(), external("subject-of-cy", "cy@example.com")); !errors.Is(err, auth.ErrAccountKeyNeeded) {
		t.Fatalf("a keyless person's sign-in for the identity alone: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`) + count(t, db, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("%d sessions and people, want only Cy", n)
	}
}

func TestAnEnrolmentWritesTheAccountKeyOnceAndOpensTheSessionTheSignInAsked(t *testing.T) {
	users, db, clock := newUsers(t)
	in := external("subject-of-cy", "cy@example.com")
	in.AuthTime = clock.Add(-2 * time.Minute)
	signed := ticketOf(t, users, in)
	*clock = clock.Add(time.Minute)

	req := enrolment(t, signed.Ticket)
	token, session, cy, err := users.EnrolExternal(t.Context(), req)
	if err != nil {
		t.Fatalf("EnrolExternal: %v", err)
	}
	if !bytes.Equal(cy.PublicKey, req.PublicKey) || cy.SealID != signed.User.SealID || cy.HasPassword || cy.Enrolled {
		t.Errorf("enrolled %+v", cy)
	}
	// The session the sign-in would have opened: its length, and the
	// provider's authentication time as its step-up time.
	if !session.AuthenticatedAt.Equal(in.AuthTime.Truncate(time.Second)) ||
		session.ExpiresAt.Sub(session.CreatedAt) != 24*time.Hour {
		t.Errorf("session %+v", session)
	}
	if p, err := users.AuthenticateSession(t.Context(), token); err != nil || p.UserID != cy.ID {
		t.Fatalf("the session authenticated %+v (%v)", p, err)
	}
	var wrap []byte
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT wrap FROM platform_wraps WHERE user_id = ? AND product_key_id = ?`, cy.ID, authtest.ProductKeyID).Scan(&wrap); err != nil ||
		!bytes.Equal(wrap, req.PlatformWrap) {
		t.Fatalf("the stored wrap is %x (%v)", wrap, err)
	}

	// The ticket worked once.
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, signed.Ticket)); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a ticket used twice: %v", err)
	}
	// And whatever writes the database, the wrap is insert only, and the
	// public key written once.
	for _, stmt := range []string{
		`UPDATE platform_wraps SET wrap = wrap WHERE user_id = ?`,
		`UPDATE platform_wraps SET product_key_id = 'mailie:2' WHERE user_id = ?`,
		`INSERT OR REPLACE INTO platform_wraps SELECT user_id, product_key_id, wrap, created_at + 1 FROM platform_wraps WHERE user_id = ?`,
		`INSERT INTO platform_wraps SELECT user_id, product_key_id, wrap, 0 FROM platform_wraps WHERE user_id = ?
		 ON CONFLICT DO UPDATE SET wrap = excluded.wrap`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), stmt, cy.ID); err == nil || !strings.Contains(err.Error(), "written once") {
			t.Errorf("%s: %v, want a refusal", stmt, err)
		}
	}
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET public_key = ? WHERE id = ?`, authtest.PublicKey(t), cy.ID); err == nil {
		t.Error("the account public key was replaced")
	}
}

func TestAnEnrolmentTicketWorksOnceForItsProductKeyIDAndTenMinutesOnly(t *testing.T) {
	users, db, clock := newUsers(t)
	first := ticketOf(t, users, external("subject-of-cy", "cy@example.com"))
	second := ticketOf(t, users, external("subject-of-cy", "cy@example.com"))

	// Not a ticket, another product key id than the one it was issued for:
	// refused, and the ticket is still its own.
	other := enrolment(t, first.Ticket)
	other.ProductKeyID = "mailie:2"
	for name, req := range map[string]auth.ExternalEnrolment{
		"no ticket":              enrolment(t, ""),
		"not a ticket":           enrolment(t, "not-a-ticket"),
		"a ticket of no one":     enrolment(t, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))),
		"another product key id": other,
	} {
		if _, _, _, err := users.EnrolExternal(t.Context(), req); !errors.Is(err, auth.ErrTicketInvalid) {
			t.Errorf("%s: %v, want ErrTicketInvalid", name, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM external_enrolments`); n != 2 {
		t.Fatalf("%d tickets left, want both", n)
	}
	// One enrolment makes every other ticket of the person moot: two tabs
	// signed in, the first enrols, the second's ticket is gone.
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, first.Ticket)); err != nil {
		t.Fatalf("the first ticket: %v", err)
	}
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, second.Ticket)); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a second tab's ticket: %v, want ErrTicketInvalid", err)
	}

	// Ten minutes, and no more.
	dee := ticketOf(t, users, external("subject-of-dee", "dee@example.com"))
	*clock = clock.Add(auth.EnrolmentTicketTTL)
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, dee.Ticket)); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a ticket ten minutes old: %v, want ErrTicketInvalid", err)
	}
	if n, err := users.SweepTickets(t.Context()); err != nil || n != 1 || count(t, db, `SELECT count(*) FROM external_enrolments`) != 0 {
		t.Errorf("the sweep deleted %d (%v)", n, err)
	}
}

func TestAnEnrolmentTicketIsOnlyItsActiveLinkedPersons(t *testing.T) {
	users, db, _ := newUsers(t)

	// Disabled: the ticket goes with the disabling, and is not one when they
	// are switched back on.
	cy := ticketOf(t, users, external("subject-of-cy", "cy@example.com"))
	if err := users.SetDisabled(t.Context(), cy.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM external_enrolments WHERE user_id = ?`, cy.User.ID); n != 0 {
		t.Errorf("disabling her left %d enrolment tickets", n)
	}
	if err := users.SetDisabled(t.Context(), cy.User.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, cy.Ticket)); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a disabled person's ticket: %v, want ErrTicketInvalid", err)
	}

	// An identity no longer linked to the person: its ticket proves nothing
	// of them.
	dee := ticketOf(t, users, external("subject-of-dee", "dee@example.com"))
	if _, err := db.Writer().ExecContext(t.Context(),
		`UPDATE user_identities SET subject = 'subject-of-another-dee' WHERE user_id = ?`, dee.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, dee.Ticket)); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Errorf("a ticket whose identity is not the person's: %v, want ErrTicketInvalid", err)
	}

	// A person whose account key was written another way since: refused,
	// and nothing stored.
	eve := ticketOf(t, users, external("subject-of-eve", "eve@example.com"))
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET public_key = ? WHERE id = ?`,
		authtest.PublicKey(t), eve.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, eve.Ticket)); !errors.Is(err, auth.ErrAccountKeyExists) {
		t.Errorf("a person with an account key: %v, want ErrAccountKeyExists", err)
	}
	if n := count(t, db, `SELECT count(*) FROM platform_wraps`) + count(t, db, `SELECT count(*) FROM sessions`); n != 0 {
		t.Errorf("refused enrolments wrote %d wraps and sessions", n)
	}
}

func TestAnEnrolmentChecksTheShapesOfWhatItStoresAndOpensNothing(t *testing.T) {
	users, db, _ := newUsers(t)
	signed := ticketOf(t, users, external("subject-of-cy", "cy@example.com"))
	accountWrap := authtest.Wrap(t)
	for name, c := range map[string]struct {
		edit func(*auth.ExternalEnrolment)
		want error
	}{
		"a public key of 31 bytes":          {func(e *auth.ExternalEnrolment) { e.PublicKey = e.PublicKey[:31] }, auth.ErrInvalidPublicKey},
		"a public key of low order":         {func(e *auth.ExternalEnrolment) { e.PublicKey = make([]byte, 32) }, auth.ErrInvalidPublicKey},
		"an account wrap":                   {func(e *auth.ExternalEnrolment) { e.PlatformWrap = accountWrap }, auth.ErrInvalidPlatformWrap},
		"a wrap of 60 bytes":                {func(e *auth.ExternalEnrolment) { e.PlatformWrap = e.PlatformWrap[:60] }, auth.ErrInvalidPlatformWrap},
		"no wrap":                           {func(e *auth.ExternalEnrolment) { e.PlatformWrap = nil }, auth.ErrInvalidPlatformWrap},
		"a product key id of two spellings": {func(e *auth.ExternalEnrolment) { e.ProductKeyID = "mailie:01" }, auth.ErrInvalidProductKeyID},
		"another product's key id":          {func(e *auth.ExternalEnrolment) { e.ProductKeyID = "wappie:1" }, auth.ErrInvalidProductKeyID},
	} {
		req := enrolment(t, signed.Ticket)
		c.edit(&req)
		if _, _, _, err := users.EnrolExternal(t.Context(), req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Refused before the ticket was touched: it still enrols, with a wrap
	// whose bytes are anything of the right shape.
	if _, _, _, err := users.EnrolExternal(t.Context(), enrolment(t, signed.Ticket)); err != nil {
		t.Fatalf("the ticket after the refusals: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM platform_wraps`); n != 1 {
		t.Fatalf("%d wraps stored", n)
	}
}

func TestASignInAskingForTheKeyAnswersThePlatformWrapAtThePinnedIDOnly(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	var stored []byte
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT wrap FROM platform_wraps WHERE user_id = ?`, cy.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	// Asking for the key: the session, and the wrap at the pinned id.
	signed, err := users.SignInExternal(t.Context(), withKey(t, users, external("subject-of-cy", "cy@example.com")))
	if err != nil || signed.Token == "" || !bytes.Equal(signed.PlatformWrap, stored) || signed.Ticket != "" {
		t.Fatalf("a sign-in asking for the key answered %+v (%v)", signed, err)
	}
	// For the identity alone: the session, and no wrap.
	signed, err = users.SignInExternal(t.Context(), external("subject-of-cy", "cy@example.com"))
	if err != nil || signed.Token == "" || signed.PlatformWrap != nil {
		t.Fatalf("a sign-in for the identity alone answered %+v (%v)", signed, err)
	}

	sessions := count(t, db, `SELECT count(*) FROM sessions`)
	// A product key id pinned with no wrap stored under it (a new epoch, or
	// an account key a reset made): no_wrap, and no session.
	if _, _, err := users.PinKey(t.Context(), issuer, "subject-of-cy", "mailie:2", authtest.ProductKey("epoch 2")); err != nil {
		t.Fatal(err)
	}
	in := external("subject-of-cy", "cy@example.com")
	in.WantsKey, in.ProductKeyID = true, "mailie:2"
	if _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrNoPlatformWrap) {
		t.Errorf("an id with no wrap: %v, want ErrNoPlatformWrap", err)
	}
	// An id nothing is pinned under, and one that is not an id.
	in.ProductKeyID = "mailie:3"
	if _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrProductKeyNotPinned) {
		t.Errorf("an id nothing is pinned under: %v, want ErrProductKeyNotPinned", err)
	}
	in.ProductKeyID = "mailie:01"
	if _, err := users.SignInExternal(t.Context(), in); !errors.Is(err, auth.ErrInvalidProductKeyID) {
		t.Errorf("an id in another spelling: %v, want ErrInvalidProductKeyID", err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`); n != sessions {
		t.Errorf("refused sign-ins started %d sessions", n-sessions)
	}
}

func TestAResetDeletesEveryPlatformWrapOfThePersonOnly(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	_, _, dee := signInExternal(t, users, external("subject-of-dee", "dee@example.com"))
	wraps := func(userID string) int {
		return count(t, db, `SELECT count(*) FROM platform_wraps WHERE user_id = ?`, userID)
	}
	if wraps(cy.ID) != 1 || wraps(dee.ID) != 1 {
		t.Fatalf("%d and %d wraps before the reset", wraps(cy.ID), wraps(dee.ID))
	}
	code, _, err := users.CreateReset(t.Context(), cy.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "cy@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatalf("CompleteReset: %v", err)
	}
	if wraps(cy.ID) != 0 || wraps(dee.ID) != 1 {
		t.Errorf("after Cy's reset: %d and %d wraps", wraps(cy.ID), wraps(dee.ID))
	}
	// Her next sign-in asking for the key finds none for her new key, and
	// signing in for the identity alone still works.
	if _, err := users.SignInExternal(t.Context(), withKey(t, users, external("subject-of-cy", "cy@example.com"))); !errors.Is(err, auth.ErrNoPlatformWrap) {
		t.Errorf("a sign-in asking for the key after a reset: %v, want ErrNoPlatformWrap", err)
	}
	if signed, err := users.SignInExternal(t.Context(), external("subject-of-cy", "cy@example.com")); err != nil || signed.Token == "" {
		t.Errorf("a sign-in for the identity alone after a reset: %+v (%v)", signed, err)
	}
}

func TestDeletingAPersonDeletesTheirPlatformWrapsAndEnrolmentTickets(t *testing.T) {
	users, db, _ := newUsers(t)
	_, _, cy := signInExternal(t, users, external("subject-of-cy", "cy@example.com"))
	dee := ticketOf(t, users, external("subject-of-dee", "dee@example.com"))
	_, _, eve := signInExternal(t, users, external("subject-of-eve", "eve@example.com"))
	for _, id := range []string{cy.ID, dee.User.ID} {
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := users.DeleteTx(t.Context(), tx, id, false)
			return err
		}); err != nil {
			t.Fatalf("DeleteTx: %v", err)
		}
	}
	for _, table := range []string{"platform_wraps", "external_enrolments"} {
		if n := count(t, db, `SELECT count(*) FROM `+table+` WHERE user_id IN (?, ?)`, cy.ID, dee.User.ID); n != 0 {
			t.Errorf("%s keeps %d rows of the people deleted", table, n)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM platform_wraps WHERE user_id = ?`, eve.ID); n != 1 {
		t.Errorf("Eve's wrap went with them (%d left)", n)
	}
}

// keylessPerson is a person who signed in through a provider before anything
// wrote their account key: created by a first sign-in that asked for the
// key, whose ticket was never used.
func keylessPerson(t *testing.T, users *auth.Users, db *store.Store, subject, email string) auth.User {
	t.Helper()
	signed := ticketOf(t, users, external(subject, email))
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM external_enrolments WHERE user_id = ?`, signed.User.ID); err != nil {
		t.Fatal(err)
	}
	return signed.User
}

func TestAKeylessPersonsNextSignInAskingForTheKeyIsAnsweredATicketForThem(t *testing.T) {
	users, db, _ := newUsers(t)
	cy := keylessPerson(t, users, db, "subject-of-cy", "cy@example.com")
	signed := ticketOf(t, users, external("subject-of-cy", "cy@example.com"))
	if signed.User.ID != cy.ID || signed.User.SealID != cy.SealID {
		t.Fatalf("the ticket is for %+v, want Cy", signed.User)
	}
	if _, _, user, err := users.EnrolExternal(t.Context(), enrolment(t, signed.Ticket)); err != nil || user.ID != cy.ID {
		t.Fatalf("enrolling Cy: %+v, %v", user, err)
	}
}
