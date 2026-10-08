package store_test

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/kmssealer"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// testKMSKeyARN is a key ARN in AWS's documentation account.
const testKMSKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func kmsSealer(t *testing.T, w *secretstest.Wrapper, env string) *kmssealer.Sealer {
	t.Helper()
	s, err := kmssealer.New(w, env, testKMSKeyARN)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestARootSealedUnderAnotherMAIL_ENVSaysWhatSealedItAndIsNeverReplacedUnasked(t *testing.T) {
	// The server ran a while under MAIL_ENV=dev and now runs under prod:
	// the root reads as current (the header names no env) and does not
	// open. Putting dev back opens it; nothing may treat it as lost.
	db := storetest.New(t)
	w := secretstest.NewWrapper("kms")
	dev, prod := kmsSealer(t, w, "dev"), kmsSealer(t, w, "prod")
	root, _, err := db.SendHashRoot(t.Context(), dev)
	if err != nil {
		t.Fatal(err)
	}
	kept := storedRoot(t, db)

	_, _, err = db.SendHashRoot(t.Context(), prod)
	if !errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, secrets.ErrSealedElsewhere) {
		t.Fatalf("a root sealed under dev, opened under prod: %v", err)
	}
	if !strings.Contains(err.Error(), "it was sealed with "+dev.Describe()) {
		t.Errorf("the refusal does not name what sealed the root: %v", err)
	}

	// A rewrap does not call it current: it opens it first.
	err = db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := store.ResealSendHashRootTx(t.Context(), tx, prod)
		return err
	})
	if !errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, secrets.ErrSealedElsewhere) {
		t.Fatalf("a rewrap under prod of a root sealed under dev: %v", err)
	}

	// Nor is it replaced without the operator's word that the key is lost.
	replace := func(lost bool) error {
		return db.Write(t.Context(), func(tx *sql.Tx) error {
			return store.ReplaceSendHashRootTx(t.Context(), tx, prod, lost)
		})
	}
	if err := replace(false); !errors.Is(err, store.ErrSendHashRootMayOpen) ||
		!strings.Contains(err.Error(), dev.Describe()) {
		t.Fatalf("replacing a root sealed under another env, unasked: %v", err)
	}
	if storedRoot(t, db) != kept {
		t.Fatal("a root sealed under another env was replaced")
	}
	if again, _, err := db.SendHashRoot(t.Context(), dev); err != nil || !bytes.Equal(again, root) {
		t.Fatalf("with dev put back: the same root %v, %v", bytes.Equal(again, root), err)
	}

	// Told the key is lost for good, it replaces it, and records the new
	// sealer.
	if err := replace(true); err != nil {
		t.Fatalf("replacing with the key lost: %v", err)
	}
	fresh, created, err := db.SendHashRoot(t.Context(), prod)
	if err != nil || created || bytes.Equal(fresh, root) {
		t.Fatalf("after the replacement: created %v, the old root %v, %v", created, bytes.Equal(fresh, root), err)
	}
	if with, err := db.Meta(t.Context(), store.MetaSendHashRootSealedWith); err != nil || with != prod.Describe() {
		t.Fatalf("the record after the replacement: %q, %v", with, err)
	}
}

func TestARootOfACredentialKeyThatIsNotGivenIsReplacedWithoutTheKMSWord(t *testing.T) {
	// A keyring root under a KMS sealer with no keyring key: the envelope
	// names its key, which is not given, so it is no question of settings.
	db := storetest.New(t)
	if _, _, err := db.SendHashRoot(t.Context(), keyringOf(t, 1, map[uint8]byte{1: 0xA1})); err != nil {
		t.Fatal(err)
	}
	prod := kmsSealer(t, secretstest.NewWrapper("kms"), "prod")
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return store.ReplaceSendHashRootTx(t.Context(), tx, prod, false)
	})
	if err != nil {
		t.Fatalf("replacing a root whose credential key is lost: %v", err)
	}
}

func TestARewrapNeverCallsCurrentARootItCouldNotOpen(t *testing.T) {
	// A key service that does not answer: the rewrap says so, rather than
	// "already sealed".
	db := storetest.New(t)
	w := secretstest.NewWrapper("kms")
	prod := kmsSealer(t, w, "prod")
	if _, _, err := db.SendHashRoot(t.Context(), prod); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("operation error KMS: Decrypt, AccessDeniedException: not authorized")
	w.FailDecrypts(denied)
	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := store.ResealSendHashRootTx(t.Context(), tx, prod)
		return err
	})
	if !errors.Is(err, denied) || errors.Is(err, store.ErrSendHashRoot) {
		t.Fatalf("a rewrap while KMS refuses to decrypt: %v", err)
	}
}
