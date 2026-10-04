package main

import (
	"context"
	"errors"
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
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
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
