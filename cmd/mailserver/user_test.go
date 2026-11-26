package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// localConfig is a daemon's configuration over a fresh data directory, with
// a credential key, and nothing listening.
func localConfig(t *testing.T) config.Config {
	t.Helper()
	key := make([]byte, secrets.KeyLen)
	for i := range key {
		key[i] = 0x5c
	}
	return config.Config{
		DataDir:     t.TempDir(),
		Credentials: config.Credentials{ActiveKeyID: 1, Keys: map[uint8][]byte{1: key}},
	}
}

func TestClosingAnAccountWithBootstrapRefusesWhileTheDaemonRuns(t *testing.T) {
	// Two writers on one database with no coordination is how an index
	// gets corrupted; the daemon's lock says one is already there.
	cfg := localConfig(t)
	lock, err := lockfile.Acquire(cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	for name, run := range map[string]func(context.Context, config.Config, []string) error{
		"disable": userDisable, "delete": userDelete,
	} {
		err := run(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"})
		if err == nil || !strings.Contains(err.Error(), "daemon is running") {
			t.Errorf("user %s --bootstrap beside the daemon: %v", name, err)
		}
	}
}

func TestDeletingAPersonWithBootstrapRemovesThemAndTheirMailboxes(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, keyring)
	if _, err := repo.Create(t.Context(), account.Account{
		ID: "acc_00000000000000a1", Email: "ana@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "ana", State: account.StateActive,
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePassword(t.Context(), "acc_00000000000000a1", "hunter2"); err != nil {
		t.Fatal(err)
	}

	if err := userDisable(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"}); err != nil {
		t.Fatalf("user disable --bootstrap: %v", err)
	}
	if u, err := auth.NewUsers(db).Get(t.Context(), ana.ID); err != nil || !u.Disabled {
		t.Fatalf("after disable: %+v, %v", u, err)
	}
	if err := userDelete(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"}); err != nil {
		t.Fatalf("user delete --bootstrap: %v", err)
	}
	if _, err := auth.NewUsers(db).Get(t.Context(), ana.ID); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("the person is still there: %v", err)
	}
	var credentials int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(t.Context(), "acc_00000000000000a1"); !errors.Is(err, account.ErrNotFound) || credentials != 0 {
		t.Errorf("the mailbox (%v) or its %d credentials survived", err, credentials)
	}

	// The same rules as over REST: the last active owner needs --force.
	if err := userDelete(t.Context(), cfg, []string{"--email", "owner@example.com", "--bootstrap"}); err == nil ||
		!strings.Contains(err.Error(), "last active owner") {
		t.Errorf("deleting the last owner without --force: %v", err)
	}
	if err := userDelete(t.Context(), cfg, []string{"--email", "owner@example.com", "--bootstrap", "--force"}); err != nil {
		t.Errorf("deleting the last owner with --force: %v", err)
	}
}

func TestDeletingAPersonWithBootstrapRevokesTheGoogleGrantsOfTheirMailboxes(t *testing.T) {
	// The same deletion for good as over REST: Ana's Gmail mailbox goes with
	// her, and its grant is revoked at Google once that commits.
	var (
		mu      sync.Mutex
		revoked []string
	)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			mu.Lock()
			revoked = append(revoked, r.PostForm.Get("token"))
			mu.Unlock()
		}
	}))
	t.Cleanup(google.Close)
	was := googleRevokeURL
	googleRevokeURL = google.URL
	t.Cleanup(func() { googleRevokeURL = was })

	cfg := localConfig(t)
	// The test's endpoint is on loopback.
	cfg.AccountAllowPrivate = true
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, keyring)
	if _, err := repo.Create(t.Context(), account.Account{
		ID: "acc_00000000000000b1", Email: "ana@gmail.com", Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "ana@gmail.com",
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveGrant(t.Context(), "acc_00000000000000b1", &oauth2.Token{AccessToken: "a",
		RefreshToken: "refresh-ana", Expiry: time.Now().Add(time.Hour)}, account.ClientInstalled); err != nil {
		t.Fatal(err)
	}

	if err := userDelete(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"}); err != nil {
		t.Fatalf("user delete --bootstrap: %v", err)
	}
	if _, err := repo.Get(t.Context(), "acc_00000000000000b1"); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("her mailbox is still there: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "refresh-ana" {
		t.Errorf("revoked %q, want her mailbox's grant", revoked)
	}
}

func TestClosingTheLastReaderOfATeamMailboxWithBootstrapNeedsForce(t *testing.T) {
	// The same rule as over REST, through the same method: Ana is the only
	// one who reads the team's mailbox, Bea owns the team beside her.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	team, err := ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return ws.AddMemberTx(t.Context(), tx, team.ID, bea.ID, workspace.RoleOwner, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := account.NewRepository(db, nil).Create(t.Context(), account.Account{
		ID: "acc_00000000000000a1", Email: "support@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "support", WorkspaceID: team.ID, State: account.StateActive,
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(context.Context, config.Config, []string) error{userDisable, userDelete} {
		err := run(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"})
		if err == nil || !strings.Contains(err.Error(), "acc_00000000000000a1") {
			t.Errorf("closing the last reader without --force: %v", err)
		}
	}
	if u, err := auth.NewUsers(db).Get(t.Context(), ana.ID); err != nil || u.Disabled {
		t.Fatalf("a refused closure changed Ana: %+v, %v", u, err)
	}
	if err := userDelete(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap", "--force"}); err != nil {
		t.Fatalf("deleting the last reader with --force: %v", err)
	}
	// The team's mailbox stays the team's, read by nobody now.
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM accounts WHERE id = 'acc_00000000000000a1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the team's mailbox went with its last reader: %d, %v", n, err)
	}
}

func TestAForcedClosureWithBootstrapJournalsTheCommandLineAsItsActor(t *testing.T) {
	// Ana alone reads the team's mailbox, which Bea owns beside her: closing
	// her with --force strands it, journaled as the command line's doing.
	for name, run := range map[string]func(context.Context, config.Config, []string) error{
		"disabled": userDisable, "deleted": userDelete,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := localConfig(t)
			db := storetest.NewAt(t, cfg.DatabasePath(), nil)
			authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
			ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
			bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
			ws := workspace.NewRepository(db, nil)
			team, err := ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Write(t.Context(), func(tx *sql.Tx) error {
				return ws.AddMemberTx(t.Context(), tx, team.ID, bea.ID, workspace.RoleOwner, time.Now())
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := account.NewRepository(db, nil).Create(t.Context(), account.Account{
				ID: "acc_00000000000000a1", Email: "support@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
				IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit",
				LoginUser: "support", WorkspaceID: team.ID, State: account.StateActive,
			}, ana.ID); err != nil {
				t.Fatal(err)
			}
			if err := run(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap", "--force"}); err != nil {
				t.Fatalf("user %s --bootstrap --force: %v", name, err)
			}
			rows, err := db.Reader().QueryContext(t.Context(), `SELECT kind || ' ' || cause || ' ' || actor
				FROM mailbox_events WHERE account_id = 'acc_00000000000000a1' ORDER BY seq`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var journal []string
			for rows.Next() {
				var entry string
				if err := rows.Scan(&entry); err != nil {
					t.Fatal(err)
				}
				journal = append(journal, entry)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if want := "stranded " + name + " cli"; len(journal) != 1 || journal[0] != want {
				t.Errorf("the journal holds %q, want %q", journal, want)
			}
		})
	}
}

func TestClosingTheLastReaderOfATeamThatOutlivesThemWithBootstrapNeedsForce(t *testing.T) {
	// Bea is the team's other member, her membership disabled: the team
	// outlives Ana, and nobody could read its mailbox again.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	team, err := ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return ws.AddMemberTx(t.Context(), tx, team.ID, bea.ID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE workspace_members SET status = 'disabled'
		WHERE workspace_id = ? AND user_id = ?`, team.ID, bea.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := account.NewRepository(db, nil).Create(t.Context(), account.Account{
		ID: "acc_00000000000000a1", Email: "support@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: "support", WorkspaceID: team.ID, State: account.StateActive,
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(context.Context, config.Config, []string) error{userDisable, userDelete} {
		err := run(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap"})
		if err == nil || !strings.Contains(err.Error(), "acc_00000000000000a1") {
			t.Errorf("closing the last reader of a team that outlives her without --force: %v", err)
		}
	}
	if u, err := auth.NewUsers(db).Get(t.Context(), ana.ID); err != nil || u.Disabled {
		t.Fatalf("a refused closure changed Ana: %+v, %v", u, err)
	}
	if err := userDelete(t.Context(), cfg, []string{"--email", "ana@example.com", "--bootstrap", "--force"}); err != nil {
		t.Fatalf("deleting Ana with --force: %v", err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM accounts WHERE id = 'acc_00000000000000a1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the team's mailbox went with Ana: %d, %v", n, err)
	}
}

func TestTheCommandLineNeitherInvitesNorResetsAPassword(t *testing.T) {
	// The server has no people of its own: nothing here invites one or
	// makes a reset invitation, with the daemon up or stopped, and a script
	// that still calls either is told so by name, with nothing asked of the
	// daemon or written to the database.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the daemon was asked %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	cfg := localConfig(t)
	cfg.HTTPAddr, cfg.AdminKey = strings.TrimPrefix(srv.URL, "http://"), "deadbeef.secret"
	for _, args := range [][]string{
		{"invite", "--email", "ana@example.com"},
		{"invite", "--bootstrap", "--role", "owner", "--email", "ana@example.com"},
		{"invite", "--email", "ana@example.com", "--workspace", "wsp_0000000000000001"},
		{"password", "--bootstrap", "--email", "ana@example.com"},
	} {
		err := userCommand(t.Context(), cfg, args)
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), "invites nobody and resets no password") ||
			!strings.Contains(err.Error(), "apikey create") {
			t.Errorf("user %v: %v", args, err)
		}
	}
	if _, err := os.Stat(cfg.DatabasePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a database was opened: %v", err)
	}
}

func TestClosingAnAccountAsksTheDaemonWithTheAddressInTheBody(t *testing.T) {
	// A URL is logged by every proxy on its way; a body is not.
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(raw))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer deadbeef.secret" {
			t.Errorf("called without the admin key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"usr_1","email":"ana@example.com","sessions_ended":2,"keys_revoked":1,` +
			`"accounts_removed":1,"sessions_deleted":2,"keys_deleted":1,"invites_deleted":1}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}

	if err := userDisable(t.Context(), cfg, []string{"--email", "ana@example.com"}); err != nil {
		t.Fatalf("user disable: %v", err)
	}
	if err := userDelete(t.Context(), cfg, []string{"--email", "ana@example.com", "--force"}); err != nil {
		t.Fatalf("user delete: %v", err)
	}
	want := []string{
		`POST /v1/users/disable {"email":"ana@example.com"}`,
		`POST /v1/users/delete {"email":"ana@example.com","force":true}`,
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("the daemon was asked\n%s\nwant\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
	if err := userDelete(t.Context(), cfg, []string{}); err == nil || !strings.Contains(err.Error(), "--email") {
		t.Errorf("no address: %v", err)
	}
}

func TestAnAddressCanComeFromStandardInputAndStayOffTheCommandLine(t *testing.T) {
	// Running the CLI against the daemon's data directory on a server goes
	// through sudo, often systemd-run too, and both log the command line: an
	// address given as --email X lands in the host's journal, unmasked, on
	// the very request to delete that person's data.
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"usr_1","email":"ana@example.com"}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}
	withStdin := func(input string) {
		t.Helper()
		previous := stdin
		stdin = strings.NewReader(input)
		t.Cleanup(func() { stdin = previous })
	}

	for name, run := range map[string]func(context.Context, config.Config, []string) error{
		"disable": userDisable, "delete": userDelete,
	} {
		withStdin("  ana@example.com\n")
		if err := run(t.Context(), cfg, []string{"--email", "-"}); err != nil {
			t.Errorf("user %s --email -: %v", name, err)
		}
		withStdin("")
		if err := run(t.Context(), cfg, []string{"--email", "-"}); err == nil || !strings.Contains(err.Error(), "standard input") {
			t.Errorf("user %s --email - with nothing to read: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the daemon was asked %d times, want 2: %v", len(seen), seen)
	}
	for _, request := range seen {
		if !strings.Contains(request, `"email":"ana@example.com"`) {
			t.Errorf("the address read from standard input did not arrive whole: %s", request)
		}
	}
}

// pipeStdin makes standard input a pipe that carries input: not a terminal.
func pipeStdin(t *testing.T, input string) *strings.Reader {
	t.Helper()
	previous, wasTerminal := stdin, stdinIsTerminal
	r := strings.NewReader(input)
	stdin, stdinIsTerminal = r, func() bool { return false }
	t.Cleanup(func() { stdin, stdinIsTerminal = previous, wasTerminal })
	return r
}

// terminalStdin makes standard input a terminal, on which typed is what the
// operator types where it is echoed (an address, for --email -) and each of
// hidden is one entry typed where it is not.
func terminalStdin(t *testing.T, typed string, hidden ...string) {
	t.Helper()
	previous, wasTerminal, previousRead := stdin, stdinIsTerminal, readHidden
	stdin, stdinIsTerminal = strings.NewReader(typed), func() bool { return true }
	readHidden = func(context.Context) ([]byte, error) {
		if len(hidden) == 0 {
			t.Error("asked for one more password than was typed")
			return nil, io.EOF
		}
		next := hidden[0]
		hidden = hidden[1:]
		return []byte(next), nil
	}
	t.Cleanup(func() { stdin, stdinIsTerminal, readHidden = previous, wasTerminal, previousRead })
}

// captureOutput runs fn with standard output and standard error each read
// into a string of its own.
func captureOutput(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	prev := os.Stderr
	os.Stderr = w
	printed := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		printed <- string(b)
	}()
	stdout, err = captureStdout(t, fn)
	os.Stderr = prev
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	stderr = <-printed
	if cerr := r.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	return stdout, stderr, err
}
