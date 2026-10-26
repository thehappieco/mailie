// Consent to sync, as the console asks for it and takes it back, and asking an
// account for a pass.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '../src/api/types'
import { SYNC_TEXT_VERSION as CONSENT_TEXT_VERSION } from '../src/open/versions'
import { account, failure, freshModules, json, reply, type Route, serve, stubPage, syncing, syncOff } from './support'

const VERSION = CONSENT_TEXT_VERSION
/** A policy revision newer than the text this console carries. */
const NEWER = '2027-01-privacy-bodies'
const notYet = { consented: false, current_version: VERSION }
const given = { consented: true, consented_at: 1_790_000_000, version: VERSION, current_version: VERSION }

async function signedIn(route: Route) {
  await freshModules()
  const session = await import('../src/state/session')
  const accounts = await import('../src/state/accounts')
  const sync = await import('../src/state/sync')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    // A server without workspaces: every list is the person's whole.
    if (request.path === '/v1/workspaces') return failure('not_found', 404)
    return route(request)
  })
  await session.adoptSession(reply())
  return { session, accounts, sync, fetch }
}

const calls = (fetch: ReturnType<typeof serve>, method: string, path: string) =>
  fetch.mock.calls.filter(([url, init]) => new URL(String(url)).pathname === path && (init?.method ?? 'GET') === method)

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('consent to sync', () => {
  it('asks a person who has not agreed, and remembers "not now" only for this page', async () => {
    const { sync } = await signedIn(({ path }) => path === '/v1/me/sync-consent' ? json(notYet) : json([]))
    expect(sync.needsConsent()).toBe(false)
    await sync.loadConsent()
    expect(sync.needsConsent()).toBe(true)
    sync.dismissConsent()
    expect(sync.consent.dismissed).toBe(true)
    // A new page load (a fresh module) asks again: nothing about the answer was stored.
    const again = await signedIn(({ path }) => path === '/v1/me/sync-consent' ? json(notYet) : json([]))
    await again.sync.loadConsent()
    expect(again.sync.consent.dismissed).toBe(false)
    expect(again.sync.needsConsent()).toBe(true)
    expect(JSON.stringify({ ...localStorage_(), ...sessionStorage_() })).not.toContain('dismiss')
  })

  it('agrees to the revision of the text it shows, then reads the accounts again', async () => {
    let enabled = false
    const { accounts, sync, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/me/sync-consent' && method === 'GET') return json(notYet)
      if (path === '/v1/me/sync-consent' && method === 'POST') { enabled = true; return json(given) }
      return json([account({ state: 'active', sync: enabled ? syncing({ state: 'off', running: false, messages: 0, initial_progress: 0 }) : syncOff() })])
    })
    await accounts.loadAccounts()
    await sync.loadConsent()
    expect(await sync.grantConsent()).toBe(true)
    const [post] = calls(fetch, 'POST', '/v1/me/sync-consent')
    expect(JSON.parse(String(post![1]!.body))).toEqual({ version: CONSENT_TEXT_VERSION })
    expect(sync.consent.consented).toBe(true)
    expect(sync.needsConsent()).toBe(false)
    expect(accounts.accounts.notice).toEqual({ kind: 'sync-on' })
    await vi.waitFor(() => expect(accounts.accounts.list[0]!.sync.enabled).toBe(true))
  })

  it('lets folder lists read live give way to the index’s when sync turns on', async () => {
    const live = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 5000 }]
    const indexed = [{ id: 11, name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 1284, sync_state: 'live' }]
    const { accounts, sync } = await signedIn(({ path, method }) => {
      if (path === '/v1/me/sync-consent') return json(method === 'POST' ? given : notYet)
      if (path === '/v1/accounts/acc_0000000000000001/folders') return json(live)
      if (path === '/v1/accounts/acc_0000000000000002/folders') return json(indexed)
      return json([])
    })
    await sync.loadConsent()
    await accounts.loadFolders('acc_0000000000000001')
    await accounts.loadFolders('acc_0000000000000002')
    expect(await sync.grantConsent()).toBe(true)
    // Read again when next wanted, from the index: only an indexed folder has an id Mail can list by.
    expect(accounts.accounts.folders['acc_0000000000000001']).toBeUndefined()
    expect(accounts.accounts.folders['acc_0000000000000002']?.list).toEqual(indexed)
  })

  it('asks again, without calling sync off, when the person agreed to an older text', async () => {
    const { sync } = await signedIn(() => json({ ...given, version: '2026-01-older' }))
    await sync.loadConsent()
    expect(sync.syncOn()).toBe(true)
    expect(sync.needsConsent()).toBe(true)
  })

  it('agrees to nothing once the server names a newer revision than its text, and asks for a reload', async () => {
    // The page was loaded before the daemon moved to a new policy; Refresh
    // reads the consent again and learns of it.
    let current = VERSION
    const { sync, fetch } = await signedIn(({ path }) => path === '/v1/me/sync-consent' ? json({ consented: false, current_version: current }) : json([]))
    await sync.loadConsent()
    expect(sync.consentTextOutdated()).toBe(false)
    current = NEWER
    await sync.loadConsent()
    expect(sync.consentTextOutdated()).toBe(true)
    expect(sync.needsConsent()).toBe(true)
    expect(await sync.grantConsent()).toBe(false)
    expect(calls(fetch, 'POST', '/v1/me/sync-consent')).toHaveLength(0)
    expect(sync.consent.consented).toBe(false)
  })

  it('does not renew consent to a newer revision than its text either', async () => {
    const { sync, fetch } = await signedIn(() => json({ ...given, current_version: NEWER }))
    await sync.loadConsent()
    expect(sync.syncOn()).toBe(true)
    expect(sync.needsConsent()).toBe(true)
    expect(await sync.grantConsent()).toBe(false)
    expect(calls(fetch, 'POST', '/v1/me/sync-consent')).toHaveLength(0)
    expect(sync.consent.version).toBe(VERSION)
  })

  it('says so when the server moved to a newer revision this page has not read yet, and stays off', async () => {
    const { sync, fetch } = await signedIn(({ method }) => method === 'POST' ? failure('bad_request', 400) : json(notYet))
    await sync.loadConsent()
    expect(await sync.grantConsent()).toBe(false)
    const [post] = calls(fetch, 'POST', '/v1/me/sync-consent')
    expect(JSON.parse(String(post![1]!.body))).toEqual({ version: CONSENT_TEXT_VERSION })
    expect(sync.consent.problem).toEqual({ op: 'grant-sync', code: 'bad_request' })
    expect(sync.consent.consented).toBe(false)
    const { describe: describeFailure } = await import('../src/ui/errors')
    expect(describeFailure(sync.consent.problem!)).toBe('The text about sync changed on this server while this page was open. Reload the page to read the current text.')
  })

  it('turns sync off with DELETE, forgets folder lists read from the index, and does not ask again at once', async () => {
    const indexed = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 1284, sync_state: 'live' }]
    const live = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 5000 }]
    const { accounts, sync, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/me/sync-consent') return json(method === 'DELETE' ? notYet : given)
      if (path === '/v1/accounts/acc_0000000000000001/folders') return json(indexed)
      if (path === '/v1/accounts/acc_0000000000000002/folders') return json(live)
      return json([])
    })
    await sync.loadConsent()
    await accounts.loadFolders('acc_0000000000000001')
    await accounts.loadFolders('acc_0000000000000002')
    expect(await sync.withdrawConsent()).toBe(true)
    expect(calls(fetch, 'DELETE', '/v1/me/sync-consent')).toHaveLength(1)
    expect(sync.consent.consented).toBe(false)
    expect(accounts.accounts.folders['acc_0000000000000001']).toBeUndefined()
    expect(accounts.accounts.folders['acc_0000000000000002']?.list).toEqual(live)
    expect(accounts.accounts.notice).toEqual({ kind: 'sync-off' })
    expect(sync.consent.dismissed).toBe(true)
  })

  it('notices sync turned off elsewhere when it reads the consent again, and forgets folder lists read from the index', async () => {
    const indexed = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 1284, sync_state: 'live' }]
    let answer: object = given
    const { accounts, sync } = await signedIn(({ path }) => {
      if (path === '/v1/me/sync-consent') return json(answer)
      if (path === '/v1/accounts/acc_0000000000000001/folders') return json(indexed)
      return json([])
    })
    await sync.loadConsent()
    await accounts.loadFolders('acc_0000000000000001')
    // Another tab turned it off.
    answer = notYet
    await sync.loadConsent()
    expect(sync.consent.consented).toBe(false)
    expect(sync.needsConsent()).toBe(true)
    expect(accounts.accounts.folders['acc_0000000000000001']).toBeUndefined()
  })

  it('waits for the deletion longer than the server may take over it', async () => {
    const { sync } = await signedIn(() => json(notYet))
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    await sync.withdrawConsent()
    // The route allows 150 s for deleting and compacting.
    expect(timeout.mock.calls[0]).toEqual([160_000])
  })

  it('forgets the answer when the person changes', async () => {
    const { session, sync } = await signedIn(() => json(given))
    await sync.loadConsent()
    expect(sync.consent.consented).toBe(true)
    await session.signOut()
    expect(sync.consent.loaded).toBe(false)
    expect(sync.consent.consented).toBe(false)
  })
})

describe('asking for a sync', () => {
  it('asks the account for a pass and takes the status the 202 carries', async () => {
    const before: Account = account({ state: 'active', sync: syncing({ state: 'live', initial_progress: 100, messages: 10 }) })
    const { accounts, sync, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts') return json([before])
      if (path === '/v1/accounts/acc_0000000000000001/sync' && method === 'POST') return json(syncing({ state: 'live', initial_progress: 100, messages: 12 }), 202)
      return failure('not_found', 404)
    })
    await accounts.loadAccounts()
    await sync.syncNow('acc_0000000000000001')
    const [post] = calls(fetch, 'POST', '/v1/accounts/acc_0000000000000001/sync')
    expect(JSON.parse(String(post![1]!.body))).toEqual({})
    expect(accounts.accounts.list[0]!.sync.messages).toBe(12)
    expect(sync.syncRequests['acc_0000000000000001']).toEqual({ busy: false, failure: null, requested: true })
  })

  it('explains a refusal in the console’s words', async () => {
    const { sync } = await signedIn(() => failure('conflict', 409))
    await sync.syncNow('acc_0000000000000001')
    const { describe: describeFailure } = await import('../src/ui/errors')
    const problem = sync.syncRequests['acc_0000000000000001']!.failure!
    expect(problem).toEqual({ op: 'sync-now', code: 'conflict' })
    expect(describeFailure(problem)).toBe('Mailie cannot sync this mailbox right now. Try again in a moment.')
  })
})

describe('the words of an account’s sync', () => {
  const words = async (sync: Account['sync'], state: Account['state'] = 'active') => {
    const { syncView } = await import('../src/ui/sync')
    const { stateDetail } = await import('../src/ui/labels')
    const shown = account({ state, sync })
    return { view: syncView(shown, Date.UTC(2026, 8, 22)), detail: stateDetail(shown) }
  }

  it('a paused mailbox reads Paused, since when, and that its index is kept', async () => {
    const { view, detail } = await words(syncing({ running: false, state: 'stopped', paused: true, paused_since: 1_790_000_000 }))
    expect(view).toEqual({ tone: 'off', label: 'Paused', detail: 'Paused since September 21, 2026. Mailie keeps what it indexed, so it can still be searched and read, but indexes nothing new until sync is resumed.' })
    expect(detail).toBe('Connected. Sync is paused.')
    // Paused whatever else it says: its agreement may not hold now (nobody reads it, its person is off).
    expect((await words(syncOff({ paused: true, paused_since: 1_790_000_000, messages: 4 }))).view?.label).toBe('Paused')
  })

  it('a paused mailbox that kept no index says nothing is indexed and that a resume indexes the last 90 days', async () => {
    const { view } = await words(syncOff({ enabled: true, paused: true, paused_since: 1_790_000_000 }))
    expect(view?.detail).toBe('Paused since September 21, 2026. Nothing of this mailbox is indexed: once sync is resumed, Mailie indexes the last 90 days.')
  })

  it('a held mailbox reads Turned off, never Paused, and names nobody', async () => {
    const { view, detail } = await words(syncOff({ held: true, held_since: 1_790_000_000 }))
    expect(view).toEqual({ tone: 'warn', label: 'Turned off', detail: 'Turned off since September 21, 2026: Mailie keeps no index of this mailbox, and nothing of it syncs until you turn it on again.' })
    expect(detail).toBe('Connected. Sync is off.')
    expect(JSON.stringify(view)).not.toMatch(/[Pp]aused|operator|recovered/)
  })

  it('a mailbox no worker holds yet reads Starting, never Paused', async () => {
    const { view } = await words(syncing({ running: false, state: 'stopped' }))
    expect(view).toEqual({ tone: 'off', label: 'Starting', detail: 'Mailie picks this mailbox up again shortly.' })
  })

  it('pauses and resumes a personal mailbox with no text named, taking the sync each answer carries', async () => {
    const live = syncing({ state: 'live', initial_progress: 100 })
    const paused = { ...live, running: false, state: 'stopped', paused: true as const, paused_since: 1_790_000_000 }
    let current: Account = account({ state: 'active', sync: live })
    const { accounts, sync, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts') return json([current])
      if (path === '/v1/accounts/acc_0000000000000001/sync/pause' && method === 'POST') { current = { ...current, sync: paused }; return json(paused) }
      if (path === '/v1/accounts/acc_0000000000000001/sync/resume' && method === 'POST') { current = { ...current, sync: live }; return json(live) }
      return failure('not_found', 404)
    })
    await accounts.loadAccounts()
    expect(await sync.pauseSync(accounts.accounts.list[0]!)).toBeNull()
    expect(accounts.accounts.list[0]!.sync).toEqual(paused)
    expect(await sync.resumeSync(accounts.accounts.list[0]!)).toBeNull()
    expect(accounts.accounts.list[0]!.sync).toEqual(live)
    for (const path of ['/v1/accounts/acc_0000000000000001/sync/pause', '/v1/accounts/acc_0000000000000001/sync/resume']) {
      const [post] = calls(fetch, 'POST', path)
      expect(JSON.parse(String(post![1]!.body)), path).toEqual({})
    }
    // Neither turns sync on: a pause is not a consent.
    expect(calls(fetch, 'PUT', '/v1/accounts/acc_0000000000000001/sync')).toEqual([])
  })

  it('explains a pause refused for a mailbox whose sync is off, and a resume refused for one nobody reads', async () => {
    const { sync } = await signedIn(({ path }) => path.endsWith('/pause') || path.endsWith('/resume') ? failure('conflict', 409) : failure('not_found', 404))
    const { describe: describeFailure } = await import('../src/ui/errors')
    const pause = await sync.pauseSync(account({ state: 'active' }))
    expect(pause).toEqual({ op: 'pause-sync', code: 'conflict' })
    expect(describeFailure(pause!)).toBe('This mailbox’s sync is off, so there is nothing to pause.')
    const resume = await sync.resumeSync(account({ state: 'active' }))
    expect(describeFailure(resume!)).toBe('Nobody in the team reads this mailbox now, so its sync cannot be resumed until someone does.')
    expect(describeFailure({ op: 'pause-sync', code: 'not_authorized' })).toBe('Only the person whose mailbox it is pauses or resumes its sync; for a team’s mailbox, its owners and admins.')
    expect(describeFailure({ op: 'resume-sync', code: 'not_authorized' })).toBe('Only the person whose mailbox it is pauses or resumes its sync; for a team’s mailbox, its owners and admins.')
  })
})

function localStorage_(): Record<string, string> {
  try { return { ...globalThis.localStorage } } catch { return {} }
}
function sessionStorage_(): Record<string, string> {
  const out: Record<string, string> = {}
  for (let i = 0; i < sessionStorage.length; i++) { const key = sessionStorage.key(i)!; out[key] = sessionStorage.getItem(key)! }
  return out
}
