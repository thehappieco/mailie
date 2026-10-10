package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0014: mailbox keys and the grants sealed to people
// (docs/key-scheme.md sections 8 and 9).

func TestMigrationFourteenAddsTheKeyTablesEmptyAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 13)
	before := dumpTables(t, s)
	migrateTo(t, s, 14)
	after := dumpTables(t, s)

	for table, rows := range before {
		got, ok := after[table]
		if !ok {
			t.Errorf("table %s is gone", table)
			continue
		}
		if len(got) != len(rows) {
			t.Errorf("%s held %d rows and holds %d", table, len(rows), len(got))
			continue
		}
		for i, row := range rows {
			if len(got[i]) != len(row) {
				t.Errorf("%s row %d has %d columns, had %d", table, i, len(got[i]), len(row))
			}
			for col, want := range row {
				if got[i][col] != want {
					t.Errorf("%s row %d column %s was %s and is %s", table, i, col, want, got[i][col])
				}
			}
		}
	}
	var added []string
	for table, rows := range after {
		if _, ok := before[table]; ok {
			continue
		}
		added = append(added, table)
		if len(rows) != 0 {
			t.Errorf("%s holds %d rows; every mailbox stays without a key", table, len(rows))
		}
	}
	slices.Sort(added)
	if want := []string{"mailbox_grants", "mailbox_keys"}; !slices.Equal(added, want) {
		t.Errorf("0014 added the tables %v, want %v", added, want)
	}
	if views := queryInt(t, s, `SELECT count(*) FROM sqlite_schema WHERE type = 'view'`); views != 0 {
		t.Errorf("0014 left %d views, which a later rebuild would refuse", views)
	}
}

// Two lowercase UUIDv4 namespaces.
const (
	namespaceOne = "9d035f2b-81d0-420e-90e2-bb16e950497b"
	namespaceTwo = "b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4"
)

// grantShape is 88 bytes starting with a grant's header at epoch: what the
// schema holds of a grant is its length and its magic.
func grantShape(t *testing.T, epoch int) []byte {
	t.Helper()
	g := make([]byte, 88)
	if _, err := rand.Read(g); err != nil {
		t.Fatal(err)
	}
	copy(g, []byte{0x4d, 0x4c, 0x01, 0x01, 0x01, byte(epoch >> 8), byte(epoch), 0x00})
	return g
}

func TestTheMailboxKeyTablesRefuseWhatTheSchemaForbids(t *testing.T) {
	// The seed at schema 14: acc_t1 is Support's (wsp_t1), where Bea
	// (usr_2) is an admin holding every flag; acc_t2 is Support's too;
	// acc_0000000000000005 is the operator's; Dan (usr_4) is not a member
	// of Support.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 14)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	pub := bytes.Repeat([]byte{7}, 32)
	key := func(account string, epoch int, namespace string) error {
		return exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by, created_at)
			VALUES (?, ?, ?, ?, 'usr_0000000000000002', 500)`, account, epoch, pub, namespace)
	}
	grant := func(account, workspace, user string, epoch int, blob []byte) error {
		return exec(`INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch, grant, granted_by, created_at)
			VALUES (?, ?, ?, ?, ?, 'usr_0000000000000002', 500)`, account, workspace, user, epoch, blob)
	}
	refused := func(what string, err error, says string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want a refusal saying %q", what, err, says)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// Epochs: 1 first, then each the next, never skipped nor repeated.
	refused("a first key at epoch 2", key("acc_t1", 2, namespaceOne), "epoch")
	accepted("a first key at epoch 1", key("acc_t1", 1, namespaceOne))
	refused("epoch 1 again", key("acc_t1", 1, namespaceOne), "epoch")
	refused("a skipped epoch", key("acc_t1", 3, namespaceOne), "epoch")
	refused("epoch 0", key("acc_t2", 0, namespaceTwo), "epoch")
	// One namespace per mailbox, its own.
	refused("a second namespace for a mailbox", key("acc_t1", 2, namespaceTwo), "one namespace")
	refused("the namespace of another mailbox", key("acc_t2", 1, namespaceOne), "no other mailbox")
	refused("a namespace outside its spelling", key("acc_t2", 1, strings.ToUpper(namespaceTwo)), "CHECK")
	accepted("the next epoch, with the mailbox's namespace", key("acc_t1", 2, namespaceOne))
	// An operator mailbox has no key.
	refused("an operator mailbox's key", key("acc_0000000000000005", 1, namespaceTwo), "operator")
	refused("a public key of another length", exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace,
		created_at) VALUES ('acc_t2', 1, ?, ?, 500)`, pub[:31], namespaceTwo), "CHECK")

	// Written once; only the writer's name may go, blanked.
	refused("a key's public key changed", exec(`UPDATE mailbox_keys SET public_key = ? WHERE account_id = 'acc_t1'`,
		bytes.Repeat([]byte{8}, 32)), "written once")
	refused("a key's namespace changed", exec(`UPDATE mailbox_keys SET namespace = ? WHERE account_id = 'acc_t1'`,
		namespaceTwo), "written once")
	refused("a key's writer renamed", exec(`UPDATE mailbox_keys SET created_by = 'usr_0000000000000001'`), "written once")
	accepted("a key's writer blanked", exec(`UPDATE mailbox_keys SET created_by = '' WHERE account_id = 'acc_t1'`))

	// Grants: for a member of the mailbox's workspace, at an epoch it has.
	accepted("Bea's grant at epoch 2", grant("acc_t1", "wsp_t1", "usr_0000000000000002", 2, grantShape(t, 2)))
	refused("a second grant for the same epoch", grant("acc_t1", "wsp_t1", "usr_0000000000000002", 2, grantShape(t, 2)),
		"UNIQUE")
	refused("a grant whose key row does not exist", grant("acc_t1", "wsp_t1", "usr_0000000000000006", 3, grantShape(t, 3)),
		"FOREIGN KEY")
	refused("a grant on a mailbox without a key", grant("acc_t2", "wsp_t1", "usr_0000000000000001", 1, grantShape(t, 1)),
		"FOREIGN KEY")
	refused("a grant for someone who is not a member", grant("acc_t1", "wsp_t1", "usr_0000000000000004", 2, grantShape(t, 2)),
		"FOREIGN KEY")
	refused("a grant naming another workspace", grant("acc_t1", "wsp_t2", "usr_0000000000000002", 1, grantShape(t, 1)),
		"FOREIGN KEY")
	refused("a grant of another length", grant("acc_t1", "wsp_t1", "usr_0000000000000006", 2, grantShape(t, 2)[:87]), "CHECK")
	wappie := grantShape(t, 2)
	copy(wappie, "WP")
	refused("a grant without Mailie's magic", grant("acc_t1", "wsp_t1", "usr_0000000000000006", 2, wappie), "CHECK")
	refused("a grant changed", exec(`UPDATE mailbox_grants SET grant = ?`, grantShape(t, 2)), "written once")
	refused("a grant moved to another epoch", exec(`UPDATE mailbox_grants SET epoch = 1`), "written once")
	refused("a grant's giver renamed", exec(`UPDATE mailbox_grants SET granted_by = 'usr_0000000000000001'`), "written once")
	accepted("a grant's giver blanked", exec(`UPDATE mailbox_grants SET granted_by = ''`))

	// Removing the membership, or the mailbox, takes what was sealed with
	// it.
	accepted("Fay's grant", grant("acc_t1", "wsp_t1", "usr_0000000000000006", 2, grantShape(t, 2)))
	accepted("Fay leaves Support", exec(`DELETE FROM workspace_members WHERE workspace_id = 'wsp_t1'
		AND user_id = 'usr_0000000000000006'`))
	if n := queryInt(t, s, `SELECT count(*) FROM mailbox_grants WHERE user_id = 'usr_0000000000000006'`); n != 0 {
		t.Errorf("a removed member keeps %d grants", n)
	}
	accepted("the mailbox removed", exec(`DELETE FROM accounts WHERE id = 'acc_t1'`))
	if n := queryInt(t, s, `SELECT (SELECT count(*) FROM mailbox_keys) + (SELECT count(*) FROM mailbox_grants)`); n != 0 {
		t.Errorf("a removed mailbox left %d key and grant rows", n)
	}
	// Its namespace is free again: nothing names it.
	accepted("the namespace of a removed mailbox", key("acc_t2", 1, namespaceOne))
}
