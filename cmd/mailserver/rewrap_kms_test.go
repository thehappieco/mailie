package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/thehappieco/kit/kms"

	"github.com/thehappieco/mailie/internal/app"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
)

// credentialKMSKeyARN is a key ARN in AWS's documentation account.
const credentialKMSKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// withKMSKey is cfg with the credentials' KMS key set, in prod, keeping the
// keyring's keys cfg has.
func withKMSKey(cfg config.Config) config.Config {
	cfg.Env = config.EnvProd
	cfg.Credentials.KMSKeyARN = credentialKMSKeyARN
	cfg.Credentials.KMSRegion = "eu-west-1"
	return cfg
}

// configuredSealer is the sealer app.NewSealer builds for cfg, over wrapper
// in place of AWS KMS (nil for a configuration without a KMS key).
func configuredSealer(t *testing.T, cfg config.Config, wrapper *secretstest.Wrapper) secrets.Sealer {
	t.Helper()
	var w kms.Wrapper
	if wrapper != nil {
		w = wrapper
	}
	sealer, err := app.NewSealerWith(cfg, w)
	if err != nil {
		t.Fatalf("NewSealerWith: %v", err)
	}
	return sealer
}

func TestRewrapMovesEveryCredentialAndTheRootFromTheKeyringToKMSAndIsIdempotent(t *testing.T) {
	cfg := localConfig(t)
	keyring := configuredSealer(t, cfg, nil)
	d := newSealedDatabase(t, cfg, keyring)

	// The move: MAIL_CREDENTIAL_KMS_KEY_ARN set, the hex key kept, the
	// daemon stopped, rewrap-credentials.
	wrapper := secretstest.NewWrapper("kms")
	both := configuredSealer(t, withKMSKey(cfg), wrapper)
	done, err := rewrapCredentials(t.Context(), d.db, both)
	if err != nil || done != (resealed{credentials: 2, root: true}) {
		t.Fatalf("rewrap to KMS: %+v, %v", done, err)
	}
	if wrapper.Generated() != 3 {
		t.Errorf("%d data keys for three envelopes", wrapper.Generated())
	}

	// The hex key can leave the environment: everything opens under KMS
	// alone, every credential's keyid is 0 and every envelope THCSEAL.
	alone := withKMSKey(cfg)
	alone.Credentials.Keys = nil
	kmsOnly := configuredSealer(t, alone, wrapper)
	d.opensWith(t, kmsOnly, 0)
	stored, err := d.db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil {
		t.Fatal(err)
	}
	if root, err := base64.StdEncoding.DecodeString(stored); err != nil || !bytes.HasPrefix(root, []byte(secrets.MagicTHCSEAL)) {
		t.Fatalf("the send-hash root is not a THCSEAL envelope: %v", err)
	}

	// Run again, with or without the hex key, it finds nothing to do.
	before := dumpSealed(t, d.db)
	for _, sealer := range []secrets.Sealer{both, kmsOnly} {
		if done, err := rewrapCredentials(t.Context(), d.db, sealer); err != nil || done != (resealed{}) {
			t.Fatalf("a second rewrap re-sealed %+v (%v); want nothing", done, err)
		}
	}
	if !bytes.Equal(dumpSealed(t, d.db), before) {
		t.Fatal("a rewrap with nothing to do changed some rows")
	}
}

func TestAKMSThatCannotAnswerNeverHasTheRootReplaced(t *testing.T) {
	cfg := withKMSKey(localConfig(t))
	cfg.Credentials.Keys = nil
	wrapper := secretstest.NewWrapper("kms")
	sealer := configuredSealer(t, cfg, wrapper)
	d := newSealedDatabase(t, cfg, sealer)
	before := dumpSealed(t, d.db)

	// KMS refuses to decrypt (a key policy that lost the host, throttling, a
	// region not reached) while the operator, misled, asks for a new root:
	// the root still opens once KMS answers, so nothing replaces it.
	denied := errors.New("operation error KMS: Decrypt, AccessDeniedException: not authorized to perform kms:Decrypt")
	wrapper.FailDecrypts(denied)
	for _, lost := range []bool{false, true} {
		if err := replaceSendHashRoot(t.Context(), d.db, sealer, lost); !errors.Is(err, denied) ||
			errors.Is(err, store.ErrSendHashRoot) || secrets.DoesNotOpen(err) {
			t.Fatalf("--new-send-hash-root (--kms-key-lost %v) while KMS refuses: %v", lost, err)
		}
	}
	// Every row is current, but the rewrap opens the root before it says
	// so, and says that it could not.
	if _, err := rewrapCredentials(t.Context(), d.db, sealer); !errors.Is(err, denied) ||
		errors.Is(err, store.ErrSendHashRoot) || secrets.DoesNotOpen(err) {
		t.Fatalf("a rewrap while KMS refuses: %v", err)
	}
	if !bytes.Equal(dumpSealed(t, d.db), before) {
		t.Fatal("a KMS that could not answer changed some rows")
	}

	wrapper.FailDecrypts(nil)
	d.opensWith(t, sealer, 0)
}

func TestRewrapStopsAtAKMSKeyItCannotUseBeforeTouchingTheDatabase(t *testing.T) {
	cfg := localConfig(t)
	d := newSealedDatabase(t, cfg, configuredSealer(t, cfg, nil))
	before := dumpSealed(t, d.db)

	// DescribeKey cannot answer: the command was cancelled before it ran.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := rewrapCommand(ctx, withKMSKey(cfg), nil)
	if err == nil || !strings.Contains(err.Error(), credentialKMSKeyARN) || !errors.Is(err, context.Canceled) {
		t.Fatalf("rewrap-credentials with a KMS key it cannot describe: %v", err)
	}
	if !bytes.Equal(dumpSealed(t, d.db), before) {
		t.Fatal("the refused rewrap changed some rows")
	}
}

func TestARewrapUnderKMSWithoutTheKeyOfARowChangesNothing(t *testing.T) {
	cfg := localConfig(t)
	d := newSealedDatabase(t, cfg, configuredSealer(t, cfg, nil))
	before := dumpSealed(t, d.db)

	// The hex key was removed before the rewrap ran.
	alone := withKMSKey(cfg)
	alone.Credentials.Keys = nil
	_, err := rewrapCredentials(t.Context(), d.db, configuredSealer(t, alone, secretstest.NewWrapper("kms")))
	if !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("rewrap without the hex key: %v; want ErrUnknownKey", err)
	}
	if !bytes.Equal(dumpSealed(t, d.db), before) {
		t.Fatal("a failed rewrap changed some rows")
	}
}

func TestUnderKMSAChangedMAIL_ENVIsNeitherAlreadySealedNorALostKey(t *testing.T) {
	// The server sealed everything under MAIL_ENV=dev, then moved to prod.
	// Every envelope reads as current and none opens. The rewrap must not
	// say "already sealed", the advice must be to put MAIL_ENV back, and
	// the root must not be replaced on the way: with dev back, it all opens.
	dev := withKMSKey(localConfig(t))
	dev.Env = config.EnvDev
	dev.Credentials.Keys = nil
	wrapper := secretstest.NewWrapper("kms")
	devSealer := configuredSealer(t, dev, wrapper)
	d := newSealedDatabase(t, dev, devSealer)
	before := dumpSealed(t, d.db)

	prod := dev
	prod.Env = config.EnvProd
	prodSealer := configuredSealer(t, prod, wrapper)

	_, err := rewrapCredentials(t.Context(), d.db, prodSealer)
	if !errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, secrets.ErrSealedElsewhere) {
		t.Fatalf("a rewrap under prod of what dev sealed: %v", err)
	}
	advice := app.ExplainSealed(err, prodSealer).Error()
	for _, says := range []string{"MAIL_ENV", "--kms-key-lost", devSealer.Describe(), prodSealer.Describe()} {
		if !strings.Contains(advice, says) {
			t.Errorf("the advice does not say %q: %s", says, advice)
		}
	}
	if strings.Contains(advice, "as well") {
		t.Errorf("the advice asks for a key to be given beside the KMS key: %s", advice)
	}

	err = replaceSendHashRoot(t.Context(), d.db, prodSealer, false)
	if !errors.Is(err, store.ErrSendHashRootMayOpen) {
		t.Fatalf("--new-send-hash-root under prod, without --kms-key-lost: %v", err)
	}
	if !bytes.Equal(dumpSealed(t, d.db), before) {
		t.Fatal("a changed MAIL_ENV changed some rows")
	}
	d.opensWith(t, devSealer, 0)
}

func TestRewrapMovesEverythingBackFromKMSToTheKeyringWhileKMSOnlyOpens(t *testing.T) {
	// The way back, after the daemon has run under KMS: the hex key seals
	// again, the KMS key only opens, a rewrap moves every row back, and the
	// KMS key can then leave the environment.
	cfg := localConfig(t)
	alone := withKMSKey(cfg)
	alone.Credentials.Keys = nil
	wrapper := secretstest.NewWrapper("kms")
	d := newSealedDatabase(t, alone, configuredSealer(t, alone, wrapper))

	back := withKMSKey(cfg)
	back.Credentials.KMSOpensOnly = true
	backSealer := configuredSealer(t, back, wrapper)
	if back.Credentials.Sealer() != config.SealerKeyring || !strings.HasPrefix(backSealer.Describe(), "credential key") {
		t.Fatalf("the way back seals with %s (%s)", backSealer.Describe(), back.Credentials.Sealer())
	}
	generated := wrapper.Generated()
	done, err := rewrapCredentials(t.Context(), d.db, backSealer)
	if err != nil || done != (resealed{credentials: 2, root: true}) {
		t.Fatalf("rewrap back to the keyring: %+v, %v", done, err)
	}
	if wrapper.Generated() != generated {
		t.Error("the way back sealed something under KMS")
	}

	// The KMS key can go: the keyring alone opens everything, under the
	// active key id.
	d.opensWith(t, configuredSealer(t, cfg, nil), int(cfg.Credentials.ActiveKeyID))
	if done, err := rewrapCredentials(t.Context(), d.db, backSealer); err != nil || done != (resealed{}) {
		t.Fatalf("a second rewrap re-sealed %+v (%v); want nothing", done, err)
	}
}

func TestTheUsageAndARestoreSayTheCredentialsMayBeUnderAKMSKey(t *testing.T) {
	// After a move to KMS the hex key is gone: neither the usage nor a
	// restore may send the operator looking for it.
	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	text, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(text)), " ")
	if !strings.Contains(flat, "MAIL_CREDENTIAL_KEY_HEX is required, unless MAIL_CREDENTIAL_KMS_KEY_ARN names an AWS KMS key") {
		t.Errorf("the usage still requires the hex key:\n%s", text)
	}
	for _, says := range []string{"MAIL_CREDENTIAL_KEY_HEX", "KMS key", "MAIL_ENV", "Credentials under AWS KMS"} {
		if !strings.Contains(restoredCredentials, says) {
			t.Errorf("a restore does not say %q: %s", says, restoredCredentials)
		}
	}
}
