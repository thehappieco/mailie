package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Mailbox keys and the grants sealed with them (docs/key-scheme.md, sections
// 8, 9 and 12.11 to 12.15).
//
// A browser makes a mailbox's key pair and seals its private key to people
// (grants); the server stores the public half, the namespace and the grants,
// checks their shapes and who may send them, and never makes, holds or opens
// a mailbox key. On a mailbox that has a key, reading takes the flag and a
// grant at its current epoch (the one rule of store.ReaderSQL), which every
// access decision here reads. Who may write what:
//
//   - the link writes a mailbox's first key and its linker's grant
//     (AddAccount, section 12.11);
//   - a person who reads a mailbox without a key writes its first one, with a
//     grant for everyone who holds read with an account key (WriteFirstKey,
//     section 12.14);
//   - a personal mailbox's person writes it the next one (WriteNewKey,
//     section 12.12), never a team mailbox's anyone;
//   - an owner or an admin who reads a mailbox gives read with a grant
//     (SetAccess, section 12.13);
//   - anyone who reads a mailbox hands its key to a member who holds the
//     flag without a grant (SupplyKey, section 12.13).
//
// Each needs a fresh step-up (section 11) when it writes a key or a grant for
// someone else, checked before anything slow and again in the transaction
// that writes. Only a person signed in does any of it: an API key seals
// nothing and reads none of it, and an operator mailbox has no key
// (section 12.15).

// MailboxKeyPair is a mailbox's key pair at its current epoch, as a console
// reads it to check a grant it opens (section 9.2): the epoch, the public
// half and the namespace. The private key is only ever sealed in grants.
type MailboxKeyPair struct {
	Epoch     int    `json:"epoch"`
	PublicKey string `json:"public_key"`
	Namespace string `json:"namespace"`
}

// MailboxKeyState is a mailbox's key as the console of a person who holds
// read on it needs it: the key pair at its current epoch (absent for a
// mailbox without a key), the person's own grant at that epoch (absent while
// they wait for the key), and to whom they may seal, or who may seal to them.
type MailboxKeyState struct {
	Epoch     int    `json:"epoch,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Grant     string `json:"grant,omitempty"`
	// Waiting are, for a person who reads the mailbox, the active members
	// who hold read and have an account key but no grant at the current
	// epoch: to whom they may supply the key (PUT .../grants/{user}).
	Waiting []KeyRecipient `json:"waiting"`
	// Suppliers are, for a person who waits for the key, who reads the
	// mailbox now and may supply it.
	Suppliers []KeySupplier `json:"suppliers"`
	// KeylessReaders are, on a mailbox without a key, the other active
	// members who hold read and have an account key: whom its first key is
	// sealed to beside its writer (POST .../mailbox-key).
	KeylessReaders []KeyRecipient `json:"keyless_readers"`
}

// KeyRecipient is a person a grant may be sealed to: who they are, their
// seal id, which binds the grant, and their account public key, which it is
// sealed to (section 9.1).
type KeyRecipient struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	SealID    string `json:"seal_id"`
	PublicKey string `json:"public_key"`
}

// KeySupplier is a person who reads a mailbox and may hand its key on.
type KeySupplier struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

// SealedGrant is a grant the server stored: a mailbox's private key at one
// epoch sealed to one person, 88 bytes it cannot open.
type SealedGrant struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
	Epoch     int    `json:"epoch"`
	Grant     string `json:"grant"`
	// GrantedBy is who sealed it; absent once that person is deleted.
	GrantedBy string `json:"granted_by,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// FirstKeyRequest is the first key pair of a mailbox that has none
// (section 12.14), at epoch 1: its public half, its namespace, and one grant
// for its writer and for every other active member who holds read and has an
// account key, no more and no fewer.
type FirstKeyRequest struct {
	PublicKey string           `json:"public_key"`
	Namespace string           `json:"namespace"`
	Grants    []GrantToRequest `json:"grants"`
}

// GrantToRequest is one grant of a first key: to whom, the grant, base64url
// of 88 bytes, and the account public key the browser sealed it to,
// base64url of 32 bytes, which must be that person's now: the writer's own
// included.
type GrantToRequest struct {
	UserID    string `json:"user_id"`
	Grant     string `json:"grant"`
	PublicKey string `json:"public_key"`
}

// NewKeyRequest is a personal mailbox's next key pair (section 12.12): at
// the epoch after its current one, with its person's grant. The namespace is
// the mailbox's, and is not sent.
type NewKeyRequest struct {
	Epoch     int    `json:"epoch"`
	PublicKey string `json:"public_key"`
	Grant     string `json:"grant"`
}

// SupplyKeyRequest hands a mailbox's key to a member who holds read on it
// without a grant (section 12.13): the grant, at the mailbox key's current
// epoch, which it names, and the member's account public key the browser
// sealed it to, which must be theirs now.
type SupplyKeyRequest struct {
	Epoch     int    `json:"epoch"`
	Grant     string `json:"grant"`
	PublicKey string `json:"public_key"`
}

// Errors of mailbox keys.
var (
	errNoAccountKey = E(CodeConflict,
		"your account has no account key yet, and a mailbox you link is keyed to it", nil)
	errLinkKeyNeeded = E(CodeBadRequest,
		"a person links a mailbox with its key: public_key, namespace and grant, made by the browser", nil)
	errOperatorKey = E(CodeBadRequest,
		"a mailbox linked by an instance key has no key: public_key, namespace and grant come only from a person's browser", nil)
	errBadPublicKey = E(CodeBadRequest, "public_key is base64url of 32 bytes the server accepts as a mailbox key", nil)
	errBadNamespace = E(CodeBadRequest, "namespace is a lowercase UUIDv4", nil)
	errBadGrant     = E(CodeBadRequest,
		"a grant is base64url of 88 bytes with a grant's header at the mailbox key's epoch", nil)
	errBadEpoch    = E(CodeBadRequest, "an epoch is 1 to 65535", nil)
	errTeamKey     = E(CodeNotAuthorized, "a team mailbox is never given a new key: whoever made it would hold its only grant", nil)
	errGrantNeeded = E(CodeBadRequest,
		"read on a mailbox that has a key comes with the person's grant, sealed by the browser of someone who reads it", nil)
	errSealedTo = E(CodeBadRequest,
		"a grant names the account public key it was sealed to: public_key, base64url of 32 bytes", nil)
	errSealedToAlone = E(CodeBadRequest, "public_key names what a grant was sealed to, and comes only with one", nil)
)

// requireStepUp refuses a session whose step-up is not fresh
// (docs/key-scheme.md section 11), before anything slow: what a write that
// needs one asks first. The write asks again in its transaction (stepUpTx),
// which is the one that counts.
func (s *Service) requireStepUp(ctx context.Context, p Principal) error {
	if err := requireSession(p); err != nil {
		return err
	}
	if err := s.users.RequireStepUp(ctx, p.UserID, p.SessionID); err != nil {
		return fromUsers(err, "reading the step-up failed")
	}
	return nil
}

// stepUpTx is requireStepUp inside the transaction that writes a key or a
// grant, rendered for the caller.
func (s *Service) stepUpTx(ctx context.Context, tx *sql.Tx, p Principal) error {
	if err := requireSession(p); err != nil {
		return err
	}
	if err := s.users.RequireStepUpTx(ctx, tx, p.UserID, p.SessionID); err != nil {
		return fromUsers(err, "reading the step-up failed")
	}
	return nil
}

// stepUpCheck is a write's check that the caller's step-up is fresh.
func (s *Service) stepUpCheck(ctx context.Context, p Principal) workspace.Check {
	return func(tx *sql.Tx) error { return s.stepUpTx(ctx, tx, p) }
}

// requireAccountKey refuses a person who has no account key yet: nothing is
// sealed to them, and they seal nothing (section 12.10 is how a person who
// signs in elsewhere gets one).
func (s *Service) requireAccountKey(ctx context.Context, p Principal) error {
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		return fromUsers(err, "reading the account failed")
	}
	if len(user.PublicKey) == 0 {
		return errNoAccountKey
	}
	return nil
}

// strictBytes decodes base64url without padding of exactly n bytes, in its
// one spelling.
func strictBytes(s string, n int) ([]byte, bool) {
	b, err := strictB64.DecodeString(s)
	if err != nil || len(b) != n || strictB64.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// mailboxPublicKey decodes a mailbox's public key as a browser sends it.
func mailboxPublicKey(s string) ([]byte, error) {
	pub, ok := strictBytes(s, keyscheme.KeyLen)
	if !ok || keyscheme.CheckPublicKey(pub) != nil {
		return nil, errBadPublicKey
	}
	return pub, nil
}

// grantBytes decodes a grant as a browser sends it; its header is checked
// where its epoch is known.
func grantBytes(s string) ([]byte, error) {
	g, ok := strictBytes(s, keyscheme.GrantLen)
	if !ok {
		return nil, errBadGrant
	}
	return g, nil
}

// sealedOf decodes a grant a browser sealed for someone, with the account
// public key it says it sealed it to (docs/key-scheme.md section 9.3), each
// in its shape. Whether that key is the recipient's now is the write's to
// find, in its transaction.
func sealedOf(grant, publicKey string) (workspace.Sealed, error) {
	g, err := grantBytes(grant)
	if err != nil {
		return workspace.Sealed{}, err
	}
	to, ok := strictBytes(publicKey, keyscheme.KeyLen)
	if !ok {
		return workspace.Sealed{}, errSealedTo
	}
	return workspace.Sealed{Grant: g, SealedTo: to}, nil
}

// checkNamespace refuses a namespace outside its spelling.
func checkNamespace(ns string) error {
	if !keyscheme.ValidNamespace(ns) {
		return errBadNamespace
	}
	return nil
}

// linkKeyOf reads the key a person's link carries (section 12.11): every
// part, each in its shape, the grant at epoch 1.
func linkKeyOf(req AddAccountRequest) (*workspace.LinkKey, error) {
	if req.PublicKey == "" || req.Namespace == "" || req.Grant == "" {
		return nil, errLinkKeyNeeded
	}
	pub, err := mailboxPublicKey(req.PublicKey)
	if err != nil {
		return nil, err
	}
	if err := checkNamespace(req.Namespace); err != nil {
		return nil, err
	}
	grant, err := grantBytes(req.Grant)
	if err != nil {
		return nil, err
	}
	if err := grantAtEpoch(grant, keyscheme.MinEpoch); err != nil {
		return nil, err
	}
	key := workspace.LinkKey{PublicKey: pub, Namespace: req.Namespace, Grant: grant}
	if err := workspace.CheckLinkKey(key); err != nil {
		return nil, fromKeys(err, "checking the mailbox key failed")
	}
	return &key, nil
}

// grantAtEpoch refuses a grant whose header is not a grant's at the epoch
// the request names (section 9.3): a request at odds with itself, whatever
// the mailbox's key. Whether that epoch is the mailbox key's is the write's
// to find, in its transaction.
func grantAtEpoch(grant []byte, epoch int) error {
	if keyscheme.CheckGrantShape(grant, epoch) != nil {
		return errBadGrant
	}
	return nil
}

// MailboxKey answers a mailbox's key as the console of the person signed in
// needs it (sections 12.13 and 12.14): for a person who holds read on it, the
// key pair at its current epoch and their own grant there; to whom they may
// supply the key, while they read it; who may supply it to them, while they
// wait for it; and, on a mailbox without a key, whom its first key is sealed
// to. A person who sees the mailbox without holding read is not_authorized;
// one who does not see it, not_found. A key reads none of it.
func (s *Service) MailboxKey(ctx context.Context, p Principal, accountID string) (MailboxKeyState, error) {
	if err := requireSession(p); err != nil {
		return MailboxKeyState{}, err
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, accountID, needCard)
	if err != nil {
		return MailboxKeyState{}, err
	}
	state, err := s.workspaces.KeyState(ctx, a.ID, p.UserID)
	if err != nil {
		return MailboxKeyState{}, fromKeys(err, "reading the mailbox key failed")
	}
	if !state.HoldsFlag {
		return MailboxKeyState{}, errNoRead
	}
	out := MailboxKeyState{Waiting: []KeyRecipient{}, Suppliers: []KeySupplier{}, KeylessReaders: []KeyRecipient{}}
	switch {
	case state.Key == nil:
		// Read by the flag: the caller may write its first key.
		out.KeylessReaders = presentRecipients(state.KeylessReaders)
	case state.Reads:
		out.Epoch, out.PublicKey, out.Namespace = state.Key.Epoch, b64(state.Key.PublicKey), state.Key.Namespace
		out.Grant = b64(state.Grant)
		out.Waiting = presentRecipients(state.Waiting)
	default:
		out.Epoch, out.PublicKey, out.Namespace = state.Key.Epoch, b64(state.Key.PublicKey), state.Key.Namespace
		for _, r := range state.Suppliers {
			out.Suppliers = append(out.Suppliers, KeySupplier{UserID: r.UserID, Email: r.Email, Name: r.Name})
		}
	}
	return out, nil
}

// WriteFirstKey writes the first key pair of a mailbox that has none
// (section 12.14), with a grant for its writer and for every other active
// member who holds read and has an account key, after a fresh step-up. Only
// a person who reads the mailbox now, by the flag, and has an account key
// writes it, so that an owner or an admin who reads none of a team's
// mailboxes cannot take one by keying it. Grants that are not exactly those
// people's, or one that names another account public key than its
// recipient's now, are a conflict: someone enrolled, was given read or was
// reset meanwhile, and the console reads the mailbox key again.
func (s *Service) WriteFirstKey(ctx context.Context, p Principal, accountID string, req FirstKeyRequest) (MailboxKeyPair, error) {
	if err := requireSession(p); err != nil {
		return MailboxKeyPair{}, err
	}
	pub, err := mailboxPublicKey(req.PublicKey)
	if err != nil {
		return MailboxKeyPair{}, err
	}
	if err := checkNamespace(req.Namespace); err != nil {
		return MailboxKeyPair{}, err
	}
	if len(req.Grants) == 0 {
		return MailboxKeyPair{}, E(CodeBadRequest, "a first key comes with its writer's grant at least", nil)
	}
	grants := make([]workspace.GrantTo, 0, len(req.Grants))
	for _, g := range req.Grants {
		sealed, err := sealedOf(g.Grant, g.PublicKey)
		if err != nil {
			return MailboxKeyPair{}, err
		}
		if err := grantAtEpoch(sealed.Grant, keyscheme.MinEpoch); err != nil {
			return MailboxKeyPair{}, err
		}
		grants = append(grants, workspace.GrantTo{UserID: g.UserID, Grant: sealed.Grant, SealedTo: sealed.SealedTo})
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		return MailboxKeyPair{}, err
	}
	if err := s.requireStepUp(ctx, p); err != nil {
		return MailboxKeyPair{}, err
	}
	key, err := s.workspaces.WriteFirstKey(ctx, a.ID, p.UserID,
		workspace.FirstKey{PublicKey: pub, Namespace: req.Namespace, Grants: grants}, s.stepUpCheck(ctx, p))
	if err != nil {
		return MailboxKeyPair{}, fromKeys(err, "writing the mailbox key failed")
	}
	s.log.Info("mailbox keyed", "account", a.ID, "by", p.Actor(), "epoch", key.Epoch, "grants", len(grants))
	s.accessChanged()
	s.reconcile(a.ID)
	return presentKeyPair(key), nil
}

// WriteNewKey writes a personal mailbox a new key pair at the epoch after its
// current one, with its person's own grant, after a fresh step-up, and
// deletes every grant of an older epoch (section 12.12): when its person can
// no longer open their grant (after a reset), or at will. Only the mailbox's
// person does: a team mailbox is never given a new key (not_authorized), and
// anyone else's mailbox does not exist for the caller.
func (s *Service) WriteNewKey(ctx context.Context, p Principal, accountID string, req NewKeyRequest) (MailboxKeyPair, error) {
	if err := requireSession(p); err != nil {
		return MailboxKeyPair{}, err
	}
	if req.Epoch < keyscheme.MinEpoch || req.Epoch > keyscheme.MaxEpoch {
		return MailboxKeyPair{}, errBadEpoch
	}
	pub, err := mailboxPublicKey(req.PublicKey)
	if err != nil {
		return MailboxKeyPair{}, err
	}
	grant, err := grantBytes(req.Grant)
	if err != nil {
		return MailboxKeyPair{}, err
	}
	if err := grantAtEpoch(grant, req.Epoch); err != nil {
		return MailboxKeyPair{}, err
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		return MailboxKeyPair{}, err
	}
	switch {
	case a.OwnerUserID == "":
		return MailboxKeyPair{}, errTeamKey
	case a.OwnerUserID != p.UserID:
		return MailboxKeyPair{}, errNoAccount
	}
	if err := s.requireStepUp(ctx, p); err != nil {
		return MailboxKeyPair{}, err
	}
	key, err := s.workspaces.WriteNextKey(ctx, a.ID, p.UserID,
		workspace.NextKey{Epoch: req.Epoch, PublicKey: pub, Grant: grant}, s.stepUpCheck(ctx, p))
	if err != nil {
		return MailboxKeyPair{}, fromKeys(err, "writing the mailbox key failed")
	}
	s.log.Info("mailbox given a new key", "account", a.ID, "by", p.Actor(), "epoch", key.Epoch)
	s.accessChanged()
	s.reconcile(a.ID)
	return presentKeyPair(key), nil
}

// SupplyKey hands a mailbox's key to a member who holds read on it and has
// no grant at its current epoch (section 12.13): they had not enrolled when
// the key was written, or were reset since. Any person who reads the mailbox
// may, after a fresh step-up, owner, admin or member: it gives nobody read
// who was not given it. The recipient must be an active member who holds the
// flag and has an account key; the grant is at the current epoch, which the
// request names, and names the account public key it was sealed to, which
// must be the recipient's now (a conflict otherwise: they were reset since
// the console read them).
func (s *Service) SupplyKey(ctx context.Context, p Principal, accountID, userID string, req SupplyKeyRequest) (SealedGrant, error) {
	if err := requireSession(p); err != nil {
		return SealedGrant{}, err
	}
	if req.Epoch < keyscheme.MinEpoch || req.Epoch > keyscheme.MaxEpoch {
		return SealedGrant{}, errBadEpoch
	}
	sealed, err := sealedOf(req.Grant, req.PublicKey)
	if err != nil {
		return SealedGrant{}, err
	}
	if err := grantAtEpoch(sealed.Grant, req.Epoch); err != nil {
		return SealedGrant{}, err
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeAdmin, accountID, needCard)
	if err != nil {
		return SealedGrant{}, err
	}
	if err := s.requireStepUp(ctx, p); err != nil {
		return SealedGrant{}, err
	}
	g, err := s.workspaces.SupplyGrant(ctx, a.ID, userID, p.UserID, req.Epoch, sealed, s.stepUpCheck(ctx, p))
	if err != nil {
		return SealedGrant{}, fromKeys(err, "handing the key on failed")
	}
	s.log.Info("mailbox key supplied", "account", a.ID, "to", userID, "by", p.Actor(), "epoch", g.Epoch)
	s.accessChanged()
	s.reconcile(a.ID)
	return presentSealedGrant(g), nil
}

// fromKeys maps a refusal of a mailbox key or a grant onto the transport
// vocabulary (docs/workspaces.md, "Errors"): a value outside its shape is
// bad_request; a state that moved under the console — another epoch, a key
// or a grant already written, a namespace in use, a first key whose grants
// no longer match, a person without an account key or without the flag, a
// grant sealed to an account key the person no longer has — is conflict; a
// giver who does not read, not_authorized. Everything else is
// fromWorkspace's.
func fromKeys(err error, what string) error {
	var se *Error
	switch {
	case errors.As(err, &se):
		return err
	case errors.Is(err, keyscheme.ErrPublicKey):
		return errBadPublicKey
	case errors.Is(err, keyscheme.ErrShape):
		return errBadGrant
	case errors.Is(err, keyscheme.ErrBinding):
		return E(CodeBadRequest, "a namespace is a lowercase UUIDv4, and an epoch 1 to 65535", err)
	case errors.Is(err, workspace.ErrEpoch):
		return E(CodeConflict, "that is not the mailbox key's current epoch, or for a new key the next one; "+
			"read the mailbox key again", err)
	case errors.Is(err, workspace.ErrKeyless):
		return E(CodeConflict, "the mailbox has no key: it is read by the flag alone until someone who reads it "+
			"writes its first one", err)
	case errors.Is(err, workspace.ErrKeyed):
		return E(CodeConflict, "the mailbox already has a key; read it again", err)
	case errors.Is(err, workspace.ErrNamespaceTaken):
		return E(CodeConflict, "another mailbox uses that namespace; draw another", err)
	case errors.Is(err, workspace.ErrSealedGrantExists):
		return E(CodeConflict, "that person already holds the mailbox's key at that epoch", err)
	case errors.Is(err, workspace.ErrNotEnrolled):
		return E(CodeConflict, "that person has no account key yet: they are given read by the flag alone, "+
			"and the key once they have one", err)
	case errors.Is(err, workspace.ErrGrantsIncomplete):
		return E(CodeConflict, "a first key comes with exactly one grant for each person who holds read and has "+
			"an account key, its writer included; read the mailbox key again", err)
	case errors.Is(err, workspace.ErrNoReadFlag):
		return E(CodeConflict, "that person does not hold read on the mailbox: the key goes only to someone given read", err)
	case errors.Is(err, workspace.ErrSealedGrantNeeded):
		return errGrantNeeded
	case errors.Is(err, workspace.ErrSealedGrantUnwanted):
		return E(CodeBadRequest, "a grant comes only with the read it gives; a member who holds read gets the key "+
			"with PUT /v1/accounts/{id}/grants/{user}", err)
	case errors.Is(err, workspace.ErrNotReader):
		return E(CodeNotAuthorized, "only someone who reads the mailbox now writes its key or hands it on", err)
	case errors.Is(err, workspace.ErrTeamKey):
		return errTeamKey
	case errors.Is(err, workspace.ErrSealedToAnother):
		return E(CodeConflict, "that grant was sealed to an account key the person does not have now: they were "+
			"reset since they were read; read them again, and seal it again", err)
	default:
		return fromWorkspace(err, what)
	}
}

func presentKeyPair(k workspace.KeyPair) MailboxKeyPair {
	return MailboxKeyPair{Epoch: k.Epoch, PublicKey: b64(k.PublicKey), Namespace: k.Namespace}
}

func presentRecipients(people []workspace.Recipient) []KeyRecipient {
	out := make([]KeyRecipient, 0, len(people))
	for _, r := range people {
		out = append(out, KeyRecipient{
			UserID: r.UserID, Email: r.Email, Name: r.Name, SealID: r.SealID, PublicKey: b64(r.PublicKey),
		})
	}
	return out
}

func presentSealedGrant(g workspace.SealedGrant) SealedGrant {
	return SealedGrant{
		AccountID: g.AccountID, UserID: g.UserID, Epoch: g.Epoch, Grant: b64(g.Grant), GrantedBy: g.GrantedBy,
		CreatedAt: unixOrZero(g.CreatedAt),
	}
}
