// Workspaces and teams as a person presses them, on a page (test/dom.ts):
// choosing the workspace reads its mailboxes; connecting a mailbox into a
// team shows that team first; a grant is ticked and saved with the request
// that gives exactly that; taking a link over asks first; the team's people
// change only after a question, and a refusal is said in the console's words;
// an invitation's link is shown once, copied on request and forgotten; an
// invitation opened signed in joins the team; turning sync off names the team
// mailboxes whose index goes with it, before it may be confirmed; a mailbox
// connected from a team shown goes into it, once the person agreed to the
// current text of sync; a change of people reads them again, and the
// person's own role follows what the server says now; the header names the
// workspace shown on its sections only, never on the person's own; a member
// of a team is offered no connecting there, and their personal workspace
// instead; and the Members line names invitations only to whoever sees them.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { check, click, fill, find, fire, flush, keydown, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Account, MailboxAccess, Member, Workspace } from '../src/api/types'
import AccessPanel from '../src/components/AccessPanel.vue'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import AddAccountDialog from '../src/components/AddAccountDialog.vue'
import InvitationDialog from '../src/components/InvitationDialog.vue'
import SyncPermission from '../src/components/SyncPermission.vue'
import WorkspaceSwitcher from '../src/components/WorkspaceSwitcher.vue'
import OpenConsole from '../src/open/OpenConsole.vue'
import OpenMembers from '../src/open/OpenMembers.vue'
import { SYNC_TEXT_VERSION } from '../src/open/versions'
import { accounts, loadAccounts, loadProviders } from '../src/state/accounts'
import { holdInvitation, invitation } from '../src/state/invitation'
import { session, signIn, signOut } from '../src/state/session'
import { loadConsent } from '../src/state/sync'
import { loadDirectory, loadMembers, team } from '../src/state/team'
import { loadWorkspaces, selectWorkspace, workspaces } from '../src/state/workspaces'
import { account, ana, failure, json, reply, serve, stubPage, syncing, type Route } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const BEA = 'usr_00000000000000b2'
const CAROL = 'usr_00000000000000c3'
const CODE = 'SUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUk'
const LINK = `http://localhost:5174/#invite=${CODE}&email=dan%40example.test`
const full = { read: true, act: true, send: true, manage: true }
const none = { read: false, act: false, send: false, manage: false }
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })
const member = (userID: string, fields: Partial<Member> = {}): Member => ({ user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, links: 0, joined_at: 1_790_000_000, ...fields })
const shared = account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, linked_by: BEA, access: full, sync: syncing() })

interface Seen { path: string; method: string; query: Record<string, string>; body: unknown }
let seen: Seen[] = []
let mounted: Mounted | null = null
let clipboard: string[] = []

/** Signed in as Ana, with these workspaces and the team shown; route answers the rest. */
async function signedIn(role: string, route: Route, list: () => Workspace[] = () => [personal, support(role)]) {
  serve(request => {
    const url = new URL(request.url)
    seen.push({ path: request.path, method: request.method, query: Object.fromEntries(url.searchParams), body: request.body })
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (request.path === '/v1/workspaces' && request.method === 'GET') return json(list())
    return route(request)
  })
  await signIn('ana@example.test', 'correct-password')
  await loadWorkspaces()
  selectWorkspace(TEAM)
}
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)

beforeEach(() => {
  stubPage()
  seen = []
  clipboard = []
  vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async (text: string) => { clipboard.push(text) }) } })
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('choosing the workspace', () => {
  it('reads the mailboxes of the one chosen, and only them', async () => {
    await signedIn('member', ({ path, method }) => path === '/v1/accounts' && method === 'GET' ? json([]) : failure('not_found', 404))
    selectWorkspace(PERSONAL)
    await loadAccounts()
    mounted = mount(WorkspaceSwitcher)
    await flush()
    const select = find('select[name=workspace]')!
    expect(select.querySelectorAll('option').map(option => words(option))).toEqual(['Personal', 'Support · Member'])
    select.value = TEAM
    await fire(select, 'change')
    await vi.waitFor(() => expect(accounts.workspace).toBe(TEAM))
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM])
  })
})

describe('the console’s frame in a team', () => {
  /** Ana, with the role given in Support, shown, on the whole open console; the event stream is refused, so it stops. */
  async function consoleIn(role: string) {
    await signedIn(role, ({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([shared])
      if (path === '/v1/providers') return json([])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role }), member(BEA, { name: 'Bea Lima', links: 1 })])
      if (path === `/v1/workspaces/${TEAM}/invites`) return json([])
      if (path === `/v1/workspaces/${TEAM}/access`) return json([])
      if (path === '/v1/events') return failure('not_authorized', 403)
      return failure('not_found', 404)
    })
    mounted = mount(OpenConsole)
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    await flush()
  }
  const crumb = () => words(find('.console-breadcrumb')!)
  const intro = () => words(find('.console-section-intro p')!)
  const open = (label: string) => click(find('.console-sidebar .console-nav-item', label))

  it('names the workspace shown on its own sections, and none on the person’s: their account and their API keys', async () => {
    await consoleIn('admin')
    expect(crumb()).toBe('Console / Support / Mailboxes')
    await open('Account')
    expect(crumb()).toBe('Console / Account')
    await open('API keys & MCP')
    expect(crumb()).toBe('Console / API keys & MCP')
    await open('Storage')
    expect(crumb()).toBe('Console / Support / Storage')
    await open('Members')
    expect(crumb()).toBe('Console / Support / Members')
    // The profile button in the sidebar opens the same section as Account.
    await click(find('.console-sidebar .profile-trigger'))
    expect(crumb()).toBe('Console / Account')
  })

  it('names the invitations in the Members line only for whoever sees them: an owner or an admin, not a member', async () => {
    await consoleIn('member')
    await open('Members')
    expect(intro()).toBe('Who is in Support and their roles.')
    expect(words()).not.toContain('Pending invitations')
    mounted?.unmount()
    mounted = null
    await signOut()
    seen = []
    await consoleIn('admin')
    await open('Members')
    expect(intro()).toBe('Who is in Support, their roles, and the invitations to join it.')
    await vi.waitFor(() => expect(words()).toContain('Pending invitations'))
  })

  it('offers a member of a team nothing to connect there, and shows their personal workspace on request', async () => {
    await signedIn('member', ({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name })])
      return failure('not_found', 404)
    })
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await flush()
    expect(words()).toContain('Nothing shared with you here yet')
    expect(find('button', 'Connect an email account')).toBeNull()
    await click(find('.empty-card button', 'Show your personal workspace'))
    expect(workspaces.currentID).toBe(PERSONAL)
    await vi.waitFor(() => expect(accounts.workspace).toBe(PERSONAL))
    await flush()
    // Her own workspace, where she may connect one.
    expect(words()).toContain('Connect your first email account')
    expect(find('.empty-card button', 'Connect an email account')).not.toBeNull()
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([TEAM, PERSONAL])
  })

  it('offers no connecting beside the mailboxes shared with a member of a team, and does to its admin', async () => {
    await consoleIn('member')
    expect(words(find('.cards')!)).toContain('suporte@example.test')
    expect(find('button', 'Connect an email account')).toBeNull()
    mounted?.unmount()
    mounted = null
    await signOut()
    seen = []
    await consoleIn('admin')
    expect(find('.section-actions button', 'Connect an email account')).not.toBeNull()
  })
})

describe('connecting a mailbox into a team', () => {
  it('asks where, the personal workspace first, and shows the team chosen before connecting into it', async () => {
    const posted: unknown[] = []
    await signedIn('admin', ({ path, method, body }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([])
      if (path === '/v1/providers') return json([{ id: 'imap', oauth: false, password: true, flows: [] }])
      if (path === '/v1/accounts' && method === 'POST') {
        posted.push(body)
        return json({ account: account({ id: 'acc_new', email: 'vendas@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, linked_by: ana.id, access: full }) }, 201)
      }
      return failure('not_found', 404)
    })
    selectWorkspace(PERSONAL)
    await loadAccounts()
    await loadProviders()
    mounted = mount(AddAccountDialog, { resume: null })
    await flush()
    await click(find('dialog .provider-option', 'Other provider (IMAP)'))
    const places = page.querySelectorAll('dialog .place-choice input[name=workspace]')
    expect(places.map(input => input.getAttribute('value'))).toEqual([PERSONAL, TEAM])
    expect(find('dialog .place-choice input[name=workspace]')!.checked).toBe(true)
    expect(words(find('dialog .place-choice')!)).toContain('Its owners and admins see that it is there. Only you can use it until you give other members access.')
    await check(places[1]!)
    await fill(find('dialog input[name=mailbox]'), 'vendas@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).toMatchObject({ email: 'vendas@example.test', provider: 'imap', workspace_id: TEAM })
    // The team was shown first: its list was read, and the new mailbox is in it.
    expect(workspaces.currentID).toBe(TEAM)
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM])
    await vi.waitFor(() => expect(accounts.list.map(item => item.id)).toEqual(['acc_new']))
  })
})

describe('connecting a mailbox from a team shown', () => {
  /** Ana administers Support, shown; the server answers a password mailbox as linked where it was asked. */
  async function connecting(consent: object, posted: unknown[]) {
    await signedIn('admin', ({ path, method, body }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([])
      if (path === '/v1/providers') return json([{ id: 'imap', oauth: false, password: true, flows: [] }])
      if (path === '/v1/me/sync-consent' && method === 'GET') return json(consent)
      if (path === '/v1/me/sync-consent' && method === 'POST') return json({ consented: true, consented_at: 1_790_000_100, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/accounts' && method === 'POST') {
        posted.push(body)
        const into = (body as { workspace_id?: string }).workspace_id ?? PERSONAL
        return json({ account: account({ id: 'acc_new', email: 'vendas@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: into, linked_by: ana.id, access: full }) }, 201)
      }
      return failure('not_found', 404)
    })
    await Promise.all([loadAccounts(), loadProviders(), loadConsent()])
    mounted = mount(AddAccountDialog, { resume: null })
    await flush()
    await click(find('dialog .provider-option', 'Other provider (IMAP)'))
  }
  const placeOf = (id: string) => find(`dialog .place-choice input[name=workspace][value=${id}]`)!
  const connectButton = () => find('dialog form button[type=submit]')!

  it('connects into the team shown, as its empty list says, unless the person picks another', async () => {
    const posted: unknown[] = []
    await connecting({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION }, posted)
    expect(placeOf(TEAM).checked).toBe(true)
    expect(placeOf(PERSONAL).checked).toBe(false)
    await fill(find('dialog input[name=mailbox]'), 'vendas@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).toMatchObject({ workspace_id: TEAM })
    // It stays where it was connected: no other workspace was shown for it.
    expect(workspaces.currentID).toBe(TEAM)
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([TEAM])
    await vi.waitFor(() => expect(accounts.list.map(item => item.id)).toEqual(['acc_new']))
  })

  it('asks someone who agreed to an earlier text of sync to agree to the current one before linking into a team, and only then', async () => {
    const posted: unknown[] = []
    await connecting({ consented: true, consented_at: 1_780_000_000, version: '2026-01-older', current_version: SYNC_TEXT_VERSION }, posted)
    expect(words(find('dialog .sync-renewal')!)).toContain('A mailbox you connect to Support syncs under your agreement to mail sync, which was to an earlier text. Agree to the current text first.')
    expect(connectButton().disabled).toBe(true)
    // Their personal workspace is what that text covered.
    await check(placeOf(PERSONAL))
    expect(find('dialog .sync-renewal')).toBeNull()
    expect(connectButton().disabled).toBe(false)
    await check(placeOf(TEAM))
    await click(find('dialog .sync-renewal button', 'Read the current text…'))
    // The text itself, then the agreement to its revision.
    expect(words(page.querySelectorAll('dialog').at(-1)!)).toContain('Who can read the index')
    await click(find('dialog button.primary', 'I agree'))
    expect(sent('POST', '/v1/me/sync-consent').map(request => request.body)).toEqual([{ version: SYNC_TEXT_VERSION }])
    await vi.waitFor(() => expect(find('dialog .sync-renewal')).toBeNull())
    expect(connectButton().disabled).toBe(false)
    expect(posted).toEqual([])
  })
})

describe('the team’s other mailboxes', () => {
  it('closes the access dialog of one when the console shows another workspace', async () => {
    const hidden: MailboxAccess = { account_id: 'acc_hidden', email: 'diretoria@example.test', provider: 'gmail', state: 'active', linked_by: BEA, grants: [{ account_id: 'acc_hidden', user_id: BEA, ...full, updated_at: 1 }] }
    await signedIn('admin', ({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role: 'admin' }), member(BEA, { name: 'Bea Lima', links: 1 })])
      if (path === `/v1/workspaces/${TEAM}/access`) return json([hidden])
      return failure('not_found', 404)
    })
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(find('.other-card')).not.toBeNull())
    await click(find('.other-card button', 'Access…'))
    expect(words(find('dialog')!)).toContain('Access to diretoria@example.test')
    selectWorkspace(PERSONAL)
    await flush()
    expect(find('dialog')).toBeNull()
  })
})

describe('who can use a mailbox', () => {
  const directory = (): MailboxAccess[] => [{ account_id: shared.id, email: shared.email, provider: 'imap', state: 'active', linked_by: BEA, grants: grants.map(([userID, flags]) => ({ account_id: shared.id, user_id: userID, ...flags, updated_at: 1 })) }]
  let grants: [string, typeof full][] = []

  /** Ana administers Support and holds every flag on a mailbox Bea linked; Carol holds nothing yet. */
  async function panel(route: Route = () => failure('not_found', 404)) {
    grants = [[BEA, full], [ana.id, full]]
    await signedIn('admin', request => {
      const { path, method } = request
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role: 'admin' }), member(BEA, { name: 'Bea Lima', links: 1 }), member(CAROL, { name: 'Carol Dias' })])
      if (path === `/v1/workspaces/${TEAM}/access`) return json(directory())
      if (path === '/v1/accounts' && method === 'GET') return json([shared])
      return route(request)
    })
    await loadAccounts()
    await Promise.all([loadMembers(), loadDirectory()])
    mounted = mount(AccessPanel, { accountId: shared.id, email: shared.email, account: shared })
    await flush()
  }
  const box = (userID: string, flag: string) => find(`input[name="${userID}-${flag}"]`)!
  const row = (userID: string) => find(`li[data-user="${userID}"]`)!

  it('ticks Read with Act, and saves the whole grant only when asked', async () => {
    await panel(({ path, method, body }) => {
      if (path === `/v1/accounts/${shared.id}/access/${CAROL}` && method === 'PUT') {
        grants.push([CAROL, body as typeof full])
        return json({ account_id: shared.id, user_id: CAROL, ...(body as object), updated_at: 2 })
      }
      return failure('not_found', 404)
    })
    await check(box(CAROL, 'act'))
    expect(box(CAROL, 'read').checked).toBe(true)
    expect(words(row(CAROL))).toContain('Now: No access')
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${CAROL}`)).toEqual([])
    await click(find(`li[data-user="${CAROL}"] button`, 'Save access'))
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${CAROL}`).map(request => request.body)).toEqual([{ read: true, act: true, send: false, manage: false }])
    await vi.waitFor(() => expect(box(CAROL, 'act').checked).toBe(true))
    expect(find(`li[data-user="${CAROL}"] button`, 'Save access')).toBeNull()
  })

  it('takes access away with a revoke naming only what goes, and Cancel sets back what was ticked', async () => {
    grants = []
    await panel(({ path, method }) => {
      if (path === `/v1/accounts/${shared.id}/access/${CAROL}` && method === 'DELETE') {
        grants = grants.map(([userID, flags]) => [userID, userID === CAROL ? { ...flags, send: false } : flags])
        return new Response(null, { status: 204 })
      }
      return failure('not_found', 404)
    })
    grants.push([CAROL, { ...none, read: true, send: true }])
    await loadDirectory()
    await flush()
    await check(box(CAROL, 'send'), false)
    await click(find(`li[data-user="${CAROL}"] button`, 'Cancel'))
    expect(box(CAROL, 'send').checked).toBe(true)
    await check(box(CAROL, 'send'), false)
    await click(find(`li[data-user="${CAROL}"] button`, 'Save access'))
    const revokes = sent('DELETE', `/v1/accounts/${shared.id}/access/${CAROL}`)
    expect(revokes.map(request => request.query)).toEqual([{ flags: 'send' }])
  })

  it('says a refused change in the console’s words, never the server’s, and keeps what was ticked', async () => {
    await panel(() => failure('not_authorized', 403))
    await check(box(CAROL, 'manage'))
    await click(find(`li[data-user="${CAROL}"] button`, 'Save access'))
    expect(words(find(`li[data-user="${CAROL}"] .alert`)!)).toBe('You can give only what you hold on this mailbox, and change who has access only as an owner or an admin of the team, or as someone who manages it.')
    expect(words()).not.toContain('hunter2')
    expect(box(CAROL, 'manage').checked).toBe(true)
  })

  it('asks before taking a link over, says what it changes, and posts nothing on Cancel', async () => {
    await panel(({ path, method }) => path === `/v1/accounts/${shared.id}/take-over` && method === 'POST'
      ? json({ ...shared, linked_by: ana.id }) : failure('not_found', 404))
    seen = []
    await click(find('.take-over button', 'Take over the link…'))
    const dialog = words(find('dialog')!)
    expect(dialog).toContain('suporte@example.test syncs under your agreement to sync from now on, instead of Bea Lima’s. Its index is kept.')
    expect(dialog).toContain('If you turn sync off, its index is deleted, for everyone in Support who reads it.')
    expect(dialog).toContain('Bea Lima keeps their access as an ordinary member: it can then be changed, and they can leave the team.')
    await click(find('dialog button', 'Cancel'))
    expect(sent('POST', `/v1/accounts/${shared.id}/take-over`)).toEqual([])
    await click(find('.take-over button', 'Take over the link…'))
    await click(find('dialog button.primary', 'Take over the link'))
    expect(sent('POST', `/v1/accounts/${shared.id}/take-over`)).toHaveLength(1)
    expect(find('dialog')).toBeNull()
    expect(accounts.list.find(item => item.id === shared.id)?.linked_by).toBe(ana.id)
  })
})

describe('the people of a team', () => {
  let members: Member[] = []
  /** Ana owns Support; Bea linked a mailbox there; Carol is a member. */
  async function membersOf(role: string, route: Route = () => failure('not_found', 404), list?: () => Workspace[], others?: Member[]) {
    members = [member(ana.id, { email: ana.email, name: ana.name, role, last_owner: role === 'owner' && !others }), member(BEA, { name: 'Bea Lima', links: 1 }), member(CAROL, { name: 'Carol Dias' })]
    if (others) members.push(...others)
    else if (role !== 'owner') members.push(member('usr_00000000000000d4', { name: 'Dora', role: 'owner', last_owner: true }))
    await signedIn(role, request => {
      const { path, method } = request
      if (path === `/v1/workspaces/${TEAM}/members` && method === 'GET') return json(members)
      if (path === `/v1/workspaces/${TEAM}/invites` && method === 'GET') return json([])
      return route(request)
    }, list)
    mounted = mount(OpenMembers)
    await vi.waitFor(() => expect(team.members.loaded).toBe(true))
    await flush()
  }
  const row = (userID: string) => find(`li[data-user="${userID}"]`)!

  it('changes a role at once from its list, and sets it back when the server refuses', async () => {
    let refuse = false
    await membersOf('owner', ({ path, method, body }) => {
      if (path === `/v1/workspaces/${TEAM}/members/${CAROL}` && method === 'PATCH') {
        if (refuse) return failure('conflict', 409)
        members = members.map(item => item.user_id === CAROL ? { ...item, ...(body as object) } : item)
        return json(members.find(item => item.user_id === CAROL))
      }
      return failure('not_found', 404)
    })
    const select = find(`select[name="role-${CAROL}"]`)!
    select.value = 'admin'
    await fire(select, 'change')
    expect(sent('PATCH', `/v1/workspaces/${TEAM}/members/${CAROL}`).map(request => request.body)).toEqual([{ role: 'admin' }])
    expect(words(find('.success')!)).toBe('Carol Dias is now Admin.')
    refuse = true
    const again = find(`select[name="role-${CAROL}"]`)!
    again.value = 'owner'
    await fire(again, 'change')
    expect(again.value).toBe('admin')
    expect(words(row(CAROL))).toContain('The team’s protections refuse this')
  })

  it('asks before disabling or removing someone, and Cancel changes nothing', async () => {
    await membersOf('owner', ({ path, method }) => {
      if (path !== `/v1/workspaces/${TEAM}/members/${CAROL}` || method !== 'DELETE') return failure('not_found', 404)
      members = members.filter(item => item.user_id !== CAROL)
      return new Response(null, { status: 204 })
    })
    await click(find(`li[data-user="${CAROL}"] button`, 'Disable…'))
    expect(words(find('dialog')!)).toContain('Carol Dias stays listed in Support but loses their access to every mailbox of it now. Enabling them again gives none of it back.')
    await click(find('dialog button', 'Cancel'))
    await click(find(`li[data-user="${CAROL}"] button`, 'Remove…'))
    expect(words(find('dialog')!)).toContain('Carol Dias leaves Support: their access to its mailboxes goes, and invitations to it still waiting for them are deleted.')
    expect(seen.filter(request => request.method === 'PATCH' || request.method === 'DELETE')).toEqual([])
    await click(find('dialog button.danger', 'Remove'))
    expect(sent('DELETE', `/v1/workspaces/${TEAM}/members/${CAROL}`)).toHaveLength(1)
    expect(find(`li[data-user="${CAROL}"]`)).toBeNull()
    expect(words(find('.success')!)).toBe('Carol Dias was removed from Support.')
    // The person a mailbox there is linked by is offered neither.
    expect(words(row(BEA))).not.toMatch(/Remove…|Disable…/)
  })

  it('shows an invitation’s link once, keeps it through Escape, copies it on request, and forgets it when closed', async () => {
    await membersOf('owner', ({ path, method }) => path === `/v1/workspaces/${TEAM}/invites` && method === 'POST'
      ? json({ id: 'inv_0000000000000001', email: 'dan@example.test', workspace_id: TEAM, role: 'admin', url: LINK, created_by: ana.id, created_at: 1_790_000_000, expires_at: 1_790_604_800 }, 201)
      : failure('not_found', 404))
    await click(find('.team-actions button', 'Invite someone'))
    await fill(find('dialog input[name=invite-email]'), 'dan@example.test')
    await check(find('dialog input[name=invite-role][value=admin]'))
    expect(sent('POST', `/v1/workspaces/${TEAM}/invites`)).toEqual([])
    await submit(find('dialog form'))
    expect(sent('POST', `/v1/workspaces/${TEAM}/invites`).map(request => request.body)).toEqual([{ email: 'dan@example.test', role: 'admin' }])
    expect(words(find('dialog .invite-link')!)).toBe(LINK)
    expect(find('dialog .invite-link')!.getAttribute('translate')).toBe('no')
    expect(words(find('dialog')!)).toContain('It works only for dan@example.test, once, until')
    // Escape is held back, and a cancel the browser sends anyway changes nothing.
    expect(keydown({ key: 'Escape' }).defaultPrevented).toBe(true)
    await fire(find('dialog'), 'cancel')
    expect(words(find('dialog .invite-link')!)).toBe(LINK)
    await click(find('dialog button', 'Copy link'))
    expect(clipboard).toEqual([LINK])
    await click(find('dialog button', 'Done'))
    expect(find('dialog')).toBeNull()
    expect(words()).not.toContain(CODE)
    expect(JSON.stringify(team)).not.toContain(CODE)
  })

  it('reads the members again after making another owner a member, and stops offering the one owner left to leave or step down', async () => {
    // Ana and Dora own Support: either may leave, until Dora is a member.
    await membersOf('owner', ({ path, method, body }) => {
      if (path === `/v1/workspaces/${TEAM}/members/usr_00000000000000d4` && method === 'PATCH') {
        members = members.map(item => item.user_id === ana.id ? { ...item, last_owner: true } : item.user_id === 'usr_00000000000000d4' ? { ...item, ...(body as object) } : item)
        return json(members.find(item => item.user_id === 'usr_00000000000000d4'))
      }
      return failure('not_found', 404)
    }, undefined, [member('usr_00000000000000d4', { name: 'Dora', role: 'owner' })])
    expect(find(`li[data-user="${ana.id}"] button`, 'Leave the team…')).not.toBeNull()
    const select = find('select[name="role-usr_00000000000000d4"]')!
    select.value = 'member'
    await fire(select, 'change')
    await vi.waitFor(() => expect(find(`li[data-user="${ana.id}"] button`, 'Leave the team…')).toBeNull())
    expect(find(`select[name="role-${ana.id}"]`)).toBeNull()
    expect(words(row(ana.id))).toContain('You are the team’s only owner')
  })

  it('reads the person’s workspaces again on Refresh, and offers what their role there allows now', async () => {
    let role = 'admin'
    await membersOf('admin', () => failure('not_found', 404), () => [personal, support(role)])
    expect(find('.team-actions button', 'Invite someone')).not.toBeNull()
    // Another owner made Ana a member.
    role = 'member'
    members = members.map(item => item.user_id === ana.id ? { ...item, role: 'member' } : item)
    const before = sent('GET', '/v1/workspaces').length
    await click(find('.section-actions button', 'Refresh'))
    await vi.waitFor(() => expect(sent('GET', '/v1/workspaces')).toHaveLength(before + 1))
    await vi.waitFor(() => expect(find('.team-actions button', 'Invite someone')).toBeNull())
    expect(find('.team-actions button', 'Rename…')).toBeNull()
    expect(words(find('.team-card')!)).toContain('Your role: Member')
  })

  it('lets a member leave, after saying what goes, and shows them another of their workspaces', async () => {
    let list = [personal, support('member')]
    await membersOf('member', ({ path, method }) => {
      if (path === `/v1/workspaces/${TEAM}/members/${ana.id}` && method === 'DELETE') { list = [personal]; return new Response(null, { status: 204 }) }
      return failure('not_found', 404)
    }, () => list)
    expect(words(row(CAROL))).not.toMatch(/Remove…|Disable…/)
    await click(find(`li[data-user="${ana.id}"] button`, 'Leave the team…'))
    expect(words(find('dialog')!)).toContain('Your access to the mailboxes of Support goes now. To come back you need a new invitation.')
    await click(find('dialog button.danger', 'Leave the team'))
    expect(sent('DELETE', `/v1/workspaces/${TEAM}/members/${ana.id}`)).toHaveLength(1)
    await vi.waitFor(() => expect(workspaces.currentID).toBe(PERSONAL))
    expect(workspaces.lost?.name).toBe('Support')
  })
})

describe('an invitation opened signed in', () => {
  it('joins the team it names, which the console then shows', async () => {
    let list = [personal]
    await signedIn('member', ({ path, method }) => {
      if (path !== '/v1/auth/invites/accept' || method !== 'POST') return failure('not_found', 404)
      list = [personal, support('member')]
      return json(support('member'))
    }, () => list)
    selectWorkspace(PERSONAL)
    holdInvitation({ invite: CODE, email: 'ana@example.test' })
    mounted = mount(InvitationDialog)
    await flush()
    expect(words(find('dialog')!)).toContain('Joining gives you access to no mailbox')
    await click(find('dialog button', 'Join the team'))
    expect(sent('POST', '/v1/auth/invites/accept').map(request => request.body)).toEqual([{ invite: CODE }])
    expect(find('dialog')).toBeNull()
    expect(workspaces.currentID).toBe(TEAM)
    expect(invitation.joined?.name).toBe('Support')
  })

  it('offers signing out for an invitation of another address, and keeps it to use then', async () => {
    await signedIn('member', () => failure('not_found', 404), () => [personal])
    holdInvitation({ invite: CODE, email: 'bea@example.test' })
    mounted = mount(InvitationDialog)
    await flush()
    expect(find('dialog button', 'Join the team')).toBeNull()
    await click(find('dialog button', 'Sign out'))
    expect(session.phase).toBe('signed-out')
    expect(invitation.pending?.email).toBe('bea@example.test')
    expect(sent('POST', '/v1/auth/invites/accept')).toEqual([])
  })
})

describe('turning sync off', () => {
  it('waits to offer it until the team mailboxes it reaches are named', async () => {
    let answer!: (response: Response) => void
    await signedIn('admin', ({ path, method }) => {
      if (path === '/v1/me/sync-consent' && method === 'GET') return json({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/accounts' && method === 'GET') return new Promise<Response>(resolve => { answer = resolve })
      return failure('not_found', 404)
    })
    await loadConsent()
    mounted = mount(SyncPermission)
    await flush()
    await click(find('button[role=switch]'))
    expect(words(find('dialog')!)).toContain('Checking which team mailboxes this reaches…')
    expect(find('dialog button.danger', 'Turn off and delete')!.disabled).toBe(true)
    await vi.waitFor(() => expect(answer).toBeTypeOf('function'))
    answer(json([{ ...shared, linked_by: ana.id }]))
    await vi.waitFor(() => expect(find('dialog .team-warning')).not.toBeNull())
    expect(find('dialog button.danger', 'Turn off and delete')!.disabled).toBe(false)
    expect(sent('DELETE', '/v1/me/sync-consent')).toEqual([])
  })

  it('names the team mailboxes the person linked, whose index goes for everyone who reads them', async () => {
    const own: Account = account({ id: 'acc_own', email: 'ana@gmail.example', state: 'active', workspace_id: PERSONAL, linked_by: ana.id, access: full })
    const linked: Account = { ...shared, linked_by: ana.id }
    const other: Account = account({ id: 'acc_other', email: 'vendas@example.test', state: 'active', workspace_id: TEAM, linked_by: BEA, access: full })
    await signedIn('admin', ({ path, method }) => {
      if (path === '/v1/me/sync-consent' && method === 'GET') return json({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/accounts' && method === 'GET') return json([own, linked, other])
      return failure('not_found', 404)
    })
    await loadConsent()
    mounted = mount(SyncPermission)
    await flush()
    await click(find('button[role=switch]'))
    await vi.waitFor(() => expect(find('dialog .team-warning')).not.toBeNull())
    const warning = words(find('dialog .team-warning')!)
    expect(warning).toContain('suporte@example.test in Support')
    expect(warning).toContain('This team mailbox was linked by you, so it syncs under your agreement.')
    expect(warning).not.toContain('vendas@example.test')
    expect(warning).not.toContain('ana@gmail.example')
    expect(warning).toContain('To keep a team’s index, have an owner or an admin of the team take the link over first.')
    // Every workspace's mailboxes, read whole: what turning sync off reaches is the person's, not one workspace's.
    expect(sent('GET', '/v1/accounts').some(request => !request.query.workspace)).toBe(true)
  })
})
