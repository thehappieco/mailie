// The workspace the console shows, and what follows it (state/workspaces.ts,
// state/accounts.ts, state/storage.ts, state/team.ts, state/invitation.ts):
// every list narrows to it with ?workspace=, read only once it is known (and
// held, never read whole, while the workspaces cannot be read); a slow answer
// for one workspace never lands in the next; the last one chosen is
// remembered for each person apart, and never asked for once it is gone; a team's people and grants
// change only through the server, with a request that gives exactly what was
// ticked, and are read again whole after any change, since the server works
// out their protections across the team.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { MailboxAccess, Member, TeamInvite, User, Workspace } from '../src/api/types'
import { memberPermissions } from '../src/ui/access'
import { account, ana, failure, freshModules, json, memoryStorage, reply, stubPage } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const BEA = 'usr_00000000000000b2'
const CODE = 'SUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUk'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support: Workspace = { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 }
const full = { read: true, act: true, send: true, manage: true }
const mine = account({ id: 'acc_mine', email: 'ana@gmail.example', state: 'active', workspace_id: PERSONAL, linked_by: ana.id, access: full })
const shared = account({ id: 'acc_shared', email: 'suporte@example.test', state: 'active', workspace_id: TEAM, linked_by: BEA, access: { read: true, act: false, send: false, manage: false } })
const bea: Member = { user_id: BEA, email: 'bea@example.test', name: 'Bea Lima', role: 'member', status: 'active', last_owner: false, links: 1, joined_at: 1_790_000_000 }

interface Request { path: string; method: string; query: Record<string, string>; body: unknown }

/** A fresh page, signed in as Ana (or options.user), over a daemon that answers with route; every request is kept. */
async function page(route: (request: Request) => Response | Promise<Response> | undefined, options: { workspaces?: () => Workspace[] | Response; user?: User } = {}) {
  await freshModules()
  const session = await import('../src/state/session')
  const workspaces = await import('../src/state/workspaces')
  const accounts = await import('../src/state/accounts')
  const storage = await import('../src/state/storage')
  const team = await import('../src/state/team')
  const invitation = await import('../src/state/invitation')
  const requests: Request[] = []
  const listed = options.workspaces ?? (() => [personal, support])
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = new URL(String(input))
    const request: Request = { path: url.pathname, method: init?.method ?? 'GET', query: Object.fromEntries(url.searchParams), body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined }
    requests.push(request)
    if (request.path === '/v1/auth/login') return json(reply(undefined, options.user))
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (request.path === '/v1/workspaces' && request.method === 'GET') {
      const answer = listed()
      return answer instanceof Response ? answer : json(answer)
    }
    return await route(request) ?? failure('not_found', 404)
  })
  await session.signIn('ana@example.test', 'correct-password')
  return { session, workspaces, accounts, storage, team, invitation, requests }
}

/** GET /v1/accounts as the daemon answers it: narrowed by ?workspace=, or every one. */
function listing(request: Request): Response | undefined {
  if (request.path !== '/v1/accounts' || request.method !== 'GET') return undefined
  const every = [mine, shared]
  return json(request.query.workspace ? every.filter(item => item.workspace_id === request.query.workspace) : every)
}
const reads = (requests: Request[], path: string) => requests.filter(request => request.path === path && request.method === 'GET')

let storage: Storage
beforeEach(() => {
  stubPage()
  storage = memoryStorage()
  vi.stubGlobal('localStorage', storage)
})
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('the workspace shown', () => {
  it('is read before any list, the person’s own at first, and every list narrows to it', async () => {
    const { accounts, workspaces, requests } = await page(listing)
    await accounts.loadAccounts()
    expect(requests.map(request => request.path).filter(path => path.startsWith('/v1/') && path !== '/v1/auth/login')).toEqual(['/v1/workspaces', '/v1/accounts'])
    expect(reads(requests, '/v1/accounts')[0]!.query).toEqual({ workspace: PERSONAL })
    expect(workspaces.workspaces.currentID).toBe(PERSONAL)
    expect(accounts.accounts.list.map(item => item.id)).toEqual(['acc_mine'])
  })

  it('reads the list again for the workspace chosen, and opens there on the next visit', async () => {
    const first = await page(listing)
    await first.accounts.loadAccounts()
    first.workspaces.selectWorkspace(TEAM)
    await first.accounts.accountsSettled()
    expect(reads(first.requests, '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM])
    expect(first.accounts.accounts.list.map(item => item.id)).toEqual(['acc_shared'])
    expect(storage.getItem(`mailie_workspace:${ana.id}`)).toBe(TEAM)

    vi.restoreAllMocks()
    const next = await page(listing)
    await next.accounts.loadAccounts()
    expect(reads(next.requests, '/v1/accounts').map(request => request.query.workspace)).toEqual([TEAM])
  })

  it('remembers the workspace shown for each person apart, under their opaque id alone', async () => {
    const BEA_PERSONAL = 'wsp_000000000000cccc'
    const beaUser: User = { id: BEA, email: 'bea@example.test', name: 'Bea Lima', role: 'member', created_at: 1_790_000_000 }
    const first = await page(listing)
    await first.accounts.loadAccounts()
    first.workspaces.selectWorkspace(TEAM)
    await first.accounts.accountsSettled()

    // Bea, in the same team, signs in on the same browser: she opens on her own workspace, not on Ana's choice.
    vi.restoreAllMocks()
    const bea = await page(listing, { user: beaUser, workspaces: () => [{ ...personal, id: BEA_PERSONAL }, { ...support, role: 'member' }] })
    await bea.accounts.loadAccounts()
    expect(bea.workspaces.workspaces.currentID).toBe(BEA_PERSONAL)
    expect(reads(bea.requests, '/v1/accounts').map(request => request.query.workspace)).toEqual([BEA_PERSONAL])

    // Ana's is still hers.
    vi.restoreAllMocks()
    const again = await page(listing)
    await again.accounts.loadAccounts()
    expect(again.workspaces.workspaces.currentID).toBe(TEAM)

    // Nothing in storage names either of them but by id: no address, no name, nothing shared by the browser.
    const kept = Array.from({ length: storage.length }, (_, index) => storage.key(index)!).filter(key => key.startsWith('mailie_workspace'))
    expect(kept).toEqual([`mailie_workspace:${ana.id}`])
    expect(JSON.stringify(Object.fromEntries(kept.map(key => [key, storage.getItem(key)])))).not.toMatch(/@|Ana|Bea/)
  })

  it('never asks for a remembered workspace the person is no longer in', async () => {
    storage.setItem(`mailie_workspace:${ana.id}`, 'wsp_0000000000000gone')
    const { accounts, requests } = await page(listing)
    await accounts.loadAccounts()
    expect(reads(requests, '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL])
    expect(JSON.stringify(requests)).not.toContain('gone')
  })

  it('lists every mailbox together on a server without workspaces', async () => {
    const { accounts, workspaces, requests } = await page(listing, { workspaces: () => failure('not_found', 404) })
    await accounts.loadAccounts()
    expect(workspaces.workspaces.supported).toBe(false)
    expect(reads(requests, '/v1/accounts')[0]!.query).toEqual({})
    expect(accounts.accounts.list).toHaveLength(2)
  })

  it('keeps a mailbox of another workspace out of the list shown, whatever answer names it', async () => {
    const { accounts } = await page(request => listing(request) ?? (request.path === `/v1/accounts/${shared.id}` ? json(shared) : undefined))
    await accounts.loadAccounts()
    await accounts.refreshAccount(shared.id)
    expect(accounts.accounts.list.map(item => item.id)).toEqual(['acc_mine'])
  })

  it('shows another workspace when the one shown goes away, and says which went', async () => {
    let list = [personal, support]
    const { accounts, workspaces, requests } = await page(listing, { workspaces: () => list })
    await accounts.loadAccounts()
    workspaces.selectWorkspace(TEAM)
    await accounts.accountsSettled()
    list = [personal]
    await workspaces.loadWorkspaces()
    await accounts.accountsSettled()
    expect(workspaces.workspaces.currentID).toBe(PERSONAL)
    expect(workspaces.workspaces.lost?.name).toBe('Support')
    expect(reads(requests, '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM, PERSONAL])
    expect(accounts.accounts.list.map(item => item.id)).toEqual(['acc_mine'])
  })

  it('reads storage for the workspace shown, and again for another', async () => {
    const { storage: usage, workspaces, requests } = await page(request => listing(request)
      ?? (request.path === '/v1/me/storage' ? json({ mailboxes: [], workspaces: [], total: { messages: 0, bytes: 0 } }) : undefined))
    await usage.loadStorage()
    expect(usage.storage.workspace).toBe(PERSONAL)
    workspaces.selectWorkspace(TEAM)
    await vi.waitFor(() => expect(usage.storage.workspace).toBe(TEAM))
    expect(reads(requests, '/v1/me/storage').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM])
  })
})

describe('when the workspaces cannot be read', () => {
  const usage = (workspace: string) => ({ mailboxes: [], workspaces: [{ workspace_id: workspace, mailboxes: 0, messages: 0, bytes: 0 }], total: { messages: 0, bytes: 0 } })
  const storageOf = (request: Request) => request.path === '/v1/me/storage' ? json(usage(request.query.workspace ?? '')) : undefined

  it('holds every list rather than read one without its workspace, says why, and reads it once they load', async () => {
    let down = true
    const { accounts, storage: usageStore, workspaces, requests } = await page(request => listing(request) ?? storageOf(request),
      { workspaces: () => down ? failure('internal', 500) : [personal, support] })
    await accounts.loadAccounts()
    await usageStore.loadStorage()
    // A team's mailboxes listed with the person's own would read as theirs.
    expect(reads(requests, '/v1/accounts')).toEqual([])
    expect(reads(requests, '/v1/me/storage')).toEqual([])
    expect(accounts.accounts.failure).toEqual({ op: 'load-workspaces', code: 'internal' })
    expect(usageStore.storage.failure).toEqual({ op: 'load-workspaces', code: 'internal' })
    down = false
    await workspaces.loadWorkspaces()
    await accounts.accountsSettled()
    await vi.waitFor(() => expect(usageStore.storage.loaded).toBe(true))
    expect(reads(requests, '/v1/accounts').map(request => request.query)).toEqual([{ workspace: PERSONAL }])
    expect(reads(requests, '/v1/me/storage').map(request => request.query)).toEqual([{ workspace: PERSONAL }])
    expect(accounts.accounts.list.map(item => item.id)).toEqual(['acc_mine'])
  })

  it('tries them again by itself while the console is shown, backing off, as long as a rate limit asks', async () => {
    const answers = [failure('internal', 500), failure('rate_limited', 429, { 'Retry-After': '5' })]
    const { accounts, workspaces, requests } = await page(listing, { workspaces: () => answers.shift() ?? [personal, support] })
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      const stop = workspaces.retryWorkspaces()
      await accounts.loadAccounts()
      const asked = () => reads(requests, '/v1/workspaces').length
      expect(asked()).toBe(1)
      await vi.advanceTimersByTimeAsync(1_999)
      expect(asked()).toBe(1)
      await vi.advanceTimersByTimeAsync(1)
      expect(asked()).toBe(2)
      // 4 s would be next; the server asked for 5.
      await vi.advanceTimersByTimeAsync(4_999)
      expect(asked()).toBe(2)
      await vi.advanceTimersByTimeAsync(1)
      expect(asked()).toBe(3)
      await vi.waitFor(() => expect(workspaces.workspaces.loaded).toBe(true))
      await accounts.accountsSettled()
      expect(accounts.accounts.failure).toBeNull()
      expect(reads(requests, '/v1/accounts').map(request => request.query)).toEqual([{ workspace: PERSONAL }])
      stop()
    } finally {
      vi.useRealTimers()
    }
  })

  it('waits for someone to ask again once the console is no longer shown', async () => {
    const { workspaces, requests } = await page(listing, { workspaces: () => failure('internal', 500) })
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      workspaces.retryWorkspaces()()
      await workspaces.loadWorkspaces()
      await vi.advanceTimersByTimeAsync(60_000)
      expect(reads(requests, '/v1/workspaces')).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('never draws a slow answer for one workspace over the list or the storage of the one shown since', async () => {
    const held: Record<string, (response: Response) => void> = {}
    const { accounts, storage: usageStore, workspaces } = await page(request => {
      if (request.query.workspace === PERSONAL && (request.path === '/v1/accounts' || request.path === '/v1/me/storage')) {
        return new Promise<Response>(resolve => { held[request.path] = resolve })
      }
      return listing(request) ?? storageOf(request)
    })
    await workspaces.loadWorkspaces()
    const personalList = accounts.loadAccounts()
    const personalUsage = usageStore.loadStorage()
    await vi.waitFor(() => expect(Object.keys(held)).toHaveLength(2))
    workspaces.selectWorkspace(TEAM)
    await accounts.accountsSettled()
    await vi.waitFor(() => expect(usageStore.storage.loaded).toBe(true))
    held['/v1/accounts']!(json([mine]))
    held['/v1/me/storage']!(json(usage(PERSONAL)))
    await Promise.all([personalList, personalUsage])
    expect(accounts.accounts.workspace).toBe(TEAM)
    expect(accounts.accounts.list.map(item => item.id)).toEqual(['acc_shared'])
    expect(usageStore.storage.workspace).toBe(TEAM)
    expect(usageStore.storage.usage?.workspaces?.[0]?.workspace_id).toBe(TEAM)
  })
})

describe('an invitation to a team, accepted signed in', () => {
  it('joins the team it names and shows it, and is forgotten', async () => {
    const { invitation, workspaces, requests } = await page(request => request.path === '/v1/auth/invites/accept' ? json({ ...support, role: 'member' }) : listing(request))
    invitation.holdInvitation({ invite: CODE, email: 'ana@example.test' })
    expect(invitation.invitationFits('Ana@Example.test')).toBe(true)
    expect(await invitation.acceptInvitation()).toBe(true)
    expect(requests.find(request => request.path === '/v1/auth/invites/accept')?.body).toEqual({ invite: CODE })
    expect(workspaces.workspaces.currentID).toBe(TEAM)
    expect(invitation.invitation.joined?.name).toBe('Support')
    expect(invitation.invitation.pending).toBeNull()
  })

  it('stays, unspent, when the server refuses it, and never fits another address', async () => {
    const { invitation, workspaces } = await page(request => request.path === '/v1/auth/invites/accept' ? failure('not_authorized', 403) : listing(request))
    invitation.holdInvitation({ invite: CODE, email: 'bea@example.test' })
    expect(invitation.invitationFits('ana@example.test')).toBe(false)
    expect(await invitation.acceptInvitation()).toBe(false)
    expect(invitation.invitation.pending?.invite).toBe(CODE)
    expect(invitation.invitation.problem).toEqual({ op: 'accept-invite', code: 'not_authorized' })
    expect(workspaces.workspaces.currentID).not.toBe(TEAM)
  })
})

describe('a team’s people and grants', () => {
  /** Ana administers Support, shown. */
  async function inTeam(route: (request: Request) => Response | undefined) {
    const shown = await page(request => route(request) ?? listing(request))
    await shown.workspaces.loadWorkspaces()
    shown.workspaces.selectWorkspace(TEAM)
    await shown.accounts.loadAccounts()
    return shown
  }
  const directory = (grant: object): MailboxAccess[] => [{ account_id: shared.id, email: shared.email, provider: 'imap', state: 'active', linked_by: BEA, grants: [{ account_id: shared.id, user_id: BEA, ...full, updated_at: 1 }, { account_id: shared.id, user_id: ana.id, read: true, act: false, send: false, manage: false, ...grant, updated_at: 1 }] }]

  it('gives with the whole grant, and takes flags away with a revoke naming them', async () => {
    const { team, requests } = await inTeam(request => {
      if (request.path === `/v1/workspaces/${TEAM}/access`) return json(directory({}))
      if (request.path === `/v1/accounts/${shared.id}/access/${BEA}`) return request.method === 'PUT' ? json({ account_id: shared.id, user_id: BEA, ...full, updated_at: 2 }) : new Response(null, { status: 204 })
      return undefined
    })
    const before = { read: true, act: false, send: true, manage: false }
    expect(await team.saveGrant(shared.id, BEA, before, { ...before, act: true })).toBeNull()
    expect(await team.saveGrant(shared.id, BEA, before, { ...before, send: false })).toBeNull()
    expect(await team.saveGrant(shared.id, BEA, before, { read: false, act: false, send: false, manage: false })).toBeNull()
    const changes = requests.filter(request => request.path.includes('/access/'))
    expect(changes.map(request => [request.method, request.query, request.body])).toEqual([
      ['PUT', {}, { read: true, act: true, send: true, manage: false }],
      ['DELETE', { flags: 'send' }, undefined],
      ['DELETE', {}, undefined],
    ])
    // The directory is read again after each change, never guessed.
    expect(reads(requests, `/v1/workspaces/${TEAM}/access`)).toHaveLength(3)
  })

  it('reads the caller’s own card again when their own grant changes', async () => {
    const { team, accounts, requests } = await inTeam(request => {
      if (request.path === `/v1/workspaces/${TEAM}/access`) return json(directory({ send: true }))
      if (request.path === `/v1/accounts/${shared.id}/access/${ana.id}`) return new Response(null, { status: 204 })
      if (request.path === `/v1/accounts/${shared.id}`) return json({ ...shared, access: { read: true, act: false, send: false, manage: false } })
      return undefined
    })
    await team.saveGrant(shared.id, ana.id, { read: true, act: false, send: true, manage: false }, { read: true, act: false, send: false, manage: false })
    expect(reads(requests, `/v1/accounts/${shared.id}`)).toHaveLength(1)
    expect(accounts.accounts.list.find(item => item.id === shared.id)?.access?.send).toBe(false)
  })

  it('takes a link over, and keeps the mailbox’s card', async () => {
    const { team, accounts, requests } = await inTeam(request => {
      if (request.path === `/v1/accounts/${shared.id}/take-over`) return json({ ...shared, linked_by: ana.id, access: full })
      if (request.path === `/v1/workspaces/${TEAM}/access` || request.path === `/v1/workspaces/${TEAM}/members`) return json([])
      return undefined
    })
    expect(await team.takeOverLink(shared.id)).toBeNull()
    expect(requests.find(request => request.path.endsWith('/take-over'))?.method).toBe('POST')
    expect(accounts.accounts.list.find(item => item.id === shared.id)?.linked_by).toBe(ana.id)
  })

  it('hands an invitation’s link to the caller alone, and keeps it nowhere', async () => {
    const made: TeamInvite = { id: 'inv_0000000000000001', email: 'carol@example.test', workspace_id: TEAM, role: 'member', url: `http://localhost:5174/#invite=${CODE}&email=carol%40example.test`, created_by: ana.id, created_at: 1, expires_at: 2 }
    const { team, requests } = await inTeam(request => {
      if (request.path === `/v1/workspaces/${TEAM}/invites` && request.method === 'POST') return json(made, 201)
      if (request.path === `/v1/workspaces/${TEAM}/invites`) return json([{ ...made, url: undefined }])
      return undefined
    })
    const outcome = await team.inviteMember('carol@example.test', 'member')
    expect('invite' in outcome && outcome.invite.url).toContain(CODE)
    expect(requests.find(request => request.method === 'POST' && request.path.endsWith('/invites'))?.body).toEqual({ email: 'carol@example.test', role: 'member' })
    await vi.waitFor(() => expect(team.team.invites.loaded).toBe(true))
    expect(JSON.stringify(team.team)).not.toContain(CODE)
    const stored = Array.from({ length: storage.length }, (_, i) => storage.getItem(storage.key(i)!) ?? '')
    expect(stored.join(' ')).not.toContain(CODE)
  })

  it('refuses a link that is not one a person may be handed', async () => {
    const { team } = await inTeam(request => request.path === `/v1/workspaces/${TEAM}/invites` && request.method === 'POST'
      ? json({ id: 'inv_0000000000000001', email: 'carol@example.test', workspace_id: TEAM, role: 'member', url: 'javascript:alert(1)', created_at: 1, expires_at: 2 }, 201) : undefined)
    const outcome = await team.inviteMember('carol@example.test', 'member')
    expect(outcome).toEqual({ failure: { op: 'create-invite', code: 'invalid_response' } })
  })

  it('leaves the team, and shows the person another of their workspaces', async () => {
    let list = [personal, support]
    const { team, workspaces, requests } = await page(request => {
      if (request.path === `/v1/workspaces/${TEAM}/members/${ana.id}` && request.method === 'DELETE') { list = [personal]; return new Response(null, { status: 204 }) }
      return listing(request)
    }, { workspaces: () => list })
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace(TEAM)
    expect(await team.leaveTeam()).toBeNull()
    expect(requests.some(request => request.method === 'DELETE' && request.path === `/v1/workspaces/${TEAM}/members/${ana.id}`)).toBe(true)
    expect(workspaces.workspaces.currentID).toBe(PERSONAL)
    expect(workspaces.workspaces.lost?.id).toBe(TEAM)
  })

  it('reads the members again after a change, so the caller hears that they are now the last owner', async () => {
    // Ana and Bea own Support. Ana makes Bea a member: only the list read
    // again says that Ana is now the one owner, who may neither leave nor
    // step down.
    let members: Member[] = [
      { ...bea, user_id: ana.id, email: ana.email, name: ana.name, role: 'owner', links: 0 },
      { ...bea, role: 'owner', links: 0 },
    ]
    const { team, workspaces } = await page(request => {
      if (request.path === `/v1/workspaces/${TEAM}/members` && request.method === 'GET') return json(members)
      if (request.path === `/v1/workspaces/${TEAM}/members/${BEA}` && request.method === 'PATCH') {
        members = [{ ...members[0]!, last_owner: true }, { ...members[1]!, role: 'member' }]
        return json(members[1])
      }
      return listing(request)
    }, { workspaces: () => [personal, { ...support, role: 'owner' }] })
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace(TEAM)
    await team.loadMembers()
    expect(memberPermissions(ana.id, 'owner', team.memberOf(ana.id)!).canLeave).toBe(true)
    expect(await team.changeMember(BEA, { role: 'member' })).toBeNull()
    await vi.waitFor(() => expect(team.memberOf(ana.id)?.last_owner).toBe(true))
    const mine = memberPermissions(ana.id, 'owner', team.memberOf(ana.id)!)
    expect(mine.canLeave).toBe(false)
    expect(mine.roles).toEqual(['owner'])
  })

  it('follows the caller’s own role as their row lists it, and reads their workspaces again when their role refuses them', async () => {
    let role = 'admin'
    const { team, workspaces, requests } = await page(request => {
      if (request.path === `/v1/workspaces/${TEAM}/members`) return json([{ ...bea, user_id: ana.id, email: ana.email, name: ana.name, role, links: 0 }, bea])
      if (request.path === `/v1/accounts/${shared.id}/access/${BEA}`) return failure('not_authorized', 403)
      return listing(request)
    }, { workspaces: () => [personal, { ...support, role }] })
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace(TEAM)
    // Another owner made Ana a member, here or elsewhere.
    role = 'member'
    await team.loadMembers()
    expect(workspaces.currentWorkspace()?.role).toBe('member')

    role = 'admin'
    const before = reads(requests, '/v1/workspaces').length
    const refused = await team.saveGrant(shared.id, BEA, { read: false, act: false, send: false, manage: false }, { read: false, act: false, send: false, manage: true })
    expect(refused).toEqual({ op: 'change-access', code: 'not_authorized' })
    await vi.waitFor(() => expect(reads(requests, '/v1/workspaces')).toHaveLength(before + 1))
    await vi.waitFor(() => expect(workspaces.currentWorkspace()?.role).toBe('admin'))
  })

  it('reads the team again when one of its mailboxes leaves the person’s list: the directory drops it, and its linker is no longer kept', async () => {
    const linked = account({ id: 'acc_linked', email: 'vendas@example.test', state: 'active', workspace_id: TEAM, linked_by: ana.id, access: full })
    let removed = false
    const { team, accounts, workspaces, requests } = await page(request => {
      if (request.path === `/v1/accounts/${linked.id}` && request.method === 'DELETE') { removed = true; return new Response(null, { status: 204 }) }
      if (request.path === '/v1/accounts' && request.method === 'GET') return json(removed ? [] : [linked])
      if (request.path === `/v1/workspaces/${TEAM}/members`) return json([{ ...bea, user_id: ana.id, email: ana.email, name: ana.name, role: 'admin', links: removed ? 0 : 1 }])
      if (request.path === `/v1/workspaces/${TEAM}/access`) {
        return json(removed ? [] : [{ account_id: linked.id, email: linked.email, provider: 'gmail', state: 'active', linked_by: ana.id, grants: [{ account_id: linked.id, user_id: ana.id, ...full, updated_at: 1 }] }])
      }
      return undefined
    })
    await workspaces.loadWorkspaces()
    workspaces.selectWorkspace(TEAM)
    await accounts.loadAccounts()
    await Promise.all([team.loadMembers(), team.loadDirectory()])
    expect(team.memberOf(ana.id)?.links).toBe(1)
    expect(await accounts.removeAccount(linked.id)).toBe(true)
    await vi.waitFor(() => expect(team.team.directory.list).toEqual([]))
    await vi.waitFor(() => expect(team.memberOf(ana.id)?.links).toBe(0))
    expect(reads(requests, `/v1/workspaces/${TEAM}/access`)).toHaveLength(2)
    expect(reads(requests, `/v1/workspaces/${TEAM}/members`)).toHaveLength(2)
  })

  it('marks who is in the team by name, and its people reset with the workspace shown', async () => {
    const { team, workspaces } = await inTeam(request => request.path === `/v1/workspaces/${TEAM}/members` ? json([bea]) : undefined)
    await team.loadMembers()
    expect(team.personName(BEA)).toBe('Bea Lima')
    expect(team.personName(ana.id)).toBe(ana.name)
    workspaces.selectWorkspace(PERSONAL)
    expect(team.team.members.list).toEqual([])
    // A personal workspace has no people to read.
    await team.loadMembers()
    expect(team.team.members.loaded).toBe(false)
  })
})
