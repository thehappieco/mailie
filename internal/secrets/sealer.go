package secrets

import (
	"context"
	"errors"
	"fmt"
)

// MagicTHCSEAL starts every THCSEAL envelope: a secret sealed under a key
// service's data key, one data key per envelope, with the encryption context
// {service, env, purpose, ref}. A keyring envelope starts with Version1
// (0x01) instead, so the first byte alone tells the two kinds apart. The
// keyring never opens one; the credentials table records key id 0 for it.
const MagicTHCSEAL = "THCSEAL"

// The purposes this server seals for, and so the only ones a key service's
// policy has to allow: each is the purpose of a THCSEAL envelope's encryption
// context, and a purpose added here changes that policy before the code that
// uses it ships. A Binding with any other purpose seals and opens nothing.
//
//nolint:gosec // G101: a purpose names what a secret is; it is not one
const (
	// PurposeOAuthToken is an account's OAuth token, the credentials row
	// whose field is "oauth_token".
	PurposeOAuthToken = "credential/oauth-token"
	// PurposePassword is an account's IMAP password, the credentials row
	// whose field is "password".
	PurposePassword = "credential/password"
	// PurposeSendHashRoot is the key of a send record's hashes.
	PurposeSendHashRoot = "send/hash-root"
	// PurposeKDFSaltKey is the key of the salts the server hands out for
	// the addresses people sign in with (docs/key-scheme.md section 5.3).
	PurposeKDFSaltKey = "auth/kdf-salt-key"
)

// Purposes lists every purpose this server seals for.
func Purposes() []string {
	return []string{PurposeOAuthToken, PurposePassword, PurposeSendHashRoot, PurposeKDFSaltKey}
}

// MaxBindingLen is the longest purpose or ref a binding may have, as a key
// service's encryption context takes it.
const MaxBindingLen = 128

// Binding is what an envelope is sealed for, and the only thing it opens
// for. Purpose is what the secret is, one of Purposes; Ref is which one: a
// credential's account id, the meta row that keeps the send-hash root or the
// salt key. Both
// are part of what an envelope authenticates, so one moved to another row
// does not open there.
//
// A binding is what a key service's encryption context carries beside the
// service and the deployment, so Validate holds both to that context's rule
// (at most MaxBindingLen bytes of [a-z0-9._:/-]) in every sealer, the
// keyring included: a binding no key service would take is refused where it
// is made, not at the first sealing in production.
type Binding struct {
	Purpose string
	Ref     string
}

// credentialPurposes maps a credentials row's field to its purpose.
var credentialPurposes = map[string]string{
	"oauth_token": PurposeOAuthToken,
	"password":    PurposePassword,
}

// Credential is the binding of one field of one account's credentials:
// "oauth_token" or "password", the two fields the credentials table holds.
// Any other field has no purpose, and its binding seals and opens nothing
// (ErrBinding).
func Credential(accountID, field string) Binding {
	return Binding{Purpose: credentialPurposes[field], Ref: accountID}
}

// Validate reports whether a sealer may seal or open for b: a purpose of
// Purposes, and a ref within the encryption context's rule. The error says
// which part fails, never its value.
func (b Binding) Validate() error {
	switch b.Purpose {
	case PurposeOAuthToken, PurposePassword, PurposeSendHashRoot, PurposeKDFSaltKey:
	case "":
		return fmt.Errorf("%w: it has no purpose", ErrBinding)
	default:
		return fmt.Errorf("%w: its purpose is not one this server seals for", ErrBinding)
	}
	if b.Ref == "" {
		return fmt.Errorf("%w: it has no ref", ErrBinding)
	}
	if len(b.Ref) > MaxBindingLen {
		return fmt.Errorf("%w: its ref is longer than %d bytes", ErrBinding, MaxBindingLen)
	}
	for i := 0; i < len(b.Ref); i++ {
		if !contextByte(b.Ref[i]) {
			return fmt.Errorf("%w: its ref has a character outside [a-z0-9._:/-]", ErrBinding)
		}
	}
	return nil
}

// contextByte reports whether c may appear in an encryption context's value.
func contextByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	case c == '.', c == '_', c == ':', c == '/', c == '-':
		return true
	}
	return false
}

// Sealer seals the secrets this server must be able to open again.
//
// Seal and Open take a context because a sealer may call a key service for
// every envelope: a caller passes the one that bounds that call. Knows and
// Current only read an envelope's header, never opening it, so that a rewrap
// can tell what it has to re-seal without a call per row.
type Sealer interface {
	// Seal seals plaintext for b, with what this sealer seals with now.
	Seal(ctx context.Context, b Binding, plaintext []byte) ([]byte, error)
	// Open opens an envelope sealed for b.
	//
	// An envelope that does not open here (sealed for any other binding,
	// under a key the sealer does not hold, altered, or not an envelope) is
	// ErrDecrypt, ErrUnknownKey or ErrMalformed, and nothing else is: a
	// sealer under a key service maps its own such answers onto them. Any
	// other error (a key service not reached, or that refused the call, a
	// context that ended, a binding that is not valid) says nothing about
	// the envelope and is returned as it is. DoesNotOpen tells the two
	// apart, and only the first is a reason to ask for another key.
	Open(ctx context.Context, b Binding, envelope []byte) ([]byte, error)
	// Knows reports whether envelope is of this sealer's kind and under a
	// key it holds: whether Open can try it.
	Knows(envelope []byte) bool
	// Current reports whether envelope is what Seal would write now: its
	// kind, under its key. An envelope that is not is what
	// `rewrap-credentials` re-seals.
	Current(envelope []byte) bool
	// Describe names what Seal seals with, for logs and the command line. It
	// never holds key material.
	Describe() string
}

// Composite is a Sealer that seals with its active sealer and opens an
// envelope with whichever of its sealers knows it: the active one first, then
// the others in order. That is how the kind of key changes without a flag
// day: the new kind seals, the old one still opens what it sealed, and
// `rewrap-credentials` moves every row to the new one.
type Composite struct {
	sealers []Sealer
}

var _ Sealer = (*Composite)(nil)

// NewComposite builds a composite that seals with active and opens what
// active or any of others knows.
func NewComposite(active Sealer, others ...Sealer) *Composite {
	return &Composite{sealers: append([]Sealer{active}, others...)}
}

// Seal seals with the active sealer.
func (c *Composite) Seal(ctx context.Context, b Binding, plaintext []byte) ([]byte, error) {
	return c.sealers[0].Seal(ctx, b, plaintext)
}

// Open opens with the first sealer that knows the envelope.
func (c *Composite) Open(ctx context.Context, b Binding, envelope []byte) ([]byte, error) {
	for _, s := range c.sealers {
		if s.Knows(envelope) {
			return s.Open(ctx, b, envelope)
		}
	}
	return nil, unknownEnvelope(envelope)
}

// Knows reports whether any of the sealers knows the envelope.
func (c *Composite) Knows(envelope []byte) bool {
	for _, s := range c.sealers {
		if s.Knows(envelope) {
			return true
		}
	}
	return false
}

// Current reports whether the active sealer would write the envelope now.
func (c *Composite) Current(envelope []byte) bool { return c.sealers[0].Current(envelope) }

// Describe names the active sealer.
func (c *Composite) Describe() string { return c.sealers[0].Describe() }

// Reseal opens an envelope with whichever sealer of s knows it and seals it
// again with the active one, for the same binding. The plaintext never leaves
// this function.
func Reseal(ctx context.Context, s Sealer, b Binding, envelope []byte) ([]byte, error) {
	plaintext, err := s.Open(ctx, b, envelope)
	if err != nil {
		return nil, err
	}
	sealed, err := s.Seal(ctx, b, plaintext)
	// Best effort: Go cannot guarantee the copy is gone, but leaving the
	// buffer readable in the heap for the rest of the process is worse.
	clear(plaintext)
	return sealed, err
}

// DoesNotOpen reports whether err from a Sealer's Open says the envelope does
// not open with the configured keys (ErrDecrypt, ErrUnknownKey or
// ErrMalformed) rather than that the sealer could not try. Only then is the
// envelope known to need another key, or to be lost with its key: a caller
// that would replace what it cannot open, or tell the operator to, does so
// for this alone.
func DoesNotOpen(err error) bool {
	return errors.Is(err, ErrDecrypt) || errors.Is(err, ErrUnknownKey) || errors.Is(err, ErrMalformed)
}

// unknownEnvelope is the error for an envelope no configured sealer knows,
// saying what it is so the operator knows which key to give back.
func unknownEnvelope(envelope []byte) error {
	switch {
	case isTHCSEAL(envelope):
		return fmt.Errorf("%w: a THCSEAL envelope, which no configured sealer opens", ErrUnknownKey)
	case len(envelope) >= headerLen && envelope[0] == Version1:
		return fmt.Errorf("%w: %d", ErrUnknownKey, envelope[1])
	default:
		return ErrMalformed
	}
}
