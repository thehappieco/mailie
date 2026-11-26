package app

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/store"
)

// consoleDatabase is a daemon's configuration over a database the open
// console's people filled: Ana's personal workspace holds a live mailbox and
// a removed one, a team a live one, and the operator workspace one. It
// returns what no start-up line may name.
func consoleDatabase(t *testing.T) (config.Config, []string) {
	t.Helper()
	cfg := localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	personal := authtest.Personal(t, db, ana.ID)
	const team = "wsp_team00000000001"
	mailbox := func(id, workspaceID, owner, address, label string) string {
		linker := "NULL"
		if owner != "" {
			linker = "'" + owner + "'"
		}
		return fmt.Sprintf(`INSERT INTO accounts(id, workspace_id, owner_user_id, email, display_name, provider,
			auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state,
			state_changed_at, created_at, updated_at)
			VALUES ('%s', '%s', %s, '%s', '%s', 'imap', 'password', 'imap.example.com', 993, 'smtp.example.com', 587,
			'starttls', 'login', 1, 'active', 100, 100, 100)`, id, workspaceID, linker, address, label)
	}
	for _, q := range []string{
		mailbox("acc_anawork00000001", personal, ana.ID, "ana@work.example.com", "Ana at work"),
		mailbox("acc_anaold000000001", personal, ana.ID, "ana@old.example.com", "Ana before"),
		fmt.Sprintf(`UPDATE accounts SET removed_at = %[1]d, removed_by = '%[2]s', purge_after = %[1]d + 2592000
			WHERE id = 'acc_anaold000000001'`, time.Now().Unix(), ana.ID),
		`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
			VALUES ('` + team + `', 'team', 'local', 'Support', 100, 100)`,
		mailbox("acc_support00000001", team, "", "support@example.com", "Support desk"),
		mailbox("acc_operator0000001", "wsp_operator", "", "ops@example.com", "Operations"),
	} {
		if _, err := db.Writer().ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg, []string{
		ana.ID, personal, team, "acc_anawork00000001", "acc_anaold000000001", "acc_support00000001",
		"acc_operator0000001", "ana@", "work.example.com", "support@", "Ana at work", "Ana before", "Support desk",
	}
}

// startLines runs a daemon over cfg with opts until it answers, stops it, and
// returns the lines it logged that say line.
func startLines(t *testing.T, cfg config.Config, opts Options, line string) ([]string, string) {
	t.Helper()
	d := startDaemon(t, cfg, opts)
	d.stop()
	if err := d.wait(); err != nil {
		t.Fatal(err)
	}
	logs := d.logs.String()
	var said []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, line) {
			said = append(said, l)
		}
	}
	return said, logs
}

func TestAStartOverMailboxesOutsideTheOperatorWorkspaceWarnsWithTheirCountsAlone(t *testing.T) {
	// Every start says it, with or without a console: a headless server is
	// where it matters.
	cfg, secret := consoleDatabase(t)
	for _, webDir := range []string{"", cfg.WebDir} {
		run := cfg
		run.WebDir = webDir
		said, logs := startLines(t, run, Options{}, workspaceMailboxesLine)
		if len(said) != 1 {
			t.Fatalf("console %q: said %d times:\n%s", webDir, len(said), logs)
		}
		l := said[0]
		for _, want := range []string{
			"level=WARN", "no instance key reaches them",
			"personal_live=1", "personal_removed=1", "team_live=1", "team_removed=0",
			"Moving from the open console", "README",
		} {
			if !strings.Contains(l, want) {
				t.Errorf("console %q: the line does not say %q: %s", webDir, want, l)
			}
		}
		for _, never := range secret {
			if strings.Contains(l, never) {
				t.Errorf("console %q: the line names %q: %s", webDir, never, l)
			}
		}
	}
}

func TestAStartOverOperatorMailboxesAloneSaysNothingOfWorkspaces(t *testing.T) {
	// People and their empty personal workspaces, and a mailbox in the
	// operator workspace, which an instance key reaches.
	cfg := localConfig(t)
	cfg.WebDir = ""
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO accounts(id, workspace_id, email, provider, auth_kind,
		imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
		created_at, updated_at)
		VALUES ('acc_operator0000001', 'wsp_operator', 'ops@example.com', 'imap', 'password', 'imap.example.com', 993,
		'smtp.example.com', 587, 'starttls', 'ops', 1, 'active', 100, 100, 100)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if said, logs := startLines(t, cfg, Options{}, "operator workspace"); len(said) != 0 {
		t.Errorf("a database whose mailboxes are all the operator's:\n%s", logs)
	}
}

func TestADaemonWherePeopleSignInOnlyThroughAnExtensionNeverWarnsOfWorkspaceMailboxes(t *testing.T) {
	// There personal and team workspaces are what the edition is made of.
	cfg, _ := consoleDatabase(t)
	if said, logs := startLines(t, cfg, Options{ExternalSignInOnly: true}, "operator workspace"); len(said) != 0 {
		t.Errorf("a daemon whose people sign in only through an extension:\n%s", logs)
	}
}

func TestAFailureToCountWorkspaceMailboxesIsLoggedAndDecidesNothing(t *testing.T) {
	// The check returns nothing for the start to stop on: a database it
	// cannot read is said, at warn, and the start goes on.
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "mail.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnWorkspaceMailboxes(context.Background(), db, obs.NewLoggerTo(&logs, "info", "text"))
	out := logs.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, "counting the mailboxes outside the operator workspace failed") ||
		strings.Contains(out, workspaceMailboxesLine) {
		t.Errorf("a count that failed logged:\n%s", out)
	}
}
