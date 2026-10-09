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

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// An invite is a one-time right to create an account for one address.
//
// The code is 32 random bytes and only its SHA-256 is stored, like a session
// token. It reaches the person inside a link whose fragment carries it —
// `#invite=…&email=…` — because a fragment is never sent to a server, so the
// code does not land in an access log, a proxy or a Referer header on the way.

// InviteTTL is how long an invite stays redeemable.
const InviteTTL = 7 * 24 * time.Hour

const inviteCodeBytes = 32

// ErrInviteInvalid is one answer for a code that does not exist, has expired,
// was already used, or is for another address. They are one error because
// the person holding the link can do the same thing about each: ask for a new
// one.
var ErrInviteInvalid = errors.New("auth: the invite is not valid, has been used, or has expired")

// ErrInviteNotFound is an invite id with no pending invite behind it, in the
// workspace named.
var ErrInviteNotFound = errors.New("auth: no such pending invite")

// ErrInviteJoinsOnly is a team invite offered at sign-up whose creator may not
// bring people onto the server: it adds someone who already has an account
// here to a team, accepted signed in, and creates no account. Given whether
// or not the address has an account, so a link does not tell its holder which
// addresses do.
var ErrInviteJoinsOnly = errors.New("auth: this invite adds an existing account to a team and creates none")

// ErrSealIDNotOpened is a sign-up whose wraps are bound to a seal id other
// than the one opening its invitation answered (OpenSignUp), or that never
// opened it: the person would get a seal id their wraps do not open under.
var ErrSealIDNotOpened = errors.New("auth: bind the account key to the seal id the invitation answers")

// Invite is an invite as its creator sees it. The code is not here: it exists
// once, in the link returned when the invite is made.
//
// An invite is one of two kinds. An instance invite (no WorkspaceID) makes a
// new person, with Role as their role on the instance. A team invite names a
// team and WorkspaceRole, the role in it: a person who already has an account
// accepts it signed in (AcceptInvite); a new person signs up with it, as an
// instance member, only when whoever made it may bring people onto the server
// (an instance owner, or the operator; see SignUp). Either way the person
// joins the team, and no mailbox: accepting never grants access to one.
type Invite struct {
	ID            string
	Email         string
	Role          Role
	WorkspaceID   string
	WorkspaceRole workspace.Role
	CreatedBy     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// NewInvite describes an invite to make.
type NewInvite struct {
	Email string
	// Role is the instance role of an instance invite; a team invite's is
	// always member.
	Role Role
	// WorkspaceID makes a team invite, into this team, with WorkspaceRole.
	WorkspaceID   string
	WorkspaceRole workspace.Role
	// CreatedBy is "usr_…", "key:<prefix>" or "cli", for the audit trail.
	CreatedBy string
	// Check runs first in the transaction that stores the invite: where the
	// service re-reads that the caller may still invite. nil checks nothing.
	Check func(*sql.Tx) error
}

// CreateInvite makes an invite and returns the only copy of its code.
//
// The invite keeps the role it was made with, and that role is what its
// person gets: nothing about signing up first or last changes it. Who may
// invite whom, as what, is the service's to decide.
//
// An instance invite is for an address with no account. A team invite is for
// any address that is not already a member of the team, active or not, into a
// team whose workspaces may be changed here.
func (u *Users) CreateInvite(ctx context.Context, in NewInvite) (string, Invite, error) {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return "", Invite{}, err
	}
	role := in.Role
	if in.WorkspaceID != "" {
		if _, err := workspace.ParseRole(string(in.WorkspaceRole)); err != nil {
			return "", Invite{}, ErrInvalidWorkspaceRole
		}
		// A new person a team brings onto the server is an instance member.
		role = RoleMember
	} else if in.WorkspaceRole != "" {
		return "", Invite{}, ErrInvalidWorkspaceRole
	}
	if _, err := ParseRole(string(role)); err != nil {
		return "", Invite{}, err
	}
	id, err := newID("inv_")
	if err != nil {
		return "", Invite{}, err
	}
	raw := make([]byte, inviteCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Invite{}, fmt.Errorf("auth: read random: %w", err)
	}
	sum := sha256.Sum256(raw)
	now := u.now().UTC().Truncate(time.Second)
	out := Invite{
		ID: id, Email: email, Role: role, WorkspaceID: in.WorkspaceID, WorkspaceRole: in.WorkspaceRole,
		CreatedBy: in.CreatedBy, CreatedAt: now, ExpiresAt: now.Add(InviteTTL),
	}

	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		if in.Check != nil {
			if err := in.Check(tx); err != nil {
				return err
			}
		}
		if in.WorkspaceID == "" {
			var taken int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM users WHERE email = ?`, email).Scan(&taken); err != nil {
				return fmt.Errorf("auth: create invite: %w", err)
			}
			if taken > 0 {
				return ErrEmailTaken
			}
		} else if err := u.teamInvitableTx(ctx, tx, in.WorkspaceID, email); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO invites(id, code_hash, email, role, workspace_id, workspace_role, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			out.ID, sum[:], out.Email, string(out.Role), nullable(out.WorkspaceID), string(out.WorkspaceRole),
			out.CreatedBy, now.Unix(), out.ExpiresAt.Unix())
		if err != nil {
			return fmt.Errorf("auth: create invite: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", Invite{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), out, nil
}

// teamInvitableTx refuses a team invite into anything but a team whose
// memberships may change here, or for an address already a member of it.
func (u *Users) teamInvitableTx(ctx context.Context, tx *sql.Tx, workspaceID, email string) error {
	w, err := workspace.GetTx(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	switch {
	case w.Kind == workspace.KindPersonal:
		return workspace.ErrPersonal
	case w.Kind == workspace.KindOperator:
		return workspace.ErrOperator
	case w.Source != workspace.SourceLocal:
		return workspace.ErrManagedElsewhere
	}
	if err := u.source.Changeable(); err != nil {
		return err
	}
	var member int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_members m JOIN users p ON p.id = m.user_id
		WHERE m.workspace_id = ? AND p.email = ?`, workspaceID, email).Scan(&member); err != nil {
		return fmt.Errorf("auth: create invite: %w", err)
	}
	if member > 0 {
		return workspace.ErrAlreadyMember
	}
	return nil
}

// PendingInvites lists the invites not used and not expired, newest first:
// a team's with its id, the instance's with an empty one.
func (u *Users) PendingInvites(ctx context.Context, workspaceID string) ([]Invite, error) {
	rows, err := u.store.Reader().QueryContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE coalesce(workspace_id, '') = ? AND used_at = 0 AND expires_at > ?
		ORDER BY created_at DESC, id`, workspaceID, u.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("auth: list invites: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list invites: %w", err)
	}
	return out, nil
}

// Invite reads one pending invite of a workspace ("" for the instance's).
func (u *Users) Invite(ctx context.Context, workspaceID, id string) (Invite, error) {
	inv, err := scanInvite(u.store.Reader().QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE id = ? AND coalesce(workspace_id, '') = ? AND used_at = 0 AND expires_at > ?`,
		id, workspaceID, u.now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrInviteNotFound
	}
	return inv, err
}

// RevokeInvite deletes a pending invite of a workspace ("" for the
// instance's), so its link no longer works. A used invite stays: it is the
// record of how its person arrived.
//
// check, when not nil, runs first in the same transaction with the invite
// about to go: where the service re-reads that the caller may still revoke
// it, which can depend on the role it names.
func (u *Users) RevokeInvite(ctx context.Context, workspaceID, id string, check func(*sql.Tx, Invite) error) error {
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites
			WHERE id = ? AND coalesce(workspace_id, '') = ? AND used_at = 0`, id, workspaceID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrInviteNotFound
		case err != nil:
			return err
		}
		if check != nil {
			if err := check(tx, inv); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM invites
			WHERE id = ? AND coalesce(workspace_id, '') = ? AND used_at = 0`, id, workspaceID)
		if err != nil {
			return fmt.Errorf("auth: revoke invite: %w", err)
		}
		return requireRow(res, ErrInviteNotFound)
	})
}

// NeedsFirstOwner reports whether the instance has nobody to administer it
// and nobody on the way: no active owner, and no instance invite for an owner
// still waiting to be used. The command line's bootstrap invite is an owner's
// then, and a member's otherwise.
func (u *Users) NeedsFirstOwner(ctx context.Context) (bool, error) {
	var n int
	err := u.store.Reader().QueryRowContext(ctx, `SELECT
		  (SELECT count(*) FROM users WHERE role = 'owner' AND status = 'active')
		+ (SELECT count(*) FROM invites WHERE workspace_id IS NULL AND role = 'owner' AND used_at = 0 AND expires_at > ?)`,
		u.now().Unix()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("auth: look for an owner: %w", err)
	}
	return n == 0, nil
}

const inviteColumns = `id, email, role, coalesce(workspace_id, ''), workspace_role, created_by, created_at, expires_at`

func scanInvite(row interface{ Scan(...any) error }) (Invite, error) {
	var inv Invite
	err := row.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.WorkspaceID, &inv.WorkspaceRole, &inv.CreatedBy,
		unixScanner{&inv.CreatedAt}, unixScanner{&inv.ExpiresAt})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Invite{}, err
		}
		return Invite{}, fmt.Errorf("auth: scan invite: %w", err)
	}
	return inv, nil
}

// InviteLink is the sign-up address for an invite: the console's origin, and
// the code and address in the fragment.
func InviteLink(publicURL, code, email string) string {
	return strings.TrimRight(publicURL, "/") + "/#invite=" + url.QueryEscape(code) + "&email=" + url.QueryEscape(email)
}

// SignUpRequest is what the sign-up form sends: the person's browser made
// their account key and derived their auth key (docs/key-scheme.md section
// 12.1); the password never comes.
type SignUpRequest struct {
	Invite string
	Email  string
	Name   string
	// SealID is the seal id the browser bound the wraps to: the one
	// OpenSignUp answered for this invitation, which the person gets.
	SealID    string
	Enrolment Enrolment
	UserAgent string
}

// SignUpOpening is what opening an invitation answers: the target the new
// password is derived under, and the seal id of the person it signs up.
type SignUpOpening struct {
	Target
	SealID string
}

// OpenSignUp checks an invitation for the address its link names before its
// person chooses a password, as SignUp will (ErrInviteInvalid,
// ErrInviteJoinsOnly, ErrEmailTaken), and answers what their browser binds
// and derives under (docs/key-scheme.md section 12.1): the address's target,
// and the seal id the server draws for the person the invitation will
// create, once, kept with the invitation, so that the wraps the browser
// sends with the sign-up are bound to the seal id the person gets. Opening
// it again answers the same seal id. Nothing else changes.
func (u *Users) OpenSignUp(ctx context.Context, code, email string) (SignUpOpening, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return SignUpOpening{}, err
	}
	codeHash, ok := hashInviteCode(code)
	if !ok {
		return SignUpOpening{}, ErrInviteInvalid
	}
	if err := u.checkSignUp(ctx, codeHash, email); err != nil {
		return SignUpOpening{}, err
	}
	target, err := u.target(email)
	if err != nil {
		return SignUpOpening{}, err
	}
	out := SignUpOpening{Target: target}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now().Unix()
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET seal_id = ?
			  WHERE code_hash = ? AND used_at = 0 AND expires_at > ? AND seal_id = ''`,
			keyscheme.NewSealID(), codeHash, now); err != nil {
			return fmt.Errorf("auth: open invite: %w", err)
		}
		err := tx.QueryRowContext(ctx, `SELECT seal_id FROM invites WHERE code_hash = ? AND used_at = 0 AND expires_at > ?`,
			codeHash, now).Scan(&out.SealID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return fmt.Errorf("auth: open invite: %w", err)
		}
		return nil
	})
	if err != nil {
		return SignUpOpening{}, err
	}
	return out, nil
}

// SignUp redeems an invite: it creates the account, enrolled in the key
// scheme with what the browser sent and the address's target salt, with the
// personal workspace the workspace source makes for it, joins the invite's
// team for a team invite, and starts its first session, whose step-up time is
// now, in one transaction with marking the invite used, so a crash cannot
// leave an invite spent with nobody behind it or an account behind an invite
// that still works.
//
// The person gets the role their invite names. The first person to sign up
// on a server is an owner only when invited as one.
//
// A team invite signs a new person up only when its creator may bring people
// onto the server, as they stand now: the operator ("cli", or an instance
// key), or an instance owner still active. Any person may create a team and
// invite into it; were that enough to create an account, any member could
// mint one for any address, with a password of their choosing, and the
// sign-up would spend the instance invites waiting for that address. Any
// other team invite is ErrInviteJoinsOnly here, whether the address has an
// account or not, and stays unspent for its person to accept signed in.
//
// The person's seal id is the one opening the invitation drew (OpenSignUp),
// which the browser bound the wraps to and names in SealID; anything else is
// ErrSealIDNotOpened, and spends nothing. The answer's User carries it and
// the public key, for the browser to keep the account key under
// (docs/key-scheme.md section 7).
func (u *Users) SignUp(ctx context.Context, req SignUpRequest) (string, Session, User, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	name, err := NormalizeName(req.Name)
	if err != nil {
		return "", Session{}, User{}, err
	}
	if err := req.Enrolment.check(); err != nil {
		return "", Session{}, User{}, err
	}
	codeHash, ok := hashInviteCode(req.Invite)
	if !ok {
		return "", Session{}, User{}, ErrInviteInvalid
	}

	// Checked before the hashes as well as inside the transaction: a bad
	// code or a taken address should cost no Argon2id to refuse.
	if err := u.checkSignUp(ctx, codeHash, email); err != nil {
		return "", Session{}, User{}, err
	}
	// After the invitation's own refusals, which say more: a browser that
	// could not open it has no seal id to send.
	if !keyscheme.ValidSealID(req.SealID) {
		return "", Session{}, User{}, ErrSealIDNotOpened
	}
	hashed, err := hashVerifiers(ctx, req.Enrolment.AuthKey, req.Enrolment.RecoveryProof)
	if err != nil {
		return "", Session{}, User{}, err
	}
	target, err := u.target(email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	userID, err := newID("usr_")
	if err != nil {
		return "", Session{}, User{}, err
	}

	now := u.now().UTC().Truncate(time.Second)
	user := User{
		ID: userID, Email: email, Name: name, HasPassword: true, Enrolled: true, SealID: req.SealID,
		PublicKey: req.Enrolment.PublicKey, PasswordChangedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	in := req.Enrolment
	var token string
	var session Session
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		var (
			invited, team, createdBy, sealID string
			teamRole                         workspace.Role
		)
		err := tx.QueryRowContext(ctx,
			`UPDATE invites SET used_at = ?, used_by = ?
			  WHERE code_hash = ? AND used_at = 0 AND expires_at > ?
			  RETURNING email, role, coalesce(workspace_id, ''), workspace_role, created_by, seal_id`,
			now.Unix(), userID, codeHash, now.Unix()).Scan(&invited, &user.Role, &team, &teamRole, &createdBy, &sealID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return fmt.Errorf("auth: redeem invite: %w", err)
		}
		// Returning here rolls the UPDATE back, so a mistyped address does
		// not spend the invite of the person it was really for.
		if !strings.EqualFold(invited, email) {
			return ErrInviteInvalid
		}
		if sealID != req.SealID {
			return ErrSealIDNotOpened
		}
		// Read again here, in the transaction that counts: an owner demoted
		// or disabled since the pre-check no longer vouches for anyone.
		if team != "" {
			if err := signsUpTx(ctx, tx, createdBy); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at,
			                   seal_id, public_key, auth_verifier, kdf_salt, kdf_m, kdf_t, kdf_p, password_wrap,
			                   recovery_wrap, recovery_verifier, zk_enrolled_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user.ID, user.Email, user.Name, string(user.Role), userActive, now.Unix(), now.Unix(), now.Unix(),
			user.SealID, in.PublicKey, hashed.auth, target.Salt, target.KDF.M, target.KDF.T, target.KDF.P,
			in.PasswordWrap, in.RecoveryWrap, hashed.recovery, now.Unix())
		if store.IsUnique(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("auth: create user: %w", err)
		}
		if err := u.source.PersonCreatedTx(ctx, tx, user.ID, now); err != nil {
			return err
		}
		if team != "" {
			if err := u.workspaces.AddMemberTx(ctx, tx, team, user.ID, teamRole, now); err != nil {
				return err
			}
		}
		// The address has an account now: every other invite still waiting
		// for it to sign up, and every other invite to the same team, is
		// spent with this one.
		if err := dropOtherInvitesTx(ctx, tx, email, team, true); err != nil {
			return err
		}
		token, session, err = startSessionTx(ctx, tx, user.ID, req.UserAgent, now, SessionTTL, now)
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// checkSignUp is the read-only half of SignUp's checks, run before the hash.
//
// Whether a team invite may sign anyone up is asked before whether the
// address has an account, so that the answer to a team invite its creator
// may not use this way never depends on the address.
func (u *Users) checkSignUp(ctx context.Context, codeHash []byte, email string) error {
	var invited, team, createdBy string
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT email, coalesce(workspace_id, ''), created_by FROM invites
		  WHERE code_hash = ? AND used_at = 0 AND expires_at > ?`,
		codeHash, u.now().Unix()).Scan(&invited, &team, &createdBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInviteInvalid
	case err != nil:
		return fmt.Errorf("auth: check invite: %w", err)
	}
	if !strings.EqualFold(invited, email) {
		return ErrInviteInvalid
	}
	if team != "" {
		if err := signsUpTx(ctx, u.store.Reader(), createdBy); err != nil {
			return err
		}
	}
	var taken int
	if err := u.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE email = ?`, email).Scan(&taken); err != nil {
		return fmt.Errorf("auth: check address: %w", err)
	}
	if taken > 0 {
		return ErrEmailTaken
	}
	return nil
}

// AcceptInvite redeems a team invite for a person who already has an account
// and is signed in: they join the team with the invite's role, and the invite
// is spent, in one transaction. The invite must be for their address.
//
// A refusal spends nothing: an instance invite (ErrInviteInvalid, which signs
// a new person up instead), another address's invite, an expired or used one,
// or a person already a member of that team (workspace.ErrAlreadyMember).
// Accepting spends the team's other invites for the same address too.
// Joining a team never grants access to a mailbox.
func (u *Users) AcceptInvite(ctx context.Context, userID, code string) (workspace.Workspace, error) {
	codeHash, ok := hashInviteCode(code)
	if !ok {
		return workspace.Workspace{}, ErrInviteInvalid
	}
	now := u.now().UTC().Truncate(time.Second)
	var out workspace.Workspace
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		var email, status string
		err := tx.QueryRowContext(ctx, `SELECT email, status FROM users WHERE id = ?`, userID).Scan(&email, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrUserNotFound
		case err != nil:
			return fmt.Errorf("auth: accept invite: %w", err)
		case status != userActive:
			return ErrUserDisabled
		}
		var (
			invited, team string
			role          workspace.Role
		)
		err = tx.QueryRowContext(ctx,
			`UPDATE invites SET used_at = ?, used_by = ?
			  WHERE code_hash = ? AND used_at = 0 AND expires_at > ? AND workspace_id IS NOT NULL
			  RETURNING email, workspace_id, workspace_role`,
			now.Unix(), userID, codeHash, now.Unix()).Scan(&invited, &team, &role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return fmt.Errorf("auth: accept invite: %w", err)
		}
		// Returning rolls the UPDATE back: the invite is still the person's
		// it was made for.
		if !strings.EqualFold(invited, email) {
			return ErrInviteInvalid
		}
		if err := u.workspaces.AddMemberTx(ctx, tx, team, userID, role, now); err != nil {
			return err
		}
		// Another invite to the same team for the same address is spent
		// with this one: left waiting, it would let the person back in,
		// with whatever role it names, after they were removed.
		if err := dropOtherInvitesTx(ctx, tx, email, team, false); err != nil {
			return err
		}
		out, err = workspace.GetTx(ctx, tx, team)
		if err != nil {
			return err
		}
		out.Role, out.Status = role, workspace.StatusActive
		return nil
	})
	if err != nil {
		return workspace.Workspace{}, err
	}
	return out, nil
}

// dropOtherInvitesTx deletes, in the transaction that redeems an invite for
// email, the other unused invites for that address that this one makes moot:
// those to the same team (team, when there is one) and, when the address has
// just signed up (signedUp), the instance's, which can never sign it up again
// and would otherwise still count as an owner on the way (NeedsFirstOwner).
// A sign-up through a team invite spends them too: only an instance owner's
// or the operator's team invite signs anyone up (signsUpTx), the same people
// who make instance invites. An invite to another team stays: the person may
// still accept it.
func dropOtherInvitesTx(ctx context.Context, tx *sql.Tx, email, team string, signedUp bool) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM invites
		WHERE email = ?1 AND used_at = 0 AND ((?2 <> '' AND workspace_id = ?2) OR (?3 AND workspace_id IS NULL))`,
		email, team, signedUp); err != nil {
		return fmt.Errorf("auth: spend the address's other invites: %w", err)
	}
	return nil
}

// rowQuerier is a transaction or the reader pool: what signsUpTx reads with.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// signsUpTx refuses, with ErrInviteJoinsOnly, a team invite whose creator may
// not bring a new person onto the server: only the operator ("cli", or an
// instance key's "key:<prefix>": a key of the operator workspace) and an instance owner who is still active
// may. A creator who was deleted is "" by then, and vouches for nobody.
func signsUpTx(ctx context.Context, q rowQuerier, createdBy string) error {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT ?1 = 'cli'
		OR EXISTS (SELECT 1 FROM api_keys WHERE 'key:' || prefix = ?1 AND workspace_id = ?2)
		OR EXISTS (SELECT 1 FROM users WHERE id = ?1 AND role = 'owner' AND status = 'active')`,
		createdBy, workspace.OperatorID).Scan(&ok)
	switch {
	case err != nil:
		return fmt.Errorf("auth: check who made the invite: %w", err)
	case !ok:
		return ErrInviteJoinsOnly
	}
	return nil
}

func hashInviteCode(code string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil || len(raw) != inviteCodeBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}
