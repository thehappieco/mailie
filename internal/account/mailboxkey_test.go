package account_test

import (
	"bytes"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A mailbox's key pair and its linker's grant, written with the link
// (docs/key-scheme.md sections 8 and 12.11).

// keyRegistry is a registry that links iCloud accounts against an in-process
// server, and Gmail ones through the pasted flow of an installed client.
func keyRegistry(t *testing.T, address, password string) (*account.Registry, *account.Repository, *store.Store) {
	t.Helper()
	repo, db := newRepo(t)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		SpoolDir:          t.TempDir(),
		Google:            account.OAuthClient{ClientID: "client"},
		AllowInsecureAuth: true,
		AllowPrivate:      true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: address, Password: password})
	registry.RedirectForTest(appleIMAP, srv.Addr)
	return registry, repo, db
}

func TestALinkWritesTheMailboxKeyAndTheLinkersGrantOnBothPaths(t *testing.T) {
	const address, password = "ana@icloud.com", "abcd-efgh-ijkl-mnop"
	registry, repo, db := keyRegistry(t, address, password)
	ws := workspace.NewRepository(db, nil)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)

	byPassword := authtest.LinkKey(t)
	withPassword, _, err := registry.Add(t.Context(), account.AddRequest{
		Email: address, Provider: provider.KindIMAP, ICloud: true, Password: password, LinkerID: ana.ID, Key: &byPassword,
	})
	if err != nil {
		t.Fatalf("the password form: %v", err)
	}
	byOAuth := authtest.LinkKey(t)
	withOAuth, flow, err := registry.Add(t.Context(), account.AddRequest{
		Email: "ana@gmail.com", Provider: provider.KindGmail, Flow: account.FlowPasted, LinkerID: ana.ID, Key: &byOAuth,
	})
	if err != nil || flow == nil {
		t.Fatalf("the OAuth flow: %v, %+v", err, flow)
	}
	if withOAuth.State != account.StatePendingAuth {
		t.Errorf("the OAuth mailbox is %q before its consent", withOAuth.State)
	}

	for _, link := range []struct {
		account account.Account
		key     workspace.LinkKey
	}{{withPassword, byPassword}, {withOAuth, byOAuth}} {
		got, err := ws.CurrentKey(t.Context(), link.account.ID)
		if err != nil || got.Epoch != 1 || !bytes.Equal(got.PublicKey, link.key.PublicKey) ||
			got.Namespace != link.key.Namespace || got.CreatedBy != ana.ID {
			t.Errorf("%s's key: %+v, %v", link.account.Email, got, err)
		}
		state, err := ws.KeyState(t.Context(), link.account.ID, ana.ID)
		if err != nil || !bytes.Equal(state.Grant, link.key.Grant) || !state.Reads {
			t.Errorf("%s: ana's grant %x, reads %v (%v)", link.account.Email, state.Grant, state.Reads, err)
		}
	}
	// She reads both, by the flag and the grant.
	readable, err := repo.ListVisible(t.Context(), account.Visibility{UserID: ana.ID, Need: workspace.Flags{Read: true}})
	if err != nil || len(readable) != 2 {
		t.Errorf("ana reads %d mailboxes (%v), want both", len(readable), err)
	}
	// Resuming the consent carries no key: the row has its own.
	if _, err := registry.StartAuth(t.Context(), withOAuth.ID, account.FlowPasted, ana.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := ws.CurrentKey(t.Context(), withOAuth.ID); err != nil || got.Namespace != byOAuth.Namespace {
		t.Errorf("after resuming the consent: %+v, %v", got, err)
	}
}

func TestAnOperatorLinkCarriesNoKey(t *testing.T) {
	const address, password = "ops@icloud.com", "abcd-efgh-ijkl-mnop"
	registry, repo, db := keyRegistry(t, address, password)
	key := authtest.LinkKey(t)
	for _, req := range []account.AddRequest{
		{Email: address, Provider: provider.KindIMAP, ICloud: true, Password: password, Key: &key},
		{Email: "ops@gmail.com", Provider: provider.KindGmail, Flow: account.FlowPasted, Key: &key},
	} {
		if _, _, err := registry.Add(t.Context(), req); !errors.Is(err, account.ErrOperatorKey) {
			t.Errorf("%s nobody links, with a key: %v, want ErrOperatorKey", req.Email, err)
		}
	}
	if list, err := repo.List(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("refused links stored %d mailboxes (%v)", len(list), err)
	}
	// Without one, the operator links as before: keyless.
	created, _, err := registry.Add(t.Context(), account.AddRequest{
		Email: address, Provider: provider.KindIMAP, ICloud: true, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.NewRepository(db, nil).CurrentKey(t.Context(), created.ID); !errors.Is(err, workspace.ErrKeyless) {
		t.Errorf("an operator mailbox's key: %v", err)
	}
}

func TestALinkThatCannotBeKeyedStoresNoMailbox(t *testing.T) {
	registry, repo, db := keyRegistry(t, "unused@icloud.com", "unused")
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	dan := authtest.NewLegacyUser(t, db, "dan@example.org", auth.RoleMember)
	link := func(linker, email string, key workspace.LinkKey) error {
		_, _, err := registry.Add(t.Context(), account.AddRequest{
			Email: email, Provider: provider.KindGmail, Flow: account.FlowPasted, LinkerID: linker, Key: &key,
		})
		return err
	}
	used := authtest.LinkKey(t)
	if err := link(ana.ID, "ana@gmail.com", used); err != nil {
		t.Fatal(err)
	}
	lowOrder := authtest.LinkKey(t)
	lowOrder.PublicKey = make([]byte, 32)
	badNamespace := authtest.LinkKey(t)
	badNamespace.Namespace = "Not-A-Namespace"
	laterEpoch := authtest.LinkKey(t)
	laterEpoch.Grant = authtest.Grant(t, 2)
	shortGrant := authtest.LinkKey(t)
	shortGrant.Grant = shortGrant.Grant[:87]
	again := authtest.LinkKey(t)
	again.Namespace = used.Namespace
	for _, tc := range []struct {
		what   string
		linker string
		key    workspace.LinkKey
		want   error
	}{
		{"a public key of low order", ana.ID, lowOrder, keyscheme.ErrPublicKey},
		{"a namespace outside its spelling", ana.ID, badNamespace, keyscheme.ErrBinding},
		{"a grant at epoch 2", ana.ID, laterEpoch, workspace.ErrEpoch},
		{"a grant of another length", ana.ID, shortGrant, keyscheme.ErrShape},
		{"another mailbox's namespace", ana.ID, again, workspace.ErrNamespaceTaken},
		{"a linker without an account key", dan.ID, authtest.LinkKey(t), workspace.ErrNotEnrolled},
	} {
		if err := link(tc.linker, "other@gmail.com", tc.key); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.what, err, tc.want)
		}
	}
	list, err := repo.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var emails []string
	for _, a := range list {
		emails = append(emails, a.Email)
	}
	if !slices.Equal(emails, []string{"ana@gmail.com"}) {
		t.Errorf("mailboxes stored: %v, want the one keyed link", emails)
	}
}

func TestAKeyedMailboxShowsAMemberWaitingForTheKeyItsCardAndNothingToRead(t *testing.T) {
	f := newVisibility(t)
	ana := f.person("ana@example.org")
	dan := authtest.NewLegacyUser(t, f.db, "dan@example.org", auth.RoleMember)
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return f.ws.AddMemberTx(t.Context(), tx, team.ID, dan.ID, workspace.RoleMember, f.db.Now())
	}); err != nil {
		t.Fatal(err)
	}
	box := f.link("acc_box", team.ID, ana.ID, "support@example.org")
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	if _, err := f.ws.SetGrant(t.Context(), box.ID, dan.ID, workspace.Flags{Read: true, Act: true, Send: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}

	if got := f.sees(account.Visibility{UserID: dan.ID}); !slices.Equal(got, []string{box.ID}) {
		t.Errorf("dan, waiting for the key, sees %v; want the card", got)
	}
	for _, need := range []workspace.Flags{{Read: true}, {Act: true}, {Read: true, Act: true}} {
		f.hidden(box.ID, account.Visibility{UserID: dan.ID, Need: need})
	}
	if got := f.sees(account.Visibility{UserID: dan.ID, Need: workspace.Flags{Send: true}}); !slices.Equal(got, []string{box.ID}) {
		t.Errorf("dan sends from %v", got)
	}
	if got := f.sees(account.Visibility{UserID: ana.ID, Need: workspace.Flags{Read: true, Act: true}}); !slices.Equal(got, []string{box.ID}) {
		t.Errorf("ana, who holds the key, reads and acts on %v", got)
	}
	// Supplied the key, he reads it.
	if _, err := f.db.Writer().ExecContext(t.Context(), `UPDATE users SET public_key = ? WHERE id = ?`,
		authtest.PublicKey(t), dan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ws.SupplyGrant(t.Context(), box.ID, dan.ID, ana.ID, 1, authtest.Grant(t, 1), nil); err != nil {
		t.Fatal(err)
	}
	if got := f.sees(account.Visibility{UserID: dan.ID, Need: workspace.Flags{Read: true, Act: true}}); !slices.Equal(got, []string{box.ID}) {
		t.Errorf("dan, supplied the key, reads %v", got)
	}
}
