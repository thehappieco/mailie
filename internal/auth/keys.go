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
	// UserID is the person the caller acts as: the signed-in user, or the
	// user a key was issued for. Empty for an instance key, which answers to
	// nobody but the operator and reaches the operator workspace's mailboxes.
	UserID   string
	UserRole Role
	Scope    Scope
	// AccountIDs restricts the key to specific accounts. Empty means every
	// account, which is what a personal deployment wants and what a key handed
	// to one integration should not have.
	AccountIDs []string
	// Tool marks a key presented to the MCP server: a tool, such as an AI
	// assistant, acting with it. It changes what nothing sees: an instance
	// key reaches the operator workspace's mailboxes over REST and MCP alike,
	// and a person's mailbox is reached by a tool only through a key that
	// person created.
	Tool bool
	// TermsVersion is, for a key, the revision of the key terms the person
	// it acts as agreed to by creating it; empty for an instance key and for
	// a key somebody else made for them.
	TermsVersion string
}

// IsSession reports whether the caller is a person signed in to the console.
func (p Principal) IsSession() bool { return p.Kind == KindSession }

// IsInstance reports whether the caller is an instance key: a key bound to no
// user, which is what the CLI holds and what reaches the operator workspace.
func (p Principal) IsInstance() bool { return !p.IsSession() && p.UserID == "" }

// Actor names the caller for audit columns: "usr_…" for a person, "key:<prefix>"
// for a key, whoever it acts as — the key is what did it.
func (p Principal) Actor() string {
	if p.IsSession() {
		return p.UserID
	}
	return "key:" + p.KeyPrefix
}

// MayAccess reports whether the principal may touch an account.
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

// Key is a stored key, as listed to an administrator. It never carries the
// secret: that exists once, in the response to the call that created it.
type Key struct {
	Prefix     string
	Name       string
	Scope      Scope
	AccountIDs []string
	// Restricted records that the key was made for chosen accounts. With
	// none of them left, AccountIDs is empty like a key for every account's,
	// and the key has been revoked.
	Restricted bool
	// UserID is the person the key acts as. Empty for an instance key.
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time
	// TermsVersion is the revision of the key terms the person agreed to
	// when they created the key; empty for a key nobody agreed to anything
	// through (an instance key, or one from before keys had terms).
	TermsVersion string
	// CreatedBy is who created the key: "usr_…", "key:<prefix>" or "cli".
	CreatedBy string
}

// Revoked reports whether the key has been revoked.
func (k Key) Revoked() bool { return !k.RevokedAt.IsZero() }

// Expired reports whether the key has expired at the given time.
func (k Key) Expired(now time.Time) bool { return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) }

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
	Name       string
	Scope      Scope
	AccountIDs []string
	// UserID binds the key to a person: it then sees what that person owns
	// and nothing else, within its own scope and restriction. Empty issues an
	// instance key.
	UserID string
	// TTL is how long the key lives. Zero means MaxLifetime.
	TTL time.Duration
	// TermsVersion is the revision of the key terms the person was shown and
	// agreed to; recorded on the key.
	TermsVersion string
	// CreatedBy names who created the key, for the record: "usr_…",
	// "key:<prefix>" or "cli".
	CreatedBy string
	// MaxLive, when positive, bounds how many live keys (neither revoked nor
	// expired) UserID may hold, this one included. Counted in the
	// transaction that inserts, so two creations at once cannot both pass.
	MaxLive int
}

// Errors issuing a key can report about its request.
var (
	// ErrUnknownAccount is a restriction naming an account that does not
	// exist, or, for an instance key, one outside the operator workspace,
	// which is all an instance key ever reaches.
	ErrUnknownAccount = errors.New("auth: a key cannot be restricted to an account that does not exist")
	// ErrUserNotFound is a user id nobody has.
	ErrUserNotFound = errors.New("auth: no such user")
	// ErrTooManyKeys is a person who already holds NewKeyRequest.MaxLive
	// live keys.
	ErrTooManyKeys = errors.New("auth: this person already holds as many live keys as allowed")
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

	presented, prefix, hash, err := Generate()
	if err != nil {
		return "", Key{}, err
	}
	now := k.now().UTC().Truncate(time.Second)
	out := Key{
		Prefix:       prefix,
		Name:         req.Name,
		Scope:        req.Scope,
		AccountIDs:   req.AccountIDs,
		Restricted:   len(req.AccountIDs) > 0,
		UserID:       req.UserID,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		TermsVersion: req.TermsVersion,
		CreatedBy:    req.CreatedBy,
	}

	err = k.store.Write(ctx, func(tx *sql.Tx) error {
		if req.MaxLive > 0 && req.UserID != "" {
			var live int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM api_keys WHERE user_id = ? AND revoked_at = 0 AND expires_at > ?`,
				req.UserID, now.Unix()).Scan(&live); err != nil {
				return fmt.Errorf("auth: count live keys: %w", err)
			}
			if live >= req.MaxLive {
				return ErrTooManyKeys
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, user_id, terms_version, created_by,
			                      restricted)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			out.Prefix, hash, out.Name, string(out.Scope), out.CreatedAt.Unix(), out.ExpiresAt.Unix(), nullable(out.UserID),
			out.TermsVersion, out.CreatedBy, out.Restricted,
		)
		if store.IsForeignKey(err) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("auth: insert key: %w", err)
		}
		for _, accountID := range req.AccountIDs {
			if req.UserID == "" {
				// An instance key reaches the operator workspace's
				// mailboxes and nothing else: one restricted to a person's
				// mailbox would reach nothing at all.
				var operator int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ? AND workspace_id = ?`,
					accountID, workspace.OperatorID).Scan(&operator); err != nil {
					return fmt.Errorf("auth: check account %s: %w", accountID, err)
				}
				if operator == 0 {
					return ErrUnknownAccount
				}
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
		return nil
	})
	if err != nil {
		return "", Key{}, err
	}
	return presented, out, nil
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
		owner     keyOwner
	)
	err := k.store.Reader().QueryRowContext(ctx,
		`SELECT k.hash, k.scope, k.expires_at, k.revoked_at, k.terms_version, k.user_id, u.role, u.status
		   FROM api_keys k LEFT JOIN users u ON u.id = k.user_id
		  WHERE k.prefix = ?`, prefix,
	).Scan(&hash, &scopeStr, &expiresAt, &revokedAt, &terms, &owner.userID, &owner.role, &owner.status)
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
	// same as a live one with the wrong secret.
	now := k.now()
	if revokedAt != 0 || (expiresAt != 0 && !now.Before(time.Unix(expiresAt, 0))) {
		return Principal{}, ErrInvalidKey
	}
	// A key acting as a person stops with that person: disabling someone
	// has to end every way they had in, not only their browser sessions.
	if !owner.usable() {
		return Principal{}, ErrInvalidKey
	}
	scope, err := ParseScope(scopeStr)
	if err != nil {
		return Principal{}, ErrInvalidKey
	}

	accountIDs, err := k.accountsFor(ctx, prefix)
	if err != nil {
		return Principal{}, err
	}
	k.touch(ctx, prefix, now)
	return Principal{
		Kind: KindKey, KeyPrefix: prefix, Scope: scope, AccountIDs: accountIDs,
		UserID: owner.userID.String, UserRole: Role(owner.role.String), TermsVersion: terms,
	}, nil
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

// keyOwner is the user a key acts as, read beside the key in one query.
type keyOwner struct {
	userID, role, status sql.NullString
}

// usable reports whether the key may still act: an instance key always may, a
// user's key only while that user is active.
func (o keyOwner) usable() bool {
	return !o.userID.Valid || o.status.String == userActive
}

// Recheck re-reads the mutable parts of a key that has already authenticated.
//
// Handlers call this immediately before writing a response: a long query that
// started while a key was live must not publish its result after the key was
// revoked. It is a row read, never another Argon2id derivation — paying for a
// second hash on every request would halve throughput for no extra safety,
// since the secret was already proved.
func (k *Keys) Recheck(ctx context.Context, p Principal) error {
	var scopeStr string
	var expiresAt, revokedAt int64
	var owner keyOwner
	err := k.store.Reader().QueryRowContext(ctx,
		`SELECT k.scope, k.expires_at, k.revoked_at, k.user_id, u.role, u.status
		   FROM api_keys k LEFT JOIN users u ON u.id = k.user_id
		  WHERE k.prefix = ?`, p.KeyPrefix,
	).Scan(&scopeStr, &expiresAt, &revokedAt, &owner.userID, &owner.role, &owner.status)
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
	// A scope that shrank mid-request is also a reason to stop, and so is a
	// person disabled or demoted while their key's request ran: the answer
	// was computed for a role they no longer hold.
	if Scope(scopeStr) != p.Scope || !owner.usable() || Role(owner.role.String) != p.UserRole {
		return ErrInvalidKey
	}
	// And so is a restriction that lost a mailbox the principal still names
	// (its person lost read on it): a caller that holds a principal for
	// long — a stdio MCP session, a subscription, an event stream — would
	// otherwise keep reaching it, and reach it again once read is granted
	// back, which the key itself never will.
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

// List returns every key, including revoked ones.
//
// Revoked keys stay listed deliberately: they are part of the answer to "what
// could have reached this mailbox", and deleting the row destroys that answer.
func (k *Keys) List(ctx context.Context) ([]Key, error) {
	return k.list(ctx, `1`)
}

// ListFor returns the keys that act as one person, revoked and expired ones
// included, newest first.
func (k *Keys) ListFor(ctx context.Context, userID string) ([]Key, error) {
	return k.list(ctx, `user_id = ?`, userID)
}

// list reads the keys matching where, a constant fragment with its
// arguments, newest first, with their account restrictions.
func (k *Keys) list(ctx context.Context, where string, args ...any) ([]Key, error) {
	rows, err := k.store.Reader().QueryContext(ctx,
		`SELECT prefix, name, scope, coalesce(user_id, ''), created_at, expires_at, revoked_at, last_used_at,
		        terms_version, created_by, restricted
		   FROM api_keys WHERE `+where+` ORDER BY created_at DESC, rowid DESC`, args...)
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
		if err := rows.Scan(&key.Prefix, &key.Name, &scopeStr, &key.UserID, &created, &expires, &revoked, &lastUsed,
			&key.TermsVersion, &key.CreatedBy, &key.Restricted); err != nil {
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
		ids, err := k.accountsFor(ctx, out[i].Prefix)
		if err != nil {
			return nil, err
		}
		out[i].AccountIDs = ids
	}
	return out, nil
}

// ErrNotFound means no key carries that prefix.
var ErrNotFound = errors.New("auth: no such key")

// Revoke marks a key unusable. Revoking an already-revoked key is not an error:
// the caller asked for a state, and the state holds.
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

// RevokeFor revokes a key that acts as userID. Anyone else's key, or an
// instance key, is ErrNotFound: to this caller it does not exist.
func (k *Keys) RevokeFor(ctx context.Context, prefix, userID string) error {
	return k.store.Write(ctx, func(tx *sql.Tx) error {
		var revokedAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT revoked_at FROM api_keys WHERE prefix = ? AND user_id = ?`, prefix, userID).Scan(&revokedAt)
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

// nullable stores an empty string as NULL, for the optional foreign keys.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func unixOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}
