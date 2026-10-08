package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// sealedDatabase is a database whose credentials (a password, and a token of
// another mailbox) and send-hash root were sealed with sealer.
type sealedDatabase struct {
	db   *store.Store
	root []byte
}

func newSealedDatabase(t *testing.T, cfg config.Config, sealer secrets.Sealer) sealedDatabase {
	t.Helper()
	db := storetest.NewAt(t, cfg.DatabasePath(), nil)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	repo := account.NewRepository(db, sealer)
	for _, a := range []account.Account{
		{ID: "acc_00000000000000a1", Email: "ana@mail.example", AuthKind: "password", LoginUser: "ana"},
		{ID: "acc_00000000000000a2", Email: "ana@gmail.example", AuthKind: "oauth2", LoginUser: "ana@gmail.example"},
	} {
		a.Provider, a.IMAPHost, a.IMAPPort = provider.KindIMAP, "imap.mail.example", 993
		a.SMTPHost, a.SMTPPort, a.SMTPTLS = "smtp.mail.example", 465, "implicit"
		if _, err := repo.Create(t.Context(), a, ana.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SavePassword(t.Context(), "acc_00000000000000a1", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveToken(t.Context(), "acc_00000000000000a2", &oauth2.Token{RefreshToken: "1//0eXaMpLe"}); err != nil {
		t.Fatal(err)
	}
	root, created, err := db.SendHashRoot(t.Context(), sealer)
	if err != nil || !created {
		t.Fatalf("SendHashRoot: created %v, %v", created, err)
	}
	return sealedDatabase{db: db, root: root}
}

// opensWith checks that only sealer is needed to read everything, and that
// every credential's keyid column says what sealed it.
func (d sealedDatabase) opensWith(t *testing.T, sealer secrets.Sealer, wantKeyID int) {
	t.Helper()
	repo := account.NewRepository(d.db, sealer)
	if password, err := repo.Password(t.Context(), "acc_00000000000000a1"); err != nil || password != "hunter2" {
		t.Fatalf("the password: %q, %v", password, err)
	}
	if token, err := repo.Token(t.Context(), "acc_00000000000000a2"); err != nil || token.RefreshToken != "1//0eXaMpLe" {
		t.Fatalf("the token: %+v, %v", token, err)
	}
	if root, created, err := d.db.SendHashRoot(t.Context(), sealer); err != nil || created || !bytes.Equal(root, d.root) {
		t.Fatalf("the send-hash root: created %v, the same %v, %v", created, bytes.Equal(root, d.root), err)
	}
	rows, err := d.db.Reader().QueryContext(t.Context(), `SELECT keyid, ciphertext FROM credentials`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var keyID int
		var envelope []byte
		if err := rows.Scan(&keyID, &envelope); err != nil {
			t.Fatal(err)
		}
		if keyID != wantKeyID || !sealer.Current(envelope) {
			t.Errorf("a credential under keyid %d (want %d), current %v", keyID, wantKeyID, sealer.Current(envelope))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func keyOf(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeyLen) }

func TestRewrapMovesCredentialsAndTheRootFromAPreviousKeyToTheActiveOneAndIsIdempotent(t *testing.T) {
	cfg := localConfig(t)
	cfg.Credentials = config.Credentials{ActiveKeyID: 1, Keys: map[uint8][]byte{1: keyOf(0xA1)}}
	previous, err := secrets.NewKeyring(1, map[uint8][]byte{1: keyOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	d := newSealedDatabase(t, cfg, previous)

	// The rotation: a new active key, the old one kept as a previous key
	// while the command runs.
	cfg.Credentials = config.Credentials{ActiveKeyID: 2, Keys: map[uint8][]byte{1: keyOf(0xA1), 2: keyOf(0xD4)}}
	if err := rewrapCommand(t.Context(), cfg, nil); err != nil {
		t.Fatalf("rewrap-credentials: %v", err)
	}

	// The old key can leave the environment: everything opens without it.
	active, err := secrets.NewKeyring(2, map[uint8][]byte{2: keyOf(0xD4)})
	if err != nil {
		t.Fatal(err)
	}
	d.opensWith(t, active, 2)

	both, err := secrets.NewKeyring(2, map[uint8][]byte{1: keyOf(0xA1), 2: keyOf(0xD4)})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := rewrapCredentials(t.Context(), d.db, both); err != nil || done != (resealed{}) {
		t.Fatalf("a second rewrap re-sealed %+v (%v); want nothing", done, err)
	}
	d.opensWith(t, active, 2)
}

func TestRewrapMovesEveryRowToAnotherKindOfSealerAndBack(t *testing.T) {
	cfg := localConfig(t)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: keyOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	d := newSealedDatabase(t, cfg, keyring)
	kms := secretstest.New("kms")

	// The other kind becomes the active one; the keyring still opens what
	// it sealed, until the rewrap has moved every row.
	toKMS := secrets.NewComposite(kms, keyring)
	done, err := rewrapCredentials(t.Context(), d.db, toKMS)
	if err != nil || done != (resealed{credentials: 2, root: true}) {
		t.Fatalf("rewrap to the other kind: %+v, %v", done, err)
	}
	d.opensWith(t, kms, 0)
	if done, err := rewrapCredentials(t.Context(), d.db, toKMS); err != nil || done != (resealed{}) {
		t.Fatalf("a second rewrap re-sealed %+v (%v); want nothing", done, err)
	}

	// And back to the keyring alone, under a new key.
	rotated, err := secrets.NewKeyring(2, map[uint8][]byte{2: keyOf(0xD4)})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := rewrapCredentials(t.Context(), d.db, secrets.NewComposite(rotated, kms)); err != nil ||
		done != (resealed{credentials: 2, root: true}) {
		t.Fatalf("rewrap back to the keyring: %+v, %v", done, err)
	}
	d.opensWith(t, rotated, 2)
}

func TestARewrapThatCannotOpenARowChangesNothing(t *testing.T) {
	cfg := localConfig(t)
	lost, err := secrets.NewKeyring(1, map[uint8][]byte{1: keyOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	d := newSealedDatabase(t, cfg, lost)
	before := dumpSealed(t, d.db)

	// The previous key was dropped from the environment before the rewrap.
	active, err := secrets.NewKeyring(2, map[uint8][]byte{2: keyOf(0xD4)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewrapCredentials(t.Context(), d.db, active); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("rewrap without the previous key: %v; want ErrUnknownKey", err)
	}
	if after := dumpSealed(t, d.db); !bytes.Equal(after, before) {
		t.Fatal("a failed rewrap changed some rows")
	}
}

func TestANewSendHashRootReplacesOnlyARootNoConfiguredKeyOpens(t *testing.T) {
	cfg := localConfig(t)
	cfg.Credentials = config.Credentials{ActiveKeyID: 1, Keys: map[uint8][]byte{1: keyOf(0xA1)}}
	lost, err := secrets.NewKeyring(1, map[uint8][]byte{1: keyOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	d := newSealedDatabase(t, cfg, lost)
	credentialsBefore := dumpCredentials(t, d.db)

	// While the key opens it, there is nothing to replace.
	if err := rewrapCommand(t.Context(), cfg, []string{"--new-send-hash-root"}); !errors.Is(err, store.ErrSendHashRootOpens) {
		t.Fatalf("replacing a root the configured key opens: %v", err)
	}

	// The key is lost for good: another one takes its place.
	cfg.Credentials = config.Credentials{ActiveKeyID: 2, Keys: map[uint8][]byte{2: keyOf(0xD4)}}
	if err := rewrapCommand(t.Context(), cfg, []string{"--new-send-hash-root"}); err != nil {
		t.Fatalf("rewrap-credentials --new-send-hash-root: %v", err)
	}
	sealer, err := secrets.NewKeyring(2, map[uint8][]byte{2: keyOf(0xD4)})
	if err != nil {
		t.Fatal(err)
	}
	root, created, err := d.db.SendHashRoot(context.Background(), sealer)
	if err != nil || created || bytes.Equal(root, d.root) {
		t.Fatalf("after the replacement: created %v, the old root %v, %v", created, bytes.Equal(root, d.root), err)
	}
	// The credentials stay as they were, for their mailboxes to be
	// authorized again.
	if !bytes.Equal(dumpCredentials(t, d.db), credentialsBefore) {
		t.Fatal("replacing the root touched the credentials")
	}
}

// dumpCredentials is every credentials row, in order, as bytes.
func dumpCredentials(t *testing.T, db *store.Store) []byte {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT account_id, field, keyid, ciphertext FROM credentials ORDER BY account_id, field`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out bytes.Buffer
	for rows.Next() {
		var id, field string
		var keyID int
		var envelope []byte
		if err := rows.Scan(&id, &field, &keyID, &envelope); err != nil {
			t.Fatal(err)
		}
		out.WriteString(id + "/" + field + "/" + strconv.Itoa(keyID) + "/" + base64.StdEncoding.EncodeToString(envelope) + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// dumpSealed is every credential and the send-hash root's row, as bytes.
func dumpSealed(t *testing.T, db *store.Store) []byte {
	t.Helper()
	root, err := db.Meta(t.Context(), store.MetaSendHashRoot)
	if err != nil {
		t.Fatal(err)
	}
	return append(dumpCredentials(t, db), root...)
}
