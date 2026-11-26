package config

import "fmt"

// ConsentVersions name the revisions of the texts a person agrees to in a
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

// The revisions asked for when none is configured. They name the texts of the
// open console a self-hosted server served until it was deleted (platform
// decision 0063), whose words the repository's history keeps under
// web/src/open: what sync stores (SyncText.vue, its fourth revision), what
// actions on messages change (ActionsText.vue, its third) and what a tool
// holding a new API key can do (KeyTermsText.vue, its third). It had no text
// about sending, so nobody agreed to sending from it. No console in this
// repository shows a text now: a deployment whose people agree to texts sets
// the revisions its own texts carry, as the hosted service does for its app.
// The defaults stay as they were, so that what a database recorded under them
// keeps its meaning, and the core's console specs (web/test/edition.ts) and
// the contract fixtures (internal/api/testdata/contract) name the same
// revisions. They are pinned here, in Go alone, by consent_test.go.
const (
	DefaultSyncConsentVersion    = "2026-10-open-sync-4"
	DefaultActionsConsentVersion = "2026-10-open-actions-3"
	DefaultSendConsentVersion    = "2026-10-open-sending"
	DefaultKeyTermsVersion       = "2026-10-open-api-keys-3"
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
