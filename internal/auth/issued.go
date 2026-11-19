package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/workspace"
)

// Keys an issuer makes (docs/workspaces.md, "Keys an issuer makes").
//
// An extension of the daemon may follow an issuer that makes API keys
// elsewhere, for people of a directory it follows, and mirror them here
// (migration 0028): each is a row of api_keys with its issuer, holding
// mailboxes in key_access, so that once authenticated it is a workspace key
// like any other. Its credential is the issuer's: this server keeps the
// issuer's hash of it, never a secret, and asks the issuer's own check
// (internal/service's KeyIssuer) to compare a presented credential with it.
// No Argon2id is spent on one: the issuer's secret has 256 bits, and its
// check is a constant-time comparison of two hashes.

// IssuedOutcome is what authenticating an issued key found once its secret
// matched: accepted (""), or why it was refused.
type IssuedOutcome string

// What an issued key whose secret matched may be refused for. Each is the
// one ErrInvalidKey to the caller.
const (
	// IssuedAccepted is a key that works.
	IssuedAccepted IssuedOutcome = ""
	// IssuedRevoked is a key revoked: by its issuer, or here.
	IssuedRevoked IssuedOutcome = "revoked"
	// IssuedExpired is a key past its expiry by this server's clock.
	IssuedExpired IssuedOutcome = "expired"
	// IssuedHeld is a key this server stopped on its own (an operator's
	// hold, its creator stopped here), or one the edition refuses whole.
	IssuedHeld IssuedOutcome = "held"
)

// IssuedCheck is an authentication of an issued key whose secret matched:
// its id at its issuer and what was found. The zero IssuedCheck is a
// credential whose secret matched nothing, of which nothing is said to
// anyone.
type IssuedCheck struct {
	ExternalID string
	Outcome    IssuedOutcome
}

// AuthenticateIssued checks a credential an issuer made, presented under
// selector ("" for one whose selector could not be read), and returns the
// caller it stands for.
//
// verify compares the credential with the hash held for its selector, "" when
// none is held, in constant time: the issuer's own check. It runs whether or
// not a key is held, so that an unknown selector costs what a wrong secret
// costs, but for one thing, which unknown decides as for this server's own
// keys: a selector that matches nothing can only fail, so a caller that has
// presented too many of those is refused before the comparison, with a
// ThrottledError.
//
// Every refusal is ErrInvalidKey. Once the secret matched, IssuedCheck says
// which key it was and why it was refused (revoked, expired by this server's
// clock, held here, or every issued key refused, refuseAll); it is the zero
// IssuedCheck when the secret matched nothing. An issued key never records a
// use (last_used_at): nothing keeps when it was used.
func (k *Keys) AuthenticateIssued(ctx context.Context, issuer, selector string, verify func(hash string) bool,
	unknown Gate, refuseAll bool,
) (Principal, IssuedCheck, error) {
	var (
		hash, scopeStr, ws, createdBy, terms, externalID string
		expiresAt, revokedAt                             int64
		stopped                                          bool
	)
	found := false
	if selector != "" && issuer != "" {
		err := k.store.Reader().QueryRowContext(ctx,
			`SELECT k.hash, k.scope, k.expires_at, k.revoked_at, coalesce(k.workspace_id, ''), k.created_by,
			        k.terms_version, i.external_id,
			        EXISTS (SELECT 1 FROM issued_key_stops s WHERE s.prefix = k.prefix)
			   FROM api_keys k JOIN issued_keys i ON i.prefix = k.prefix
			  WHERE k.prefix = ? AND k.issuer = ?`, selector, issuer,
		).Scan(&hash, &scopeStr, &expiresAt, &revokedAt, &ws, &createdBy, &terms, &externalID, &stopped)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return Principal{}, IssuedCheck{}, fmt.Errorf("auth: look up an issued key: %w", err)
		default:
			found = true
		}
	}
	if !found {
		if unknown != nil {
			if ok, wait := unknown(); !ok {
				return Principal{}, IssuedCheck{}, &ThrottledError{RetryAfter: wait}
			}
		}
		_ = verify("")
		return Principal{}, IssuedCheck{}, ErrInvalidKey
	}
	if !verify(hash) {
		// A wrong secret counts for no key.
		return Principal{}, IssuedCheck{}, ErrInvalidKey
	}
	check := IssuedCheck{ExternalID: externalID}
	switch now := k.now(); {
	case revokedAt != 0:
		check.Outcome = IssuedRevoked
	case expiresAt == 0 || !now.Before(time.Unix(expiresAt, 0)):
		// The schema never holds an issued key that does not expire; one
		// that did would be refused rather than read as never expiring.
		check.Outcome = IssuedExpired
	case stopped || refuseAll:
		check.Outcome = IssuedHeld
	}
	scope, err := ParseScope(scopeStr)
	if err != nil || ws == "" || ws == workspace.OperatorID {
		check.Outcome = IssuedHeld
	}
	if check.Outcome != IssuedAccepted {
		return Principal{}, check, ErrInvalidKey
	}
	return Principal{
		Kind: KindKey, KeyPrefix: selector, Scope: scope, WorkspaceID: ws, TermsVersion: terms, CreatedBy: createdBy,
		Issuer: issuer,
	}, check, nil
}

// IssuedExternalID reads an issued key's id at its issuer; "" when this
// server holds no such key.
func (k *Keys) IssuedExternalID(ctx context.Context, issuer, selector string) (string, error) {
	var id string
	err := k.store.Reader().QueryRowContext(ctx,
		`SELECT external_id FROM issued_keys WHERE prefix = ? AND issuer = ?`, selector, issuer).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("auth: read an issued key: %w", err)
	}
	return id, nil
}

// OwnKeysRevoked is what RevokeWorkspaceKeys stopped: how many keys, how many
// of them had not expired yet, and when.
type OwnKeysRevoked struct {
	Revoked   int
	Unexpired int
	At        time.Time
}

// RevokeWorkspaceKeys revokes, in one write, every live key of this server's
// own outside the operator workspace — a workspace's keys and the ones
// migration 0012 carried over — saying it was revoked (KeyStopManual), and
// reports how many and when: what an edition whose keys an issuer makes does
// at start. The operator's keys and the issued keys are untouched, and a key
// already stopped keeps its moment and its reason, so a second call stops
// none.
func (k *Keys) RevokeWorkspaceKeys(ctx context.Context) (OwnKeysRevoked, error) {
	now := k.now().UTC().Truncate(time.Second)
	out := OwnKeysRevoked{At: now}
	err := k.store.Write(ctx, func(tx *sql.Tx) error {
		const own = `issuer = '' AND revoked_at = 0 AND (workspace_id IS NULL OR workspace_id <> '` +
			workspace.OperatorID + `')`
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE `+own+`
			AND (expires_at = 0 OR expires_at > ?)`, now.Unix()).Scan(&out.Unexpired); err != nil {
			return fmt.Errorf("auth: count the workspace keys: %w", err)
		}
		n, err := execCount(ctx, tx, `UPDATE api_keys SET revoked_at = ?, stop_reason = ? WHERE `+own,
			now.Unix(), string(workspace.KeyStopManual))
		if err != nil {
			return fmt.Errorf("auth: revoke the workspace keys: %w", err)
		}
		out.Revoked = n
		return nil
	})
	if err != nil {
		return OwnKeysRevoked{}, err
	}
	return out, nil
}
