package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/secrets"
)

// The salt key is K_salt of docs/key-scheme.md section 5.3: the key of the
// salt the server hands out for an address, HMAC-SHA256 of the address as the
// server stores it. Every account's salt is its address's under this key, and
// a challenge for an address with no account answers the same, so a challenge
// does not say which addresses have one.
//
// It is random, made once, and kept in the meta table sealed like a
// credential, as the send-hash root is (metasecret.go). A challenge answers
// any address's salt to whoever asks, so what it guards is less a secret than
// the server's own: never an operator's variable, and never in a backup in
// clear.
const (
	// MetaKDFSaltKey is the meta row that keeps the salt key: the base64 of
	// its envelope.
	MetaKDFSaltKey = "kdf_salt_key"
	// MetaKDFSaltKeySealedWith is the meta row that says, in words, what
	// sealed it; only ever for a message.
	MetaKDFSaltKeySealedWith = "kdf_salt_key_sealed_with"
	// KDFSaltKeyLen is the salt key's size.
	KDFSaltKeyLen = 32
)

// KDFSaltKeyBinding is what the salt key is sealed for.
var KDFSaltKeyBinding = secrets.Binding{Purpose: secrets.PurposeKDFSaltKey, Ref: "meta/" + MetaKDFSaltKey}

var (
	// ErrKDFSaltKey is a salt key the configured keys do not open, as
	// ErrSendHashRoot is a root.
	ErrKDFSaltKey = errors.New("store: the salt key does not open with the configured keys")
	// ErrKDFSaltKeyOpens is a replacement asked for a salt key the
	// configured sealer opens.
	ErrKDFSaltKeyOpens = errors.New("store: the salt key opens with the configured keys; it is not replaced")
	// ErrKDFSaltKeyMayOpen is a replacement asked, without saying the key is
	// lost, for a salt key of the configured sealer's own kind that does not
	// open (secrets.ErrSealedElsewhere).
	ErrKDFSaltKeyMayOpen = errors.New("store: the salt key may open under the KMS key and MAIL_ENV " +
		"that sealed it; it is not replaced unless that key is lost for good")
)

var kdfSaltKey = sealedSecret{
	key: MetaKDFSaltKey, sealedWith: MetaKDFSaltKeySealedWith, binding: KDFSaltKeyBinding,
	size: KDFSaltKeyLen, what: "salt key",
	notOpen: ErrKDFSaltKey, opens: ErrKDFSaltKeyOpens, mayOpen: ErrKDFSaltKeyMayOpen,
}

// KDFSaltKey opens the database's salt key, and reports whether it made it
// now: the first time a database without one is opened. One the configured
// keys do not open is ErrKDFSaltKey, never replaced. Any other error is
// returned as it is.
func (s *Store) KDFSaltKey(ctx context.Context, sealer secrets.Sealer) ([]byte, bool, error) {
	return kdfSaltKey.open(ctx, s, sealer)
}

// ResealKDFSaltKeyTx seals the salt key again with sealer's active sealer
// when its envelope is not what that writes now, and reports whether it did,
// as ResealSendHashRootTx does the root.
func ResealKDFSaltKeyTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) (bool, error) {
	return kdfSaltKey.resealTx(ctx, tx, sealer)
}

// ReplaceKDFSaltKeyTx puts a new salt key in place of one the configured keys
// do not open (ErrKDFSaltKey), under the rules of ReplaceSendHashRootTx.
//
// Nothing stops working: every account keeps the salt it derives with, and is
// moved to its address's salt under the new key at the person's next sign-in
// (docs/key-scheme.md section 5.3). Until then a challenge tells such an
// account from an address that has none (the threat model's section 5.9).
func ReplaceKDFSaltKeyTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer, kmsKeyLost bool) error {
	return kdfSaltKey.replaceTx(ctx, tx, sealer, kmsKeyLost)
}
