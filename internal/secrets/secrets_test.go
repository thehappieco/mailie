package secrets_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
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

// cred is the binding of one field of one account.
func cred(accountID, field string) secrets.Binding { return secrets.Credential(accountID, field) }

func seal(t *testing.T, s secrets.Sealer, b secrets.Binding, plaintext string) []byte {
	t.Helper()
	sealed, err := s.Seal(t.Context(), b, []byte(plaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

func TestASealedCredentialRoundTrips(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	want := []byte(`{"refresh_token":"1//0eXaMpLe","expiry":"2026-09-22T10:00:00Z"}`)

	sealed, err := kr.Seal(t.Context(), cred("acc_1", "oauth_token"), want)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("0eXaMpLe")) {
		t.Fatal("the plaintext is visible in the envelope")
	}
	got, err := kr.Open(t.Context(), cred("acc_1", "oauth_token"), sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip changed the value: %q", got)
	}
}

func TestTwoSealsOfTheSameValueDiffer(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	a := seal(t, kr, cred("acc_1", "password"), "hunter2")
	b := seal(t, kr, cred("acc_1", "password"), "hunter2")
	// Equal envelopes would mean a repeated nonce, which is the one thing
	// GCM cannot survive.
	if bytes.Equal(a, b) {
		t.Fatal("two seals produced identical envelopes: the nonce is not random")
	}
}

func TestAnEnvelopeMovedToAnotherAccountDoesNotOpen(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, kr, cred("acc_1", "oauth_token"), "token")

	// Someone with write access to the database pastes account 1's token into
	// account 2's row, hoping the daemon will authenticate as account 1.
	if _, err := kr.Open(t.Context(), cred("acc_2", "oauth_token"), sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestAnEnvelopeMovedToAnotherFieldDoesNotOpen(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, kr, cred("acc_1", "oauth_token"), "token")

	if _, err := kr.Open(t.Context(), cred("acc_1", "password"), sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestACredentialNeverOpensAsTheSendHashRootNorTheRootAsACredential(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	root := secrets.Binding{Purpose: secrets.PurposeSendHashRoot, Ref: "meta/send_hash_root"}
	credential := seal(t, kr, cred("acc_1", "password"), "hunter2")
	sealedRoot := seal(t, kr, root, "a root of thirty-two bytes, not.")

	if _, err := kr.Open(t.Context(), root, credential); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("a credential opened as the root: %v", err)
	}
	if _, err := kr.Open(t.Context(), cred("acc_1", "password"), sealedRoot); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("the root opened as a credential: %v", err)
	}
}

func TestABindingAKeyServiceWouldRefuseSealsAndOpensNothing(t *testing.T) {
	// Every sealer holds a binding to what a key service's encryption
	// context takes, the keyring too: one that would be refused only in
	// production is refused in every test first.
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	good := seal(t, kr, cred("acc_1", "password"), "hunter2")
	for name, b := range map[string]secrets.Binding{
		"no purpose":                  {Ref: "acc_1"},
		"no ref":                      {Purpose: secrets.PurposePassword},
		"nothing":                     {},
		"a field for its purpose":     {Purpose: "password", Ref: "acc_1"},
		"the other field":             {Purpose: "oauth_token", Ref: "acc_1"},
		"a purpose nobody allows":     {Purpose: "credential/api-key", Ref: "acc_1"},
		"a field the table never has": secrets.Credential("acc_1", "api_key"),
		"a capital in the ref":        {Purpose: secrets.PurposePassword, Ref: "acc_A"},
		"a space in the ref":          {Purpose: secrets.PurposePassword, Ref: "acc 1"},
		"a ref too long":              {Purpose: secrets.PurposePassword, Ref: strings.Repeat("a", secrets.MaxBindingLen+1)},
	} {
		if err := b.Validate(); !errors.Is(err, secrets.ErrBinding) {
			t.Errorf("%s: Validate = %v, want ErrBinding", name, err)
		}
		for _, s := range []secrets.Sealer{kr, secretstest.New("kms")} {
			if _, err := s.Seal(t.Context(), b, []byte("hunter2")); !errors.Is(err, secrets.ErrBinding) {
				t.Errorf("%s: %s sealed: %v", name, s.Describe(), err)
			}
		}
		_, err := kr.Open(t.Context(), b, good)
		if !errors.Is(err, secrets.ErrBinding) {
			t.Errorf("%s: the keyring opened: %v", name, err)
		}
		// A binding refused is the caller's mistake, never an envelope that
		// does not open.
		if secrets.DoesNotOpen(err) {
			t.Errorf("%s: a refused binding reads as an envelope that does not open: %v", name, err)
		}
	}
	if err := (secrets.Binding{Purpose: secrets.PurposePassword, Ref: strings.Repeat("a", secrets.MaxBindingLen)}).Validate(); err != nil {
		t.Errorf("a ref of the longest length: %v", err)
	}
}

func TestEveryPurposeIsOneAKeyServiceTakes(t *testing.T) {
	// These names are what a key service's policy allows: a purpose added
	// here changes that policy before the code that uses it ships, and a
	// name changed here stops every envelope sealed under the old one.
	want := []string{"credential/oauth-token", "credential/password", "send/hash-root"}
	got := secrets.Purposes()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Purposes = %q, want %q", got, want)
	}
	for _, purpose := range got {
		if len(purpose) > secrets.MaxBindingLen || strings.Trim(purpose, "abcdefghijklmnopqrstuvwxyz0123456789._:/-") != "" {
			t.Errorf("%q is not a purpose an encryption context takes", purpose)
		}
		if err := (secrets.Binding{Purpose: purpose, Ref: "acc_0123456789abcdef"}).Validate(); err != nil {
			t.Errorf("%q: %v", purpose, err)
		}
	}
	// A credential's purpose is named by its field, and only the two fields
	// the credentials table holds have one.
	for field, purpose := range map[string]string{
		"oauth_token": secrets.PurposeOAuthToken,
		"password":    secrets.PurposePassword,
	} {
		if b := secrets.Credential("acc_0123456789abcdef", field); b.Purpose != purpose || b.Validate() != nil {
			t.Errorf("Credential(%q) = %+v, want the purpose %q", field, b, purpose)
		}
	}
}

func TestOnlyAnEnvelopeThatDoesNotOpenReadsAsOne(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, kr, cred("acc_1", "password"), "hunter2")
	stranger := seal(t, newKeyring(t, 7, map[uint8][]byte{7: key(0xE7)}), cred("acc_1", "password"), "x")
	for name, envelope := range map[string][]byte{
		"another binding": nil, "another key": stranger, "cut short": sealed[:secrets.Overhead-1],
	} {
		b := cred("acc_1", "password")
		if envelope == nil {
			envelope, b = sealed, cred("acc_2", "password")
		}
		if _, err := kr.Open(t.Context(), b, envelope); !secrets.DoesNotOpen(err) {
			t.Errorf("%s: %v does not read as an envelope that does not open", name, err)
		}
	}

	// A sealer that could not try says nothing about the envelope.
	kms := secretstest.New("kms")
	other := seal(t, kms, cred("acc_1", "password"), "hunter2")
	unreachable := errors.New("kms: the key service did not answer")
	kms.FailOpens(fmt.Errorf("decrypt: %w", unreachable))
	for _, s := range []secrets.Sealer{kms, secrets.NewComposite(kr, kms)} {
		_, err := s.Open(t.Context(), cred("acc_1", "password"), other)
		if !errors.Is(err, unreachable) || secrets.DoesNotOpen(err) {
			t.Errorf("%s: an unreachable key service reads as %v", s.Describe(), err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	kms.FailOpens(nil)
	if _, err := kms.Open(ctx, cred("acc_1", "password"), other); !errors.Is(err, context.DeadlineExceeded) || secrets.DoesNotOpen(err) {
		t.Errorf("an ended context reads as %v", err)
	}
	if _, err := kms.Open(t.Context(), cred("acc_1", "password"), other); err != nil {
		t.Fatalf("the sealer does not open again: %v", err)
	}
}

func TestFlippingAnyBitBreaksDecryption(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, kr, cred("acc_1", "oauth_token"), "a token worth stealing")

	// Every byte, including the header: the version and key id are inside the
	// additional data precisely so an edit to them is a failure rather than a
	// behaviour change.
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if _, err := kr.Open(t.Context(), cred("acc_1", "oauth_token"), tampered); err == nil {
			t.Fatalf("byte %d could be flipped without detection", i)
		}
	}
}

func TestAWrongKeyIsNotTakenForTamperedData(t *testing.T) {
	sealer := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	opener := newKeyring(t, 1, map[uint8][]byte{1: key(0xB2)})
	sealed := seal(t, sealer, cred("acc_1", "password"), "hunter2")

	if _, err := opener.Open(t.Context(), cred("acc_1", "password"), sealed); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestAnEnvelopeSealedUnderARetiredKeyReportsItClearly(t *testing.T) {
	old := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, old, cred("acc_1", "password"), "hunter2")

	// The operator rotated the key and dropped the old one from the
	// environment before rewrapping the rows.
	current := newKeyring(t, 2, map[uint8][]byte{2: key(0xC3)})
	_, err := current.Open(t.Context(), cred("acc_1", "password"), sealed)
	if !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey so the operator knows to restore the old key, got %v", err)
	}
}

func TestResealMovesARowToTheActiveKeyAndKeepsThePlaintext(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	want := []byte("1//0eXaMpLe-refresh-token")
	sealed, _ := kr.Seal(t.Context(), cred("acc_1", "oauth_token"), want)

	// A rewrap runs with both keys configured; that is the whole point of
	// MAIL_CREDENTIAL_PREVIOUS_KEYS.
	rotated := newKeyring(t, 2, map[uint8][]byte{1: key(0xA1), 2: key(0xD4)})
	if rotated.Current(sealed) || !rotated.Knows(sealed) {
		t.Fatal("a row under key 1 should be known but not current when key 2 is active")
	}

	rewrapped, err := secrets.Reseal(t.Context(), rotated, cred("acc_1", "oauth_token"), sealed)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if !rotated.Current(rewrapped) {
		t.Fatal("the rewrapped row still reports the old key")
	}
	id, err := secrets.KeyID(rewrapped)
	if err != nil || id != 2 {
		t.Fatalf("KeyID = %d (%v), want 2", id, err)
	}

	// The old key can now be retired: the value is still readable.
	after := newKeyring(t, 2, map[uint8][]byte{2: key(0xD4)})
	got, err := after.Open(t.Context(), cred("acc_1", "oauth_token"), rewrapped)
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rewrap changed the value: %q", got)
	}
}

func TestATruncatedEnvelopeIsRejectedBeforeDecryption(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	sealed := seal(t, kr, cred("acc_1", "password"), "hunter2")

	for _, n := range []int{0, 1, secrets.Overhead - 1} {
		if _, err := kr.Open(t.Context(), cred("acc_1", "password"), sealed[:n]); !errors.Is(err, secrets.ErrMalformed) {
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

func TestKeyIDZeroIsNeverAKeyringKeyAndNamesATHCSEALEnvelope(t *testing.T) {
	// The credentials table records 0 for an envelope no keyring key sealed,
	// which only works while no keyring key can be 0.
	if _, err := secrets.NewKeyring(0, map[uint8][]byte{0: key(0xA1)}); err == nil {
		t.Fatal("a keyring took key id 0 as its active key")
	}
	if _, err := secrets.NewKeyring(1, map[uint8][]byte{0: key(0xA0), 1: key(0xA1)}); err == nil {
		t.Fatal("a keyring took key id 0 as a previous key")
	}
	other := seal(t, secretstest.New("kms"), cred("acc_1", "password"), "hunter2")
	if !bytes.HasPrefix(other, []byte(secrets.MagicTHCSEAL)) {
		t.Fatalf("the test sealer's envelope starts %q", other[:8])
	}
	if id, err := secrets.KeyID(other); err != nil || id != 0 {
		t.Fatalf("KeyID of a THCSEAL envelope = %d (%v), want 0", id, err)
	}
	if _, err := secrets.KeyID([]byte{0x02, 0x01}); !errors.Is(err, secrets.ErrMalformed) {
		t.Fatalf("KeyID of an unknown kind: %v, want ErrMalformed", err)
	}

	// A keyring neither knows a THCSEAL envelope nor takes it for a v1 one.
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	if kr.Knows(other) || kr.Current(other) {
		t.Fatal("the keyring claims a THCSEAL envelope")
	}
	if _, err := kr.Open(t.Context(), cred("acc_1", "password"), other); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("the keyring opening a THCSEAL envelope: %v, want ErrUnknownKey", err)
	}
}

func TestACompositeOpensBothKindsAndSealsWithTheActiveOne(t *testing.T) {
	keyring := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	kms := secretstest.New("kms")
	fromKeyring := seal(t, keyring, cred("acc_1", "password"), "sealed by the keyring")
	fromKMS := seal(t, kms, cred("acc_1", "oauth_token"), "sealed by the other kind")

	for name, tc := range map[string]struct {
		active, previous secrets.Sealer
		current, old     []byte
	}{
		"the other kind seals, the keyring still opens": {kms, keyring, fromKMS, fromKeyring},
		"the keyring seals, the other kind still opens": {keyring, kms, fromKeyring, fromKMS},
	} {
		t.Run(name, func(t *testing.T) {
			c := secrets.NewComposite(tc.active, tc.previous)
			for b, envelope := range map[secrets.Binding][]byte{
				cred("acc_1", "password"): fromKeyring, cred("acc_1", "oauth_token"): fromKMS,
			} {
				if !c.Knows(envelope) {
					t.Fatalf("the composite does not know %s's envelope", b.Purpose)
				}
				if _, err := c.Open(t.Context(), b, envelope); err != nil {
					t.Fatalf("the composite cannot open %s's envelope: %v", b.Purpose, err)
				}
				// The binding holds whichever sealer opens it.
				moved := secrets.Binding{Purpose: b.Purpose, Ref: "acc_2"}
				if _, err := c.Open(t.Context(), moved, envelope); !errors.Is(err, secrets.ErrDecrypt) {
					t.Fatalf("%s's envelope opened for another account: %v", b.Purpose, err)
				}
			}
			if !c.Current(tc.current) || c.Current(tc.old) {
				t.Fatal("the composite's current is not its active sealer's")
			}
			sealed := seal(t, c, cred("acc_1", "password"), "new")
			if !tc.active.Current(sealed) || tc.previous.Knows(sealed) {
				t.Fatal("the composite sealed with another sealer than its active one")
			}
			if c.Describe() != tc.active.Describe() {
				t.Fatalf("the composite describes itself as %q", c.Describe())
			}
		})
	}

	// An envelope none of them knows names what it is.
	stranger := seal(t, newKeyring(t, 7, map[uint8][]byte{7: key(0xE7)}), cred("acc_1", "password"), "x")
	c := secrets.NewComposite(kms, keyring)
	if _, err := c.Open(t.Context(), cred("acc_1", "password"), stranger); !errors.Is(err, secrets.ErrUnknownKey) ||
		!strings.Contains(err.Error(), "7") {
		t.Fatalf("an envelope under key 7: %v", err)
	}
	foreign := seal(t, secretstest.New("another"), cred("acc_1", "password"), "x")
	if _, err := c.Open(t.Context(), cred("acc_1", "password"), foreign); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("a THCSEAL envelope nobody here sealed: %v", err)
	}
}

func TestResealMovesAnEnvelopeFromOneKindToTheOther(t *testing.T) {
	keyring := newKeyring(t, 1, map[uint8][]byte{1: key(0xA1)})
	kms := secretstest.New("kms")
	c := secrets.NewComposite(kms, keyring)
	old := seal(t, keyring, cred("acc_1", "oauth_token"), "1//0eXaMpLe")

	moved, err := secrets.Reseal(t.Context(), c, cred("acc_1", "oauth_token"), old)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if !c.Current(moved) || keyring.Knows(moved) {
		t.Fatal("the resealed envelope is not the active sealer's")
	}
	got, err := kms.Open(t.Context(), cred("acc_1", "oauth_token"), moved)
	if err != nil || string(got) != "1//0eXaMpLe" {
		t.Fatalf("after the move: %q, %v", got, err)
	}
}

func TestASealerThatCallsOutHonoursItsContext(t *testing.T) {
	kms := secretstest.New("kms")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := kms.Seal(ctx, cred("acc_1", "password"), []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a seal under a cancelled context: %v", err)
	}
	if _, err := secrets.NewComposite(kms).Seal(ctx, cred("acc_1", "password"), []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a composite did not pass its context on: %v", err)
	}
}

// TestTheEnvelopeLabelsNeverChange opens envelopes that earlier code produced
// from a fixed key. Every stored credential depends on these labels: the
// module rename's search and replace once turned them into the new module
// path, and production could no longer decrypt any mailbox. A credential's
// purpose is not its label either: the keyring maps it back to the field
// name every stored credential was sealed with.
func TestTheEnvelopeLabelsNeverChange(t *testing.T) {
	kr := newKeyring(t, 1, map[uint8][]byte{1: key(0x5E)})

	for name, tc := range map[string]struct {
		binding  secrets.Binding
		envelope string
		want     string
	}{
		// Sealed before the module rename (b3a8d02).
		"a password": {
			secrets.Binding{Purpose: secrets.PurposePassword, Ref: "acc_fixture"},
			"0101ba3689e795a57d406c66632be07d6d63a7da6579720212293ca5aa086930bdf8b86371bafedbf52bc8479fa358253973627d4582d73c9d530bfcfe8faaa4a4b5",
			"sealed before the module was renamed",
		},
		// Sealed by the keyring before purposes had names (b211fbf), with
		// the field "oauth_token" as the label.
		"an OAuth token": {
			secrets.Binding{Purpose: secrets.PurposeOAuthToken, Ref: "acc_fixture"},
			"01016e97e3a8916a89e2fbfdb82f216ae267c7dd60cb286bd94604cd919b5cf0c9edff317845b623420248664abfde3a0d0051958f72f047c6bf05d0b1f060",
			"sealed before purposes were named",
		},
		// The send-hash root's purpose is its own label.
		"the send-hash root": {
			secrets.Binding{Purpose: secrets.PurposeSendHashRoot, Ref: "meta/send_hash_root"},
			"010151caa69717ccdfe2e193e74384856484ce5a050109986cdd8ba16594330ad526a369268b5d4e35f8e0511ebfb01a9b375a02cc1a90cb69a1e9274281",
			"the label of the send-hash root.",
		},
	} {
		envelope, err := hex.DecodeString(tc.envelope)
		if err != nil {
			t.Fatal(err)
		}
		got, err := kr.Open(t.Context(), tc.binding, envelope)
		if err != nil {
			t.Fatalf("%s: an envelope sealed by earlier code no longer opens: %v", name, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s: Open = %q, want %q", name, got, tc.want)
		}
	}
}
