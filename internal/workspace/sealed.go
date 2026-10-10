package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
)

// Mailbox keys and the grants sealed with them (docs/key-scheme.md, sections
// 8, 9 and 12.11 to 12.15).
//
// A mailbox's key pair is made by a browser, never here. The server stores
// its public half and its namespace (mailbox_keys, a KeyPair), and the grants
// a browser sealed of its private key, one per person and epoch
// (mailbox_grants, a SealedGrant), which it checks only for their shape and
// cannot open (section 9.3). Who reads a mailbox follows from both, by the one
// rule of store.ReaderSQL. What each write checks of the data is here; who
// may ask for it — that the caller sees the mailbox, and their fresh step-up
// (section 11) — is the service's Check, run first in the same transaction.
//
// Elsewhere in this package "key" means an API key (keys.go, MailboxKey); a
// mailbox's own key pair is a KeyPair.

// KeyPair is a mailbox's key pair at one epoch, as the server holds it: the
// public half and the namespace, never the private key, which only grants
// carry, sealed.
type KeyPair struct {
	AccountID string
	// Epoch is 1 for the mailbox's first key pair and one more for each next
	// one (section 3.3); the highest is the current one.
	Epoch     int
	PublicKey []byte
	// Namespace is the mailbox's, the same for every epoch (section 3.2).
	Namespace string
	// CreatedBy is the person whose browser made it; empty once they are
	// deleted.
	CreatedBy string
	CreatedAt time.Time
}

// SealedGrant is a mailbox's private key at one epoch sealed to one person's
// account public key (section 9.1): 88 bytes the server stores and hands back
// to that person, and cannot open.
type SealedGrant struct {
	AccountID   string
	WorkspaceID string
	UserID      string
	Epoch       int
	Grant       []byte
	// GrantedBy is who sent it, "usr_…": the person themself when it came
	// with a key they wrote; empty once that person is deleted.
	GrantedBy string
	CreatedAt time.Time
}

// LinkKey is what the browser that links a mailbox sends with the link
// (section 12.11): the public half of the key pair it made, the namespace it
// drew, and the linker's own grant at epoch 1.
type LinkKey struct {
	PublicKey []byte
	Namespace string
	Grant     []byte
}

// FirstKey is the first key pair of a mailbox that has none (section 12.14):
// its public half, its namespace, and a grant at epoch 1 for its writer and
// for every other active member who holds read and has an account key.
type FirstKey struct {
	PublicKey []byte
	Namespace string
	Grants    []GrantTo
}

// GrantTo is one grant of those a key pair is written with: to whom, the 88
// bytes, and the account public key they were sealed to, which must be that
// person's now (ErrSealedToAnother).
type GrantTo struct {
	UserID   string
	Grant    []byte
	SealedTo []byte
}

// Sealed is a grant as a browser sends it for someone else: the 88 bytes, and
// the account public key it says it sealed them to. The server cannot tell
// what the bytes were sealed to (docs/key-scheme.md section 9.3), but it
// refuses a grant whose stated key is not its recipient's account public key
// now (ErrSealedToAnother): what a console that read the recipient before
// their reset would send, a grant that would count them as a reader and never
// open.
type Sealed struct {
	Grant []byte
	// SealedTo is the recipient's account public key the browser sealed
	// Grant to.
	SealedTo []byte
}

// NextKey is a new key pair for a personal mailbox (section 12.12): at the
// epoch after its current one, with its person's grant. The namespace is the
// mailbox's, which never changes, and is not sent.
type NextKey struct {
	Epoch     int
	PublicKey []byte
	Grant     []byte
}

// Recipient is a person as a console needs them to seal a grant to them or to
// say who can hand the key on: who they are, and what a grant to them is
// sealed to and bound by.
type Recipient struct {
	UserID string
	Email  string
	Name   string
	SealID string
	// PublicKey is their account public key; nil until they enrol.
	PublicKey []byte
}

// Errors of mailbox keys and sealed grants. A public key, a namespace or a
// grant outside its shape is the key scheme's own: keyscheme.ErrPublicKey,
// keyscheme.ErrBinding and keyscheme.ErrShape.
var (
	// ErrKeyless is a mailbox that has no key: it is read by the flag alone
	// (section 12.14), and nothing is sealed for it.
	ErrKeyless = errors.New("workspace: the mailbox has no key")
	// ErrKeyed is a first key for a mailbox that has one.
	ErrKeyed = errors.New("workspace: the mailbox already has a key")
	// ErrNamespaceTaken is a namespace another mailbox uses.
	ErrNamespaceTaken = errors.New("workspace: another mailbox uses that namespace")
	// ErrEpoch is a grant at another epoch than the mailbox key's current
	// one, or a new key pair at another than the next.
	ErrEpoch = errors.New("workspace: that is not the mailbox key's epoch")
	// ErrSealedGrantExists is a grant for a person who already holds one at
	// that epoch: a grant is written once.
	ErrSealedGrantExists = errors.New("workspace: that person already holds the mailbox's key at that epoch")
	// ErrNotEnrolled is a person with no account public key: nothing is
	// sealed to them, and they seal nothing, until they enrol.
	ErrNotEnrolled = errors.New("workspace: that person has no account key yet")
	// ErrGrantsIncomplete is a first key whose grants are not exactly its
	// writer's and one for every other active member who holds read and has
	// an account key: a console that raced someone's enrolment reloads.
	ErrGrantsIncomplete = errors.New("workspace: a first key comes with a grant for everyone who holds read and has an account key, and no other")
	// ErrSealedGrantNeeded is read given on a mailbox that has a key, to a
	// person with an account key, without their grant.
	ErrSealedGrantNeeded = errors.New("workspace: read on a mailbox with a key comes with the person's grant")
	// ErrSealedGrantUnwanted is a grant sent with a change that does not
	// give read: a member who holds the flag gets the key by SupplyGrant.
	ErrSealedGrantUnwanted = errors.New("workspace: a grant comes only with the read it gives")
	// ErrNotReader is a person who does not read the mailbox writing its
	// key or handing it on: read and its key pass only from a reader.
	ErrNotReader = errors.New("workspace: that person does not read the mailbox")
	// ErrNoReadFlag is the key supplied to a person who does not hold read
	// on the mailbox: supplying it gives nobody read.
	ErrNoReadFlag = errors.New("workspace: that person does not hold read on the mailbox")
	// ErrTeamKey is a new key pair for a team mailbox, which is never given
	// one (section 12.12): whoever made it would hold its only grant.
	ErrTeamKey = errors.New("workspace: a team mailbox is never given a new key")
	// ErrSealedToAnother is a grant whose browser says it sealed it to
	// another account public key than its recipient's now: one read before
	// they were reset, whose grant would never open.
	ErrSealedToAnother = errors.New("workspace: the grant was sealed to another account key than the person's")
)

// requireSealedTo refuses a grant sealed, by what its browser says, to
// another account public key than pub, its recipient's now
// (ErrSealedToAnother).
func requireSealedTo(pub, sealedTo []byte) error {
	if len(pub) == 0 || !bytes.Equal(pub, sealedTo) {
		return ErrSealedToAnother
	}
	return nil
}

// grantEpoch is the epoch a grant names in its header (section 9.1), once
// its shape is a grant's at that epoch; keyscheme.ErrShape otherwise.
func grantEpoch(grant []byte) (int, error) {
	if len(grant) != keyscheme.GrantLen {
		return 0, fmt.Errorf("%w: a grant is %d bytes", keyscheme.ErrShape, keyscheme.GrantLen)
	}
	epoch := int(binary.BigEndian.Uint16(grant[5:7]))
	if epoch < keyscheme.MinEpoch {
		return 0, fmt.Errorf("%w: a grant's epoch is at least %d", keyscheme.ErrShape, keyscheme.MinEpoch)
	}
	if err := keyscheme.CheckGrantShape(grant, epoch); err != nil {
		return 0, err
	}
	return epoch, nil
}

// checkGrantAt refuses a grant that does not have a grant's shape
// (keyscheme.ErrShape), or has it at another epoch than epoch (ErrEpoch).
func checkGrantAt(grant []byte, epoch int) error {
	got, err := grantEpoch(grant)
	if err != nil {
		return err
	}
	if got != epoch {
		return fmt.Errorf("%w: the grant is at epoch %d, the mailbox key at %d", ErrEpoch, got, epoch)
	}
	return nil
}

// checkEpoch refuses an epoch outside a mailbox key's range.
func checkEpoch(epoch int) error {
	if epoch < keyscheme.MinEpoch || epoch > keyscheme.MaxEpoch {
		return fmt.Errorf("%w: an epoch is %d to %d", keyscheme.ErrBinding, keyscheme.MinEpoch, keyscheme.MaxEpoch)
	}
	return nil
}

// checkKeyPair refuses a public key the server does not store, and a
// namespace outside its spelling.
func checkKeyPair(pub []byte, namespace string) error {
	if err := keyscheme.CheckPublicKey(pub); err != nil {
		return err
	}
	if !keyscheme.ValidNamespace(namespace) {
		return fmt.Errorf("%w: a namespace is a lowercase UUIDv4", keyscheme.ErrBinding)
	}
	return nil
}

// CheckLinkKey checks what a link carries, before anything is dialled or
// stored: a public key the server stores, a namespace in its spelling, and a
// grant's shape at epoch 1. WriteLinkKeyTx checks them again.
func CheckLinkKey(k LinkKey) error {
	if err := checkKeyPair(k.PublicKey, k.Namespace); err != nil {
		return err
	}
	return checkGrantAt(k.Grant, keyscheme.MinEpoch)
}

const keyPairColumns = `k.account_id, k.epoch, k.public_key, k.namespace, k.created_by, k.created_at`

func scanKeyPair(row rowScanner) (KeyPair, error) {
	var (
		k       KeyPair
		created int64
	)
	if err := row.Scan(&k.AccountID, &k.Epoch, &k.PublicKey, &k.Namespace, &k.CreatedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KeyPair{}, ErrKeyless
		}
		return KeyPair{}, fmt.Errorf("workspace: read the mailbox key: %w", err)
	}
	k.CreatedAt = unix(created)
	return k, nil
}

// currentKeyOn reads a mailbox's key pair at its current epoch; ErrKeyless
// when it has none.
func currentKeyOn(ctx context.Context, q querier, accountID string) (KeyPair, error) {
	return scanKeyPair(q.QueryRowContext(ctx, `SELECT `+keyPairColumns+` FROM mailbox_keys k
		WHERE k.account_id = ? ORDER BY k.epoch DESC LIMIT 1`, accountID))
}

// CurrentKey reads a mailbox's key pair at its current epoch; ErrKeyless
// when it has none.
func (r *Repository) CurrentKey(ctx context.Context, accountID string) (KeyPair, error) {
	return currentKeyOn(ctx, r.store.Reader(), accountID)
}

// CurrentKeyTx is CurrentKey inside the caller's transaction.
func CurrentKeyTx(ctx context.Context, tx *sql.Tx, accountID string) (KeyPair, error) {
	return currentKeyOn(ctx, tx, accountID)
}

// CurrentKeys reads the key pair at its current epoch of each mailbox named
// that has one; a mailbox without a key is absent.
func (r *Repository) CurrentKeys(ctx context.Context, accountIDs []string) (map[string]KeyPair, error) {
	out := make(map[string]KeyPair, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT `+keyPairColumns+` FROM mailbox_keys k
		WHERE k.account_id IN (SELECT value FROM json_each(?))
		  AND k.epoch = `+store.CurrentEpochSQL("k.account_id"), string(list))
	if err != nil {
		return nil, fmt.Errorf("workspace: read the mailbox keys: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		k, err := scanKeyPair(rows)
		if err != nil {
			return nil, err
		}
		out[k.AccountID] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the mailbox keys: %w", err)
	}
	return out, nil
}

// WaitingForKey reports, of the mailboxes named, those on which a person
// waits for the key (store.WaitingSQL): they hold read as an active member
// active on the instance, the mailbox has a key, and they hold no grant at
// its current epoch. They see its card and read nothing of it yet.
func (r *Repository) WaitingForKey(ctx context.Context, userID string, accountIDs []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(accountIDs) == 0 || userID == "" {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	ids, err := listIDs(ctx, r.store.Reader(), `SELECT g.account_id FROM mailbox_access g
		WHERE g.user_id = ? AND g.account_id IN (SELECT value FROM json_each(?)) AND `+store.WaitingSQL("g"),
		userID, string(list))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// KeyState is a mailbox's key as one person's console needs it
// (docs/key-scheme.md sections 12.13 and 12.14), read in one transaction.
// Which of it the person may be shown is the service's to decide.
type KeyState struct {
	// Key is the mailbox's key pair at its current epoch; nil for a mailbox
	// without a key.
	Key *KeyPair
	// Grant is the person's own grant at the current epoch; nil when they
	// hold none.
	Grant []byte
	// HoldsFlag is the person holding read on the mailbox as an active
	// member, active on the instance (store.FlagHolderSQL).
	HoldsFlag bool
	// Reads is the person reading it now, by the one rule.
	Reads bool
	// Waiting are, on a mailbox that has a key, the active members who
	// hold read and have an account key but no grant at its current epoch,
	// by address: to whom a reader may supply the key.
	Waiting []Recipient
	// Suppliers are who read the mailbox now, by address: who may supply
	// the key to someone waiting.
	Suppliers []Recipient
	// KeylessReaders are, on a mailbox without a key, the active members
	// other than the person who hold read and have an account key, by
	// address: to whom its first key is sealed beside its writer.
	KeylessReaders []Recipient
}

// KeyState reads a mailbox's key as userID's console needs it; ErrNoMailbox
// for a mailbox nobody knows.
func (r *Repository) KeyState(ctx context.Context, accountID, userID string) (KeyState, error) {
	tx, err := r.store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return KeyState{}, fmt.Errorf("workspace: read the mailbox key: %w", err)
	}
	//nolint:errcheck // a read-only transaction: nothing to keep or undo
	defer func() { _ = tx.Rollback() }()
	var exists int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, accountID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return KeyState{}, ErrNoMailbox
	case err != nil:
		return KeyState{}, fmt.Errorf("workspace: read the mailbox: %w", err)
	}
	var out KeyState
	key, err := currentKeyOn(ctx, tx, accountID)
	switch {
	case errors.Is(err, ErrKeyless):
	case err != nil:
		return KeyState{}, err
	default:
		out.Key = &key
		g, err := sealedGrantOn(ctx, tx, accountID, userID, key.Epoch)
		switch {
		case errors.Is(err, ErrNoGrant):
		case err != nil:
			return KeyState{}, err
		default:
			out.Grant = g.Grant
		}
	}
	if out.HoldsFlag, err = holdsReadFlag(ctx, tx, accountID, userID); err != nil {
		return KeyState{}, err
	}
	if out.Reads, err = readsNow(ctx, tx, accountID, userID); err != nil {
		return KeyState{}, err
	}
	if out.Suppliers, err = recipientsOn(ctx, tx, accountID, store.ReaderSQL("g")); err != nil {
		return KeyState{}, err
	}
	if out.Key != nil {
		out.Waiting, err = recipientsOn(ctx, tx, accountID, store.WaitingSQL("g")+` AND u.public_key IS NOT NULL`)
	} else {
		out.KeylessReaders, err = recipientsOn(ctx, tx, accountID,
			store.FlagHolderSQL("g")+` AND u.public_key IS NOT NULL AND g.user_id <> ?`, userID)
	}
	if err != nil {
		return KeyState{}, err
	}
	return out, nil
}

// recipientsOn lists the people of a mailbox's grants, aliased g, with their
// person aliased u, who meet where, by address.
func recipientsOn(ctx context.Context, q querier, accountID, where string, args ...any) ([]Recipient, error) {
	rows, err := q.QueryContext(ctx, `SELECT u.id, u.email, u.name, u.seal_id, u.public_key
		  FROM mailbox_access g JOIN users u ON u.id = g.user_id
		 WHERE g.account_id = ? AND `+where+` ORDER BY u.email, u.id`, append([]any{accountID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("workspace: list the mailbox's people: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Recipient
	for rows.Next() {
		var p Recipient
		if err := rows.Scan(&p.UserID, &p.Email, &p.Name, &p.SealID, &p.PublicKey); err != nil {
			return nil, fmt.Errorf("workspace: list the mailbox's people: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list the mailbox's people: %w", err)
	}
	return out, nil
}

const sealedGrantColumns = `s.account_id, s.workspace_id, s.user_id, s.epoch, s.grant, s.granted_by, s.created_at`

func scanSealedGrant(row rowScanner) (SealedGrant, error) {
	var (
		g       SealedGrant
		created int64
	)
	if err := row.Scan(&g.AccountID, &g.WorkspaceID, &g.UserID, &g.Epoch, &g.Grant, &g.GrantedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SealedGrant{}, ErrNoGrant
		}
		return SealedGrant{}, fmt.Errorf("workspace: read the grant: %w", err)
	}
	g.CreatedAt = unix(created)
	return g, nil
}

// sealedGrantOn reads a person's grant on a mailbox at one epoch; ErrNoGrant
// when there is none.
func sealedGrantOn(ctx context.Context, q querier, accountID, userID string, epoch int) (SealedGrant, error) {
	return scanSealedGrant(q.QueryRowContext(ctx, `SELECT `+sealedGrantColumns+` FROM mailbox_grants s
		WHERE s.account_id = ? AND s.user_id = ? AND s.epoch = ?`, accountID, userID, epoch))
}

// publicKeyOf reads a person's account public key: nil until they enrol.
func publicKeyOf(ctx context.Context, q querier, userID string) ([]byte, error) {
	var pub []byte
	switch err := q.QueryRowContext(ctx, `SELECT public_key FROM users WHERE id = ?`, userID).Scan(&pub); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoSuchPerson
	case err != nil:
		return nil, fmt.Errorf("workspace: read the person's account key: %w", err)
	}
	return pub, nil
}

// requireEnrolled refuses a person with no account public key
// (ErrNotEnrolled).
func requireEnrolled(ctx context.Context, q querier, userID string) error {
	pub, err := publicKeyOf(ctx, q, userID)
	if err != nil {
		return err
	}
	if pub == nil {
		return ErrNotEnrolled
	}
	return nil
}

// holdsReadFlag reports whether a person holds read on a mailbox and it
// counts (store.FlagHolderSQL), whatever the key.
func holdsReadFlag(ctx context.Context, q querier, accountID, userID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND g.user_id = ? AND `+store.FlagHolderSQL("g"), accountID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read whether the person holds read: %w", err)
	}
	return n > 0, nil
}

// requireNamespaceFreeTx refuses a namespace another mailbox uses
// (ErrNamespaceTaken). The schema refuses it too.
func requireNamespaceFreeTx(ctx context.Context, tx *sql.Tx, accountID, namespace string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_keys WHERE namespace = ? AND account_id <> ?`,
		namespace, accountID).Scan(&n); err != nil {
		return fmt.Errorf("workspace: look for the namespace: %w", err)
	}
	if n > 0 {
		return ErrNamespaceTaken
	}
	return nil
}

// requireKeylessTx refuses a mailbox that has a key (ErrKeyed).
func requireKeylessTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	_, err := currentKeyOn(ctx, tx, accountID)
	switch {
	case errors.Is(err, ErrKeyless):
		return nil
	case err != nil:
		return err
	}
	return ErrKeyed
}

func insertKeyPairTx(ctx context.Context, tx *sql.Tx, accountID string, epoch int, pub []byte, namespace, createdBy string,
	now int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, accountID, epoch, pub, namespace, createdBy, now); err != nil {
		return fmt.Errorf("workspace: write the mailbox key: %w", err)
	}
	return nil
}

func insertSealedGrantTx(ctx context.Context, tx *sql.Tx, accountID, workspaceID, userID string, epoch int, grant []byte,
	grantedBy string, now int64,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch, grant, granted_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, accountID, workspaceID, userID, epoch, grant, grantedBy, now)
	switch {
	case store.IsUnique(err):
		return ErrSealedGrantExists
	case err != nil:
		return fmt.Errorf("workspace: write the grant: %w", err)
	}
	return nil
}

// dropSealedGrantsTx deletes a person's grants on a mailbox, every epoch's:
// what taking their read takes with the flag (section 12.13).
func dropSealedGrantsTx(ctx context.Context, tx *sql.Tx, accountID, userID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_grants WHERE account_id = ? AND user_id = ?`,
		accountID, userID); err != nil {
		return fmt.Errorf("workspace: delete the person's grants: %w", err)
	}
	return nil
}

// DropSealedGrantsOfTx deletes every grant a person holds, on every mailbox
// and at every epoch, inside the caller's transaction: what disabling them on
// the instance and resetting their account key take (docs/key-scheme.md
// sections 12.6 and 12.13). Their flags stay; on a mailbox that has a key
// they wait for it again.
func DropSealedGrantsOfTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_grants WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("workspace: delete the person's grants: %w", err)
	}
	return nil
}

// sealedWithReadTx decides, inside SetGrantSealed's transaction, what a
// change of a person's flags does with a grant (docs/key-scheme.md section
// 9.3): the epoch to write the one sent at, or 0 when none is written.
// addsRead is the change giving the person read.
func sealedWithReadTx(ctx context.Context, tx *sql.Tx, accountID, userID string, addsRead bool, sealed *Sealed) (int, error) {
	if sealed == nil && !addsRead {
		return 0, nil
	}
	key, err := currentKeyOn(ctx, tx, accountID)
	keyed := true
	switch {
	case errors.Is(err, ErrKeyless):
		keyed = false
	case err != nil:
		return 0, err
	}
	pub, err := publicKeyOf(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	if sealed == nil {
		if keyed && pub != nil {
			return 0, ErrSealedGrantNeeded
		}
		// A mailbox without a key is read by the flag; a person without an
		// account key holds it and waits for the key.
		return 0, nil
	}
	switch {
	case !addsRead:
		return 0, ErrSealedGrantUnwanted
	case !keyed:
		return 0, ErrKeyless
	case pub == nil:
		return 0, ErrNotEnrolled
	}
	if err := requireSealedTo(pub, sealed.SealedTo); err != nil {
		return 0, err
	}
	if err := checkGrantAt(sealed.Grant, key.Epoch); err != nil {
		return 0, err
	}
	switch _, err := sealedGrantOn(ctx, tx, accountID, userID, key.Epoch); {
	case err == nil:
		return 0, ErrSealedGrantExists
	case !errors.Is(err, ErrNoGrant):
		return 0, err
	}
	return key.Epoch, nil
}

// WriteLinkKeyTx writes a mailbox's first key pair, at epoch 1, and its
// linker's own grant, inside the transaction that creates the mailbox and
// after the linker's flags (GrantLinkTx): the request that links a mailbox
// carries both (docs/key-scheme.md sections 8 and 12.11). The mailbox has a
// linker (an operator mailbox has no key: ErrOperator) who holds read on it
// and has an account key (ErrNotEnrolled), and no key yet (ErrKeyed); the
// namespace is one no other mailbox uses (ErrNamespaceTaken); and the key
// pair and the grant pass CheckLinkKey.
func WriteLinkKeyTx(ctx context.Context, tx *sql.Tx, accountID, linker string, key LinkKey, now time.Time) error {
	if linker == "" {
		return ErrOperator
	}
	if err := CheckLinkKey(key); err != nil {
		return err
	}
	mb, err := mailboxTx(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if mb.kind == KindOperator {
		return ErrOperator
	}
	if err := requireKeylessTx(ctx, tx, accountID); err != nil {
		return err
	}
	if err := requireEnrolled(ctx, tx, linker); err != nil {
		return err
	}
	switch holds, err := holdsReadFlag(ctx, tx, accountID, linker); {
	case err != nil:
		return err
	case !holds:
		return ErrNotReader
	}
	if err := requireNamespaceFreeTx(ctx, tx, accountID, key.Namespace); err != nil {
		return err
	}
	at := now.UTC().Unix()
	if err := insertKeyPairTx(ctx, tx, accountID, keyscheme.MinEpoch, key.PublicKey, key.Namespace, linker, at); err != nil {
		return err
	}
	return insertSealedGrantTx(ctx, tx, accountID, mb.workspaceID, linker, keyscheme.MinEpoch, key.Grant, linker, at)
}

// WriteFirstKey writes the first key pair of a mailbox that has none, at
// epoch 1, with a grant for its writer and one for every other active member
// who holds read and has an account key, in one transaction
// (docs/key-scheme.md section 12.14). The writer reads the mailbox now — by
// the flag, since it has no key — and has an account key: nobody else writes
// a mailbox's first key, so that an owner or an admin who does not read a
// team mailbox cannot take it by keying it.
//
// Refused: a mailbox that has a key (ErrKeyed); an operator mailbox
// (ErrOperator); a writer who does not read it (ErrNotReader) or has no
// account key (ErrNotEnrolled); a namespace another mailbox uses
// (ErrNamespaceTaken); grants that are not exactly one for each of those
// people (ErrGrantsIncomplete), or one sealed, by what the browser says, to
// another account public key than its recipient's now (ErrSealedToAnother);
// and what CheckLinkKey refuses of the key pair and of each grant. The
// writer's fresh step-up is the Check's.
func (r *Repository) WriteFirstKey(ctx context.Context, accountID, writerID string, key FirstKey, check Check) (KeyPair, error) {
	if err := checkKeyPair(key.PublicKey, key.Namespace); err != nil {
		return KeyPair{}, err
	}
	sent := make([]string, 0, len(key.Grants))
	for _, g := range key.Grants {
		if err := checkGrantAt(g.Grant, keyscheme.MinEpoch); err != nil {
			return KeyPair{}, err
		}
		sent = append(sent, g.UserID)
	}
	slices.Sort(sent)
	if len(slices.Compact(slices.Clone(sent))) != len(sent) {
		return KeyPair{}, fmt.Errorf("%w: two grants for one person", ErrGrantsIncomplete)
	}
	now := r.now().UTC().Truncate(time.Second)
	var out KeyPair
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		if mb.kind == KindOperator {
			return ErrOperator
		}
		if err := requireKeylessTx(ctx, tx, accountID); err != nil {
			return err
		}
		switch reads, err := readsNow(ctx, tx, accountID, writerID); {
		case err != nil:
			return err
		case !reads:
			return ErrNotReader
		}
		if err := requireEnrolled(ctx, tx, writerID); err != nil {
			return err
		}
		if err := requireNamespaceFreeTx(ctx, tx, accountID, key.Namespace); err != nil {
			return err
		}
		// Who holds read and could open a grant: the writer among them.
		holders, err := recipientsOn(ctx, tx, accountID, store.FlagHolderSQL("g")+` AND u.public_key IS NOT NULL`)
		if err != nil {
			return err
		}
		want := make([]string, 0, len(holders))
		keys := make(map[string][]byte, len(holders))
		for _, h := range holders {
			want = append(want, h.UserID)
			keys[h.UserID] = h.PublicKey
		}
		slices.Sort(want)
		if !slices.Equal(want, sent) {
			return fmt.Errorf("%w: %d sent for %d people", ErrGrantsIncomplete, len(sent), len(want))
		}
		for _, g := range key.Grants {
			if err := requireSealedTo(keys[g.UserID], g.SealedTo); err != nil {
				return err
			}
		}
		at := now.Unix()
		if err := insertKeyPairTx(ctx, tx, accountID, keyscheme.MinEpoch, key.PublicKey, key.Namespace, writerID, at); err != nil {
			return err
		}
		for _, g := range key.Grants {
			if err := insertSealedGrantTx(ctx, tx, accountID, mb.workspaceID, g.UserID, keyscheme.MinEpoch, g.Grant,
				writerID, at); err != nil {
				return err
			}
		}
		out, err = currentKeyOn(ctx, tx, accountID)
		return err
	})
	if err != nil {
		return KeyPair{}, err
	}
	return out, nil
}

// WriteNextKey writes a personal mailbox a new key pair at the epoch after
// its current one, with its person's grant, and deletes every grant at an
// older epoch, in one transaction (docs/key-scheme.md section 12.12): when
// they can no longer open its grant (after a reset), or at will. The
// namespace is the mailbox's own, which never changes.
//
// Only the person of a personal mailbox writes it one: a team mailbox is
// never given a new key (ErrTeamKey), and anyone else's mailbox is
// ErrNoMailbox. Refused too: a mailbox without a key (ErrKeyless: its first
// is WriteFirstKey's), an epoch other than the next (ErrEpoch), a person who
// does not hold read on it (ErrNotReader) or has no account key
// (ErrNotEnrolled), and a key pair or a grant CheckLinkKey would refuse, the
// grant at that epoch. The person's fresh step-up is the Check's.
func (r *Repository) WriteNextKey(ctx context.Context, accountID, personID string, next NextKey, check Check) (KeyPair, error) {
	if err := checkEpoch(next.Epoch); err != nil {
		return KeyPair{}, err
	}
	if err := keyscheme.CheckPublicKey(next.PublicKey); err != nil {
		return KeyPair{}, err
	}
	if err := checkGrantAt(next.Grant, next.Epoch); err != nil {
		return KeyPair{}, err
	}
	now := r.now().UTC().Truncate(time.Second)
	var out KeyPair
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		switch {
		case mb.kind == KindTeam:
			return ErrTeamKey
		case mb.kind != KindPersonal || mb.personID != personID || personID == "":
			return ErrNoMailbox
		}
		current, err := currentKeyOn(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if next.Epoch != current.Epoch+1 {
			return fmt.Errorf("%w: the next key is at epoch %d", ErrEpoch, current.Epoch+1)
		}
		switch holds, err := holdsReadFlag(ctx, tx, accountID, personID); {
		case err != nil:
			return err
		case !holds:
			return ErrNotReader
		}
		if err := requireEnrolled(ctx, tx, personID); err != nil {
			return err
		}
		at := now.Unix()
		if err := insertKeyPairTx(ctx, tx, accountID, next.Epoch, next.PublicKey, current.Namespace, personID, at); err != nil {
			return err
		}
		if err := insertSealedGrantTx(ctx, tx, accountID, mb.workspaceID, personID, next.Epoch, next.Grant, personID, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_grants WHERE account_id = ? AND epoch < ?`,
			accountID, next.Epoch); err != nil {
			return fmt.Errorf("workspace: delete the grants of older epochs: %w", err)
		}
		out, err = currentKeyOn(ctx, tx, accountID)
		return err
	})
	if err != nil {
		return KeyPair{}, err
	}
	return out, nil
}

// SupplyGrant writes the grant that hands a mailbox's key to a member who
// holds read on it and has no grant at its current epoch (docs/key-scheme.md
// section 12.13): they had not enrolled when the key was written, or were
// reset since. Any person who reads the mailbox may (ErrNotReader
// otherwise), owner, admin or member: it gives nobody read who was not given
// it.
//
// The recipient is an active member of the mailbox's workspace
// (ErrNotMember) who holds the read flag (ErrNoReadFlag), has an account key
// (ErrNotEnrolled) and no grant at the current epoch
// (ErrSealedGrantExists); the mailbox has a key (ErrKeyless); the grant has
// a grant's shape (keyscheme.ErrShape) at the current epoch, which epoch
// names (ErrEpoch); and its browser sealed it, by what it says, to the
// recipient's account public key now (ErrSealedToAnother). The giver's
// fresh step-up is the Check's.
func (r *Repository) SupplyGrant(ctx context.Context, accountID, recipientID, giverID string, epoch int, sealed Sealed,
	check Check,
) (SealedGrant, error) {
	if err := checkEpoch(epoch); err != nil {
		return SealedGrant{}, err
	}
	if err := checkGrantAt(sealed.Grant, epoch); err != nil {
		return SealedGrant{}, err
	}
	now := r.now().UTC().Truncate(time.Second)
	var out SealedGrant
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		current, err := currentKeyOn(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if epoch != current.Epoch {
			return fmt.Errorf("%w: the mailbox key is at epoch %d", ErrEpoch, current.Epoch)
		}
		switch reads, err := readsNow(ctx, tx, accountID, giverID); {
		case err != nil:
			return err
		case !reads:
			return ErrNotReader
		}
		m, err := MemberTx(ctx, tx, mb.workspaceID, recipientID)
		if errors.Is(err, ErrNotMember) || (err == nil && !m.Active()) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		switch holds, err := holdsReadFlag(ctx, tx, accountID, recipientID); {
		case err != nil:
			return err
		case !holds:
			return ErrNoReadFlag
		}
		if m.PublicKey == nil {
			return ErrNotEnrolled
		}
		if err := requireSealedTo(m.PublicKey, sealed.SealedTo); err != nil {
			return err
		}
		switch _, err := sealedGrantOn(ctx, tx, accountID, recipientID, epoch); {
		case err == nil:
			return ErrSealedGrantExists
		case !errors.Is(err, ErrNoGrant):
			return err
		}
		if err := insertSealedGrantTx(ctx, tx, accountID, mb.workspaceID, recipientID, epoch, sealed.Grant, giverID,
			now.Unix()); err != nil {
			return err
		}
		out, err = sealedGrantOn(ctx, tx, accountID, recipientID, epoch)
		return err
	})
	if err != nil {
		return SealedGrant{}, err
	}
	return out, nil
}
