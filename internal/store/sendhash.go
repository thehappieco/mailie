package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/secrets"
)

// The send-hash root is the key of the hashes a send record keeps of what was
// composed, and of the idempotency key of a send that came without one
// (service.Deps.SendHashKey): without it, neither the record nor a log line
// can confirm a guess of a message.
//
// It is random, made once, and kept in the meta table sealed like a
// credential (metasecret.go), so it opens wherever the credentials do,
// survives a rotation of the key (rewrap-credentials re-seals it with them)
// and never needs key material a sealer may not have to hand.
const (
	// MetaSendHashRoot is the meta row that keeps the root: the base64 of
	// its envelope.
	MetaSendHashRoot = "send_hash_root"
	// MetaSendHashRootSealedWith is the meta row that says, in words, what
	// sealed the root (the sealer's Describe when it did: a credential key's
	// id, or a KMS key's ARN and the env). Nothing authenticates it, so it
	// only ever goes into a message, and decides nothing: when the root does
	// not open, it names what did seal it, which an envelope under a key
	// service does not.
	MetaSendHashRootSealedWith = "send_hash_root_sealed_with"
	// SendHashRootLen is the root's size.
	SendHashRootLen = 32
)

// SendHashRootBinding is what the root is sealed for: a credential's envelope
// moved into its row, or its envelope into a credential's, does not open.
var SendHashRootBinding = secrets.Binding{Purpose: secrets.PurposeSendHashRoot, Ref: "meta/" + MetaSendHashRoot}

// ErrSendHashRoot is a send-hash root the configured keys do not open: the
// key that sealed it is not given, the row was altered, or it is not a root
// (secrets.DoesNotOpen, or the row's own shape). A sealer that could not try
// (a key service not reached, or that refused the call, a context that
// ended) is never this: that says nothing about the root, which may open on
// the next try.
var ErrSendHashRoot = errors.New("store: the send-hash root does not open with the configured keys")

// ErrSendHashRootOpens is a replacement asked for a root the configured
// sealer opens, which nothing needs and which would forget every send record.
var ErrSendHashRootOpens = errors.New("store: the send-hash root opens with the configured keys; it is not replaced")

// ErrSendHashRootMayOpen is a replacement asked, without saying the key is
// lost, for a root that is of the configured sealer's own kind and does not
// open (secrets.ErrSealedElsewhere): sealed under another KMS key or another
// MAIL_ENV, it opens again once they are put back, and replacing it would
// forget every send record.
var ErrSendHashRootMayOpen = errors.New("store: the send-hash root may open under the KMS key and MAIL_ENV " +
	"that sealed it; it is not replaced unless that key is lost for good")

// sendHashRoot is the root as a sealed secret of the meta table.
var sendHashRoot = sealedSecret{
	key: MetaSendHashRoot, sealedWith: MetaSendHashRootSealedWith, binding: SendHashRootBinding,
	size: SendHashRootLen, what: "send-hash root",
	notOpen: ErrSendHashRoot, opens: ErrSendHashRootOpens, mayOpen: ErrSendHashRootMayOpen,
}

// SendHashRoot opens the database's send-hash root, and reports whether it
// made it now: the first time a database without one is opened. A root the
// configured keys do not open is ErrSendHashRoot, never replaced: that would
// hide the wrong key until the first credential it cannot open. Any other
// error is returned as it is.
func (s *Store) SendHashRoot(ctx context.Context, sealer secrets.Sealer) ([]byte, bool, error) {
	return sendHashRoot.open(ctx, s, sealer)
}

// ResealSendHashRootTx seals the root again with sealer's active sealer when
// its envelope is not what that writes now, opening it with whichever sealer
// knows it, and reports whether it did. A database without a root has nothing
// to re-seal: its daemon's next start makes one.
//
// A root that is current is opened all the same, and one that does not open
// is an error, never "already sealed": under a key service the header names
// neither the key nor the env, so a root sealed under another KMS key or
// another MAIL_ENV reads as current and opens only once they are put back.
func ResealSendHashRootTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer) (bool, error) {
	return sendHashRoot.resealTx(ctx, tx, sealer)
}

// ReplaceSendHashRootTx puts a new root in place of one the configured keys
// do not open (ErrSendHashRoot), for a database whose key is lost for good.
// It refuses to replace one they open (ErrSendHashRootOpens), and writes
// nothing when the sealer could not try to open it: a key service not
// reached, or that refuses to decrypt while it still seals, says nothing
// about the root, and replacing a root that would still open forgets every
// send record. A root of the configured key service's own kind that it does
// not unwrap (secrets.ErrSealedElsewhere) is most likely under another KMS
// key or MAIL_ENV, which putting back opens: it is replaced only with
// kmsKeyLost, the operator's word that the key that sealed it is lost for
// good, and is otherwise ErrSendHashRootMayOpen.
//
// A send record made under the old root is no longer recognised: a key-less
// send repeated after the replacement is sent again, and one repeated with
// its idempotency key is refused as that key reused.
func ReplaceSendHashRootTx(ctx context.Context, tx *sql.Tx, sealer secrets.Sealer, kmsKeyLost bool) error {
	return sendHashRoot.replaceTx(ctx, tx, sealer, kmsKeyLost)
}
