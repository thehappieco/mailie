// The revisions of the open console's texts, as the server knows them. They
// are the daemon's defaults (internal/config/consent.go), and the contract
// spec holds them to the fixtures the handlers write. A new text comes with a
// new revision here and there, together.

/**
 * What sync stores (SyncText.vue). The second revision said who reads the
 * index of a team mailbox; the third, that a team's mailbox syncs under the
 * team's agreement, which its owners and admins give and withdraw (for
 * everyone who reads it), while a person's covers their personal mailboxes.
 * The fourth, that a removed mailbox is kept 30 days with its index, synced
 * and read by nobody, so that whoever may remove it recovers it, held off
 * until someone who may turns its sync on, or deletes it for good sooner;
 * that only an owner removes a team's mailbox; and that removing it, or
 * turning its sync off, tells the others who read it.
 */
export const SYNC_TEXT_VERSION = '2026-10-open-sync-4'
/**
 * What actions on messages change (ActionsText.vue). The second revision
 * says who may ask for a change: someone allowed to act on the mailbox, not
 * only its person. The third, that a tool with an API key given Act acts
 * under the key terms, not under this agreement: turning actions off stops
 * the person's own, and a key's from before keys belonged to workspaces,
 * never one created since.
 */
export const ACTIONS_TEXT_VERSION = '2026-10-open-actions-3'
/**
 * What a tool holding a new key can do (KeyTermsText.vue). The second
 * revision says that the key belongs to its workspace, whose owners and
 * admins see it, choose its mailboxes and revoke it, as whoever runs the
 * server may too; that it reads only mailboxes given by someone who reads
 * them; that it may act and send where it is given to, whatever anyone
 * chooses about actions in their account, every send confirmed by the tool;
 * that it stops when its creator leaves or is closed; and that keys under
 * the first keep it, and never send. The third, that it also stops when its
 * creator's account is recovered, with the recovery code or a reset link.
 */
export const KEY_TERMS_VERSION = '2026-10-open-api-keys-3'
/**
 * The open console asks nobody about sending: it has no text for it, and
 * sends nothing. The server's default names this revision all the same.
 */
export const SEND_TEXT_VERSION = '2026-10-open-sending'
