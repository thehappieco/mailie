package account

import (
	"strings"

	"github.com/thehappieco/mailie/internal/provider"
)

// One Gmail account, one mailbox (docs/workspaces.md, "Mailboxes"). Gmail
// delivers every spelling of a consumer address to one account — any case,
// dots anywhere in its local part, a "+tag" after it, googlemail.com for
// gmail.com — and Google keeps one grant per account and OAuth client. So a
// workspace links a Gmail mailbox once under all of its spellings, live or
// removed (duplicateOfTx), and a deletion for good revokes no grant that
// another spelling of the account still holds (googleTokensTx).
//
// A Google Workspace domain's addresses are compared without regard to case
// only: its dots count. Two addresses of one Workspace account (an alias, or a
// user renamed) are not recognised as one, so deleting one mailbox for good
// can revoke the grant the other holds early: it asks to be authorized again,
// and nobody gains access. Nothing kept here could tell them apart without
// keeping an identifier of the Google account, which Mailie does not.

// canonicalAddress is the form two addresses of mailboxes of kind share
// exactly when they name one mailbox: lowercased as the database compares
// addresses (ASCII only, as COLLATE NOCASE and lower() do), and for a Gmail
// mailbox at gmail.com or googlemail.com, its local part without its dots and
// without a "+" and what follows it, at gmail.com.
func canonicalAddress(kind provider.Kind, email string) string {
	email = asciiLower(strings.TrimSpace(email))
	if kind != provider.KindGmail {
		return email
	}
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return email
	}
	local, domain := email[:at], email[at+1:]
	if domain != "gmail.com" && domain != "googlemail.com" {
		return email
	}
	local, _, _ = strings.Cut(local, "+")
	return strings.ReplaceAll(local, ".", "") + "@gmail.com"
}

// sameMailbox reports whether two addresses, of mailboxes of the kinds given,
// name one mailbox: the same address without regard to ASCII case, or two
// Gmail mailboxes of one account under any spelling.
func sameMailbox(kind provider.Kind, email string, otherKind provider.Kind, other string) bool {
	return asciiLower(strings.TrimSpace(other)) == asciiLower(strings.TrimSpace(email)) ||
		(kind == provider.KindGmail && otherKind == provider.KindGmail &&
			canonicalAddress(kind, other) == canonicalAddress(kind, email))
}

// asciiLower lowercases the ASCII letters of s and nothing else, as SQLite's
// NOCASE does.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
