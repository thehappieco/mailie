package account_test

import (
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
)

func TestAMailboxRemovedOrRecoveredBetweenTheTwoListsIsInExactlyOne(t *testing.T) {
	// The live list and the removed list are read in one read: a removal or
	// a recovery that commits between them changes neither, so a mailbox is
	// never listed twice nor missed.
	repo, db := newRepo(t)
	a := seed(t, repo, "person@example.com", provider.KindGmail)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Writer().ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	remove := func() {
		exec(`UPDATE accounts SET removed_at = 900, removed_by = 'cli', purge_after = 900 + 2592000 WHERE id = ?`, a.ID)
	}
	recover := func() {
		exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0 WHERE id = ?`, a.ID)
	}
	lists := func(meanwhile func()) (live, removed int) {
		t.Helper()
		l, r, err := account.ListVisibleAndRemovedMeanwhileForTest(t.Context(), repo,
			account.Visibility{All: true}, account.RemovedScope{Operator: true}, meanwhile)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range l {
			if x.ID == a.ID {
				live++
			}
		}
		for _, x := range r {
			if x.ID == a.ID {
				removed++
			}
		}
		return live, removed
	}

	if live, removed := lists(remove); live != 1 || removed != 0 {
		t.Errorf("removed between the reads: listed live %d, removed %d; want it live once, as the read began", live, removed)
	}
	if live, removed := lists(nil); live != 0 || removed != 1 {
		t.Errorf("once removed: listed live %d, removed %d", live, removed)
	}
	if live, removed := lists(recover); live != 0 || removed != 1 {
		t.Errorf("recovered between the reads: listed live %d, removed %d; want it removed once, as the read began", live, removed)
	}
	if live, removed := lists(nil); live != 1 || removed != 0 {
		t.Errorf("once recovered: listed live %d, removed %d", live, removed)
	}
}
