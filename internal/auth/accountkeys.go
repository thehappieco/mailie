package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/thehappieco/kit/profiles/platform"

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A person's account key, and a password this server never receives
// (docs/key-scheme.md, sections 3 to 5, 11 and 12; this file is what the
// server does in each ceremony of section 12).
//
// The browser stretches the password with Argon2id under a salt and
// parameters this server hands out, and splits the result: the auth key,
// which it sends and the server keeps only as a hash (auth_verifier), proves
// who the person is; the wrap key, which never leaves the browser, wraps the
// person's account key, an X25519 key pair the browser made. The server keeps
// the public half (public_key), written once, and the wraps (password_wrap,
// recovery_wrap), which it cannot open and hands out only against a secret
// verified in the same request. A recovery code does for the recovery wrap
// what the password does for the password wrap, with a proof the server
// keeps as a hash (recovery_verifier).
//
// The server checks shapes, never contents: a public key is a valid X25519
// key (keyscheme.CheckPublicKey), a wrap is 61 bytes starting with 0x02
// (keyscheme.CheckAccountWrapShape), an auth key and a proof are base64url
// of 32 bytes, and the parameters are its own default. Every refusal of a
// secret is one error after the same work, whether the address has an
// account, is disabled, has no password, or the secret is wrong.

// KDF is the Argon2id parameters a browser derives a password with: m in KiB,
// t passes, p lanes, a 32-byte output. The algorithm is always argon2id.
type KDF struct {
	M int
	T int
	P int
}

// DefaultKDF is the server's default (docs/key-scheme.md section 5.2): every
// account's target, and the only parameters a ceremony stores. A later
// release may raise it within the platform's bounds; every account then moves
// to it at the person's next sign-in.
var DefaultKDF = KDF{M: int(keyscheme.DefaultKDF.M), T: int(keyscheme.DefaultKDF.T), P: int(keyscheme.DefaultKDF.P)}

// KDFAlg is the one algorithm a KDF names.
const KDFAlg = "argon2id"

// Target is a salt and parameters a browser derives under: what a challenge
// answers, and what a ceremony that stores a verifier stores.
type Target struct {
	Salt []byte
	KDF  KDF
}

// Lifetimes of what the ceremonies hand out.
const (
	// TicketTTL is how long a ceremony's ticket lasts: the re-derivation of a
	// sign-in, a password change, a recovery, the upgrade's enrolment.
	TicketTTL = 10 * time.Minute
	// StepUpWindow is how long after a sign-in or a step-up a session may do
	// what the step-up guards (docs/key-scheme.md section 11).
	StepUpWindow = 10 * time.Minute
	// ResetTTL is how long a reset invitation lasts, as an invite does.
	ResetTTL = InviteTTL
)

const (
	ticketBytes     = 32
	resetCodeBytes  = 32
	secretTextBytes = 32 // an auth key's and a proof's decoded size
)

// Ticket purposes, as auth_tickets stores them.
//
//nolint:gosec // G101: the names of ceremonies, not credentials
const (
	ticketPassword = "password"
	ticketRederive = "rederive"
	ticketRecover  = "recover"
	ticketEnrol    = "enrol"
)

var (
	// ErrMalformedSecret is an auth key or a recovery proof that is not
	// strict base64url of 32 bytes: never one a browser derived.
	ErrMalformedSecret = errors.New("auth: an auth key or a recovery proof is base64url of 32 bytes")
	// ErrInvalidPublicKey is an account public key the server refuses to
	// store (keyscheme.ErrPublicKey).
	ErrInvalidPublicKey = errors.New("auth: not an account public key the server accepts")
	// ErrInvalidWrap is a password or recovery wrap of the wrong shape.
	ErrInvalidWrap = errors.New("auth: an account wrap is 61 bytes starting with 0x02")
	// ErrKDFNotCurrent is a derivation under parameters other than the
	// server's default now, or other than the ticket's.
	ErrKDFNotCurrent = errors.New("auth: derive under the salt and parameters the server answers now")
	// ErrTicketInvalid is one answer for a ticket that does not exist, has
	// expired, was used, or belongs to another person, session or ceremony.
	ErrTicketInvalid = errors.New("auth: the ticket is not valid, was used, or has expired")
	// ErrStepUpNeeded is something the step-up guards, asked of a session
	// whose step-up time is more than StepUpWindow old, or none.
	ErrStepUpNeeded = errors.New("auth: this needs a sign-in or a step-up within the last ten minutes")
	// ErrResetInvalid is one answer for a reset invitation that does not
	// exist, has expired, was used, is for another address, or whose person
	// is disabled.
	ErrResetInvalid = errors.New("auth: the reset invitation is not valid, has been used, or has expired")
	// ErrStepUpRefused is a step-up through an identity provider that does
	// not prove the session's own person, recently: no mark, an old one, an
	// authentication time before it or after now, or another identity.
	ErrStepUpRefused = errors.New("auth: the step-up does not prove this session's person")
)

// Enrolment is what a browser sends to enrol a person in the scheme, or to
// replace their account key: everything the server stores of the account
// key, the password and the recovery code, none of which opens anything here.
type Enrolment struct {
	// AuthKey is base64url of the 32-byte auth key.
	AuthKey string
	// KDF is what the auth key was derived with: the server's default.
	KDF KDF
	// PublicKey is the account public key, 32 bytes.
	PublicKey []byte
	// PasswordWrap and RecoveryWrap are the account key under the password
	// and the recovery code.
	PasswordWrap []byte
	RecoveryWrap []byte
	// RecoveryProof is base64url of the recovery code's 32-byte proof.
	RecoveryProof string
}

// check holds an enrolment to its shapes, before anything is hashed.
func (e Enrolment) check() error {
	if err := checkSecretText(e.AuthKey); err != nil {
		return err
	}
	if err := checkSecretText(e.RecoveryProof); err != nil {
		return err
	}
	if e.KDF != DefaultKDF {
		return ErrKDFNotCurrent
	}
	if err := keyscheme.CheckPublicKey(e.PublicKey); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPublicKey, err)
	}
	return checkWraps(e.PasswordWrap, e.RecoveryWrap)
}

// checkWraps holds account wraps to their shape.
func checkWraps(wraps ...[]byte) error {
	for _, w := range wraps {
		if err := keyscheme.CheckAccountWrapShape(w); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidWrap, err)
		}
	}
	return nil
}

// checkSecretText holds an auth key or a proof to its one spelling: strict
// base64url of 32 bytes, 43 characters.
func checkSecretText(s string) error {
	b, err := platform.DecodeB64(s, secretTextBytes)
	if err != nil {
		return ErrMalformedSecret
	}
	clear(b)
	return nil
}

// verifiers are the hashes an enrolment stores.
type verifiers struct{ auth, recovery string }

// hashVerifiers hashes an enrolment's auth key and recovery proof.
func hashVerifiers(ctx context.Context, authKey, proof string) (verifiers, error) {
	auth, err := hashPersonSecret(ctx, authKey)
	if err != nil {
		return verifiers{}, err
	}
	recovery, err := hashPersonSecret(ctx, proof)
	if err != nil {
		return verifiers{}, err
	}
	return verifiers{auth: auth, recovery: recovery}, nil
}

// target is an address's target: its salt under the server's salt key, and
// the default parameters (docs/key-scheme.md section 5.3).
func (u *Users) target(email string) (Target, error) {
	salt, err := keyscheme.DecoySalt(u.saltKey, email)
	if err != nil {
		return Target{}, err
	}
	return Target{Salt: salt, KDF: DefaultKDF}, nil
}

// Challenge is what a challenge answers for an address.
type Challenge struct {
	Target
	// Upgrade says the address has an active person whose password the
	// server still checks itself: their next sign-in is the upgrade's
	// (docs/key-scheme.md section 12.7). Only this release answers it.
	Upgrade bool
}

// Challenge answers the salt and parameters a browser derives a password
// under for an address: an enrolled person's stored ones, and for any other
// address (unknown, disabled, a person with no password) the address's
// target, so that an account at its target is answered exactly what its
// address would be without it. The one exception is Upgrade.
func (u *Users) Challenge(ctx context.Context, email string) (Challenge, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Challenge{}, err
	}
	target, err := u.target(email)
	if err != nil {
		return Challenge{}, err
	}
	var (
		status, hash string
		enrolled     int64
		salt         []byte
		m, t, p      int
	)
	err = u.store.Reader().QueryRowContext(ctx,
		`SELECT status, password_hash, zk_enrolled_at, kdf_salt, kdf_m, kdf_t, kdf_p FROM users WHERE email = ?`, email,
	).Scan(&status, &hash, &enrolled, &salt, &m, &t, &p)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Challenge{Target: target}, nil
	case err != nil:
		return Challenge{}, fmt.Errorf("auth: challenge: %w", err)
	case status != userActive:
		return Challenge{Target: target}, nil
	case enrolled != 0:
		return Challenge{Target: Target{Salt: salt, KDF: KDF{M: m, T: t, P: p}}}, nil
	case hash != "":
		return Challenge{Target: target, Upgrade: true}, nil
	}
	return Challenge{Target: target}, nil
}

// Rederive is the target a sign-in names when the account is not at it, and
// the ticket the browser re-derives under it with (password/finish).
type Rederive struct {
	Target
	Ticket string
}

// Login is a sign-in with an auth key.
type Login struct {
	Token   string
	Session Session
	User    User
	// PasswordWrap is the account key under the password, for the browser
	// to open: answered only to the auth key verified in this request.
	PasswordWrap []byte
	// Rederive is set when the account's salt or parameters are not its
	// target (docs/key-scheme.md section 12.2, step 5).
	Rederive *Rederive
}

// enrolledRow is what the ceremonies read of a person.
type enrolledRow struct {
	user     User
	status   string
	verifier string
	recovery string
	salt     []byte
	kdf      KDF
	wrap     []byte
	rwrap    []byte
	enrolled bool
}

const enrolledColumns = userColumns + `, status, auth_verifier, recovery_verifier, kdf_salt, kdf_m, kdf_t, kdf_p,
	password_wrap, recovery_wrap`

func scanEnrolled(row interface{ Scan(...any) error }) (enrolledRow, error) {
	var (
		r      enrolledRow
		status string
	)
	err := row.Scan(append(userFields(&r.user, &status), &r.status, &r.verifier, &r.recovery, &r.salt,
		&r.kdf.M, &r.kdf.T, &r.kdf.P, &r.wrap, &r.rwrap)...)
	if err != nil {
		return enrolledRow{}, err
	}
	r.user.Disabled = status != userActive
	r.enrolled = r.user.Enrolled
	return r, nil
}

// signsIn reports whether a person may prove a secret at all: active and
// enrolled. Anyone else is checked against the dummy.
func (r enrolledRow) signsIn() bool { return r.status == userActive && r.enrolled }

// Login signs a person in with an auth key and starts a session whose
// step-up time is now.
//
// The shape is the security property, as in SignIn before it: one Argon2id
// derivation whether or not the address has an account, and one error,
// ErrBadCredentials, for every way of failing: an unknown address, a disabled
// person, a person not enrolled (who signs in through the upgrade, or an
// identity provider), and a wrong key.
func (u *Users) Login(ctx context.Context, email, authKey, userAgent string) (Login, error) {
	if err := checkSecretText(authKey); err != nil {
		return Login{}, err
	}
	email = keyscheme.NormaliseAddress(email)
	r, err := scanEnrolled(u.store.Reader().QueryRowContext(ctx,
		`SELECT `+enrolledColumns+` FROM users WHERE email = ?`, email))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Login{}, fmt.Errorf("auth: sign in: %w", err)
	}
	verifier := ""
	if err == nil && r.signsIn() {
		verifier = r.verifier
	}
	ok, err := verifyPersonSecret(ctx, authKey, verifier)
	if err != nil {
		return Login{}, err
	}
	if !ok {
		return Login{}, ErrBadCredentials
	}
	target, err := u.target(r.user.Email)
	if err != nil {
		return Login{}, err
	}

	out := Login{User: r.user, PasswordWrap: r.wrap}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		// Read again under the write lock: the person may have been
		// disabled, or their password changed, while the hash ran.
		var status, current string
		if err := tx.QueryRowContext(ctx, `SELECT status, auth_verifier FROM users WHERE id = ?`, r.user.ID).
			Scan(&status, &current); err != nil {
			return fmt.Errorf("auth: sign in: %w", err)
		}
		if status != userActive || current != r.verifier {
			return ErrBadCredentials
		}
		now := u.now()
		var err error
		out.Token, out.Session, err = startSessionTx(ctx, tx, r.user.ID, userAgent, now, SessionTTL, now)
		if err != nil {
			return err
		}
		if !sameTarget(Target{Salt: r.salt, KDF: r.kdf}, target) {
			ticket, err := issueTicketTx(ctx, tx, r.user.ID, out.Session.ID, ticketRederive, target, now)
			if err != nil {
				return err
			}
			out.Rederive = &Rederive{Target: target, Ticket: ticket}
		}
		return nil
	})
	if err != nil {
		return Login{}, err
	}
	return out, nil
}

func sameTarget(a, b Target) bool { return a.KDF == b.KDF && string(a.Salt) == string(b.Salt) }

// PasswordChange is the first half of a password change: the current
// password wrap and the account's target, answered only to the current auth
// key verified in the same request, and the ticket that finishes it.
type PasswordChange struct {
	Target
	PasswordWrap []byte
	Ticket       string
}

// BeginPasswordChange checks the current auth key of a session's person and
// answers what changing the password needs (docs/key-scheme.md section
// 12.3). A wrong key, or a person not enrolled, is ErrBadCredentials after
// the same work; nothing changes.
func (u *Users) BeginPasswordChange(ctx context.Context, userID, sessionID, currentAuthKey string) (PasswordChange, error) {
	r, err := u.proveSecret(ctx, userID, currentAuthKey, func(r enrolledRow) string { return r.verifier })
	if err != nil {
		return PasswordChange{}, err
	}
	target, err := u.target(r.user.Email)
	if err != nil {
		return PasswordChange{}, err
	}
	out := PasswordChange{Target: target, PasswordWrap: r.wrap}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		if err := requireLiveSessionTx(ctx, tx, userID, sessionID, u.now()); err != nil {
			return err
		}
		var err error
		out.Ticket, err = issueTicketTx(ctx, tx, userID, sessionID, ticketPassword, target, u.now())
		return err
	})
	if err != nil {
		return PasswordChange{}, err
	}
	return out, nil
}

// proveSecret checks a secret of a person, read by userID, against the
// verifier pick names: one derivation whatever the person's state, and
// ErrBadCredentials for every way of failing.
func (u *Users) proveSecret(ctx context.Context, userID, secret string, pick func(enrolledRow) string) (enrolledRow, error) {
	if err := checkSecretText(secret); err != nil {
		return enrolledRow{}, err
	}
	r, err := scanEnrolled(u.store.Reader().QueryRowContext(ctx,
		`SELECT `+enrolledColumns+` FROM users WHERE id = ?`, userID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return enrolledRow{}, fmt.Errorf("auth: read the person: %w", err)
	}
	verifier := ""
	if err == nil && r.signsIn() {
		verifier = pick(r)
	}
	ok, err := verifyPersonSecret(ctx, secret, verifier)
	if err != nil {
		return enrolledRow{}, err
	}
	if !ok {
		return enrolledRow{}, ErrBadCredentials
	}
	return r, nil
}

// NewPassword is the second half of a password change, or a sign-in's
// re-derivation: the auth key and the password wrap under the target the
// ticket carries.
type NewPassword struct {
	Ticket       string
	AuthKey      string
	KDF          KDF
	PasswordWrap []byte
}

// PasswordChanged is what finishing a password change did.
type PasswordChanged struct {
	// Rotated is a change: every session of the person ended, and Token
	// and Session are the new one this browser goes on with. A sign-in's
	// re-derivation ends nothing, and they are empty.
	Rotated bool
	Token   string
	Session Session
}

// FinishPasswordChange stores a new auth key and password wrap under the
// target its ticket carries, for the ticket's person and session: a password
// change (BeginPasswordChange's ticket) or a sign-in's re-derivation
// (Login's). The account key, its public key, the grants and the recovery
// wrap do not change. A change ends every session of the person and starts
// one for this browser, with this session's step-up time; a re-derivation
// ends nothing. The ticket is used once, and every other ticket of the
// person goes with it.
func (u *Users) FinishPasswordChange(ctx context.Context, userID, sessionID string, in NewPassword, userAgent string) (PasswordChanged, error) {
	if err := checkSecretText(in.AuthKey); err != nil {
		return PasswordChanged{}, err
	}
	if in.KDF != DefaultKDF {
		return PasswordChanged{}, ErrKDFNotCurrent
	}
	if err := checkWraps(in.PasswordWrap); err != nil {
		return PasswordChanged{}, err
	}
	ticketHash, ok := hashTicket(in.Ticket)
	if !ok {
		return PasswordChanged{}, ErrTicketInvalid
	}
	verifier, err := hashPersonSecret(ctx, in.AuthKey)
	if err != nil {
		return PasswordChanged{}, err
	}
	var out PasswordChanged
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		t, err := consumeTicketTx(ctx, tx, ticketHash, now, userID, sessionID, ticketPassword, ticketRederive)
		if err != nil {
			return err
		}
		if t.target.KDF != in.KDF {
			return ErrKDFNotCurrent
		}
		if err := requireEnrolledTx(ctx, tx, userID); err != nil {
			return err
		}
		// A re-derivation is the same password: only a change moves when it
		// last changed.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET auth_verifier = ?1, kdf_salt = ?2, kdf_m = ?3, kdf_t = ?4,
			kdf_p = ?5, password_wrap = ?7, updated_at = ?6,
			password_changed_at = CASE WHEN ?9 THEN ?6 ELSE password_changed_at END WHERE id = ?8`,
			verifier, t.target.Salt, t.target.KDF.M, t.target.KDF.T, t.target.KDF.P, now.Unix(), in.PasswordWrap, userID,
			t.purpose == ticketPassword,
		); err != nil {
			return fmt.Errorf("auth: change password: %w", err)
		}
		if err := dropTicketsTx(ctx, tx, userID); err != nil {
			return err
		}
		if t.purpose == ticketRederive {
			return nil
		}
		// A change ends everything, as it always has: the usual reason for
		// changing a password is no longer trusting where the old one was
		// typed. This browser goes on with a new session, as old as its
		// step-up was.
		var stepUp int64
		if err := tx.QueryRowContext(ctx, `SELECT authenticated_at FROM sessions WHERE id = ?`, sessionID).
			Scan(&stepUp); err != nil {
			return fmt.Errorf("auth: change password: %w", err)
		}
		if _, err := revokeSessionsTx(ctx, tx, userID, now.Unix()); err != nil {
			return err
		}
		out.Rotated = true
		out.Token, out.Session, err = startSessionTx(ctx, tx, userID, userAgent, now, SessionTTL, unixOrZero(stepUp))
		return err
	})
	if err != nil {
		return PasswordChanged{}, err
	}
	return out, nil
}

// Recovery is what opening a recovery answers: the recovery wrap, answered
// only to the recovery proof verified in the same request, the account's
// target, and the ticket that finishes it.
type Recovery struct {
	Target
	SealID       string
	PublicKey    []byte
	RecoveryWrap []byte
	Ticket       string
}

// OpenRecovery checks a recovery proof for an address and answers what a
// recovery needs (docs/key-scheme.md section 12.4). Every way of failing is
// ErrBadCredentials after the same work.
func (u *Users) OpenRecovery(ctx context.Context, email, proof string) (Recovery, error) {
	if err := checkSecretText(proof); err != nil {
		return Recovery{}, err
	}
	email = keyscheme.NormaliseAddress(email)
	r, err := scanEnrolled(u.store.Reader().QueryRowContext(ctx,
		`SELECT `+enrolledColumns+` FROM users WHERE email = ?`, email))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Recovery{}, fmt.Errorf("auth: open recovery: %w", err)
	}
	verifier := ""
	if err == nil && r.signsIn() {
		verifier = r.recovery
	}
	ok, err := verifyPersonSecret(ctx, proof, verifier)
	if err != nil {
		return Recovery{}, err
	}
	if !ok {
		return Recovery{}, ErrBadCredentials
	}
	target, err := u.target(r.user.Email)
	if err != nil {
		return Recovery{}, err
	}
	out := Recovery{Target: target, SealID: r.user.SealID, PublicKey: r.user.PublicKey, RecoveryWrap: r.rwrap}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		// Read again under the write lock, as a sign-in does: disabled, or
		// gone, while the hash ran, or the code replaced (ReplaceRecovery),
		// is a wrong proof. A ticket issued after the replacement committed
		// would answer the wrap of a code its person has put aside, and no
		// replacement would be left to delete it.
		if err := requireVerifierTx(ctx, tx, r.user.ID, recoveryVerifier, r.recovery); err != nil {
			return asRefusal(err, ErrBadCredentials)
		}
		var err error
		out.Ticket, err = issueTicketTx(ctx, tx, r.user.ID, "", ticketRecover, target, u.now())
		return err
	})
	if err != nil {
		return Recovery{}, err
	}
	return out, nil
}

// RecoveryFinish is the second half of a recovery: a new password and a new
// recovery code over the same account key.
type RecoveryFinish struct {
	Ticket        string
	AuthKey       string
	KDF           KDF
	PasswordWrap  []byte
	RecoveryWrap  []byte
	RecoveryProof string
}

// FinishRecovery stores a new password and recovery code for the ticket's
// person, under the target the ticket carries. The account key is unchanged,
// so every grant still opens. Every session of the person ends, and every
// other ticket; the person signs in with the new password.
func (u *Users) FinishRecovery(ctx context.Context, in RecoveryFinish) error {
	if err := checkSecretText(in.AuthKey); err != nil {
		return err
	}
	if err := checkSecretText(in.RecoveryProof); err != nil {
		return err
	}
	if in.KDF != DefaultKDF {
		return ErrKDFNotCurrent
	}
	if err := checkWraps(in.PasswordWrap, in.RecoveryWrap); err != nil {
		return err
	}
	ticketHash, ok := hashTicket(in.Ticket)
	if !ok {
		return ErrTicketInvalid
	}
	hashed, err := hashVerifiers(ctx, in.AuthKey, in.RecoveryProof)
	if err != nil {
		return err
	}
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		t, err := consumeTicketTx(ctx, tx, ticketHash, now, "", "", ticketRecover)
		if err != nil {
			return err
		}
		if t.target.KDF != in.KDF {
			return ErrKDFNotCurrent
		}
		if err := requireEnrolledTx(ctx, tx, t.userID); err != nil {
			return asRefusal(err, ErrTicketInvalid)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET auth_verifier = ?, kdf_salt = ?, kdf_m = ?, kdf_t = ?,
			kdf_p = ?, password_wrap = ?, recovery_wrap = ?, recovery_verifier = ?, password_changed_at = ?,
			updated_at = ? WHERE id = ?`,
			hashed.auth, t.target.Salt, t.target.KDF.M, t.target.KDF.T, t.target.KDF.P, in.PasswordWrap,
			in.RecoveryWrap, hashed.recovery, now.Unix(), now.Unix(), t.userID,
		); err != nil {
			return fmt.Errorf("auth: recover: %w", err)
		}
		if err := dropTicketsTx(ctx, tx, t.userID); err != nil {
			return err
		}
		_, err = revokeSessionsTx(ctx, tx, t.userID, now.Unix())
		return err
	})
}

// ReplaceRecovery replaces the recovery wrap and its proof of a session's
// person, who made a new recovery code over the account key their browser
// holds, with the current auth key verified in the same request
// (docs/key-scheme.md section 12.5). A session alone, however recent its
// sign-in, does not: a recovery code it could set would be a password it
// could set, through a recovery. A key that does not verify is
// ErrBadCredentials after the same work, and changes nothing. Every recovery
// the person has open, opened with the old code, ends with it.
func (u *Users) ReplaceRecovery(ctx context.Context, userID, sessionID, currentAuthKey string, recoveryWrap []byte, proof string) error {
	if err := checkSecretText(proof); err != nil {
		return err
	}
	if err := checkWraps(recoveryWrap); err != nil {
		return err
	}
	r, err := u.proveSecret(ctx, userID, currentAuthKey, func(r enrolledRow) string { return r.verifier })
	if err != nil {
		return err
	}
	verifier, err := hashPersonSecret(ctx, proof)
	if err != nil {
		return err
	}
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		// Asked again where it counts: the session may have ended, or the
		// password changed, while the hashes ran.
		if err := requireLiveSessionTx(ctx, tx, userID, sessionID, u.now()); err != nil {
			return err
		}
		if err := requireVerifierTx(ctx, tx, userID, authVerifier, r.verifier); err != nil {
			return asRefusal(err, ErrBadCredentials)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET recovery_wrap = ?, recovery_verifier = ?, updated_at = ? WHERE id = ?`,
			recoveryWrap, verifier, u.now().Unix(), userID); err != nil {
			return fmt.Errorf("auth: replace the recovery code: %w", err)
		}
		// A recovery opened with the code just replaced must not finish: it
		// would set a password with a code its person has put aside. A
		// password change or a re-derivation in flight proved the password,
		// which has not changed, and goes on.
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tickets WHERE user_id = ? AND purpose = ?`,
			userID, ticketRecover); err != nil {
			return fmt.Errorf("auth: replace the recovery code: %w", err)
		}
		return nil
	})
}

// StepUp checks the auth key of a session's own person and, when it
// verifies, sets that session's step-up time to now and returns it. It
// carries no address: it proves the session's person or nobody. A step-up
// that does not verify is ErrBadCredentials, and changes nothing.
func (u *Users) StepUp(ctx context.Context, userID, sessionID, authKey string) (time.Time, error) {
	r, err := u.proveSecret(ctx, userID, authKey, func(r enrolledRow) string { return r.verifier })
	if err != nil {
		return time.Time{}, err
	}
	now := u.now().UTC().Truncate(time.Second)
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		if err := requireLiveSessionTx(ctx, tx, userID, sessionID, now); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT auth_verifier FROM users WHERE id = ?`, userID).Scan(&current); err != nil {
			return fmt.Errorf("auth: step up: %w", err)
		}
		if current != r.verifier {
			return ErrBadCredentials
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET authenticated_at = ? WHERE id = ?`, now.Unix(), sessionID)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return now, nil
}

// RequireStepUp refuses, with ErrStepUpNeeded, a session whose step-up time
// is more than StepUpWindow old, later than now, or none: what the step-up
// guards (docs/key-scheme.md section 11) asks it first.
func (u *Users) RequireStepUp(ctx context.Context, userID, sessionID string) error {
	return requireStepUpOn(ctx, u.store.Reader(), userID, sessionID, u.now())
}

// RequireStepUpTx is RequireStepUp inside the caller's transaction, where
// what it guards is written.
func (u *Users) RequireStepUpTx(ctx context.Context, tx *sql.Tx, userID, sessionID string) error {
	return requireStepUpTx(ctx, tx, userID, sessionID, u.now())
}

func requireStepUpTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, now time.Time) error {
	return requireStepUpOn(ctx, tx, userID, sessionID, now)
}

func requireStepUpOn(ctx context.Context, q rowQuerier, userID, sessionID string, now time.Time) error {
	var (
		owner, status        string
		at, expires, revoked int64
	)
	err := q.QueryRowContext(ctx, `SELECT s.user_id, s.authenticated_at, s.expires_at, s.revoked_at, u.status
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, sessionID,
	).Scan(&owner, &at, &expires, &revoked, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInvalidSession
	case err != nil:
		return fmt.Errorf("auth: read the step-up: %w", err)
	case owner != userID || !sessionLive(now, expires, revoked, status):
		return ErrInvalidSession
	}
	if !freshStepUp(at, now) {
		return ErrStepUpNeeded
	}
	return nil
}

// freshStepUp reports whether a step-up time is within StepUpWindow of now,
// and not after it.
func freshStepUp(at int64, now time.Time) bool {
	if at == 0 {
		return false
	}
	t := time.Unix(at, 0)
	return !t.After(now) && now.Sub(t) <= StepUpWindow
}

// MarkExternalStepUp records on a session that its page starts a step-up
// through an identity provider (docs/key-scheme.md section 11, hosted): the
// mark is the time now, bound to this session, used once, valid for
// StepUpWindow. A new mark replaces one not yet used.
func (u *Users) MarkExternalStepUp(ctx context.Context, userID, sessionID string) (time.Time, error) {
	now := u.now().UTC().Truncate(time.Second)
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		if err := requireLiveSessionTx(ctx, tx, userID, sessionID, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET stepup_mark_at = ? WHERE id = ?`, now.Unix(), sessionID)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return now, nil
}

// ExternalStepUp finishes a step-up through an identity provider: the
// provider says (issuer, subject) authenticated at authTime. It is refused
// with ErrStepUpRefused, changing nothing, unless the session has a mark
// younger than StepUpWindow, authTime is after the mark and not after now,
// and (issuer, subject) is the identity linked to the session's person: a
// step-up as anyone else proves nothing about this session, whatever its
// time. Otherwise the mark is used and the session's step-up time becomes
// authTime. It never creates a session, nor changes another one or whose
// this one is.
func (u *Users) ExternalStepUp(ctx context.Context, userID, sessionID, issuer, subject string, authTime time.Time) error {
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		if err := requireLiveSessionTx(ctx, tx, userID, sessionID, now); err != nil {
			return err
		}
		var mark int64
		if err := tx.QueryRowContext(ctx, `SELECT stepup_mark_at FROM sessions WHERE id = ?`, sessionID).
			Scan(&mark); err != nil {
			return fmt.Errorf("auth: step up: %w", err)
		}
		at := authTime.Unix()
		if mark == 0 || now.Sub(time.Unix(mark, 0)) > StepUpWindow || at <= mark || authTime.After(now) {
			return ErrStepUpRefused
		}
		var linked int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM user_identities WHERE issuer = ? AND subject = ? AND user_id = ?`,
			issuer, subject, userID).Scan(&linked); err != nil {
			return fmt.Errorf("auth: step up: %w", err)
		}
		if linked == 0 {
			return ErrStepUpRefused
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET authenticated_at = ?, stepup_mark_at = 0 WHERE id = ?`,
			at, sessionID)
		return err
	})
}

// UpgradeTicket is what the upgrade's check of an old password answers: the
// ticket to enrol with, the person's seal id to bind the wraps to, and the
// target to derive under, which the ticket carries and Enrol stores.
type UpgradeTicket struct {
	Target
	Ticket string
	SealID string
}

// LegacySignIn is the upgrade's one last check of a password in clear
// (docs/key-scheme.md section 12.7): an active person whom the server still
// checks a password for, and has not enrolled, gets a ticket to enrol with
// (Enrol) when the password matches their old hash, under the old rules,
// with their seal id and the target the ticket carries. It answers no
// session. Every other case, an enrolled person's included, is
// ErrBadCredentials after the same derivation: the server never checks a
// password in clear for anyone who has enrolled. It exists in the release
// that brings the scheme only.
func (u *Users) LegacySignIn(ctx context.Context, email, password string) (UpgradeTicket, error) {
	email = keyscheme.NormaliseAddress(email)
	var (
		id, status, hash, sealID string
		enrolled                 int64
	)
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT id, status, password_hash, zk_enrolled_at, seal_id FROM users WHERE email = ?`, email,
	).Scan(&id, &status, &hash, &enrolled, &sealID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UpgradeTicket{}, fmt.Errorf("auth: sign in: %w", err)
	}
	legacy := err == nil && status == userActive && enrolled == 0 && hash != ""
	against := dummyPasswordHash
	if legacy {
		against = hash
	}
	ok, err := verifyPassword(ctx, password, against)
	if err != nil {
		return UpgradeTicket{}, err
	}
	if !legacy || !ok {
		return UpgradeTicket{}, ErrBadCredentials
	}
	target, err := u.target(email)
	if err != nil {
		return UpgradeTicket{}, err
	}
	out := UpgradeTicket{Target: target, SealID: sealID}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		var (
			standing, current string
			zk                int64
		)
		if err := tx.QueryRowContext(ctx, `SELECT status, password_hash, zk_enrolled_at FROM users WHERE id = ?`, id).
			Scan(&standing, &current, &zk); err != nil {
			return fmt.Errorf("auth: sign in: %w", err)
		}
		if standing != userActive || current != hash || zk != 0 {
			return ErrBadCredentials
		}
		var err error
		out.Ticket, err = issueTicketTx(ctx, tx, id, "", ticketEnrol, target, u.now())
		return err
	})
	if err != nil {
		return UpgradeTicket{}, err
	}
	return out, nil
}

// Enrol finishes the upgrade: with the ticket LegacySignIn answered, it
// stores the person's account key, verifiers and wraps under the ticket's
// target, sets zk_enrolled_at (one way), clears the old password hash, ends
// every session the person has and starts one whose step-up time is now, in
// one transaction. From then on the server refuses their password in clear.
func (u *Users) Enrol(ctx context.Context, ticket string, in Enrolment, userAgent string) (string, Session, User, error) {
	if err := in.check(); err != nil {
		return "", Session{}, User{}, err
	}
	ticketHash, ok := hashTicket(ticket)
	if !ok {
		return "", Session{}, User{}, ErrTicketInvalid
	}
	hashed, err := hashVerifiers(ctx, in.AuthKey, in.RecoveryProof)
	if err != nil {
		return "", Session{}, User{}, err
	}
	var (
		token   string
		session Session
		user    User
	)
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		t, err := consumeTicketTx(ctx, tx, ticketHash, now, "", "", ticketEnrol)
		if err != nil {
			return err
		}
		if t.target.KDF != in.KDF {
			return ErrKDFNotCurrent
		}
		n, err := execCount(ctx, tx, `UPDATE users SET public_key = ?, auth_verifier = ?, kdf_salt = ?, kdf_m = ?,
			kdf_t = ?, kdf_p = ?, password_wrap = ?, recovery_wrap = ?, recovery_verifier = ?, password_hash = '',
			zk_enrolled_at = ?, updated_at = ?
			WHERE id = ? AND status = ? AND zk_enrolled_at = 0 AND password_hash <> ''`,
			in.PublicKey, hashed.auth, t.target.Salt, t.target.KDF.M, t.target.KDF.T, t.target.KDF.P,
			in.PasswordWrap, in.RecoveryWrap, hashed.recovery, now.Unix(), now.Unix(), t.userID, userActive)
		if err != nil {
			return fmt.Errorf("auth: enrol: %w", err)
		}
		if n == 0 {
			return ErrTicketInvalid
		}
		if err := dropTicketsTx(ctx, tx, t.userID); err != nil {
			return err
		}
		if _, err := revokeSessionsTx(ctx, tx, t.userID, now.Unix()); err != nil {
			return err
		}
		token, session, err = startSessionTx(ctx, tx, t.userID, userAgent, now, SessionTTL, now)
		if err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, t.userID))
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// Reset is a reset invitation as the operator sees it. The code is not here:
// it exists once, in the link printed when it is made.
type Reset struct {
	UserID    string
	Email     string
	Forced    bool
	ExpiresAt time.Time
}

// CreateReset makes a reset invitation for a person and returns the only copy
// of its code (docs/key-scheme.md section 12.6): a single-use link that
// gives the person a new password, recovery code and account key, and
// deletes every grant sealed to their old key. A person's earlier reset
// invitation still waiting is replaced.
//
// Deleting a person's grants takes "read" from them, so without force it is
// refused, with a BlockedError naming the mailboxes, while the person is the
// last reader of a team mailbox, whether or not anyone else belongs to the
// team (resetBlocksTx). Completing it checks again, unless it was forced.
func (u *Users) CreateReset(ctx context.Context, userID string, force bool, createdBy string) (string, Reset, error) {
	raw := make([]byte, resetCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Reset{}, fmt.Errorf("auth: read random: %w", err)
	}
	sum := sha256.Sum256(raw)
	now := u.now().UTC().Truncate(time.Second)
	out := Reset{UserID: userID, Forced: force, ExpiresAt: now.Add(ResetTTL)}
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = ?`, userID).Scan(&out.Email)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrUserNotFound
		case err != nil:
			return fmt.Errorf("auth: create reset: %w", err)
		}
		if !force {
			if err := resetBlocksTx(ctx, tx, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reset_invites WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("auth: create reset: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO reset_invites(code_hash, user_id, forced, created_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`, sum[:], userID, force, createdBy, now.Unix(), out.ExpiresAt.Unix())
		if err != nil {
			return fmt.Errorf("auth: create reset: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", Reset{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), out, nil
}

// resetBlocksTx refuses a reset of a person who is the last reader of a team
// mailbox (BlockedError, with only the mailboxes). Not the test closing a
// person uses (workspace.BlocksTx), which leaves out a team whose only member
// is the person, since that team goes with them: a reset leaves the person
// and every team of theirs standing, and a team mailbox it took "read" from
// could then never be read again.
func resetBlocksTx(ctx context.Context, tx *sql.Tx, userID string) error {
	last, err := workspace.LastReaderOfTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if len(last) > 0 {
		return &BlockedError{Blocks: workspace.Blocks{LastReaderOf: last}}
	}
	return nil
}

// OpenReset checks a reset invitation for the address its link names and
// answers the person's seal id, and the target their new password is
// derived under: the
// address's salt and the server's default parameters, exactly what
// CompleteReset stores. Not what a challenge answers, which for an account
// off its target is the salt it stores now (docs/key-scheme.md section 5.3):
// a password derived under that, stored as the target, would never sign in.
// A link that is not valid is ErrResetInvalid, as CompleteReset's; one issued
// without force for a person who has become a team mailbox's last reader is
// a BlockedError here already, before anyone chooses a password. It changes
// nothing.
func (u *Users) OpenReset(ctx context.Context, code, email string) (ResetOpening, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return ResetOpening{}, err
	}
	codeHash, ok := hashResetCode(code)
	if !ok {
		return ResetOpening{}, ErrResetInvalid
	}
	tx, err := u.store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ResetOpening{}, fmt.Errorf("auth: check reset: %w", err)
	}
	//nolint:errcheck // a read-only transaction: nothing to keep or undo
	defer func() { _ = tx.Rollback() }()
	var (
		userID string
		forced bool
		out    ResetOpening
	)
	err = tx.QueryRowContext(ctx, `SELECT r.user_id, r.forced, p.seal_id FROM reset_invites r JOIN users p ON p.id = r.user_id
		WHERE r.code_hash = ? AND r.expires_at > ? AND p.email = ? AND p.status = ?`,
		codeHash, u.now().Unix(), email, userActive).Scan(&userID, &forced, &out.SealID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ResetOpening{}, ErrResetInvalid
	case err != nil:
		return ResetOpening{}, fmt.Errorf("auth: check reset: %w", err)
	}
	if !forced {
		if err := resetBlocksTx(ctx, tx, userID); err != nil {
			return ResetOpening{}, err
		}
	}
	if out.Target, err = u.target(email); err != nil {
		return ResetOpening{}, err
	}
	return out, nil
}

// ResetOpening is what opening a reset invitation answers: the target the
// new password is derived under, and the person's seal id, which never
// changes, to bind the new wraps to.
type ResetOpening struct {
	Target
	SealID string
}

// ResetLink is the address a reset invitation opens: the console's origin,
// and the code and the address in the fragment, as an invite's.
func ResetLink(publicURL, code, email string) string {
	return strings.TrimRight(publicURL, "/") + "/#reset=" + url.QueryEscape(code) + "&email=" + url.QueryEscape(email)
}

// CompleteReset redeems a reset invitation: the person's browser made a new
// account key, password and recovery code, derived under the target OpenReset
// answered (which this stores), and in one transaction the server
// replaces the public key (the one replacement of a key written once), the
// verifiers and the wraps, deletes every grant sealed to the old key and
// every platform wrap of the person, ends their sessions and every ticket,
// enrols them if they had not, clears any old password hash, and starts a
// session whose step-up time is now.
//
// The invitation is for the address it names, of an active person; anything
// else is ErrResetInvalid and spends nothing. One issued without force is
// refused, a BlockedError, while the person is the last reader of a team
// mailbox, and stays unspent.
func (u *Users) CompleteReset(ctx context.Context, code, email string, in Enrolment, userAgent string) (string, Session, User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	if err := in.check(); err != nil {
		return "", Session{}, User{}, err
	}
	codeHash, ok := hashResetCode(code)
	if !ok {
		return "", Session{}, User{}, ErrResetInvalid
	}
	// Checked before the hashes as well as inside the transaction: a bad
	// code should cost nothing to refuse.
	var pending int
	if err := u.store.Reader().QueryRowContext(ctx, `SELECT count(*) FROM reset_invites r JOIN users p ON p.id = r.user_id
		WHERE r.code_hash = ? AND r.expires_at > ? AND p.email = ? AND p.status = ?`,
		codeHash, u.now().Unix(), email, userActive).Scan(&pending); err != nil {
		return "", Session{}, User{}, fmt.Errorf("auth: check reset: %w", err)
	}
	if pending == 0 {
		return "", Session{}, User{}, ErrResetInvalid
	}
	hashed, err := hashVerifiers(ctx, in.AuthKey, in.RecoveryProof)
	if err != nil {
		return "", Session{}, User{}, err
	}
	target, err := u.target(email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	var (
		token   string
		session Session
		user    User
	)
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		var (
			userID, invited, status string
			forced                  bool
		)
		err := tx.QueryRowContext(ctx, `DELETE FROM reset_invites WHERE code_hash = ? AND expires_at > ?
			RETURNING user_id, forced`, codeHash, now.Unix()).Scan(&userID, &forced)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrResetInvalid
		case err != nil:
			return fmt.Errorf("auth: redeem reset: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT email, status FROM users WHERE id = ?`, userID).
			Scan(&invited, &status); err != nil {
			return fmt.Errorf("auth: redeem reset: %w", err)
		}
		// Returning rolls the DELETE back: the invitation stays its
		// person's.
		if !strings.EqualFold(invited, email) || status != userActive {
			return ErrResetInvalid
		}
		if !forced {
			if err := resetBlocksTx(ctx, tx, userID); err != nil {
				return err
			}
		}
		// Every grant sealed to the old key, and every platform wrap of the
		// person, go here, in this transaction: the tables that hold them
		// come with mailbox keys (migration 0014) and the hosted service.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET public_key = ?1, key_replaced_at = max(?2, key_replaced_at + 1),
			auth_verifier = ?3, kdf_salt = ?4, kdf_m = ?5, kdf_t = ?6, kdf_p = ?7, password_wrap = ?8, recovery_wrap = ?9,
			recovery_verifier = ?10, password_hash = '', password_changed_at = ?2, updated_at = ?2,
			zk_enrolled_at = CASE zk_enrolled_at WHEN 0 THEN ?2 ELSE zk_enrolled_at END
			WHERE id = ?11`,
			in.PublicKey, now.Unix(), hashed.auth, target.Salt, target.KDF.M, target.KDF.T, target.KDF.P,
			in.PasswordWrap, in.RecoveryWrap, hashed.recovery, userID,
		); err != nil {
			return fmt.Errorf("auth: reset: %w", err)
		}
		if err := dropTicketsTx(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reset_invites WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("auth: reset: %w", err)
		}
		if _, err := revokeSessionsTx(ctx, tx, userID, now.Unix()); err != nil {
			return err
		}
		token, session, err = startSessionTx(ctx, tx, userID, userAgent, now, SessionTTL, now)
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

// SweepTickets deletes the tickets and the reset invitations that have
// expired, and reports how many it deleted. Neither is a record of anything:
// an expired one is only a hash nobody can use.
func (u *Users) SweepTickets(ctx context.Context) (int, error) {
	now := u.now().Unix()
	var n int
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		tickets, err := execCount(ctx, tx, `DELETE FROM auth_tickets WHERE expires_at <= ?`, now)
		if err != nil {
			return fmt.Errorf("auth: sweep tickets: %w", err)
		}
		resets, err := execCount(ctx, tx, `DELETE FROM reset_invites WHERE expires_at <= ?`, now)
		if err != nil {
			return fmt.Errorf("auth: sweep reset invitations: %w", err)
		}
		n = tickets + resets
		return nil
	})
	return n, err
}

// ticketRow is a ticket as it is consumed.
type ticketRow struct {
	userID  string
	purpose string
	target  Target
}

// issueTicketTx makes a ticket inside the caller's transaction and returns
// the only copy of it. sessionID is "" for a ticket bound to no session.
func issueTicketTx(ctx context.Context, tx *sql.Tx, userID, sessionID, purpose string, target Target, now time.Time) (string, error) {
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	sum := sha256.Sum256(raw)
	var session any
	if sessionID != "" {
		session = sessionID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_tickets(hash, user_id, session_id, purpose, kdf_salt, kdf_m, kdf_t,
		kdf_p, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sum[:], userID, session, purpose, target.Salt, target.KDF.M, target.KDF.T, target.KDF.P,
		now.Unix(), now.Add(TicketTTL).Unix()); err != nil {
		return "", fmt.Errorf("auth: issue ticket: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// consumeTicketTx uses a ticket once, inside the caller's transaction: one of
// purposes, not expired, and, when userID and sessionID are not empty, theirs.
// A ticket that is anything else is ErrTicketInvalid, and stays unused.
func consumeTicketTx(ctx context.Context, tx *sql.Tx, hash []byte, now time.Time, userID, sessionID string,
	purposes ...string,
) (ticketRow, error) {
	var (
		t       ticketRow
		session sql.NullString
	)
	err := tx.QueryRowContext(ctx, `DELETE FROM auth_tickets WHERE hash = ? AND expires_at > ?
		RETURNING user_id, coalesce(session_id, ''), purpose, kdf_salt, kdf_m, kdf_t, kdf_p`, hash, now.Unix(),
	).Scan(&t.userID, &session, &t.purpose, &t.target.Salt, &t.target.KDF.M, &t.target.KDF.T, &t.target.KDF.P)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ticketRow{}, ErrTicketInvalid
	case err != nil:
		return ticketRow{}, fmt.Errorf("auth: use ticket: %w", err)
	}
	// Returning rolls the DELETE back: a ticket presented by somebody else,
	// or to the wrong ceremony, is still its own.
	known := false
	for _, p := range purposes {
		known = known || p == t.purpose
	}
	if !known || (userID != "" && t.userID != userID) || session.String != sessionID {
		return ticketRow{}, ErrTicketInvalid
	}
	return t, nil
}

// dropTicketsTx deletes every ticket of a person: a ceremony that changes
// their secrets makes every other one moot.
func dropTicketsTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tickets WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("auth: drop tickets: %w", err)
	}
	return nil
}

// asRefusal turns the refusal of requireEnrolledTx (a person gone, disabled
// or not enrolled) into refusal, and returns any other error as it is.
func asRefusal(err, refusal error) error {
	if errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrUserNotFound) {
		return refusal
	}
	return err
}

// requireEnrolledTx refuses a person who is not active and enrolled.
func requireEnrolledTx(ctx context.Context, tx *sql.Tx, userID string) error {
	var (
		status   string
		enrolled int64
	)
	err := tx.QueryRowContext(ctx, `SELECT status, zk_enrolled_at FROM users WHERE id = ?`, userID).Scan(&status, &enrolled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrUserNotFound
	case err != nil:
		return fmt.Errorf("auth: read the person: %w", err)
	case status != userActive || enrolled == 0:
		return ErrBadCredentials
	}
	return nil
}

// The verifiers requireVerifierTx compares.
const (
	authVerifier     = "auth_verifier"
	recoveryVerifier = "recovery_verifier"
)

// requireVerifierTx refuses, with ErrBadCredentials, a person who is not
// active and enrolled, or whose verifier (authVerifier or recoveryVerifier)
// is no longer the one a ceremony just checked a secret against: what a
// ceremony reads again under the write lock, since the person may have been
// disabled, or that secret replaced, while its hash ran.
func requireVerifierTx(ctx context.Context, tx *sql.Tx, userID, which, checked string) error {
	var (
		status, authV, recoveryV string
		enrolled                 int64
	)
	err := tx.QueryRowContext(ctx, `SELECT status, zk_enrolled_at, auth_verifier, recovery_verifier FROM users WHERE id = ?`,
		userID).Scan(&status, &enrolled, &authV, &recoveryV)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrUserNotFound
	case err != nil:
		return fmt.Errorf("auth: read the person: %w", err)
	case status != userActive || enrolled == 0:
		return ErrBadCredentials
	}
	current := authV
	if which == recoveryVerifier {
		current = recoveryV
	}
	if checked == "" || current != checked {
		return ErrBadCredentials
	}
	return nil
}

// requireLiveSessionTx refuses a session that has ended or is not userID's.
func requireLiveSessionTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, now time.Time) error {
	var (
		owner, status    string
		expires, revoked int64
	)
	err := tx.QueryRowContext(ctx, `SELECT s.user_id, s.expires_at, s.revoked_at, u.status
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, sessionID,
	).Scan(&owner, &expires, &revoked, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInvalidSession
	case err != nil:
		return fmt.Errorf("auth: read the session: %w", err)
	case owner != userID || !sessionLive(now, expires, revoked, status):
		return ErrInvalidSession
	}
	return nil
}

// hashTicket decodes a presented ticket and returns what the database stores
// for it.
func hashTicket(ticket string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(ticket))
	if err != nil || len(raw) != ticketBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// hashResetCode decodes a presented reset code and returns what the database
// stores for it.
func hashResetCode(code string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil || len(raw) != resetCodeBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}
