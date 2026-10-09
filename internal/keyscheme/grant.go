package keyscheme

import (
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"

	"github.com/thehappieco/kit/hpke"
	"github.com/thehappieco/kit/seal"
)

// The sealed envelope's domain (docs/key-scheme.md section 10).
const (
	// SealLabel prefixes the additional data and the HPKE info of every
	// envelope Mailie seals. Wappie's is "wsv1": a label is what keeps two
	// products' envelopes apart, so they never share one.
	SealLabel = "mlv1"
)

// SealMagic is bytes 0-1 of every envelope Mailie seals: "ML". Wappie's are
// "WS"; a blob of the other product fails at its magic, before anything is
// derived.
var SealMagic = [2]byte{'M', 'L'}

// SealDomain is Mailie's envelope domain: magic "ML", label "mlv1".
func SealDomain() seal.Domain { return seal.Domain{Magic: SealMagic, Label: SealLabel} }

// Kind identifies what a sealed value is. The byte is bound into the
// additional data and the name into a direct envelope's HPKE info, so both
// are wire format: a name never changes once a value has been sealed under
// it in direct mode. Phase 3 seals only KindMailboxGrant; the bytes named for
// phase 4 are reserved, nothing is sealed under them yet, and a byte with no
// name below is unassigned.
type Kind uint8

const (
	// KindHeaders is reserved for phase 4: a message's indexed header
	// fields (subject, addresses, dates), as JSON.
	KindHeaders Kind = 0x01
	// KindSnippet is reserved for phase 4: a message's preview text.
	KindSnippet Kind = 0x02
	// KindBody is reserved for phase 4: a message's text, when the index
	// keeps one.
	KindBody Kind = 0x03
	// KindAttachmentKey is reserved for phase 4: the 32-byte key of an
	// attachment kept sealed.
	KindAttachmentKey Kind = 0x04
	// KindSearchIndex is reserved for phase 4: a segment of the search index
	// the browser builds and keeps sealed.
	KindSearchIndex Kind = 0x05
	// KindContentKey is the core content key (the kit's SPEC section 4.6),
	// sealed directly to a mailbox's public key at
	// seal.ContentKeyRow(namespace, namespace, id). Phase 4.
	KindContentKey Kind = seal.KindContentKey
	// KindMailboxGrant is the core grant (the kit's SPEC section 5): a
	// mailbox's private key sealed to one person's account key at
	// GrantRow(namespace, seal id, epoch).
	KindMailboxGrant Kind = seal.KindGrant
	// KindUserWrap is the core reserved byte. Mailie never seals it either.
	KindUserWrap Kind = seal.KindUserWrap
	// KindFolderName is reserved for phase 4: a folder's name.
	KindFolderName Kind = 0x09
	// KindDraft is reserved for phase 4: a draft written in the browser.
	KindDraft Kind = 0x0A
)

// String is the kind's wire name, which a direct envelope's HPKE info
// carries; an unnamed byte is "kind(0x..)", as the kit requires.
func (k Kind) String() string {
	switch k {
	case KindHeaders:
		return "headers"
	case KindSnippet:
		return "snippet"
	case KindBody:
		return "body"
	case KindAttachmentKey:
		return "attachment_key"
	case KindSearchIndex:
		return "search_index"
	case KindContentKey:
		return "content_key"
	case KindMailboxGrant:
		return "mailbox_grant"
	case KindUserWrap:
		return "user_wrap"
	case KindFolderName:
		return "folder_name"
	case KindDraft:
		return "draft"
	default:
		return fmt.Sprintf("kind(%#x)", byte(k))
	}
}

// Domain binds every Mailie kind to Mailie's envelope domain, so a value of
// this type is never sealed under another product's label.
func (Kind) Domain() seal.Domain { return SealDomain() }

// Mailbox key epochs, and a grant's length.
const (
	// MinEpoch is a mailbox key's first epoch. 0 is never used: the server
	// stores 1 for a mailbox's first key and one more for each new key.
	MinEpoch = 1
	// MaxEpoch is the largest, the envelope header's u16be.
	MaxEpoch = 0xffff
	// GrantLen is a grant's length: the 8-byte header, the 32-byte
	// encapsulated key, and the 32-byte mailbox key sealed with its 16-byte
	// tag.
	GrantLen = seal.DirectOverhead + KeyLen
)

// grantBinding parses and checks what a grant is bound to.
func grantBinding(namespace, sealID string, epoch int) (ns, user uuid.UUID, e uint16, err error) {
	if ns, err = parseUUIDv4(namespace, "namespace"); err != nil {
		return
	}
	if user, err = parseUUIDv4(sealID, "seal id"); err != nil {
		return
	}
	if epoch < MinEpoch || epoch > MaxEpoch {
		err = fmt.Errorf("%w: an epoch is from %d to %d", ErrBinding, MinEpoch, MaxEpoch)
		return
	}
	return ns, user, uint16(epoch), nil
}

// GrantRow is the row a grant binds to, the kit's seal.GrantRow with the
// mailbox's namespace as both its tenant and its device:
//
//	Row(namespace, namespace ‖ seal_id ‖ u16be(epoch))
//
// so a grant opens only for its mailbox, its person and its epoch.
func GrantRow(namespace, sealID string, epoch int) (uuid.UUID, error) {
	ns, user, e, err := grantBinding(namespace, sealID, epoch)
	if err != nil {
		return uuid.UUID{}, err
	}
	return seal.GrantRow(ns, ns, user, e), nil
}

// GrantInfo is a grant's HPKE info: "mlv1/mailbox_grant/<namespace>/<epoch>".
func GrantInfo(namespace string, epoch int) ([]byte, error) {
	ns, err := parseUUIDv4(namespace, "namespace")
	if err != nil {
		return nil, err
	}
	if epoch < MinEpoch || epoch > MaxEpoch {
		return nil, fmt.Errorf("%w: an epoch is from %d to %d", ErrBinding, MinEpoch, MaxEpoch)
	}
	return seal.Info(KindMailboxGrant, ns, uint16(epoch)), nil
}

// GrantAAD is a grant's additional data: "mlv1" ‖ 0x07 ‖ namespace (16) ‖
// GrantRow (16) ‖ the direct envelope's header at that epoch (8), 45 bytes.
func GrantAAD(namespace, sealID string, epoch int) ([]byte, error) {
	ns, user, e, err := grantBinding(namespace, sealID, epoch)
	if err != nil {
		return nil, err
	}
	header := []byte{SealMagic[0], SealMagic[1], seal.Version, seal.SuiteV1, seal.ModeDirect, 0, 0, 0}
	binary.BigEndian.PutUint16(header[5:7], e)
	return seal.AAD(KindMailboxGrant, ns, seal.GrantRow(ns, ns, user, e), header), nil
}

// SealGrant seals a mailbox's 32-byte private key to a person's account
// public key: the kit's direct envelope with kind 0x07 at GrantRow, at the
// mailbox key's epoch, 88 bytes. A public key of low order is refused with an
// error matching seal.ErrInvalidKey: whoever handed it out could open the
// grant. The mailbox key is the caller's to clear.
func SealGrant(recipientPublicKey []byte, namespace, sealID string, epoch int, mailboxKey []byte) ([]byte, error) {
	ns, user, e, err := grantBinding(namespace, sealID, epoch)
	if err != nil {
		return nil, err
	}
	if len(mailboxKey) != KeyLen {
		return nil, fmt.Errorf("%w: a mailbox key is %d bytes", ErrBinding, KeyLen)
	}
	pub, err := hpke.ParsePublicKey(recipientPublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", seal.ErrInvalidKey, err)
	}
	return seal.SealDirect(pub, KindMailboxGrant, ns, seal.GrantRow(ns, ns, user, e), e, mailboxKey)
}

// OpenGrant opens a grant with the person's account private key and returns
// the mailbox's private key, which the caller clears. The kit's header checks
// come first (seal.ErrShort, ErrMagic, ErrVersion, ErrSuite, ErrMode); every
// other failure is seal.ErrAuthentication: another person, mailbox or epoch,
// another kind, a changed byte, and a grant that opens to anything but the
// private half of mailboxPublicKey, the key the server holds for the
// mailbox at that epoch. That last check is what a grant sealed by someone
// who knows only the person's public key (HPKE's base mode does not
// authenticate the sender) cannot pass without the mailbox's real key.
func OpenGrant(account hpke.PrivateKey, namespace, sealID string, epoch int, mailboxPublicKey, grant []byte) ([]byte, error) {
	ns, user, e, err := grantBinding(namespace, sealID, epoch)
	if err != nil {
		return nil, err
	}
	if len(mailboxPublicKey) != KeyLen {
		return nil, fmt.Errorf("%w: a mailbox public key is %d bytes", ErrBinding, KeyLen)
	}
	key, err := seal.OpenDirect(account, KindMailboxGrant, ns, seal.GrantRow(ns, ns, user, e), grant)
	if err != nil {
		return nil, err
	}
	if !isPublicHalf(key, mailboxPublicKey) {
		clear(key)
		return nil, seal.ErrAuthentication
	}
	return key, nil
}

// CheckGrantShape is what the server checks of a grant it is sent, which it
// cannot open: 88 bytes, Mailie's magic, version 1, suite 1, direct mode,
// the mailbox key's current epoch and a zero reserved byte.
func CheckGrantShape(grant []byte, epoch int) error {
	if len(grant) != GrantLen {
		return fmt.Errorf("%w: a grant is %d bytes", ErrShape, GrantLen)
	}
	h, err := seal.ParseHeader(SealDomain(), grant)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrShape, err)
	}
	if h.Mode != seal.ModeDirect || int(h.Epoch) != epoch || grant[7] != 0 {
		return fmt.Errorf("%w: a grant is a direct envelope at the current epoch", ErrShape)
	}
	return nil
}
