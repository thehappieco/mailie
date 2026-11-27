package store

import (
	"context"
	"database/sql"
	"fmt"
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
