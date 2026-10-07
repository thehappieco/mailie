// The revisions of the open console's texts, as the server knows them. They
// are the daemon's defaults (internal/config/consent.go), and the contract
// spec holds them to the fixtures the handlers write. A new text comes with a
// new revision here and there, together.

/**
 * What sync stores (SyncText.vue). The second revision said who reads the
 * index of a team mailbox; the third, that a team's mailbox syncs under the
 * team's agreement, which its owners and admins give and withdraw (for
 * everyone who reads it), while a person's covers their personal mailboxes.
 */
export const SYNC_TEXT_VERSION = '2026-10-open-sync-3'
/**
 * What actions on messages change (ActionsText.vue). The second revision
 * says who may ask for a change: someone allowed to act on the mailbox, not
 * only its person.
 */
export const ACTIONS_TEXT_VERSION = '2026-10-open-actions-2'
/** What a tool holding a new key can do (KeyTermsText.vue). */
export const KEY_TERMS_VERSION = '2026-10-open-api-keys'
/**
 * The open console asks nobody about sending: it has no text for it, and
 * sends nothing. The server's default names this revision all the same.
 */
export const SEND_TEXT_VERSION = '2026-10-open-sending'
