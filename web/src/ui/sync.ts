// What an account's sync, and a folder's, look like to a person. The API
// speaks in states and error classes (initial, backoff, rate_limited); these
// are the words for them. An error class the console does not know is a
// generic sentence, never the class itself.

import type { Account, Folder } from '../api/types'
import { ahead } from './format'
import { t } from './i18n'
import type { Tone } from './labels'

export type SyncTone = Tone | 'busy'

export interface SyncView {
  tone: SyncTone
  label: string
  detail: string
  /** 0..100 while the first sync runs. */
  progress?: number
}

/**
 * syncView describes a syncing account: one that is active and whose owner
 * turned sync on. Anything else is described by its account state
 * (labels.stateDetail), and this answers null.
 */
export function syncView(account: Account, now = Date.now()): SyncView | null {
  const sync = account.sync
  if (account.state !== 'active' || !sync.enabled) return null
  switch (sync.state) {
    case 'initial':
      return { tone: 'busy', label: t('Syncing'), detail: t('Indexing the last 90 days, newest first.'), progress: Math.min(100, Math.max(0, sync.initial_progress)) }
    case 'live':
      return { tone: 'live', label: t('Up to date'), detail: t('New mail is indexed as it arrives.') }
    case 'backoff': {
      const retry = sync.next_retry_at ? t('Mailie tries again {when}.', { when: ahead(sync.next_retry_at, now) }) : t('Mailie will try again.')
      return { tone: 'warn', label: t('Retrying'), detail: `${syncErrorText(sync.error_class)} ${retry}` }
    }
    case 'stopped':
      // No worker holds the account. With a failure on record, that is what
      // to say; without one it is a gap the engine closes on its own (a
      // restart, a worker not picked up yet), not something that went wrong.
      if (!sync.error_class) return { tone: 'off', label: t('Paused'), detail: t('Mailie resumes syncing this mailbox shortly.') }
      return { tone: 'bad', label: t('Stopped'), detail: syncErrorText(sync.error_class) }
    case 'off':
      // Permitted and active, but no worker holds it yet: the engine picks
      // new consent and new accounts up within moments.
      return { tone: 'off', label: t('Starting'), detail: t('Sync is on. This mailbox starts syncing shortly.') }
    default:
      return { tone: 'off', label: t('Sync is on'), detail: '' }
  }
}

/** What went wrong, from the short class the server reports (provider.Class). */
export function syncErrorText(errorClass: string | undefined): string {
  switch (errorClass) {
    case 'needs_reauth': return t('The provider asks for this account to be authorized again.')
    case 'auth_failed': return t('The mail server did not accept the sign-in.')
    case 'not_connected': return t('The mail server accepted the sign-in but would not open the mailbox. Check that IMAP is turned on for it.')
    case 'too_many_connections': return t('The mail server has too many connections open for this mailbox. Closing other mail apps can help.')
    case 'rate_limited': return t('The provider is limiting how often Mailie can ask.')
    case 'connection_closed':
    case 'timeout': return t('The connection to the mail server dropped or was too slow.')
    case 'uidvalidity_changed': return t('The mail server renumbered a folder, so Mailie is indexing it again.')
    case 'unsupported': return t('The mail server does not support something sync needs.')
    default: return t('The last sync attempt failed.')
  }
}

export interface FolderBadge { text: string; title: string; tone: '' | 'planned' | 'busy' | 'live' | 'bad' }

/**
 * The badge beside a folder. fromIndex: the list came from the index, so the
 * folder's own sync state is known; otherwise it came from the mail server,
 * and only whether sync would take the folder is.
 */
export function folderBadge(folder: Folder, fromIndex: boolean): FolderBadge | null {
  if (!fromIndex) return folder.synced ? { text: t('Included in sync'), title: t('Mailie indexes this folder when sync is on.'), tone: 'planned' } : null
  if (!folder.synced) return folder.selectable ? { text: t('Not synced'), title: t('Mailie does not sync this folder.'), tone: '' } : null
  switch (folder.sync_state) {
    case 'new':
    case 'initial': return { text: t('Syncing'), title: t('The first sync of this folder is running.'), tone: 'busy' }
    case 'live': return { text: t('Synced'), title: t('This folder is indexed and kept up to date.'), tone: 'live' }
    case 'resync': return { text: t('Resyncing'), title: t('The mail server renumbered this folder, so Mailie is indexing it again.'), tone: 'busy' }
    case 'error': return { text: t('Sync error'), title: t('The last sync of this folder failed. Mailie will try again.'), tone: 'bad' }
    default: return null
  }
}

/** A listing read from the index carries each folder's sync state; a live one never does. */
export function fromIndex(folders: readonly Folder[]): boolean {
  return folders.some(folder => folder.sync_state !== undefined)
}
