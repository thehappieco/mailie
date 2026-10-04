package service_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/service"
)

// A unit test cannot reach imap.mail.me.com, and an iCloud request may not
// point anywhere else, so these tests stop before the login is tried or put
// the account in the database directly. The login itself, against a stand-in
// for Apple's server, is internal/account's TestAnICloudAccountGetsApplesServers.

func TestICloudIsOfferedToEveryCallerByPasswordOnly(t *testing.T) {
	for name, publicURL := range map[string]string{
		"a local console":  "http://localhost:5174",
		"a hosted console": "https://console.mailie.example",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureWith(t, fixtureOptions{
				publicURL: publicURL,
				registry: account.RegistryOptions{
					Google:    account.OAuthClient{ClientID: "google-installed"},
					Microsoft: account.OAuthClient{ClientID: "ms-installed", Tenant: "common"},
					GoogleWeb: account.OAuthClient{ClientID: "google-web", ClientSecret: "s"},
				},
			})
			owner := f.person(t, "ana@example.com", auth.RoleOwner)
			member := f.person(t, "bob@example.com", auth.RoleMember)
			callers := map[string]service.Principal{
				"an owner's session":  owner,
				"a member's session":  member,
				"a key acting as bob": {KeyPrefix: "dddddddd", Scope: auth.ScopeAdmin, UserID: member.UserID, UserRole: member.UserRole},
				"an instance key":     admin(),
				"a read key":          reader(),
			}
			for who, p := range callers {
				providers, err := f.svc.Providers(t.Context(), p)
				if err != nil {
					t.Fatalf("%s: %v", who, err)
				}
				ids := make([]string, 0, len(providers))
				for _, got := range providers {
					ids = append(ids, got.ID)
				}
				if !slices.Equal(ids, []string{"gmail", "microsoft", "icloud", "imap"}) {
					t.Fatalf("%s: providers = %v, want gmail, microsoft, icloud, imap", who, ids)
				}
				icloud := providers[2]
				// Flows is an empty list, not null: the console iterates it.
				if icloud.OAuth || !icloud.Password || icloud.Flows == nil || len(icloud.Flows) != 0 {
					t.Errorf("%s: icloud = %+v, want a password and nothing else", who, icloud)
				}
			}
		})
	}
}

func TestAnICloudAccountIsPresentedAsICloud(t *testing.T) {
	f := newFixture(t)
	for _, a := range []account.Account{
		{ID: "acc_00000000000000a1", Email: "ana@icloud.com", IMAPHost: "imap.mail.me.com"},
		// A custom domain on iCloud+, and a host written by hand.
		{ID: "acc_00000000000000a2", Email: "ana@lima.example", IMAPHost: "IMAP.mail.me.com"},
		// Somewhere else: generic, whatever the address says.
		{ID: "acc_00000000000000a3", Email: "bob@me.com", IMAPHost: "imap.bridge.example"},
	} {
		a.Provider, a.AuthKind, a.State = provider.KindIMAP, "password", account.StateActive
		a.IMAPPort, a.SMTPHost, a.SMTPPort, a.SMTPTLS, a.LoginUser = 993, "smtp.mail.me.com", 587, "starttls", a.Email
		if _, err := f.repo.Create(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		"acc_00000000000000a1": "icloud",
		"acc_00000000000000a2": "icloud",
		"acc_00000000000000a3": "imap",
	}

	listed, err := f.svc.ListAccounts(t.Context(), admin())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(want) {
		t.Fatalf("listed %d accounts, want %d", len(listed), len(want))
	}
	for _, a := range listed {
		if a.Provider != want[a.ID] {
			t.Errorf("listed %s (%s) as %q, want %q", a.ID, a.Email, a.Provider, want[a.ID])
		}
		one, err := f.svc.GetAccount(t.Context(), admin(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if one.Provider != want[a.ID] {
			t.Errorf("got %s (%s) as %q, want %q", a.ID, a.Email, one.Provider, want[a.ID])
		}
	}
	// Only the presentation changes: the row is still generic IMAP.
	stored, err := f.repo.Get(t.Context(), "acc_00000000000000a1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provider != provider.KindIMAP {
		t.Errorf("stored kind = %q, want imap", stored.Provider)
	}
}

func TestAnICloudAccountCannotPointElsewhere(t *testing.T) {
	f := newFixture(t)
	session := f.person(t, "ana@example.com", auth.RoleMember)
	base := service.AddAccountRequest{Email: "ana@icloud.com", Provider: "icloud", Password: "abcd-efgh-ijkl-mnop"}
	for name, change := range map[string]func(*service.AddAccountRequest){
		"an IMAP host": func(r *service.AddAccountRequest) { r.IMAPHost = "imap.elsewhere.example" },
		"an IMAP port": func(r *service.AddAccountRequest) { r.IMAPPort = 143 },
		"an SMTP host": func(r *service.AddAccountRequest) { r.SMTPHost = "smtp.elsewhere.example" },
		"an SMTP port": func(r *service.AddAccountRequest) { r.SMTPPort = 465 },
		"a TLS mode":   func(r *service.AddAccountRequest) { r.SMTPTLS = "implicit" },
		// The custom-domain case too: naming iCloud is what makes it iCloud.
		"a custom domain": func(r *service.AddAccountRequest) {
			r.Email, r.IMAPHost, r.SMTPHost = "ana@lima.example", "imap.lima.example", "smtp.lima.example"
		},
	} {
		req := base
		change(&req)
		for who, p := range map[string]service.Principal{"a session": session, "an instance key": admin()} {
			_, err := f.svc.AddAccount(t.Context(), p, req)
			if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "Apple's servers") {
				t.Errorf("%s, %s: %v", name, who, err)
			}
		}
	}

	// Apple has no OAuth for this server: no password, no account.
	noPassword := base
	noPassword.Password = ""
	_, err := f.svc.AddAccount(t.Context(), session, noPassword)
	if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "app-specific password") {
		t.Errorf("no password: %v", err)
	}

	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind", len(accounts))
	}
}

func TestAnICloudSignInIsAWholeAddress(t *testing.T) {
	// A custom domain signs in as the account's iCloud address. A name alone
	// might pass Apple's IMAP, but its SMTP wants the whole address and one
	// login serves both, so the account would break the day it sends.
	f := newFixture(t)
	session := f.person(t, "ana@example.com", auth.RoleMember)
	for _, login := range []string{"ana", "ana@", "Ana <ana@icloud.com>", "<ana@icloud.com>", "ana@icloud.com, bob@icloud.com"} {
		_, err := f.svc.AddAccount(t.Context(), session, service.AddAccountRequest{
			Email: "ana@lima.example", Provider: "icloud", Password: "abcd-efgh-ijkl-mnop", LoginUser: login,
		})
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "whole address") {
			t.Errorf("login_user %q: %v", login, err)
		}
	}
	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind", len(accounts))
	}
}

func TestAnICloudAddressDefaultsToICloud(t *testing.T) {
	f := newFixture(t)
	session := f.person(t, "ana@example.com", auth.RoleMember)
	for _, address := range []string{"ana@icloud.com", "ana@me.com", "ana@mac.com", "Ana@iCloud.com"} {
		// With no provider named, the address makes it iCloud: servers are
		// refused as they are for iCloud, and a missing password is asked
		// for as an app-specific one rather than a generic server's.
		_, err := f.svc.AddAccount(t.Context(), session, service.AddAccountRequest{
			Email: address, Password: "abcd-efgh-ijkl-mnop", IMAPHost: "imap.elsewhere.example", SMTPHost: "smtp.elsewhere.example",
		})
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "Apple's servers") {
			t.Errorf("%s with servers named: %v", address, err)
		}
		_, err = f.svc.AddAccount(t.Context(), session, service.AddAccountRequest{Email: address})
		if service.CodeOf(err) != service.CodeBadRequest || !strings.Contains(service.MessageOf(err), "app-specific password") {
			t.Errorf("%s with no password: %v", address, err)
		}
	}
	if accounts, _ := f.repo.List(t.Context()); len(accounts) != 0 {
		t.Fatalf("refused requests left %d accounts behind", len(accounts))
	}

	// Naming imap is how an Apple address reaches another server, a bridge
	// say — and then it is generic IMAP, presented as such.
	req := f.passwordAccount(t, "ana@icloud.com")
	req.Provider = "imap"
	result, err := f.svc.AddAccount(t.Context(), session, req)
	if err != nil {
		t.Fatalf("an Apple address on another server, provider imap: %v", err)
	}
	if result.Account.Provider != "imap" {
		t.Errorf("provider = %q, want imap: it does not point at Apple", result.Account.Provider)
	}
}

func TestAnUnknownProviderNamesICloudAmongTheChoices(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.AddAccount(t.Context(), admin(), service.AddAccountRequest{
		Email: "ana@example.com", Provider: "aol", Password: "x",
	})
	if msg := service.MessageOf(err); msg != "provider must be gmail, microsoft, icloud or imap" {
		t.Errorf("message = %q", msg)
	}
}
