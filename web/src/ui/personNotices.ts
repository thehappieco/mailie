// The words of a person's notice (state/notices.ts): what someone else did to
// a mailbox they read, and where the mailbox stands now. One place, because
// the same words are drawn at the top of the console and said by the live
// region. The mailbox is named as the server names it: its address while it
// is kept, a masked hint of it once it is deleted for good. Who did it is
// never said: the server does not tell.

import type { PersonNotice } from '../api/types'
import { dayStamp } from './format'
import { t } from './i18n'

/** What happened, in the team it happened in when the server names one. */
function happened(notice: PersonNotice): string {
  const values = { mailbox: notice.mailbox, team: notice.workspace_name }
  if (notice.kind === 'sync_off') {
    return notice.workspace_name
      ? t('Sync of {mailbox} was turned off for {team}, and its index was deleted.', values)
      : t('Sync of {mailbox} was turned off, and its index was deleted.', values)
  }
  return notice.workspace_name
    ? t('{mailbox} was removed from {team}, and you no longer read it.', values)
    : t('{mailbox} was removed, and you no longer read it.', values)
}

/** Where the mailbox stands now, when that adds something. */
function standing(notice: PersonNotice): string {
  switch (notice.state) {
    case 'removed': {
      const values = { date: dayStamp(notice.recoverable_until), team: notice.workspace_name }
      return notice.workspace_name
        ? t('It is kept until {date}, and an owner of {team} can recover it until then.', values)
        : t('It is kept until {date}, and can be recovered until then.', values)
    }
    case 'deleted': return t('It has since been deleted for good.')
    case 'live': return notice.kind === 'sync_off'
      ? notice.workspace_name ? t('An owner or an admin of {team} can turn it on again.', { team: notice.workspace_name }) : ''
      : t('It has since been recovered.')
    default: return ''
  }
}

export function personNoticeText(notice: PersonNotice): string {
  return [happened(notice), standing(notice)].filter(Boolean).join(' ')
}
