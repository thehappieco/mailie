// Names for the values the API sends. The API speaks in identifiers
// (pending_auth, uidpoll); a person reads these.

import type { Account, AccountState, AuthKind, Folder, ProviderID, Role } from '../api/types'
import type { IconName } from '../components/AppIcon.vue'
import { accessOf } from './access'
import { t } from './i18n'
import { reasonText } from './reasons'

/** Product names are the same in every language; the generic option is not. */
export function providerName(id: ProviderID | ''): string {
  switch (id) {
    case 'gmail': return 'Gmail'
    case 'microsoft': return 'Microsoft 365'
    case 'icloud': return 'iCloud'
    case 'imap': return t('Other (IMAP)')
    default: return ''
  }
}

/**
 * The longer label on the provider choice, where Outlook.com needs naming too.
 * Apple translates its product's name, so iCloud Mail goes through the catalog.
 */
export function providerChoice(id: ProviderID): string {
  switch (id) {
    case 'gmail': return t('Gmail or Google Workspace')
    case 'microsoft': return t('Microsoft 365 or Outlook')
    case 'icloud': return t('iCloud Mail')
    case 'imap': return t('Other provider (IMAP)')
  }
}

/**
 * Generic icons, deliberately. The providers' marks are theirs, with rules for
 * where they may appear; a console that is not a sign-in button does not
 * need them, and the name beside the icon already says which is which.
 */
export function providerIcon(id: ProviderID | ''): IconName {
  switch (id) {
    case 'gmail': return 'mail'
    case 'microsoft': return 'inbox'
    case 'icloud': return 'cloud'
    default: return 'server'
  }
}

export type Tone = 'live' | 'warn' | 'bad' | 'off'

export function stateTone(state: AccountState): Tone {
  switch (state) {
    case 'active': return 'live'
    case 'pending_auth': return 'warn'
    case 'needs_reauth':
    case 'error': return 'bad'
    case 'disabled': return 'off'
  }
}

export function stateLabel(state: AccountState): string {
  switch (state) {
    case 'active': return t('Active')
    case 'pending_auth': return t('Waiting for authorization')
    case 'needs_reauth': return t('Needs authorization')
    case 'error': return t('Connection failed')
    case 'disabled': return t('Disabled')
  }
}

/**
 * What the state means for this account, in a sentence or two. Mapped from
 * the state, and for a failing account from the few reasons the console
 * explains itself (ui/reasons.ts): the server's own state_reason and
 * last_error are diagnostics in English and are never shown (the same rule
 * as error messages). Authorizing a mailbox again is for whoever manages it:
 * anyone else is told so, not what they could not do.
 */
export function stateDetail(account: Account): string {
  if (needsAuthorization(account) && !accessOf(account).manage) {
    switch (account.state) {
      case 'pending_auth': return t('Authorization was not finished. Someone who manages this mailbox has to finish it.')
      case 'needs_reauth': return t('The provider asks for this mailbox to be authorized again, by someone who manages it. Mail is not syncing.')
      default: return t('The last attempt to connect failed. Someone who manages this mailbox has to authorize it again.')
    }
  }
  switch (account.state) {
    case 'active':
      // How far the sync has come is the sync block's to say (ui/sync.ts).
      return account.sync.enabled ? t('Connected. Sync is on.') : t('Connected. Sync is off.')
    case 'pending_auth':
      return t('Authorization was not finished. Finish it, or remove this account.')
    case 'needs_reauth':
      return reasonText(account) ?? t('The provider asks for this account to be authorized again. Mail is not syncing.')
    case 'error':
      return reasonText(account) ?? (account.auth_kind === 'oauth2'
        ? t('The last attempt to connect failed. Try the authorization again.')
        : t('The last attempt to connect failed. Check the account at the provider.'))
    case 'disabled':
      return t('This account is disabled and does not sync.')
  }
}

/** Only OAuth accounts in these states can be fixed by signing in at the provider again. */
export function needsAuthorization(account: Account): boolean {
  return account.auth_kind === 'oauth2' && (account.state === 'pending_auth' || account.state === 'needs_reauth' || account.state === 'error')
}

export function authKindLabel(kind: AuthKind): string {
  return kind === 'oauth2' ? t('Provider sign-in (OAuth)') : t('Password (IMAP and SMTP)')
}

/** Empty until the sync engine has seen the server and picked a tier; callers render nothing then. */
export function syncTierLabel(tier: string | undefined): string {
  switch (tier) {
    case undefined:
    case '': return ''
    case 'condstore': return t('Incremental (CONDSTORE)')
    case 'uidpoll': return t('Periodic check')
    default: return tier
  }
}

export function roleLabel(role: Role): string {
  return role === 'owner' ? t('Owner') : t('Member')
}

export function folderRoleLabel(role: string | undefined): string {
  switch (role) {
    case 'inbox': return t('Inbox')
    case 'sent': return t('Sent')
    case 'drafts': return t('Drafts')
    case 'trash': return t('Trash')
    case 'junk': return t('Junk')
    case 'archive': return t('Archive')
    case 'all': return t('All mail')
    case 'flagged': return t('Flagged')
    case 'important': return t('Important')
    default: return ''
  }
}

export function folderRoleIcon(role: string | undefined): IconName {
  switch (role) {
    case 'inbox': return 'inbox'
    case 'sent': return 'send'
    case 'drafts': return 'pencil'
    case 'trash': return 'trash'
    case 'junk': return 'alert'
    case 'archive': return 'archive'
    case 'all': return 'layers'
    case 'flagged':
    case 'important': return 'star'
    default: return 'folder'
  }
}

// Folders with a role first, in the order a mail client lists them, then the
// rest by name.
const roleOrder = ['inbox', 'sent', 'drafts', 'archive', 'all', 'flagged', 'important', 'junk', 'trash']
function rank(role: string | undefined): number {
  const index = role ? roleOrder.indexOf(role) : -1
  return index < 0 ? roleOrder.length : index
}

/** Folders in the order a mail client lists them: inbox, sent, drafts… then the rest by name. */
export function sortFolders<T extends Pick<Folder, 'role' | 'display_name' | 'name'>>(list: readonly T[]): T[] {
  return [...list].sort((a, b) => rank(a.role) - rank(b.role) || (a.display_name || a.name).localeCompare(b.display_name || b.name))
}
