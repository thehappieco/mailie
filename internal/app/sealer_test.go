package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func TestTheDaemonCreatesTheSendHashRootAtItsFirstStartAndKeepsIt(t *testing.T) {
	cfg := localConfig(t)
	t.Run("first start", func(t *testing.T) { runDaemon(t, cfg, Options{}) })

	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	first, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil || first == "" {
		t.Fatalf("no send-hash root after the first start: %q, %v", first, err)
	}
	sealer, err := NewSealer(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, created, err := db.SendHashRoot(t.Context(), sealer)
	if err != nil || created {
		t.Fatalf("the root the daemon made does not open with its key: created %v, %v", created, err)
	}

	t.Run("second start", func(t *testing.T) { runDaemon(t, cfg, Options{}) })
	after, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil || after != first {
		t.Fatalf("a restart replaced the send-hash root: %v", err)
	}
	again, _, err := db.SendHashRoot(t.Context(), sealer)
	if err != nil || !bytes.Equal(again, root) {
		t.Fatalf("the root changed across a restart: %v", err)
	}
}

func TestADatabaseWhoseSendHashRootCannotBeOpenedRefusesToStart(t *testing.T) {
	cfg := localConfig(t)
	// The database was last run with another credential key: the one the
	// environment holds now cannot open what it sealed.
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	other, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytes.Repeat([]byte{0x77}, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SendHashRoot(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	kept, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil {
		t.Fatal(err)
	}

	logs := &lockedBuffer{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	refused := Run(ctx, cfg, obs.NewLoggerTo(logs, "info", "text"), Options{})
	if !errors.Is(refused, store.ErrSendHashRoot) || !errors.Is(refused, secrets.ErrDecrypt) {
		t.Fatalf("Run = %v; want the send-hash root refused", refused)
	}
	configured, err := NewSealer(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, says := range []string{configured.Describe(), "rewrap-credentials --new-send-hash-root"} {
		if !strings.Contains(refused.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, refused)
		}
	}
	// Refused before anything started, and with nothing replaced.
	if strings.Contains(logs.String(), "msg=starting") || strings.Contains(logs.String(), "msg=listening") {
		t.Errorf("the daemon started before refusing:\n%s", logs)
	}
	if now, err := db.Meta(t.Context(), store.MetaSendHashRoot); err != nil || now != kept {
		t.Fatalf("the refused start changed the root: %v", err)
	}
}

func TestOnlyARootTheKeysDoNotOpenSendsTheOperatorToReplaceIt(t *testing.T) {
	// A key service that does not answer at the start stops the daemon too,
	// but it is no reason to replace the root: told to, an operator would
	// run --new-send-hash-root while it still does not answer, and forget
	// every send record of a root that was never lost.
	db := storetest.New(t)
	kms := secretstest.New("kms")
	if _, _, err := openSendHashRoot(t.Context(), db, kms); err != nil {
		t.Fatal(err)
	}

	unreachable := errors.New("kms: the key service did not answer")
	kms.FailOpens(unreachable)
	_, _, err := openSendHashRoot(t.Context(), db, kms)
	if !errors.Is(err, unreachable) || errors.Is(err, store.ErrSendHashRoot) {
		t.Fatalf("an unreachable key service: %v", err)
	}
	for _, never := range []string{"--new-send-hash-root", "authorized again", "lost"} {
		if strings.Contains(err.Error(), never) {
			t.Errorf("a key service that did not answer is reported with %q: %v", never, err)
		}
	}

	kms.FailOpens(nil)
	_, _, err = openSendHashRoot(t.Context(), db, secretstest.New("another"))
	if !errors.Is(err, store.ErrSendHashRoot) || !strings.Contains(err.Error(), "--new-send-hash-root") ||
		!strings.Contains(err.Error(), "test sealer another") {
		t.Fatalf("a root no configured key opens: %v", err)
	}
}
