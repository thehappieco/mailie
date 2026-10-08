package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// testKMSKeyARN is a key ARN in AWS's documentation account.
const testKMSKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// underKMS is cfg with the credentials' KMS key set, in prod, keeping
// whatever keyring keys cfg has.
func underKMS(cfg config.Config) config.Config {
	cfg.Env = config.EnvProd
	cfg.Credentials.KMSKeyARN = testKMSKeyARN
	cfg.Credentials.KMSRegion = "eu-west-1"
	return cfg
}

func TestUnderAKMSKeyTheConfiguredSealerSealsTHCSEALAndStillOpensTheKeyrings(t *testing.T) {
	cfg := localConfig(t)
	keyring, err := NewSealer(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := secrets.Credential("acc_00000000000000a1", "password")
	old, err := keyring.Seal(t.Context(), b, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}

	sealer, err := NewSealerWith(underKMS(cfg), secretstest.NewWrapper("kms"))
	if err != nil {
		t.Fatalf("NewSealerWith: %v", err)
	}
	if d := sealer.Describe(); !strings.Contains(d, testKMSKeyARN) || !strings.Contains(d, "prod") {
		t.Errorf("Describe = %q; want the key's ARN and the env", d)
	}
	if got, err := sealer.Open(t.Context(), b, old); err != nil || string(got) != "hunter2" {
		t.Fatalf("the keyring's envelope under the KMS sealer: %q, %v", got, err)
	}
	if sealer.Current(old) {
		t.Fatal("a keyring envelope reads as current under a KMS key")
	}
	sealed, err := sealer.Seal(t.Context(), b, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := secrets.KeyID(sealed); err != nil || id != 0 || !bytes.HasPrefix(sealed, []byte(secrets.MagicTHCSEAL)) {
		t.Fatalf("sealed under KMS: key id %d, %v, %q", id, err, sealed[:8])
	}

	// Without the hex key the KMS sealer alone opens what it sealed, and
	// the keyring's envelopes are not lost but unknown: give their key back.
	alone := underKMS(cfg)
	alone.Credentials.Keys = nil
	only, err := NewSealerWith(alone, secretstest.NewWrapper("kms"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := only.Open(t.Context(), b, sealed); err != nil || string(got) != "hunter2" {
		t.Fatalf("the KMS sealer alone: %q, %v", got, err)
	}
	if _, err := only.Open(t.Context(), b, old); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("a keyring envelope without its key: %v; want ErrUnknownKey", err)
	}
}

func TestUnderAKMSKeyPreviousCredentialKeysOpenWithoutTheActiveOne(t *testing.T) {
	cfg := localConfig(t)
	key := cfg.Credentials.Keys[1]
	b := secrets.Credential("acc_00000000000000a1", "oauth_token")
	old, err := secrets.NewKeyring(4, map[uint8][]byte{4: key})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := old.Seal(t.Context(), b, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	// MAIL_CREDENTIAL_PREVIOUS_KEYS=4:… and no MAIL_CREDENTIAL_KEY_HEX: the
	// active id names no key.
	previous := underKMS(cfg)
	previous.Credentials = config.Credentials{
		ActiveKeyID: 1, Keys: map[uint8][]byte{4: key}, KMSKeyARN: testKMSKeyARN, KMSRegion: "eu-west-1",
	}
	sealer, err := NewSealerWith(previous, secretstest.NewWrapper("kms"))
	if err != nil {
		t.Fatalf("NewSealerWith: %v", err)
	}
	if got, err := sealer.Open(t.Context(), b, sealed); err != nil || string(got) != "token" {
		t.Fatalf("a previous key's envelope: %q, %v", got, err)
	}
}

func TestTheConfiguredSealerHasAWrapperExactlyWhenAKMSKeyIsConfigured(t *testing.T) {
	cfg := localConfig(t)
	if _, err := NewSealerWith(cfg, secretstest.NewWrapper("kms")); err == nil {
		t.Error("a wrapper the configuration does not name was used")
	}
	if _, err := NewSealerWith(underKMS(cfg), nil); err == nil || !strings.Contains(err.Error(), testKMSKeyARN) {
		t.Errorf("a KMS key without a wrapper: %v", err)
	}
	noEnv := underKMS(cfg)
	noEnv.Env = ""
	if _, err := NewSealerWith(noEnv, secretstest.NewWrapper("kms")); err == nil {
		t.Error("a KMS sealer without an env")
	}
}

func TestADaemonWhoseKMSKeyCannotBeUsedStopsBeforeOpeningTheDatabase(t *testing.T) {
	// DescribeKey cannot answer (here: the start was cancelled, which stops
	// the call before it leaves the process). The daemon stops at once,
	// naming the key and the cause, with nothing opened or changed.
	cfg := underKMS(localConfig(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	logs := &lockedBuffer{}
	started := time.Now()
	err := Run(ctx, cfg, obs.NewLoggerTo(logs, "info", "text"), Options{})
	if err == nil {
		t.Fatal("the daemon started with a KMS key it could not describe")
	}
	if time.Since(started) > 10*time.Second {
		t.Errorf("the refusal took %v", time.Since(started))
	}
	for _, says := range []string{testKMSKeyARN, "MAIL_CREDENTIAL_KMS_KEY_ARN", "describe key"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cause is not kept: %v", err)
	}
	if _, statErr := os.Stat(cfg.DatabasePath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the database was opened before the key was checked: %v", statErr)
	}
	if strings.Contains(err.Error(), "--new-send-hash-root") {
		t.Errorf("a key that could not be described is reported as a lost one: %v", err)
	}
}

func TestAKMSErrorThatIsNotAboutTheRootNeverCountsAsALostKey(t *testing.T) {
	// KMS denies, throttles or is not reached at the start: the daemon does
	// not start, but nothing tells the operator to replace the root, which
	// opens again once KMS answers.
	db := storetest.New(t)
	wrapper := secretstest.NewWrapper("kms")
	sealer, err := NewSealerWith(underKMS(localConfig(t)), wrapper)
	if err != nil {
		t.Fatal(err)
	}
	root, created, err := openSendHashRoot(t.Context(), db, sealer)
	if err != nil || !created {
		t.Fatalf("the first start under KMS: created %v, %v", created, err)
	}
	kept, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil {
		t.Fatal(err)
	}

	for _, failure := range []error{
		errors.New("operation error KMS: Decrypt, AccessDeniedException: not authorized"),
		errors.New("operation error KMS: Decrypt, ThrottlingException: Rate exceeded"),
		errors.New("operation error KMS: Decrypt, https response error: dial tcp: i/o timeout"),
	} {
		wrapper.FailDecrypts(failure)
		_, _, err := openSendHashRoot(t.Context(), db, sealer)
		if !errors.Is(err, failure) || errors.Is(err, store.ErrSendHashRoot) || secrets.DoesNotOpen(err) {
			t.Fatalf("%v: %v", failure, err)
		}
		for _, never := range []string{"--new-send-hash-root", "authorized again", "lost"} {
			if strings.Contains(err.Error(), never) {
				t.Errorf("a KMS that could not answer is reported with %q: %v", never, err)
			}
		}
	}
	if now, err := db.Meta(t.Context(), store.MetaSendHashRoot); err != nil || now != kept {
		t.Fatalf("the root changed while KMS could not answer: %v", err)
	}

	wrapper.FailDecrypts(nil)
	again, created, err := openSendHashRoot(t.Context(), db, sealer)
	if err != nil || created || !bytes.Equal(again, root) {
		t.Fatalf("once KMS answers: created %v, the same root %v, %v", created, bytes.Equal(again, root), err)
	}

	// A root under another KMS key does not open: that one is the operator's
	// to put back, or, with that key lost for good, to replace.
	other, err := NewSealerWith(underKMS(localConfig(t)), secretstest.NewWrapper("another key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := openSendHashRoot(t.Context(), db, other); !errors.Is(err, store.ErrSendHashRoot) ||
		!strings.Contains(err.Error(), "--new-send-hash-root --kms-key-lost") || !strings.Contains(err.Error(), testKMSKeyARN) {
		t.Fatalf("a root another key sealed: %v", err)
	}
}

func TestADaemonWhoseMAIL_ENVChangedIsToldToPutItBackNotToGiveAnotherKey(t *testing.T) {
	// The daemon ran under dev with the KMS key and now starts under prod.
	// Nothing could be added beside the KMS key to open what dev sealed, and
	// no rewrap moves it: the advice is to put MAIL_ENV back, naming what
	// sealed the root, and replacing it needs --kms-key-lost.
	db := storetest.New(t)
	wrapper := secretstest.NewWrapper("kms")
	devCfg := underKMS(localConfig(t))
	devCfg.Env = config.EnvDev
	dev, err := NewSealerWith(devCfg, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := openSendHashRoot(t.Context(), db, dev); err != nil {
		t.Fatal(err)
	}
	prod, err := NewSealerWith(underKMS(localConfig(t)), wrapper)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = openSendHashRoot(t.Context(), db, prod)
	if !errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, secrets.ErrSealedElsewhere) {
		t.Fatalf("a root sealed under dev, started under prod: %v", err)
	}
	for _, says := range []string{
		"MAIL_ENV", "Set both back", "--new-send-hash-root --kms-key-lost",
		"it was sealed with " + dev.Describe(), prod.Describe(),
	} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if strings.Contains(err.Error(), "as well") {
		t.Errorf("the refusal asks for a key to be given beside the KMS key: %v", err)
	}
	if _, _, err := openSendHashRoot(t.Context(), db, dev); err != nil {
		t.Fatalf("with MAIL_ENV put back: %v", err)
	}
}

func TestOnTheWayBackTheKeyringSealsAndTheKMSKeyOnlyOpens(t *testing.T) {
	cfg := underKMS(localConfig(t))
	wrapper := secretstest.NewWrapper("kms")
	kmsSealer, err := NewSealerWith(cfg, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	b := secrets.Credential("acc_00000000000000a1", "password")
	underKMSKey, err := kmsSealer.Seal(t.Context(), b, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}

	back := cfg
	back.Credentials.KMSOpensOnly = true
	sealer, err := NewSealerWith(back, wrapper)
	if err != nil {
		t.Fatalf("NewSealerWith: %v", err)
	}
	sealed, err := sealer.Seal(t.Context(), b, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := secrets.KeyID(sealed); err != nil || id != cfg.Credentials.ActiveKeyID {
		t.Fatalf("sealed on the way back: key id %d, %v; want the keyring's", id, err)
	}
	if got, err := sealer.Open(t.Context(), b, underKMSKey); err != nil || string(got) != "hunter2" {
		t.Fatalf("what the KMS key sealed, on the way back: %q, %v", got, err)
	}
	if sealer.Current(underKMSKey) || !sealer.Current(sealed) {
		t.Fatal("on the way back a KMS envelope reads as current, or a keyring one does not")
	}

	// It needs the key it seals with.
	noKey := back
	noKey.Credentials.Keys = nil
	if _, err := NewSealerWith(noKey, wrapper); err == nil {
		t.Fatal("the way back without the hex key")
	}
}
