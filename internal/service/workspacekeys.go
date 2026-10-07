package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A workspace's API keys (docs/workspaces.md, "API keys").
//
// A tool — an AI assistant through the MCP server, a script against the REST
// API — reaches mail only through a key, and a key belongs to a workspace, as
// Wappie's do. Only an active owner or admin of the workspace, signed in to
// the console, creates one, lists the workspace's keys and revokes any of
// them; in a personal workspace, that is its person. A key never mints, lists
// or changes a key: a leaked key must not be a way to keep access after it is
// revoked.
//
// A key reaches exactly the mailboxes of its workspace it holds something on,
// each with its own read, act and send. What it holds stands on its own:
// whoever gave it, and the person who created the key, may lose their own
// access, and the key keeps its mailboxes until an owner or an admin takes
// them away or revokes it. So that no role reads mail through a key, read on a
// mailbox is given to a key only by an owner or an admin who reads that
// mailbox right then; act (where the key reads) and send are given by any
// owner or admin. A key never counts as a reader and never passes read.
//
// Creating a key is the agreement of the person who creates it to the key
// terms, in the words the console showed them, and the key records which
// revision those were: the terms say what a tool holding the key may do with
// what it holds, and no person's own consent to actions or sending is asked
// for a key's — except a person's key from before keys belonged to their
// workspace, created under terms that let it act only while its person
// allowed actions and never send, which goes on that way. The key stops when the person who created it leaves the workspace or
// is disabled or deleted on the instance, never when they are demoted or
// their membership is disabled.

// DefaultKeyTermsVersion is the revision of the text a person reads before
// creating a key, when the deployment configures no other
// (MAIL_CONSENT_VERSION_KEYS). A console creating one names the revision it
// showed, so a key is never taken as agreed to words the person was not
// shown. A key keeps the revision it was created under: changing the current
// one asks for it on the next key, and leaves the keys that exist working
// under theirs.
const DefaultKeyTermsVersion = config.DefaultKeyTermsVersion

// MaxWorkspaceKeys is how many live keys — neither revoked nor expired — one
// workspace may hold. Enough for every assistant and script a team uses; few
// enough that the list stays something its owners and admins can read. The
// persons' keys migration 0012 moved into a workspace are not counted: there
// may be more of them, and they expire on their own.
const MaxWorkspaceKeys = 20

// DefaultKeyDays is how long a key lives when the creator does not choose;
// KeyDays are the choices.
const DefaultKeyDays = 90

// KeyDays are the lifetimes, in days, a key may be given.
var KeyDays = []int{30, 90, 365}

// DailyKeySendLimit is how many sends one workspace key may start in a day.
const DailyKeySendLimit = 100

// maxKeySends bounds how many of a key's sends its workspace's owners and
// admins are listed, newest first: the records are kept 30 days.
const maxKeySends = 200

// WorkspaceKey is a key of a workspace, as its owners and admins, and the
// person who created it, see it. The secret is never here.
type WorkspaceKey struct {
	Prefix string     `json:"prefix"`
	Name   string     `json:"name"`
	Scope  auth.Scope `json:"scope"`
	// WorkspaceID is the key's workspace; absent for a key carried over
	// from a person's that reached several workspaces (CarriedOver).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// CarriedOver is a person's key the upgrade to workspace keys found
	// reaching mailboxes of several workspaces: it keeps exactly those,
	// gains none, and is revoked when its last one goes.
	CarriedOver bool `json:"carried_over,omitempty"`
	// Origin is "person" for a person's key made for chosen mailboxes and
	// "person-all" for one made for every mailbox of theirs, which the
	// upgrade to workspace keys moved into their workspace, giving the
	// second the mailboxes its person read then (and none linked since);
	// absent for a key made as keys are now.
	Origin string `json:"origin,omitempty"`
	// Mailboxes are what the key holds: in the workspace that lists it, or
	// in every workspace for the person who created it.
	Mailboxes []KeyMailbox `json:"mailboxes"`
	// OtherWorkspaces counts, for a carried-over key one workspace lists,
	// the other workspaces it holds mailboxes in, without naming them.
	OtherWorkspaces int `json:"other_workspaces,omitempty"`
	// CreatedBy is the person who created the key, who answers for it: its
	// creator leaving the workspace, or being disabled or deleted, revokes
	// it. Absent once they are deleted.
	CreatedBy  string `json:"created_by,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
	// Live is neither revoked nor expired.
	Live bool `json:"live"`
	// TermsVersion is the revision of the key terms its creator agreed to.
	TermsVersion string `json:"terms_version"`
	// Sends is whether the key can send at all: the send scope, on a server
	// whose keys may send, under the current key terms.
	Sends bool `json:"sends"`
}

// KeyMailbox is what a key holds on one mailbox.
type KeyMailbox struct {
	AccountID   string `json:"account_id"`
	WorkspaceID string `json:"workspace_id"`
	Read        bool   `json:"read"`
	Act         bool   `json:"act"`
	Send        bool   `json:"send"`
	// GrantedBy is who set it last: "usr_…" or "migration"; absent once
	// that person is deleted.
	GrantedBy string `json:"granted_by,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// CreatedWorkspaceKey is a new key and the only copy of its secret.
type CreatedWorkspaceKey struct {
	// Key is shown once. Only its Argon2id hash is stored.
	Key string `json:"key"`
	WorkspaceKey
}

// WorkspaceKeyRequest is a key an owner or an admin asks for.
type WorkspaceKeyRequest struct {
	Name string `json:"name"`
	// Scope is read, write (read and the actions on messages) or send
	// (write and sending, which a server may refuse: MAIL_KEYS_MAY_SEND).
	Scope string `json:"scope"`
	// TTLDays is one of KeyDays; zero is DefaultKeyDays.
	TTLDays int `json:"ttl_days,omitempty"`
	// TermsVersion must be the current revision of the key terms: the text
	// the person creating it was shown.
	TermsVersion string `json:"terms_version"`
	// Mailboxes are what the key holds from the start, each on a mailbox of
	// the workspace. None makes a key that reaches nothing yet.
	Mailboxes []KeyMailboxRequest `json:"mailboxes,omitempty"`
}

// KeyMailboxRequest is what a key is given on one mailbox when it is created.
type KeyMailboxRequest struct {
	AccountID string `json:"account_id"`
	Read      bool   `json:"read"`
	Act       bool   `json:"act"`
	Send      bool   `json:"send"`
}

// KeyAccessRequest sets exactly what a key holds on a mailbox. Every flag is
// required, so leaving one out never takes it away by accident; all false is
// refused, since taking the mailbox out of the key is how a hold goes.
type KeyAccessRequest struct {
	Read *bool `json:"read"`
	Act  *bool `json:"act"`
	Send *bool `json:"send"`
}

// Errors of the key routes.
var (
	errTooManyKeys = Ef(CodeConflict, nil,
		"this workspace already has %d live keys; revoke one it no longer uses first", MaxWorkspaceKeys)
	errNoSuchKey   = E(CodeNotFound, "no such api key", nil)
	errKeyCreators = E(CodeNotAuthorized, "only an owner or an admin of the workspace creates, lists and revokes its API keys", nil)
	errKeysCreated = E(CodeBadRequest, "API keys are created in a workspace by its owners and admins: "+
		"POST /v1/workspaces/{id}/apikeys", nil)
	errKeysMayNotSend = E(CodeNotAuthorized, "this server's API keys do not send email", nil)
	errKeyReadNotHeld = E(CodeNotAuthorized,
		"you can give a key read on a mailbox only while you read it yourself", nil)
)

// keyAdminOf reads a workspace a person asks about its keys, and checks that
// they may: a person signed in, an active owner or admin of it. A workspace
// they are not an active member of does not exist for them.
func (s *Service) keyAdminOf(ctx context.Context, p Principal, workspaceID string) (workspace.Workspace, error) {
	if err := requireSession(p); err != nil {
		return workspace.Workspace{}, err
	}
	w, me, err := s.workspaceOf(ctx, p, workspaceID)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if w.Kind == workspace.KindOperator || !adminOf(me) {
		return workspace.Workspace{}, errKeyCreators
	}
	return w, nil
}

// keyAdminTx re-reads, inside a write's transaction, that the caller is still
// an active owner or admin of the workspace.
func keyAdminTx(ctx context.Context, tx *sql.Tx, p Principal, workspaceID string) error {
	me, err := callerTx(ctx, tx, p, workspaceID)
	if err != nil {
		return err
	}
	if !adminOf(me) {
		return errKeyCreators
	}
	return nil
}

// readsNowTx reports, inside a write's transaction, whether the caller reads
// a mailbox right now: a grant with read, as an active member active on the
// instance (callerTx has checked the membership).
func readsNowTx(ctx context.Context, tx *sql.Tx, p Principal, accountID string) (bool, error) {
	g, err := workspace.GrantTx(ctx, tx, accountID, p.UserID)
	switch {
	case errors.Is(err, workspace.ErrNoGrant):
		return false, nil
	case err != nil:
		return false, err
	}
	return g.Read, nil
}

// ListWorkspaceKeys lists a workspace's keys for its owners and admins:
// revoked and expired ones too, which are part of the answer to "what could
// have reached this mailbox", live ones first, each group newest first, with
// what each holds there. A key carried over from a person's that holds a
// mailbox of the workspace is listed with that workspace's mailboxes.
func (s *Service) ListWorkspaceKeys(ctx context.Context, p Principal, workspaceID string) ([]WorkspaceKey, error) {
	if _, err := s.keyAdminOf(ctx, p, workspaceID); err != nil {
		return nil, err
	}
	keys, err := s.keys.ListIn(ctx, workspaceID)
	if err != nil {
		return nil, E(CodeInternal, "listing keys failed", err)
	}
	return s.presentKeys(keys), nil
}

// CreateWorkspaceKey issues a key of a workspace: for an active owner or
// admin of it, signed in, agreeing to the current key terms. Read on a
// mailbox is given only where they read it themselves; act where the key
// reads, with the write scope or more; send with the send scope, which a
// server whose keys may not send refuses. It lasts what they chose; the
// person who created it leaving the workspace, or being disabled or deleted,
// revokes it sooner.
func (s *Service) CreateWorkspaceKey(ctx context.Context, p Principal, workspaceID string, req WorkspaceKeyRequest) (CreatedWorkspaceKey, error) {
	w, err := s.keyAdminOf(ctx, p, workspaceID)
	if err != nil {
		return CreatedWorkspaceKey{}, err
	}
	if req.TermsVersion != s.consent.Keys {
		// A console showing an older text, or none: the person has not
		// seen what they would be agreeing to.
		return CreatedWorkspaceKey{}, Ef(CodeConflict, nil,
			"creating a key must name the current key terms version, %s; reload and review them", s.consent.Keys)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxKeyName {
		return CreatedWorkspaceKey{}, Ef(CodeBadRequest, nil,
			"a key needs a name of at most %d characters, so it can be recognised later", maxKeyName)
	}
	scope := auth.Scope(req.Scope)
	switch {
	case scope == auth.ScopeSend && s.keysMayNotSend:
		return CreatedWorkspaceKey{}, E(CodeBadRequest,
			"scope must be read or write: this server's API keys do not send email", nil)
	case scope != auth.ScopeRead && scope != auth.ScopeWrite && scope != auth.ScopeSend:
		return CreatedWorkspaceKey{}, E(CodeBadRequest, "scope must be read, write or send", nil)
	}
	days := req.TTLDays
	if days == 0 {
		days = DefaultKeyDays
	}
	if !slices.Contains(KeyDays, days) {
		return CreatedWorkspaceKey{}, E(CodeBadRequest, "ttl_days must be 30, 90 or 365", nil)
	}
	var grants []workspace.KeyGrant
	for _, m := range req.Mailboxes {
		id := strings.TrimSpace(m.AccountID)
		if id == "" || slices.ContainsFunc(grants, func(g workspace.KeyGrant) bool { return g.AccountID == id }) {
			return CreatedWorkspaceKey{}, E(CodeBadRequest, "each mailbox is named once, by its account_id", nil)
		}
		flags := workspace.Flags{Read: m.Read, Act: m.Act, Send: m.Send}
		if err := checkKeyRequest(scope, flags, s.keysMayNotSend); err != nil {
			return CreatedWorkspaceKey{}, err
		}
		grants = append(grants, workspace.KeyGrant{AccountID: id, Flags: flags})
	}
	check := func(tx *sql.Tx) error {
		if err := keyAdminTx(ctx, tx, p, w.ID); err != nil {
			return err
		}
		for _, g := range grants {
			ws, err := workspace.MailboxWorkspaceTx(ctx, tx, g.AccountID)
			switch {
			case errors.Is(err, workspace.ErrNoMailbox):
				return errNoAccount
			case err != nil:
				return err
			case ws != w.ID:
				return errNoAccount
			}
			if !g.Read {
				continue
			}
			reads, err := readsNowTx(ctx, tx, p, g.AccountID)
			if err != nil {
				return err
			}
			if !reads {
				return errKeyReadNotHeld
			}
		}
		return nil
	}
	if err := s.precheck(ctx, check); err != nil {
		return CreatedWorkspaceKey{}, fromWorkspace(err, "reading the workspace failed")
	}

	secret, key, err := s.keys.Issue(ctx, auth.NewKeyRequest{
		Name: name, Scope: scope, WorkspaceID: w.ID, Mailboxes: grants,
		TTL: time.Duration(days) * 24 * time.Hour, TermsVersion: req.TermsVersion, CreatedBy: p.Actor(),
		MaxLive: MaxWorkspaceKeys, Check: check,
	})
	switch {
	case errors.Is(err, auth.ErrTooManyKeys):
		return CreatedWorkspaceKey{}, errTooManyKeys
	case errors.Is(err, auth.ErrUnknownAccount):
		// Removed between the check and the insert.
		return CreatedWorkspaceKey{}, errNoAccount
	case errors.Is(err, auth.ErrNoWorkspace):
		return CreatedWorkspaceKey{}, errNoWorkspace
	case err != nil:
		return CreatedWorkspaceKey{}, fromWorkspace(err, "issuing the key failed")
	}
	s.accessChanged()
	s.log.Info("api key created", "workspace", w.ID, "by", p.UserID, "key", key.Prefix, "scope", string(key.Scope),
		"mailboxes", len(key.Mailboxes), "terms", key.TermsVersion)
	return CreatedWorkspaceKey{Key: secret, WorkspaceKey: s.presentWorkspaceKey(key)}, nil
}

// checkKeyRequest is what flags given to a key must be, whoever gives them:
// at least one, act only with read and the write scope or more, send only
// with the send scope on a server whose keys may send.
func checkKeyRequest(scope auth.Scope, f workspace.Flags, keysMayNotSend bool) error {
	switch {
	case !f.Read && !f.Act && !f.Send:
		return E(CodeBadRequest, "a key needs at least one of read, act and send on a mailbox it is given", nil)
	case f.Act && !f.Read:
		return E(CodeBadRequest, "act needs read", nil)
	case f.Send && keysMayNotSend:
		return errKeysMayNotSend
	case f.Act && !scope.Covers(auth.ScopeWrite), f.Send && !scope.Covers(auth.ScopeSend):
		return E(CodeBadRequest, "act needs a key of the write or send scope, and send a key of the send scope", nil)
	}
	return nil
}

// RevokeWorkspaceKey revokes a key of a workspace: any owner or admin of it,
// signed in, whoever created the key. A key carried over from a person's
// loses this workspace's mailboxes instead, and is revoked with its last.
// Revoking a key already revoked is not an error.
func (s *Service) RevokeWorkspaceKey(ctx context.Context, p Principal, workspaceID, prefix string) error {
	if _, err := s.keyAdminOf(ctx, p, workspaceID); err != nil {
		return err
	}
	err := s.keys.RevokeIn(ctx, workspaceID, prefix, func(tx *sql.Tx) error {
		return keyAdminTx(ctx, tx, p, workspaceID)
	})
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return errNoSuchKey
	case err != nil:
		return fromWorkspace(err, "revoking the key failed")
	}
	s.accessChanged()
	s.log.Info("api key revoked", "workspace", workspaceID, "by", p.UserID, "key", prefix)
	return nil
}

// SetKeyAccess sets exactly what a key of the workspace holds on one of its
// mailboxes: an owner or an admin of it, signed in. Read the key does not
// hold yet only from one who reads the mailbox right then; act where the key
// will read, with the write scope or more; send with the send scope, on a
// server whose keys may send. Taking flags away needs nothing held. A key
// carried over from a person's may only lose flags.
func (s *Service) SetKeyAccess(ctx context.Context, p Principal, workspaceID, prefix, accountID string, req KeyAccessRequest) (KeyMailbox, error) {
	if _, err := s.keyAdminOf(ctx, p, workspaceID); err != nil {
		return KeyMailbox{}, err
	}
	if req.Read == nil || req.Act == nil || req.Send == nil {
		return KeyMailbox{}, E(CodeBadRequest,
			"read, act and send are all required: what the key holds is set to exactly them", nil)
	}
	flags := workspace.Flags{Read: *req.Read, Act: *req.Act, Send: *req.Send}
	if !flags.Read && !flags.Act && !flags.Send {
		return KeyMailbox{}, E(CodeBadRequest,
			"a key needs at least one of read, act and send on a mailbox; DELETE takes the mailbox out of it", nil)
	}
	if flags.Send && s.keysMayNotSend {
		return KeyMailbox{}, errKeysMayNotSend
	}
	held, err := s.workspaces.SetKeyAccess(ctx, prefix, accountID, flags, p.Actor(),
		func(tx *sql.Tx, key workspace.KeyRef, mailboxWorkspace string, before workspace.Flags) error {
			if err := s.keyTargetTx(ctx, tx, p, workspaceID, key, mailboxWorkspace, before); err != nil {
				return err
			}
			if !flags.Read || before.Read {
				return nil
			}
			reads, err := readsNowTx(ctx, tx, p, accountID)
			if err != nil {
				return err
			}
			if !reads {
				return errKeyReadNotHeld
			}
			return nil
		})
	if err != nil {
		return KeyMailbox{}, fromWorkspace(err, "setting what the key holds failed")
	}
	s.accessChanged()
	return presentKeyMailbox(held), nil
}

// RevokeKeyAccess takes a mailbox of the workspace out of a key: everything it
// held there. An owner or an admin of it, signed in, whoever gave it. A key
// carried over from a person's left with none is revoked.
func (s *Service) RevokeKeyAccess(ctx context.Context, p Principal, workspaceID, prefix, accountID string) error {
	if _, err := s.keyAdminOf(ctx, p, workspaceID); err != nil {
		return err
	}
	err := s.workspaces.DropKeyAccess(ctx, prefix, accountID,
		func(tx *sql.Tx, key workspace.KeyRef, mailboxWorkspace string, before workspace.Flags) error {
			return s.keyTargetTx(ctx, tx, p, workspaceID, key, mailboxWorkspace, before)
		})
	if err != nil {
		return fromWorkspace(err, "taking the mailbox out of the key failed")
	}
	s.accessChanged()
	return nil
}

// keyTargetTx is who may change what a key holds on a mailbox, re-read in the
// write's transaction: an active owner or admin of the workspace asked about,
// for a key of that workspace and a mailbox of it, or a carried-over key and
// a mailbox of the workspace it holds. Anything else does not exist for them.
func (s *Service) keyTargetTx(ctx context.Context, tx *sql.Tx, p Principal, workspaceID string, key workspace.KeyRef,
	mailboxWorkspace string, before workspace.Flags,
) error {
	if err := keyAdminTx(ctx, tx, p, workspaceID); err != nil {
		return err
	}
	switch {
	case key.CarriedOver():
		if mailboxWorkspace != workspaceID || !before.Any() {
			return errNoSuchKey
		}
	case key.WorkspaceID != workspaceID:
		return errNoSuchKey
	case mailboxWorkspace != workspaceID:
		return errNoAccount
	}
	return nil
}

// ListKeySends lists a key's sends from the workspace's mailboxes, newest
// first: the record of each, never a subject or an address. For the
// workspace's owners and admins, signed in.
func (s *Service) ListKeySends(ctx context.Context, p Principal, workspaceID, prefix string) ([]SendStatus, error) {
	if _, err := s.keyAdminOf(ctx, p, workspaceID); err != nil {
		return nil, err
	}
	key, err := s.keys.Get(ctx, prefix)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return nil, errNoSuchKey
	case err != nil:
		return nil, E(CodeInternal, "reading the key failed", err)
	}
	if key.WorkspaceID != workspaceID &&
		(key.WorkspaceID != "" || !slices.ContainsFunc(key.Mailboxes, func(m workspace.KeyAccess) bool {
			return m.WorkspaceID == workspaceID
		})) {
		return nil, errNoSuchKey
	}
	rows, err := s.store.SendsBy(ctx, "key:"+prefix, workspaceID, maxKeySends)
	if err != nil {
		return nil, E(CodeInternal, "listing the key's sends failed", err)
	}
	out := make([]SendStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentSendStatus(row))
	}
	return out, nil
}

// ListMyAPIKeys lists the keys the person signed in created, in every
// workspace, with everything each holds: revoked and expired ones too, live
// ones first, each group newest first. They may revoke any of them.
func (s *Service) ListMyAPIKeys(ctx context.Context, p Principal) ([]WorkspaceKey, error) {
	if err := requireSession(p); err != nil {
		return nil, err
	}
	keys, err := s.keys.ListCreatedBy(ctx, p.UserID)
	if err != nil {
		return nil, E(CodeInternal, "listing keys failed", err)
	}
	return s.presentKeys(keys), nil
}

// CreateMyAPIKey is refused: a key is created in a workspace, by its owners
// and admins (CreateWorkspaceKey). Kept so that a console of before says why.
func (s *Service) CreateMyAPIKey(_ context.Context, p Principal) error {
	if err := requireSession(p); err != nil {
		return err
	}
	return errKeysCreated
}

// RevokeMyAPIKey revokes a key the person signed in created, in whichever
// workspace. Anybody else's is not found; revoking one already revoked is not
// an error.
func (s *Service) RevokeMyAPIKey(ctx context.Context, p Principal, prefix string) error {
	if err := requireSession(p); err != nil {
		return err
	}
	switch err := s.keys.RevokeCreatedBy(ctx, prefix, p.UserID); {
	case errors.Is(err, auth.ErrNotFound):
		return errNoSuchKey
	case err != nil:
		return E(CodeInternal, "revoking the key failed", err)
	}
	s.accessChanged()
	s.log.Info("api key revoked", "by", p.UserID, "key", prefix)
	return nil
}

// presentKeys renders keys for their workspace's owners and admins, or the
// person who created them.
func (s *Service) presentKeys(keys []auth.Key) []WorkspaceKey {
	out := make([]WorkspaceKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.presentWorkspaceKey(k))
	}
	return out
}

func (s *Service) presentWorkspaceKey(k auth.Key) WorkspaceKey {
	mailboxes := make([]KeyMailbox, 0, len(k.Mailboxes))
	for _, m := range k.Mailboxes {
		mailboxes = append(mailboxes, presentKeyMailbox(m))
	}
	return WorkspaceKey{
		Prefix: k.Prefix, Name: k.Name, Scope: k.Scope, WorkspaceID: k.WorkspaceID, CarriedOver: k.WorkspaceID == "",
		Origin: k.Origin, Mailboxes: mailboxes, OtherWorkspaces: k.OtherWorkspaces, CreatedBy: k.CreatedBy,
		CreatedAt: unixOrZero(k.CreatedAt), ExpiresAt: unixOrZero(k.ExpiresAt),
		LastUsedAt: unixOrZero(k.LastUsedAt), RevokedAt: unixOrZero(k.RevokedAt), Live: k.Live(s.now()),
		TermsVersion: k.TermsVersion, Sends: s.keySends(k.Scope, k.Origin),
	}
}

func presentKeyMailbox(m workspace.KeyAccess) KeyMailbox {
	return KeyMailbox{
		AccountID: m.AccountID, WorkspaceID: m.WorkspaceID, Read: m.Read, Act: m.Act, Send: m.Send,
		GrantedBy: m.GrantedBy, UpdatedAt: unixOrZero(m.UpdatedAt),
	}
}

// keySends reports whether a workspace key of this scope and origin may send
// at all on this server: the send scope, on a server whose keys may send. A
// person's key the upgrade to workspace keys carried over never sends: the
// terms its person agreed to said it could not.
func (s *Service) keySends(scope auth.Scope, origin string) bool {
	return scope.Covers(auth.ScopeSend) && !s.keysMayNotSend && origin == ""
}

// earlierKey reports whether a workspace key is a person's key the upgrade to
// workspace keys carried over, created under key terms that said it acted on
// its person's mailboxes only while they allowed actions, and never sent: it
// goes on that way, as the person who created it.
func earlierKey(p Principal) bool { return p.IsWorkspaceKey() && p.Origin != "" }

// keyStillLive asks again, right before a workspace key's request touches a
// mail server, whether the key still works: not revoked, not expired, its
// scope the same. A row read; the secret was proved already.
func (s *Service) keyStillLive(ctx context.Context, p Principal) error {
	if !p.IsWorkspaceKey() || s.keys == nil {
		return nil
	}
	if err := s.keys.Recheck(ctx, p); err != nil {
		return fromCredential(err)
	}
	return nil
}
