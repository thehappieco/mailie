package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LinksOf names the links at the provider (migration 0026) a change ends:
// the ones a person started, the ones into a workspace, or a person's into a
// workspace. Every field set narrows them.
type LinksOf struct {
	// StartedBy is the person who started them; empty is anyone.
	StartedBy string
	// Into is the team they link into; empty is any workspace, the starter's
	// personal one included, whose links name none.
	Into string
	// StartedAtOrBefore is a moment, in unix seconds: only the links started
	// within its second or before it end, as only such sessions do when an
	// external directory revokes them. Zero is any.
	StartedAtOrBefore int64
}

// ErrNoLinksNamed is EndLinksTx asked to end the links of nobody and of no
// workspace, which would be every link there is.
var ErrNoLinksNamed = errors.New("store: ending links needs the person or the workspace they are of")

// EndLinksTx ends, inside the caller's transaction, the links at the provider
// of names: their rows go, whatever they hold, so a return that comes back
// later finds nothing to take, and the transaction that would create a
// mailbox for one already under way finds no link to finish and creates
// nothing (internal/account, links.go). It runs in every transaction that
// ends what a link needs: its starter disabled, suspended or closed, leaving
// the team or no longer administering it, the team closing, their account
// recovered or reset, their sessions ended everywhere.
func EndLinksTx(ctx context.Context, tx *sql.Tx, names LinksOf) error {
	if names.StartedBy == "" && names.Into == "" {
		return ErrNoLinksNamed
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_links
		WHERE (?1 = '' OR owner_user_id = ?1) AND (?2 = '' OR workspace_id = ?2) AND (?3 = 0 OR created_at <= ?3)`,
		names.StartedBy, names.Into, names.StartedAtOrBefore); err != nil {
		return fmt.Errorf("store: end the links at the provider: %w", err)
	}
	return nil
}
