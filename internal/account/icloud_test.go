package account_test

import (
	"errors"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

// appleIMAP is the server Apple documents for iCloud Mail, the one address an
// iCloud account's login is tried against.
const appleIMAP = "imap.mail.me.com:993"

// icloudRegistry is a registry whose connections to Apple's IMAP server land
// on an in-process server instead. The server knows one user, the whole
// address, so a login under any other name is refused.
func icloudRegistry(t *testing.T, address, password string) (*account.Registry, *account.Repository) {
	t.Helper()
	repo, _ := newRepo(t)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		SpoolDir: t.TempDir(),
		// The stand-in speaks plain TCP on 127.0.0.1.
		AllowInsecureAuth: true,
		AllowPrivate:      true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: address, Password: password})
	registry.RedirectForTest(appleIMAP, srv.Addr)
	return registry, repo
}

func TestAnICloudAccountGetsApplesServers(t *testing.T) {
	for _, tc := range []struct {
		address, login string
		// signIn is the one name the stand-in accepts, as Apple would.
		signIn string
	}{
		{address: "ana@icloud.com", signIn: "ana@icloud.com"},
		// An iCloud+ custom domain: iCloud because the person said so, and
		// signed in as the account's own iCloud address, since Apple refuses
		// the custom-domain one.
		{address: "ana@lima.example", login: "ana@icloud.com", signIn: "ana@icloud.com"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			registry, repo := icloudRegistry(t, tc.signIn, "abcd-efgh-ijkl-mnop")
			created, flow, err := registry.Add(t.Context(), account.AddRequest{
				Email: tc.address, LoginUser: tc.login, Provider: provider.KindIMAP, ICloud: true, Password: "abcd-efgh-ijkl-mnop",
			})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if flow != nil || created.State != account.StateActive || created.AuthKind != "password" {
				t.Errorf("state = %q, auth = %q, flow = %+v: a password account is active once its login works",
					created.State, created.AuthKind, flow)
			}

			stored, err := repo.Get(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Stored as what it is — the provider column knows no iCloud.
			if stored.Provider != provider.KindIMAP {
				t.Errorf("stored kind = %q, want imap", stored.Provider)
			}
			if stored.IMAPHost != "imap.mail.me.com" || stored.IMAPPort != 993 {
				t.Errorf("imap = %s:%d", stored.IMAPHost, stored.IMAPPort)
			}
			if stored.SMTPHost != "smtp.mail.me.com" || stored.SMTPPort != 587 || stored.SMTPTLS != "starttls" {
				t.Errorf("smtp = %s:%d %s", stored.SMTPHost, stored.SMTPPort, stored.SMTPTLS)
			}
			// Apple's SMTP needs the whole address, and one login serves both.
			if stored.LoginUser != tc.signIn {
				t.Errorf("login user = %q, want %q", stored.LoginUser, tc.signIn)
			}
			if stored.Email != tc.address {
				t.Errorf("email = %q, want %q: the sign-in does not replace the address", stored.Email, tc.address)
			}
			// Whatever a generic server gets, until sending shows otherwise.
			if stored.SaveSentCopy != provider.ProfileFor(provider.KindIMAP).SaveSentDefault {
				t.Errorf("save_sent_copy = %v, want the generic default", stored.SaveSentCopy)
			}
			if !stored.IsICloud() || stored.ProviderName() != "icloud" {
				t.Errorf("stored account is not recognised as iCloud: %+v", stored)
			}
		})
	}
}

func TestAnICloudPasswordIsTriedBeforeTheAccountIsStored(t *testing.T) {
	// The Apple Account password is the mistake people make; Apple refuses
	// it for IMAP, and that has to be said while the form is still open.
	registry, repo := icloudRegistry(t, "ana@icloud.com", "abcd-efgh-ijkl-mnop")
	_, _, err := registry.Add(t.Context(), account.AddRequest{
		Email: "ana@icloud.com", Provider: provider.KindIMAP, ICloud: true, Password: "the-apple-account-password",
	})
	if !errors.Is(err, account.ErrLoginRefused) {
		t.Fatalf("a wrong password: %v, want ErrLoginRefused", err)
	}
	if accounts, _ := repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("a refused login left %d accounts behind", len(accounts))
	}
}

func TestACustomDomainIsRefusedAsAnICloudSignIn(t *testing.T) {
	// What Apple does with a custom-domain address as the login, and why the
	// console asks for the iCloud address beside it: nothing is stored.
	registry, repo := icloudRegistry(t, "ana@icloud.com", "abcd-efgh-ijkl-mnop")
	_, _, err := registry.Add(t.Context(), account.AddRequest{
		Email: "ana@lima.example", Provider: provider.KindIMAP, ICloud: true, Password: "abcd-efgh-ijkl-mnop",
	})
	if !errors.Is(err, account.ErrLoginRefused) {
		t.Fatalf("a custom domain signing in as itself: %v, want ErrLoginRefused", err)
	}
	if accounts, _ := repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("a refused login left %d accounts behind", len(accounts))
	}
}

func TestAnICloudRequestWithoutAPasswordStoresNothing(t *testing.T) {
	// Apple offers this server no OAuth: without the password the registry
	// would store the account and send it to a consent flow that cannot exist.
	registry, repo := icloudRegistry(t, "ana@icloud.com", "abcd-efgh-ijkl-mnop")
	if _, _, err := registry.Add(t.Context(), account.AddRequest{
		Email: "ana@icloud.com", Provider: provider.KindIMAP, ICloud: true,
	}); err == nil {
		t.Fatal("an iCloud account was accepted without a password")
	}
	if accounts, _ := repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("a refused request left %d accounts behind", len(accounts))
	}
}

func TestAnICloudRequestCannotNameOtherServers(t *testing.T) {
	// Pointed anywhere but Apple, it would be presented as something it is
	// not. The service refuses these first, with a message; this is the
	// registry keeping the invariant for any other caller.
	registry, repo := icloudRegistry(t, "ana@icloud.com", "abcd-efgh-ijkl-mnop")
	base := account.AddRequest{
		Email: "ana@icloud.com", Provider: provider.KindIMAP, ICloud: true, Password: "abcd-efgh-ijkl-mnop",
	}
	for name, change := range map[string]func(*account.AddRequest){
		"an IMAP host":   func(r *account.AddRequest) { r.IMAPHost = "imap.elsewhere.example" },
		"an IMAP port":   func(r *account.AddRequest) { r.IMAPPort = 143 },
		"an SMTP host":   func(r *account.AddRequest) { r.SMTPHost = "smtp.elsewhere.example" },
		"an SMTP port":   func(r *account.AddRequest) { r.SMTPPort = 465 },
		"a TLS mode":     func(r *account.AddRequest) { r.SMTPTLS = "implicit" },
		"another kind":   func(r *account.AddRequest) { r.Provider = provider.KindGmail },
		"a guessed kind": func(r *account.AddRequest) { r.Email, r.Provider = "ana@gmail.com", "" },
	} {
		req := base
		change(&req)
		if _, _, err := registry.Add(t.Context(), req); err == nil {
			t.Errorf("%s: an iCloud account was stored pointing elsewhere", name)
		}
	}
	if accounts, _ := repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind", len(accounts))
	}
}

func TestOnlyGenericIMAPOnApplesServerIsICloud(t *testing.T) {
	for _, tc := range []struct {
		kind provider.Kind
		host string
		want string
	}{
		{provider.KindIMAP, "imap.mail.me.com", "icloud"},
		{provider.KindIMAP, "IMAP.Mail.Me.Com", "icloud"},
		{provider.KindIMAP, "imap.fastmail.com", "imap"},
		{provider.KindIMAP, "imap.mail.me.com.evil.example", "imap"},
		{provider.KindGmail, "imap.mail.me.com", "gmail"},
		{provider.KindMicrosoft, "outlook.office365.com", "microsoft"},
	} {
		a := account.Account{Provider: tc.kind, IMAPHost: tc.host}
		if got := a.ProviderName(); got != tc.want {
			t.Errorf("%s on %s is presented as %q, want %q", tc.kind, tc.host, got, tc.want)
		}
	}
}

func TestOnlyApplesOwnDomainsAreRecognisedAsICloud(t *testing.T) {
	for address, want := range map[string]bool{
		"ana@icloud.com":              true,
		"ana@me.com":                  true,
		"ana@mac.com":                 true,
		"Ana@iCloud.COM":              true,
		`"ana@gmail.com"@me.com`:      true,
		"ana@gmail.com":               false,
		"ana@notme.com":               false,
		"ana@icloud.com.evil.example": false,
		"ana@mail.icloud.com":         false,
		"icloud.com":                  false,
	} {
		if got := account.IsICloudAddress(address); got != want {
			t.Errorf("IsICloudAddress(%q) = %v, want %v", address, got, want)
		}
	}
}
