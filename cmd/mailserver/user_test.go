package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		LoginUser: "ana", OwnerUserID: ana.ID, State: account.StateActive,
	}); err != nil {
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

func TestTheRetentionSweepRunsAtOnceThenOnScheduleAndStopsWithTheDaemon(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	db := storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), clock)
	users := auth.NewUsersWithClock(db, clock)
	pending := func() int {
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM invites`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expire := func(email string) {
		// Made long enough ago that it has been expired for the whole
		// retention.
		mu.Lock()
		now = now.Add(-auth.InviteTTL - auth.InviteRetention - time.Minute)
		mu.Unlock()
		if _, _, err := users.CreateInvite(t.Context(), auth.NewInvite{Email: email, Role: auth.RoleMember, CreatedBy: "cli"}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		now = now.Add(auth.InviteTTL + auth.InviteRetention + time.Minute)
		mu.Unlock()
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	run := func(every time.Duration) (stop func()) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			housekeep(ctx, users, db.SweepSends, db.Scrub, slog.New(slog.DiscardHandler), every)
		}()
		return func() {
			t.Helper()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the sweep outlived the daemon's context")
			}
		}
	}

	// Swept means gone from the files as well, not only from the table.
	onDisk := func(email string) bool {
		for _, file := range []string{db.Path(), db.Path() + "-wal"} {
			raw, err := os.ReadFile(file)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), email) {
				return true
			}
		}
		return false
	}

	// With an hour to the first tick, only the sweep at start can do this.
	expire("first@example.com")
	stop := run(time.Hour)
	waitFor("the sweep at start", func() bool { return pending() == 0 && !onDisk("first@example.com") })
	stop()

	stop = run(20 * time.Millisecond)
	expire("second@example.com")
	waitFor("the next scheduled sweep", func() bool { return pending() == 0 && !onDisk("second@example.com") })
	stop()
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

// passwordState is what a reset changes for one person: their stored hash and
// how many of their sessions still work.
func passwordState(t *testing.T, db *store.Store, userID string) string {
	t.Helper()
	var hash string
	var live int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT password_hash, (SELECT count(*) FROM sessions WHERE user_id = users.id AND revoked_at = 0)
		   FROM users WHERE id = ?`, userID).Scan(&hash, &live); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s|%d live", hash, live)
}

const newPassword = "a brand new password"

func TestAPasswordResetWithBootstrapSignsInAndEndsOnlyThatPersonsSessions(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	users := auth.NewUsers(db)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.NewUser(t, db, "bob@example.com", auth.RoleMember)
	laptop := authtest.SignIn(t, users, "ana@example.com")
	phone := authtest.SignIn(t, users, "ana@example.com")
	bobs := authtest.SignIn(t, users, "bob@example.com")

	// One line, for automation: whatever follows it is not the password.
	pipeStdin(t, newPassword+"\nnot this line\n")
	var logs bytes.Buffer
	ctx := obs.WithLogger(t.Context(), obs.NewLoggerTo(&logs, "info", "json"))
	if err := userPassword(ctx, cfg, []string{"--bootstrap", "--email", "Ana@Example.com"}); err != nil {
		t.Fatalf("user password --bootstrap: %v", err)
	}

	if _, _, _, err := users.SignIn(t.Context(), "ana@example.com", authtest.Password, "test"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("the old password still signs in: %v", err)
	}
	if _, _, _, err := users.SignIn(t.Context(), "ana@example.com", newPassword, "test"); err != nil {
		t.Errorf("the new password does not sign in: %v", err)
	}
	for name, token := range map[string]string{"laptop": laptop, "phone": phone} {
		if _, err := users.AuthenticateSession(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("ana's %s session survived the reset: %v", name, err)
		}
	}
	if _, err := users.AuthenticateSession(t.Context(), bobs); err != nil {
		t.Errorf("bob's session ended with ana's reset: %v", err)
	}
	// The operator's event is in the log, and the password nowhere in it.
	if got := logs.String(); !strings.Contains(got, "password set by the operator") || !strings.Contains(got, ana.ID) ||
		!strings.Contains(got, `"sessions_ended":2`) || strings.Contains(got, newPassword) {
		t.Errorf("the log says %q", got)
	}
}

func TestResettingAPasswordRefusesWithoutBootstrap(t *testing.T) {
	// No route sets someone's password, so there is no daemon to ask.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	before := passwordState(t, db, ana.ID)

	input := pipeStdin(t, newPassword+"\n")
	err := userPassword(t.Context(), cfg, []string{"--email", "ana@example.com"})
	if err == nil || !strings.Contains(err.Error(), "--bootstrap") {
		t.Errorf("user password without --bootstrap: %v", err)
	}
	if input.Len() != len(newPassword)+1 {
		t.Errorf("standard input was read before refusing")
	}
	if after := passwordState(t, db, ana.ID); after != before {
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

	pipeStdin(t, newPassword+"\n")
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
	before := passwordState(t, db, ana.ID)

	// Nobody is asked to type a password for nobody.
	input := pipeStdin(t, newPassword+"\n")
	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "nobody@example.com"})
	if err == nil || !strings.Contains(err.Error(), "no person has that address") {
		t.Errorf("user password for an unknown address: %v", err)
	}
	if input.Len() != len(newPassword)+1 {
		t.Errorf("a password was read for an address nobody has")
	}
	if after := passwordState(t, db, ana.ID); after != before {
		t.Errorf("a reset for nobody changed somebody: %s, was %s", after, before)
	}
}

func TestAShortPasswordIsRefusedByTheResetAndChangesNothing(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	before := passwordState(t, db, ana.ID)

	for _, input := range []string{"too short\n", "\n", ""} {
		pipeStdin(t, input)
		if err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"}); err == nil ||
			!strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("user password with %q on standard input: %v", input, err)
		}
	}
	terminalStdin(t, "", "too short")
	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	if !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("user password with a short password at a terminal: %v", err)
	}
	if after := passwordState(t, db, ana.ID); after != before {
		t.Errorf("a refused password changed the person: %s, was %s", after, before)
	}
}

func TestAPasswordThatIsNotUTF8IsRefusedByTheResetAndChangesNothing(t *testing.T) {
	// Piped from a Latin-1 file, or typed at a terminal not set to UTF-8:
	// the console could never send these bytes, so the person could never
	// sign in with what the operator meant.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	before := passwordState(t, db, ana.ID)

	pipeStdin(t, "contrase\xf1a2026\n")
	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	if !errors.Is(err, auth.ErrPasswordNotUTF8) || !strings.HasSuffix(err.Error(), "nothing was changed") {
		t.Errorf("user password with Latin-1 on standard input: %v", err)
	}
	terminalStdin(t, "", "contrase\xf1a2026")
	err = userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	if !errors.Is(err, auth.ErrPasswordNotUTF8) || !strings.HasSuffix(err.Error(), "nothing was changed") {
		t.Errorf("user password with Latin-1 typed at a terminal: %v", err)
	}
	if after := passwordState(t, db, ana.ID); after != before {
		t.Errorf("a refused password changed the person: %s, was %s", after, before)
	}
}

func TestMismatchedEntriesAtATerminalAreRefusedAndChangeNothing(t *testing.T) {
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	before := passwordState(t, db, ana.ID)

	terminalStdin(t, "", newPassword, newPassword+"!")
	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "ana@example.com"})
	if !errors.Is(err, errPasswordsDiffer) {
		t.Errorf("user password with two different entries: %v", err)
	}
	if after := passwordState(t, db, ana.ID); after != before {
		t.Errorf("mismatched entries changed the person: %s, was %s", after, before)
	}
}

func TestTheNewPasswordMustBeTypedTheSameTwice(t *testing.T) {
	typing := func(entries ...string) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			if len(entries) == 0 {
				return nil, io.EOF
			}
			next := entries[0]
			entries = entries[1:]
			return []byte(next), nil
		}
	}
	var said strings.Builder
	say := func(s string) { said.WriteString(s) }

	got, err := readNewPassword(t.Context(), typing(newPassword, newPassword), say, "ana@example.com")
	if err != nil || got != newPassword {
		t.Errorf("two equal entries: %q, %v", got, err)
	}
	if !strings.Contains(said.String(), "ana@example.com") || strings.Contains(said.String(), newPassword) {
		t.Errorf("the prompts say %q: they must name the person and never show the password", said.String())
	}
	if _, err := readNewPassword(t.Context(), typing(newPassword, "a brand new passwork"), say, "ana@example.com"); !errors.Is(err, errPasswordsDiffer) {
		t.Errorf("two different entries: %v", err)
	}
	if _, err := readNewPassword(t.Context(), typing(newPassword, newPassword+" "), say, "ana@example.com"); !errors.Is(err, errPasswordsDiffer) {
		t.Errorf("entries that differ by a trailing space: %v", err)
	}
	// A short first entry is refused before the second is asked for.
	asked := 0
	counting := func(context.Context) ([]byte, error) { asked++; return []byte("short"), nil }
	if _, err := readNewPassword(t.Context(), counting, say, "ana@example.com"); !errors.Is(err, auth.ErrPasswordTooShort) || asked != 1 {
		t.Errorf("a short first entry: %v after %d entries, want ErrPasswordTooShort after 1", err, asked)
	}
	if _, err := readNewPassword(t.Context(), typing(newPassword), say, "ana@example.com"); !errors.Is(err, io.EOF) {
		t.Errorf("a terminal that closes before the second entry: %v", err)
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
		go func() { <-f.hidden; f.typed <- []byte(newPassword) }()
		line, err := readHiddenFrom(t.Context(), f)
		if err != nil || string(line) != newPassword || !f.echoing() {
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

func TestAnAddressFromStandardInputNeedsThePasswordTypedAtATerminal(t *testing.T) {
	// Standard input cannot carry both the address and the password: a
	// pipe after --email - is refused, and at a terminal the address is
	// typed in the clear and the password twice without echo.
	cfg := localConfig(t)
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	before := passwordState(t, db, ana.ID)

	pipeStdin(t, "ana@example.com\n"+newPassword+"\n")
	err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "-"})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("user password --email - from a pipe: %v", err)
	}
	if after := passwordState(t, db, ana.ID); after != before {
		t.Errorf("a refused reset changed the person: %s, was %s", after, before)
	}

	terminalStdin(t, "ana@example.com\n", newPassword, newPassword)
	if err := userPassword(t.Context(), cfg, []string{"--bootstrap", "--email", "-"}); err != nil {
		t.Fatalf("user password --email - at a terminal: %v", err)
	}
	if _, _, _, err := auth.NewUsers(db).SignIn(t.Context(), "ana@example.com", newPassword, "test"); err != nil {
		t.Errorf("the password typed at the terminal does not sign in: %v", err)
	}
}

func TestThePasswordIsNeverAnArgument(t *testing.T) {
	// An argument is in the process listing and the shell history; a
	// stray one here may well be the password, so it is not quoted back.
	pipeStdin(t, "")
	err := userPassword(t.Context(), localConfig(t), []string{"--bootstrap", "--email", "ana@example.com", newPassword})
	if err == nil || strings.Contains(err.Error(), newPassword) {
		t.Errorf("a password given as an argument: %v", err)
	}
	args := []string{"--bootstrap", "--email", "ana@example.com", "--password", newPassword}
	if err := userPassword(t.Context(), localConfig(t), args); err == nil {
		t.Errorf("--password was accepted")
	}
}
