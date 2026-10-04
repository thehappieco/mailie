// The sentence for the notice at the top of the accounts section. One place,
// because the same sentence is drawn there, said by the live region, and shown
// when the add dialog finishes.

import type { Notice } from '../state/accounts'
import { describe } from './errors'
import { t } from './i18n'

export function noticeText(notice: Notice): string {
  switch (notice.kind) {
    case 'connected': return notice.syncing
      ? t('{email} is connected. Mailie is starting to sync it: the last 90 days first, then new mail as it arrives.', { email: notice.email })
      : t('{email} is connected. You can list its folders now. Sync is off until you turn it on.', { email: notice.email })
    case 'removed': return t('{email} was removed from Mailie.', { email: notice.email })
    case 'failed': return notice.email ? t('{email} was not connected. {reason}', { email: notice.email, reason: describe(notice.failure) }) : describe(notice.failure)
    case 'sync-on': return t('Sync is on. Mailie is indexing your mailboxes, starting with the last 90 days.')
    case 'sync-off': return t('Sync is off. Mailie deleted the index of your mail.')
  }
}
