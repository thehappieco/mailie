package store

import (
	"testing"
)

// Migration 0024: when a person last withdrew their consent to actions or to
// sending.

func TestMigrationTwentyFourAddsTheWithdrawalsAndChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 23)
	seedJournal21(t, f)
	// One person allows both, one only actions; the others never answered,
	// or withdrew before the moment was kept, which reads the same.
	f.accepted("consents", f.exec(`UPDATE users SET actions_consent_at = 900, actions_consent_version = 'a1',
		send_consent_at = 910, send_consent_version = 's1' WHERE id = 'usr_0000000000000001'`))
	f.accepted("a consent to actions", f.exec(`UPDATE users SET actions_consent_at = 920, actions_consent_version = 'a1'
		WHERE id = 'usr_0000000000000002'`))
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 24)

	// Nobody has withdrawn anything the schema knows of.
	tried := compareMigrated(t, before, dumpTables(t, f.s), map[string]map[string]addedColumn{
		"users": {"actions_consent_withdrawn_at": sameValue("int64:0"), "send_consent_withdrawn_at": sameValue("int64:0")},
	}, nil)
	if tried["users"] < 3 {
		t.Errorf("the seed tried the migration on %v", tried)
	}
	// One table gained columns, which ALTER TABLE writes into its text;
	// nothing was added beside them, nothing else changed, nothing went.
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	if len(added) != 0 || len(dropped) != 0 || len(changed) != 1 || changed[0] != "table users" {
		t.Errorf("0024 added %v, changed %v and dropped %v", added, changed, dropped)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

func TestAPersonsWithdrawalsMarkNoWorkspaceFromAnotherSource(t *testing.T) {
	// A consent to actions or to sending is not part of what an access
	// snapshot shows (migration 0023): neither is its withdrawal.
	f := changesFixture(t)
	const bea = "usr_0000000000000002"
	before := f.mark("ext_a")
	f.accepted("a withdrawal", f.exec(`UPDATE users SET actions_consent_at = 0, actions_consent_version = '',
		actions_consent_withdrawn_at = 990, send_consent_withdrawn_at = 990 WHERE id = ?`, bea))
	if after := f.mark("ext_a"); after != before {
		t.Errorf("a withdrawal moved ext_a's mark from %d to %d", before, after)
	}
}
