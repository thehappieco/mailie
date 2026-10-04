package secrets_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/secrets"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeyLen) }

func newKeyring(t *testing.T, active uint8, keys map[uint8][]byte) *secrets.Keyring {
	t.Helper()
	kr, err := secrets.NewKeyring(active, keys)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestASealedCredentialRoundTrips(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	want := []byte(`{"refresh_token":"1//0eXaMpLe","expiry":"2026-09-22T10:00:00Z"}`)

	sealed, err := kr.Seal("acc_1", "oauth_token", want)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("0eXaMpLe")) {
		t.Fatal("the plaintext is visible in the envelope")
	}
	got, err := kr.Open("acc_1", "oauth_token", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip changed the value: %q", got)
	}
}

func TestTwoSealsOfTheSameValueDiffer(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	a, _ := kr.Seal("acc_1", "password", []byte("hunter2"))
	b, _ := kr.Seal("acc_1", "password", []byte("hunter2"))
	// Equal envelopes would mean a repeated nonce, which is the one thing
	// GCM cannot survive.
	if bytes.Equal(a, b) {
		t.Fatal("two seals produced identical envelopes: the nonce is not random")
	}
}

func TestAnEnvelopeMovedToAnotherAccountDoesNotOpen(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed, _ := kr.Seal("acc_1", "oauth_token", []byte("token"))

	// Someone with write access to the database pastes account 1's token into
	// account 2's row, hoping the daemon will authenticate as account 1.
	if _, err := kr.Open("acc_2", "oauth_token", sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestAnEnvelopeMovedToAnotherFieldDoesNotOpen(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed, _ := kr.Seal("acc_1", "oauth_token", []byte("token"))

	if _, err := kr.Open("acc_1", "password", sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestFlippingAnyBitBreaksDecryption(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed, _ := kr.Seal("acc_1", "oauth_token", []byte("a token worth stealing"))

	// Every byte, including the header: the version and key id are inside the
	// additional data precisely so an edit to them is a failure rather than a
	// behaviour change.
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if _, err := kr.Open("acc_1", "oauth_token", tampered); err == nil {
			t.Fatalf("byte %d could be flipped without detection", i)
		}
	}
}

func TestAWrongKeyIsNotTakenForTamperedData(t *testing.T) {
	sealer := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	opener := newKeyring(t, 1, map[uint8][]byte{1: key(0xB2)})
	sealed, _ := sealer.Seal("acc_1", "password", []byte("hunter2"))

	if _, err := opener.Open("acc_1", "password", sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestAnEnvelopeSealedUnderARetiredKeyReportsItClearly(t *testing.T) {
	old := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed, _ := old.Seal("acc_1", "password", []byte("hunter2"))

	// The operator rotated the key and dropped the old one from the
	// environment before rewrapping the rows.
	current := newKeyring(t, 2, map[uint8][]byte{2: key(0xC3)})
	_, err := current.Open("acc_1", "password", sealed)
	if !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey so the operator knows to restore the old key, got %v", err)
	}
}

func TestRewrapMovesARowToTheActiveKeyAndKeepsThePlaintext(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	want := []byte("1//0eXaMpLe-refresh-token")
	sealed, _ := kr.Seal("acc_1", "oauth_token", want)

	// A rewrap runs with both keys configured; that is the whole point of
	// MAIL_CREDENTIAL_PREVIOUS_KEYS.
	rotated := newKeyring(t, 2, map[uint8][]byte{1: key(0xA1), 2: key(0xD4)})
	if !rotated.NeedsRewrap(sealed) {
		t.Fatal("a row under key 1 should need rewrapping when key 2 is active")
	}

	rewrapped, err := rotated.Rewrap("acc_1", "oauth_token", sealed)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if rotated.NeedsRewrap(rewrapped) {
		t.Fatal("the rewrapped row still reports the old key")
	}
	id, err := secrets.KeyID(rewrapped)
	if err != nil || id != 2 {
		t.Fatalf("KeyID = %d (%v), want 2", id, err)
	}

	// The old key can now be retired: the value is still readable.
	after := newKeyring(t, 2, map[uint8][]byte{2: key(0xD4)})
	got, err := after.Open("acc_1", "oauth_token", rewrapped)
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rewrap changed the value: %q", got)
	}
}

func TestATruncatedEnvelopeIsRejectedBeforeDecryption(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed, _ := kr.Seal("acc_1", "password", []byte("hunter2"))

	for _, n := range []int{0, 1, secrets.Overhead - 1} {
		if _, err := kr.Open("acc_1", "password", sealed[:n]); !errors.Is(err, secrets.ErrMalformed) {
			t.Errorf("length %d: want ErrMalformed, got %v", n, err)
		}
	}
}

func TestAKeyringRefusesAKeyOfTheWrongSize(t *testing.T) {
	_, err := secrets.NewKeyring(1, map[uint8][]byte{1: []byte("short")})
	if err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Fatalf("want a size complaint, got %v", err)
	}
}

func TestAKeyringRefusesAnActiveKeyItDoesNotHave(t *testing.T) {
	if _, err := secrets.NewKeyring(3, map[uint8][]byte{1: key(0xA1)}); err == nil {
		t.Fatal("want an error when the active key is missing from the keyring")
	}
}

func TestADerivedKeyBelongsToOnePurposeAndIsNeverTheKeyItself(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0x11)})
	a, err := kr.DeriveKey("send-compose-hash")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := kr.DeriveKey("send-compose-hash")
	other, _ := kr.DeriveKey("something-else")
	if len(a) != secrets.KeyLen || !bytes.Equal(a, again) {
		t.Fatalf("the same purpose gave %x and %x", a, again)
	}
	if bytes.Equal(a, other) || bytes.Equal(a, key(0x11)) {
		t.Fatal("a derived key equals another purpose's, or the credential key")
	}
	// It follows the active key.
	rotated := newKeyring(t, 2, map[uint8][]byte{1: key(0x11), 2: key(0x22)})
	if b, _ := rotated.DeriveKey("send-compose-hash"); bytes.Equal(a, b) {
		t.Fatal("two active keys derived the same key")
	}
	if _, err := kr.DeriveKey(""); err == nil {
		t.Fatal("a key derived for no purpose")
	}
}

// TestTheEnvelopeLabelsNeverChange opens an envelope and derives a key that the
// code before the module rename (b3a8d02) produced from a fixed key. Every
// stored credential depends on these labels: the rename's search and replace
// once turned them into the new module path, and production could no longer
// decrypt any mailbox.
func TestTheEnvelopeLabelsNeverChange(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0x5E)})

	envelope, err := hex.DecodeString("0101ba3689e795a57d406c66632be07d6d63a7da6579720212293ca5aa086930bdf8b86371bafedbf52bc8479fa358253973627d4582d73c9d530bfcfe8faaa4a4b5")
	if err != nil {
		t.Fatal(err)
	}
	got, err := kr.Open("acc_fixture", "password", envelope)
	if err != nil {
		t.Fatalf("an envelope sealed before the rename no longer opens: %v", err)
	}
	if want := "sealed before the module was renamed"; string(got) != want {
		t.Fatalf("Open = %q, want %q", got, want)
	}

	derived, err := kr.DeriveKey("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(derived), "d544f04f4ddcf0a0c217e64253af5c3cf8bc28d2c60d196deb947f3eae79623f"; got != want {
		t.Fatalf("DeriveKey(fixture) = %s, want %s: a derived key changed", got, want)
	}
}
