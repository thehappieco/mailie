package service

import (
	"context"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Keys an issuer makes (docs/workspaces.md, "Keys an issuer makes").
//
// An extension of the daemon may follow an issuer that makes API keys
// elsewhere, for the people of a directory it follows, and have this server
// mirror them: each one a workspace key of its creator's personal workspace,
// holding there what its issuer's grants and this server's own judgment of
// them leave it (migration 0028). Once authenticated, an issued key is an
// ordinary workspace key's principal, auth.Principal.Issuer set, and every
// rule a workspace key obeys holds for it, in REST and MCP alike, written
// once: it reaches exactly the mailboxes it holds; reads and acts, act only
// beside read; never sends; reaches no team's mailbox, whatever the edition
// says; and acts only while its creator allows actions, at the current
// revision, whatever MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT says.
//
// The core names no issuer's format. An extension registers a KeyIssuer
// (internal/app's Options.KeyIssuer, only beside ExternalSignInOnly), which
// recognises its own credentials, compares one with the hash this server
// holds, and hears each use: a credential the issuer claims never reaches
// this server's own keys or sessions, and never costs an Argon2id
// derivation.

// KeyIssuer is an extension that makes keys elsewhere and has this server
// mirror them (internal/app's Options.KeyIssuer). Its methods never block
// and do no I/O.
type KeyIssuer interface {
	// Issuer is the directory's issuer these keys belong to.
	Issuer() string
	// Claim reports whether raw — the bearer exactly as presented after the
	// scheme and one space, never trimmed — is of this issuer's family
	// (claimed: it never reaches this server's own keys or sessions), and,
	// when it is also well formed, its selector, which is not secret. It is
	// never asked about a credential of a console session's shape
	// (auth.IsSessionToken), which stays a session.
	Claim(raw string) (selector string, claimed bool)
	// Verify compares raw with hash, the one held for selector ("" when none
	// is held), in constant time, costing the same either way.
	Verify(raw, selector, hash string) bool
	// Used hears each accepted use and each refused one, once the hash
	// matched; it only counts in memory.
	Used(IssuedKeyUse)
}

// IssuedKeyUse is one use of an issued key whose secret matched: accepted,
// or refused and why.
type IssuedKeyUse struct {
	// ExternalID is the key's id at its issuer.
	ExternalID string
	At         time.Time
	// AccountID is the mailbox a use was refused on; "" for an accepted use
	// and for a refusal of the key itself.
	AccountID string
	// Use is "read", "act" or "send"; "" with AccountID "".
	Use string
	// Refused is why a use was refused; "" for an accepted one.
	Refused UseRefusal
}

// UseRefusal is why a use of an issued key was refused, in the core's words;
// the extension maps them to its issuer's.
type UseRefusal string

// Why a use of an issued key is refused.
const (
	// UseNoFlag: the key holds no such use on a mailbox of its workspace.
	UseNoFlag UseRefusal = "no_flag"
	// UseCreatorConsent: an action, while the key's creator does not allow
	// actions at the current revision.
	UseCreatorConsent UseRefusal = "creator_consent"
	// UseNotOffered: a use this server never offers an issued key (sending,
	// a team's mailbox).
	UseNotOffered UseRefusal = "not_offered"
	// UseWorkspaceClosed: a mailbox of a closed workspace.
	UseWorkspaceClosed UseRefusal = "workspace_closed"
	// UseRevoked: the key itself is revoked.
	UseRevoked UseRefusal = "revoked"
	// UseExpired: the key itself is past its expiry by this server's clock.
	UseExpired UseRefusal = "expired"
	// UseHeld: the key itself is stopped here (an operator's hold, its
	// creator stopped here, or every issued key refused by the edition).
	UseHeld UseRefusal = "held"
)

// UseRefusals lists every UseRefusal, as migration 0028 counts them.
func UseRefusals() []UseRefusal {
	return []UseRefusal{UseNoFlag, UseCreatorConsent, UseNotOffered, UseWorkspaceClosed, UseRevoked, UseExpired, UseHeld}
}

// MaxBearerLen bounds a bearer credential: one longer is an invitation to make
// the server hash something enormous.
const MaxBearerLen = 4096

// Bearer reads the credential of a request from its Authorization header's
// values, as net/http hands them (http.Header's Values): exactly one value,
// "Bearer" in any case, one space, and the rest, at most MaxBearerLen bytes,
// returned exactly as presented, never trimmed: an issuer's credential is
// checked on the bytes the client sent (a space more is not the same
// credential). One that is only spaces and tabs is none. Every transport
// reads the bearer here; Authenticate and AuthenticateTool take what it
// returns.
func Bearer(values []string) (raw string, ok bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, raw, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(raw) > MaxBearerLen || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

// BearerRefused is the answer to a request whose Authorization header Bearer
// refused: when any of its values carries a credential the issuer claims —
// after its scheme and a space or a tab, or with no scheme at all — the one
// answer every refusal of an issued key gets (invalid or expired api key),
// so that an issuer's credential is refused alike however its header was
// malformed; otherwise the transport's own, otherwise.
func (s *Service) BearerRefused(values []string, otherwise error) error {
	if s.issuer == nil {
		return otherwise
	}
	for _, v := range values {
		w := strings.TrimLeft(v, " \t")
		if _, claimed := s.claim(w); claimed {
			return errInvalidIssuedKey
		}
		if i := strings.IndexAny(w, " \t"); i >= 0 {
			if _, claimed := s.claim(w[i+1:]); claimed {
				return errInvalidIssuedKey
			}
		}
	}
	return otherwise
}

// errInvalidIssuedKey is every refusal of an issued key: fromCredential's one
// answer for a key.
var errInvalidIssuedKey = E(CodeUnauthorized, "invalid or expired api key", auth.ErrInvalidKey)

// claim asks the issuer whether raw is one of its credentials, never about
// one of a session token's shape.
func (s *Service) claim(raw string) (selector string, claimed bool) {
	if s.issuer == nil || auth.IsSessionToken(strings.TrimSpace(raw)) {
		return "", false
	}
	return s.issuer.Claim(raw)
}

// CredentialSubject is the subject a credential's failures are charged to by
// a transport's rate limit, reserved before it is checked and given back once
// it proves good: "key:" and an issued key's selector, or an own key's
// prefix; "" for a session token, which has no public part, and for a
// credential whose selector cannot be read, which the address's limit on
// keys that match nothing charges instead.
func (s *Service) CredentialSubject(raw string) string {
	if selector, claimed := s.claim(raw); claimed {
		if selector == "" {
			return ""
		}
		return "key:" + selector
	}
	if prefix, _, ok := auth.SplitKey(strings.TrimSpace(raw)); ok {
		return "key:" + prefix
	}
	return ""
}

// authenticateIssued checks a credential the issuer claimed (auth's
// AuthenticateIssued): one indexed read and the issuer's constant-time
// comparison, never Argon2id, and one answer for every refusal. Once its
// secret matched, the issuer hears the use, accepted or refused.
func (s *Service) authenticateIssued(ctx context.Context, raw, selector string, unknownKey auth.Gate) (Principal, error) {
	issuer := s.issuer.Issuer()
	p, check, err := s.keys.AuthenticateIssued(ctx, issuer, selector, func(hash string) bool {
		return s.issuer.Verify(raw, selector, hash)
	}, unknownKey, s.issuedKeysRefused)
	if check.ExternalID != "" {
		use := IssuedKeyUse{ExternalID: check.ExternalID, At: s.now()}
		switch check.Outcome {
		case auth.IssuedRevoked:
			use.Refused = UseRevoked
		case auth.IssuedExpired:
			use.Refused = UseExpired
		case auth.IssuedHeld:
			use.Refused = UseHeld
		}
		s.issuer.Used(use)
	}
	if err != nil {
		return Principal{}, fromCredential(err)
	}
	return p, nil
}

// issuedUseRefused tells the issuer that a use of an issued key was refused
// on a mailbox: for a principal of an issued key only, and only from the one
// place each refusal is decided. need names the use (read for a mailbox's
// card).
func (s *Service) issuedUseRefused(ctx context.Context, p Principal, accountID string, need workspace.Flags, why UseRefusal) {
	if p.Issuer == "" || s.issuer == nil || s.keys == nil || accountID == "" {
		return
	}
	id, err := s.keys.IssuedExternalID(ctx, p.Issuer, p.KeyPrefix)
	if err != nil {
		s.log.Warn("reading an issued key to report a refused use failed", "key", p.KeyPrefix, "err", err)
		return
	}
	if id == "" {
		return
	}
	s.issuer.Used(IssuedKeyUse{ExternalID: id, At: s.now(), AccountID: accountID, Use: useOf(need), Refused: why})
}

// useOf names the use an operation needs, as a refusal reports it.
func useOf(need workspace.Flags) string {
	switch {
	case need.Send:
		return "send"
	case need.Act:
		return "act"
	default:
		return "read"
	}
}

// issuedNotFound tells the issuer that an issued key asked for a mailbox it
// holds nothing on, answered not_found: only for a live mailbox of the key's
// own workspace (one indexed read), so that nobody can put another
// workspace's mailbox, or a made-up id, in what the issuer hears.
func (s *Service) issuedNotFound(ctx context.Context, p Principal, accountID string, need workspace.Flags) {
	if p.Issuer == "" || s.issuer == nil || s.store == nil || p.WorkspaceID == "" {
		return
	}
	var ours bool
	if err := s.store.Reader().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts
		WHERE id = ? AND workspace_id = ? AND removed_at = 0)`, accountID, p.WorkspaceID).Scan(&ours); err != nil {
		s.log.Warn("reading a mailbox to report a refused use failed", "key", p.KeyPrefix, "err", err)
		return
	}
	if ours {
		s.issuedUseRefused(ctx, p, accountID, need, UseNoFlag)
	}
}

// Errors of this server's own keys where an issuer makes them.
var errKeysIssuedElsewhere = E(CodeNotAuthorized,
	"this server's API keys are made by their issuer; none is created or given more here", nil)

// OwnKeysRevoked is what RevokeOwnWorkspaceKeys stopped.
type OwnKeysRevoked = auth.OwnKeysRevoked

// RevokeOwnWorkspaceKeys revokes every live key of this server's own outside
// the operator workspace, saying it was revoked (manual), and reports how
// many, how many of them had not expired, and when: what the daemon does at
// start where an edition's keys are made by their issuer
// (Deps.KeysIssuedElsewhere), before anything listens. The operator's keys,
// which the command line holds, and the issued keys are untouched; a second
// start stops none.
func (s *Service) RevokeOwnWorkspaceKeys(ctx context.Context) (OwnKeysRevoked, error) {
	out, err := s.keys.RevokeWorkspaceKeys(ctx)
	if err != nil {
		return OwnKeysRevoked{}, E(CodeInternal, "revoking the workspace keys failed", err)
	}
	if out.Revoked > 0 {
		s.accessChanged()
	}
	return out, nil
}
