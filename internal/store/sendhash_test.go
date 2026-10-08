package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func keyringOf(t *testing.T, active uint8, keys map[uint8]byte) *secrets.Keyring {
	t.Helper()
	material := map[uint8][]byte{}
	for id, b := range keys {
		material[id] = bytes.Repeat([]byte{b}, secrets.KeyLen)
	}
	kr, err := secrets.NewKeyring(active, material)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func storedRoot(t *testing.T, db *store.Store) string {
	t.Helper()
	v, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheSendHashRootIsCreatedOnceAndIsTheSameAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db := storetest.NewAt(t, path, nil)
	if storedRoot(t, db) != "" {
		t.Fatal("a fresh database already holds a send-hash root")
	}

	root, created, err := db.SendHashRoot(t.Context(), keyringOf(t, 1, map[uint8]byte{1: 0xA1}))
	if err != nil || !created {
		t.Fatalf("first SendHashRoot: created %v, %v", created, err)
	}
	if len(root) != store.SendHashRootLen || bytes.Equal(root, make([]byte, store.SendHashRootLen)) {
		t.Fatalf("the root is %x", root)
	}
	kept := storedRoot(t, db)
	envelope, err := base64.StdEncoding.DecodeString(kept)
	if err != nil {
		t.Fatalf("the meta row is not base64: %v", err)
	}
	if bytes.Contains(envelope, root) || envelope[0] != secrets.Version1 {
		t.Fatal("the root is kept in clear, or not in a keyring envelope")
	}

	again, created, err := db.SendHashRoot(t.Context(), keyringOf(t, 1, map[uint8]byte{1: 0xA1}))
	if err != nil || created || !bytes.Equal(again, root) {
		t.Fatalf("second SendHashRoot: created %v, same %v, %v", created, bytes.Equal(again, root), err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A restart: another process, the same key.
	reopened := storetest.NewAt(t, path, nil)
	after, created, err := reopened.SendHashRoot(t.Context(), keyringOf(t, 1, map[uint8]byte{1: 0xA1}))
	if err != nil || created || !bytes.Equal(after, root) {
		t.Fatalf("after a restart: created %v, same %v, %v", created, bytes.Equal(after, root), err)
	}
	if storedRoot(t, reopened) != kept {
		t.Fatal("opening the root rewrote its row")
	}
}

func TestASendHashRootTheKeysCannotOpenIsRefusedAndNeverReplaced(t *testing.T) {
	db := storetest.New(t)
	if _, _, err := db.SendHashRoot(t.Context(), keyringOf(t, 1, map[uint8]byte{1: 0xA1})); err != nil {
		t.Fatal(err)
	}
	kept := storedRoot(t, db)

	for name, kr := range map[string]*secrets.Keyring{
		"another key under the same id": keyringOf(t, 1, map[uint8]byte{1: 0xB2}),
		"the key that sealed it left":   keyringOf(t, 2, map[uint8]byte{2: 0xC3}),
	} {
		_, created, err := db.SendHashRoot(t.Context(), kr)
		if !errors.Is(err, store.ErrSendHashRoot) || created {
			t.Errorf("%s: created %v, %v; want ErrSendHashRoot", name, created, err)
		}
		if storedRoot(t, db) != kept {
			t.Fatalf("%s: the root was replaced", name)
		}
	}

	// A credential's envelope pasted into the row does not pass for a root.
	kr := keyringOf(t, 1, map[uint8]byte{1: 0xA1})
	credential, err := kr.Seal(t.Context(), secrets.Credential("acc_1", "password"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(t.Context(), store.MetaSendHashRoot, base64.StdEncoding.EncodeToString(credential)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SendHashRoot(t.Context(), kr); !errors.Is(err, store.ErrSendHashRoot) {
		t.Fatalf("a credential's envelope opened as the root: %v", err)
	}
}

func TestAReplacedSendHashRootIsANewOneAndOnlyWhenTheOldCannotBeOpened(t *testing.T) {
	db := storetest.New(t)
	lost := keyringOf(t, 1, map[uint8]byte{1: 0xA1})
	old, _, err := db.SendHashRoot(t.Context(), lost)
	if err != nil {
		t.Fatal(err)
	}
	replace := func(kr secrets.Sealer) error {
		return db.Write(t.Context(), func(tx *sql.Tx) error { return store.ReplaceSendHashRootTx(t.Context(), tx, kr, false) })
	}

	if err := replace(lost); !errors.Is(err, store.ErrSendHashRootOpens) {
		t.Fatalf("replacing a root the configured key opens: %v", err)
	}
	current := keyringOf(t, 2, map[uint8]byte{2: 0xC3})
	if err := replace(current); err != nil {
		t.Fatalf("replacing a root no key opens: %v", err)
	}
	fresh, created, err := db.SendHashRoot(t.Context(), current)
	if err != nil || created || bytes.Equal(fresh, old) {
		t.Fatalf("after the replacement: created %v, the old root %v, %v", created, bytes.Equal(fresh, old), err)
	}
}

func TestARootTheSealerCouldNotTryToOpenIsNeitherLostNorReplaced(t *testing.T) {
	// A key service that cannot be reached, throttles, or refuses to decrypt
	// while it still generates keys says nothing about the root, which
	// opens again once it answers: replacing it then would forget every
	// send record of a root that was never lost.
	db := storetest.New(t)
	kms := secretstest.New("kms")
	root, _, err := db.SendHashRoot(t.Context(), kms)
	if err != nil {
		t.Fatal(err)
	}
	kept := storedRoot(t, db)
	replace := func(s secrets.Sealer) error {
		return db.Write(t.Context(), func(tx *sql.Tx) error { return store.ReplaceSendHashRootTx(t.Context(), tx, s, false) })
	}

	for name, failure := range map[string]error{
		"a call that timed out":    fmt.Errorf("kms: decrypt: %w", context.DeadlineExceeded),
		"a throttled key service":  errors.New("kms: ThrottlingException"),
		"a decrypt that is denied": errors.New("kms: AccessDeniedException"),
	} {
		kms.FailOpens(failure)
		_, _, err := db.SendHashRoot(t.Context(), kms)
		if err == nil || errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, failure) {
			t.Errorf("%s: SendHashRoot = %v; want the sealer's error, not a root that does not open", name, err)
		}
		err = replace(kms)
		if err == nil || errors.Is(err, store.ErrSendHashRoot) || !errors.Is(err, failure) {
			t.Errorf("%s: the replacement went on: %v", name, err)
		}
		if storedRoot(t, db) != kept {
			t.Fatalf("%s: a root the sealer could not try was replaced", name)
		}
	}

	kms.FailOpens(nil)
	again, created, err := db.SendHashRoot(t.Context(), kms)
	if err != nil || created || !bytes.Equal(again, root) {
		t.Fatalf("once the key service answers: created %v, same %v, %v", created, bytes.Equal(again, root), err)
	}
}
