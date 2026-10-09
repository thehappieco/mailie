// Package kmssealer is the secrets.Sealer under a key service: every
// envelope a THCSEAL v1 envelope (the kit's thcseal, the platform's tier-2
// format), with a data key of its own that a kms.Wrapper makes and unwraps
// under the encryption context {service: mailie, env, purpose, ref}. In the
// daemon the wrapper is AWS KMS (the kit's kms/awskms, which app.NewSealer
// dials); in tests it is secretstest.Wrapper.
//
// Unlike the keyring, the key never reaches this process: KMS keeps it, and
// hands out a data key per envelope, wrapped under it and bound to the
// envelope's context. A database and its backups therefore open only where
// KMS agrees to unwrap, for exactly the binding each envelope was sealed for,
// and every unwrapping is a CloudTrail record that names the purpose and the
// ref, never a person or an address.
//
// This package links no AWS SDK: it sees only the kit's kms.Wrapper
// interface, so internal/store and the other importers of internal/secrets
// stay as they are, and only the binaries that build the sealer link the SDK.
package kmssealer

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/thehappieco/kit/kms"
	"github.com/thehappieco/kit/thcseal"

	"github.com/thehappieco/mailie/internal/secrets"
)

// Service is the service of every envelope's encryption context, which a key
// policy conditions on beside the env and the purpose.
const Service = "mailie"

// Context is the encryption context of an envelope sealed for b in env:
// {service: mailie, env, purpose, ref}. KMS writes it to CloudTrail in clear
// and the key policy conditions on it, so it names what a secret is and
// which row keeps it, never whose it is: the purpose is one of
// secrets.Purposes, the ref a mailbox's random id or the meta row of the
// send-hash root or the salt key.
func Context(env string, b secrets.Binding) kms.Context {
	return kms.Context{Service: Service, Env: env, Purpose: b.Purpose, Ref: b.Ref}
}

// EncryptionContext is Context as KMS takes it, the pairs a key policy's
// conditions read, as backup.EncryptionContext is a backup's.
func EncryptionContext(env string, b secrets.Binding) map[string]string {
	return Context(env, b).Map()
}

// Sealer seals with a kms.Wrapper. It is a secrets.Sealer.
type Sealer struct {
	wrapper kms.Wrapper
	env     string
	key     string
}

var _ secrets.Sealer = (*Sealer)(nil)

// New builds a sealer over wrapper for env, the deployment (MAIL_ENV), which
// every envelope's context carries: an envelope sealed in one env does not
// open in another. key names the wrapper's key for Describe (an AWS KMS key's
// ARN); it is never key material.
func New(wrapper kms.Wrapper, env, key string) (*Sealer, error) {
	if wrapper == nil {
		return nil, errors.New("kmssealer: no key wrapper")
	}
	if key == "" {
		return nil, errors.New("kmssealer: the key is not named")
	}
	// Any binding will do: the env is the one field a binding does not
	// hold to the rule already.
	probe := Context(env, secrets.Binding{Purpose: secrets.PurposePassword, Ref: "ref"})
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("kmssealer: the env: %w", err)
	}
	return &Sealer{wrapper: wrapper, env: env, key: key}, nil
}

// Describe names the key and the env every envelope is sealed under.
func (s *Sealer) Describe() string { return fmt.Sprintf("KMS key %s (env %s)", s.key, s.env) }

// Seal seals plaintext for b, under a fresh data key from the wrapper. An
// error is the key service's, or the binding's: nothing is sealed.
func (s *Sealer) Seal(ctx context.Context, b secrets.Binding, plaintext []byte) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return thcseal.Seal(ctx, s.wrapper, Context(s.env, b), plaintext)
}

// Open opens an envelope sealed for b.
//
// What says the envelope does not open here is one of the errors
// secrets.DoesNotOpen reads: an envelope that is not THCSEAL v1 or is cut
// short (ErrMalformed), one of another key provider, such as a development
// key's, or a keyring envelope (ErrUnknownKey), and one whose data key does
// not unwrap under its binding or whose ciphertext does not authenticate
// (ErrDecrypt: another env's, another key's, another mailbox's, another
// field's, or altered). That last one is also secrets.ErrSealedElsewhere:
// the header names neither the key nor the env, so this sealer cannot tell
// a lost key from a MAIL_ENV or key ARN that changed, which putting back
// would fix. Anything else, KMS not reached, throttling, refusing the call or
// answering for another key, or a context that ended, says nothing about the
// envelope and is returned as it is: it must never be taken for a lost key.
func (s *Sealer) Open(ctx context.Context, b secrets.Binding, envelope []byte) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if !s.Knows(envelope) {
		if id, err := secrets.KeyID(envelope); err == nil && id != 0 {
			return nil, fmt.Errorf("%w: %d, a credential key's envelope, which %s does not open",
				secrets.ErrUnknownKey, id, s.Describe())
		}
		return nil, secrets.ErrMalformed
	}
	decoded, err := thcseal.Decode(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", secrets.ErrMalformed, err)
	}
	if decoded.Provider != s.wrapper.Provider() {
		return nil, fmt.Errorf("%w: a THCSEAL envelope of key provider 0x%02x, not 0x%02x, the provider of %s",
			secrets.ErrUnknownKey, decoded.Provider, s.wrapper.Provider(), s.Describe())
	}
	plaintext, err := thcseal.Open(ctx, s.wrapper, Context(s.env, b), envelope)
	switch {
	case err == nil:
		return plaintext, nil
	case errors.Is(err, thcseal.ErrDecrypt):
		// Which of the binding, the key, the env or the bytes it was is not
		// said, as the keyring does not say it; only that it is of this
		// sealer's kind, sealed under settings it does not have now.
		return nil, fmt.Errorf("%w: %w", secrets.ErrDecrypt, secrets.ErrSealedElsewhere)
	case errors.Is(err, thcseal.ErrMalformed):
		return nil, fmt.Errorf("%w: %w", secrets.ErrMalformed, err)
	case errors.Is(err, thcseal.ErrProviderMismatch):
		return nil, fmt.Errorf("%w: %w", secrets.ErrUnknownKey, err)
	default:
		return nil, err
	}
}

// Knows reports whether envelope is a THCSEAL envelope, which only this
// sealer may try: the keyring never opens one. Whether its provider is this
// sealer's is Open's to say, so that one of another provider is reported as
// such rather than as an envelope nothing knows.
func (s *Sealer) Knows(envelope []byte) bool {
	return bytes.HasPrefix(envelope, []byte(thcseal.Magic))
}

// Current reports whether envelope is what Seal writes now: a well-formed
// THCSEAL v1 envelope of this wrapper's provider.
//
// That is all its header can say. The magic, the version, the provider byte
// and the length of the wrapped data key are decidable without a call; the
// KMS key that wrapped the data key is not, because the wrapped key is KMS's
// CiphertextBlob, whose layout AWS does not document, and the env and the
// binding are only in the additional data. So an envelope under another key
// of the same provider, or sealed in another env, reads as current here and
// fails to open (ErrDecrypt): moving to another KMS key, or to another
// MAIL_ENV, is not a rewrap. KMS's own rotation of a key keeps its ARN and
// opens every envelope it ever wrapped, and needs nothing here.
func (s *Sealer) Current(envelope []byte) bool {
	decoded, err := thcseal.Decode(envelope)
	return err == nil && decoded.Provider == s.wrapper.Provider()
}
