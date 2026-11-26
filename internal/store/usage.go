package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// RuleVersion is the version of the rule Usage counts a mailbox's bytes by.
// The figure is logical and deterministic: the same rows give the same bytes
// on every server and after every VACUUM, whatever SQLite's pages hold.
//
// Rule 1, per mailbox, sums every value of:
//
//   - each messages row still in its folder (vanished_at = 0): one row per
//     copy of a message in a folder, so a Gmail message under three synced
//     labels is three rows, and counts three times;
//   - the parts rows of those messages;
//   - the mailbox's folders rows;
//   - its open drafts rows (state = 'open'). Nothing writes a draft yet
//     (the table is the planned draft → send path's), so this adds nothing
//     today.
//
// A value counts octet_length for TEXT and BLOB (its bytes in the
// database's UTF-8, never its characters), 8 for a non-null INTEGER or REAL,
// and 0 for NULL. Nothing else counts: not the full-text index's shadow
// tables nor any index's overhead, not the bodies and attachments fetched on
// demand (a cache, LRU bounded), not sends, the events journal, the mailbox
// journal, notices, keys or grants.
//
// The columns counted are listed in usageTables, never read from the schema
// at run time: a migration that adds a column to one of these tables fails
// TestTheUsageRuleCoversEveryColumnOfTheTablesItCounts until the column is
// listed, so that what a mailbox's figure means changes only by a decision,
// with a new RuleVersion when the figure of rows already stored changes.
const RuleVersion = 1

// MailboxUsage is what the index holds for one mailbox.
type MailboxUsage struct {
	// Messages counts the rows still in their folder (not vanished): a copy
	// of a message in each folder it is in.
	Messages int64
	// Bytes is the mailbox's figure under RuleVersion: what this database
	// holds for it, counted logically.
	Bytes int64
	// ProviderBytes sums the messages' sizes as the mail server reported
	// them (RFC822.SIZE), per copy: their size on the mail server, not what
	// this database keeps of them, which is their metadata.
	ProviderBytes int64
}

// usageTable is one table the rule counts, with its columns in the schema's
// order.
type usageTable struct {
	name    string
	columns []string
}

// The tables and columns rule 1 counts, as the migrations leave them (0001,
// and 0003 for folders' last four).
var (
	usageMessages = usageTable{"messages", []string{
		"id", "account_id", "folder_id", "uidvalidity", "uid", "modseq", "message_id", "group_key", "dup_of",
		"in_reply_to", "references_json", "subject", "from_json", "to_json", "cc_json", "bcc_json",
		"reply_to_json", "from_addr", "from_text", "to_text", "date", "internal_date", "size", "flags_json",
		"seen", "flagged", "answered", "draft", "deleted", "has_attachments", "snippet", "body_fetched_at",
		"vanished_at", "stale", "first_seen_at", "updated_at", "body_text",
	}}
	usageParts = usageTable{"parts", []string{
		"msg_id", "path", "mime_type", "charset", "encoding", "disposition", "filename", "content_id", "size",
		"is_body", "is_attachment", "sha256",
	}}
	usageFolders = usageTable{"folders", []string{
		"id", "account_id", "name", "display_name", "delim", "attrs_json", "role", "role_source", "selectable",
		"synced", "passive", "uidvalidity", "uidnext", "max_seen_uid", "highest_modseq", "backfill_floor",
		"backfill_cursor", "last_full_uid_scan_at", "last_flag_scan_at", "server_count", "local_count",
		"unseen_count", "sync_state", "sync_error", "last_synced_at", "missing_since", "initial_total",
		"initial_fetched", "initial_since", "resync_from",
	}}
	usageDrafts = usageTable{"drafts", []string{
		"id", "account_id", "compose_json", "compose_hash", "message_id_hdr", "state", "error", "created_by",
		"created_at", "updated_at", "sent_at", "expires_at",
	}}
	usageTables = []usageTable{usageMessages, usageParts, usageFolders, usageDrafts}
)

// rowBytesSQL is the expression of one row's bytes under the rule, for t
// aliased alias: the sum over its columns of each value's bytes.
func (t usageTable) rowBytesSQL(alias string) string {
	terms := make([]string, len(t.columns))
	for i, c := range t.columns {
		v := alias + "." + c
		terms[i] = "CASE typeof(" + v + ") WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 " +
			"ELSE octet_length(" + v + ") END"
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// usageSQL reads, for each account in the JSON array ?1 that holds anything
// the rule counts, its live message rows, the sum of their RFC822.SIZE and
// its bytes under the rule. One statement, so one snapshot: the figure never
// mixes rows from before and after a write.
//
//nolint:gosec // G202: what is joined in is the rule's own column lists, fixed in this file, never input
var usageSQL = `WITH ids(id) AS (SELECT DISTINCT value FROM json_each(?1)),
	um AS (SELECT m.account_id AS id, count(*) AS n, sum(m.size) AS provider, sum(` + usageMessages.rowBytesSQL("m") + `) AS bytes
	       FROM messages m WHERE m.account_id IN (SELECT id FROM ids) AND m.vanished_at = 0 GROUP BY m.account_id),
	up AS (SELECT m.account_id AS id, sum(` + usageParts.rowBytesSQL("p") + `) AS bytes
	       FROM messages m JOIN parts p ON p.msg_id = m.id
	       WHERE m.account_id IN (SELECT id FROM ids) AND m.vanished_at = 0 GROUP BY m.account_id),
	uf AS (SELECT f.account_id AS id, sum(` + usageFolders.rowBytesSQL("f") + `) AS bytes
	       FROM folders f WHERE f.account_id IN (SELECT id FROM ids) GROUP BY f.account_id),
	ud AS (SELECT d.account_id AS id, sum(` + usageDrafts.rowBytesSQL("d") + `) AS bytes
	       FROM drafts d WHERE d.account_id IN (SELECT id FROM ids) AND d.state = 'open' GROUP BY d.account_id)
	SELECT ids.id, coalesce(um.n, 0), coalesce(um.provider, 0),
	       coalesce(um.bytes, 0) + coalesce(up.bytes, 0) + coalesce(uf.bytes, 0) + coalesce(ud.bytes, 0)
	FROM ids LEFT JOIN um ON um.id = ids.id LEFT JOIN up ON up.id = ids.id
	         LEFT JOIN uf ON uf.id = ids.id LEFT JOIN ud ON ud.id = ids.id
	WHERE um.id IS NOT NULL OR uf.id IS NOT NULL OR ud.id IS NOT NULL`

// Usage reports, for each account named, its index rows not vanished from
// their folder, the sum of their sizes on the mail server and its bytes under
// the rule (RuleVersion). An account that holds nothing the rule counts is
// absent from the map. A removed mailbox is counted as any other: it keeps
// its index, and counts, until it is deleted for good (or its index is
// deleted before).
//
// Counted per row, and a row is one copy of a message in one folder: a Gmail
// message under three synced labels is three rows, and counts three times,
// in both figures. The rows of one message are grouped for listing by
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
	//nolint:gosec // G202: usageSQL is built from the column lists above, constants alone; the ids are bound
	rows, err := s.r.QueryContext(ctx, usageSQL, string(list))
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
		if err := rows.Scan(&id, &u.Messages, &u.ProviderBytes, &u.Bytes); err != nil {
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
