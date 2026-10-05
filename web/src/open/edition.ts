// The open edition: the console a self-hosted server serves (src/main.ts).
// Its texts belong to whoever runs the server, so they name this server,
// never a company, and link to no policy; the server's consent revisions
// default to these texts (versions.ts).

import type { Edition } from '../edition'
import { t } from '../ui/i18n'
import ActionsText from './ActionsText.vue'
import KeyTermsText from './KeyTermsText.vue'
import OpenConsole from './OpenConsole.vue'
import SyncText from './SyncText.vue'
import { ACTIONS_TEXT_VERSION, KEY_TERMS_VERSION, SYNC_TEXT_VERSION } from './versions'

export const openEdition: Edition = {
  console: OpenConsole,
  sync: {
    version: SYNC_TEXT_VERSION,
    component: SyncText,
    changedWhileOpen: () => t('The text about sync changed on this server while this page was open. Reload the page to read the current text.'),
    changedSince: () => t('This server’s text about sync changed since you turned sync on. Read what sync stores now; sync stays on unless you turn it off.'),
  },
  actions: {
    version: ACTIONS_TEXT_VERSION,
    component: ActionsText,
    changedWhileOpen: () => t('The text about actions changed on this server while this page was open. Reload the page to read the current text.'),
    changedSince: date => t('Paused: this server’s text about actions changed since you allowed them on {date}. Review the new text and agree to use them again.', { date }),
  },
  keyTerms: { version: KEY_TERMS_VERSION, component: KeyTermsText },
  // A self-hosted server's interface is its console.
  shell: {
    rootLabel: () => t('Console'),
    navLabel: () => t('Console navigation'),
  },
  copy: {
    accountSection: () => t('Account'),
    mailboxesIntro: () => t('Gmail, Microsoft 365 or Outlook, iCloud Mail, or any provider that offers IMAP. This server checks the sign-in, lists the mailbox’s folders and, once you turn sync on, keeps an index of its messages’ details for your tools to search.'),
    indexHint: () => t('This server stores only the details of each message, never bodies or attachments. When a tool reads a message, the server fetches it from the mail server and does not keep it.'),
    actionsOff: () => t('This server stops changing your mailboxes: a tool asking to mark, star, archive or move a message is refused. Nothing is deleted, and your mail stays as it is now.'),
  },
  // Where the server serves MCP over HTTP (MAIL_MCP_HTTP, its default).
  mcp: true,
  // A self-hosted server makes its own teams (the local workspace source).
  teams: true,
  // Its owners bring people onto it, and its members are the rest.
  serverRole: true,
}
