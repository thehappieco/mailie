package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// fakeDaemon answers GET /v1/accounts/{id} with whatever the test scripts,
// one reply per poll.
func fakeDaemon(t *testing.T, replies ...func(http.ResponseWriter)) (config.Config, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deadbeef.secret" {
			t.Errorf("poll without the admin key: %q", r.Header.Get("Authorization"))
		}
		n := int(polls.Add(1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		replies[n](w)
	}))
	t.Cleanup(srv.Close)
	return config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}, &polls
}

func state(s, reason string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"acc_1","state":"` + s + `","state_reason":"` + reason + `"}`))
	}
}

func TestWaitingForConsentRidesOutARateLimit(t *testing.T) {
	// The person is halfway through a consent screen; a 429 on the poll is
	// no reason to abandon them.
	throttled := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","message":"too many requests"}`))
	}
	cfg, polls := fakeDaemon(t, throttled, state("active", ""))

	started := time.Now()
	if err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute); err != nil {
		t.Fatalf("waitForState: %v", err)
	}
	if polls.Load() != 2 {
		t.Errorf("polled %d times, want 2", polls.Load())
	}
	// Never faster than the poll interval, even when Retry-After says less.
	if waited := time.Since(started); waited < pollInterval {
		t.Errorf("retried after %v, want at least %v", waited, pollInterval)
	}
}

func TestWaitingForConsentStopsWhenTheAccountFails(t *testing.T) {
	cfg, polls := fakeDaemon(t, state("error", "consent was declined"))
	err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "consent was declined") {
		t.Fatalf("err = %v, want the reason the account failed", err)
	}
	if polls.Load() != 1 {
		t.Errorf("polled %d times after the account failed", polls.Load())
	}
}

func TestWaitingForConsentGivesUpOnARealError(t *testing.T) {
	cfg, _ := fakeDaemon(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"no such account"}`))
	})
	if err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "no such account") {
		t.Fatalf("err = %v", err)
	}
}

func TestACustomDomainOnICloudCanNameItsSignInFromTheCLI(t *testing.T) {
	// Apple refuses an iCloud+ custom-domain address as the login; the
	// account's iCloud address has to travel with it.
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"acc_1","email":"ana@lima.example","provider":"icloud","auth_kind":"password","state":"active"}}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}

	err := accountAdd(t.Context(), cfg, []string{
		"--email", "ana@lima.example", "--provider", "icloud", "--password", "abcd-efgh-ijkl-mnop",
		"--login-user", "ana@icloud.com",
	})
	if err != nil {
		t.Fatalf("account add: %v", err)
	}
	if sent["login_user"] != "ana@icloud.com" || sent["email"] != "ana@lima.example" || sent["provider"] != "icloud" {
		t.Errorf("sent %v", sent)
	}
	for _, key := range []string{"imap_host", "imap_port", "smtp_host", "smtp_port", "smtp_tls"} {
		if _, ok := sent[key]; ok {
			t.Errorf("sent %s, which the daemon refuses for iCloud", key)
		}
	}
}

// linkOperatorMailbox stores a mailbox of the operator workspace, as linking
// one does; connected gives it a stored credential, as a login that worked
// leaves. One without never connected, and is deleted at once when removed.
func linkOperatorMailbox(t *testing.T, db *store.Store, id, email string, connected bool) {
	t.Helper()
	repo := account.NewRepository(db, daemonKeyring(t))
	if _, err := repo.Create(t.Context(), account.Account{
		ID: id, WorkspaceID: workspace.OperatorID, Email: email, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, ""); err != nil {
		t.Fatal(err)
	}
	if connected {
		if err := repo.SavePassword(t.Context(), id, "correct horse battery staple"); err != nil {
			t.Fatal(err)
		}
	}
}

// removal is where a mailbox's removal stands in the database: when it may
// be deleted for good (zero while it is live), and when a recovery held its
// sync. found is false once it is deleted for good.
func removal(t *testing.T, db *store.Store, id string) (purgeAfter, syncHeldAt int64, found bool) {
	t.Helper()
	err := db.Reader().QueryRowContext(t.Context(), `SELECT purge_after, sync_held_at FROM accounts WHERE id = ?`, id).
		Scan(&purgeAfter, &syncHeldAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return purgeAfter, syncHeldAt, true
}

func TestTheOperatorRemovesRecoversAndDeletesForGoodFromTheCommandLine(t *testing.T) {
	cfg, db := realDaemon(t)
	ctx := t.Context()
	const kept, never = "acc_00000000000000b1", "acc_00000000000000b2"
	linkOperatorMailbox(t, db, kept, "ops@mail.example", true)
	linkOperatorMailbox(t, db, never, "new@mail.example", false)
	command := func(args ...string) (stdout, stderr string) {
		t.Helper()
		stdout, stderr, err := captureOutput(t, func() error { return accountCommand(ctx, cfg, args) })
		if err != nil {
			t.Fatalf("account %s: %v", strings.Join(args, " "), err)
		}
		return stdout, stderr
	}

	// Removed: kept 30 days, and the command says until when and how to
	// bring it back or finish it. Written as the usage writes it, the flag
	// after the id.
	_, said := command("remove", kept, "--yes")
	purgeAfter, _, found := removal(t, db, kept)
	if !found || purgeAfter == 0 {
		t.Fatalf("the removed mailbox: purge_after %d, found %v", purgeAfter, found)
	}
	until := time.Unix(purgeAfter, 0).UTC().Format(time.RFC3339)
	for _, want := range []string{"kept until " + until, "account recover " + kept, "account purge " + kept} {
		if !strings.Contains(said, want) {
			t.Errorf("account remove said %q, without %q", said, want)
		}
	}
	// A link that never connected goes at once, and says so.
	if _, said := command("remove", "--yes", never); !strings.Contains(said, "never connected") {
		t.Errorf("removing a link that never connected said %q", said)
	}
	if _, _, found := removal(t, db, never); found {
		t.Error("a link that never connected is kept")
	}

	// Listed in the operator workspace, which is the default.
	listedRow := func() string {
		t.Helper()
		out, _ := command("removed")
		for line := range strings.Lines(out) {
			if strings.HasPrefix(line, kept+" ") {
				return line
			}
		}
		return ""
	}
	if row := listedRow(); !strings.Contains(row, "ops@mail.example") || !strings.Contains(row, until) {
		t.Errorf("account removed lists %q", row)
	}

	// Recovered, its sync held until the operator switches it on.
	if _, said := command("recover", kept); !strings.Contains(said, "recovered ops@mail.example") ||
		!strings.Contains(said, "account sync "+kept+" on") {
		t.Errorf("account recover said %q", said)
	}
	if purgeAfter, held, _ := removal(t, db, kept); purgeAfter != 0 || held == 0 {
		t.Errorf("after the recovery: purge_after %d, sync held at %d", purgeAfter, held)
	}
	if row := listedRow(); row != "" {
		t.Errorf("a recovered mailbox is still listed: %q", row)
	}
	if out, _ := command("sync", kept, "status"); !strings.HasPrefix(out, "sync held since ") {
		t.Errorf("account sync status of a held mailbox said %q", out)
	}
	if out, _ := command("sync", kept, "on"); !strings.HasPrefix(out, "sync on") {
		t.Errorf("account sync on said %q", out)
	}
	if _, held, _ := removal(t, db, kept); held != 0 {
		t.Error("switching sync on left the recovery's hold")
	}

	// Deleting for good takes a removed mailbox.
	var failure *apiError
	if err := accountCommand(ctx, cfg, []string{"purge", "--yes", kept}); !errors.As(err, &failure) || failure.Code != "conflict" {
		t.Errorf("deleting a live mailbox for good: %v", err)
	}
	command("remove", "--yes", kept)
	if _, said := command("purge", kept, "--yes"); !strings.Contains(said, "deleted for good") {
		t.Errorf("account purge said %q", said)
	}
	if _, _, found := removal(t, db, kept); found {
		t.Error("the mailbox deleted for good is still there")
	}
	if _, said := command("removed"); !strings.Contains(said, "no removed mailboxes in "+workspace.OperatorID) {
		t.Errorf("an empty list said %q", said)
	}
}

func TestTheOperatorDeletesAPersonsRemovedMailboxForGoodAndNeverRecoversIt(t *testing.T) {
	cfg, db := realDaemon(t)
	ctx := t.Context()
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	personal := authtest.Personal(t, db, ana.ID)
	const mine = "acc_00000000000000c1"
	repo := account.NewRepository(db, daemonKeyring(t))
	if _, err := repo.Create(ctx, account.Account{
		ID: mine, WorkspaceID: personal, Email: "ana@mail.example", Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "ana@mail.example", State: account.StateActive,
	}, ana.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePassword(ctx, mine, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	registry := account.NewRegistry(ctx, repo, account.RegistryOptions{SpoolDir: t.TempDir()})
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.Remove(ctx, mine, ana.ID, nil); err != nil {
		t.Fatal(err)
	}

	out, _, err := captureOutput(t, func() error {
		return accountCommand(ctx, cfg, []string{"removed", "--workspace", personal})
	})
	if err != nil || !strings.Contains(out, mine) || !strings.Contains(out, "ana@mail.example") {
		t.Errorf("account removed --workspace lists %q (%v)", out, err)
	}
	// Recovering it is Ana's to decide, never the operator's.
	var failure *apiError
	if err := accountCommand(ctx, cfg, []string{"recover", mine}); !errors.As(err, &failure) || failure.Code != "not_authorized" {
		t.Errorf("the operator recovering a person's mailbox: %v", err)
	}
	if purgeAfter, _, _ := removal(t, db, mine); purgeAfter == 0 {
		t.Error("the operator recovered a person's mailbox")
	}
	// Erasing it when she asks is the operator's.
	if _, _, err := captureOutput(t, func() error {
		return accountCommand(ctx, cfg, []string{"purge", "--yes", mine})
	}); err != nil {
		t.Fatalf("account purge: %v", err)
	}
	if _, _, found := removal(t, db, mine); found {
		t.Error("the operator's deletion for good left the mailbox")
	}
}

func TestAnAccountCommandReadsItsFlagsBeforeOrAfterTheID(t *testing.T) {
	for _, args := range [][]string{
		{"acc_00000000000000b1", "--yes"},
		{"--yes", "acc_00000000000000b1"},
		{"acc_00000000000000b1", "-yes=true"},
	} {
		fs := flag.NewFlagSet("account remove", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "")
		rest, err := parseInterspersed(fs, args)
		if err != nil || !*yes || len(rest) != 1 || rest[0] != "acc_00000000000000b1" {
			t.Errorf("%q: yes %v, arguments %q (%v)", args, *yes, rest, err)
		}
	}
	// Two arguments around a flag stay two, in their order.
	fs := flag.NewFlagSet("account sync", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	rest, err := parseInterspersed(fs, []string{"acc_00000000000000b1", "--yes", "off"})
	if err != nil || !*yes || strings.Join(rest, " ") != "acc_00000000000000b1 off" {
		t.Errorf("account sync ID --yes off: yes %v, arguments %q (%v)", *yes, rest, err)
	}
	// Without a flag, nothing changes.
	fs = flag.NewFlagSet("account remove", flag.ContinueOnError)
	yes = fs.Bool("yes", false, "")
	if rest, err := parseInterspersed(fs, []string{"acc_00000000000000b1"}); err != nil || *yes || len(rest) != 1 {
		t.Errorf("account remove ID: yes %v, arguments %q (%v)", *yes, rest, err)
	}
}
