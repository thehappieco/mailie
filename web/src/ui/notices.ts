// The sentence for the notice at the top of the accounts section. One place,
// because the same sentence is drawn there, said by the live region, and shown
// when the add dialog finishes.

import type { Notice } from '../state/accounts'
import { describe } from './errors'
import { dayStamp } from './format'
import { t } from './i18n'

export function noticeText(notice: Notice): string {
  switch (notice.kind) {
    case 'connected': return notice.syncing
      ? t('{email} is connected. Mailie is starting to sync it: the last 90 days first, then new mail as it arrives.', { email: notice.email })
      : t('{email} is connected. You can list its folders now. Sync is off until you turn it on.', { email: notice.email })
    case 'removed': return notice.keptUntil
      ? t('{email} was removed. Mailie keeps it until {date}: until then it can be recovered, under Removed mailboxes.', { email: notice.email, date: dayStamp(notice.keptUntil) })
      : t('{email} was removed from Mailie.', { email: notice.email })
    // As it came back: paused where its consent stands, held by the server's operator's turn-off, off where its consent is off.
    case 'recovered': switch (notice.sync) {
      case 'paused': return t('{email} was recovered. Its sync stays paused until it is resumed.', { email: notice.email })
      case 'held': return t('{email} was recovered. Its sync stays off until it is turned on again.', { email: notice.email })
      default: return t('{email} was recovered. Its sync is off until it is turned on.', { email: notice.email })
    }
    case 'purged': return t('{email} was deleted for good.', { email: notice.email })
    case 'failed': return notice.email ? t('{email} was not connected. {reason}', { email: notice.email, reason: describe(notice.failure) }) : describe(notice.failure)
    case 'sync-on': return t('Sync is on. Mailie is indexing your mailboxes, starting with the last 90 days.')
    case 'sync-off': return t('Sync is off. Mailie deleted the index of your mail.')
  }
}
