package account_test

import (
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
)

// What a mailbox's row keeps of how it connected (migration 0027): what its
// server turned out to be, and when it first stored something to sign in
// with.

// connectedAt is what a mailbox's row says of its first connection, unix
// seconds.
func connectedAt(t *testing.T, db *store.Store, id string) int64 {
	t.Helper()
	var at int64
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT connected_at FROM accounts WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestAPasswordMailboxRecordsWhatItsServerIsWhenItIsAdded(t *testing.T) {
	// The in-process server advertises only the capabilities go-imap
	// knows, never Gmail's extension: a server that advertises it is
	// ServerKindOf's test, and the sync login's (internal/sync).
	for _, tc := range []struct {
		name, host string
		want       string
	}{
		{"a server of its own", "imap.mail.example", account.ServerOther},
		// Google's own host, by its name, as another IMAP server.
		{"Google's host", "imap.gmail.com", account.ServerGmail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, db := newRepo(t)
			registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
				SpoolDir: t.TempDir(), AllowInsecureAuth: true, AllowPrivate: true,
			})
			t.Cleanup(func() { _ = registry.Close() })
			srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{
				User: "ana@workspace.example", Password: "hunter2",
			})
			registry.RedirectForTest(tc.host+":993", srv.Addr)
			now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
			repo.WithClock(func() time.Time { return now })
			created, _, err := registry.Add(t.Context(), account.AddRequest{
				Email: "ana@workspace.example", Provider: provider.KindIMAP, Password: "hunter2",
				IMAPHost: tc.host, IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
			})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			stored, err := repo.Get(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.ServerKind != tc.want || created.ServerKind != tc.want {
				t.Errorf("the server is recorded as %q (returned %q), want %q", stored.ServerKind, created.ServerKind, tc.want)
			}
			// Connected when its password was stored, in the write that
			// created it.
			if got := connectedAt(t, db, created.ID); got != now.Unix() {
				t.Errorf("it connected at %d, want %d", got, now.Unix())
			}
		})
	}
}

func TestAServerKindIsWrittenOnlyWhenItChanges(t *testing.T) {
	repo, db := newRepo(t)
	a := seed(t, repo, "ana@gmail.example", provider.KindIMAP)
	if a.ServerKind != "" {
		t.Fatalf("a mailbox nobody logged in to yet has the server %q", a.ServerKind)
	}
	writes := func() int {
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), `SELECT changes FROM temp_server_kind_writes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := db.Writer().ExecContext(t.Context(), `CREATE TABLE temp_server_kind_writes (changes INTEGER NOT NULL);
		INSERT INTO temp_server_kind_writes VALUES (0);
		CREATE TRIGGER count_server_kind AFTER UPDATE OF server_kind ON accounts
		BEGIN UPDATE temp_server_kind_writes SET changes = changes + 1; END;`); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{account.ServerOther, account.ServerOther, account.ServerGmail, account.ServerGmail} {
		if err := repo.SetServerKind(t.Context(), a.ID, kind); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.Get(t.Context(), a.ID)
	if err != nil || got.ServerKind != account.ServerGmail || writes() != 2 {
		t.Errorf("the server %q after %d writes (%v); want gmail after the 2 that changed it", got.ServerKind, writes(), err)
	}
}

func TestServerKindOfIsTheSendPathsGmailTest(t *testing.T) {
	for _, tc := range []struct {
		caps provider.Caps
		host string
		want string
	}{
		{provider.Caps{}, "imap.mail.example", account.ServerOther},
		{provider.Caps{Raw: []string{"IMAP4rev1", "X-GM-EXT-1"}}, "imap.mail.example", account.ServerGmail},
		{provider.Caps{}, "IMAP.GMAIL.COM.", account.ServerGmail},
		{provider.Caps{}, "imap.googlemail.com", account.ServerGmail},
	} {
		if got := account.ServerKindOf(tc.caps, tc.host); got != tc.want || (got == account.ServerGmail) !=
			provider.IsGmailServer(tc.caps, tc.host) {
			t.Errorf("ServerKindOf(%v, %q) = %q, want %q", tc.caps.Raw, tc.host, got, tc.want)
		}
	}
}

func TestAMailboxKeepsTheMomentItFirstConnectedThroughAReauthorization(t *testing.T) {
	repo, db := newRepo(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	repo.WithClock(func() time.Time { return now })
	box := seed(t, repo, "ana@gmail.example", provider.KindGmail)
	if got := connectedAt(t, db, box.ID); got != 0 {
		t.Fatalf("a mailbox that never stored a grant connected at %d", got)
	}
	first := now.Add(time.Hour)
	now = first
	if err := repo.SaveToken(t.Context(), box.ID, &oauth2.Token{RefreshToken: "first"}); err != nil {
		t.Fatal(err)
	}
	// Its token refreshed, and a re-authorization's new grant: stored
	// again over the first, which keeps its moment.
	for _, refresh := range []string{"refreshed", "re-authorized"} {
		now = now.Add(time.Hour)
		if err := repo.SaveToken(t.Context(), box.ID, &oauth2.Token{RefreshToken: refresh}); err != nil {
			t.Fatal(err)
		}
	}
	if got := connectedAt(t, db, box.ID); got != first.Unix() {
		t.Errorf("the mailbox connected at %d, want its first grant's %d", got, first.Unix())
	}
}
