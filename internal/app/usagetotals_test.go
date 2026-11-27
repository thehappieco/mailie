package app

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/store"
)

// indexedDatabase is a database whose two operator mailboxes hold an index,
// with the totals of usage rule 1 (migration 0030) left empty, as the
// migration leaves them on a database it upgrades. It returns what no line
// about them may name: the mailboxes and their addresses. (Their figures it
// cannot say either: usageLine leaves no room for one.)
func indexedDatabase(t *testing.T, path string) []string {
	t.Helper()
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			VALUES ('acc_operator0000001', 'wsp_operator', 'ops@example.com', 'imap', 'password', 'imap.example.com',
			993, 'smtp.example.com', 587, 'starttls', 'ops', 1, 'active', 100, 100, 100),
			('acc_operator0000002', 'wsp_operator', 'alerts@example.com', 'imap', 'password', 'imap.example.com',
			993, 'smtp.example.com', 587, 'starttls', 'alerts', 1, 'active', 100, 100, 100)`,
		`INSERT INTO folders(account_id, name, display_name) VALUES ('acc_operator0000001', 'INBOX', 'Inbox'),
			('acc_operator0000002', 'INBOX', 'Inbox')`,
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, subject, internal_date, size,
			first_seen_at, updated_at)
			SELECT account_id, id, 1, 1, 'g', 'Disk alert', 1761000000, 300, 1, 1 FROM folders`,
		`INSERT INTO parts(msg_id, path, mime_type, size) SELECT id, '1', 'text/plain', 300 FROM messages`,
		`DELETE FROM usage_days`,
		`DELETE FROM usage_fixed`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if usage, err := db.Usage(t.Context(), []string{"acc_operator0000001", "acc_operator0000002"}); err != nil ||
		len(usage) != 2 {
		t.Fatalf("the index: %v, %v", usage, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return []string{"acc_operator0000001", "acc_operator0000002", "ops@", "alerts@", "example.com"}
}

// usageLine is the whole of a line the check of the usage totals logs: how
// many mailboxes, and how long, nothing else.
var usageLine = regexp.MustCompile(`^time=\S+ level=INFO msg="usage totals (repaired|checked)" accounts=(\d+) took=\S+$`)

func TestAStartFillsTheUsageTotalsAndSaysOnlyHowManyMailboxes(t *testing.T) {
	cfg := localConfig(t)
	cfg.WebDir = ""
	never := indexedDatabase(t, cfg.DatabasePath())
	for _, want := range []struct{ said, accounts string }{
		{"repaired", "2"}, // the first start after the migration fills both
		{"checked", "0"},  // and the next finds nothing to repair
	} {
		said, logs := startLines(t, cfg, Options{}, "usage totals")
		if len(said) != 1 {
			t.Fatalf("the start said it %d times:\n%s", len(said), logs)
		}
		m := usageLine.FindStringSubmatch(said[0])
		if m == nil || m[1] != want.said || m[2] != want.accounts {
			t.Errorf("the start said %q, want the totals %s for %s mailboxes alone", said[0], want.said, want.accounts)
		}
		for _, name := range never {
			if strings.Contains(said[0], name) {
				t.Errorf("the line names %q: %s", name, said[0])
			}
		}
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if drifted, err := db.CheckUsageTotals(t.Context()); err != nil || len(drifted) != 0 {
		t.Errorf("after the start the totals of %v drifted (%v)", drifted, err)
	}
}

func TestAFailedCheckOfTheUsageTotalsIsLoggedAndStopsNothing(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "mail.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	reconcileUsageTotals(context.Background(), db, obs.NewLoggerTo(&logs, "info", "text"))
	out := logs.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, "checking the usage totals failed") || strings.Contains(out, "accounts=") {
		t.Errorf("a check that failed logged:\n%s", out)
	}
}

func TestHousekeepingChecksTheUsageTotalsDailyAndNeverAtStart(t *testing.T) {
	// The start checked them before anything ran; the housekeeping's
	// round at start leaves them, and every 24th round after checks them.
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "mail.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var rounds, checks atomic.Int32
	var roundsAtCheck []int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		housekeep(ctx, auth.NewUsers(db), db.SweepSends, func(context.Context, time.Duration) (int, int, error) {
			rounds.Add(1)
			return 0, 0, nil
		}, nil, nil, nil, func(context.Context) {
			roundsAtCheck = append(roundsAtCheck, rounds.Load())
			if checks.Add(1) == 2 {
				cancel()
			}
		}, db.Scrub, obs.NewLoggerTo(&bytes.Buffer{}, "info", "text"), time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cancel()
		<-done
		t.Fatalf("the housekeeping checked the totals %d times in %d rounds", checks.Load(), rounds.Load())
	}
	if len(roundsAtCheck) != 2 || roundsAtCheck[0] != 25 || roundsAtCheck[1] != 49 {
		t.Errorf("the totals were checked after rounds %v, want after the 25th and the 49th (the start's and 24 more each)",
			roundsAtCheck)
	}
}
