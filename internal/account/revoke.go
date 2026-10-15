package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
)

// Revoking a mailbox's grant at its provider once the mailbox is deleted for
// good (docs/workspaces.md, "Removed mailboxes"): the 30 days over, its
// owners or the operator deleting it before, the person it goes with deleted
// or forgotten. Where the provider offers a revocation endpoint — Google's,
// for the refresh token (RFC 7009) — the grant is revoked there, best
// effort, after the deletion commits, with a timeout, and its failure only
// logged, never the token. Microsoft's identity platform revokes no single
// grant (only every session of the person), and a password has nothing to
// revoke: the stored credential is deleted, and lapses.
//
// The token is opened inside the transaction that deletes it, and handed to
// the step after the commit in memory only. A grant another mailbox here
// still holds — the same Google address linked again elsewhere through the
// same OAuth client, which Google keeps one grant for — is not revoked: that
// would cut the other mailbox off.

// GoogleRevokeEndpoint is Google's OAuth 2.0 revocation endpoint.
const GoogleRevokeEndpoint = "https://oauth2.googleapis.com/revoke"

// revokeTimeout bounds opening the tokens of a deletion, and revoking them
// after it.
const revokeTimeout = 10 * time.Second

// grantToRevoke is a grant a deletion for good leaves to revoke at the
// provider once it commits: the refresh token, in memory only, or why it
// could not be opened.
type grantToRevoke struct {
	accountID string
	token     []byte
	// unopened is why the token could not be opened, for the log: nothing is
	// revoked then.
	unopened error
}

// grantsToRevokeTx reads, inside the transaction that deletes these
// mailboxes for good and before it does, the Google grants to revoke once it
// commits: every Google mailbox among them with an OAuth token, whose grant
// no other mailbox here holds. A token that does not open is recorded as
// such, and the deletion goes on.
func (r *Repository) grantsToRevokeTx(ctx context.Context, tx *sql.Tx, ids []string) ([]grantToRevoke, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("account: encode ids: %w", err)
	}
	sealed, err := googleTokensTx(ctx, tx, string(list))
	if err != nil {
		return nil, err
	}
	if len(sealed) == 0 {
		return nil, nil
	}
	openCtx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	out := make([]grantToRevoke, 0, len(sealed))
	for _, s := range sealed {
		g := grantToRevoke{accountID: s.accountID}
		plaintext, err := r.sealer.Open(openCtx, secrets.Credential(s.accountID, "oauth_token"), s.ciphertext)
		if err != nil {
			g.unopened = err
			out = append(out, g)
			continue
		}
		var token oauth2.Token
		err = json.Unmarshal(plaintext, &token)
		clear(plaintext)
		switch {
		case err != nil:
			g.unopened = fmt.Errorf("account: decode the stored token: %w", err)
		case token.RefreshToken == "":
			// Nothing a revocation could take back: the access token lapses
			// within the hour.
			continue
		default:
			g.token = []byte(token.RefreshToken)
		}
		out = append(out, g)
	}
	return out, nil
}

// sealedToken is a Google mailbox's stored token, sealed.
type sealedToken struct {
	accountID  string
	ciphertext []byte
}

// googleTokensTx reads the sealed tokens of the Google mailboxes in list (a
// JSON array of ids) whose grant no mailbox outside it holds.
func googleTokensTx(ctx context.Context, tx *sql.Tx, list string) ([]sealedToken, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id, c.ciphertext FROM accounts a
		  JOIN credentials c ON c.account_id = a.id AND c.field = 'oauth_token'
		 WHERE a.id IN (SELECT value FROM json_each(?1)) AND a.provider = ?2 AND a.auth_kind = 'oauth2'
		   AND NOT EXISTS (SELECT 1 FROM accounts o JOIN credentials oc ON oc.account_id = o.id AND oc.field = 'oauth_token'
		                    WHERE o.id NOT IN (SELECT value FROM json_each(?1)) AND o.provider = a.provider
		                      AND o.auth_kind = 'oauth2' AND o.oauth_client = a.oauth_client
		                      AND lower(o.email) = lower(a.email))
		 ORDER BY a.id`, list, string(provider.KindGmail))
	if err != nil {
		return nil, fmt.Errorf("account: read the grants to revoke: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []sealedToken
	for rows.Next() {
		var s sealedToken
		if err := rows.Scan(&s.accountID, &s.ciphertext); err != nil {
			return nil, fmt.Errorf("account: read the grants to revoke: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: read the grants to revoke: %w", err)
	}
	return out, nil
}

// revokes reports whether a deletion for good leaves grants to revoke.
func (r *Registry) revokes() bool { return r.googleRevokeURL != "" }

// forgetGrants clears the tokens of a deletion that did not commit.
func forgetGrants(grants []grantToRevoke) {
	for _, g := range grants {
		clear(g.token)
	}
}

// revokeClient is the client a revocation is sent with: no proxy, no
// redirect followed, every connection checked at dial time against private
// addresses unless the deployment allows them, and bounded.
func (r *Registry) revokeClient() *http.Client {
	dialer := &net.Dialer{Timeout: revokeTimeout, Control: r.dialControl()}
	return &http.Client{
		Timeout: revokeTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: revokeTimeout,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// revokeGrants revokes, once their deletion has committed, the grants it left
// to revoke, best effort: each failure is logged, without its token, and
// nothing is retried. The tokens are cleared from memory once sent. Without
// a revocation endpoint, nothing is sent.
func (r *Registry) revokeGrants(ctx context.Context, grants []grantToRevoke) {
	if len(grants) == 0 {
		return
	}
	defer forgetGrants(grants)
	if r.googleRevokeURL == "" {
		return
	}
	// The deletion committed: a caller that went away meanwhile does not
	// take the revocation with it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	client := r.revokeClient()
	defer client.CloseIdleConnections()
	for _, g := range grants {
		if g.unopened != nil {
			r.log.Warn("a mailbox deleted for good kept its grant at Google: its token did not open, so it was not revoked",
				"account", g.accountID, "err", g.unopened)
			continue
		}
		status, err := r.revokeGoogle(ctx, client, g.token)
		switch {
		case err != nil:
			r.log.Warn("revoking a deleted mailbox's grant at Google failed; it lapses when its person removes it",
				"account", g.accountID, "err", err)
		case status == http.StatusOK:
			r.log.Info("revoked a deleted mailbox's grant at Google", "account", g.accountID)
		case status == http.StatusBadRequest:
			// Google's answer for a token it no longer holds: revoked by
			// its person, or expired.
			r.log.Info("a deleted mailbox's grant at Google was already gone", "account", g.accountID)
		default:
			r.log.Warn("Google refused to revoke a deleted mailbox's grant", "account", g.accountID, "status", status)
		}
	}
}

// revokeGoogle sends one revocation, and returns the status Google answered.
func (r *Registry) revokeGoogle(ctx context.Context, client *http.Client, token []byte) (int, error) {
	body := url.Values{"token": {string(token)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.googleRevokeURL, strings.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("account: build the revocation: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		// The error may quote the request; never its body, which holds the
		// token, but say only what failed.
		var ue *url.Error
		if errors.As(err, &ue) {
			return 0, fmt.Errorf("account: send the revocation: %w", ue.Err)
		}
		return 0, errors.New("account: send the revocation failed")
	}
	//nolint:errcheck // the answer is read for its status alone
	defer func() { _ = resp.Body.Close() }()
	//nolint:errcheck // drained so the connection is let go; nothing in it is used
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, nil
}
