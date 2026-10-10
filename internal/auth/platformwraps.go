package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/keyscheme"
)

// The account key of a person who signs in through an identity provider
// (docs/key-scheme.md sections 6, 12.8 and 12.10). Such a provider may
// deliver a product key to the person's page, sealed to that page alone,
// which the server never sees; the extension pins its public half for the
// identity (PinKey). The page keeps the person's account key wrapped under a
// key derived from it, the platform wrap, which this server stores
// (platform_wraps, migration 0015) and cannot open: it checks only the
// wrap's shape (keyscheme.CheckPlatformWrapShape).
//
//   - A first sign-in that asked for the product key, of a person who has
//     no account key, starts no session: it answers a single-use enrolment
//     ticket (external_enrolments), ten minutes, stored as SHA-256, bound to
//     the person, the identity and the pinned product key id. The page makes
//     the account key, wraps it, and presents the ticket with the public key
//     and the wrap (EnrolExternal), which writes both once and opens the
//     session. A session never exists for a person without an account key,
//     so a session copied from a browser never chooses one.
//   - A later sign-in that asked for the product key answers the person's
//     wrap at the pinned product key id with the session; none stored there
//     is ErrNoPlatformWrap, and no session.
//   - The reset invitation, which replaces the account key, deletes every
//     platform wrap of the old one in its transaction (CompleteReset); the
//     person's deletion takes them too.
//
// A step-up through the provider compares the product key the provider
// names now with the pin, read only, and never pins (ExternalStepUp).

var (
	// ErrAccountKeyNeeded is a sign-in through an identity provider of a
	// person who has no account key yet, which did not ask for the product
	// key: the page signs in again asking for it (WantsKey), and nothing
	// was created or linked.
	ErrAccountKeyNeeded = errors.New("auth: this person has no account key yet; sign in again asking for the product key")
	// ErrNoPlatformWrap is a sign-in that asked for the product key, of a
	// person who has an account key and no platform wrap at its product key
	// id (docs/key-scheme.md section 17.2): their account key was made
	// another way (a reset invitation), or under another product key.
	ErrNoPlatformWrap = errors.New("auth: no platform wrap is stored for this person at that product key id")
	// ErrInvalidProductKeyID is a product key id that is not Mailie's in
	// its one spelling (keyscheme.ValidProductKeyID).
	ErrInvalidProductKeyID = errors.New("auth: a product key id is mailie:<epoch>, the epoch 1 to 2147483647 without a leading zero")
	// ErrProductKeyNotPinned is a product key id no key is pinned under for
	// the identity: the extension pins the key it was delivered before it
	// signs the person in (PinKey).
	ErrProductKeyNotPinned = errors.New("auth: no product key is pinned for this identity under that id")
	// ErrProductKeyChanged is a step-up whose provider names, for the
	// identity, another product key than the one pinned under its id, or
	// one under an id nothing is pinned under: only the provider, or
	// whoever can write its database, can do that.
	ErrProductKeyChanged = errors.New("auth: the identity's product key is not the one pinned for it")
	// ErrInvalidPlatformWrap is a platform wrap of the wrong shape.
	ErrInvalidPlatformWrap = errors.New("auth: a platform wrap is 61 bytes starting with 0x03")
	// ErrAccountKeyExists is an enrolment of a person who has an account
	// key already: the public key is written once (another tab enrolled
	// first, say), and signing in again answers its wrap.
	ErrAccountKeyExists = errors.New("auth: this person has an account key already")
)

// EnrolmentTicketTTL is how long a first sign-in's enrolment ticket lasts.
const EnrolmentTicketTTL = TicketTTL

// requirePinTx refuses, with ErrProductKeyNotPinned, a product key id no key
// is pinned under for the identity.
func requirePinTx(ctx context.Context, tx *sql.Tx, issuer, subject, keyID string) error {
	if _, err := pinnedKeyTx(ctx, tx, issuer, subject, keyID); err != nil {
		return err
	}
	return nil
}

// pinnedKeyTx reads the key pinned for the identity under keyID, read only:
// it pins nothing. None is ErrProductKeyNotPinned.
func pinnedKeyTx(ctx context.Context, tx *sql.Tx, issuer, subject, keyID string) ([]byte, error) {
	var key []byte
	err := tx.QueryRowContext(ctx,
		`SELECT key FROM identity_key_pins WHERE issuer = ? AND subject = ? AND key_id = ?`,
		issuer, subject, keyID).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrProductKeyNotPinned
	case err != nil:
		return nil, fmt.Errorf("auth: read the pinned key: %w", err)
	}
	return key, nil
}

// platformWrapTx reads a person's platform wrap at a product key id; none is
// ErrNoPlatformWrap.
func platformWrapTx(ctx context.Context, tx *sql.Tx, userID, productKeyID string) ([]byte, error) {
	var wrap []byte
	err := tx.QueryRowContext(ctx,
		`SELECT wrap FROM platform_wraps WHERE user_id = ? AND product_key_id = ?`, userID, productKeyID).Scan(&wrap)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoPlatformWrap
	case err != nil:
		return nil, fmt.Errorf("auth: read the platform wrap: %w", err)
	}
	return wrap, nil
}

// issueEnrolmentTx makes a first sign-in's enrolment ticket inside
// SignInExternal's transaction and returns the only copy of it, and when it
// expires. It keeps what the session it opens gets: the sign-in's
// authentication time (authenticated, at most now) and length.
func issueEnrolmentTx(ctx context.Context, tx *sql.Tx, userID string, in ExternalSignIn, authenticated, now time.Time,
) (string, time.Time, error) {
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: read random: %w", err)
	}
	sum := sha256.Sum256(raw)
	var authTime int64
	if !authenticated.IsZero() {
		authTime = authenticated.Unix()
	}
	expires := now.Add(EnrolmentTicketTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_enrolments(hash, user_id, issuer, subject, product_key_id,
		auth_time, session_ttl, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sum[:], userID, in.Issuer, in.Subject, in.ProductKeyID, authTime, int64(in.TTL/time.Second),
		now.Unix(), expires.Unix()); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: issue the enrolment ticket: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), expires, nil
}

// ExternalEnrolment is what a person's page sends with a first sign-in's
// ticket (docs/key-scheme.md section 12.10).
type ExternalEnrolment struct {
	// Ticket is the enrolment ticket the sign-in answered.
	Ticket string
	// PublicKey is the account public key the page made, 32 bytes.
	PublicKey []byte
	// PlatformWrap is the account key wrapped under the product key, bound
	// to the person's seal id, the identity's subject, ProductKeyID and
	// PublicKey: 61 bytes starting with 0x03, which nothing here opens.
	PlatformWrap []byte
	// ProductKeyID is the product key id the page wrapped under: the one
	// the ticket was issued for.
	ProductKeyID string
	// UserAgent is the browser's, for the session list.
	UserAgent string
}

// check holds an enrolment to its shapes, before anything is read.
func (e ExternalEnrolment) check() error {
	if err := keyscheme.CheckPublicKey(e.PublicKey); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPublicKey, err)
	}
	if err := keyscheme.CheckPlatformWrapShape(e.PlatformWrap); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPlatformWrap, err)
	}
	if !keyscheme.ValidProductKeyID(e.ProductKeyID) {
		return ErrInvalidProductKeyID
	}
	return nil
}

// EnrolExternal writes the account key of a person who signs in through an
// identity provider and has none yet, with the enrolment ticket their
// sign-in answered (docs/key-scheme.md section 12.10), and starts their
// session. In one transaction:
//
//   - the ticket is used: not expired, issued for in.ProductKeyID, its
//     person active and its identity still linked to them; anything else is
//     ErrTicketInvalid, and leaves it unused;
//   - the person's account public key is written, once: a person who has one
//     already is ErrAccountKeyExists;
//   - their platform wrap is stored under the ticket's product key id, whose
//     key must still be pinned for the identity;
//   - their other enrolment tickets go, and the session starts, as long as
//     the sign-in asked and with its authentication time as the step-up time.
//
// The public key and the wrap are checked for their shapes only: nothing
// here opens a wrap.
func (u *Users) EnrolExternal(ctx context.Context, in ExternalEnrolment) (string, Session, User, error) {
	hash, ok := hashTicket(in.Ticket)
	if !ok {
		return "", Session{}, User{}, ErrTicketInvalid
	}
	if err := in.check(); err != nil {
		return "", Session{}, User{}, err
	}
	var (
		token   string
		session Session
		user    User
	)
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now().UTC().Truncate(time.Second)
		var (
			userID, issuer, subject, productKeyID string
			authTime, ttl                         int64
		)
		err := tx.QueryRowContext(ctx, `DELETE FROM external_enrolments WHERE hash = ? AND expires_at > ?
			RETURNING user_id, issuer, subject, product_key_id, auth_time, session_ttl`, hash, now.Unix(),
		).Scan(&userID, &issuer, &subject, &productKeyID, &authTime, &ttl)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrTicketInvalid
		case err != nil:
			return fmt.Errorf("auth: use the enrolment ticket: %w", err)
		}
		// Returning rolls the DELETE back: a ticket presented with another
		// product key id is still its own.
		if productKeyID != in.ProductKeyID {
			return ErrTicketInvalid
		}
		var status string
		var linked int
		if err := tx.QueryRowContext(ctx, `SELECT u.status, (SELECT count(*) FROM user_identities i
			WHERE i.issuer = ? AND i.subject = ? AND i.user_id = u.id) FROM users u WHERE u.id = ?`,
			issuer, subject, userID).Scan(&status, &linked); err != nil {
			return fmt.Errorf("auth: read the person enrolling: %w", err)
		}
		if status != userActive || linked == 0 {
			return ErrTicketInvalid
		}
		if err := requirePinTx(ctx, tx, issuer, subject, productKeyID); err != nil {
			return err
		}
		written, err := execCount(ctx, tx,
			`UPDATE users SET public_key = ?, updated_at = ? WHERE id = ? AND public_key IS NULL`,
			in.PublicKey, now.Unix(), userID)
		if err != nil {
			return fmt.Errorf("auth: write the account public key: %w", err)
		}
		if written == 0 {
			return ErrAccountKeyExists
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO platform_wraps(user_id, product_key_id, wrap, created_at) VALUES (?, ?, ?, ?)`,
			userID, productKeyID, in.PlatformWrap, now.Unix()); err != nil {
			return fmt.Errorf("auth: store the platform wrap: %w", err)
		}
		// Moot now: the account key is written.
		if _, err := tx.ExecContext(ctx, `DELETE FROM external_enrolments WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("auth: drop the enrolment tickets: %w", err)
		}
		var authenticated time.Time
		if authTime > 0 {
			authenticated = time.Unix(authTime, 0)
		}
		token, session, err = startSessionTx(ctx, tx, userID, in.UserAgent, now, time.Duration(ttl)*time.Second,
			authenticated)
		if err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, userID))
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// ExternalProof is what an identity provider says of a step-up
// (ExternalStepUp): who authenticated, when, and the product key it names
// for them now.
type ExternalProof struct {
	// Issuer and Subject are the identity that authenticated.
	Issuer  string
	Subject string
	// AuthTime is when it authenticated (OpenID Connect's auth_time).
	AuthTime time.Time
	// ProductKeyID and ProductKey are the product key the provider names
	// for the identity now (its userinfo), compared with the one pinned
	// under that id, read only. Empty, for a provider that delivers no
	// product key, only when nothing is pinned for the identity.
	ProductKeyID string
	ProductKey   []byte
}

// requireProductKeyTx compares a step-up's product key with the pin, read
// only: ErrProductKeyChanged when another key is pinned under its id, when
// nothing is, or when it names none and the identity has keys pinned.
func requireProductKeyTx(ctx context.Context, tx *sql.Tx, in ExternalProof) error {
	if in.ProductKeyID == "" && len(in.ProductKey) == 0 {
		var pinned int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity_key_pins WHERE issuer = ? AND subject = ?`,
			in.Issuer, in.Subject).Scan(&pinned); err != nil {
			return fmt.Errorf("auth: read the pinned keys: %w", err)
		}
		if pinned > 0 {
			return ErrProductKeyChanged
		}
		return nil
	}
	key, err := pinnedKeyTx(ctx, tx, in.Issuer, in.Subject, in.ProductKeyID)
	switch {
	case errors.Is(err, ErrProductKeyNotPinned):
		return ErrProductKeyChanged
	case err != nil:
		return err
	case subtle.ConstantTimeCompare(key, in.ProductKey) != 1:
		return ErrProductKeyChanged
	}
	return nil
}
