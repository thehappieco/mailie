package obs

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Redaction of identifiers and credentials in log output.
//
// This server holds, for several people at once, the address of everyone they
// correspond with and a refresh token that opens their mailbox. A log line is
// the easiest way for either to leave the machine: aggregators have their own
// retention, their own access rules and none of this database's encryption.
//
// So two defences, applied to every record at every level:
//
//  1. By shape. Anything that looks like an email address keeps its first
//     letter and its domain, which is enough to match a line to a row for
//     somebody who already has the database and not enough to name anyone for
//     somebody who has only the logs. An API key keeps its prefix — the prefix
//     is a lookup selector, not a secret, and it is what makes a line
//     traceable to a caller — and loses the part that authenticates.
//
//  2. By key. An attribute whose name says it carries a credential is dropped
//     whole, because an access token has no shape to recognise: it is
//     whatever the provider decided to mint.
//
// Message bodies and subjects are not redacted here because they are not
// logged at all; see the project rules.
var (
	// emailRE is an address with a real domain. The prefix keeps one
	// character so two different senders do not collapse into one line.
	// Letters are any script's: with ASCII classes, "joão@exemplo.com.br"
	// matched only from the "o" before the @, and "joão" reached the log
	// whole — and net/mail, which checks every address here, accepts it.
	emailRE = regexp.MustCompile(`([\p{L}\p{N}])[\p{L}\p{M}\p{N}._%+'-]*(@[\p{L}\p{M}\p{N}.-]+\.[\p{L}\p{M}]{2,})`)
	// apiKeyRE is this project's key format: an 8-hex lookup prefix, a dot,
	// and 32 random bytes in base64url.
	apiKeyRE = regexp.MustCompile(`\b([0-9a-f]{8})\.[A-Za-z0-9_-]{20,}`)
	// bearerRE catches a credential that arrived as a header value.
	bearerRE = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	// xoauth2RE catches the SASL initial response, which is a base64 blob
	// containing "auth=Bearer <token>".
	xoauth2RE = regexp.MustCompile(`(?i)\b(AUTHENTICATE|AUTH)\s+(XOAUTH2|OAUTHBEARER|PLAIN|LOGIN)\s+\S+`)
)

// secretKeys are attribute names whose value never reaches the log. Matched as
// substrings, lowercased: "refresh_token", "smtp_password" and "authorization"
// all land here without being listed one by one.
var secretKeys = []string{"token", "password", "secret", "authorization", "credential", "passphrase", "code_verifier"}

const redacted = "[redacted]"

// Redact masks identifiers and credentials in s.
func Redact(s string) string {
	s = xoauth2RE.ReplaceAllString(s, "$1 $2 "+redacted)
	s = bearerRE.ReplaceAllString(s, "$1 "+redacted)
	s = apiKeyRE.ReplaceAllString(s, "$1."+redacted)
	return emailRE.ReplaceAllString(s, "$1…$2")
}

// isSecretKey reports whether an attribute name promises a credential.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, needle := range secretKeys {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}

// redactAttr is the slog ReplaceAttr hook. Groups are irrelevant: an address is
// an address however deeply it is nested.
//
// Every value that can carry text goes through Redact, not only strings. The
// commonest attribute in this codebase is "err", and an error is not a string
// to slog: it would reach the handler untouched. Errors are exactly where
// addresses turn up — an IMAP server's refusal names the login it refused,
// and Microsoft's AADSTS descriptions quote the account — so each one is
// rendered as the handler would render it, and masked.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if isSecretKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(a.Value.String()))
	case slog.KindAny:
		// fmt rather than a type switch on error: it calls Error or String
		// itself, and survives one that panics on a nil receiver.
		return slog.String(a.Key, Redact(fmt.Sprint(a.Value.Any())))
	}
	return a
}
