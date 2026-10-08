package kmssealer_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/thehappieco/kit/kms"
	"github.com/thehappieco/kit/thcseal"

	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/kmssealer"
	"github.com/thehappieco/mailie/internal/secrets/secretstest"
	"github.com/thehappieco/mailie/internal/store"
)

// testKeyARN names the wrapper's key; AWS's documentation account.
const testKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func newSealer(t *testing.T, w kms.Wrapper, env string) *kmssealer.Sealer {
	t.Helper()
	s, err := kmssealer.New(w, env, testKeyARN)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func seal(t *testing.T, s secrets.Sealer, b secrets.Binding, plaintext string) []byte {
	t.Helper()
	sealed, err := s.Seal(t.Context(), b, []byte(plaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

func TestACredentialSealedUnderKMSOpensOnlyForItsOwnPurposeAndRef(t *testing.T) {
	s := newSealer(t, secretstest.NewWrapper("kms"), "prod")
	own := secrets.Credential("acc_00000000000000a1", "oauth_token")
	sealed := seal(t, s, own, `{"refresh_token":"1//0eXaMpLe"}`)
	if bytes.Contains(sealed, []byte("0eXaMpLe")) {
		t.Fatal("the plaintext is visible in the envelope")
	}
	if !bytes.HasPrefix(sealed, []byte(secrets.MagicTHCSEAL)) {
		t.Fatalf("not a THCSEAL envelope: %q", sealed[:8])
	}
	if id, err := secrets.KeyID(sealed); err != nil || id != 0 {
		t.Fatalf("KeyID = %d, %v; want 0, which no keyring key has", id, err)
	}
	got, err := s.Open(t.Context(), own, sealed)
	if err != nil || string(got) != `{"refresh_token":"1//0eXaMpLe"}` {
		t.Fatalf("Open for its own binding: %q, %v", got, err)
	}

	// Pasted into another mailbox's row, another field's, or the send-hash
	// root's, it does not open there.
	for _, other := range []secrets.Binding{
		secrets.Credential("acc_00000000000000a2", "oauth_token"),
		secrets.Credential("acc_00000000000000a1", "password"),
		store.SendHashRootBinding,
	} {
		if _, err := s.Open(t.Context(), other, sealed); !errors.Is(err, secrets.ErrDecrypt) || !secrets.DoesNotOpen(err) {
			t.Errorf("opened for %+v: %v; want ErrDecrypt", other, err)
		}
	}
	root := seal(t, s, store.SendHashRootBinding, strings.Repeat("r", 32))
	if _, err := s.Open(t.Context(), own, root); !errors.Is(err, secrets.ErrDecrypt) {
		t.Errorf("the send-hash root opened as a credential: %v", err)
	}
}

func TestEveryEnvelopeHasADataKeyOfItsOwn(t *testing.T) {
	w := secretstest.NewWrapper("kms")
	s := newSealer(t, w, "prod")
	b := secrets.Credential("acc_00000000000000a1", "password")
	a, c := seal(t, s, b, "hunter2"), seal(t, s, b, "hunter2")
	if bytes.Equal(a, c) || w.Generated() != 2 {
		t.Fatalf("two seals: equal %v, %d data keys; want two different envelopes, two keys", bytes.Equal(a, c), w.Generated())
	}
}

func TestAnEnvelopeOpensOnlyInTheEnvAndUnderTheKeyItWasSealedIn(t *testing.T) {
	w := secretstest.NewWrapper("kms")
	b := secrets.Credential("acc_00000000000000a1", "password")
	sealed := seal(t, newSealer(t, w, "prod"), b, "hunter2")

	for name, s := range map[string]*kmssealer.Sealer{
		"another env": newSealer(t, w, "dev"),
		"another key": newSealer(t, secretstest.NewWrapper("another"), "prod"),
	} {
		_, err := s.Open(t.Context(), b, sealed)
		if !errors.Is(err, secrets.ErrDecrypt) || !secrets.DoesNotOpen(err) {
			t.Errorf("%s: %v; want ErrDecrypt", name, err)
		}
		// Said as what it most likely is, settings that changed, which a key
		// added beside this one could never open.
		if !errors.Is(err, secrets.ErrSealedElsewhere) {
			t.Errorf("%s: %v; want ErrSealedElsewhere", name, err)
		}
		// Nothing in the header says it: the sealer would write the same.
		if !s.Current(sealed) || !s.Knows(sealed) {
			t.Errorf("%s: an envelope of its provider reads as not current", name)
		}
	}
}

func TestAnEnvelopeFromAnotherKeyProviderIsRefusedWithoutAskingKMS(t *testing.T) {
	// A development key's envelope (the kit's localkek writes 0x7F) in a
	// production database: the AWS provider must never open it.
	b := secrets.Credential("acc_00000000000000a1", "password")
	dev := newSealer(t, secretstest.NewWrapperFor("kms", kms.ProviderLocal), "prod")
	sealed := seal(t, dev, b, "hunter2")

	w := secretstest.NewWrapper("kms")
	s := newSealer(t, w, "prod")
	_, err := s.Open(t.Context(), b, sealed)
	if !errors.Is(err, secrets.ErrUnknownKey) || !secrets.DoesNotOpen(err) || !strings.Contains(err.Error(), "0x7f") {
		t.Fatalf("an envelope of another provider: %v; want ErrUnknownKey naming it", err)
	}
	if errors.Is(err, secrets.ErrSealedElsewhere) {
		t.Fatalf("an envelope of another provider reads as one of this key's settings: %v", err)
	}
	if w.Decrypted() != 0 {
		t.Fatal("KMS was asked to unwrap another provider's key")
	}
	if !s.Knows(sealed) || s.Current(sealed) {
		t.Fatalf("knows %v, current %v; want known, not current, so a rewrap moves it", s.Knows(sealed), s.Current(sealed))
	}

	// Relabelled as KMS's, it does not open either: the header is
	// authenticated.
	relabelled := bytes.Clone(sealed)
	relabelled[len(thcseal.Magic)+1] = kms.ProviderAWS
	if _, err := s.Open(t.Context(), b, relabelled); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("a relabelled envelope: %v; want ErrDecrypt", err)
	}
}

func TestAKeyServiceThatCannotAnswerNeverReadsAsAnEnvelopeThatDoesNotOpen(t *testing.T) {
	// A send-hash root, or a credential, that KMS could not unwrap because
	// it was not reached, throttled or refused the call is not lost: told it
	// was, an operator would replace a root that still opens.
	w := secretstest.NewWrapper("kms")
	s := newSealer(t, w, "prod")
	sealed := seal(t, s, store.SendHashRootBinding, strings.Repeat("r", 32))

	for _, failure := range []error{
		errors.New("operation error KMS: Decrypt, https response error StatusCode: 0, dial tcp: i/o timeout"),
		errors.New("operation error KMS: Decrypt, ThrottlingException: Rate exceeded"),
		errors.New("operation error KMS: Decrypt, AccessDeniedException: not authorized to perform kms:Decrypt"),
		errors.New("operation error KMS: Decrypt, DisabledException: the key is disabled"),
	} {
		w.FailDecrypts(failure)
		_, err := s.Open(t.Context(), store.SendHashRootBinding, sealed)
		if !errors.Is(err, failure) || secrets.DoesNotOpen(err) {
			t.Errorf("%v: Open = %v; want it as it is, not an envelope that does not open", failure, err)
		}
	}
	w.FailDecrypts(nil)

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Open(ended, store.SendHashRootBinding, sealed); !errors.Is(err, context.Canceled) || secrets.DoesNotOpen(err) {
		t.Errorf("a context that ended: %v", err)
	}
	if _, err := s.Open(t.Context(), store.SendHashRootBinding, sealed); err != nil {
		t.Fatalf("once KMS answers again: %v", err)
	}
}

func TestAKeyringEnvelopeIsKnownOnlyToTheKeyringAndNamedByItsKey(t *testing.T) {
	keyring, err := secrets.NewKeyring(3, map[uint8][]byte{3: bytes.Repeat([]byte{0xA1}, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	b := secrets.Credential("acc_00000000000000a1", "password")
	old := seal(t, keyring, b, "hunter2")

	s := newSealer(t, secretstest.NewWrapper("kms"), "prod")
	if s.Knows(old) || s.Current(old) {
		t.Fatal("the KMS sealer claims a keyring envelope")
	}
	if _, err := s.Open(t.Context(), b, old); !errors.Is(err, secrets.ErrUnknownKey) || !strings.Contains(err.Error(), ": 3,") {
		t.Fatalf("a keyring envelope opened alone: %v; want ErrUnknownKey naming key 3", err)
	}

	// Beside the keyring, the KMS sealer seals and the keyring still opens
	// what it sealed, until a rewrap moves it.
	both := secrets.NewComposite(s, keyring)
	if got, err := both.Open(t.Context(), b, old); err != nil || string(got) != "hunter2" {
		t.Fatalf("the composite does not open the keyring's envelope: %q, %v", got, err)
	}
	if both.Current(old) {
		t.Fatal("a keyring envelope reads as current under KMS")
	}
	moved, err := secrets.Reseal(t.Context(), both, b, old)
	if err != nil || !s.Current(moved) {
		t.Fatalf("Reseal: current %v, %v", s.Current(moved), err)
	}
	if _, err := keyring.Open(t.Context(), b, moved); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("the keyring opens a THCSEAL envelope: %v", err)
	}
}

func TestAMalformedTHCSEALEnvelopeIsMalformed(t *testing.T) {
	s := newSealer(t, secretstest.NewWrapper("kms"), "prod")
	b := secrets.Credential("acc_00000000000000a1", "password")
	sealed := seal(t, s, b, "hunter2")
	for name, envelope := range map[string][]byte{
		"cut short":       sealed[:20],
		"another version": append(append([]byte(thcseal.Magic), 0x02), sealed[len(thcseal.Magic)+1:]...),
		"not an envelope": []byte("hunter2"),
		"empty":           nil,
	} {
		_, err := s.Open(t.Context(), b, envelope)
		if !errors.Is(err, secrets.ErrMalformed) || !secrets.DoesNotOpen(err) {
			t.Errorf("%s: %v; want ErrMalformed", name, err)
		}
		if s.Current(envelope) {
			t.Errorf("%s reads as current", name)
		}
	}
	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	if _, err := s.Open(t.Context(), b, flipped); !errors.Is(err, secrets.ErrDecrypt) {
		t.Errorf("a flipped tag: %v; want ErrDecrypt", err)
	}
}

func TestTheEncryptionContextIsTheBindingInItsDeployment(t *testing.T) {
	if secrets.MagicTHCSEAL != thcseal.Magic {
		t.Fatalf("secrets.MagicTHCSEAL %q is not the kit's %q", secrets.MagicTHCSEAL, thcseal.Magic)
	}
	for b, want := range map[secrets.Binding]map[string]string{
		secrets.Credential("acc_0123456789abcdef", "oauth_token"): {
			"service": "mailie", "env": "prod", "purpose": "credential/oauth-token", "ref": "acc_0123456789abcdef",
		},
		secrets.Credential("acc_0123456789abcdef", "password"): {
			"service": "mailie", "env": "prod", "purpose": "credential/password", "ref": "acc_0123456789abcdef",
		},
		store.SendHashRootBinding: {
			"service": "mailie", "env": "prod", "purpose": "send/hash-root", "ref": "meta/send_hash_root",
		},
	} {
		if got := kmssealer.EncryptionContext("prod", b); !maps.Equal(got, want) {
			t.Errorf("%+v: %v; want %v", b, got, want)
		}
		if err := kmssealer.Context("prod", b).Validate(); err != nil {
			t.Errorf("%+v: a context KMS would refuse: %v", b, err)
		}
	}
}

func TestASealerIsRefusedAnEnvTheContextCannotCarry(t *testing.T) {
	w := secretstest.NewWrapper("kms")
	for _, env := range []string{"", "Prod", "prod env", strings.Repeat("p", kms.MaxFieldLen+1)} {
		if _, err := kmssealer.New(w, env, testKeyARN); !errors.Is(err, kms.ErrInvalidContext) {
			t.Errorf("env %q: %v; want ErrInvalidContext", env, err)
		}
	}
	if _, err := kmssealer.New(nil, "prod", testKeyARN); err == nil {
		t.Error("a sealer without a wrapper")
	}
	if _, err := kmssealer.New(w, "prod", ""); err == nil {
		t.Error("a sealer whose key has no name")
	}
	s := newSealer(t, w, "prod")
	if d := s.Describe(); !strings.Contains(d, testKeyARN) || !strings.Contains(d, "prod") {
		t.Errorf("Describe = %q; want the key's ARN and the env", d)
	}
}

func TestABindingNoKeyServiceTakesSealsAndOpensNothing(t *testing.T) {
	w := secretstest.NewWrapper("kms")
	s := newSealer(t, w, "prod")
	sealed := seal(t, s, secrets.Credential("acc_00000000000000a1", "password"), "hunter2")
	for _, b := range []secrets.Binding{
		secrets.Credential("acc_00000000000000a1", "refresh"),
		{Purpose: secrets.PurposePassword, Ref: "Acc 1"},
		{Purpose: secrets.PurposePassword},
	} {
		if _, err := s.Seal(t.Context(), b, []byte("x")); !errors.Is(err, secrets.ErrBinding) {
			t.Errorf("Seal for %+v: %v; want ErrBinding", b, err)
		}
		if _, err := s.Open(t.Context(), b, sealed); !errors.Is(err, secrets.ErrBinding) || secrets.DoesNotOpen(err) {
			t.Errorf("Open for %+v: %v; want ErrBinding", b, err)
		}
	}
	if w.Generated() != 1 || w.Decrypted() != 0 {
		t.Fatalf("KMS was called for a binding it would refuse: %d keys, %d unwrapped", w.Generated(), w.Decrypted())
	}
}
