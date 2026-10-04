package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// MailboxUsage is what the index holds for one mailbox.
type MailboxUsage struct {
	// Messages counts the rows still in their folder (not vanished).
	Messages int64
	// Bytes sums their sizes, RFC822.SIZE as the server reported it: the
	// messages' size on the mail server, not what this database keeps of
	// them, which is their metadata.
	Bytes int64
}

// Usage reports, for each account named, the index rows not vanished from
// their folder and the sum of their sizes. An account with nothing indexed is
// absent from the map.
//
// Counted per row, and a row is one copy of a message in one folder: a Gmail
// message under three synced labels is three rows, and counts three times,
// size included. The rows of one message are grouped for listing by
// Message-ID, or by subject, sender and date when it has none (group_key),
// which is a guess that serves a list and would not serve a count: two
// messages can share a Message-ID, and on a generic IMAP server each copy
// is a message the server stores again.
func (s *Store) Usage(ctx context.Context, accountIDs []string) (map[string]MailboxUsage, error) {
	out := make(map[string]MailboxUsage, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("store: encode ids: %w", err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT account_id, count(*), coalesce(sum(size), 0) FROM messages
		WHERE account_id IN (SELECT value FROM json_each(?)) AND vanished_at = 0
		GROUP BY account_id`, string(list))
	if err != nil {
		return nil, fmt.Errorf("store: read usage: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id string
			u  MailboxUsage
		)
		if err := rows.Scan(&id, &u.Messages, &u.Bytes); err != nil {
			return nil, fmt.Errorf("store: read usage: %w", err)
		}
		out[id] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read usage: %w", err)
	}
	return out, nil
}

// DiskBytes is the size of the database on disk: the file and its
// write-ahead log, which holds pages not yet checkpointed into it. Read from
// the paths the store was opened with, never from anything a caller names.
func (s *Store) DiskBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		info, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist) && p != s.path:
			// No log right now: everything is in the file.
		case err != nil:
			return 0, fmt.Errorf("store: size of the database: %w", err)
		default:
			total += info.Size()
		}
	}
	return total, nil
}
