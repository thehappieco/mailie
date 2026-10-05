// The live stream as the console keeps it: reconnecting with Last-Event-ID,
// backing off on failures, never retrying a refused credential, and turning
// events into reads of what changed.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '../src/api/types'
import { account, failure, freshModules, json, reply, stubPage, syncing, syncOff } from './support'

interface Stream { lastEventID: string; token: string; workspace: string; send: (text: string) => void; end: () => void }
type Api = (request: { path: string; method: string; token: string }) => Response | Promise<Response>

const encoder = new TextEncoder()
const event = (seq: number, type: string, accountID = 'acc_0000000000000001', payload: object = {}) =>
  `id: ${seq}\nevent: ${type}\ndata: ${JSON.stringify({ seq, type, account_id: accountID, at: 1_790_000_000, payload: { account_id: accountID, ...payload } })}\n\n`

/**
 * A signed-in console over a fake daemon. /v1/events answers with whatever
 * `events` returns for each connection; by default a stream the test drives.
 */
async function console_(api: Api, events?: (stream: Stream, attempt: number) => Response | Promise<Response> | 'stream') {
  await freshModules()
  const session = await import('../src/state/session')
  const accounts = await import('../src/state/accounts')
  const live = await import('../src/state/live')
  const connection = await import('../src/state/connection')
  const workspaces = await import('../src/state/workspaces')
  live.timing.random = () => 1
  const streams: Stream[] = []
  const fetch = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = new URL(String(input))
    const headers = (init?.headers ?? {}) as Record<string, string>
    const token = (headers.Authorization ?? '').replace(/^Bearer /, '')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/v1/auth/login') return json(reply())
    if (url.pathname === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (url.pathname !== '/v1/events') return api({ path: url.pathname, method, token })
    let controller!: ReadableStreamDefaultController<Uint8Array>
    const body = new ReadableStream<Uint8Array>({ start(c) { controller = c } })
    const closed = { value: false }
    init?.signal?.addEventListener('abort', () => { if (!closed.value) { closed.value = true; controller.error(new DOMException('aborted', 'AbortError')) } })
    const stream: Stream = {
      lastEventID: headers['Last-Event-ID'] ?? '', token, workspace: url.searchParams.get('workspace') ?? '',
      send: text => { if (!closed.value) controller.enqueue(encoder.encode(text)) },
      end: () => { if (!closed.value) { closed.value = true; controller.close() } },
    }
    streams.push(stream)
    const answer = events ? await events(stream, streams.length) : 'stream'
    if (answer !== 'stream') return answer
    stream.send(': connected\n\n')
    return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
  })
  await session.signIn('ana@example.test', 'correct-password')
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] })
  return { session, accounts, live, connection, workspaces, fetch, streams }
}

const gets = (fetch: { mock: { calls: unknown[][] } }, path: string) =>
  fetch.mock.calls.filter(([url, init]) => new URL(String(url)).pathname === path && ((init as RequestInit | undefined)?.method ?? 'GET') === 'GET').length

beforeEach(() => { stubPage() })
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('the live stream', () => {
  it('reconnects after the server ends the stream, resuming after the last event it received', async () => {
    const { live, connection, streams } = await console_(() => json(account({ state: 'active' })))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(streams).toHaveLength(1)
    expect(streams[0]!.lastEventID).toBe('')
    expect(connection.server.stream).toBe('open')
    streams[0]!.send(event(7, 'sync.progress') + event(8, 'sync.progress'))
    streams[0]!.end()
    await vi.advanceTimersByTimeAsync(0)
    expect(connection.server.stream).toBe('retrying')
    expect(live.lastEventID()).toBe('8')
    // A healthy stream ended: the next one is a second away, not a backoff.
    await vi.advanceTimersByTimeAsync(999)
    expect(streams).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(streams).toHaveLength(2)
    expect(streams[1]!.lastEventID).toBe('8')
    live.stopLive()
  })

  it('backs off on failures, 1 s doubling to 30 s, and starts over after a stream that worked', async () => {
    const times: number[] = []
    let healthyAt = 7
    const { live, streams } = await console_(() => json(account({ state: 'active' })), (stream, attempt) => {
      times.push(Date.now())
      if (attempt !== healthyAt) return Promise.reject(new TypeError('network down'))
      setTimeout(() => { stream.send(event(20, 'sync.progress')); stream.end() }, 0)
      return 'stream'
    })
    const start = Date.now()
    live.startLive()
    await vi.advanceTimersByTimeAsync(1_000 + 2_000 + 4_000 + 8_000 + 16_000 + 30_000 + 1)
    expect(streams).toHaveLength(7)
    const gaps = times.slice(1).map((at, i) => at - times[i]!)
    expect(times[0]! - start).toBe(0)
    expect(gaps).toEqual([1_000, 2_000, 4_000, 8_000, 16_000, 30_000])
    // The seventh delivered an event and ended: the eighth is a second later.
    healthyAt = 0
    await vi.advanceTimersByTimeAsync(1_000)
    expect(streams).toHaveLength(8)
    expect(streams[7]!.lastEventID).toBe('20')
    live.stopLive()
  })

  it('waits as long as a rate limit’s Retry-After asks before reconnecting', async () => {
    const { live, connection, streams } = await console_(() => json(account({ state: 'active' })), (_stream, attempt) =>
      attempt === 1 ? failure('rate_limited', 429, { 'Retry-After': '20' }) : 'stream')
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(connection.server.stream).toBe('retrying')
    // The backoff alone would have tried again after a second.
    await vi.advanceTimersByTimeAsync(19_999)
    expect(streams).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(streams).toHaveLength(2)
    expect(connection.server.stream).toBe('open')
    live.stopLive()
  })

  it('never retries a refused credential: the session ends and nothing reconnects', async () => {
    const { session, live, connection, streams } = await console_(() => json([]), () => failure('unauthorized', 401))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(session.session.phase).toBe('signed-out')
    expect(session.session.notice).toBe('expired')
    await vi.advanceTimersByTimeAsync(10 * 60_000)
    expect(streams).toHaveLength(1)
    expect(['off', 'stopped']).toContain(connection.server.stream)
  })

  it('stops when the server says in the stream that the session ended', async () => {
    const { session, live, streams } = await console_(() => json([]))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    streams[0]!.send(event(3, 'sync.progress') + 'event: error\ndata: {"code":"unauthorized","message":"the session has ended"}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(session.session.phase).toBe('signed-out')
    await vi.advanceTimersByTimeAsync(10 * 60_000)
    expect(streams).toHaveLength(1)
  })

  it('reconnects with the new token when a password change replaced the one the stream used', async () => {
    const { session, live, streams } = await console_(({ path, method }) => {
      if (path === '/v1/auth/password' && method === 'POST') return json(reply('tok_second_000000000000000000000000000000000'))
      return json([])
    })
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(streams[0]!.token).toBe(reply().token)
    await session.changePassword('correct-password', 'another-password')
    // The server ended every other session, the stream's included.
    streams[0]!.send('event: error\ndata: {"code":"unauthorized","message":"x"}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(session.session.phase).toBe('ready')
    expect(streams).toHaveLength(2)
    expect(streams[1]!.token).toBe('tok_second_000000000000000000000000000000000')
    live.stopLive()
  })

  it('reads an account again at most once a second, however many events name it', async () => {
    let messages = 0
    const { accounts, live, fetch, streams } = await console_(({ path }) => {
      if (path === '/v1/accounts') return json([account({ state: 'active', sync: syncing({ messages: 0 }) })])
      return json(account({ state: 'active', sync: syncing({ messages: messages }) }))
    })
    await accounts.loadAccounts()
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    messages = 1285
    streams[0]!.send([1, 2, 3, 4, 5].map(seq => event(seq, 'sync.progress')).join(''))
    streams[0]!.send(event(6, 'message.new', 'acc_0000000000000001', { subject: 'Lunch?', folder_role: 'inbox' }))
    await vi.advanceTimersByTimeAsync(999)
    expect(gets(fetch, '/v1/accounts/acc_0000000000000001')).toBe(0)
    await vi.advanceTimersByTimeAsync(1)
    expect(gets(fetch, '/v1/accounts/acc_0000000000000001')).toBe(1)
    expect(accounts.accounts.list[0]!.sync.messages).toBe(1285)
    live.stopLive()
  })

  it('announces a first sync that finished', async () => {
    let state = 'initial'
    const { accounts, live, streams } = await console_(({ path }) => {
      if (path === '/v1/workspaces') return failure('not_found', 404)
      const row = account({ state: 'active', sync: syncing({ state, initial_progress: state === 'live' ? 100 : 42 }) })
      return json(path === '/v1/accounts' ? [row] : row)
    })
    const { announcement } = await import('../src/ui/announce')
    await accounts.loadAccounts()
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    state = 'live'
    streams[0]!.send(event(9, 'folder.changed', 'acc_0000000000000001', { change: 'initial_done' }))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(announcement.value).toBe('suporte@example.test finished its first sync.')
    live.stopLive()
  })

  it('reads every account again when the stream says events were lost', async () => {
    const { live, fetch, streams } = await console_(() => json([]))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(gets(fetch, '/v1/accounts')).toBe(0)
    streams[0]!.send('event: lagged\ndata: {"since":3}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(gets(fetch, '/v1/accounts')).toBe(1)
    live.stopLive()
  })

  it('refreshes a folder list read from the index when its folders change, and leaves a live one of a mailbox that does not sync alone', async () => {
    const indexed = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 3, sync_state: 'live' }]
    const rows = [account({ state: 'active', sync: syncing() }), account({ id: 'acc_0000000000000002', state: 'active', sync: syncOff() })]
    const { accounts, live, fetch, streams } = await console_(({ path }) => {
      if (path.endsWith('/folders')) return json(path.includes('0001') ? indexed : [{ ...indexed[0], sync_state: undefined }])
      if (path === '/v1/accounts') return json(rows)
      return json(rows.find(row => path.endsWith(row.id)))
    })
    const folders = (id: string) => gets(fetch, `/v1/accounts/${id}/folders`)
    await accounts.loadAccounts()
    await accounts.loadFolders('acc_0000000000000001')
    await accounts.loadFolders('acc_0000000000000002')
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    streams[0]!.send(event(1, 'message.new') + event(2, 'message.new', 'acc_0000000000000002'))
    await vi.advanceTimersByTimeAsync(3_000)
    expect(folders('acc_0000000000000001')).toBe(2)
    expect(folders('acc_0000000000000002')).toBe(1)
    live.stopLive()
  })

  it('replaces a live folder list with the index’s once a syncing mailbox has its folders listed', async () => {
    const listed = { name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 3 }
    let answer: object[] = [listed]
    const { accounts, live, fetch, streams } = await console_(({ path }) => {
      if (path.endsWith('/folders')) return json(answer)
      if (path === '/v1/accounts') return json([account({ state: 'active', sync: syncing() })])
      return json(account({ state: 'active', sync: syncing() }))
    })
    const inbox = () => accounts.accounts.folders.acc_0000000000000001?.list.find(folder => folder.role === 'inbox')?.id ?? 0
    await accounts.loadAccounts()
    // Read live: the account sheet listed the folders before the index had them.
    await accounts.loadFolders('acc_0000000000000001')
    expect(inbox()).toBe(0)
    answer = [{ ...listed, id: 11, sync_state: 'new' }]
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    streams[0]!.send(event(1, 'folder.changed', 'acc_0000000000000001', { folder_id: 11, name: 'INBOX', role: 'inbox', change: 'added' }))
    await vi.advanceTimersByTimeAsync(3_000)
    expect(gets(fetch, '/v1/accounts/acc_0000000000000001/folders')).toBe(2)
    expect(inbox()).toBe(11)
    live.stopLive()
  })

  it('hands each event to the handlers an edition registered for its type, and says when events were lost', async () => {
    const { live, streams } = await console_(() => json(account({ state: 'active' })))
    const heard: string[] = []
    live.onLiveEvent('message.new', event => heard.push(`new ${event.seq}`))
    const stop = live.onLiveEvent('send.finished', event => heard.push(`sent ${event.seq}`))
    live.onLiveLagged(() => heard.push('lagged'))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    streams[0]!.send(event(1, 'message.new') + event(2, 'sync.progress') + event(3, 'send.finished'))
    await vi.advanceTimersByTimeAsync(0)
    stop()
    streams[0]!.send(event(4, 'send.finished') + 'event: lagged\ndata: {"since":3}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(heard).toEqual(['new 1', 'sent 3', 'lagged'])
    live.stopLive()
  })

  it('ends a wait for an authorization as soon as an account.state event names the account', async () => {
    let state: Account['state'] = 'pending_auth'
    const loopback = { flow: 'loopback', auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?state=x', state: 'x', expires_at: Math.floor(Date.now() / 1000) + 600 }
    const { accounts, live, fetch, streams } = await console_(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'POST') return json({ account: account(), auth: loopback }, 201)
      return json(account({ state }))
    })
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    await accounts.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(accounts.connect.phase).toBe('waiting')
    // With the stream open the safety poll is slow.
    await vi.advanceTimersByTimeAsync(5_000)
    expect(gets(fetch, '/v1/accounts/acc_0000000000000001')).toBe(0)
    state = 'active'
    streams[0]!.send(event(4, 'account.state', 'acc_0000000000000001', { state: 'active', previous_state: 'pending_auth' }))
    await vi.advanceTimersByTimeAsync(0)
    expect(accounts.connect.phase).toBe('done')
    live.stopLive()
  })

  it('opens the stream narrowed to the workspace shown, and again, from now, for another', async () => {
    const list = [{ id: 'wsp_000000000000aaaa', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1 },
      { id: 'wsp_000000000000bbbb', kind: 'team', source: 'local', name: 'Support', role: 'member', status: 'active', created_at: 1 }]
    const { live, workspaces, streams } = await console_(({ path }) => json(path === '/v1/workspaces' ? list : []))
    await workspaces.loadWorkspaces()
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(streams[0]!.workspace).toBe('wsp_000000000000aaaa')
    streams[0]!.send(event(7, 'sync.progress'))
    await vi.advanceTimersByTimeAsync(0)
    workspaces.selectWorkspace('wsp_000000000000bbbb')
    await vi.advanceTimersByTimeAsync(0)
    expect(streams).toHaveLength(2)
    expect(streams[1]!.workspace).toBe('wsp_000000000000bbbb')
    // The new workspace's list is read whole; its stream does not replay the other's.
    expect(streams[1]!.lastEventID).toBe('')
    live.stopLive()
  })

  it('reads the list again when read access to a mailbox comes or goes, once for several, and drops its folders when it went', async () => {
    const indexed = [{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 3, sync_state: 'live' }]
    const { accounts, live, fetch, streams } = await console_(({ path }) => {
      if (path === '/v1/workspaces') return failure('not_found', 404)
      if (path.endsWith('/folders')) return json(indexed)
      return json(path === '/v1/accounts' ? [account({ state: 'active', sync: syncing() })] : account({ state: 'active', sync: syncing() }))
    })
    const heard: string[] = []
    live.onLiveAccess(change => heard.push(`${change.account_id} ${change.read}`))
    await accounts.loadAccounts()
    await accounts.loadFolders('acc_0000000000000001')
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    const before = gets(fetch, '/v1/accounts')
    // Like lagged, an access change has no id: it is not a journal entry.
    streams[0]!.send('event: access\ndata: {"account_id":"acc_0000000000000001","read":false}\n\n' + 'event: access\ndata: {"account_id":"acc_0000000000000002","read":true}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(heard).toEqual(['acc_0000000000000001 false', 'acc_0000000000000002 true'])
    expect(accounts.accounts.folders.acc_0000000000000001).toBeUndefined()
    expect(live.lastEventID()).toBe('')
    await vi.advanceTimersByTimeAsync(1_000)
    expect(gets(fetch, '/v1/accounts')).toBe(before + 1)
    live.stopLive()
  })

  it('reads again what was read of the team shown when access changes: its people and who holds what', async () => {
    const list = [{ id: 'wsp_000000000000aaaa', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1 },
      { id: 'wsp_000000000000bbbb', kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1 }]
    const { live, workspaces, fetch, streams } = await console_(({ path }) => json(path === '/v1/workspaces' ? list : []))
    const team = await import('../src/state/team')
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace('wsp_000000000000bbbb')
    await Promise.all([team.loadMembers(), team.loadDirectory()])
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    const members = '/v1/workspaces/wsp_000000000000bbbb/members'
    const directory = '/v1/workspaces/wsp_000000000000bbbb/access'
    expect([gets(fetch, members), gets(fetch, directory)]).toEqual([1, 1])
    // A team mailbox removed: whoever read it hears that it went.
    streams[0]!.send('event: access\ndata: {"account_id":"acc_0000000000000001","read":false}\n\n')
    await vi.advanceTimersByTimeAsync(1_000)
    expect([gets(fetch, members), gets(fetch, directory)]).toEqual([2, 2])
    live.stopLive()
  })

  it('reads the person’s workspaces again when the stream of one they are no longer in ends, and follows where they are', async () => {
    let list = [{ id: 'wsp_000000000000aaaa', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1 },
      { id: 'wsp_000000000000bbbb', kind: 'team', source: 'local', name: 'Support', role: 'member', status: 'active', created_at: 1 }]
    const { live, workspaces, streams } = await console_(({ path }) => json(path === '/v1/workspaces' ? list : []))
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace('wsp_000000000000bbbb')
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    expect(streams[0]!.workspace).toBe('wsp_000000000000bbbb')
    list = list.slice(0, 1)
    streams[0]!.send('event: error\ndata: {"code":"not_found","message":"no such workspace"}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(workspaces.workspaces.currentID).toBe('wsp_000000000000aaaa')
    expect(workspaces.workspaces.lost?.name).toBe('Support')
    await vi.advanceTimersByTimeAsync(0)
    expect(streams.at(-1)!.workspace).toBe('wsp_000000000000aaaa')
    // Never again the one it left.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(streams.filter(stream => stream.workspace === 'wsp_000000000000bbbb')).toHaveLength(1)
    live.stopLive()
  })

  it('stops when the person signs out, and the next person starts from now', async () => {
    const { session, live, connection, streams } = await console_(() => json([]))
    live.startLive()
    await vi.advanceTimersByTimeAsync(0)
    streams[0]!.send(event(12, 'sync.progress'))
    await vi.advanceTimersByTimeAsync(0)
    expect(live.lastEventID()).toBe('12')
    await session.signOut()
    expect(connection.server.stream).toBe('off')
    expect(live.lastEventID()).toBe('')
    await vi.advanceTimersByTimeAsync(10 * 60_000)
    expect(streams).toHaveLength(1)
  })
})
