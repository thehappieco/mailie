package account_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/kmssealer"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// testKMSKeyARN is a key ARN in AWS's documentation account.
const testKMSKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func TestAPasswordAKeyServiceCannotSealLeavesNoAccountAndCanBeAddedAgain(t *testing.T) {
	// Under a KMS key, sealing is a call that can fail: throttled, a
	// credential refresh that failed, a disabled key, a request that ended.
	// The account must not be left in pending_auth without its password:
	// a retry would be a duplicate, and no route gives an account a
	// password afterwards.
	wrapper := secretstest.NewWrapper("kms")
	sealer, err := kmssealer.New(wrapper, "prod", testKMSKeyARN)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(storetest.New(t), sealer)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		SpoolDir:          t.TempDir(),
		AllowInsecureAuth: true,
		AllowPrivate:      true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@icloud.com", Password: "abcd-efgh-ijkl-mnop"})
	registry.RedirectForTest(appleIMAP, srv.Addr)
	req := account.AddRequest{
		Email: "ana@icloud.com", Provider: provider.KindIMAP, ICloud: true, Password: "abcd-efgh-ijkl-mnop",
	}

	throttled := errors.New("operation error KMS: GenerateDataKey, ThrottlingException: Rate exceeded")
	wrapper.FailGenerates(throttled)
	if _, _, err := registry.Add(t.Context(), req); !errors.Is(err, throttled) {
		t.Fatalf("Add while KMS throttles: %v; want the key service's error", err)
	}
	if accounts, err := repo.List(t.Context()); err != nil || len(accounts) != 0 {
		t.Fatalf("a password that could not be sealed left %d accounts behind (%v)", len(accounts), err)
	}

	wrapper.FailGenerates(nil)
	created, _, err := registry.Add(t.Context(), req)
	if err != nil {
		t.Fatalf("the same add, once KMS answers: %v", err)
	}
	if created.State != account.StateActive {
		t.Errorf("state = %q; want active", created.State)
	}
	if password, err := repo.Password(t.Context(), created.ID); err != nil || password != req.Password {
		t.Fatalf("the stored password: %q, %v", password, err)
	}
	if stored, err := repo.Get(t.Context(), created.ID); err != nil || stored.State != account.StateActive {
		t.Fatalf("the stored account: %q, %v", stored.State, err)
	}
}

func TestACredentialSealedUnderAnotherMAIL_ENVSaysToPutItBack(t *testing.T) {
	// Under a KMS key the envelope names neither the key nor the env: one
	// sealed under dev and read under prod does not open, and the answer
	// is the setting to put back, not a key to add beside it.
	db := storetest.New(t)
	wrapper := secretstest.NewWrapper("kms")
	dev, err := kmssealer.New(wrapper, "dev", testKMSKeyARN)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := kmssealer.New(wrapper, "prod", testKMSKeyARN)
	if err != nil {
		t.Fatal(err)
	}
	a := seed(t, account.NewRepository(db, dev), "ana@mail.example", provider.KindIMAP)
	if err := account.NewRepository(db, dev).SavePassword(t.Context(), a.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}

	_, err = account.NewRepository(db, prod).Password(t.Context(), a.ID)
	if !errors.Is(err, secrets.ErrSealedElsewhere) || !secrets.DoesNotOpen(err) {
		t.Fatalf("a password sealed under dev, read under prod: %v", err)
	}
	for _, says := range []string{"MAIL_ENV", "MAIL_CREDENTIAL_KMS_KEY_ARN", prod.Describe()} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the error does not say %q: %v", says, err)
		}
	}
	if strings.Contains(err.Error(), "among them") {
		t.Errorf("the error asks for another key: %v", err)
	}
	if password, err := account.NewRepository(db, dev).Password(t.Context(), a.ID); err != nil || password != "hunter2" {
		t.Fatalf("with dev put back: %q, %v", password, err)
	}
}
