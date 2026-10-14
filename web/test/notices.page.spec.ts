// A person's notices as they meet them, on a page (test/dom.ts): above every
// section of the console, in whichever workspace it happened, naming the
// mailbox and where it stands now, never who did it; each said once through
// the live region, however often they are read; dismissed for good with the
// request that deletes it; and none on a server older than notices.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, find, flush, words } from './dom'
import { mount, type Mounted } from './mount'
import type { PersonNotice, Workspace } from '../src/api/types'
import OpenConsole from '../src/open/OpenConsole.vue'
import { accounts } from '../src/state/accounts'
import { loadNotices, notices } from '../src/state/notices'
import { adoptSession, signOut } from '../src/state/session'
import { announcement } from '../src/ui/announce'
import { failure, json, reply, serve, stubPage } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const removedNotice: PersonNotice = {
  id: 'mbe_0000000000000001', kind: 'removed', workspace_id: 'wsp_000000000000bbbb', workspace_name: 'Support', account_id: 'acc_billing',
  mailbox: 'billing@example.test', at: 1_790_000_000, state: 'removed', recoverable_until: 1_792_592_000,
}
const syncOffNotice: PersonNotice = {
  id: 'mbe_0000000000000002', kind: 'sync_off', workspace_id: 'wsp_000000000000bbbb', workspace_name: 'Support', account_id: 'acc_shared',
  mailbox: 'suporte@example.test', at: 1_790_000_500, state: 'live',
}
const deletedNotice: PersonNotice = {
  id: 'mbe_0000000000000003', kind: 'removed', workspace_id: 'wsp_000000000000cccc', account_id: 'acc_gone', mailbox: 'v…@example.test', at: 1_789_000_000, state: 'deleted',
}

let mounted: Mounted | null = null
let given: PersonNotice[] | null = []
let dismissed: string[] = []

/** Ana on the whole open console, in her personal workspace; the server answers given as her notices, or 404 for none at all. */
async function signedIn() {
  serve(({ path, method }) => {
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/workspaces') return json([personal])
    if (path === '/v1/accounts') return json([])
    if (path === '/v1/providers') return json([])
    if (path === '/v1/me/notices') return given ? json(given) : failure('not_found', 404)
    const one = path.match(/^\/v1\/me\/notices\/([^/]+)$/)
    if (one && method === 'DELETE') {
      dismissed.push(one[1]!)
      given = (given ?? []).filter(notice => notice.id !== one[1])
      return new Response(null, { status: 204 })
    }
    if (path === '/v1/events') return failure('not_authorized', 403)
    return failure('not_found', 404)
  })
  await adoptSession(reply())
  mounted = mount(OpenConsole)
  await vi.waitFor(() => expect(accounts.loaded).toBe(true))
  await flush()
}
const shown = () => [...find('.person-notices')?.querySelectorAll('.person-notice-text') ?? []].map(element => words(element))

beforeEach(() => {
  stubPage()
  given = []
  dismissed = []
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('a person’s notices', () => {
  it('names each mailbox and where it stands now above the section shown, never who did it', async () => {
    given = [syncOffNotice, removedNotice, deletedNotice]
    await signedIn()
    await vi.waitFor(() => expect(shown()).toHaveLength(3))
    expect(shown()).toEqual([
      'Sync of suporte@example.test was turned off for Support, and its index was deleted. An owner or an admin of Support can turn it on again.',
      'billing@example.test was removed from Support, and you no longer read it. It is kept until October 21, 2026, and an owner of Support can recover it until then.',
      'v…@example.test was removed, and you no longer read it. It has since been deleted for good.',
    ])
    // Above the section, in the console's frame, whatever the section shown.
    expect(find('.console-content .person-notices')).not.toBeNull()
  })

  it('says each notice once, however often they are read, and dismisses one for good', async () => {
    given = [removedNotice]
    await signedIn()
    await vi.waitFor(() => expect(announcement.value).toBe('billing@example.test was removed from Support, and you no longer read it. It is kept until October 21, 2026, and an owner of Support can recover it until then.'))
    announcement.value = ''
    await loadNotices()
    await flush()
    expect(announcement.value).toBe('')
    // A new one comes: it alone is said.
    given = [syncOffNotice, removedNotice]
    await loadNotices()
    await vi.waitFor(() => expect(announcement.value).toBe('Sync of suporte@example.test was turned off for Support, and its index was deleted. An owner or an admin of Support can turn it on again.'))
    await click(find('.person-notice', 'billing@example.test')!.querySelector('button'))
    await vi.waitFor(() => expect(dismissed).toEqual([removedNotice.id]))
    expect(shown()).toEqual([expect.stringContaining('suporte@example.test')])
    expect(notices.list.map(notice => notice.id)).toEqual([syncOffNotice.id])
  })

  it('shows none, and says nothing, on a server older than notices', async () => {
    given = null
    await signedIn()
    await vi.waitFor(() => expect(notices.loaded).toBe(true))
    expect(find('.person-notices')).toBeNull()
    expect(notices.failure).toBeNull()
  })
})
