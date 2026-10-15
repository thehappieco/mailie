package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/store"
)

// Mirroring an external directory (docs/workspaces.md, "Following an
// external directory").
//
// Under a workspace source that is not the local one (Platform), workspaces
// and memberships are not created or changed here, from the console or the
// command line (ErrManagedElsewhere): an extension of the daemon follows an
// external directory, and the service applies what its documents say of a
// person in one write, through these steps. A document names the person's
// personal workspace and the teams they belong to, each by the directory's
// own id (ValidExternalID), which never looks like one of this server's, and
// never changes its kind, its source or, for a personal workspace, its
// person (ErrKindChanged, ErrRepointed).
//
// What a directory says applies without the protections a local change
// keeps: a membership it ends takes no refusal for the last owner, the last
// reader, a closed team or a person who is not active. Ending one may leave
// a team mailbox with nobody who reads it (stranded, StrandCandidatesTx),
// which the journal records; ended while the directory's generation is not
// confirmed, such a membership is held instead (EndMembershipTx), disabled
// with its flags and sealed grants, until a document lists it again or the
// generation is confirmed.

// Errors of the mirror.
var (
	// ErrInvalidExternalID is a workspace id a directory sent that is not 1
	// to 64 of letters, digits, '_', '.' and '-', or that looks like one of
	// this server's ("wsp_…").
	ErrInvalidExternalID = errors.New("workspace: a directory's workspace id is 1 to 64 of [A-Za-z0-9_.-], never wsp_…")
	// ErrKindChanged is a directory's workspace sent with another kind than
	// the one it was mirrored with: a kind never changes (migration 0016).
	ErrKindChanged = errors.New("workspace: a directory's workspace never changes its kind")
	// ErrRepointed is a directory's workspace id that names a workspace of
	// this server's own, or a personal workspace of another person: a
	// workspace's source and person never change (migration 0016).
	ErrRepointed = errors.New("workspace: a directory's workspace never changes its source or its person")
	// ErrNotMirrored is a membership asked of a workspace that is not a team
	// from another source: only those are mirrored.
	ErrNotMirrored = errors.New("workspace: only a team from another source has its memberships mirrored")
)

// maxExternalID bounds a directory's workspace id.
const maxExternalID = 64

// ValidExternalID reports whether id may name a workspace a directory sends:
// 1 to 64 of A-Z, a-z, 0-9, '_', '.' and '-', and never "wsp_…", which this
// server's own ids are, so that no directory's id is ever taken for one of
// them.
func ValidExternalID(id string) bool {
	if id == "" || len(id) > maxExternalID || strings.HasPrefix(id, "wsp_") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// MirrorName makes a team's name, as a directory sends it, into one this
// server keeps rather than refusing it: bytes that are not UTF-8 and control
// characters become spaces, a run of spaces one, and a name longer than
// MaxNameLength is cut at a character. ErrInvalidName when nothing is left.
func MirrorName(s string) (string, error) {
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
	if s == "" || !utf8.ValidString(s) {
		return "", ErrInvalidName
	}
	return s, nil
}

// Mirrored is a workspace as a directory's document names it.
type Mirrored struct {
	// ID is the directory's id for it (ValidExternalID).
	ID string
	// Kind is KindPersonal or KindTeam, and never changes.
	Kind Kind
	// PersonID is the person of a personal workspace: the document's.
	PersonID string
	// Name is a team's, as MirrorName keeps it.
	Name string
	// Closed is a team the directory closed.
	Closed bool
	// Version is the directory's version of the team's name and closure: a
	// team takes them only from a higher one than it last took
	// (workspaces.source_version), and from any after a reset.
	Version int64
}

// CheckMirroredTx checks, inside the caller's transaction and writing
// nothing, that a directory's workspace may be mirrored as d says: its id is
// one (ErrInvalidExternalID), and a workspace already there with that id has
// d's kind (ErrKindChanged), comes from another source and, personal, is d's
// person's (ErrRepointed). A workspace not there yet passes.
func CheckMirroredTx(ctx context.Context, tx *sql.Tx, d Mirrored) error {
	if !ValidExternalID(d.ID) {
		return ErrInvalidExternalID
	}
	if d.Kind != KindPersonal && d.Kind != KindTeam {
		return ErrKindChanged
	}
	w, err := GetTx(ctx, tx, d.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case w.Kind != d.Kind:
		return ErrKindChanged
	case w.Source == SourceLocal:
		return ErrRepointed
	case d.Kind == KindPersonal && w.PersonID != d.PersonID:
		return ErrRepointed
	}
	return nil
}

// sourceResetSQL is the sum of every directory's resets, what a workspace
// from another source is written at (migration 0022).
const sourceResetSQL = `(SELECT coalesce(sum(d.reset), 0) FROM directories d)`

// MirrorWorkspaceTx creates or updates a team a directory sends, inside the
// caller's transaction, and reports whether it created it, whether its name
// or closure changed, and, when its closure did, its mailboxes, whose use and
// sync that stopped or gave back (SetClosedTx). A new team takes the
// directory's id, the source the directory mirrors, its name, its closure
// and its version. One already there takes the name and the closure only
// from a higher version than it last took, so an older document never undoes
// a newer one; a reset of the directory sets every version back to 0
// (store.ResetDirectoryTx), and the next document's state is taken whatever
// its version. Its memberships are MirrorMemberTx's. A personal workspace is
// ReplacePersonalTx's (ErrPersonal here).
func MirrorWorkspaceTx(ctx context.Context, tx *sql.Tx, d Mirrored, now time.Time) (created, changed bool, accounts []string, err error) {
	if d.Kind != KindTeam {
		return false, false, nil, ErrPersonal
	}
	if err := CheckMirroredTx(ctx, tx, d); err != nil {
		return false, false, nil, err
	}
	name, err := MirrorName(d.Name)
	if err != nil {
		return false, false, nil, err
	}
	if d.Version < 0 {
		return false, false, nil, fmt.Errorf("workspace: a directory's version is never negative")
	}
	at := now.UTC().Unix()
	w, err := GetTx(ctx, tx, d.ID)
	if errors.Is(err, ErrNotFound) {
		closedAt := int64(0)
		if d.Closed {
			closedAt = at
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at,
			  closed_at, source_version, source_reset)
			VALUES (?, 'team', ?, ?, ?, ?, ?, ?, `+sourceResetSQL+`)`,
			d.ID, SourcePlatform, name, at, at, closedAt, d.Version); err != nil {
			return false, false, nil, fmt.Errorf("workspace: mirror the team: %w", err)
		}
		return true, true, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT source_version FROM workspaces WHERE id = ?`, d.ID).Scan(&version); err != nil {
		return false, false, nil, fmt.Errorf("workspace: read the team's version: %w", err)
	}
	if d.Version <= version {
		return false, false, nil, nil
	}
	if name != w.Name {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET name = ?, updated_at = ? WHERE id = ?`,
			name, at, d.ID); err != nil {
			return false, false, nil, fmt.Errorf("workspace: rename the team: %w", err)
		}
		changed = true
	}
	closure, closed, err := SetClosedTx(ctx, tx, d.ID, d.Closed, now)
	if err != nil {
		return false, false, nil, err
	}
	if closure {
		changed, accounts = true, closed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET source_version = ? WHERE id = ?`, d.Version, d.ID); err != nil {
		return false, false, nil, fmt.Errorf("workspace: record the team's version: %w", err)
	}
	return false, changed, accounts, nil
}

// ReplacePersonalTx gives a person, inside the caller's transaction, the
// personal workspace a directory names for them, newID. A person who has it
// already changes nothing. One who has none (a person a directory's source
// created, which creates no workspace of its own) gets it. One whose personal
// workspace is another — the local one signing up made, or one an earlier
// document named — has it replaced only while it holds no mailbox, live or
// removed, and no live key: it is deleted, with its stopped keys and its
// membership, and newID is created, the person its only member, an active
// owner, at the directories' reset. Otherwise nothing changes and pending is
// true: the old workspace stays theirs until what it holds is gone, and a
// later document replaces it then. It returns the id it replaced, whether
// the replacement waits, and how many stopped keys went with the old one.
func ReplacePersonalTx(ctx context.Context, tx *sql.Tx, userID, newID string, now time.Time) (replaced string, pending bool, stopped int, err error) {
	if err := CheckMirroredTx(ctx, tx, Mirrored{ID: newID, Kind: KindPersonal, PersonID: userID}); err != nil {
		return "", false, 0, err
	}
	at := now.UTC().Unix()
	cur, err := PersonalOfTx(ctx, tx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return "", false, 0, err
	case cur.ID == newID:
		return "", false, 0, nil
	default:
		var mailboxes, live, keys int
		if err := tx.QueryRowContext(ctx, `SELECT
			  (SELECT count(*) FROM accounts a WHERE a.workspace_id = ?1),
			  (SELECT count(*) FROM api_keys k WHERE k.workspace_id = ?1 AND k.revoked_at = 0
			     AND (k.expires_at = 0 OR k.expires_at > ?2)),
			  (SELECT count(*) FROM api_keys k WHERE k.workspace_id = ?1)`, cur.ID, at).
			Scan(&mailboxes, &live, &keys); err != nil {
			return "", false, 0, fmt.Errorf("workspace: read what the personal workspace holds: %w", err)
		}
		if mailboxes > 0 || live > 0 {
			return "", true, 0, nil
		}
		// Its membership and its keys, all stopped, go with it (ON DELETE
		// CASCADE); a personal workspace has no invites.
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, cur.ID); err != nil {
			if store.IsForeignKey(err) {
				return "", true, 0, nil
			}
			return "", false, 0, fmt.Errorf("workspace: delete the personal workspace replaced: %w", err)
		}
		replaced, stopped = cur.ID, keys
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id, kind, source, name, person_id, created_at, updated_at,
		  source_reset)
		VALUES (?, 'personal', ?, '', ?, ?, ?, `+sourceResetSQL+`)`, newID, SourcePlatform, userID, at, at); err != nil {
		return "", false, 0, fmt.Errorf("workspace: mirror the personal workspace: %w", err)
	}
	if err := insertMemberTx(ctx, tx, newID, userID, RoleOwner, now); err != nil {
		return "", false, 0, err
	}
	return replaced, false, stopped, nil
}

// mirroredTeamTx reads a workspace a membership is mirrored in: a team from
// another source, or ErrNotMirrored.
func mirroredTeamTx(ctx context.Context, tx *sql.Tx, workspaceID string) (Workspace, error) {
	w, err := GetTx(ctx, tx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if w.Kind != KindTeam || w.Source == SourceLocal {
		return Workspace{}, ErrNotMirrored
	}
	return w, nil
}

// MirrorMemberTx writes, inside the caller's transaction, a membership a
// directory's document lists, and reports whether anything changed: a new
// one, active with its role, whatever the person's standing here (a
// suspended person is a member, reading nothing until resumed) and whether
// or not the team is closed; a role changed, with what changing it here does
// (a demotion to member ends the consent attempts they started on the team's
// mailboxes, any change expires the invites they made there, and becoming an
// owner or an admin takes a stored manage away, by the schema); and a
// membership held or disabled made active again, a hold keeping its flags
// and sealed grants, which count again. None of the refusals of a local
// change apply: the directory decides who belongs.
func MirrorMemberTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, role Role, now time.Time) (bool, error) {
	if _, err := ParseRole(string(role)); err != nil {
		return false, err
	}
	if _, err := mirroredTeamTx(ctx, tx, workspaceID); err != nil {
		return false, err
	}
	var (
		was    Role
		status Status
		held   int64
	)
	err := tx.QueryRowContext(ctx, `SELECT role, status, held_at FROM workspace_members
		WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID).Scan(&was, &status, &held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, insertMemberTx(ctx, tx, workspaceID, userID, role, now)
	case err != nil:
		return false, fmt.Errorf("workspace: read the membership: %w", err)
	case was == role && status == StatusActive && held == 0:
		return false, nil
	}
	at := now.UTC().Unix()
	if role == RoleMember && was != RoleMember {
		// They managed the team's mailboxes by their role, and manage none
		// of them now.
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending
			WHERE owner_user_id = ? AND account_id IN (SELECT id FROM accounts WHERE workspace_id = ?)`,
			userID, workspaceID); err != nil {
			return false, fmt.Errorf("workspace: end the consent attempts of mailboxes no longer managed: %w", err)
		}
	}
	if err := expireInvitesByTx(ctx, tx, workspaceID, userID, at); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_members SET role = ?, status = 'active', held_at = 0, updated_at = ?
		WHERE workspace_id = ? AND user_id = ?`, string(role), at, workspaceID, userID); err != nil {
		return false, fmt.Errorf("workspace: mirror the membership: %w", err)
	}
	return true, nil
}

// EndResult is what ending a membership did.
type EndResult struct {
	// Held is a membership held rather than ended: kept disabled, with its
	// flags and sealed grants. Kept is one that was held already and stays
	// as it was: nothing changed.
	Held bool
	Kept bool
	// KeysRevoked counts the keys the person created in the team, revoked
	// as their creator left it.
	KeysRevoked int
	// Stranded are the mailboxes its end left with nobody who reads them,
	// as the journal recorded them.
	Stranded []Stranding
}

// EndMembershipTx ends, inside the caller's transaction, a membership of a
// team from another source that a directory's document no longer lists. No
// protection refuses it: not the last owner, not the last reader, not a
// closed team, not a person who is not active.
//
// With hold, asked while the directory's generation is not confirmed, a
// membership whose end would leave a mailbox stranded (StrandCandidatesTx)
// is held instead: disabled, its held_at set, its flags and sealed grants
// kept, unusable, until a document lists it again (MirrorMemberTx) or the
// generation is confirmed, which ends it; what a disable here ends ends
// with it: the consent attempts the person started on the team's mailboxes,
// the invites they made there, and the keys they created there
// (KeyStopCreatorLeft). A membership already held stays as it is.
//
// Otherwise it ends as a local removal does — the person's grants there,
// flags and sealed grants, their consent attempts, the team's invites still
// waiting for their address, the invites they made there and the keys they
// created there go with it — and each mailbox it leaves stranded, a held
// membership's included, is journaled (store.MailboxStranded, cause
// membership_ended, by the directory), naming the person, who still exists.
func EndMembershipTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, hold bool, now time.Time) (EndResult, error) {
	if _, err := mirroredTeamTx(ctx, tx, workspaceID); err != nil {
		return EndResult{}, err
	}
	var held int64
	err := tx.QueryRowContext(ctx, `SELECT held_at FROM workspace_members WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID).Scan(&held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return EndResult{}, ErrNotMember
	case err != nil:
		return EndResult{}, fmt.Errorf("workspace: read the membership: %w", err)
	}
	strands, err := StrandCandidatesTx(ctx, tx, userID, StrandScope{WorkspaceID: workspaceID})
	if err != nil {
		return EndResult{}, err
	}
	at := now.UTC().Unix()
	if hold && len(strands) > 0 {
		if held != 0 {
			return EndResult{Held: true, Kept: true}, nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending
			WHERE owner_user_id = ? AND account_id IN (SELECT id FROM accounts WHERE workspace_id = ?)`,
			userID, workspaceID); err != nil {
			return EndResult{}, fmt.Errorf("workspace: end the consent attempts of a held membership: %w", err)
		}
		if err := expireInvitesByTx(ctx, tx, workspaceID, userID, at); err != nil {
			return EndResult{}, err
		}
		revoked, err := revokeCreatedInTx(ctx, tx, workspaceID, userID, at)
		if err != nil {
			return EndResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspace_members SET status = 'disabled', held_at = ?, updated_at = ?
			WHERE workspace_id = ? AND user_id = ?`, at, at, workspaceID, userID); err != nil {
			return EndResult{}, fmt.Errorf("workspace: hold the membership: %w", err)
		}
		return EndResult{Held: true, KeysRevoked: revoked}, nil
	}
	stranded, err := JournalStrandedTx(ctx, tx, strands, userID, store.CauseMembershipEnded, store.ActorDirectory, now)
	if err != nil {
		return EndResult{}, err
	}
	if err := leaveTx(ctx, tx, workspaceID, userID); err != nil {
		return EndResult{}, err
	}
	if err := expireInvitesByTx(ctx, tx, workspaceID, userID, at); err != nil {
		return EndResult{}, err
	}
	revoked, err := revokeCreatedInTx(ctx, tx, workspaceID, userID, at)
	if err != nil {
		return EndResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID); err != nil {
		return EndResult{}, fmt.Errorf("workspace: end the membership: %w", err)
	}
	return EndResult{KeysRevoked: revoked, Stranded: stranded}, nil
}

// HeldMembership is a membership held (EndMembershipTx).
type HeldMembership struct {
	WorkspaceID string
	UserID      string
}

// HeldMembershipsTx lists, inside the caller's transaction, every membership
// held, oldest hold first.
func HeldMembershipsTx(ctx context.Context, tx *sql.Tx) ([]HeldMembership, error) {
	rows, err := tx.QueryContext(ctx, `SELECT workspace_id, user_id FROM workspace_members WHERE held_at <> 0
		ORDER BY held_at, workspace_id, user_id`)
	if err != nil {
		return nil, fmt.Errorf("workspace: list the held memberships: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var out []HeldMembership
	for rows.Next() {
		var h HeldMembership
		if err := rows.Scan(&h.WorkspaceID, &h.UserID); err != nil {
			return nil, fmt.Errorf("workspace: list the held memberships: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list the held memberships: %w", err)
	}
	return out, nil
}

// MirroredTeamsOfTx lists, inside the caller's transaction, the teams from
// another source a person has a membership of, held or not, oldest first:
// what a document that no longer lists one of them ends.
func MirroredTeamsOfTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	return listIDs(ctx, tx, `SELECT w.id FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? AND w.kind = 'team' AND w.source <> 'local'
		ORDER BY w.created_at, w.id`, userID)
}

// MailboxesInTx lists, inside the caller's transaction, the mailboxes of
// these workspaces, live or removed: what a change to their members, name or
// closure may start or stop syncing.
func MailboxesInTx(ctx context.Context, tx *sql.Tx, workspaceIDs []string) ([]string, error) {
	if len(workspaceIDs) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(workspaceIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	return listIDs(ctx, tx, `SELECT id FROM accounts WHERE workspace_id IN (SELECT value FROM json_each(?))
		ORDER BY created_at, rowid`, string(list))
}

// PersonalBlock is a local personal workspace of a person who signs in
// through an identity provider, and what keeps a directory's document from
// replacing it (ReplacePersonalTx): the mailboxes in it, live or removed,
// and its live keys. Its stopped keys would go with it.
type PersonalBlock struct {
	UserID      string
	WorkspaceID string
	Accounts    int
	LiveKeys    int
	StoppedKeys int
}

// PersonalReplacementBlocks lists the local personal workspaces of people who
// sign in through an identity provider that a directory's document would not
// replace while they hold what they hold, oldest person first: what a daemon
// that follows a directory says at start.
func (r *Repository) PersonalReplacementBlocks(ctx context.Context) ([]PersonalBlock, error) {
	now := r.now().UTC().Unix()
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT u.id, w.id,
		  (SELECT count(*) FROM accounts a WHERE a.workspace_id = w.id),
		  (SELECT count(*) FROM api_keys k WHERE k.workspace_id = w.id AND k.revoked_at = 0
		     AND (k.expires_at = 0 OR k.expires_at > ?1)),
		  (SELECT count(*) FROM api_keys k WHERE k.workspace_id = w.id AND NOT (k.revoked_at = 0
		     AND (k.expires_at = 0 OR k.expires_at > ?1)))
		  FROM users u JOIN workspaces w ON w.person_id = u.id AND w.kind = 'personal' AND w.source = 'local'
		 WHERE EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = u.id)
		 ORDER BY u.created_at, u.id`, now)
	if err != nil {
		return nil, fmt.Errorf("workspace: read what keeps personal workspaces: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []PersonalBlock
	for rows.Next() {
		var b PersonalBlock
		if err := rows.Scan(&b.UserID, &b.WorkspaceID, &b.Accounts, &b.LiveKeys, &b.StoppedKeys); err != nil {
			return nil, fmt.Errorf("workspace: read what keeps personal workspaces: %w", err)
		}
		if b.Accounts > 0 || b.LiveKeys > 0 {
			out = append(out, b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read what keeps personal workspaces: %w", err)
	}
	return out, nil
}
