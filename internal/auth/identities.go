package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
)

// External identities are the other way in, beside a password: a person an
// identity provider vouched for, as an extension of the daemon presents them
// after it has done the provider's protocol (internal/app, Options.Extensions;
// the service's SignInExternal). The provider names the person by its issuer,
// an origin, and a subject, its own stable id for them; that pair is recorded
// with the one person its first sign-in created here (user_identities), and
// signs that person in from then on.
//
// What this package decides is who the pair is, and the session it gets:
//
//   - a pair already linked signs in its person, unless they are disabled;
//   - a pair seen for the first time only ever creates a person, and only
//     with an address the provider verified, never one lower case would make
//     another, and never one somebody here has already (ErrEmailTaken):
//     whoever has it, however they sign in, disabled or not, is not the
//     pair's to take over, and accounts are never linked by matching
//     addresses. The new person is an instance member without a password,
//     named as the provider names them;
//   - the session lasts what the extension asks, never more than SessionTTL,
//     and nothing extends it.
//
// It also keeps the public keys an identity is known by (identity_key_pins):
// each pinned the first time its key id is seen and never replaced, and gone
// only with the person the identity signs in (DeleteTx). A pin whose identity
// never came to be linked, because the sign-in it was pinned for was refused,
// signs nobody in, and the hourly sweep deletes it (SweepUnlinkedPins).

// Bounds of an identity, as the schema holds them (migration 0010).
const (
	// maxIssuerLength is more than any origin a DNS name makes.
	maxIssuerLength = 300
	// MaxSubjectLength is OpenID Connect's bound on a subject, in bytes.
	MaxSubjectLength = 255
	// MaxKeyIDLength bounds a pinned key's id, in bytes.
	MaxKeyIDLength = 255
	// MaxPinnedKeyBytes bounds a pinned key: a public key in any encoding
	// a provider publishes one in fits many times over.
	MaxPinnedKeyBytes = 4096
)

var (
	// ErrInvalidIssuer is an issuer that is not exactly an origin as a
	// browser writes it: https, or http on a loopback address or a name
	// under .localhost, a host in lower case, a port only when it is not
	// the scheme's, and nothing after it.
	ErrInvalidIssuer = errors.New("auth: an issuer is an origin, https://host[:port], or http:// on loopback or .localhost")
	// ErrInvalidSubject is a subject that is empty, longer than
	// MaxSubjectLength bytes, not UTF-8, or carries control characters.
	ErrInvalidSubject = errors.New("auth: a subject is 1 to 255 bytes of text without control characters")
	// ErrEmailNotVerified is an identity seen for the first time whose
	// provider does not vouch for its address: nothing is created on an
	// address nobody verified, and the refusal does not say whether
	// somebody here has it.
	ErrEmailNotVerified = errors.New("auth: the identity provider has not verified that address")
	// ErrInvalidSessionTTL is a session asked to last nothing, or longer
	// than SessionTTL.
	ErrInvalidSessionTTL = errors.New("auth: a session lasts more than nothing and at most 14 days")
	// ErrInvalidPin is a key id or a key out of bounds.
	ErrInvalidPin = errors.New("auth: a pinned key has an id of 1 to 255 bytes of text and 1 to 4096 bytes of key")
)

// ExternalSignIn is a person an identity provider vouched for.
type ExternalSignIn struct {
	// Issuer is the provider's origin, exactly as a browser writes one.
	Issuer string
	// Subject is the provider's stable id for the person.
	Subject string
	// Email is the person's address at the provider, and EmailVerified
	// whether the provider vouches for it. Only an identity seen for the
	// first time looks at them: it creates nobody with an address nobody
	// verified, one that lower case would make another address
	// (lowerCaseMakesAnother), or one somebody here has already.
	Email         string
	EmailVerified bool
	// Name is a new person's display name, as the provider has it: made
	// into one this server takes rather than refused (providerName), since
	// the person did not type it here. The person a linked identity signs
	// in keeps theirs, and it is not looked at.
	Name string
	// UserAgent is the browser's, for the session list.
	UserAgent string
	// TTL is how long the session lasts: more than nothing, at most
	// SessionTTL.
	TTL time.Duration
	// AuthTime is when the provider says the person last authenticated
	// (OpenID Connect's auth_time): the session's step-up time, at most
	// now, never the moment of the sign-in, so a sign-in the provider
	// answered from a session of its own opens no step-up window
	// (docs/key-scheme.md section 11). Zero is none.
	AuthTime time.Time
}

// SignInExternal signs in the person an identity provider vouched for, and
// starts a session for them that expires TTL after it starts. In one
// transaction: the pair already linked signs in its person; otherwise, with
// an address the provider verified that nobody here has, a new person is
// created for it, an instance member with no password, with what the
// workspace source creates for a person, as sign-up creates one, and the pair
// is linked to them.
//
// An address somebody here has already is refused with ErrEmailTaken, and
// nothing is created or linked: an identity never takes over a person who
// exists, however they sign in. The person of a linked identity who is
// disabled is refused with ErrUserDisabled. No password is hashed, so this
// takes no hashing slot.
func (u *Users) SignInExternal(ctx context.Context, in ExternalSignIn) (string, Session, User, error) {
	if err := CheckIssuer(in.Issuer); err != nil {
		return "", Session{}, User{}, err
	}
	if err := checkSubject(in.Subject); err != nil {
		return "", Session{}, User{}, err
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	if in.TTL <= 0 || in.TTL > SessionTTL {
		return "", Session{}, User{}, ErrInvalidSessionTTL
	}

	now := u.now().UTC().Truncate(time.Second)
	var (
		token   string
		session Session
		user    User
	)
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		userID, err := u.identifyTx(ctx, tx, in, email, now)
		if err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, userID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrUserNotFound
		case err != nil:
			return err
		case user.Disabled:
			// Only a linked identity's person can be: a new one is active.
			return ErrUserDisabled
		}
		authenticated := in.AuthTime
		if authenticated.After(now) {
			authenticated = now
		}
		token, session, err = startSessionTx(ctx, tx, user.ID, in.UserAgent, now, in.TTL, authenticated)
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// identifyTx returns the ID of the person in's identity signs in, inside
// SignInExternal's transaction: the one it is linked to, or a new person
// created for it and linked to it. email is in.Email normalized.
func (u *Users) identifyTx(ctx context.Context, tx *sql.Tx, in ExternalSignIn, email string, now time.Time) (string, error) {
	issuer, subject := in.Issuer, in.Subject
	var userID string
	err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?`, issuer, subject).Scan(&userID)
	switch {
	case err == nil:
		return userID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("auth: look up identity: %w", err)
	case !in.EmailVerified:
		// Before the address is looked up: an address the provider has not
		// verified learns nothing of who has an account here.
		return "", ErrEmailNotVerified
	case lowerCaseMakesAnother(in.Email):
		return "", ErrInvalidEmail
	}

	// An identity seen for the first time only ever creates a person. The
	// one who has the address here already, compared as sign-in compares it
	// (users.email is COLLATE NOCASE), is not its to take over, whoever they
	// are and however they sign in: accounts are never linked by matching
	// addresses.
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email = ?`, email).Scan(&taken); err != nil {
		return "", fmt.Errorf("auth: look up address: %w", err)
	}
	if taken > 0 {
		return "", ErrEmailTaken
	}
	if userID, err = u.createPasswordlessTx(ctx, tx, email, providerName(in.Name), now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identities(issuer, subject, user_id, created_at) VALUES (?, ?, ?, ?)`,
		issuer, subject, userID, now.Unix()); err != nil {
		return "", fmt.Errorf("auth: link identity: %w", err)
	}
	return userID, nil
}

// createPasswordlessTx creates a person who signs in through an identity
// provider, as sign-up creates one: an instance member, never an owner, with
// what the workspace source creates for a person. They have no password: the
// hash is empty, which nothing matches (verifyPassword), and
// password_changed_at is 0. The instance invites still waiting for their
// address are spent, as signing up spends them: none of them can sign the
// address up any more.
func (u *Users) createPasswordlessTx(ctx context.Context, tx *sql.Tx, email, name string, now time.Time) (string, error) {
	userID, err := newID("usr_")
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at,
		                   seal_id)
		 VALUES (?, ?, ?, '', ?, ?, 0, ?, ?, ?)`,
		userID, email, name, string(RoleMember), userActive, now.Unix(), now.Unix(), keyscheme.NewSealID())
	switch {
	case store.IsUnique(err):
		return "", ErrEmailTaken
	case err != nil:
		return "", fmt.Errorf("auth: create user: %w", err)
	}
	if err := u.source.PersonCreatedTx(ctx, tx, userID, now); err != nil {
		return "", err
	}
	if err := dropOtherInvitesTx(ctx, tx, email, "", true); err != nil {
		return "", err
	}
	return userID, nil
}

// lowerCaseMakesAnother reports whether lower case, which NormalizeEmail
// applies, turns an address into another one: the Kelvin sign (U+212A)
// becomes the letter k, the capital I with a dot (U+0130) the letter i, the
// Ohm sign (U+2126) the omega. An identity provider verified the address as
// it wrote it, and that mailbox is not the one lower case makes of it, which
// may be somebody else's here, or somebody's to come; so such an address
// creates nobody (ErrInvalidEmail). A letter whose lower case is its own pair
// (K and k, Ä and ä) is what NormalizeEmail has always folded.
func lowerCaseMakesAnother(s string) bool {
	for _, r := range s {
		if lower := unicode.ToLower(r); lower != r && unicode.ToUpper(lower) != r {
			return true
		}
	}
	return false
}

// providerName makes a new person's name, as their identity provider gives
// it, into one NormalizeName accepts, rather than refusing it: the person did
// not type it here, and what the provider holds must not keep them out. Bytes
// that are not UTF-8 and control characters become spaces, a run of spaces
// one, and a name longer than MaxNameLength is cut at a character.
func providerName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > MaxNameLength {
		s = strings.TrimSpace(string(runes[:MaxNameLength]))
	}
	return s
}

// PinKey pins key as the one keyID names for the identity (issuer, subject),
// unless a key is already pinned there, and returns the key pinned there now
// and whether this call pinned it. Both in one transaction, so two sign-ins
// racing each other both read the one that won. A pin is never replaced: a
// caller that gets back another key than the one it offered refuses it.
func (u *Users) PinKey(ctx context.Context, issuer, subject, keyID string, key []byte) ([]byte, bool, error) {
	if err := CheckIssuer(issuer); err != nil {
		return nil, false, err
	}
	if err := checkSubject(subject); err != nil {
		return nil, false, err
	}
	if !boundedText(keyID, MaxKeyIDLength) || len(key) == 0 || len(key) > MaxPinnedKeyBytes {
		return nil, false, ErrInvalidPin
	}
	var (
		pinned   []byte
		inserted bool
	)
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		n, err := execCount(ctx, tx,
			`INSERT INTO identity_key_pins(issuer, subject, key_id, key, pinned_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT DO NOTHING`,
			issuer, subject, keyID, key, u.now().Unix())
		if err != nil {
			return fmt.Errorf("auth: pin key: %w", err)
		}
		inserted = n == 1
		if err := tx.QueryRowContext(ctx,
			`SELECT key FROM identity_key_pins WHERE issuer = ? AND subject = ? AND key_id = ?`,
			issuer, subject, keyID).Scan(&pinned); err != nil {
			return fmt.Errorf("auth: read pinned key: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// Scan copied it out of the driver's buffer: the caller owns it.
	return pinned, inserted, nil
}

// deleteIdentitiesTx deletes a person's identities and the keys pinned for
// them, inside DeleteTx's transaction, and reports how many identities went.
// The identities go first: the schema keeps a pin while its identity exists
// (identity_key_pins_kept).
func deleteIdentitiesTx(ctx context.Context, tx *sql.Tx, userID string) (int, error) {
	gone, err := deletedIdentities(ctx, tx, userID)
	if err != nil {
		return 0, fmt.Errorf("auth: delete the user's identities: %w", err)
	}
	for _, ident := range gone {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM identity_key_pins WHERE issuer = ? AND subject = ?`, ident.issuer, ident.subject); err != nil {
			return 0, fmt.Errorf("auth: delete the user's pinned keys: %w", err)
		}
	}
	return len(gone), nil
}

// identity is an issuer and a subject.
type identity struct{ issuer, subject string }

// deletedIdentities deletes a person's identities and returns them, every row
// read and the statement finished before the pins are touched.
func deletedIdentities(ctx context.Context, tx *sql.Tx, userID string) ([]identity, error) {
	rows, err := tx.QueryContext(ctx,
		`DELETE FROM user_identities WHERE user_id = ? RETURNING issuer, subject`, userID)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // rows.Err below reports what matters
	defer func() { _ = rows.Close() }()
	var out []identity
	for rows.Next() {
		var ident identity
		if err := rows.Scan(&ident.issuer, &ident.subject); err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

// UnlinkedPinGrace is how old a pin whose identity signs nobody in may get
// before SweepUnlinkedPins deletes it. An extension pins a key just before
// the sign-in that links its identity, seconds later; a pin still unlinked
// after this was pinned for a sign-in that was refused (an address nobody
// verified, an address somebody here has already), and is the provider's id
// and key for an identity that signs nobody in here.
const UnlinkedPinGrace = 10 * time.Minute

// SweepUnlinkedPins deletes the pins older than UnlinkedPinGrace whose
// identity signs nobody in, and reports how many it deleted. The daemon's
// hourly sweep runs it, so such a pin is kept at most an hour and
// UnlinkedPinGrace. A pin whose identity is linked is never swept: it goes
// only with its person (DeleteTx).
func (u *Users) SweepUnlinkedPins(ctx context.Context) (int, error) {
	cutoff := u.now().Add(-UnlinkedPinGrace).Unix()
	var n int
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = execCount(ctx, tx,
			`DELETE FROM identity_key_pins
			  WHERE pinned_at < ?
			    AND NOT EXISTS (SELECT 1 FROM user_identities i
			                     WHERE i.issuer = identity_key_pins.issuer AND i.subject = identity_key_pins.subject)`,
			cutoff)
		if err != nil {
			return fmt.Errorf("auth: sweep unlinked pins: %w", err)
		}
		return nil
	})
	return n, err
}

// CheckIssuer refuses, with ErrInvalidIssuer, an issuer that is not exactly
// an origin as a browser writes it. Exactly, rather than tidied up: the
// issuer is half of what names an identity, and two spellings of one origin
// must not name two people.
func CheckIssuer(issuer string) error {
	if issuer == "" || len(issuer) > maxIssuerLength {
		return ErrInvalidIssuer
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.ContainsAny(issuer, "?#") {
		return ErrInvalidIssuer
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || !issuerHost(host) {
		return ErrInvalidIssuer
	}
	switch u.Scheme {
	case "https":
		if port == "443" {
			return ErrInvalidIssuer
		}
	case "http":
		if port == "80" || !neverLeavesTheMachine(host) {
			return ErrInvalidIssuer
		}
	default:
		return ErrInvalidIssuer
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return ErrInvalidIssuer
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return ErrInvalidIssuer
	}
	return nil
}

// issuerHost reports whether host is a DNS name in lower case, of letters,
// digits and hyphens, or an IP address written as one is canonically.
func issuerHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String() == host && addr.Zone() == ""
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// neverLeavesTheMachine reports whether a host is loopback: localhost, a name
// under .localhost, which a browser resolves to loopback and treats as a
// secure context, or a loopback address.
func neverLeavesTheMachine(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func checkSubject(subject string) error {
	if !boundedText(subject, MaxSubjectLength) {
		return ErrInvalidSubject
	}
	return nil
}

// boundedText reports whether s is 1 to max bytes of UTF-8 without control
// characters.
func boundedText(s string, maxBytes int) bool {
	return s != "" && len(s) <= maxBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
