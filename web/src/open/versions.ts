// The revisions of the open console's texts, as the server knows them. They
// are the daemon's defaults (internal/config/consent.go), and the contract
// spec holds them to the fixtures the handlers write. A new text comes with a
// new revision here and there, together.

/** What sync stores (SyncText.vue). */
export const SYNC_TEXT_VERSION = '2026-10-open-sync'
/** What actions on messages change (ActionsText.vue). */
export const ACTIONS_TEXT_VERSION = '2026-10-open-actions'
/** What a tool holding a new key can do (KeyTermsText.vue). */
export const KEY_TERMS_VERSION = '2026-10-open-api-keys'
/**
 * The open console asks nobody about sending: it has no text for it, and
 * sends nothing. The server's default names this revision all the same.
 */
export const SEND_TEXT_VERSION = '2026-10-open-sending'
