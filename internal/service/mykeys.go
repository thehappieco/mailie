package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
)

// A person's own API keys.
//
// A tool — an AI assistant through the MCP server, a script against the REST
// API — reaches a person's mailboxes only through a key that person created
// in the console: for the mailboxes they chose, with the scope they chose, and
// for as long as they chose. Creating one is the person's agreement to what a
// tool holding it can do, in the words the console showed them, and the key
// records which revision of those words that was. It is theirs to revoke.
//
// Only a person signed in to the console manages their keys. A key never
// mints another, not even one acting for the same person: a leaked key must
// not be a way to keep access after it is revoked.

// DefaultKeyTermsVersion is the revision of the text a person reads before
// creating a key, when the deployment configures no other
// (MAIL_CONSENT_VERSION_KEYS). A console creating one names the revision it
// showed, so a key is never taken as agreed to words the person was not
// shown. A key keeps the revision it was created under: changing the current
// one asks for it on the next key, and leaves the keys that exist working.
const DefaultKeyTermsVersion = config.DefaultKeyTermsVersion

// MaxPersonalKeys is how many live keys — neither revoked nor expired — one
// person may hold. Enough for every assistant and script a person uses; few
// enough that the list stays something they can read.
const MaxPersonalKeys = 20

// DefaultPersonalKeyDays is how long a key lives when the person does not
// choose; PersonalKeyDays are the choices.
const DefaultPersonalKeyDays = 90

// PersonalKeyDays are the lifetimes, in days, a person may give a key.
var PersonalKeyDays = []int{30, 90, 365}

// PersonalKey is one of a person's keys as their console lists it. The
// secret is never here.
type PersonalKey struct {
	Prefix string     `json:"prefix"`
	Name   string     `json:"name"`
	Scope  auth.Scope `json:"scope"`
	// AccountIDs are the mailboxes the key reaches; empty is every mailbox
	// the person has, including ones they connect later — unless
	// Restricted.
	AccountIDs []string `json:"account_ids"`
	// Restricted says the key was made for chosen mailboxes. One whose
	// mailboxes were all removed since lists none, and was revoked with the
	// last: it never reached every mailbox. A removed mailbox's id is not
	// kept, so it is not listed.
	Restricted bool  `json:"restricted"`
	CreatedAt  int64 `json:"created_at"`
	ExpiresAt  int64 `json:"expires_at"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
	RevokedAt  int64 `json:"revoked_at,omitempty"`
	// TermsVersion is the revision of the key terms the person agreed to;
	// empty for a key an administrator made for them.
	TermsVersion string `json:"terms_version"`
}

// CreatedPersonalKey is a new key and the only copy of its secret.
type CreatedPersonalKey struct {
	// Key is shown once. Only its Argon2id hash is stored.
	Key string `json:"key"`
	PersonalKey
}

// PersonalKeyRequest is a key a person asks for.
type PersonalKeyRequest struct {
	Name string `json:"name"`
	// Scope is read, or write: read and the actions on messages, which
	// still need the person's consent to actions.
	Scope string `json:"scope"`
	// AccountIDs are mailboxes of the person's own; empty is all of them.
	AccountIDs []string `json:"account_ids,omitempty"`
	// TTLDays is one of PersonalKeyDays; zero is DefaultPersonalKeyDays.
	TTLDays int `json:"ttl_days,omitempty"`
	// TermsVersion must be the current revision of the key terms: the text
	// the person was shown.
	TermsVersion string `json:"terms_version"`
}

// Errors of the personal key routes.
var (
	errTooManyKeys = Ef(CodeConflict, nil,
		"you already have %d live keys; revoke one you no longer use first", MaxPersonalKeys)
	errNoSuchKey = E(CodeNotFound, "no such api key", nil)
)

// ListMyAPIKeys lists the signed-in person's keys, newest first — revoked and
// expired ones too, and any an administrator made for them: every key that
// could act as them.
func (s *Service) ListMyAPIKeys(ctx context.Context, p Principal) ([]PersonalKey, error) {
	if err := requireSession(p); err != nil {
		return nil, err
	}
	keys, err := s.keys.ListFor(ctx, p.UserID)
	if err != nil {
		return nil, E(CodeInternal, "listing keys failed", err)
	}
	out := make([]PersonalKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, presentPersonalKey(k))
	}
	return out, nil
}

// CreateMyAPIKey issues a key acting as the signed-in person. It lasts what
// they chose, whatever is left of the session it was created in, which an
// extension may have started for less than a password's (SignInExternal):
// disabling the person is what revokes it sooner.
func (s *Service) CreateMyAPIKey(ctx context.Context, p Principal, req PersonalKeyRequest) (CreatedPersonalKey, error) {
	if err := requireSession(p); err != nil {
		return CreatedPersonalKey{}, err
	}
	if req.TermsVersion != s.consent.Keys {
		// A console showing an older text, or none: the person has not
		// seen what they would be agreeing to.
		return CreatedPersonalKey{}, Ef(CodeConflict, nil,
			"creating a key must name the current key terms version, %s; reload and review them", s.consent.Keys)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxKeyName {
		return CreatedPersonalKey{}, Ef(CodeBadRequest, nil,
			"a key needs a name of at most %d characters, so it can be recognised later", maxKeyName)
	}
	scope := auth.Scope(req.Scope)
	if scope != auth.ScopeRead && scope != auth.ScopeWrite {
		return CreatedPersonalKey{}, E(CodeBadRequest, "scope must be read or write", nil)
	}
	days := req.TTLDays
	if days == 0 {
		days = DefaultPersonalKeyDays
	}
	if !slices.Contains(PersonalKeyDays, days) {
		return CreatedPersonalKey{}, E(CodeBadRequest, "ttl_days must be 30, 90 or 365", nil)
	}
	var accountIDs []string
	for _, id := range req.AccountIDs {
		id = strings.TrimSpace(id)
		if id == "" || slices.Contains(accountIDs, id) {
			continue
		}
		// Only mailboxes the person may read: one they do not see is
		// answered as one that does not exist, one they see without read
		// as not theirs to hand a tool.
		if _, err := s.authorizeAccount(ctx, p, auth.ScopeRead, id, needRead); err != nil {
			return CreatedPersonalKey{}, err
		}
		accountIDs = append(accountIDs, id)
	}

	secret, key, err := s.keys.Issue(ctx, auth.NewKeyRequest{
		Name: name, Scope: scope, AccountIDs: accountIDs, UserID: p.UserID,
		TTL: time.Duration(days) * 24 * time.Hour, TermsVersion: req.TermsVersion, CreatedBy: p.Actor(),
		MaxLive: MaxPersonalKeys,
	})
	switch {
	case errors.Is(err, auth.ErrTooManyKeys):
		return CreatedPersonalKey{}, errTooManyKeys
	case errors.Is(err, auth.ErrUnknownAccount):
		// Removed between the check and the insert.
		return CreatedPersonalKey{}, E(CodeNotFound, "no such account", err)
	case errors.Is(err, auth.ErrUserNotFound):
		return CreatedPersonalKey{}, E(CodeUnauthorized, "the session has ended; sign in again", err)
	case err != nil:
		return CreatedPersonalKey{}, E(CodeInternal, "issuing the key failed", err)
	}
	s.log.Info("api key created", "user", p.UserID, "key", key.Prefix, "scope", string(key.Scope),
		"accounts", len(key.AccountIDs), "terms", key.TermsVersion)
	return CreatedPersonalKey{Key: secret, PersonalKey: presentPersonalKey(key)}, nil
}

// RevokeMyAPIKey revokes one of the signed-in person's keys. Anybody else's
// is not found; revoking one already revoked is not an error.
func (s *Service) RevokeMyAPIKey(ctx context.Context, p Principal, prefix string) error {
	if err := requireSession(p); err != nil {
		return err
	}
	switch err := s.keys.RevokeFor(ctx, prefix, p.UserID); {
	case errors.Is(err, auth.ErrNotFound):
		return errNoSuchKey
	case err != nil:
		return E(CodeInternal, "revoking the key failed", err)
	}
	s.log.Info("api key revoked", "user", p.UserID, "key", prefix)
	return nil
}

func presentPersonalKey(k auth.Key) PersonalKey {
	ids := k.AccountIDs
	if ids == nil {
		ids = []string{}
	}
	return PersonalKey{
		Prefix: k.Prefix, Name: k.Name, Scope: k.Scope, AccountIDs: ids, Restricted: k.Restricted,
		CreatedAt: unixOrZero(k.CreatedAt), ExpiresAt: unixOrZero(k.ExpiresAt),
		LastUsedAt: unixOrZero(k.LastUsedAt), RevokedAt: unixOrZero(k.RevokedAt),
		TermsVersion: k.TermsVersion,
	}
}
