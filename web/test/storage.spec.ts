// What the person's mailboxes take up, as the Storage section reads it and
// shows it: the server's answer, never drawn for another person, and the
// database's size only when the server tells it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h } from 'vue'
import { renderToString } from 'vue/server-renderer'
import type { Storage } from '../src/api/types'
import { account, failure, freshModules, json, reply, serve, stubPage, syncOff, syncing, type Route } from './support'

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
  await session.signIn('ana@example.test', 'correct-password')
  return { session, accounts, storage, fetch }
}

async function render(): Promise<string> {
  const { default: StoragePanel } = await import('../src/components/StoragePanel.vue')
  return renderToString(createSSRApp({ render: () => h(StoragePanel) }))
}

const text = (html: string) => html.replace(/<!--[^]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('the Storage section', () => {
  it('reads what each mailbox takes up, and says per mailbox and in total what is indexed and how large it is', async () => {
    const { accounts, storage, fetch } = await signedIn(({ path }) => {
      if (path === '/v1/me/storage') return json(usage)
      return json([account({ state: 'active', sync: syncing() }), account({ id: 'acc_0000000000000002', email: 'ana@work.example', state: 'active' })])
    })
    await accounts.loadAccounts()
    await storage.loadStorage()
    expect(fetch.mock.calls.filter(([url]) => new URL(String(url)).pathname === '/v1/me/storage')).toHaveLength(1)
    const html = await render()
    const words = text(html)
    expect(words).toContain('Mailboxes 2')
    expect(words).toContain('Messages indexed 1,284')
    expect(words).toContain('Size on the mail servers 52 MB')
    expect(words).toContain('ana@gmail.com Messages 1,284 Size 52 MB')
    // A mailbox that does not sync says why it has nothing, and an address is text.
    expect(words).toContain('Not synced: nothing from this mailbox is indexed.')
    expect(html).toContain('&lt;b&gt;ana&lt;/b&gt;@work.example')
    // Only an owner signed in to the console is told the database's size.
    expect(words).not.toContain('Database on disk')
  })

  it('says a mailbox whose account stopped working keeps what was indexed, and only one with sync off has nothing indexed', async () => {
    // The server stops syncing an account that needs authorizing again and
    // keeps its index (service.Storage): its figures are what was indexed before.
    const answer: Storage = {
      mailboxes: [
        { account_id: 'acc_0000000000000001', email: 'ana@gmail.com', messages: 1284, bytes: 52_000_000 },
        { account_id: 'acc_0000000000000002', email: 'ana@outlook.example', messages: 90, bytes: 288_000 },
        { account_id: 'acc_0000000000000003', email: 'ana@work.example', messages: 0, bytes: 0 },
      ],
      total: { messages: 1374, bytes: 52_288_000 },
    }
    const { accounts, storage } = await signedIn(({ path }) => {
      if (path === '/v1/me/storage') return json(answer)
      return json([
        account({ state: 'active', email: 'ana@gmail.com', sync: syncing() }),
        account({ id: 'acc_0000000000000002', email: 'ana@outlook.example', provider: 'microsoft', state: 'needs_reauth', sync: syncOff({ enabled: true }) }),
        account({ id: 'acc_0000000000000003', email: 'ana@work.example', provider: 'imap', state: 'active' }),
      ])
    })
    await accounts.loadAccounts()
    await storage.loadStorage()
    const rows = (await render()).match(/<li[^]*?<\/li>/g)!.map(text)
    expect(rows).toEqual([
      'ana@gmail.com Messages 1,284 Size 52 MB',
      'ana@outlook.example Sync is paused until the account works again. What was indexed is kept. Messages 90 Size 288 kB',
      'ana@work.example Not synced: nothing from this mailbox is indexed. Messages 0 Size 0 bytes',
    ])
  })

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

  it('shows the database’s size when the server tells it', async () => {
    const { storage } = await signedIn(() => json({ ...usage, database_bytes: 3_400_000 }))
    await storage.loadStorage()
    expect(text(await render())).toContain('Database on disk 3.4 MB')
  })

  it('says it could not read the answer, and offers to try again', async () => {
    const { storage } = await signedIn(() => new Response('<html>502</html>', { status: 502 }))
    await storage.loadStorage()
    expect(storage.storage.failure).toEqual({ op: 'load-storage', code: 'unavailable' })
    expect(text(await render())).toContain('Could not read what your mailboxes take up. Try again in a moment. Try again')
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
