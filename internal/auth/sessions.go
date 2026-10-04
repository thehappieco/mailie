package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A session is a signed-in browser: an opaque bearer token of 32 random bytes,
// sent in the Authorization header like a key and never in a cookie.
//
// Not a cookie because a cookie is ambient: the browser attaches it to any
// request to this origin, including one another site made it send, and that is
// the whole of CSRF. A bearer token is only ever sent by code that chose to.
// The console and the API share one origin, so there is no CORS to configure
// either.
//
// The lifetime is absolute. Nothing renews a session by using it; after
// SessionTTL the person signs in again. A console that can remove mailboxes
// should not stay open for as long as somebody keeps a tab alive.

// SessionTTL is how long a sign-in lasts.
const SessionTTL = 14 * 24 * time.Hour

const (
	sessionTokenBytes = 32
	// maxUserAgent bounds what a client can make the database store.
	maxUserAgent = 200
	// lastSeenResolution keeps the bookkeeping write off the hot path, as it
	// does for keys: a console polling every few seconds must not turn every
	// read into a write on the single writer connection.
	lastSeenResolution = time.Minute
)

// ErrInvalidSession is returned for every way a session token can fail:
// unknown, malformed, expired, revoked, or belonging to a disabled user. The
// fix is the same for all of them — sign in again — and telling them apart
// would only help somebody holding a token that is not theirs.
var ErrInvalidSession = errors.New("auth: invalid or expired session")

// Session is a signed-in browser, as the console lists it. The token is not
// here: it exists once, in the reply that issued it.
type Session struct {
	ID         string
	UserID     string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// IsAPIKey reports whether a bearer token has the shape of an API key rather
// than a session token. The dot is enough: a session token is base64url, which
// has none, and a key always does.
func IsAPIKey(token string) bool { return strings.Contains(token, ".") }

// AuthenticateSession resolves a session token to the person behind it.
//
// A SHA-256 and an indexed lookup, not Argon2id: the token is 32 random bytes,
// so there is nothing for a slow hash to protect, and a console polling for an
// OAuth flow to finish would otherwise pay 19 MiB a request.
func (u *Users) AuthenticateSession(ctx context.Context, token string) (Principal, error) {
	hash, ok := hashToken(token)
	if !ok {
		return Principal{}, ErrInvalidSession
	}
	var (
		id, userID, role, status  string
		expires, revoked, lastSee int64
	)
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT s.id, s.user_id, s.expires_at, s.revoked_at, s.last_seen_at, u.role, u.status
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, hash,
	).Scan(&id, &userID, &expires, &revoked, &lastSee, &role, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Principal{}, ErrInvalidSession
	case err != nil:
		return Principal{}, fmt.Errorf("auth: look up session: %w", err)
	}
	now := u.now()
	if !sessionLive(now, expires, revoked, status) {
		return Principal{}, ErrInvalidSession
	}
	if now.Unix()-lastSee >= int64(lastSeenResolution.Seconds()) {
		u.touchSession(ctx, id, now)
	}
	// A person has every scope over what they own. What they own is decided
	// in internal/service, not by the scope.
	return Principal{Kind: KindSession, SessionID: id, UserID: userID, UserRole: Role(role), Scope: ScopeAdmin}, nil
}

// RecheckSession re-reads a session that has already authenticated.
//
// Handlers call this immediately before writing a response, as they do
// Keys.Recheck: a sign-out in another tab, a password change or a disabled
// account must stop a request that was already running from publishing its
// answer. A role that changed mid-request stops it too, since the answer was
// computed for the old one.
func (u *Users) RecheckSession(ctx context.Context, p Principal) error {
	var (
		userID, role, status string
		expires, revoked     int64
	)
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT s.user_id, s.expires_at, s.revoked_at, u.role, u.status
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.id = ?`, p.SessionID,
	).Scan(&userID, &expires, &revoked, &role, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInvalidSession
	case err != nil:
		return fmt.Errorf("auth: recheck session: %w", err)
	}
	if userID != p.UserID || Role(role) != p.UserRole || !sessionLive(u.now(), expires, revoked, status) {
		return ErrInvalidSession
	}
	return nil
}

// Session reads one session by id.
func (u *Users) Session(ctx context.Context, id string) (Session, error) {
	var s Session
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT id, user_id, user_agent, created_at, last_seen_at, expires_at FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.UserID, &s.UserAgent,
		unixScanner{&s.CreatedAt}, unixScanner{&s.LastSeenAt}, unixScanner{&s.ExpiresAt})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, ErrInvalidSession
	case err != nil:
		return Session{}, fmt.Errorf("auth: read session: %w", err)
	}
	return s, nil
}

// EndSession revokes one session. Ending one that already ended is not an
// error: the caller asked for a state, and the state holds.
func (u *Users) EndSession(ctx context.Context, id string) error {
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, u.now().Unix(), id)
		if err != nil {
			return fmt.Errorf("auth: end session: %w", err)
		}
		return nil
	})
}

// EndAllSessions revokes every session a user has, on every device.
func (u *Users) EndAllSessions(ctx context.Context, userID string) error {
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := revokeSessionsTx(ctx, tx, userID, u.now().Unix())
		return err
	})
}

// startSessionTx issues a token inside the caller's transaction, so a session
// is created in the same commit as whatever justified it: a sign-up, a
// password change.
func startSessionTx(ctx context.Context, tx *sql.Tx, userID, userAgent string, now time.Time) (string, Session, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, fmt.Errorf("auth: read random: %w", err)
	}
	id, err := newID("ses_")
	if err != nil {
		return "", Session{}, err
	}
	sum := sha256.Sum256(raw)
	now = now.UTC().Truncate(time.Second)
	s := Session{
		ID: id, UserID: userID, UserAgent: truncateUTF8(userAgent, maxUserAgent),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(SessionTTL),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, token_hash, user_agent, created_at, last_seen_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.UserID, sum[:], s.UserAgent, now.Unix(), now.Unix(), s.ExpiresAt.Unix(),
	); err != nil {
		return "", Session{}, fmt.Errorf("auth: start session: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), s, nil
}

// revokeSessionsTx ends every live session a user has and reports how many
// it ended.
func revokeSessionsTx(ctx context.Context, tx *sql.Tx, userID string, now int64) (int, error) {
	n, err := execCount(ctx, tx,
		`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at = 0`, now, userID)
	if err != nil {
		return 0, fmt.Errorf("auth: end sessions: %w", err)
	}
	return n, nil
}

func sessionLive(now time.Time, expires, revoked int64, status string) bool {
	return revoked == 0 && now.Before(time.Unix(expires, 0)) && status == userActive
}

// hashToken decodes a presented token and returns what the database stores
// for it. Anything that is not exactly 32 bytes of base64url cannot be one.
func hashToken(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != sessionTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// touchSession records that a session was used, detached from the request for
// the same reasons Keys.touch is.
func (u *Users) touchSession(ctx context.Context, id string, now time.Time) {
	detached := context.WithoutCancel(ctx)
	go func() {
		cutoff := now.Add(-lastSeenResolution).Unix()
		//nolint:errcheck // deliberately detached and best effort
		_ = u.store.Write(detached, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(detached,
				`UPDATE sessions SET last_seen_at = ? WHERE id = ? AND last_seen_at <= ?`, now.Unix(), id, cutoff)
			return err
		})
	}()
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
