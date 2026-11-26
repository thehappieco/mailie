package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// TestTheUsageRuleCoversEveryColumnOfTheTablesItCounts holds the rule's
// column lists to the migrated schema: a migration that adds, drops or
// renames a column of messages, parts, folders or drafts fails here until
// usageTables says whether and how it counts (and RuleVersion moves with it
// when the figure of rows already stored changes).
func TestTheUsageRuleCoversEveryColumnOfTheTablesItCounts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mail.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if len(usageTables) != 4 {
		t.Fatalf("the rule counts %d tables; rule 1 counts messages, parts, folders and drafts", len(usageTables))
	}
	for _, table := range usageTables {
		rows, err := s.Reader().QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table.name)
		if err != nil {
			t.Fatal(err)
		}
		var schema []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			schema = append(schema, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(schema, table.columns) {
			t.Errorf("%s has the columns %v, and the usage rule counts %v: list every column the "+
				"migrations leave, and decide whether the figure needs a new RuleVersion", table.name, schema, table.columns)
		}
	}
}
