package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/store/storetest"
)

// usageTotalsLine is the whole of what `migrate` says of the usage totals:
// how many mailboxes, and how long, never a figure.
var usageTotalsLine = regexp.MustCompile(`^usage totals: (\d+) accounts filled or repaired in \S+$`)

func TestMigrateFillsTheUsageTotalsAndSaysOnlyHowMany(t *testing.T) {
	// A database migration 0030 upgraded: two mailboxes with an index, and
	// their totals empty until something fills them.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ctx := t.Context()
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
		`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_at, updated_at,
			expires_at) VALUES ('drf_0000000000000001', 'acc_operator0000002', '{}', 'h', 'd@example.com', 'open', 1, 1, 9)`,
		`DELETE FROM usage_days`,
		`DELETE FROM usage_fixed`,
	} {
		if _, err := db.Writer().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	empty := func() bool {
		var n int
		if err := db.Reader().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_days)
			+ (SELECT count(*) FROM usage_fixed)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	}
	usage, err := db.Usage(ctx, []string{"acc_operator0000001", "acc_operator0000002"})
	if err != nil || len(usage) != 2 {
		t.Fatalf("the index: %v, %v", usage, err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A dry run says what it would apply, and fills nothing.
	out, err := captureStdout(t, func() error { return migrateCommand(ctx, cfg, []string{"--dry-run"}) })
	if err != nil || out != "schema is up to date\n" || !empty() {
		t.Fatalf("a dry run said %q (%v), and filled the totals: %v", out, err, !empty())
	}

	for _, want := range []string{"2", "0"} {
		out, err := captureStdout(t, func() error { return migrateCommand(ctx, cfg, nil) })
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != 2 || lines[0] != fmt.Sprintf("schema is at version %d", version) {
			t.Fatalf("migrate said %q", out)
		}
		m := usageTotalsLine.FindStringSubmatch(lines[1])
		if m == nil || m[1] != want {
			t.Errorf("migrate said %q of the usage totals, want %s accounts filled or repaired and nothing else", lines[1], want)
		}
		for _, name := range []string{"acc_operator", "ops@", "alerts@", "byte"} {
			if strings.Contains(out, name) {
				t.Errorf("migrate named %q: %q", name, out)
			}
		}
	}
	if drifted, err := db.CheckUsageTotals(ctx); err != nil || len(drifted) != 0 || empty() {
		t.Errorf("after migrate the totals of %v drifted (%v), or none were kept", drifted, err)
	}
	for id, u := range usage {
		var kept int64
		if err := db.Reader().QueryRowContext(ctx, `SELECT (SELECT coalesce(sum(bytes), 0) FROM usage_days WHERE account_id = ?1)
			+ (SELECT coalesce(sum(bytes), 0) FROM usage_fixed WHERE account_id = ?1)`, id).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if kept != u.Bytes {
			t.Errorf("%s keeps %d bytes, and Usage scans %d", id, kept, u.Bytes)
		}
	}
}
