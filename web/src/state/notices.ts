// The signed-in person's notices (docs/workspaces.md, "Removed mailboxes"):
// what someone else did to a mailbox they read — removed it, or turned a
// team mailbox's sync off, deleting its index — kept on the server until they
// dismiss it, in whichever workspace it happened.
//
// Read when the console starts, and again when the event stream says the
// person was given one (`event: notice`), when access changes, and on a
// Refresh. Each notice is said once through the live region the first time
// it is read, never again for the same person, however often the list is
// read. A server older than notices (not_found) has none to show; a failure
// to read them is kept, shown nowhere, and the next read tries again: a
// notice is told, never something to act on before the rest of the console.
// Everything resets when the person changes.

import { reactive, watch } from 'vue'
import * as api from '../api/notices'
import { ApiError } from '../api/http'
import type { PersonNotice } from '../api/types'
import { announce } from '../ui/announce'
import { count } from '../ui/format'
import { t } from '../ui/i18n'
import { personNoticeText } from '../ui/personNotices'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

interface NoticesState {
  list: PersonNotice[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
  /** The notice being dismissed, and what the last dismissal failed with. */
  dismissing: string
  problem: { id: string; failure: Failure } | null
}

const fresh = (): NoticesState => ({ list: [], loaded: false, loading: false, failure: null, dismissing: '', problem: null })

export const notices = reactive<NoticesState>(fresh())

let generation = 0
/** The notices already said, for this person: each is said once. */
let said = new Set<string>()
/** A read asked for while one was in flight: it runs once that one lands. */
let again = false

watch(identity, () => {
  generation++
  said = new Set()
  again = false
  Object.assign(notices, fresh())
}, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

/** Reads the person's notices, and says the ones not said yet. */
export async function loadNotices(): Promise<void> {
  if (notices.loading) { again = true; return }
  const ok = current()
  notices.loading = true
  notices.failure = null
  try {
    const list = await authorized(token => api.listNotices(token))
    if (!ok()) return
    Object.assign(notices, { list, loaded: true })
    tell(list)
  } catch (error) {
    if (!ok()) return
    if (error instanceof ApiError && error.code === 'not_found') Object.assign(notices, { list: [], loaded: true })
    else notices.failure = failure('load-notices', error)
  } finally {
    if (ok()) {
      notices.loading = false
      if (again) { again = false; void loadNotices() }
    }
  }
}

/** The person may have been given a notice (the event stream said so, or access changed): read them again once they were read. */
export function noticesChanged(): void {
  if (notices.loaded || notices.loading || notices.failure) void loadNotices()
}

/** Says, once, the notices not said yet: one by its own words, several by how many. */
function tell(list: PersonNotice[]): void {
  const fresh = list.filter(notice => !said.has(notice.id))
  for (const notice of fresh) said.add(notice.id)
  if (fresh.length === 1) announce(personNoticeText(fresh[0]!))
  else if (fresh.length > 1) announce(t('{count} notices about mailboxes you read.', { count: count(fresh.length) }))
}

/** Dismisses a notice for good. One the server no longer has is gone all the same. */
export async function dismissPersonNotice(id: string): Promise<boolean> {
  if (notices.dismissing) return false
  const ok = current()
  notices.dismissing = id
  notices.problem = null
  try {
    await authorized(token => api.dismissNotice(token, id))
  } catch (error) {
    if (!(error instanceof ApiError && error.code === 'not_found')) {
      if (ok()) { notices.problem = { id, failure: failure('dismiss-notice', error) }; notices.dismissing = '' }
      return false
    }
  }
  if (!ok()) return false
  notices.dismissing = ''
  notices.list = notices.list.filter(notice => notice.id !== id)
  return true
}
