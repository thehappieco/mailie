// What the person's mailboxes take up, as the core reads it for an edition
// that shows it (state/storage.ts): the server's answer, read again when asked
// while a read is in flight, and never kept for another person.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Storage } from '../src/api/types'
import { failure, freshModules, json, reply, serve, stubPage, type Route } from './support'

const usage: Storage = {
  mailboxes: [
    { account_id: 'acc_0000000000000001', email: 'ana@gmail.com', messages: 1284, bytes: 52_000_000 },
    { account_id: 'acc_0000000000000002', email: '<b>ana</b>@work.example', messages: 0, bytes: 0 },
  ],
  total: { messages: 1284, bytes: 52_000_000 },
}

async function signedIn(route: Route) {
  await freshModules()
  const session = await import('../src/state/session')
  const accounts = await import('../src/state/accounts')
  const storage = await import('../src/state/storage')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    // A server without workspaces: every list is the person's whole.
    if (request.path === '/v1/workspaces') return failure('not_found', 404)
    return route(request)
  })
  await session.adoptSession(reply())
  return { session, accounts, storage, fetch }
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('what the mailboxes take up', () => {
  it('reads once more when asked while a read is in flight, and keeps the later answer', async () => {
    const answers: ((response: Response) => void)[] = []
    const { storage, fetch } = await signedIn(({ path }) => path === '/v1/me/storage' ? new Promise<Response>(done => { answers.push(done) }) : json([]))
    const reads = () => fetch.mock.calls.filter(([url]) => new URL(String(url)).pathname === '/v1/me/storage').length
    const first = storage.loadStorage()
    await vi.waitFor(() => expect(answers).toHaveLength(1))
    // Sync turned off meanwhile: the answer in flight may predate it.
    await storage.loadStorage()
    expect(reads()).toBe(1)
    answers[0]!(json(usage))
    await first
    await vi.waitFor(() => expect(answers).toHaveLength(2))
    expect(storage.storage.usage).toEqual(usage)
    const after: Storage = { mailboxes: usage.mailboxes.map(item => ({ ...item, messages: 0, bytes: 0 })), total: { messages: 0, bytes: 0 } }
    answers[1]!(json(after))
    await vi.waitFor(() => expect(storage.storage.loading).toBe(false))
    expect(storage.storage.usage).toEqual(after)
    expect(reads()).toBe(2)
  })

  it('says it could not read the answer', async () => {
    const { storage } = await signedIn(() => new Response('<html>502</html>', { status: 502 }))
    await storage.loadStorage()
    expect(storage.storage.failure).toEqual({ op: 'load-storage', code: 'unavailable' })
  })

  it('never draws one person’s answer for the next', async () => {
    let answer!: (response: Response) => void
    const { session, storage } = await signedIn(({ path }) => path === '/v1/me/storage' ? new Promise<Response>(done => { answer = done }) : json([]))
    const loading = storage.loadStorage()
    await vi.waitFor(() => expect(answer).toBeDefined())
    await session.signOut()
    answer(json(usage))
    await loading
    expect(storage.storage.usage).toBeNull()
    expect(storage.storage.loaded).toBe(false)
  })
})
