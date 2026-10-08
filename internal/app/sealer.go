package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
)

// NewSealer is the sealer the configuration describes: what seals the
// credentials and the send-hash root, and opens whatever any configured key
// sealed. Today that is the keyring of MAIL_CREDENTIAL_KEY_HEX and
// MAIL_CREDENTIAL_PREVIOUS_KEYS, the self-hosted default.
//
// The daemon and every command that opens a sealed value build theirs here,
// so another kind of sealer is added in this one place, as the active one of
// a secrets.Composite beside the keyring until every row is re-sealed. It
// takes a context for that kind, which may need one to reach its key service.
func NewSealer(_ context.Context, cfg config.Config) (secrets.Sealer, error) {
	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		return nil, err
	}
	return keyring, nil
}

// openSendHashRoot opens the database's send-hash root for the daemon's
// start, and reports whether it made it now. A root the configured keys do
// not open means the key that sealed the credentials is not there either,
// and only then is the operator told how to give it back or replace the
// root: any other failure (the database, a key service that could not be
// reached or refused the call) is no reason to, and replacing a root that
// would still open forgets every send record.
func openSendHashRoot(ctx context.Context, db *store.Store, sealer secrets.Sealer) ([]byte, bool, error) {
	root, created, err := db.SendHashRoot(ctx, sealer)
	if errors.Is(err, store.ErrSendHashRoot) {
		return nil, false, fmt.Errorf("%w; the daemon seals with %s: give it the key the root was sealed with "+
			"as well, then run `mailserver rewrap-credentials`; if that key is lost for good, `mailserver "+
			"rewrap-credentials --new-send-hash-root`, with the daemon stopped, replaces the root, and every "+
			"mailbox has to be authorized again", err, sealer.Describe())
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening the send-hash root: %w", err)
	}
	return root, created, nil
}
