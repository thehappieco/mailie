package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
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
		_, _ = w.Write([]byte(`{"id":"usr_1","email":"ana@example.com","role":"member","url":"https://c.example/#invite=x",` +
			`"expires_at":1790000000}`))
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
		"invite": userInvite, "disable": userDisable, "delete": userDelete,
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
	if len(seen) != 3 {
		t.Fatalf("the daemon was asked %d times, want 3: %v", len(seen), seen)
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

// resetState is what a reset invitation changes for one person before it is
// used: nothing of theirs, and how many reset invitations wait for them.
func resetState(t *testing.T, db *store.Store, userID string) string {
	t.Helper()
	var verifier string
	var live, resets int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT auth_verifier, (SELECT count(*) FROM sessions WHERE user_id = users.id AND revoked_at = 0),
		        (SELECT count(*) FROM reset_invites WHERE user_id = users.id)
		   FROM users WHERE id = ?`, userID).Scan(&verifier, &live, &resets); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s|%d live|%d resets", verifier, live, resets)
}

func TestAPasswordResetWithBootstrapPrintsAResetLinkThatChangesNothingUntilUsed(t *testing.T) {
	cfg := localConfig(t)
	cfg.PublicURL = "https://console.example"
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	users := auth.NewUsers(db)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	before := resetState(t, db, ana.ID)

	var logs bytes.Buffer
	ctx := obs.WithLogger(t.Context(), obs.NewLoggerTo(&logs, "info", "json"))
	out, err := captureStdout(t, func() error {
		return userPassword(ctx, cfg, []string{"--bootstrap", "--email", "Ana@Example.com"})
	})
	if err != nil {
		t.Fatalf("user password --bootstrap: %v", err)
	}
	link, err := url.Parse(strings.TrimSpace(out))
	if err != nil || link.Host != "console.example" || link.RawQuery != "" {
		t.Fatalf("printed %q (%v), want one link to the console", out, err)
	}
	fragment, err := url.ParseQuery(link.Fragment)
	if err != nil || fragment.Get("reset") == "" || fragment.Get("email") != "ana@example.com" {
		t.Fatalf("the link's fragment is %q, want the code and the address", link.Fragment)
	}
	// Nothing of hers changed yet: her sessions work, and the old password.
	if after := resetState(t, db, ana.ID); after != strings.Replace(before, "0 resets", "1 resets", 1) {
		t.Errorf("the invitation changed the person: %s, was %s", after, before)
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); err != nil {
		t.Errorf("making the invitation ended a session: %v", err)
	}
	// The link is what the person uses.
	in := authtest.EnrolmentWith(t, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), authtest.RecoveryProof)
	if _, _, _, err := users.CompleteReset(t.Context(), fragment.Get("reset"), fragment.Get("email"), in, "test"); err != nil {
		t.Fatalf("the printed link does not reset: %v", err)
	}
	if _, err := users.AuthenticateSession(t.Context(), laptop); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("a session survived the reset: %v", err)
	}
	// The operator's event is in the log, and the code nowhere in it.
	if got := logs.String(); !strings.Contains(got, "reset invitation made by the operator") || !strings.Contains(got, ana.ID) ||
		strings.Contains(got, fragment.Get("reset")) {
		t.Errorf("the log says %q", got)
	}
}

func TestAResetOfATeamMailboxsLastReaderWithBootstrapNeedsForce(t *testing.T) {
	// Ana is the only one who reads the team's mailbox, Bea owns the team
	// beside her: the test closing her uses.
	cfg := localConfig(t)
	cfg.PublicURL = "https://console.example"
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
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
	before := resetState(t, db, ana.ID)

	_, err = captureStdout(t, func() error {
		return userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	})
	if err == nil || !strings.Contains(err.Error(), "acc_00000000000000a1") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a reset of the last reader without --force: %v", err)
	}
	if after := resetState(t, db, ana.ID); after != before {
		t.Errorf("a refused reset changed the person: %s, was %s", after, before)
	}
	out, err := captureStdout(t, func() error {
		return userPassword(t.Context(), cfg, []string{"--bootstrap", "--force", "--email", "ana@example.com"})
	})
	if err != nil || !strings.Contains(out, "#reset=") {
		t.Fatalf("a forced reset: %q, %v", out, err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM reset_invites WHERE user_id = ? AND forced = 1`, ana.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d forced reset invitations (%v), want the one", n, err)
	}
}

func TestResettingAPasswordRefusesWithoutBootstrap(t *testing.T) {
	// No route makes a reset invitation, so there is no daemon to ask.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	before := resetState(t, db, ana.ID)

	err := userPassword(t.Context(), cfg, []string{"--email", "ana@example.com"})
	if err == nil || !strings.Contains(err.Error(), "--bootstrap") {
		t.Errorf("user password without --bootstrap: %v", err)
	}
	if after := resetState(t, db, ana.ID); after != before {
		t.Errorf("a refused reset changed the person: %s, was %s", after, before)
	}
}

func TestResettingAPasswordRefusesWhileTheDaemonRuns(t *testing.T) {
	cfg := localConfig(t)
	lock, err := lockfile.Acquire(cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	err = userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	if err == nil || !strings.Contains(err.Error(), "daemon is running") {
		t.Errorf("user password --bootstrap beside the daemon: %v", err)
	}
}

func TestResettingThePasswordOfAnUnknownAddressChangesNothing(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	before := resetState(t, db, ana.ID)

	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "nobody@example.com"})
	if err == nil || !strings.Contains(err.Error(), "no person has that address") {
		t.Errorf("user password for an unknown address: %v", err)
	}
	if after := resetState(t, db, ana.ID); after != before {
		t.Errorf("a reset for nobody changed somebody: %s, was %s", after, before)
	}
}

func TestAnAddressFromStandardInputMakesAResetInvitation(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	pipeStdin(t, "ana@example.com\n")
	out, err := captureStdout(t, func() error {
		return userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "-"})
	})
	if err != nil || !strings.Contains(out, "#reset=") || strings.Contains(out, "\nana@example.com") {
		t.Fatalf("user password --email -: %q, %v", out, err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM reset_invites WHERE user_id = ?`, ana.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d reset invitations (%v), want one", n, err)
	}
}

func TestNoPasswordIsEverAnArgument(t *testing.T) {
	// An argument is in the process listing and the shell history; a
	// stray one here may well be a password, so it is not quoted back.
	const password = "a brand new password"
	err := userPassword(t.Context(), localConfig(t), []string{"--bootstrap", "--email", "ana@example.com", password})
	if err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("a password given as an argument: %v", err)
	}
	args := []string{"--bootstrap", "--email", "ana@example.com", "--password", password}
	if err := userPassword(t.Context(), localConfig(t), args); err == nil {
		t.Errorf("--password was accepted")
	}
}

// fakeTerminal is a terminal for readHiddenFrom: it records whether it
// echoes, and a line arrives when the test types one.
type fakeTerminal struct {
	mu     sync.Mutex
	echo   bool
	hidden chan struct{} // closed once echo is off
	typed  chan []byte
}

func newFakeTerminal(t *testing.T) *fakeTerminal {
	f := &fakeTerminal{echo: true, hidden: make(chan struct{}), typed: make(chan []byte)}
	// A read nobody waits for any more ends with the test, as it ends with
	// the process.
	t.Cleanup(func() { close(f.typed) })
	return f
}

func (f *fakeTerminal) hide() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.echo = false
	close(f.hidden)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.echo = true
	}, nil
}

func (f *fakeTerminal) readLine() ([]byte, error) {
	line, ok := <-f.typed
	if !ok {
		return nil, io.EOF
	}
	return line, nil
}

func (f *fakeTerminal) echoing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.echo
}

func TestEchoComesBackWhicheverWayAHiddenEntryEnds(t *testing.T) {
	t.Run("Return", func(t *testing.T) {
		f := newFakeTerminal(t)
		go func() { <-f.hidden; f.typed <- []byte("a secret") }()
		line, err := readHiddenFrom(t.Context(), f)
		if err != nil || string(line) != "a secret" || !f.echoing() {
			t.Errorf("read %q, %v; echoing %v", line, err, f.echoing())
		}
	})
	t.Run("an interrupt while typing", func(t *testing.T) {
		f := newFakeTerminal(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-f.hidden; cancel() }()
		if _, err := readHiddenFrom(ctx, f); !errors.Is(err, context.Canceled) || !f.echoing() {
			t.Errorf("%v; echoing %v", err, f.echoing())
		}
	})
	t.Run("an interrupt before the prompt", func(t *testing.T) {
		f := newFakeTerminal(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := readHiddenFrom(ctx, f); !errors.Is(err, context.Canceled) || !f.echoing() {
			t.Errorf("%v; echoing %v", err, f.echoing())
		}
	})
}

func TestAHiddenEntryReadsOneLineAndNothingPastIt(t *testing.T) {
	r := strings.NewReader("secret\b\bxx\r\nthe next line")
	if line, err := readTerminalLine(r); err != nil || string(line) != "secrxx" || r.Len() != len("the next line") {
		t.Errorf("read %q, %v, leaving %d bytes", line, err, r.Len())
	}
	if line, err := readTerminalLine(strings.NewReader("no newline")); err != nil || string(line) != "no newline" {
		t.Errorf("a line ended by the end of input: %q, %v", line, err)
	}
	if _, err := readTerminalLine(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("nothing typed before the end of input: %v", err)
	}
}

func TestABootstrapInviteIsAnOwnersOnlyWhileTheServerHasNone(t *testing.T) {
	// Signing up first no longer makes anyone an owner: the invite decides.
	// The quick start's `user invite --bootstrap --email ADDRESS` still makes
	// the first owner, and only the first.
	cfg := localConfig(t)
	cfg.PublicURL = "https://console.example"
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	invite := func(args ...string) {
		t.Helper()
		if err := userInvite(t.Context(), cfg, append([]string{"--bootstrap"}, args...)); err != nil {
			t.Fatalf("user invite %v: %v", args, err)
		}
	}
	roles := func() string {
		t.Helper()
		rows, err := db.Reader().QueryContext(t.Context(), `SELECT email, role FROM invites ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var email, role string
			if err := rows.Scan(&email, &role); err != nil {
				t.Fatal(err)
			}
			out = append(out, email+" "+role)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ", ")
	}

	invite("--email", "colleague@example.com", "--role", "member")
	invite("--email", "first@example.com")
	invite("--email", "second@example.com")
	invite("--email", "deputy@example.com", "--role", "owner")
	want := "colleague@example.com member, first@example.com owner, second@example.com member, deputy@example.com owner"
	if got := roles(); got != want {
		t.Errorf("invites:\n%s\nwant:\n%s", got, want)
	}
}
