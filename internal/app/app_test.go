package app

import (
	"bytes"
	"context"
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
	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/webui"
)

// noMailboxes is a sweep of removed mailboxes that finds none.
func noMailboxes(context.Context, time.Duration) (int, int, error) { return 0, 0, nil }

// apiNotFound is the API's own 404 body.
const apiNotFound = `{"code":"not_found","message":"no such endpoint"}`

func TestTheFirstInviteHintOnlySuggestsACommandThatCanWork(t *testing.T) {
	// The REST form needs an admin key and MAIL_PUBLIC_URL, and answers 409
	// without the latter; the bootstrap form refuses to run beside the daemon
	// that is printing the hint, so it must always say to stop it first.
	for _, c := range []struct {
		name              string
		publicURL, anyKey bool
		want, never       string
	}{
		{"no public url", false, true, "set MAIL_PUBLIC_URL", "(needs MAIL_ADMIN_KEY)"},
		{"no key to call it with", true, false, "stop the daemon and run: mailserver user invite --bootstrap", "(needs MAIL_ADMIN_KEY)"},
		{"both", true, true, "mailserver user invite --role owner --email ADDRESS (needs MAIL_ADMIN_KEY)", ""},
	} {
		// Nobody becomes an owner by signing up first: the invite has to
		// say so.
		if hint := firstInviteHint(c.publicURL, c.anyKey); strings.Count(hint, "--role owner") != strings.Count(hint, "user invite") {
			t.Errorf("%s: hint %q invites the first person without the owner role", c.name, hint)
		}
		hint := firstInviteHint(c.publicURL, c.anyKey)
		if !strings.Contains(hint, c.want) {
			t.Errorf("%s: hint %q does not say %q", c.name, hint, c.want)
		}
		if c.never != "" && strings.Contains(hint, c.never) {
			t.Errorf("%s: hint %q suggests a command that cannot work here", c.name, hint)
		}
		if strings.Contains(hint, "--bootstrap") && !strings.Contains(hint, "stop") && !strings.Contains(hint, "stopped") {
			t.Errorf("%s: hint %q offers --bootstrap without saying the daemon must be stopped", c.name, hint)
		}
	}
}

func TestSectionsACrashLeftInTheSpoolAreRemovedAtStart(t *testing.T) {
	// A download spools the section it fetched and removes it when the
	// response ends; a send spools its attachments and removes them when it
	// ends. A daemon killed in between leaves a message's content on disk,
	// which the next start must not keep.
	dir := t.TempDir()
	for _, name := range []string{"part-123", "part-456", "send-789"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("Olá, the body of a message"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := sweepSpool(dir)
	if err != nil || n != 3 {
		t.Fatalf("sweepSpool = %d, %v; want the two spooled sections and the attachment removed", n, err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != "unrelated" {
		t.Errorf("left behind %v; only files the adapter spools are its to remove", left)
	}
	if n, err := sweepSpool(filepath.Join(dir, "never-created")); err != nil || n != 0 {
		t.Errorf("a spool directory that does not exist yet is not an error: %d, %v", n, err)
	}
}

// testConsole is a built console in a temporary directory.
func testConsole(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<!doctype html><title>Console</title>`), 0o644); err != nil {
		t.Fatal(err)
	}
	web, err := webui.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = web.Close() })
	return web
}

func TestWithMCPOverHTTPOffEveryRequestToMCPIsTheAPIs404(t *testing.T) {
	// Switched off, /mcp is an endpoint that does not exist: the same JSON
	// whether a console is served behind it or not, and for every method
	// and path under it — a client probing it learns nothing else.
	for name, console := range map[string]http.Handler{"without a console": nil, "with a console": testConsole(t)} {
		mux, err := routes(&api.Handler{}, nil, nil, console)
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			for _, path := range []string{"/mcp", "/mcp/", "/mcp/sessions/1"} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(`{"jsonrpc":"2.0"}`)))
				if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" ||
					rec.Body.String() != apiNotFound {
					t.Errorf("%s: %s %s answered %d %q %q", name, method, path, rec.Code,
						rec.Header().Get("Content-Type"), rec.Body.String())
				}
			}
		}
		// The rest of the API is untouched, and the OAuth discovery
		// documents an MCP client probes for are still missing.
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: the health probe answered %d", name, rec.Code)
		}
		for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server"} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: GET %s answered %d", name, path, rec.Code)
			}
		}
	}
}

func TestWithMCPOverHTTPOnTheEndpointIsTheMCPHandler(t *testing.T) {
	mcpHTTP := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for name, console := range map[string]http.Handler{"without a console": nil, "with a console": testConsole(t)} {
		mux, err := routes(&api.Handler{}, mcpHTTP, nil, console)
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/mcp", nil))
			if rec.Code != http.StatusTeapot {
				t.Errorf("%s: %s /mcp answered %d, not from the MCP handler", name, method, rec.Code)
			}
		}
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
			housekeep(ctx, users, db.SweepSends, noMailboxes, nil, db.Scrub, slog.New(slog.DiscardHandler), every)
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

	// A key pinned for a sign-in that was refused, long enough ago that no
	// sign-in can still be under way: its identity signs nobody in.
	mu.Lock()
	now = now.Add(-auth.UnlinkedPinGrace - time.Minute)
	mu.Unlock()
	if _, _, err := users.PinKey(t.Context(), "https://accounts.example.com", "subject-of-nobody-here", "k1", []byte("k")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(auth.UnlinkedPinGrace + time.Minute)
	mu.Unlock()
	unlinked := func() int {
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM identity_key_pins`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// With an hour to the first tick, only the sweep at start can do this.
	expire("first@example.com")
	stop := run(time.Hour)
	waitFor("the sweep at start", func() bool {
		return pending() == 0 && !onDisk("first@example.com") && unlinked() == 0 && !onDisk("subject-of-nobody-here")
	})
	stop()

	stop = run(20 * time.Millisecond)
	expire("second@example.com")
	waitFor("the next scheduled sweep", func() bool { return pending() == 0 && !onDisk("second@example.com") })
	stop()
}

func TestHousekeepingPurgesRemovedMailboxesPastTheirThirtyDays(t *testing.T) {
	var mu sync.Mutex
	removedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := removedAt
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	at := func(when time.Time) { mu.Lock(); now = when; mu.Unlock() }
	db := storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), clock)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytes.Repeat([]byte{0xA1}, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, keyring).WithClock(clock)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	logger := slog.New(slog.DiscardHandler)
	svc := service.New(service.Deps{Accounts: registry, Store: db, Now: clock, Log: logger})
	users := auth.NewUsersWithClock(db, clock)
	ctx := t.Context()
	link := func(id, email string) {
		t.Helper()
		if _, err := repo.Create(ctx, account.Account{ID: id, Email: email, Provider: provider.KindIMAP,
			AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
			SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email}, ""); err != nil {
			t.Fatal(err)
		}
		if err := repo.SavePassword(ctx, id, "correct horse battery staple"); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Remove(ctx, id, "cli", nil); err != nil {
			t.Fatal(err)
		}
	}
	const expired, recent = "acc_00000000000000a1", "acc_00000000000000a2"
	link(expired, "expired@mail.example")
	at(removedAt.Add(48 * time.Hour))
	link(recent, "recent@mail.example")
	// An entry of the mailbox journal older than its retention.
	if _, err := db.Writer().ExecContext(ctx, `INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor,
		created_at) VALUES ('mbe_00000000000000e1', 'recovered', 'wsp_operator', 'acc_gone', 'cli', ?)`,
		removedAt.Add(-91*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	// The first one's 30 days are over, the second one's are not: with an
	// hour to the first tick, only the sweep at start can do this.
	at(removedAt.Add(account.RemovedKeptFor))
	swept := make(chan struct{}, 1)
	sweepMailboxes := func(ctx context.Context, every time.Duration) (int, int, error) {
		mailboxes, entries, err := svc.SweepRemovedMailboxes(ctx, every)
		select {
		case swept <- struct{}{}:
		default:
		}
		return mailboxes, entries, err
	}
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		housekeep(sweepCtx, users, db.SweepSends, sweepMailboxes, nil, db.Scrub, logger, time.Hour)
	}()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the sweep at start")
	}
	cancel()
	<-done

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Reader().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM accounts WHERE id = ?`, expired); n != 0 {
		t.Error("the mailbox past its 30 days is still there")
	}
	if n := count(`SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, recent); n != 1 {
		t.Error("the mailbox within its 30 days was deleted")
	}
	if n := count(`SELECT count(*) FROM mailbox_events WHERE id = 'mbe_00000000000000e1'`); n != 0 {
		t.Error("the journal keeps an entry past its retention")
	}
	for _, file := range []string{db.Path(), db.Path() + "-wal"} {
		raw, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "expired@mail.example") {
			t.Errorf("%s still holds the address of the mailbox deleted for good", filepath.Base(file))
		}
	}
}
