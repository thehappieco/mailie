package sync_test

import (
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// What a sync login found the server to be is kept with the mailbox
// (account.Account.ServerKind, migration 0027), for what an extension
// reports of it: written when it changes, and only then.

func TestASyncLoginRecordsWhetherTheServerIsGmailsAndWritesOnlyOnAChange(t *testing.T) {
	// A mailbox added as another IMAP server, on a host that is not
	// Google's: what it advertises decides.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), kind: provider.KindIMAP})
	h.exec(`CREATE TABLE server_kind_writes (n INTEGER NOT NULL)`)
	h.exec(`INSERT INTO server_kind_writes VALUES (0)`)
	h.exec(`CREATE TRIGGER count_server_kind_writes AFTER UPDATE OF server_kind ON accounts
		BEGIN UPDATE server_kind_writes SET n = n + 1; END`)
	kind := func() string {
		var k string
		if err := h.db.Reader().QueryRowContext(t.Context(), `SELECT server_kind FROM accounts WHERE id = ?`, h.account).
			Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	writes := func() int { return h.count(`SELECT n FROM server_kind_writes`) }
	if kind() != "" {
		t.Fatalf("a mailbox no login reached says its server is %q", kind())
	}
	h.start()
	h.waitLive()
	if kind() != "other" || writes() != 1 {
		t.Fatalf("after the first login the server is %q after %d writes, want other after one", kind(), writes())
	}

	// The same server again: nothing is written.
	lists := h.box.CallCount(providertest.MethodListFolders)
	h.box.KillSessions(provider.RoleSync)
	h.waitFor("the next login to list the folders", 5*time.Second, func() bool {
		return h.box.CallCount(providertest.MethodListFolders) > lists
	})
	if kind() != "other" || writes() != 1 {
		t.Errorf("the same server again: %q after %d writes, want other after one", kind(), writes())
	}

	// The server now advertises Gmail's extension: it is Gmail's.
	gmail := providertest.GmailCaps()
	gmail.Raw = []string{"IMAP4rev1", "X-GM-EXT-1", "IDLE"}
	h.box.SetCaps(gmail)
	h.box.KillSessions(provider.RoleSync)
	h.waitFor("the server recorded as Gmail's", 5*time.Second, func() bool { return kind() == "gmail" })
	if writes() != 2 {
		t.Errorf("%d writes, want the two that changed it", writes())
	}
}
