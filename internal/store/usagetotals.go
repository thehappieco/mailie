package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The totals usage rule 1 keeps per mailbox (migration 0030): usage_days, the
// bytes of its message rows still in their folder and of their parts, by UTC
// day of internal_date; usage_fixed, the bytes of its folders rows and open
// drafts, which take no date. The migration's triggers move them in the
// transaction of every write to those rows. What follows fills them the
// first time (the migration leaves them empty, since migrations run SQL
// alone and the rule's terms are built here), finds a mailbox whose totals
// drifted from its rows, and repairs it, always from the rule's own column
// lists (usageTables): the rule is spelt once, in Go, and the triggers'
// text is held to it by test.
//
// A drifted total never stops a write: the tables have no constraint a
// delta could break. It is found here, outside the write path, at the
// daemon's start, in `mailserver migrate` and daily.

// utcDaySQL is the UTC day of the unix time expr, by floor division: a time
// before 1970 falls on its own day (-1 on day -1), never on day 0 as integer
// division, which truncates toward zero, would put it. Migration 0030's
// triggers spell the same expression.
func utcDaySQL(expr string) string {
	return "((" + expr + " - (((" + expr + " % 86400) + 86400) % 86400)) / 86400)"
}

// usageDaysScan reads rule 1's bytes of live message rows and their parts,
// per account and UTC day of internal_date, without a day that sums to
// nothing: of every account, or with one, of the account bound as ?1.
//
//nolint:gosec // G202: what is joined in is the rule's own column lists and fixed fragments, never input
func usageDaysScan(one bool) string {
	only := ""
	if one {
		only = " AND m.account_id = ?1"
	}
	day := utcDaySQL("m.internal_date")
	return `SELECT account_id, day, sum(bytes) FROM (
		SELECT m.account_id AS account_id, ` + day + ` AS day, ` + usageMessages.rowBytesSQL("m") + ` AS bytes
		FROM messages m WHERE ` + countedMessageSQL + only + `
		UNION ALL
		SELECT m.account_id, ` + day + `, ` + usageParts.rowBytesSQL("p") + `
		FROM messages m JOIN parts p ON p.msg_id = m.id WHERE ` + countedMessageSQL + only + `)
	GROUP BY account_id, day HAVING sum(bytes) <> 0`
}

// usageFixedScan reads rule 1's bytes of folders rows and open drafts, per
// account, without an account they sum nothing for: of every account, or
// with one, of the account bound as ?1.
//
//nolint:gosec // G202: what is joined in is the rule's own column lists and fixed fragments, never input
func usageFixedScan(one bool) string {
	folders, drafts := "", ""
	if one {
		folders, drafts = " WHERE f.account_id = ?1", " AND d.account_id = ?1"
	}
	return `SELECT account_id, sum(bytes) FROM (
		SELECT f.account_id AS account_id, ` + usageFolders.rowBytesSQL("f") + ` AS bytes FROM folders f` + folders + `
		UNION ALL
		SELECT d.account_id, ` + usageDrafts.rowBytesSQL("d") + ` FROM drafts d WHERE ` + countedDraftSQL + drafts + `)
	GROUP BY account_id HAVING sum(bytes) <> 0`
}

// checkUsageSQL lists, once each, the accounts whose kept totals differ from
// the rule's scan of their rows: a day or a fixed total the scan has and the
// totals do not, or the other way, or both with other bytes. A total of 0,
// or NULL, is a total the rule does not have, so the rows a mailbox's
// deleted mail left at 0 are no drift. Compared row by row, a total kept
// under the wrong day is found as well as a wrong one, and so are the rows
// of an account that is gone.
//
//nolint:gosec // G202: built from the scans above, constants alone
var checkUsageSQL = `WITH
	kept_days AS (SELECT account_id, day, coalesce(bytes, 0) AS bytes FROM usage_days WHERE coalesce(bytes, 0) <> 0),
	rule_days(account_id, day, bytes) AS (` + usageDaysScan(false) + `),
	kept_fixed AS (SELECT account_id, coalesce(bytes, 0) AS bytes FROM usage_fixed WHERE coalesce(bytes, 0) <> 0),
	rule_fixed(account_id, bytes) AS (` + usageFixedScan(false) + `)
	SELECT account_id FROM (SELECT account_id, day, bytes FROM kept_days EXCEPT SELECT account_id, day, bytes FROM rule_days)
	UNION SELECT account_id FROM (SELECT account_id, day, bytes FROM rule_days EXCEPT SELECT account_id, day, bytes FROM kept_days)
	UNION SELECT account_id FROM (SELECT account_id, bytes FROM kept_fixed EXCEPT SELECT account_id, bytes FROM rule_fixed)
	UNION SELECT account_id FROM (SELECT account_id, bytes FROM rule_fixed EXCEPT SELECT account_id, bytes FROM kept_fixed)
	ORDER BY account_id`

// CheckUsageTotals reads, in one snapshot (ReadTx), every account's kept
// totals and usage rule 1's scan of its rows by UTC day and for its fixed
// rows, and returns the accounts whose totals differ. It writes nothing; on
// the reader pool, it holds no write lock while it scans.
func (s *Store) CheckUsageTotals(ctx context.Context) ([]string, error) {
	tx, err := s.ReadTx(ctx)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, checkUsageSQL)
	if err != nil {
		return nil, fmt.Errorf("store: check the usage totals: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: check the usage totals: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: check the usage totals: %w", err)
	}
	return out, nil
}

// RepairUsageTotals recomputes the totals of each account from its rows, the
// scan inside the write that replaces them, one short write per account: no
// write of another can fall between what it read and what it keeps. An
// account that is gone loses whatever rows it still had. It stops at the
// first account it could not repair.
func (s *Store) RepairUsageTotals(ctx context.Context, ids []string) error {
	days, fixed := `INSERT INTO usage_days(account_id, day, bytes) `+usageDaysScan(true),
		`INSERT INTO usage_fixed(account_id, bytes) `+usageFixedScan(true)
	for _, id := range ids {
		err := s.Write(ctx, func(tx *sql.Tx) error {
			for _, step := range []struct{ what, query string }{
				{"forget the kept days", `DELETE FROM usage_days WHERE account_id = ?1`},
				{"forget the kept fixed bytes", `DELETE FROM usage_fixed WHERE account_id = ?1`},
				{"count the days", days},
				{"count the fixed bytes", fixed},
			} {
				//nolint:gosec // G202: the statements are built from the rule's column lists, constants alone
				if _, err := tx.ExecContext(ctx, step.query, id); err != nil {
					return fmt.Errorf("%s: %w", step.what, err)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("store: repair the usage totals: %w", err)
		}
	}
	return nil
}

// ReconcileUsageTotals checks every account's totals and repairs the ones
// that differ from their rows; it returns how many accounts it repaired. The
// first run after migration 0030 fills every mailbox that holds anything the
// rule counts, and a run that finds nothing to repair takes no write lock.
func (s *Store) ReconcileUsageTotals(ctx context.Context) (int, error) {
	ids, err := s.CheckUsageTotals(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if err := s.RepairUsageTotals(ctx, ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// UTCDay is the UTC day of a moment, by floor division of its unix time, as
// the totals keep a row under it (utcDaySQL): a moment before 1970 falls on
// its own day, never on day 0.
func UTCDay(t time.Time) int64 {
	unix := t.Unix()
	day := unix / 86400
	if unix%86400 < 0 {
		day--
	}
	return day
}

// MailboxBytes is what one mailbox of a workspace holds under usage rule 1,
// read from its kept totals, beside what a snapshot says of it.
type MailboxBytes struct {
	AccountID string
	// Removed is a mailbox removed and kept until PurgeAfter (unix seconds,
	// 0 on a live one).
	Removed    bool
	PurgeAfter int64
	// Pause is why a live mailbox does not sync (PauseSQL), empty when it
	// does and on a removed one, as the snapshot reads it.
	Pause string
	// Pending is a mailbox never authorized: it holds no credential yet.
	// Its first credential ends it, and nothing makes it pending again.
	Pending bool
	// Bytes is the mailbox's figure under the rule: its dated bytes and its
	// fixed bytes.
	Bytes int64
	// DatedSince is, of Bytes, what its message rows and their parts hold
	// whose internal_date falls on the UTC day asked for or after it, a
	// date ahead of today included.
	DatedSince int64
	// Fixed is, of Bytes, what its folders rows and open drafts hold, which
	// take no date.
	Fixed int64
}

// workspaceBytesSQL reads every mailbox of the workspace ?1, live and
// removed alike until it is deleted for good, the oldest first as the
// snapshot orders them, with its kept totals: every day, the days from ?2
// on, and its fixed bytes. A total that is NULL counts nothing.
//
//nolint:gosec // G202: PauseSQL is built from constants alone; every value is bound
func workspaceBytesSQL(withheld bool) string {
	return `SELECT a.id, a.removed_at <> 0, a.purge_after, ` + PauseSQL(withheld) + `,
		NOT EXISTS (SELECT 1 FROM credentials c WHERE c.account_id = a.id),
		coalesce((SELECT sum(coalesce(d.bytes, 0)) FROM usage_days d WHERE d.account_id = a.id), 0),
		coalesce((SELECT sum(coalesce(d.bytes, 0)) FROM usage_days d WHERE d.account_id = a.id AND d.day >= ?2), 0),
		coalesce((SELECT coalesce(f.bytes, 0) FROM usage_fixed f WHERE f.account_id = a.id), 0)
	  FROM accounts a WHERE a.workspace_id = ?1 ORDER BY a.created_at, a.rowid`
}

// WorkspaceBytesTx reads, inside the caller's transaction, which a read-only
// one keeps to one moment (ReadTx), every mailbox of a workspace with what
// it holds under usage rule 1, from the totals kept as its rows change,
// never a scan of the rows themselves: live and removed mailboxes alike,
// until they are deleted for good, the oldest first. Of each, the bytes
// dated on sinceDay (a UTC day, UTCDay) or after it are DatedSince. withheld
// is the edition withholding teams, as PauseSQL reads it. A workspace that
// holds no mailbox, or that nobody knows, has none; the caller asks which.
func (s *Store) WorkspaceBytesTx(ctx context.Context, tx *sql.Tx, workspaceID string, sinceDay int64, withheld bool,
) ([]MailboxBytes, error) {
	rows, err := tx.QueryContext(ctx, workspaceBytesSQL(withheld), workspaceID, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("store: read a workspace's bytes: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []MailboxBytes{}
	for rows.Next() {
		var (
			mb   MailboxBytes
			days int64
		)
		if err := rows.Scan(&mb.AccountID, &mb.Removed, &mb.PurgeAfter, &mb.Pause, &mb.Pending, &days, &mb.DatedSince,
			&mb.Fixed); err != nil {
			return nil, fmt.Errorf("store: read a workspace's bytes: %w", err)
		}
		if mb.Removed {
			// A removed mailbox is what it is: kept until PurgeAfter.
			mb.Pause = ""
		} else {
			mb.PurgeAfter = 0
		}
		mb.Bytes = days + mb.Fixed
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read a workspace's bytes: %w", err)
	}
	return out, nil
}

// followedWorkspaceBytesSQL sums the kept totals of every mailbox, live and
// removed alike, per workspace whose source is not the local one: 0 for one
// that holds nothing.
const followedWorkspaceBytesSQL = `WITH kept(workspace_id, bytes) AS (
		SELECT a.workspace_id, coalesce(d.bytes, 0) FROM accounts a JOIN usage_days d ON d.account_id = a.id
		UNION ALL
		SELECT a.workspace_id, coalesce(f.bytes, 0) FROM accounts a JOIN usage_fixed f ON f.account_id = a.id),
	per(workspace_id, bytes) AS (SELECT workspace_id, sum(bytes) FROM kept GROUP BY workspace_id)
	SELECT w.id, coalesce(p.bytes, 0) FROM workspaces w LEFT JOIN per p ON p.workspace_id = w.id
	 WHERE w.source <> 'local'`

// FollowedWorkspaceBytes sums, in one statement, every mailbox's kept total
// per workspace whose source is not local, live and removed mailboxes
// alike: what each followed workspace holds under usage rule 1, 0 for one
// that holds nothing. A local workspace is absent.
func (s *Store) FollowedWorkspaceBytes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.r.QueryContext(ctx, followedWorkspaceBytesSQL)
	if err != nil {
		return nil, fmt.Errorf("store: sum the followed workspaces' bytes: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var (
			id    string
			bytes int64
		)
		if err := rows.Scan(&id, &bytes); err != nil {
			return nil, fmt.Errorf("store: sum the followed workspaces' bytes: %w", err)
		}
		out[id] = bytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sum the followed workspaces' bytes: %w", err)
	}
	return out, nil
}
