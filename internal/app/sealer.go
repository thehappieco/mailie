package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/thehappieco/kit/kms"
	"github.com/thehappieco/kit/kms/awskms"

	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/secrets/kmssealer"
	"github.com/thehappieco/mailie/internal/store"
)

// NewSealer is the sealer the configuration describes: what seals the
// credentials and the send-hash root, and opens whatever any configured key
// sealed. Without MAIL_CREDENTIAL_KMS_KEY_ARN that is the keyring of
// MAIL_CREDENTIAL_KEY_HEX and MAIL_CREDENTIAL_PREVIOUS_KEYS, the self-hosted
// default. With it, the KMS sealer seals, and the keyring, when keys are
// given, only opens what it sealed before, until `rewrap-credentials` has
// moved every row; with MAIL_CREDENTIAL_SEALER=keyring beside it, the way
// back, the keyring seals and the KMS sealer only opens (NewSealerWith).
//
// The daemon and every command that opens a sealed value build theirs here.
// For a KMS key this is the one place the server's KMS client is made: the
// kit's awskms, with the EC2 instance role's credentials through IMDSv2 and
// nothing else (never the SDK's default chain, the environment or a profile,
// unlike the backup), checked at once with DescribeKey, which must show that
// exact ARN, enabled and symmetric. A key it cannot use stops the caller here,
// before anything is opened, naming the key and the cause.
func NewSealer(ctx context.Context, cfg config.Config) (secrets.Sealer, error) {
	arn := cfg.Credentials.KMSKeyARN
	if arn == "" {
		return NewSealerWith(cfg, nil)
	}
	wrapper, err := awskms.New(ctx, cfg.Credentials.KMSRegion, arn)
	if err != nil {
		return nil, fmt.Errorf("the credentials' KMS key %s (MAIL_CREDENTIAL_KMS_KEY_ARN) cannot be used: %w", arn, err)
	}
	return NewSealerWith(cfg, wrapper)
}

// NewSealerWith is NewSealer over a given key wrapper, which is nil exactly
// when the configuration names no KMS key: the KMS sealer under wrapper seals,
// beside the keyring's keys when there are any, for what they sealed before;
// or, with Credentials.KMSOpensOnly, the keyring seals and the KMS sealer
// only opens what it sealed, until a rewrap has moved it back.
// It exists so that a test runs the configured sealer over a fake wrapper;
// everything that runs on a server calls NewSealer, the only place the
// server's KMS credentials are chosen.
func NewSealerWith(cfg config.Config, wrapper kms.Wrapper) (secrets.Sealer, error) {
	creds := cfg.Credentials
	switch {
	case creds.KMSKeyARN == "" && wrapper != nil:
		return nil, errors.New("a key wrapper without MAIL_CREDENTIAL_KMS_KEY_ARN")
	case creds.KMSKeyARN == "":
		keyring, err := secrets.NewKeyring(creds.ActiveKeyID, creds.Keys)
		if err != nil {
			return nil, err
		}
		return keyring, nil
	case wrapper == nil:
		return nil, fmt.Errorf("no key wrapper for the credentials' KMS key %s", creds.KMSKeyARN)
	}
	sealer, err := kmssealer.New(wrapper, string(cfg.Env), creds.KMSKeyARN)
	if err != nil {
		return nil, err
	}
	if creds.KMSOpensOnly {
		keyring, err := secrets.NewKeyring(creds.ActiveKeyID, creds.Keys)
		if err != nil {
			return nil, err
		}
		return secrets.NewComposite(keyring, sealer), nil
	}
	if len(creds.Keys) == 0 {
		return sealer, nil
	}
	// The keyring only opens here. Its active id is the configured one when
	// MAIL_CREDENTIAL_KEY_HEX is given, and otherwise any of its keys: it
	// never seals, so which it would seal under does not matter.
	active := creds.ActiveKeyID
	if _, ok := creds.Keys[active]; !ok {
		active = slices.Min(slices.Collect(maps.Keys(creds.Keys)))
	}
	keyring, err := secrets.NewKeyring(active, creds.Keys)
	if err != nil {
		return nil, err
	}
	return secrets.NewComposite(sealer, keyring), nil
}

// openSendHashRoot opens the database's send-hash root for the daemon's
// start, and reports whether it made it now. A root the configured keys do
// not open means the key that sealed the credentials is not there either,
// and only then is the operator told what to do (ExplainSealed): any other
// failure (the database, a key service that could not be reached or refused
// the call) is no reason to, and replacing a root that would still open
// forgets every send record.
func openSendHashRoot(ctx context.Context, db *store.Store, sealer secrets.Sealer) ([]byte, bool, error) {
	root, created, err := db.SendHashRoot(ctx, sealer)
	if errors.Is(err, store.ErrSendHashRoot) {
		return nil, false, ExplainSealed(err, sealer)
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening the send-hash root: %w", err)
	}
	return root, created, nil
}

// openKDFSaltKey opens the database's salt key for the daemon's start, and
// reports whether it made it now, as openSendHashRoot does the root.
func openKDFSaltKey(ctx context.Context, db *store.Store, sealer secrets.Sealer) ([]byte, bool, error) {
	key, created, err := db.KDFSaltKey(ctx, sealer)
	if errors.Is(err, store.ErrKDFSaltKey) {
		return nil, false, ExplainSealed(err, sealer)
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening the salt key: %w", err)
	}
	return key, created, nil
}

// ExplainSealed adds to an error about a sealed value that does not open
// with the configured sealer what the operator can do about it; the daemon's
// start and `rewrap-credentials` say the same. Any other error is returned
// as it is: a sealer that could not try says nothing about the value, and
// nothing tells the operator to replace what may still open.
//
// A value of the configured KMS key's own kind that it does not unwrap
// (secrets.ErrSealedElsewhere) was most likely sealed under another KMS key
// or another MAIL_ENV, which nothing given beside the key can open and no
// rewrap can move: the way back is the settings that sealed it, and only a
// key lost for good is a reason to replace it. Any other value that does not
// open needs the key that sealed it given as well.
//
// The daemon opens the send-hash root first and the salt key after it, both
// sealed by the same sealer: a root whose key is lost leaves, as a rule, a
// salt key that does not open either, so the root's advice replaces both in
// one run, which refuses to replace a salt key that still opens.
func ExplainSealed(err error, sealer secrets.Sealer) error {
	salt := errors.Is(err, store.ErrKDFSaltKey)
	replace, after := "--new-send-hash-root --new-salt-key", "replaces the root and the salt key (leave out "+
		"--new-salt-key if the salt key still opens: it is never replaced then), and every mailbox has to be "+
		"authorized again"
	if salt {
		replace, after = "--new-salt-key", "replaces it, and each account moves to its new salt at its next sign-in"
	}
	switch {
	case errors.Is(err, secrets.ErrSealedElsewhere):
		return fmt.Errorf("%w; configured to seal with %s, which does not unwrap it: it was sealed under "+
			"another MAIL_CREDENTIAL_KMS_KEY_ARN or another MAIL_ENV. Set both back to the values that sealed it "+
			"and start again; `mailserver rewrap-credentials` cannot move it. Only if that KMS key is lost for "+
			"good, `mailserver rewrap-credentials %s --kms-key-lost`, with the daemon stopped, %s "+
			"(docs/self-hosting.md, \"Credentials under AWS KMS\")", err, sealer.Describe(), replace, after)
	case salt:
		return fmt.Errorf("%w; configured to seal with %s: give it the key the salt key was sealed with "+
			"as well, then run `mailserver rewrap-credentials`; if that key is lost for good, `mailserver "+
			"rewrap-credentials %s`, with the daemon stopped, %s", err, sealer.Describe(), replace, after)
	case errors.Is(err, store.ErrSendHashRoot):
		return fmt.Errorf("%w; configured to seal with %s: give it the key the root was sealed with "+
			"as well, then run `mailserver rewrap-credentials`; if that key is lost for good, `mailserver "+
			"rewrap-credentials %s`, with the daemon stopped, %s", err, sealer.Describe(), replace, after)
	}
	return err
}
