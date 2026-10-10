package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Administering workspaces (docs/workspaces.md, "Who may do what"): the
// workspaces a caller belongs to, a team's members and invites, and who holds
// what on each of its mailboxes.
//
// A team's owners and admins administer it, as Wappie's do: they see its
// members and who holds what on each mailbox, invite, change and remove
// members (an admin members only), grant and revoke, and manage every
// mailbox of the team by their role, reading none. A member sees only the
// mailboxes they hold a grant on, and neither the members nor the directory,
// and does not leave by themselves: an owner or an admin removes them. An
// owner leaves while another owner remains. The operator administers every
// team's people, grants manage to members and revokes, and reads nothing.
//
// The rules about who may ask are all here. internal/workspace keeps the data
// and the protections a change must never break — a team keeps an active
// owner, a team mailbox someone reads keeps a reader — and runs, first inside
// each write's transaction, the check this file hands it, which re-reads the
// caller's own place in the workspace there. A workspace's key administers
// nothing: these take a person signed in, or the operator's unrestricted
// instance admin key.
//
// Not found rather than forbidden, as for mailboxes: a workspace the caller
// is not an active member of does not exist for them, nor does a mailbox of
// it they neither administer nor hold anything on. Once they see it, a role
// or a flag they lack is not_authorized.

// Workspace is a workspace as a caller sees it.
type Workspace struct {
	ID string `json:"id"`
	// Kind is personal, team or operator.
	Kind string `json:"kind"`
	// Source is where it and its memberships come from: local, created and
	// changed here, or platform, mirrored from the platform and never
	// changed here.
	Source string `json:"source"`
	// Name is a team's; empty for a personal workspace and the operator's,
	// which a client names itself.
	Name string `json:"name"`
	// Role and Status are the caller's own membership; absent from the
	// operator's listing.
	Role   string `json:"role,omitempty"`
	Status string `json:"status,omitempty"`
	// Members and Mailboxes are counts, in the operator's listing only.
	Members   *int  `json:"members,omitempty"`
	Mailboxes *int  `json:"mailboxes,omitempty"`
	CreatedAt int64 `json:"created_at"`
}

// CreateWorkspaceRequest creates a team.
type CreateWorkspaceRequest struct {
	Name string `json:"name"`
	// OwnerEmail names the team's first owner, an existing person, when the
	// operator creates it. A person who creates a team is its owner, and
	// may not name another.
	OwnerEmail string `json:"owner_email,omitempty"`
}

// RenameWorkspaceRequest renames a team.
type RenameWorkspaceRequest struct {
	Name string `json:"name"`
}

// Member is a membership of a workspace.
type Member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	// Role is owner, admin or member; Status active or disabled. A disabled
	// membership is listed and reaches nothing.
	Role   string `json:"role"`
	Status string `json:"status"`
	// PersonDisabled is the person switched off on the instance: their
	// membership counts for nothing until they are back.
	PersonDisabled bool `json:"person_disabled,omitempty"`
	// LastOwner is the team's only active owner, who cannot be demoted,
	// disabled or removed until another member is an owner.
	LastOwner bool `json:"last_owner"`
	// LastReaderOf are the team mailboxes this member is the only reader
	// of: their read is not revoked, nor their membership disabled or
	// removed, nor their account closed without force, until someone else
	// reads them.
	LastReaderOf []string `json:"last_reader_of"`
	// SealID and PublicKey are what a grant to the person is bound by and
	// sealed to (docs/key-scheme.md sections 3.1 and 9.1), for an owner or
	// an admin who gives them read on a mailbox that has a key: their seal
	// id, and their account public key, absent until they enrol.
	SealID    string `json:"seal_id,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	JoinedAt  int64  `json:"joined_at"`
}

// MemberRequest changes a member's role, status or both; a field left out
// stays as it is.
type MemberRequest struct {
	Role   *string `json:"role,omitempty"`
	Status *string `json:"status,omitempty"`
}

// TeamInvite is an invite into a team, as an owner or admin sees it. The link
// is in the answer that creates it and nowhere else.
type TeamInvite struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	WorkspaceID string `json:"workspace_id"`
	// Role is the role in the team it gives.
	Role      string `json:"role"`
	URL       string `json:"url,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// TeamInviteRequest invites an address into a team.
type TeamInviteRequest struct {
	Email string `json:"email"`
	// Role is owner, admin or member; member when left out.
	Role string `json:"role,omitempty"`
}

// AcceptInviteRequest redeems a team invite for the person signed in.
type AcceptInviteRequest struct {
	Invite string `json:"invite"`
}

// MailboxAccess is one mailbox of a workspace and who holds what on it: the
// access directory, which shows addresses and grants, never the index.
type MailboxAccess struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Provider  string `json:"provider"`
	State     string `json:"state"`
	// LinkedBy is who linked it, for attribution only: "usr_…",
	// "key:<prefix>" or "cli"; absent once that person is deleted.
	LinkedBy string `json:"linked_by,omitempty"`
	// Sync is the mailbox's own consent to sync: a team mailbox's, its
	// workspace's; an operator mailbox's switch. Absent for a personal
	// mailbox, which syncs under its person's own consent.
	Sync *MailboxConsent `json:"sync,omitempty"`
	// Readers counts who reads it now, by the one rule (docs/key-scheme.md
	// section 12.13): active members, active on the instance, holding read,
	// and, on a mailbox that has a key, a grant at its current epoch. Keys
	// and roles never count.
	Readers int `json:"readers"`
	// NoReader is a team mailbox nobody can read: it syncs nothing more,
	// and only removing it and linking it again gives anyone read on it.
	NoReader bool `json:"no_reader"`
	// Epoch is the mailbox key's current epoch; absent for a mailbox
	// without a key, which is read by the flag alone.
	Epoch  int     `json:"epoch,omitempty"`
	Grants []Grant `json:"grants"`
	// Keys are the live API keys holding something on it, by when they
	// were given it. Keys never count as readers.
	Keys []MailboxKey `json:"keys"`
}

// MailboxKey is a live API key holding something on a mailbox, as the access
// directory lists it.
type MailboxKey struct {
	Prefix string     `json:"prefix"`
	Name   string     `json:"name"`
	Scope  auth.Scope `json:"scope"`
	Read   bool       `json:"read"`
	Act    bool       `json:"act"`
	Send   bool       `json:"send"`
	// CreatedBy is the person who created the key; absent once they are
	// deleted.
	CreatedBy string `json:"created_by,omitempty"`
	// GrantedBy is who set what it holds here last: "usr_…" or "migration";
	// absent once that person is deleted.
	GrantedBy string `json:"granted_by,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
	// CarriedOver is a person's key the upgrade to workspace keys carried
	// over, with mailboxes in several workspaces.
	CarriedOver bool `json:"carried_over,omitempty"`
}

// MailboxConsent is a mailbox's own consent to sync, as its workspace's
// owners and admins see it.
type MailboxConsent struct {
	// Enabled is whether it stands. Sync runs when it does, the mailbox is
	// active and, for a team's, someone reads it.
	Enabled bool `json:"enabled"`
	// EnabledAt, EnabledBy and Version say when, by whom ("usr_…",
	// "key:<prefix>", "cli"; absent once that person is deleted) and to
	// which revision of the sync text it was given; no revision for an
	// operator mailbox.
	EnabledAt int64  `json:"enabled_at,omitempty"`
	EnabledBy string `json:"enabled_by,omitempty"`
	Version   string `json:"version,omitempty"`
	// Migrated is a consent the upgrade to this release copied from the
	// person who linked the mailbox (EnabledBy), still bound to them: their
	// turning sync off, or their account being disabled or deleted, stops it
	// and deletes its index, until an owner or an admin confirms it at the
	// current revision (PUT /v1/accounts/{id}/sync). Also set, with Enabled
	// false, on a mailbox kept stopped with its index because that person
	// was disabled.
	Migrated bool `json:"migrated,omitempty"`
	// Current is whether Version is the revision this server asks for now.
	Current bool `json:"current"`
}

// Grant is what one person holds on one mailbox.
type Grant struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
	Read      bool   `json:"read"`
	Act       bool   `json:"act"`
	Send      bool   `json:"send"`
	// Manage is stored for members only: owners and admins manage every
	// mailbox of their workspace by their role, which the members list says.
	Manage bool `json:"manage"`
	// GrantedBy is who set it last: "usr_…", "key:<prefix>" or "migration";
	// absent once that person is deleted.
	GrantedBy string `json:"granted_by,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
	// Sealed is the person holding the mailbox's key at its current epoch
	// (a grant, docs/key-scheme.md section 9): on a mailbox that has a key,
	// what reading takes beside the flag. Always false on one without.
	Sealed bool `json:"sealed"`
}

// GrantRequest sets exactly what a person holds on a mailbox. Every flag is
// required, so leaving one out never takes it away by accident; all false is
// refused, since revoking is how a grant goes.
type GrantRequest struct {
	Read   *bool `json:"read"`
	Act    *bool `json:"act"`
	Send   *bool `json:"send"`
	Manage *bool `json:"manage"`
	// Grant is the mailbox's key sealed to the person at its current epoch
	// (docs/key-scheme.md sections 9.3 and 12.13), base64url of 88 bytes,
	// which the giver's browser sealed: required with read given on a
	// mailbox that has a key to a person who has an account key, and
	// refused with anything else. It takes a fresh step-up when the person
	// is someone else.
	Grant string `json:"grant,omitempty"`
	// PublicKey is the person's account public key the browser sealed Grant
	// to, base64url of 32 bytes: required with a grant, and refused without
	// one. A grant whose key is not the person's now is a conflict: the
	// console read them before their reset.
	PublicKey string `json:"public_key,omitempty"`
}

// Errors of administering a workspace.
var (
	errAdministers = E(CodeNotAuthorized,
		"administering workspaces needs a person signed in, or an unrestricted instance admin key", nil)
	errTeamAdmin        = E(CodeNotAuthorized, "only an owner or an admin of the team may do that", nil)
	errAdminMembersOnly = E(CodeNotAuthorized,
		"an admin changes and removes members only, and never makes anyone an admin or an owner", nil)
	errAdminInvitesMembers  = E(CodeNotAuthorized, "an admin invites members only; an owner invites admins and owners", nil)
	errNoInvite             = E(CodeNotFound, "no such pending invite", nil)
	errNoAccount            = E(CodeNotFound, "no such account", nil)
	errOperatorGrantsManage = E(CodeNotAuthorized,
		"the operator grants manage only: read passes from an owner or an admin who reads the mailbox, "+
			"and act and send from an owner or an admin", nil)
	errNoLeave = E(CodeNotAuthorized,
		"members and admins do not leave a team by themselves: an owner removes an admin, and an owner or an admin a member", nil)
	errGrantReadNotHeld = E(CodeNotAuthorized,
		"you can grant read on a mailbox only while you read it yourself", nil)
)

// administers admits who may administer workspaces: a person signed in, or
// the operator.
func administers(p Principal) error {
	if p.IsSession() || isOperator(p) {
		return nil
	}
	return errAdministers
}

// adminOf reports whether a membership administers its team's people and
// grants.
func adminOf(m workspace.Member) bool {
	return m.Role == workspace.RoleOwner || m.Role == workspace.RoleAdmin
}

// workspaceOf reads a workspace the caller asks about, and their active
// membership of it: the operator reaches every workspace and has none; a
// person who is not an active member is told it does not exist.
func (s *Service) workspaceOf(ctx context.Context, p Principal, id string) (workspace.Workspace, workspace.Member, error) {
	if s.workspaces == nil {
		return workspace.Workspace{}, workspace.Member{}, errNoWorkspace
	}
	w, err := s.workspaces.Get(ctx, id)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return workspace.Workspace{}, workspace.Member{}, errNoWorkspace
	case err != nil:
		return workspace.Workspace{}, workspace.Member{}, E(CodeInternal, "reading the workspace failed", err)
	}
	if isOperator(p) {
		return w, workspace.Member{}, nil
	}
	m, err := s.workspaces.Member(ctx, id, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNotMember):
		return workspace.Workspace{}, workspace.Member{}, errNoWorkspace
	case err != nil:
		return workspace.Workspace{}, workspace.Member{}, E(CodeInternal, "reading the workspace failed", err)
	case !m.Active():
		return workspace.Workspace{}, workspace.Member{}, errNoWorkspace
	}
	return w, m, nil
}

// callerTx re-reads, inside a write's transaction, the caller's place in a
// workspace: the operator has none and needs none; a person must still be an
// active member, or the workspace no longer exists for them.
func callerTx(ctx context.Context, tx *sql.Tx, p Principal, workspaceID string) (workspace.Member, error) {
	if isOperator(p) {
		return workspace.Member{}, nil
	}
	m, err := workspace.MemberTx(ctx, tx, workspaceID, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNotMember):
		return workspace.Member{}, errNoWorkspace
	case err != nil:
		return workspace.Member{}, err
	case !m.Active():
		return workspace.Member{}, errNoWorkspace
	}
	return m, nil
}

// ListWorkspaces lists the workspaces the caller reaches: a person's active
// memberships, with their role in each; for an instance key the operator
// workspace; for the operator every workspace, with how many members and
// mailboxes each has. A workspace key lists its workspace, or, carried over
// from a person's, the workspaces of the mailboxes it holds.
func (s *Service) ListWorkspaces(ctx context.Context, p Principal) ([]Workspace, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return nil, err
	}
	out := []Workspace{}
	if s.workspaces == nil {
		return out, nil
	}
	switch {
	case isOperator(p):
		all, err := s.workspaces.All(ctx)
		if err != nil {
			return nil, E(CodeInternal, "listing workspaces failed", err)
		}
		for _, w := range all {
			out = append(out, presentWorkspace(w, true))
		}
	case p.IsInstance():
		w, err := s.workspaces.Get(ctx, workspace.OperatorID)
		if err != nil {
			return nil, E(CodeInternal, "reading the operator workspace failed", err)
		}
		out = append(out, presentWorkspace(w, false))
	case p.IsWorkspaceKey():
		ids, err := s.workspaces.KeyWorkspaces(ctx, p.KeyPrefix)
		if err != nil {
			return nil, E(CodeInternal, "listing workspaces failed", err)
		}
		for _, id := range ids {
			w, err := s.workspaces.Get(ctx, id)
			switch {
			case errors.Is(err, workspace.ErrNotFound):
				continue
			case err != nil:
				return nil, E(CodeInternal, "listing workspaces failed", err)
			}
			out = append(out, presentWorkspace(w, false))
		}
	default:
		mine, err := s.workspaces.ForPerson(ctx, p.UserID)
		if err != nil {
			return nil, E(CodeInternal, "listing workspaces failed", err)
		}
		for _, w := range mine {
			out = append(out, presentWorkspace(w, false))
		}
	}
	return out, nil
}

// CreateWorkspace creates a team: a person's, who becomes its owner, or the
// operator's for an existing person it names as the owner. The workspace
// source decides whether teams are created here at all.
func (s *Service) CreateWorkspace(ctx context.Context, p Principal, req CreateWorkspaceRequest) (Workspace, error) {
	if err := administers(p); err != nil {
		return Workspace{}, err
	}
	if s.workspaces == nil {
		return Workspace{}, E(CodeInternal, "workspaces are not available", nil)
	}
	owner := p.UserID
	switch {
	case isOperator(p):
		email, err := auth.NormalizeEmail(req.OwnerEmail)
		if err != nil {
			return Workspace{}, E(CodeBadRequest,
				"owner_email is required: the existing person who will own the team", err)
		}
		user, err := s.users.GetByEmail(ctx, email)
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			return Workspace{}, E(CodeNotFound, "no account has that address", err)
		case err != nil:
			return Workspace{}, E(CodeInternal, "reading the person failed", err)
		}
		owner = user.ID
	case strings.TrimSpace(req.OwnerEmail) != "":
		return Workspace{}, E(CodeBadRequest,
			"owner_email is the operator's to give: a person who creates a team owns it", nil)
	}
	w, err := s.workspaces.CreateTeam(ctx, req.Name, owner, nil)
	switch {
	case errors.Is(err, workspace.ErrNoSuchPerson):
		return Workspace{}, E(CodeNotFound, "no active person has that address", err)
	case err != nil:
		return Workspace{}, fromWorkspace(err, "creating the team failed")
	}
	if isOperator(p) {
		members, mailboxes := 1, 0
		out := presentWorkspace(w, false)
		out.Role, out.Status, out.Members, out.Mailboxes = "", "", &members, &mailboxes
		return out, nil
	}
	return presentWorkspace(w, false), nil
}

// RenameWorkspace renames a team: its owners and admins, or the operator.
func (s *Service) RenameWorkspace(ctx context.Context, p Principal, id string, req RenameWorkspaceRequest) (Workspace, error) {
	if err := administers(p); err != nil {
		return Workspace{}, err
	}
	_, me, err := s.workspaceOf(ctx, p, id)
	if err != nil {
		return Workspace{}, err
	}
	w, err := s.workspaces.Rename(ctx, id, req.Name, func(tx *sql.Tx) error {
		m, err := callerTx(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if !isOperator(p) && !adminOf(m) {
			return errTeamAdmin
		}
		me = m
		return nil
	})
	if err != nil {
		return Workspace{}, fromWorkspace(err, "renaming the team failed")
	}
	if !isOperator(p) {
		w.Role, w.Status = me.Role, me.Status
	}
	return presentWorkspace(w, false), nil
}

// ListMembers lists a workspace's memberships, active or not, for its
// owners and admins and for the operator; a member is not_authorized. The
// protections are marked in advance (last_owner, last_reader_of), so a client
// can explain before anyone tries.
func (s *Service) ListMembers(ctx context.Context, p Principal, id string) ([]Member, error) {
	if err := administers(p); err != nil {
		return nil, err
	}
	_, me, err := s.workspaceOf(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if !isOperator(p) && !adminOf(me) {
		return nil, errTeamAdmin
	}
	members, err := s.workspaces.Members(ctx, id)
	if err != nil {
		return nil, fromWorkspace(err, "listing members failed")
	}
	out := make([]Member, 0, len(members))
	for _, m := range members {
		out = append(out, presentMember(m))
	}
	return out, nil
}

// SetMember changes a member's role or status in a team. An owner changes
// anyone's, the operator too; an admin changes only members', and never to
// admin or owner. Any change expires the invites the person made in the team
// that are still waiting. Disabling a membership takes the person's grants
// in the team with it, and enabling it again restores none; the last reader
// of one of its mailboxes is not disabled.
func (s *Service) SetMember(ctx context.Context, p Principal, id, userID string, req MemberRequest) (Member, error) {
	if err := administers(p); err != nil {
		return Member{}, err
	}
	var change workspace.MemberChange
	if req.Role != nil {
		role, err := workspace.ParseRole(*req.Role)
		if err != nil {
			return Member{}, fromWorkspace(err, "")
		}
		change.Role = &role
	}
	if req.Status != nil {
		status, err := workspace.ParseStatus(*req.Status)
		if err != nil {
			return Member{}, fromWorkspace(err, "")
		}
		change.Status = &status
	}
	if change.Role == nil && change.Status == nil {
		return Member{}, E(CodeBadRequest, "name a role, a status or both to change", nil)
	}
	if _, _, err := s.workspaceOf(ctx, p, id); err != nil {
		return Member{}, err
	}
	m, err := s.workspaces.SetMember(ctx, id, userID, change, func(tx *sql.Tx) error {
		me, err := callerTx(ctx, tx, p, id)
		if err != nil {
			return err
		}
		switch {
		case isOperator(p), me.Role == workspace.RoleOwner:
			return nil
		case me.Role != workspace.RoleAdmin:
			return errTeamAdmin
		}
		target, err := workspace.MemberTx(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if target.Role != workspace.RoleMember || (change.Role != nil && *change.Role != workspace.RoleMember) {
			return errAdminMembersOnly
		}
		return nil
	})
	if err != nil {
		return Member{}, fromWorkspace(err, "changing the member failed")
	}
	s.accessChanged()
	return presentMember(m), nil
}

// RemoveMember removes a person from a team: an owner removes anyone,
// themselves included while another active owner remains; an admin members
// only; the operator anyone. A member or an admin never removes themselves.
// Their grants in the team go with them, the team's invites still waiting
// for their address are deleted, the invites they made there expire and the
// API keys they created there are revoked. The last reader of one of the
// team's mailboxes is not removed.
func (s *Service) RemoveMember(ctx context.Context, p Principal, id, userID string) error {
	if err := administers(p); err != nil {
		return err
	}
	if _, _, err := s.workspaceOf(ctx, p, id); err != nil {
		return err
	}
	err := s.workspaces.RemoveMember(ctx, id, userID, func(tx *sql.Tx) error {
		me, err := callerTx(ctx, tx, p, id)
		if err != nil {
			return err
		}
		switch {
		case isOperator(p), me.Role == workspace.RoleOwner:
			return nil
		case userID == p.UserID:
			return errNoLeave
		case me.Role != workspace.RoleAdmin:
			return errTeamAdmin
		}
		target, err := workspace.MemberTx(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if target.Role != workspace.RoleMember {
			return errAdminMembersOnly
		}
		return nil
	})
	if err != nil {
		return fromWorkspace(err, "removing the member failed")
	}
	s.accessChanged()
	return nil
}

// ListTeamInvites lists a team's pending invites, newest first: every one for
// an owner and the operator, the member invites for an admin.
func (s *Service) ListTeamInvites(ctx context.Context, p Principal, id string) ([]TeamInvite, error) {
	if err := administers(p); err != nil {
		return nil, err
	}
	_, me, err := s.workspaceOf(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if !isOperator(p) && !adminOf(me) {
		return nil, errTeamAdmin
	}
	pending, err := s.users.PendingInvites(ctx, id)
	if err != nil {
		return nil, E(CodeInternal, "listing invites failed", err)
	}
	out := []TeamInvite{}
	for _, inv := range pending {
		if !isOperator(p) && me.Role == workspace.RoleAdmin && inv.WorkspaceRole != workspace.RoleMember {
			continue
		}
		out = append(out, presentTeamInvite(inv, ""))
	}
	return out, nil
}

// CreateTeamInvite invites an address into a team, with a role in it, and
// returns the link to send: an owner invites any role, an admin members only,
// and so does the operator any role. Someone who already has an account
// accepts it signed in. Someone without one signs up with it, as an instance
// member with their personal workspace, only when the operator or an instance
// owner made it; anyone else's invite needs an invitation to the server first
// (auth.SignUp). Joining gives no access to any mailbox. Where people sign in
// only through an extension, nobody invites anyone.
func (s *Service) CreateTeamInvite(ctx context.Context, p Principal, id string, req TeamInviteRequest) (TeamInvite, error) {
	if err := s.passwordsInUse(); err != nil {
		return TeamInvite{}, err
	}
	if err := administers(p); err != nil {
		return TeamInvite{}, err
	}
	if _, _, err := s.workspaceOf(ctx, p, id); err != nil {
		return TeamInvite{}, err
	}
	role := workspace.RoleMember
	if req.Role != "" {
		parsed, err := workspace.ParseRole(req.Role)
		if err != nil {
			return TeamInvite{}, fromWorkspace(err, "")
		}
		role = parsed
	}
	if s.publicURL == "" {
		return TeamInvite{}, E(CodeConflict, "set MAIL_PUBLIC_URL first: invite links are built from it", nil)
	}
	code, inv, err := s.users.CreateInvite(ctx, auth.NewInvite{
		Email: req.Email, WorkspaceID: id, WorkspaceRole: role, CreatedBy: p.Actor(),
		Check: func(tx *sql.Tx) error {
			me, err := callerTx(ctx, tx, p, id)
			if err != nil {
				return err
			}
			switch {
			case isOperator(p), me.Role == workspace.RoleOwner:
				return nil
			case me.Role == workspace.RoleAdmin && role == workspace.RoleMember:
				return nil
			case me.Role == workspace.RoleAdmin:
				return errAdminInvitesMembers
			}
			return errTeamAdmin
		},
	})
	if err != nil {
		return TeamInvite{}, fromTeamInvite(err, "creating the invite failed")
	}
	return presentTeamInvite(inv, auth.InviteLink(s.publicURL, code, inv.Email)), nil
}

// RevokeTeamInvite deletes a pending invite of a team, so its link no longer
// works: an owner's or the operator's to do for any, an admin's for a member
// invite. One an admin may not see is not found for them.
func (s *Service) RevokeTeamInvite(ctx context.Context, p Principal, id, inviteID string) error {
	if err := administers(p); err != nil {
		return err
	}
	if _, _, err := s.workspaceOf(ctx, p, id); err != nil {
		return err
	}
	err := s.users.RevokeInvite(ctx, id, inviteID, func(tx *sql.Tx, inv auth.Invite) error {
		me, err := callerTx(ctx, tx, p, id)
		if err != nil {
			return err
		}
		switch {
		case isOperator(p), me.Role == workspace.RoleOwner:
			return nil
		case me.Role == workspace.RoleAdmin && inv.WorkspaceRole == workspace.RoleMember:
			return nil
		case me.Role == workspace.RoleAdmin:
			return errNoInvite
		}
		return errTeamAdmin
	})
	if err != nil {
		return fromTeamInvite(err, "revoking the invite failed")
	}
	return nil
}

// AcceptInvite redeems a team invite for the person signed in, who joins the
// team with the role it names. The invite must be for their address; a
// refused one is not spent. Joining a team grants access to no mailbox.
// Refused where people sign in only through an extension.
func (s *Service) AcceptInvite(ctx context.Context, p Principal, req AcceptInviteRequest) (Workspace, error) {
	if err := s.passwordsInUse(); err != nil {
		return Workspace{}, err
	}
	if err := requireSession(p); err != nil {
		return Workspace{}, err
	}
	w, err := s.users.AcceptInvite(ctx, p.UserID, req.Invite)
	if err != nil {
		return Workspace{}, fromTeamInvite(err, "accepting the invite failed")
	}
	return presentWorkspace(w, false), nil
}

// AccessDirectory lists a workspace's mailboxes and every grant on each, with
// who can read each one, its own consent to sync, who linked it and the live
// API keys holding something on it: for its owners and admins and the
// operator; a member is not_authorized. Addresses and grants, never what a
// mailbox holds: seeing the directory reads no mail.
func (s *Service) AccessDirectory(ctx context.Context, p Principal, id string) ([]MailboxAccess, error) {
	if err := administers(p); err != nil {
		return nil, err
	}
	_, me, err := s.workspaceOf(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if !isOperator(p) && !adminOf(me) {
		return nil, errTeamAdmin
	}
	dir, err := s.workspaces.Directory(ctx, id)
	if err != nil {
		return nil, fromWorkspace(err, "listing who has access failed")
	}
	// The provider as accounts present it: an iCloud mailbox is generic
	// IMAP in the database.
	accounts, err := s.accounts.Repo().ListVisible(ctx, account.Visibility{All: true, Workspace: id})
	if err != nil {
		return nil, E(CodeInternal, "listing the workspace's mailboxes failed", err)
	}
	byID := make(map[string]account.Account, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	out := make([]MailboxAccess, 0, len(dir))
	for _, mb := range dir {
		shown := MailboxAccess{
			AccountID: mb.AccountID, Email: mb.Email, Provider: mb.Provider, State: mb.State,
			LinkedBy: mb.LinkedBy, Readers: mb.Readers, NoReader: mb.NoReader, Epoch: mb.Epoch,
			Grants: make([]Grant, 0, len(mb.Grants)), Keys: make([]MailboxKey, 0, len(mb.Keys)),
		}
		for _, k := range mb.Keys {
			shown.Keys = append(shown.Keys, MailboxKey{
				Prefix: k.Prefix, Name: k.Name, Scope: auth.Scope(k.Scope), Read: k.Read, Act: k.Act, Send: k.Send,
				CreatedBy: k.CreatedBy, GrantedBy: k.GrantedBy, UpdatedAt: unixOrZero(k.UpdatedAt),
				CarriedOver: k.CarriedOver,
			})
		}
		if a, ok := byID[mb.AccountID]; ok {
			shown.Provider = a.ProviderName()
			if a.OwnerUserID == "" {
				shown.Sync = s.presentMailboxConsent(mb.Sync)
			}
		}
		for _, g := range mb.Grants {
			shown.Grants = append(shown.Grants, presentGrant(g))
		}
		out = append(out, shown)
	}
	return out, nil
}

// presentMailboxConsent renders a mailbox's own consent to sync.
func (s *Service) presentMailboxConsent(c workspace.Consent) *MailboxConsent {
	out := &MailboxConsent{
		Enabled: !c.At.IsZero(), EnabledBy: c.By, Version: c.Version, Migrated: c.Migrated,
		Current: c.Version == s.consent.Sync,
	}
	if !c.At.IsZero() {
		out.EnabledAt = c.At.Unix()
	}
	return out
}

// SetAccess sets exactly what a person holds on a mailbox (docs/workspaces.md,
// "Who may change a grant"):
//
//   - who: an owner or an admin of the mailbox's workspace, or the operator,
//     which grants manage only;
//   - what: read only from an owner or an admin who reads the mailbox
//     themselves, now — no role reads, and none hands itself read; act and
//     send from any owner or admin, without holding them, act to someone who
//     reads after the change; manage to members only, whom owners and admins
//     manage beside by their role;
//   - to whom: an active member of the mailbox's workspace, the caller
//     included;
//   - with what: on a mailbox that has a key, read comes with the person's
//     grant (docs/key-scheme.md sections 9.3 and 12.13), which the giver's
//     browser sealed from their own, after a fresh step-up, to the account
//     public key it names, which must be the person's now; to a member who
//     has no account key yet, and on a mailbox without a key, read is the
//     flag alone, and the key follows (SupplyKey, WriteFirstKey).
//
// Taking flags away this way is a revoke, and needs nothing held; taking
// read deletes the person's grants on the mailbox. The last reader of a team
// mailbox keeps read (the repository's rule).
func (s *Service) SetAccess(ctx context.Context, p Principal, accountID, userID string, req GrantRequest) (Grant, error) {
	if err := administers(p); err != nil {
		return Grant{}, err
	}
	if req.Read == nil || req.Act == nil || req.Send == nil || req.Manage == nil {
		return Grant{}, E(CodeBadRequest,
			"read, act, send and manage are all required: the grant is set to exactly them", nil)
	}
	flags := workspace.Flags{Read: *req.Read, Act: *req.Act, Send: *req.Send, Manage: *req.Manage}
	var sealed *workspace.Sealed
	switch {
	case req.Grant != "":
		// Sealed by a person's browser, from their own grant: never a key's.
		if err := requireSession(p); err != nil {
			return Grant{}, err
		}
		got, err := sealedOf(req.Grant, req.PublicKey)
		if err != nil {
			return Grant{}, err
		}
		sealed = &got
	case req.PublicKey != "":
		return Grant{}, errSealedToAlone
	}
	a, err := s.accessTarget(ctx, p, accountID)
	if err != nil {
		return Grant{}, err
	}
	g, err := s.workspaces.SetGrantSealed(ctx, a.ID, userID, flags, sealed, p.Actor(), func(tx *sql.Tx) error {
		if err := mayGrantTx(ctx, tx, p, a, userID, flags); err != nil {
			return err
		}
		if sealed != nil && userID != p.UserID {
			return s.stepUpTx(ctx, tx, p)
		}
		return nil
	})
	if err != nil {
		return Grant{}, fromKeys(err, "setting the access failed")
	}
	s.accessChanged()
	s.reconcile(a.ID)
	return presentGrant(g), nil
}

// RevokeAccess takes flags away from a person's grant on a mailbox; none
// named takes every one. An owner or an admin of the workspace, their own
// flags included, or the operator; a member never, not even their own. The
// last reader of a team mailbox keeps read.
func (s *Service) RevokeAccess(ctx context.Context, p Principal, accountID, userID string, drop workspace.Flags) error {
	if err := administers(p); err != nil {
		return err
	}
	a, err := s.accessTarget(ctx, p, accountID)
	if err != nil {
		return err
	}
	_, err = s.workspaces.Revoke(ctx, a.ID, userID, drop, func(tx *sql.Tx) error {
		_, err := administratorTx(ctx, tx, p, a)
		return err
	})
	if err != nil {
		return fromWorkspace(err, "revoking the access failed")
	}
	s.accessChanged()
	s.reconcile(a.ID)
	return nil
}

// ParseFlags reads a comma-separated list of flags — read, act, send,
// manage — as REST and the command line name them.
func ParseFlags(list string) (workspace.Flags, error) {
	var f workspace.Flags
	for _, name := range strings.Split(list, ",") {
		switch strings.TrimSpace(name) {
		case "":
		case "read":
			f.Read = true
		case "act":
			f.Act = true
		case "send":
			f.Send = true
		case "manage":
			f.Manage = true
		default:
			return workspace.Flags{}, Ef(CodeBadRequest, nil, "unknown flag %q: the flags are read, act, send and manage", name)
		}
	}
	return f, nil
}

// accessTarget reads a mailbox whose access the caller asks about. The
// operator reaches any. A person must be an owner or an admin of its
// workspace; a member who holds a grant on it sees it and is not_authorized;
// anyone else is told it does not exist.
func (s *Service) accessTarget(ctx context.Context, p Principal, accountID string) (account.Account, error) {
	a, err := s.accounts.Repo().Get(ctx, accountID)
	switch {
	case errors.Is(err, account.ErrNotFound):
		return account.Account{}, errNoAccount
	case err != nil:
		return account.Account{}, E(CodeInternal, "reading the account failed", err)
	}
	if isOperator(p) {
		return a, nil
	}
	_, me, err := s.workspaceOf(ctx, p, a.WorkspaceID)
	switch {
	case CodeOf(err) == CodeNotFound:
		return account.Account{}, errNoAccount
	case err != nil:
		return account.Account{}, err
	case adminOf(me):
		return a, nil
	}
	held, err := s.grantsOf(ctx, p, a.ID)
	if err != nil {
		return account.Account{}, E(CodeInternal, "reading the access to the mailbox failed", err)
	}
	// Whether they hold a grant, as administratorTx asks: a member waiting
	// for the mailbox's key holds one whose read and act count for nothing
	// yet, and may hold nothing else.
	if _, ok := held[a.ID]; !ok {
		return account.Account{}, errNoAccount
	}
	return account.Account{}, errTeamAdmin
}

// administratorTx re-reads, inside a write's transaction, that the caller
// administers a mailbox's grants: the operator, or an active owner or admin
// of its workspace, whose membership it returns. A member who holds a grant
// on it is not_authorized; anyone else is told it does not exist.
func administratorTx(ctx context.Context, tx *sql.Tx, p Principal, a account.Account) (workspace.Member, error) {
	if isOperator(p) {
		return workspace.Member{}, nil
	}
	me, err := callerTx(ctx, tx, p, a.WorkspaceID)
	if err != nil {
		return workspace.Member{}, errNoAccount
	}
	if adminOf(me) {
		return me, nil
	}
	mine, err := workspace.GrantTx(ctx, tx, a.ID, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNoGrant):
		return workspace.Member{}, errNoAccount
	case err != nil:
		return workspace.Member{}, err
	}
	if mine.Any() {
		return workspace.Member{}, errTeamAdmin
	}
	return workspace.Member{}, errNoAccount
}

// mayGrantTx is the grant rule, re-read inside the transaction that sets the
// grant: who may, and that read the grant adds passes from someone reading
// the mailbox now, by the one rule (the flag, and on a mailbox that has a key
// a grant at its current epoch). Act needing read, manage only for a member,
// the target's place in the workspace and the grant that comes with read are
// the repository's to check.
func mayGrantTx(ctx context.Context, tx *sql.Tx, p Principal, a account.Account, userID string, flags workspace.Flags) error {
	before, err := workspace.GrantTx(ctx, tx, a.ID, userID)
	if err != nil && !errors.Is(err, workspace.ErrNoGrant) {
		return err
	}
	added := workspace.Flags{
		Read: flags.Read && !before.Read, Act: flags.Act && !before.Act,
		Send: flags.Send && !before.Send, Manage: flags.Manage && !before.Manage,
	}
	if isOperator(p) {
		if added.Read || added.Act || added.Send {
			return errOperatorGrantsManage
		}
		return nil
	}
	if _, err := administratorTx(ctx, tx, p, a); err != nil {
		return err
	}
	if !added.Read {
		return nil
	}
	reads, err := workspace.ReadsNowTx(ctx, tx, a.ID, p.UserID)
	if err != nil {
		return err
	}
	if !reads {
		return errGrantReadNotHeld
	}
	return nil
}

// fromTeamInvite maps a failure making, revoking or redeeming a team invite.
func fromTeamInvite(err error, what string) error {
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, auth.ErrInviteNotFound):
		return errNoInvite
	case errors.Is(err, auth.ErrInviteInvalid):
		return E(CodeNotAuthorized,
			"that invite is not valid: it may have expired, been used, or be meant for another address", err)
	case errors.Is(err, auth.ErrInvalidEmail):
		return E(CodeBadRequest, "that is not a valid email address", err)
	case errors.Is(err, auth.ErrInvalidWorkspaceRole):
		return E(CodeBadRequest, "a role is owner, admin or member", err)
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrUserDisabled):
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	default:
		return fromWorkspace(err, what)
	}
}

func presentWorkspace(w workspace.Workspace, counts bool) Workspace {
	out := Workspace{
		ID: w.ID, Kind: string(w.Kind), Source: w.Source, Name: w.Name,
		Role: string(w.Role), Status: string(w.Status), CreatedAt: w.CreatedAt.Unix(),
	}
	if counts {
		members, mailboxes := w.Members, w.Mailboxes
		out.Members, out.Mailboxes = &members, &mailboxes
		out.Role, out.Status = "", ""
	}
	return out
}

func presentMember(m workspace.Member) Member {
	last := m.LastReaderOf
	if last == nil {
		last = []string{}
	}
	out := Member{
		UserID: m.UserID, Email: m.Email, Name: m.Name, Role: string(m.Role), Status: string(m.Status),
		PersonDisabled: m.PersonDisabled, LastOwner: m.LastOwner, LastReaderOf: last, JoinedAt: m.JoinedAt.Unix(),
		SealID: m.SealID,
	}
	if len(m.PublicKey) > 0 {
		out.PublicKey = b64(m.PublicKey)
	}
	return out
}

func presentGrant(g workspace.Grant) Grant {
	return Grant{
		AccountID: g.AccountID, UserID: g.UserID, Read: g.Read, Act: g.Act, Send: g.Send, Manage: g.Manage,
		GrantedBy: g.GrantedBy, UpdatedAt: g.UpdatedAt.Unix(), Sealed: g.Sealed,
	}
}

func presentTeamInvite(inv auth.Invite, url string) TeamInvite {
	return TeamInvite{
		ID: inv.ID, Email: inv.Email, WorkspaceID: inv.WorkspaceID, Role: string(inv.WorkspaceRole), URL: url,
		CreatedBy: inv.CreatedBy, CreatedAt: inv.CreatedAt.Unix(), ExpiresAt: inv.ExpiresAt.Unix(),
	}
}
