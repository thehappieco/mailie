package provider

import (
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// FolderRole names a folder's job, independent of what it is called.
//
// Gmail localises its folder names ("[Gmail]/Todos") and Exchange localises
// every default folder ("Itens Enviados"), so nothing may be matched by name
// where an attribute is available — and on Exchange, where no attribute is
// available, the name table below is the fallback rather than the rule.
type FolderRole string

const (
	RoleNone      FolderRole = ""
	RoleInbox     FolderRole = "inbox"
	RoleAll       FolderRole = "all"
	RoleSent      FolderRole = "sent"
	RoleDrafts    FolderRole = "drafts"
	RoleTrash     FolderRole = "trash"
	RoleJunk      FolderRole = "junk"
	RoleArchive   FolderRole = "archive"
	RoleFlagged   FolderRole = "flagged"
	RoleImportant FolderRole = "important"
)

// ParseRole converts a stored or configured role name, rejecting anything
// outside the vocabulary: a folder override naming a role that does not exist
// must not reach a column whose CHECK would refuse it, or a filter that would
// never match.
func ParseRole(s string) (FolderRole, bool) {
	switch r := FolderRole(s); r {
	case RoleNone, RoleInbox, RoleAll, RoleSent, RoleDrafts, RoleTrash, RoleJunk, RoleArchive,
		RoleFlagged, RoleImportant:
		return r, true
	}
	return RoleNone, false
}

// Where a folder's role came from, strongest first. Stored beside the role,
// because "the server said so" and "we matched a localised name" deserve
// different amounts of trust when a role looks wrong.
const (
	RoleSourceOverride   = "override"
	RoleSourceInbox      = "inbox"
	RoleSourceSpecialUse = "special-use"
	RoleSourceNameTable  = "name-table"
)

// RoleSourceRank orders role sources, strongest first, for deciding which of
// two folders claiming the same role keeps it. Unknown sources rank last.
func RoleSourceRank(source string) int {
	switch source {
	case RoleSourceOverride:
		return 0
	case RoleSourceInbox:
		return 1
	case RoleSourceSpecialUse:
		return 2
	case RoleSourceNameTable:
		return 3
	}
	return 4
}

// ResolveRole decides what a folder is for, and says how it decided.
//
// In order: an override the person set (role -> folder name), the inbox by
// its fixed name, the server's own SPECIAL-USE attribute, then the profile's
// table of localised names. The order matters — Exchange Online publishes no
// attributes at all and translates every default folder, so without the table
// its Sent folder is unfindable; and a person who has said which folder is
// which must not be overruled by a guess. An override naming a role outside
// the vocabulary is ignored, and overrides are consulted in a fixed order so
// two that name the same folder always resolve the same way.
//
// The folder index and the live listing both call this, so a folder never
// has one role in the console and another in the engine.
func ResolveRole(f Folder, profile Profile, overrides map[string]string) (FolderRole, string) {
	if len(overrides) > 0 {
		roles := make([]string, 0, len(overrides))
		for role := range overrides {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			parsed, ok := ParseRole(role)
			if !ok || parsed == RoleNone {
				continue
			}
			if strings.EqualFold(overrides[role], f.Name) {
				return parsed, RoleSourceOverride
			}
		}
	}
	if strings.EqualFold(f.Name, "INBOX") {
		return RoleInbox, RoleSourceInbox
	}
	for _, attr := range f.Attrs {
		if role := RoleFromAttr(attr); role != RoleNone {
			return role, RoleSourceSpecialUse
		}
	}
	if role := profile.RoleForName(f.Name); role != RoleNone {
		return role, RoleSourceNameTable
	}
	return RoleNone, ""
}

// RoleFromAttr maps a SPECIAL-USE attribute to a role.
func RoleFromAttr(attr imap.MailboxAttr) FolderRole {
	switch attr {
	case imap.MailboxAttrAll:
		return RoleAll
	case imap.MailboxAttrSent:
		return RoleSent
	case imap.MailboxAttrDrafts:
		return RoleDrafts
	case imap.MailboxAttrTrash:
		return RoleTrash
	case imap.MailboxAttrJunk:
		return RoleJunk
	case imap.MailboxAttrArchive:
		return RoleArchive
	case imap.MailboxAttrFlagged:
		return RoleFlagged
	case imap.MailboxAttrImportant:
		return RoleImportant
	default:
		return RoleNone
	}
}

// Profile is everything that differs between providers, as data.
//
// The sync engine reads this; it never asks which provider it is talking to.
// A server that behaves like Exchange therefore needs a table entry, not a
// branch — and the entries below each record something that was measured or
// documented, not assumed.
type Profile struct {
	Kind Kind

	// IdleRenew is how often to end and restart IDLE. Under the 28 minutes at
	// which go-imap restarts on its own and the 29 the RFC allows, and short
	// for Exchange, which has shipped periods where EXISTS was only flushed
	// after DONE — so on Microsoft the renewal is also what bounds how long
	// new mail can go unnoticed.
	IdleRenew time.Duration

	// SaveSentDefault is whether to append a copy to the Sent folder after
	// submitting. False for Gmail and Microsoft: both file the copy
	// themselves, and appending a second one is the classic duplicate.
	SaveSentDefault bool

	// RoleByName maps a lowercased folder name to a role, consulted only when
	// the server offers no SPECIAL-USE attribute.
	RoleByName map[string]FolderRole

	// NeverSyncRoles are folders to leave alone. On Gmail the archive is the
	// whole mailbox over again, and Starred and Important are views of flags
	// rather than places a message lives.
	NeverSyncRoles []FolderRole

	// ArchiveRole is where "archive" moves a message.
	ArchiveRole FolderRole

	// FlagOnlyRoles are folders that are really flags: moving a message into
	// one would be wrong, setting the flag is what the user meant.
	FlagOnlyRoles []FolderRole

	// FlagsSharedAcrossFolders is true where a flag belongs to the message
	// rather than to the copy. On Gmail, marking the inbox copy read marks
	// every label copy read, and an index that does not follow shows
	// contradictory unread counts until the next pass.
	FlagsSharedAcrossFolders bool

	// MaxConnections is the budget for this account. Gmail allows fifteen
	// across every client the person runs, so three leaves room for their
	// phone and their desktop client.
	MaxConnections int

	// SMTPMaxPerMinute paces submission.
	SMTPMaxPerMinute int
	// SMTPMaxSize refuses an oversized message before dialling. Exchange
	// advertises 150 MB and enforces about 35.
	SMTPMaxSize int64

	// AuthFailureShape is how a refused token arrives: Gmail sends a SASL
	// continuation carrying base64 JSON and only then fails the command;
	// Exchange fails outright.
	AuthFailureShape AuthFailureShape

	// DeviceCodeSupported is whether the device-code flow can be offered.
	// Google forbids it for the mail scope; Entra security defaults block it,
	// and tenants created after July 2026 cannot enable it at all.
	DeviceCodeSupported bool
}

// AuthFailureShape describes how a server refuses a bad token.
type AuthFailureShape string

const (
	// AuthFailureJSONContinuation is Gmail: "+ <base64 JSON>" and then the
	// tagged NO, with the client expected to answer in between.
	AuthFailureJSONContinuation AuthFailureShape = "json-continuation"
	// AuthFailureTagged is Exchange: a plain tagged NO.
	AuthFailureTagged AuthFailureShape = "tagged"
)

// SyncsRole reports whether a folder with this role should be indexed.
func (p Profile) SyncsRole(role FolderRole) bool {
	for _, skip := range p.NeverSyncRoles {
		if skip == role {
			return false
		}
	}
	return true
}

// SyncsFolder reports whether a folder is indexed at all: it is selectable,
// its role is one the provider syncs, and nothing else about it says it is
// one of the roles the provider never syncs.
//
// The last part is what keeps Gmail's All Mail, Starred and Important out
// whatever role the folder ends up with: an override naming All Mail as the
// archive, or a role the folder lost to another one claiming it, changes the
// label on the folder, not what it holds. So a SPECIAL-USE attribute, or —
// only when the server sent none — a name the profile's table knows, of a
// never-synced role rules the folder out.
func (p Profile) SyncsFolder(f Folder, role FolderRole) bool {
	if !f.Selectable || !p.SyncsRole(role) {
		return false
	}
	specialUse := false
	for _, attr := range f.Attrs {
		if r := RoleFromAttr(attr); r != RoleNone {
			specialUse = true
			if !p.SyncsRole(r) {
				return false
			}
		}
	}
	if !specialUse && !strings.EqualFold(f.Name, "INBOX") {
		if r := p.RoleForName(f.Name); r != RoleNone && !p.SyncsRole(r) {
			return false
		}
	}
	return true
}

// IsFlagOnly reports whether "moving" into this folder really means setting a
// flag.
func (p Profile) IsFlagOnly(role FolderRole) bool {
	for _, r := range p.FlagOnlyRoles {
		if r == role {
			return true
		}
	}
	return false
}

// RoleForName looks a folder name up in the localised table.
func (p Profile) RoleForName(name string) FolderRole {
	if p.RoleByName == nil {
		return RoleNone
	}
	// Exchange nests under the inbox on some tenants, so the leaf is what the
	// table knows.
	leaf := name
	if i := strings.LastIndexAny(name, "/."); i >= 0 && i+1 < len(name) {
		leaf = name[i+1:]
	}
	if role, ok := p.RoleByName[strings.ToLower(leaf)]; ok {
		return role
	}
	role := p.RoleByName[strings.ToLower(name)]
	return role
}

// gmailCapability is what Gmail advertises after login. Reading it is
// harmless; requesting an X-GM-* item is what this server never does.
const gmailCapability = "X-GM-EXT-1"

// IsGmailServer reports whether a server is Gmail's, whatever the account
// calls it: it advertised Gmail's extension, or it is one of Google's IMAP
// hosts.
func IsGmailServer(caps Caps, host string) bool {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")) {
	case "imap.gmail.com", "imap.googlemail.com":
		return true
	}
	for _, c := range caps.Raw {
		if strings.EqualFold(c, gmailCapability) {
			return true
		}
	}
	return false
}

// ForServer is the profile adjusted for what the server turned out to be.
//
// A Gmail mailbox connected as generic IMAP — a Google Workspace domain the
// provider guess did not recognise, or someone choosing "other IMAP" with an
// app password — is still Gmail: its All Mail holds the whole mailbox again,
// and Starred and Important are views of flags. Gmail's never-synced roles,
// flag-only roles and folder names therefore apply whatever the account's
// kind, and so do what archiving means (leaving the inbox for All Mail) and
// that a flag belongs to the message rather than to one label's copy. The
// rest of the profile is the account's.
func (p Profile) ForServer(caps Caps, host string) Profile {
	if p.Kind == KindGmail || !IsGmailServer(caps, host) {
		return p
	}
	g := gmailProfile()
	out := p
	out.NeverSyncRoles = unionRoles(p.NeverSyncRoles, g.NeverSyncRoles)
	out.FlagOnlyRoles = unionRoles(p.FlagOnlyRoles, g.FlagOnlyRoles)
	out.ArchiveRole = g.ArchiveRole
	out.FlagsSharedAcrossFolders = g.FlagsSharedAcrossFolders
	names := make(map[string]FolderRole, len(p.RoleByName)+len(g.RoleByName))
	for name, role := range p.RoleByName {
		names[name] = role
	}
	for name, role := range g.RoleByName {
		names[name] = role
	}
	out.RoleByName = names
	return out
}

func unionRoles(a, b []FolderRole) []FolderRole {
	out := append([]FolderRole(nil), a...)
	for _, r := range b {
		found := false
		for _, have := range out {
			if have == r {
				found = true
				break
			}
		}
		if !found {
			out = append(out, r)
		}
	}
	return out
}

// ProfileFor returns the quirk table for a provider.
func ProfileFor(kind Kind) Profile {
	switch kind {
	case KindGmail:
		return gmailProfile()
	case KindMicrosoft:
		return microsoftProfile()
	default:
		return genericProfile()
	}
}

func gmailProfile() Profile {
	return Profile{
		Kind:      KindGmail,
		IdleRenew: 7 * time.Minute,
		// Gmail files the copy in [Gmail]/Sent Mail itself and documents that
		// clients must not save a second one.
		SaveSentDefault: false,
		// Roles come from SPECIAL-USE, which Gmail supports; the table is a
		// safety net for an account whose language makes a name unfamiliar.
		RoleByName: map[string]FolderRole{
			"all mail": RoleAll, "sent mail": RoleSent, "drafts": RoleDrafts,
			"trash": RoleTrash, "bin": RoleTrash, "spam": RoleJunk,
			"starred": RoleFlagged, "important": RoleImportant,
		},
		// The archive holds every message again, so indexing it alongside the
		// labels would download the mailbox twice and count everything twice.
		NeverSyncRoles: []FolderRole{RoleAll, RoleFlagged, RoleImportant},
		ArchiveRole:    RoleAll,
		FlagOnlyRoles:  []FolderRole{RoleFlagged, RoleImportant},
		// A label copy is the same message: its flags are the message's flags.
		FlagsSharedAcrossFolders: true,
		MaxConnections:           3,
		SMTPMaxPerMinute:         60,
		SMTPMaxSize:              35882577, // the SIZE Gmail advertises
		AuthFailureShape:         AuthFailureJSONContinuation,
		// Google's device flow does not allow https://mail.google.com/.
		DeviceCodeSupported: false,
	}
}

func microsoftProfile() Profile {
	return Profile{
		Kind: KindMicrosoft,
		// Short, because Exchange has delivered EXISTS only after DONE, which
		// makes the renewal interval the real notification latency.
		IdleRenew: 3 * time.Minute,
		// Client submission through smtp.office365.com files the copy in Sent
		// Items already.
		SaveSentDefault: false,
		// Exchange Online advertises no SPECIAL-USE and localises every
		// default folder, so this table is the only way to find them.
		RoleByName: map[string]FolderRole{
			// English
			"sent items": RoleSent, "drafts": RoleDrafts, "deleted items": RoleTrash,
			"junk email": RoleJunk, "archive": RoleArchive,
			// Portuguese
			"itens enviados": RoleSent, "rascunhos": RoleDrafts, "itens excluídos": RoleTrash,
			"itens excluidos": RoleTrash, "lixo eletrônico": RoleJunk, "lixo eletronico": RoleJunk,
			"arquivo morto": RoleArchive,
			// Spanish
			"elementos enviados": RoleSent, "borradores": RoleDrafts,
			"elementos eliminados": RoleTrash, "correo no deseado": RoleJunk,
			// French
			"éléments envoyés": RoleSent, "elements envoyes": RoleSent,
			"brouillons": RoleDrafts, "éléments supprimés": RoleTrash,
			"elements supprimes": RoleTrash, "courrier indésirable": RoleJunk,
			"courrier indesirable": RoleJunk, "archives": RoleArchive,
			// German
			"gesendete elemente": RoleSent, "entwürfe": RoleDrafts, "entwurfe": RoleDrafts,
			"gelöschte elemente": RoleTrash, "geloschte elemente": RoleTrash,
			"junk-e-mail": RoleJunk,
			// Italian
			"posta inviata": RoleSent, "bozze": RoleDrafts, "posta eliminata": RoleTrash,
			"posta indesiderata": RoleJunk,
			// Dutch
			"verzonden items": RoleSent, "concepten": RoleDrafts,
			"verwijderde items": RoleTrash, "ongewenste e-mail": RoleJunk,
		},
		ArchiveRole:      RoleArchive,
		MaxConnections:   3,
		SMTPMaxPerMinute: 30, // documented: thirty messages a minute per mailbox
		SMTPMaxSize:      35 << 20,
		AuthFailureShape: AuthFailureTagged,
		// Offered, but off unless the operator asks: security defaults block
		// it, mandatorily for tenants created after July 2026.
		DeviceCodeSupported: true,
	}
}

func genericProfile() Profile {
	return Profile{
		Kind:      KindIMAP,
		IdleRenew: 7 * time.Minute,
		// A server that does not file the copy itself is the only case where
		// appending one is right.
		SaveSentDefault: true,
		RoleByName: map[string]FolderRole{
			"sent": RoleSent, "sent items": RoleSent, "sent messages": RoleSent,
			"drafts": RoleDrafts, "trash": RoleTrash, "deleted items": RoleTrash, "deleted messages": RoleTrash,
			"junk": RoleJunk, "spam": RoleJunk, "archive": RoleArchive,
		},
		ArchiveRole:         RoleArchive,
		MaxConnections:      4,
		SMTPMaxPerMinute:    60,
		SMTPMaxSize:         25 << 20,
		AuthFailureShape:    AuthFailureTagged,
		DeviceCodeSupported: false,
	}
}
