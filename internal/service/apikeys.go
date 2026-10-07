package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/auth"
)

// The operator's keys are managed with an instance key only, here. An
// instance key belongs to the operator workspace, so a console session — or a
// workspace's key — minting one would be a way out of the ownership rules. A
// workspace's keys are made in the console by its owners and admins, in
// workspacekeys.go; the operator lists and revokes every key here.

// APIKey is a stored key as an administrator sees it. The secret is never
// here.
type APIKey struct {
	Prefix string     `json:"prefix"`
	Name   string     `json:"name"`
	Scope  auth.Scope `json:"scope"`
	// WorkspaceID is the workspace the key belongs to: "wsp_operator" for
	// an instance key; absent for a key carried over from a person's that
	// reached several workspaces.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// AccountIDs are the operator mailboxes an instance key is restricted
	// to, none for every one; for a workspace key, the mailboxes it holds
	// something on.
	AccountIDs []string `json:"account_ids,omitempty"`
	// Restricted says an instance key was made for chosen accounts: with
	// all of them removed, it lists none and is revoked, rather than
	// reaching every one.
	Restricted bool  `json:"restricted,omitempty"`
	CreatedAt  int64 `json:"created_at"`
	ExpiresAt  int64 `json:"expires_at,omitempty"`
	RevokedAt  int64 `json:"revoked_at,omitempty"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
	// TermsVersion is the key terms revision the person who created a
	// workspace key agreed to; absent for an instance key.
	TermsVersion string `json:"terms_version,omitempty"`
	// CreatedBy is who created the key: "usr_…", "key:<prefix>" or "cli";
	// absent for keys from before this was recorded.
	CreatedBy string `json:"created_by,omitempty"`
}

// CreateAPIKeyRequest describes a key to issue.
type CreateAPIKeyRequest struct {
	Name       string   `json:"name"`
	Scope      string   `json:"scope"`
	AccountIDs []string `json:"account_ids,omitempty"`
	// UserID is refused. No key acts as a person: a workspace's keys are
	// created in the console by its owners and admins, agreeing to the key
	// terms (workspacekeys.go).
	UserID        string `json:"user_id,omitempty"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
}

// CreatedAPIKey is a new key and the only copy of its secret.
type CreatedAPIKey struct {
	APIKey
	// Key is returned once and never stored in a form anything can read
	// back.
	Key string `json:"key"`
}

// maxKeyName bounds a key's name, like a person's.
const maxKeyName = 120

// ListAPIKeys lists every key, instance and workspace keys alike, revoked ones
// included: they are part of the answer to "what could have reached this
// mailbox".
func (s *Service) ListAPIKeys(ctx context.Context, p Principal) ([]APIKey, error) {
	if err := s.authorizeKeyAdmin(p); err != nil {
		return nil, err
	}
	keys, err := s.keys.List(ctx)
	if err != nil {
		return nil, E(CodeInternal, "listing keys failed", err)
	}
	out := make([]APIKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, presentKey(k))
	}
	return out, nil
}

// CreateAPIKey issues an instance key.
//
// A key may never reach further than the key that made it, in either
// direction: not a higher scope — a leaked send key must not be a leaked admin
// key one request later — and not more accounts. A key restricted to two
// accounts can only issue keys restricted to some of those two; an empty list
// means every account, so it cannot issue one of those at all.
func (s *Service) CreateAPIKey(ctx context.Context, p Principal, req CreateAPIKeyRequest) (CreatedAPIKey, error) {
	if err := s.authorizeKeyAdmin(p); err != nil {
		return CreatedAPIKey{}, err
	}
	if strings.TrimSpace(req.UserID) != "" {
		return CreatedAPIKey{}, E(CodeBadRequest,
			"user_id is not accepted: no key acts as a person; an owner or an admin of a workspace creates its keys "+
				"in the console", nil)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxKeyName {
		return CreatedAPIKey{}, Ef(CodeBadRequest, nil,
			"a key needs a name of at most %d characters, so it can be recognised later", maxKeyName)
	}
	scope, err := auth.ParseScope(req.Scope)
	if err != nil {
		return CreatedAPIKey{}, E(CodeBadRequest, "scope must be one of read, write, send, admin", err)
	}
	if !p.Scope.Covers(scope) {
		return CreatedAPIKey{}, Ef(CodeNotAuthorized, nil, "a %s key cannot issue a %s key", p.Scope, scope)
	}
	maxDays := int(auth.MaxLifetime / (24 * time.Hour))
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxDays {
		return CreatedAPIKey{}, Ef(CodeBadRequest, nil,
			"expires_in_days must be between 1 and %d; leave it out for the longest", maxDays)
	}

	var accountIDs []string
	for _, id := range req.AccountIDs {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(accountIDs, id) {
			accountIDs = append(accountIDs, id)
		}
	}
	if len(p.AccountIDs) > 0 {
		if len(accountIDs) == 0 {
			return CreatedAPIKey{}, E(CodeNotAuthorized,
				"a key restricted to some accounts cannot issue a key for every account", nil)
		}
		for _, id := range accountIDs {
			if !p.MayAccess(id) {
				return CreatedAPIKey{}, E(CodeNotAuthorized,
					"a key restricted to some accounts can only issue keys restricted to those accounts", nil)
			}
		}
	}

	secret, key, err := s.keys.Issue(ctx, auth.NewKeyRequest{
		Name: name, Scope: scope, AccountIDs: accountIDs,
		TTL: time.Duration(req.ExpiresInDays) * 24 * time.Hour, CreatedBy: p.Actor(),
	})
	switch {
	case errors.Is(err, auth.ErrUnknownAccount):
		return CreatedAPIKey{}, E(CodeBadRequest, "account_ids names an account that does not exist", err)
	case err != nil:
		return CreatedAPIKey{}, E(CodeInternal, "issuing the key failed", err)
	}
	return CreatedAPIKey{APIKey: presentKey(key), Key: secret}, nil
}

// RevokeAPIKey revokes a key, an instance key or a workspace's. Revoking one
// already revoked is not an error.
func (s *Service) RevokeAPIKey(ctx context.Context, p Principal, prefix string) error {
	if err := s.authorizeKeyAdmin(p); err != nil {
		return err
	}
	switch err := s.keys.Revoke(ctx, prefix); {
	case errors.Is(err, auth.ErrNotFound):
		return E(CodeNotFound, "no such api key", err)
	case err != nil:
		return E(CodeInternal, "revoking the key failed", err)
	}
	s.accessChanged()
	return nil
}

// authorizeKeyAdmin admits an admin-scope instance key and nobody else.
func (s *Service) authorizeKeyAdmin(p Principal) error {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return err
	}
	if !p.IsInstance() {
		return E(CodeNotAuthorized, "managing API keys needs an instance admin key", nil)
	}
	return nil
}

func presentKey(k auth.Key) APIKey {
	ids := k.AccountIDs
	if !k.IsInstance() {
		ids = make([]string, 0, len(k.Mailboxes))
		for _, m := range k.Mailboxes {
			ids = append(ids, m.AccountID)
		}
	}
	return APIKey{
		Prefix: k.Prefix, Name: k.Name, Scope: k.Scope, WorkspaceID: k.WorkspaceID, AccountIDs: ids,
		Restricted: k.Restricted && k.IsInstance(),
		CreatedAt:  unixOrZero(k.CreatedAt), ExpiresAt: unixOrZero(k.ExpiresAt),
		RevokedAt: unixOrZero(k.RevokedAt), LastUsedAt: unixOrZero(k.LastUsedAt),
		TermsVersion: k.TermsVersion, CreatedBy: k.CreatedBy,
	}
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
