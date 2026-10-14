package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/store"
)

// Stranded mailboxes (migration 0021; docs/workspaces.md, "Removed
// mailboxes").
//
// A team mailbox is stranded when the one person who read it, would read it
// once resumed, or would read it but for a hold loses it in a way no
// protection refuses: an external directory ends their membership or forgets
// them, or they are disabled, deleted or reset with force. Nothing changes
// for the mailbox itself; the journal records it (store.MailboxStranded),
// naming the person it lost, so an extension that reports the workspace
// elsewhere can say so. Nobody is told in the console.

// Stranding is one mailbox a change left stranded, as the journal recorded
// it: Seq is its entry's.
type Stranding struct {
	AccountID   string
	WorkspaceID string
	Seq         int64
}

// JournalStrandedTx journals, inside the transaction that strands them, each
// of these mailboxes as stranded from lostUser for cause (store.CauseX), by
// actor ("usr_…", "key:<prefix>", "cli" or store.ActorDirectory), with
// lostUser as the one who read it then. It runs while lostUser still exists,
// whom the readers name, and before anything deletes the mailboxes' rows,
// which the journal reads its workspace and names from. Nobody is given a
// notice. It returns the entries, in the order of accountIDs.
func JournalStrandedTx(ctx context.Context, tx *sql.Tx, accountIDs []string, lostUser, cause, actor string, now time.Time) ([]Stranding, error) {
	out := make([]Stranding, 0, len(accountIDs))
	for _, id := range accountIDs {
		seq, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
			Kind: store.MailboxStranded, AccountID: id, Actor: actor, At: now.Unix(), Cause: cause, LostUser: lostUser,
		}, []string{lostUser}, false)
		if err != nil {
			return nil, err
		}
		var workspaceID string
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM mailbox_events WHERE seq = ?`, seq).
			Scan(&workspaceID); err != nil {
			return nil, fmt.Errorf("workspace: read the stranding back: %w", err)
		}
		out = append(out, Stranding{AccountID: id, WorkspaceID: workspaceID, Seq: seq})
	}
	return out, nil
}
