package keyscheme_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/thehappieco/kit/account"
	"github.com/thehappieco/kit/hpke"
	"github.com/thehappieco/kit/platformwrap"
	"github.com/thehappieco/kit/profiles/mailie"
	"github.com/thehappieco/kit/profiles/platform"
	"github.com/thehappieco/kit/profiles/wappie"
	"github.com/thehappieco/kit/seal"
	"github.com/thehappieco/kit/vectors"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/keyscheme"
)

func TestTheKindsAreAProfileTheKitAccepts(t *testing.T) {
	if err := seal.ValidateKinds[keyscheme.Kind](); err != nil {
		t.Fatal(err)
	}
	if keyscheme.KindMailboxGrant != seal.KindGrant || keyscheme.KindContentKey != seal.KindContentKey || keyscheme.KindUserWrap != seal.KindUserWrap {
		t.Fatal("a core kind has moved")
	}
	// A byte without a name is spelled as the kit requires. Only 0x00 is
	// pinned by a vector: the others may be named by a later version.
	for k, want := range map[keyscheme.Kind]string{0x00: "kind(0x0)", 0x0b: "kind(0xb)", 0xff: "kind(0xff)"} {
		if got := k.String(); got != want {
			t.Errorf("%d is %q, want %q", byte(k), got, want)
		}
	}
}

// Every label of the profile is distinct from the kit's other profiles'
// (its SPEC section 3.3), and the first bytes of the three envelopes of an
// account key, and of a grant, differ, so a blob in the wrong column fails at
// its first byte.
func TestNoLabelOrHeaderIsAnotherProfiles(t *testing.T) {
	ours := []string{
		keyscheme.PasswordAuthLabel, keyscheme.PasswordWrapLabel, keyscheme.RecoveryWrapLabel, keyscheme.RecoveryAuthLabel,
		keyscheme.AccountWrapTag, keyscheme.SaltLabel, keyscheme.SealLabel, keyscheme.BrowserVaultTag,
		mailie.PlatformWrapLabel, mailie.PlatformWrapSalt,
	}
	theirs := []string{
		wappie.Account().AuthLabel, wappie.Account().WrapLabel, wappie.Account().RecoveryKeyLabel, wappie.Account().RecoveryProofLabel,
		wappie.SealDomain().Label, "wappie/browser-account-key", wappie.PlatformWrapLabel, wappie.PlatformWrapSalt,
		platform.Account().AuthLabel, platform.Account().WrapLabel, platform.Account().RecoveryKeyLabel, platform.Account().RecoveryProofLabel,
	}
	seen := map[string]bool{}
	for _, l := range ours {
		if seen[l] {
			t.Errorf("%q is used twice", l)
		}
		seen[l] = true
		for _, o := range theirs {
			if l == o {
				t.Errorf("%q is another profile's label", l)
			}
		}
	}
	if keyscheme.SealMagic == wappie.SealDomain().Magic {
		t.Error("the seal magic is Wappie's")
	}
	first := map[byte]string{keyscheme.AccountWrapHeader: "account wrap", platformwrap.Header: "platform wrap", keyscheme.SealMagic[0]: "grant"}
	if len(first) != 3 {
		t.Errorf("two envelopes of the scheme start with the same byte: %v", first)
	}
}

func TestTheProfileDerivesOnlyWithinThePlatformsBounds(t *testing.T) {
	p := keyscheme.Account()
	if err := p.Check(keyscheme.DefaultKDF); err != nil {
		t.Fatal(err)
	}
	cheap := keyscheme.DefaultKDF
	cheap.M = 8 * 1024
	if _, err := account.DeriveBytes(p, "correct horse battery staple", make([]byte, 16), cheap); !errors.Is(err, account.ErrOutOfBounds) {
		t.Fatalf("a server's cheap parameters: %v", err)
	}
	if _, err := account.DeriveBytes(p, "correct horse battery staple", make([]byte, 8), keyscheme.DefaultKDF); !errors.Is(err, account.ErrOutOfBounds) {
		t.Fatalf("a short salt: %v", err)
	}
	if p.LegacyV1 {
		t.Fatal("an unbound wrap would open")
	}
}

// The salt a challenge answers is the salt of the address as the server
// stores it (users.email, auth.NormalizeEmail), however it is typed: a
// capital the database's NOCASE would not fold included. It is this
// server's alone, and nothing in it says whether the address has an account.
func TestTheSaltIsOfTheAddressAsTheServerStoresIt(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, typed := range []string{
		"ANA@example.com", " ana@Example.COM\n", cp(0xc4) + "nne@Example.com", cp(0x212a) + "na@example.com", cp(0x130) + "lker@example.com",
	} {
		stored, err := auth.NormalizeEmail(typed)
		if err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
		if got := keyscheme.NormaliseAddress(typed); got != stored {
			t.Errorf("%q is normalised to %q, stored as %q", typed, got, stored)
		}
		a, _ := keyscheme.DecoySalt(key, typed)
		b, _ := keyscheme.DecoySalt(key, stored)
		if !bytes.Equal(a, b) {
			t.Errorf("%q has another salt than %q, its stored spelling", typed, stored)
		}
	}
	a, err := keyscheme.DecoySalt(key, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := keyscheme.DecoySalt(key, "bo@example.com"); bytes.Equal(a, b) {
		t.Error("two addresses have one salt")
	}
	if b, _ := keyscheme.DecoySalt(bytes.Repeat([]byte{8}, 32), "ana@example.com"); bytes.Equal(a, b) {
		t.Error("two servers give one address the same salt")
	}
	if len(a) != keyscheme.SaltLen {
		t.Errorf("a salt of %d bytes", len(a))
	}
	if _, err := keyscheme.DecoySalt(key[:31], "ana@example.com"); err == nil {
		t.Error("a 31-byte salt key")
	}
}

func TestSealIDsAndNamespacesHaveOneSpelling(t *testing.T) {
	id := keyscheme.NewSealID()
	if !keyscheme.ValidSealID(id) || !keyscheme.ValidNamespace(id) {
		t.Fatalf("a fresh seal id %q is refused", id)
	}
	for _, bad := range []string{
		strings.ToUpper(id), "{" + id + "}", "urn:uuid:" + id, strings.ReplaceAll(id, "-", ""), id + " ",
		"00000000-0000-0000-0000-000000000000", "019a8b2c-3d4e-7f60-8a71-b2c3d4e5f607",
		id[:14] + "4" + id[15:19] + "c" + id[20:], "",
	} {
		if keyscheme.ValidSealID(bad) || keyscheme.ValidNamespace(bad) {
			t.Errorf("%q is accepted", bad)
		}
	}
}

func TestAnAccountWrapOpensOnlyForItsPersonKindAndKey(t *testing.T) {
	key, other := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	wrapKey := bytes.Repeat([]byte{3}, 32)
	person, someone := keyscheme.NewSealID(), keyscheme.NewSealID()
	wrap, err := keyscheme.SealAccountWrap(keyscheme.WrapPassword, wrapKey, key, person)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrap) != keyscheme.AccountWrapLen || keyscheme.CheckAccountWrapShape(wrap) != nil {
		t.Fatalf("a wrap of %d bytes", len(wrap))
	}
	pub, otherPub := public(t, key), public(t, other)
	if got, err := keyscheme.OpenAccountWrap(keyscheme.WrapPassword, wrapKey, wrap, person, pub); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("the wrap does not open: %v", err)
	}
	for name, open := range map[string]func() ([]byte, error){
		"another person": func() ([]byte, error) {
			return keyscheme.OpenAccountWrap(keyscheme.WrapPassword, wrapKey, wrap, someone, pub)
		},
		"the other kind": func() ([]byte, error) {
			return keyscheme.OpenAccountWrap(keyscheme.WrapRecovery, wrapKey, wrap, person, pub)
		},
		"another key": func() ([]byte, error) {
			return keyscheme.OpenAccountWrap(keyscheme.WrapPassword, wrapKey, wrap, person, otherPub)
		},
		"another wrapper": func() ([]byte, error) {
			return keyscheme.OpenAccountWrap(keyscheme.WrapPassword, key, wrap, person, pub)
		},
	} {
		if _, err := open(); !errors.Is(err, account.ErrWrongKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAGrantOpensOnlyToTheMailboxsKeyForItsPersonMailboxAndEpoch(t *testing.T) {
	alice, bob := bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 32)
	mailboxKey := bytes.Repeat([]byte{6}, 32)
	ns, otherNS := keyscheme.NewSealID(), keyscheme.NewSealID()
	aliceSeal, bobSeal := keyscheme.NewSealID(), keyscheme.NewSealID()
	g, err := keyscheme.SealGrant(public(t, alice), ns, aliceSeal, 3, mailboxKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != keyscheme.GrantLen || keyscheme.CheckGrantShape(g, 3) != nil {
		t.Fatalf("a grant of %d bytes", len(g))
	}
	if !errors.Is(keyscheme.CheckGrantShape(g, 4), keyscheme.ErrShape) {
		t.Error("the server takes a grant at another epoch")
	}
	priv := func(k []byte) hpke.PrivateKey {
		p, err := hpke.ParsePrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	mailboxPub := public(t, mailboxKey)
	if got, err := keyscheme.OpenGrant(priv(alice), ns, aliceSeal, 3, mailboxPub, g); err != nil || !bytes.Equal(got, mailboxKey) {
		t.Fatalf("the grant does not open: %v", err)
	}
	for name, open := range map[string]func() ([]byte, error){
		"bob's key":         func() ([]byte, error) { return keyscheme.OpenGrant(priv(bob), ns, aliceSeal, 3, mailboxPub, g) },
		"bob's seal id":     func() ([]byte, error) { return keyscheme.OpenGrant(priv(alice), ns, bobSeal, 3, mailboxPub, g) },
		"another mailbox":   func() ([]byte, error) { return keyscheme.OpenGrant(priv(alice), otherNS, aliceSeal, 3, mailboxPub, g) },
		"another epoch":     func() ([]byte, error) { return keyscheme.OpenGrant(priv(alice), ns, aliceSeal, 4, mailboxPub, g) },
		"another mailbox's": func() ([]byte, error) { return keyscheme.OpenGrant(priv(alice), ns, aliceSeal, 3, public(t, bob), g) },
	} {
		if _, err := open(); !errors.Is(err, seal.ErrAuthentication) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Anyone who knows Alice's public key can seal her a grant; it opens
	// only if it carries the key whose public half the server holds.
	forged, err := keyscheme.SealGrant(public(t, alice), ns, aliceSeal, 3, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyscheme.OpenGrant(priv(alice), ns, aliceSeal, 3, mailboxPub, forged); !errors.Is(err, seal.ErrAuthentication) {
		t.Errorf("a grant of a key the sealer chose: %v", err)
	}
}

func TestAGrantIsNeverSealedToALowOrderKey(t *testing.T) {
	for _, lo := range lowOrder {
		_, err := keyscheme.SealGrant(unhex(lo.hex), keyscheme.NewSealID(), keyscheme.NewSealID(), 1, bytes.Repeat([]byte{6}, 32))
		if !errors.Is(err, seal.ErrInvalidKey) {
			t.Errorf("%s: %v", lo.name, err)
		}
		if !errors.Is(keyscheme.CheckPublicKey(unhex(lo.hex)), keyscheme.ErrPublicKey) {
			t.Errorf("%s: the server would store it", lo.name)
		}
	}
	if err := keyscheme.CheckPublicKey(public(t, bytes.Repeat([]byte{4}, 32))); err != nil {
		t.Errorf("a real key: %v", err)
	}
	high := public(t, bytes.Repeat([]byte{4}, 32))
	high[31] |= 0x80
	if !errors.Is(keyscheme.CheckPublicKey(high), keyscheme.ErrPublicKey) {
		t.Error("the server would store a second spelling of a key")
	}
}

// The kit's own vectors of Mailie's platform wrap (vectors/mailie/golden)
// run through this package's profile, and its cross-product refusals under
// Wappie's: they are not repeated in testdata.
func TestThePlatformWrapRunsTheKitsMailieVectors(t *testing.T) {
	const path = "mailie/golden/platform-wrap-go.json"
	data, err := fs.ReadFile(vectors.FS, path)
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	ran := map[string]int{}
	for _, c := range f.Cases {
		if !c.forGo() {
			continue
		}
		if c.Op == "wappie.platform_wrap_open" {
			a := args{t, path + "#" + c.ID, c.In, nil, nil}
			b := platformwrap.Binding{UserID: a.str("user_id"), Sub: a.str("sub"), ProductKeyID: a.str("product_key_id"), AccountPublicKey: a.b64("account_public_key_b64")}
			if _, err := platformwrap.Open(wappie.PlatformWrap(), a.b64("product_key_b64"), a.b64("wrap_b64"), b); !errors.Is(err, platformwrap.ErrPlatformWrap) || c.Error != "platform_wrap" {
				t.Errorf("%s: %v", a.id, err)
			}
		} else {
			run(t, path, &f, c)
		}
		ran[c.Op]++
	}
	if ran["mailie.platform_wrap_open"] == 0 || ran["mailie.platform_wrap_seal"] == 0 || ran["wappie.platform_wrap_open"] == 0 {
		t.Fatalf("ran %v", ran)
	}
	if keyscheme.PlatformWrap() != mailie.PlatformWrap() {
		t.Fatal("the platform wrap is not the kit's Mailie profile")
	}
}

// The kit binds the sub as the user id of an account created through id.;
// Mailie binds the seal id for every person, and refuses a seal id that is
// the sub even when the sub is a version 4 UUID, which the platform accepts.
func TestThePlatformWrapsUserIDIsTheSealIDNeverTheSub(t *testing.T) {
	pub := public(t, bytes.Repeat([]byte{4}, 32))
	sealID, subV4 := keyscheme.NewSealID(), keyscheme.NewSealID()
	b, err := keyscheme.PlatformWrapBinding(sealID, subV4, 1, pub)
	if err != nil || b.UserID != sealID || b.Sub != subV4 {
		t.Fatalf("%+v, %v", b, err)
	}
	if _, err := keyscheme.PlatformWrapBinding(subV4, subV4, 1, pub); !errors.Is(err, keyscheme.ErrBinding) {
		t.Fatalf("the sub as the seal id: %v", err)
	}
}

func TestTheBrowserVaultBindsTheSealIDNotTheAddress(t *testing.T) {
	pub := public(t, bytes.Repeat([]byte{4}, 32))
	id := keyscheme.NewSealID()
	aad, err := keyscheme.BrowserVaultAAD(id, pub)
	if err != nil {
		t.Fatal(err)
	}
	want := `["mailie/browser-account-key",1,"` + id + `","` + base64.StdEncoding.EncodeToString(pub) + `"]`
	if string(aad) != want {
		t.Fatalf("%s, want %s", aad, want)
	}
	if _, err := keyscheme.BrowserVaultAAD("ana@example.com", pub); !errors.Is(err, keyscheme.ErrBinding) {
		t.Fatalf("an address as the seal id: %v", err)
	}
}
