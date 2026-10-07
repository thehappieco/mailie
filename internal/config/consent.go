package config

import "fmt"

// ConsentVersions name the revisions of the texts a person agrees to in the
// console: what sync stores (Sync), Mailie changing their mailboxes when they
// ask (Actions), sending from them (Send), and what a tool holding an API key
// they create in a workspace can do (Keys).
//
// A console asking for an agreement names the revision of the text it showed,
// and the daemon records only the one configured here, so an answer given to
// one text is never taken as agreement to another. Changing a revision asks
// everybody again. A consent to actions or sending given to another revision
// stops counting — actions and sends are refused — until they agree to the
// new one. A consent to sync given to another revision keeps their mailboxes
// syncing: the console asks again, and sync stops only if they turn it off.
// A key keeps the terms it was created under.
type ConsentVersions struct {
	Sync    string // MAIL_CONSENT_VERSION_SYNC
	Actions string // MAIL_CONSENT_VERSION_ACTIONS
	Send    string // MAIL_CONSENT_VERSION_SEND
	Keys    string // MAIL_CONSENT_VERSION_KEYS
}

// The revisions asked for when none is configured: the texts of the open
// console in web/, the console a self-hosted server serves. It asks about
// sync, actions and keys; it has no text about sending, so nobody agrees to
// sending from it. A deployment that serves another console sets the
// revisions that console's texts carry, as the hosted service does for its
// own app.
const (
	DefaultSyncConsentVersion    = "2026-10-open-sync-3"
	DefaultActionsConsentVersion = "2026-10-open-actions-3"
	DefaultSendConsentVersion    = "2026-10-open-sending"
	DefaultKeyTermsVersion       = "2026-10-open-api-keys-2"
)

// maxConsentVersionLen bounds a revision: it is stored beside every consent
// and every key, and shown back to the console.
const maxConsentVersionLen = 64

// DefaultConsentVersions is every revision at its default.
func DefaultConsentVersions() ConsentVersions {
	return ConsentVersions{
		Sync: DefaultSyncConsentVersion, Actions: DefaultActionsConsentVersion,
		Send: DefaultSendConsentVersion, Keys: DefaultKeyTermsVersion,
	}
}

// OrDefaults is v with every revision left empty at its default.
func (v ConsentVersions) OrDefaults() ConsentVersions {
	d := DefaultConsentVersions()
	return ConsentVersions{
		Sync: orDefault(v.Sync, d.Sync), Actions: orDefault(v.Actions, d.Actions),
		Send: orDefault(v.Send, d.Send), Keys: orDefault(v.Keys, d.Keys),
	}
}

func consentVersions(errs *[]error) ConsentVersions {
	return ConsentVersions{
		Sync:    consentVersion("MAIL_CONSENT_VERSION_SYNC", DefaultSyncConsentVersion, errs),
		Actions: consentVersion("MAIL_CONSENT_VERSION_ACTIONS", DefaultActionsConsentVersion, errs),
		Send:    consentVersion("MAIL_CONSENT_VERSION_SEND", DefaultSendConsentVersion, errs),
		Keys:    consentVersion("MAIL_CONSENT_VERSION_KEYS", DefaultKeyTermsVersion, errs),
	}
}

// consentVersion reads one revision: printable ASCII without spaces, at most
// maxConsentVersionLen bytes. It travels in JSON to the console and back, and
// is compared byte for byte, so nothing that could be spelt two ways is
// accepted.
func consentVersion(key, def string, errs *[]error) string {
	v := str(key, def)
	if len(v) > maxConsentVersionLen {
		*errs = append(*errs, fmt.Errorf("%s: at most %d bytes, got %d", key, maxConsentVersionLen, len(v)))
		return def
	}
	for i := range len(v) {
		if c := v[i]; c <= ' ' || c > '~' {
			*errs = append(*errs, fmt.Errorf("%s: %q must be printable ASCII without spaces, such as %s",
				key, v, def))
			return def
		}
	}
	return v
}
