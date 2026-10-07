// Package auth issues and verifies the credentials that guard every transport:
// API keys, and the sessions of people signed in to the web console.
//
// One header for both: every transport takes `Authorization: Bearer`, never a
// cookie and never a token in a URL, so there is one place where a caller is
// identified and no ambient credential a browser would attach by itself. The
// two kinds are told apart by shape — a key is "<prefix>.<secret>", a session
// token has no dot — which keeps a session lookup a SHA-256 away from the
// Argon2id a key costs. Loopback gets no shortcut: a daemon that can read
// several mailboxes and send mail as their owner should not trust a caller
// merely for being local.
package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// MaxLifetime bounds how long a key may live. A credential that never expires
// is one nobody ever notices leaking.
const MaxLifetime = 365 * 24 * time.Hour

// lastUsedResolution keeps the bookkeeping write off the hot path: recording
// the exact second of every request would make each read a write on the single
// writer connection.
const lastUsedResolution = time.Minute

// Kind is which credential a caller presented.
type Kind string

const (
	// KindKey is an API key. The zero Kind is read as a key too, because
	// that is all a caller could be before sessions existed.
	KindKey Kind = "key"
	// KindSession is a person signed in to the console.
	KindSession Kind = "session"
)

// Principal is an authenticated caller.
type Principal struct {
	Kind      Kind
	KeyPrefix string
	// SessionID identifies a console session. Safe to log, unlike the token.
	SessionID string
	// UserID is the person signed in. Empty for every key: a key belongs to
	// a workspace and acts as no person.
	UserID   string
	UserRole Role
	Scope    Scope
	// WorkspaceID is, for a key, the workspace it belongs to: the operator
	// workspace for an instance key, which reaches that workspace's
	// mailboxes; a personal workspace or a team for a workspace key, which
	// reaches what it holds there. Empty for a key carried over from a
	// person's that reached several workspaces (migration 0012), which
	// reaches what it still holds in each.
	WorkspaceID string
	// AccountIDs restricts an instance key to some of the operator
	// workspace's mailboxes. Empty means every one of them. A workspace key
	// is never restricted this way: what it holds is read live.
	AccountIDs []string
	// Tool marks a key presented to the MCP server: a tool, such as an AI
	// assistant, acting with it. It changes what nothing sees: a key reaches
	// the same mailboxes over REST and MCP alike.
	Tool bool
	// TermsVersion is, for a workspace key, the revision of the key terms the
	// person who created it agreed to; empty for an instance key.
	TermsVersion string
	// CreatedBy is, for a key, who created it: "usr_…" for a workspace key,
	// "key:<prefix>" or "cli" for an instance key; empty once that person is
	// deleted.
	CreatedBy string
	// Origin is, for a workspace key, OriginPerson or OriginPersonAll when it
	// is a person's key migration 0012 carried over, and empty for a key made
	// as keys are now.
	Origin string
}

// IsSession reports whether the caller is a person signed in to the console.
func (p Principal) IsSession() bool { return p.Kind == KindSession }

// IsInstance reports whether the caller is an instance key: a key of the
// operator workspace, which is what the CLI holds.
func (p Principal) IsInstance() bool { return !p.IsSession() && p.WorkspaceID == workspace.OperatorID }

// IsWorkspaceKey reports whether the caller is a key of a workspace other
// than the operator's, or one carried over from a person's: a key that
// reaches exactly what it holds on mailboxes.
func (p Principal) IsWorkspaceKey() bool { return !p.IsSession() && !p.IsInstance() }

// Actor names the caller for audit columns: "usr_…" for a person, "key:<prefix>"
// for a key.
func (p Principal) Actor() string {
	if p.IsSession() {
		return p.UserID
	}
	return "key:" + p.KeyPrefix
}

// MayAccess reports whether the principal may touch an account, as far as an
// instance key's restriction goes.
func (p Principal) MayAccess(accountID string) bool {
	if len(p.AccountIDs) == 0 {
		return true
	}
	for _, id := range p.AccountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

// Origins of a key, beside one made as it is now.
const (
	// OriginPerson is a person's key made for chosen mailboxes, which
	// migration 0012 moved into its workspace.
	OriginPerson = "person"
	// OriginPersonAll is a person's key made for every mailbox of theirs,
	// which migration 0012 gave the mailboxes its person read then.
	OriginPersonAll = "person-all"
)

// Key is a stored key, as listed. It never carries the secret: that exists
// once, in the response to the call that created it.
type Key struct {
	Prefix string
	Name   string
	Scope  Scope
	// WorkspaceID is the workspace the key belongs to; empty for a key
	// carried over from a person's (migration 0012).
	WorkspaceID string
	// AccountIDs restricts an instance key; Restricted records that it was
	// made for chosen accounts. With none of them left, AccountIDs is empty
	// like a key for every account's, and the key has been revoked.
	AccountIDs []string
	Restricted bool
	// Mailboxes are what a workspace key holds: in every workspace, or, as
	// one workspace lists its keys, in that one. OtherWorkspaces counts the
	// other workspaces a carried-over key holds mailboxes in, then.
	Mailboxes       []workspace.KeyAccess
	OtherWorkspaces int
	// Origin is "" for a key made as keys are now, OriginPerson or
	// OriginPersonAll for a person's key migration 0012 carried over.
	Origin     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time
	// TermsVersion is the revision of the key terms the person who created
	// a workspace key agreed to; empty for an instance key.
	TermsVersion string
	// CreatedBy is who created the key: "usr_…", "key:<prefix>" or "cli";
	// empty once that person is deleted.
	CreatedBy string
}

// Revoked reports whether the key has been revoked.
func (k Key) Revoked() bool { return !k.RevokedAt.IsZero() }

// Expired reports whether the key has expired at the given time.
func (k Key) Expired(now time.Time) bool { return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) }

// Live reports whether the key works at the given time.
func (k Key) Live(now time.Time) bool { return !k.Revoked() && !k.Expired(now) }

// IsInstance reports whether the key is an instance key.
func (k Key) IsInstance() bool { return k.WorkspaceID == workspace.OperatorID }

// Keys is the API-key repository.
type Keys struct {
	store *store.Store
	now   func() time.Time
}

// NewKeys builds the repository.
func NewKeys(s *store.Store) *Keys { return &Keys{store: s, now: s.Now} }

// NewKeysWithClock builds the repository with an injected clock, for tests.
func NewKeysWithClock(s *store.Store, now func() time.Time) *Keys { return &Keys{store: s, now: now} }

// NewKeyRequest describes a key to issue.
type NewKeyRequest struct {
	Name  string
	Scope Scope
	// WorkspaceID is the workspace the key belongs to. Empty, or the
	// operator workspace, issues an instance key; any other a workspace key,
	// whose scope is read, write or send.
	WorkspaceID string
	// AccountIDs restricts an instance key to some of the operator
	// workspace's mailboxes. Refused for a workspace key.
	AccountIDs []string
	// Mailboxes are what a workspace key holds from the start, each on a
	// mailbox of its workspace. Who may give what is Check's to decide.
	Mailboxes []workspace.KeyGrant
	// TTL is how long the key lives. Zero means MaxLifetime.
	TTL time.Duration
	// TermsVersion is the revision of the key terms the person creating a
	// workspace key was shown and agreed to; recorded on the key.
	TermsVersion string
	// CreatedBy names who created the key, for the record: "usr_…",
	// "key:<prefix>" or "cli". Also who gave its mailboxes.
	CreatedBy string
	// MaxLive, when positive, bounds how many live keys (neither revoked nor
	// expired) made as keys are now the workspace may hold, this one
	// included. The persons' keys migration 0012 moved in are not counted:
	// each person could hold 20 before, so a team may hold many more of
	// them, and they expire on their own. Counted in the transaction that
	// inserts, so two creations at once cannot both pass.
	MaxLive int
	// Check, when set, runs first in the transaction that issues: the
	// service's rule about who may create the key and give it what.
	Check func(*sql.Tx) error
}

// Errors issuing a key can report about its request.
var (
	// ErrUnknownAccount is a restriction or a mailbox naming an account that
	// does not exist, or one outside the workspace the key is for: for an
	// instance key the operator workspace, which is all it ever reaches.
	ErrUnknownAccount = errors.New("auth: a key cannot be given an account that does not exist")
	// ErrUserNotFound is a user id nobody has.
	ErrUserNotFound = errors.New("auth: no such user")
	// ErrNoWorkspace is a workspace that does not exist.
	ErrNoWorkspace = errors.New("auth: no such workspace")
	// ErrTooManyKeys is a workspace that already holds NewKeyRequest.MaxLive
	// live keys.
	ErrTooManyKeys = errors.New("auth: this workspace already holds as many live keys as allowed")
	// ErrWorkspaceKeyScope is a workspace key asked for with the admin
	// scope, or restricted to accounts: what only an instance key is.
	ErrWorkspaceKeyScope = errors.New("auth: a workspace key has the read, write or send scope and holds mailboxes")
)

// Issue creates a key and returns the only copy of its secret.
func (k *Keys) Issue(ctx context.Context, req NewKeyRequest) (secret string, key Key, err error) {
	if !req.Scope.Valid() {
		return "", Key{}, fmt.Errorf("auth: unknown scope %q", req.Scope)
	}
	if req.Name == "" {
		return "", Key{}, errors.New("auth: a key needs a name, so it can be recognised later")
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = MaxLifetime
	}
	if ttl > MaxLifetime {
		return "", Key{}, fmt.Errorf("auth: a key may live at most %v", MaxLifetime)
	}
	ws := req.WorkspaceID
	if ws == "" {
		ws = workspace.OperatorID
	}
	instance := ws == workspace.OperatorID
	if !instance && (req.Scope == ScopeAdmin || len(req.AccountIDs) > 0) {
		return "", Key{}, ErrWorkspaceKeyScope
	}
	if instance && len(req.Mailboxes) > 0 {
		return "", Key{}, ErrUnknownAccount
	}

	presented, prefix, hash, err := Generate()
	if err != nil {
		return "", Key{}, err
	}
	now := k.now().UTC().Truncate(time.Second)
	out := Key{
		Prefix:       prefix,
		Name:         req.Name,
		Scope:        req.Scope,
		WorkspaceID:  ws,
		AccountIDs:   req.AccountIDs,
		Restricted:   len(req.AccountIDs) > 0,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		TermsVersion: req.TermsVersion,
		CreatedBy:    req.CreatedBy,
	}

	err = k.store.Write(ctx, func(tx *sql.Tx) error {
		if req.Check != nil {
			if err := req.Check(tx); err != nil {
				return err
			}
		}
		if req.MaxLive > 0 {
			var live int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM api_keys WHERE workspace_id = ? AND origin = '' AND revoked_at = 0 AND expires_at > ?`,
				ws, now.Unix()).Scan(&live); err != nil {
				return fmt.Errorf("auth: count live keys: %w", err)
			}
			if live >= req.MaxLive {
				return ErrTooManyKeys
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, terms_version,
			                      created_by, restricted)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			out.Prefix, hash, out.Name, string(out.Scope), out.CreatedAt.Unix(), out.ExpiresAt.Unix(), ws,
			out.TermsVersion, out.CreatedBy, out.Restricted,
		)
		if store.IsForeignKey(err) {
			return ErrNoWorkspace
		}
		if err != nil {
			return fmt.Errorf("auth: insert key: %w", err)
		}
		for _, accountID := range req.AccountIDs {
			// An instance key reaches the operator workspace's mailboxes
			// and nothing else: one restricted to a person's mailbox would
			// reach nothing at all.
			var operator int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ? AND workspace_id = ?`,
				accountID, workspace.OperatorID).Scan(&operator); err != nil {
				return fmt.Errorf("auth: check account %s: %w", accountID, err)
			}
			if operator == 0 {
				return ErrUnknownAccount
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO api_key_accounts(key_prefix, account_id) VALUES (?, ?)`, out.Prefix, accountID)
			if store.IsForeignKey(err) {
				return ErrUnknownAccount
			}
			if err != nil {
				return fmt.Errorf("auth: restrict key to account %s: %w", accountID, err)
			}
		}
		for _, grant := range req.Mailboxes {
			err := workspace.PutKeyAccessTx(ctx, tx, out.Prefix, ws, grant, req.CreatedBy, now)
			if errors.Is(err, workspace.ErrNoMailbox) {
				return ErrUnknownAccount
			}
			if err != nil {
				return err
			}
		}
		if len(req.Mailboxes) > 0 {
			var err error
			out.Mailboxes, err = keyMailboxesTx(ctx, tx, out.Prefix)
			return err
		}
		return nil
	})
	if err != nil {
		return "", Key{}, err
	}
	return presented, out, nil
}

// keyMailboxesTx reads what a key holds, inside the transaction that gave it.
func keyMailboxesTx(ctx context.Context, tx *sql.Tx, prefix string) ([]workspace.KeyAccess, error) {
	rows, err := tx.QueryContext(ctx, `SELECT x.account_id, x.workspace_id, x.read, x.act, x.send, x.granted_by,
		       x.created_at, x.updated_at
		  FROM key_access x JOIN accounts a ON a.id = x.account_id
		 WHERE x.key_prefix = ? ORDER BY x.created_at, a.created_at, a.rowid`, prefix)
	if err != nil {
		return nil, fmt.Errorf("auth: read what the key holds: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []workspace.KeyAccess
	for rows.Next() {
		a := workspace.KeyAccess{KeyPrefix: prefix}
		var created, updated int64
		if err := rows.Scan(&a.AccountID, &a.WorkspaceID, &a.Read, &a.Act, &a.Send, &a.GrantedBy,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("auth: read what the key holds: %w", err)
		}
		a.CreatedAt, a.UpdatedAt = unixOrZero(created), unixOrZero(updated)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: read what the key holds: %w", err)
	}
	return out, nil
}

// Gate decides whether a key check that cannot succeed may still run. It is
// asked before a presented key that matches no stored key is hashed against
// dummyHash, and says how long to wait when the answer is no. A nil Gate
// admits everything.
type Gate func() (ok bool, retryAfter time.Duration)

// ThrottledError is a key check refused before it ran: the caller had already
// presented too many keys that match nothing.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return "auth: too many keys that match nothing; wait before trying again"
}

// maxConcurrentKeyHashes bounds how much memory key checks can hold: four at
// 19 MiB is 76 MiB, however many addresses the requests come from. A key check
// is a few milliseconds, so thirty-two waiting is well under a second.
const (
	maxConcurrentKeyHashes = 4
	maxKeyHashWaiters      = 32
)

var keySlots = newSlots(maxConcurrentKeyHashes, maxKeyHashWaiters)

// verifyKey is verifySecret behind the key-hashing slots.
func verifyKey(ctx context.Context, secret, phc string) (bool, error) {
	release, err := keySlots.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	return verifySecret(secret, phc), nil
}

// Authenticate verifies a presented key and returns the caller it stands for.
//
// The shape of this function is the security property. It performs one
// Argon2id derivation whether or not the prefix exists, and it reports one
// error for every way of failing.
//
// With one exception, which unknown decides. A key whose prefix matches
// nothing can only fail, so a caller that has already presented too many of
// those is refused before the derivation, with a ThrottledError. That refusal
// does say the prefix is unknown, where the derivation would not have; it is
// the price of not paying 19 MiB for every random prefix a script invents, and
// it is a small one. A prefix is a selector that appears in logs, not a secret,
// and knowing one brings a caller no closer to the 256 bits behind it.
func (k *Keys) Authenticate(ctx context.Context, presented string, unknown Gate) (Principal, error) {
	prefix, secret, ok := SplitKey(presented)
	if !ok {
		// Still pay the cost: a malformed key must not be distinguishable
		// from a well-formed one that is simply wrong.
		return Principal{}, miss(ctx, presented, unknown)
	}

	var (
		hash      string
		scopeStr  string
		expiresAt int64
		revokedAt int64
		terms     string
		ws        string
		createdBy string
		origin    string
	)
	err := k.store.Reader().QueryRowContext(ctx,
		`SELECT hash, scope, expires_at, revoked_at, terms_version, coalesce(workspace_id, ''), created_by, origin
		   FROM api_keys WHERE prefix = ?`, prefix,
	).Scan(&hash, &scopeStr, &expiresAt, &revokedAt, &terms, &ws, &createdBy, &origin)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Principal{}, miss(ctx, secret, unknown)
	case err != nil:
		return Principal{}, fmt.Errorf("auth: look up key: %w", err)
	}

	match, err := verifyKey(ctx, secret, hash)
	if err != nil {
		return Principal{}, err
	}
	if !match {
		return Principal{}, ErrInvalidKey
	}
	// Checked after the comparison so that a revoked or expired key costs the
	// same as a live one with the wrong secret. Disabling, deleting or
	// removing the person who created a workspace key revokes it in the
	// same transaction (workspace.RemoveMember, Users.Disable, DeleteTx).
	now := k.now()
	if revokedAt != 0 || (expiresAt != 0 && !now.Before(time.Unix(expiresAt, 0))) {
		return Principal{}, ErrInvalidKey
	}
	scope, err := ParseScope(scopeStr)
	if err != nil {
		return Principal{}, ErrInvalidKey
	}
	p := Principal{
		Kind: KindKey, KeyPrefix: prefix, Scope: scope, WorkspaceID: ws, TermsVersion: terms, CreatedBy: createdBy,
		Origin: origin,
	}
	if p.IsInstance() {
		if p.AccountIDs, err = k.accountsFor(ctx, prefix); err != nil {
			return Principal{}, err
		}
	}
	k.touch(ctx, prefix, now)
	return p, nil
}

// miss answers a presented key that matches no stored key: after one
// derivation against dummyHash, so it takes as long as a wrong secret does,
// unless the gate refuses first. No real key ever gets here, so the gate can
// never turn a good one away.
func miss(ctx context.Context, secret string, unknown Gate) error {
	if unknown != nil {
		if ok, wait := unknown(); !ok {
			return &ThrottledError{RetryAfter: wait}
		}
	}
	if _, err := verifyKey(ctx, secret, dummyHash); err != nil {
		return err
	}
	return ErrInvalidKey
}

// Recheck re-reads the mutable parts of a key that has already authenticated.
//
// Handlers call this immediately before writing a response: a long query that
// started while a key was live must not publish its result after the key was
// revoked. It is a row read, never another Argon2id derivation — paying for a
// second hash on every request would halve throughput for no extra safety,
// since the secret was already proved. What a workspace key holds on
// mailboxes is not part of the principal: every use reads it live.
func (k *Keys) Recheck(ctx context.Context, p Principal) error {
	var (
		scopeStr, ws         string
		expiresAt, revokedAt int64
	)
	err := k.store.Reader().QueryRowContext(ctx,
		`SELECT scope, expires_at, revoked_at, coalesce(workspace_id, '') FROM api_keys WHERE prefix = ?`, p.KeyPrefix,
	).Scan(&scopeStr, &expiresAt, &revokedAt, &ws)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInvalidKey
	case err != nil:
		return fmt.Errorf("auth: recheck key: %w", err)
	}
	now := k.now()
	if revokedAt != 0 || (expiresAt != 0 && !now.Before(time.Unix(expiresAt, 0))) {
		return ErrInvalidKey
	}
	// A scope that shrank mid-request is also a reason to stop: the answer
	// was computed for a scope the key no longer has.
	if Scope(scopeStr) != p.Scope || ws != p.WorkspaceID {
		return ErrInvalidKey
	}
	// And so is an instance key's restriction that lost a mailbox the
	// principal still names: a caller that holds a principal for long — a
	// stdio MCP session, a subscription, an event stream — would otherwise
	// keep reaching it.
	return k.recheckRestriction(ctx, p)
}

// recheckRestriction refuses, with ErrKeyNarrowed, a principal that names a
// mailbox its key's restriction no longer does. A mailbox removed since is no
// difference: its id is never reused, so nothing can be reached through it
// again.
func (k *Keys) recheckRestriction(ctx context.Context, p Principal) error {
	if len(p.AccountIDs) == 0 {
		// Unrestricted when it authenticated, and a key never gains a
		// restriction afterwards.
		return nil
	}
	current, err := k.accountsFor(ctx, p.KeyPrefix)
	if err != nil {
		return err
	}
	var dropped []string
	for _, id := range p.AccountIDs {
		if !slices.Contains(current, id) {
			dropped = append(dropped, id)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	list, err := json.Marshal(dropped)
	if err != nil {
		return fmt.Errorf("auth: recheck key: %w", err)
	}
	var still int
	if err := k.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM accounts WHERE id IN (SELECT value FROM json_each(?))`, string(list)).Scan(&still); err != nil {
		return fmt.Errorf("auth: recheck key: %w", err)
	}
	if still > 0 {
		return ErrKeyNarrowed
	}
	return nil
}

// List returns every key, including revoked ones, newest first: the
// operator's listing, with each instance key's restriction and what each
// workspace key holds.
//
// Revoked keys stay listed deliberately: they are part of the answer to "what
// could have reached this mailbox", and deleting the row destroys that answer.
func (k *Keys) List(ctx context.Context) ([]Key, error) {
	return k.list(ctx, "", `1`)
}

// ListIn returns the keys of a workspace, revoked and expired ones too, live
// ones first and each group newest first, with what each holds there: its
// own keys, and the carried-over keys that hold one of its mailboxes, which
// count the other workspaces they reach without naming them.
func (k *Keys) ListIn(ctx context.Context, workspaceID string) ([]Key, error) {
	keys, err := k.list(ctx, workspaceID, `(k.workspace_id = ?1
		OR (k.workspace_id IS NULL AND EXISTS (SELECT 1 FROM key_access x WHERE x.key_prefix = k.prefix AND x.workspace_id = ?1)))`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	return liveFirst(keys, k.now()), nil
}

// ListCreatedBy returns the workspace keys a person created, in every
// workspace, revoked and expired ones too, live ones first and each group
// newest first, with everything each holds.
func (k *Keys) ListCreatedBy(ctx context.Context, userID string) ([]Key, error) {
	if userID == "" {
		return nil, nil
	}
	keys, err := k.list(ctx, "", `k.created_by = ?1 AND (k.workspace_id IS NULL OR k.workspace_id <> '`+
		workspace.OperatorID+`')`, userID)
	if err != nil {
		return nil, err
	}
	return liveFirst(keys, k.now()), nil
}

// Get reads one key, with everything it holds; ErrNotFound when there is none.
func (k *Keys) Get(ctx context.Context, prefix string) (Key, error) {
	keys, err := k.list(ctx, "", `k.prefix = ?1`, prefix)
	if err != nil {
		return Key{}, err
	}
	if len(keys) == 0 {
		return Key{}, ErrNotFound
	}
	return keys[0], nil
}

// liveFirst orders keys with the live ones first, keeping the order within
// each group.
func liveFirst(keys []Key, now time.Time) []Key {
	slices.SortStableFunc(keys, func(a, b Key) int {
		switch al, bl := a.Live(now), b.Live(now); {
		case al == bl:
			return 0
		case al:
			return -1
		default:
			return 1
		}
	})
	return keys
}

// list reads the keys matching where, a constant fragment over api_keys
// aliased k with its arguments, newest first, with their account
// restrictions and what they hold: in workspaceID only, when it is not
// empty.
func (k *Keys) list(ctx context.Context, workspaceID, where string, args ...any) ([]Key, error) {
	rows, err := k.store.Reader().QueryContext(ctx,
		`SELECT k.prefix, k.name, k.scope, coalesce(k.workspace_id, ''), k.origin, k.created_at, k.expires_at,
		        k.revoked_at, k.last_used_at, k.terms_version, k.created_by, k.restricted
		   FROM api_keys k WHERE `+where+` ORDER BY k.created_at DESC, k.rowid DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("auth: list keys: %w", err)
	}
	//nolint:errcheck // a failed close on a read-only query has nothing to report
	defer func() { _ = rows.Close() }()

	var out []Key
	for rows.Next() {
		var (
			key                                 Key
			scopeStr                            string
			created, expires, revoked, lastUsed int64
		)
		if err := rows.Scan(&key.Prefix, &key.Name, &scopeStr, &key.WorkspaceID, &key.Origin, &created, &expires,
			&revoked, &lastUsed, &key.TermsVersion, &key.CreatedBy, &key.Restricted); err != nil {
			return nil, fmt.Errorf("auth: scan key: %w", err)
		}
		key.Scope = Scope(scopeStr)
		key.CreatedAt = unixOrZero(created)
		key.ExpiresAt = unixOrZero(expires)
		key.RevokedAt = unixOrZero(revoked)
		key.LastUsedAt = unixOrZero(lastUsed)
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list keys: %w", err)
	}
	for i := range out {
		if out[i].IsInstance() {
			ids, err := k.accountsFor(ctx, out[i].Prefix)
			if err != nil {
				return nil, err
			}
			out[i].AccountIDs = ids
			continue
		}
		if err := k.holds(ctx, &out[i], workspaceID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// holds reads what a workspace key holds into it: everything, or with
// workspaceID that workspace's mailboxes, counting the other workspaces a
// carried-over key reaches.
func (k *Keys) holds(ctx context.Context, key *Key, workspaceID string) error {
	rows, err := k.store.Reader().QueryContext(ctx, `SELECT x.account_id, x.workspace_id, x.read, x.act, x.send,
		       x.granted_by, x.created_at, x.updated_at
		  FROM key_access x JOIN accounts a ON a.id = x.account_id
		 WHERE x.key_prefix = ? ORDER BY x.created_at, a.created_at, a.rowid`, key.Prefix)
	if err != nil {
		return fmt.Errorf("auth: read what the key holds: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	others := map[string]bool{}
	key.Mailboxes = []workspace.KeyAccess{}
	for rows.Next() {
		a := workspace.KeyAccess{KeyPrefix: key.Prefix}
		var created, updated int64
		if err := rows.Scan(&a.AccountID, &a.WorkspaceID, &a.Read, &a.Act, &a.Send, &a.GrantedBy,
			&created, &updated); err != nil {
			return fmt.Errorf("auth: read what the key holds: %w", err)
		}
		a.CreatedAt, a.UpdatedAt = unixOrZero(created), unixOrZero(updated)
		if workspaceID != "" && a.WorkspaceID != workspaceID {
			others[a.WorkspaceID] = true
			continue
		}
		key.Mailboxes = append(key.Mailboxes, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("auth: read what the key holds: %w", err)
	}
	key.OtherWorkspaces = len(others)
	return nil
}

// ErrNotFound means no key carries that prefix, or none the caller may see.
var ErrNotFound = errors.New("auth: no such key")

// Revoke marks a key unusable: the operator's, for any key. Revoking an
// already-revoked key is not an error: the caller asked for a state, and the
// state holds.
func (k *Keys) Revoke(ctx context.Context, prefix string) error {
	return k.store.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET revoked_at = ? WHERE prefix = ? AND revoked_at = 0`,
			k.now().Unix(), prefix)
		if err != nil {
			return fmt.Errorf("auth: revoke key: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("auth: revoke key: %w", err)
		}
		if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE prefix = ?`, prefix).Scan(&exists); err != nil {
				return fmt.Errorf("auth: revoke key: %w", err)
			}
			if exists == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}

// RevokeIn revokes a key of a workspace: one of its own keys is revoked; a
// carried-over key that holds mailboxes of the workspace loses them, and is
// revoked with its last (the schema's trigger), since it is not this
// workspace's alone. Any other key is ErrNotFound. check runs first in the
// transaction. Revoking a key already revoked is not an error.
func (k *Keys) RevokeIn(ctx context.Context, workspaceID, prefix string, check func(*sql.Tx) error) error {
	return k.store.Write(ctx, func(tx *sql.Tx) error {
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		var (
			ws      string
			revoked int64
			held    int
		)
		err := tx.QueryRowContext(ctx, `SELECT coalesce(workspace_id, ''), revoked_at,
			       (SELECT count(*) FROM key_access x WHERE x.key_prefix = k.prefix AND x.workspace_id = ?)
			  FROM api_keys k WHERE prefix = ?`, workspaceID, prefix).Scan(&ws, &revoked, &held)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("auth: revoke key: %w", err)
		case ws == workspaceID && ws != "":
			if revoked != 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE prefix = ? AND revoked_at = 0`,
				k.now().Unix(), prefix); err != nil {
				return fmt.Errorf("auth: revoke key: %w", err)
			}
			return nil
		case ws == "" && held > 0:
			if _, err := tx.ExecContext(ctx, `DELETE FROM key_access WHERE key_prefix = ? AND workspace_id = ?`,
				prefix, workspaceID); err != nil {
				return fmt.Errorf("auth: take the workspace out of the key: %w", err)
			}
			return nil
		}
		return ErrNotFound
	})
}

// RevokeCreatedBy revokes a workspace key a person created, in any
// workspace. Anyone else's key, or an instance key, is ErrNotFound: to this
// caller it does not exist. Revoking one already revoked is not an error.
func (k *Keys) RevokeCreatedBy(ctx context.Context, prefix, userID string) error {
	return k.store.Write(ctx, func(tx *sql.Tx) error {
		var revokedAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT revoked_at FROM api_keys WHERE prefix = ? AND created_by = ? AND created_by <> ''
			    AND (workspace_id IS NULL OR workspace_id <> ?)`, prefix, userID, workspace.OperatorID).Scan(&revokedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("auth: revoke key: %w", err)
		case revokedAt != 0:
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET revoked_at = ? WHERE prefix = ? AND revoked_at = 0`, k.now().Unix(), prefix); err != nil {
			return fmt.Errorf("auth: revoke key: %w", err)
		}
		return nil
	})
}

// CarriedOverLive counts the live keys migration 0012 carried over from a
// person's, with no workspace: what the daemon says at start until none is
// left.
func (k *Keys) CarriedOverLive(ctx context.Context) (int, error) {
	var n int
	if err := k.store.Reader().QueryRowContext(ctx, `SELECT count(*) FROM api_keys
		WHERE workspace_id IS NULL AND revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`, k.now().Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count carried-over keys: %w", err)
	}
	return n, nil
}

// Count reports how many keys exist, so the daemon can say whether it has been
// bootstrapped.
func (k *Keys) Count(ctx context.Context) (int, error) {
	var n int
	if err := k.store.Reader().QueryRowContext(ctx, `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count keys: %w", err)
	}
	return n, nil
}

func (k *Keys) accountsFor(ctx context.Context, prefix string) ([]string, error) {
	rows, err := k.store.Reader().QueryContext(ctx,
		`SELECT account_id FROM api_key_accounts WHERE key_prefix = ? ORDER BY account_id`, prefix)
	if err != nil {
		return nil, fmt.Errorf("auth: read key restrictions: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("auth: scan key restriction: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: read key restrictions: %w", err)
	}
	return out, nil
}

// touch records that a key was used, without letting the bookkeeping affect
// the request. It runs detached, so a slow write cannot delay a response and a
// cancelled request still records the use that already happened.
func (k *Keys) touch(ctx context.Context, prefix string, now time.Time) {
	detached := context.WithoutCancel(ctx)
	go func() {
		cutoff := now.Add(-lastUsedResolution).Unix()
		// Bookkeeping: a failed write here must never affect the request that
		// already authenticated successfully.
		//nolint:errcheck // deliberately detached and best effort
		_ = k.store.Write(detached, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(detached,
				`UPDATE api_keys SET last_used_at = ? WHERE prefix = ? AND last_used_at < ?`,
				now.Unix(), prefix, cutoff)
			return err
		})
	}()
}

func unixOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// nullable stores an empty string as NULL, for the optional foreign keys.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
