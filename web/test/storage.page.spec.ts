// Storage as a person sees it on a page (test/dom.ts): once shown, the
// section stays mounted, so what it counts changing elsewhere (sync turned
// off in Account, which deletes the index) reads its figures again rather
// than leaving the old ones beside "nothing is indexed".
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flush, page, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Storage } from '../src/api/types'
import StoragePanel from '../src/components/StoragePanel.vue'
import { loadAccounts } from '../src/state/accounts'
import { storage } from '../src/state/storage'
import { signIn, signOut } from '../src/state/session'
import { grantConsent, loadConsent, withdrawConsent } from '../src/state/sync'
import { SYNC_TEXT_VERSION } from '../src/open/versions'
import { account, json, reply, serve, stubPage, syncOff, syncing } from './support'

const ONE = 'acc_0000000000000001'
/** The mailbox rows as a person reads them: each piece of text, in order. */
const rows = () => page.querySelectorAll('.storage-list strong, .storage-list small, .storage-list dt, .storage-list dd').map(element => words(element)).join(' ')
let mounted: Mounted | null = null

beforeEach(() => { stubPage() })
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the Storage section on a page', () => {
  it('reads its figures again when sync is turned off or on, and never leaves the old ones beside “nothing is indexed”', async () => {
    let consented = true
    const indexed = (): Storage => consented
      ? { mailboxes: [{ account_id: ONE, email: 'ana@gmail.com', messages: 1284, bytes: 52_000_000 }], total: { messages: 1284, bytes: 52_000_000 } }
      : { mailboxes: [{ account_id: ONE, email: 'ana@gmail.com', messages: 0, bytes: 0 }], total: { messages: 0, bytes: 0 } }
    const fetch = serve(({ path, method }) => {
      if (path === '/v1/auth/login') return json(reply())
      if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
      if (path === '/v1/accounts') return json([account({ id: ONE, email: 'ana@gmail.com', state: 'active', sync: consented ? syncing() : syncOff() })])
      if (path === '/v1/me/sync-consent') {
        if (method === 'DELETE') consented = false
        if (method === 'POST') consented = true
        return json(consented
          ? { consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION }
          : { consented: false, current_version: SYNC_TEXT_VERSION })
      }
      if (path === '/v1/me/storage') return json(indexed())
      return json({ code: 'not_found', message: 'no such endpoint' }, 404)
    })
    const reads = () => fetch.mock.calls.filter(([url]) => new URL(String(url)).pathname === '/v1/me/storage').length
    await signIn('ana@example.test', 'correct-password')
    await loadAccounts()
    await loadConsent()
    mounted = mount(StoragePanel)
    await vi.waitFor(() => expect(storage.loaded).toBe(true))
    await flush()
    expect(rows()).toBe('ana@gmail.com Messages 1,284 Size 52 MB')
    expect(reads()).toBe(1)

    // Turned off in Account: the server deletes the index before it answers.
    expect(await withdrawConsent()).toBe(true)
    await vi.waitFor(() => expect(rows()).toBe('ana@gmail.com Not synced: nothing from this mailbox is indexed. Messages 0 Size 0 bytes'))
    expect(page.querySelectorAll('.console-overview strong').map(element => words(element))).toEqual(['1', '0', '0 bytes'])

    // And on again.
    expect(await grantConsent()).toBe(true)
    await vi.waitFor(() => expect(rows()).toBe('ana@gmail.com Messages 1,284 Size 52 MB'))
    expect(reads()).toBeGreaterThanOrEqual(3)
  })
})
