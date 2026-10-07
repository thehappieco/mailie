// Workspaces and teams as a person presses them, on a page (test/dom.ts):
// choosing the workspace reads its mailboxes; connecting a mailbox into a
// team shows that team first, with the sync text and a box that gives the
// team's agreement with the link; a team mailbox's sync is switched by its
// owners and admins, on after the text and off once its address is typed; a
// grant is ticked and saved with the request that gives exactly that; the
// team's people change only after a question, and a refusal is said in the
// console's words; an invitation's link is shown once, copied on request and
// forgotten; an invitation opened signed in joins the team; turning one's own
// sync off says it reaches the personal workspace's mailboxes; a change of
// people reads them again, and the person's own role follows what the server
// says now; the header names the workspace shown on its sections only, never
// on the person's own; a member of a team is offered no connecting there, and
// their personal workspace instead, and is shown none of the team's people.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { check, click, fill, find, fire, flush, keydown, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { MailboxAccess, Member, Workspace } from '../src/api/types'
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
const member = (userID: string, fields: Partial<Member> = {}): Member => ({ user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1_790_000_000, ...fields })
const shared = account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: { ...full, manage: true }, sync: syncing() })

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
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role }), member(BEA, { name: 'Bea Lima' })])
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

  it('offers Members to a team’s owners and admins only, and tells a member who manages its people, never asking for them', async () => {
    await consoleIn('member')
    expect(page.querySelectorAll('.console-sidebar .console-nav-item').map(item => words(item))).not.toContain('Members')
    expect(words()).toContain('The people of Support, and who can use each of its mailboxes, are managed by its owners and admins.')
    // A member is shown none of the team's people or directory, and the server is not asked for them.
    expect(sent('GET', `/v1/workspaces/${TEAM}/members`)).toEqual([])
    expect(sent('GET', `/v1/workspaces/${TEAM}/access`)).toEqual([])
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
      if (path === '/v1/me/sync-consent' && method === 'GET') return json({ consented: false, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/accounts' && method === 'POST') {
        posted.push(body)
        return json({ account: account({ id: 'acc_new', email: 'vendas@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: { ...full, manage: true } }) }, 201)
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
    expect(words(find('dialog .place-choice')!)).toContain('It belongs to the team, and its owners and admins manage it. Only you read it until you give other members access.')
    // The personal workspace's mailboxes sync under the person's own agreement: no box for a team's.
    expect(find('dialog .team-sync')).toBeNull()
    await check(places[1]!)
    expect(find('dialog .team-sync')).not.toBeNull()
    await fill(find('dialog input[name=mailbox]'), 'vendas@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    // Left unticked: linked with sync off, and no agreement given for the team.
    expect(posted[0]).toEqual(expect.objectContaining({ email: 'vendas@example.test', provider: 'imap', workspace_id: TEAM }))
    expect(posted[0]).not.toHaveProperty('sync_consent_version')
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
      if (path === '/v1/accounts' && method === 'POST') {
        posted.push(body)
        const into = (body as { workspace_id?: string }).workspace_id ?? PERSONAL
        const syncs = !!(body as { sync_consent_version?: string }).sync_consent_version
        return json({ account: account({ id: 'acc_new', email: 'vendas@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: into, access: full, ...(syncs ? { sync: syncing() } : {}) }) }, 201)
      }
      return failure('not_found', 404)
    })
    await Promise.all([loadAccounts(), loadProviders(), loadConsent()])
    mounted = mount(AddAccountDialog, { resume: null })
    await flush()
    await click(find('dialog .provider-option', 'Other provider (IMAP)'))
  }
  const placeOf = (id: string) => find(`dialog .place-choice input[name=workspace][value=${id}]`)!
  const fillPassword = async () => {
    await fill(find('dialog input[name=mailbox]'), 'vendas@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
  }
  const current = { consented: false, current_version: SYNC_TEXT_VERSION }

  it('connects into the team shown, as its empty list says, unless the person picks another', async () => {
    const posted: unknown[] = []
    await connecting({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION }, posted)
    expect(placeOf(TEAM).checked).toBe(true)
    expect(placeOf(PERSONAL).checked).toBe(false)
    await fillPassword()
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).toMatchObject({ workspace_id: TEAM })
    // It stays where it was connected: no other workspace was shown for it.
    expect(workspaces.currentID).toBe(TEAM)
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([TEAM])
    await vi.waitFor(() => expect(accounts.list.map(item => item.id)).toEqual(['acc_new']))
  })

  it('shows the sync text into a team, and gives the team’s agreement with the link only when ticked, to the revision shown', async () => {
    const posted: unknown[] = []
    // Ana never agreed for herself: the team's agreement is not hers, and is asked for here all the same.
    await connecting(current, posted)
    const box = find('dialog .team-sync')!
    expect(words(box)).toContain('Mail sync for Support')
    expect(words(box)).toContain('Who can read the index')
    expect(words(box)).toContain('A team’s mailbox syncs under the team’s agreement')
    const agree = find('dialog input[name=team-sync]')!
    expect(agree.checked).toBe(false)
    expect(words(box)).toContain('Turn on sync for Support')
    await check(agree)
    await fillPassword()
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).toMatchObject({ workspace_id: TEAM, sync_consent_version: SYNC_TEXT_VERSION })
    // The person's own agreement was neither asked for nor given.
    expect(sent('POST', '/v1/me/sync-consent')).toEqual([])
  })

  it('forgets a ticked agreement when another team or the personal workspace is chosen, and sends none there', async () => {
    const posted: unknown[] = []
    await connecting(current, posted)
    await check(find('dialog input[name=team-sync]')!)
    await check(placeOf(PERSONAL))
    expect(find('dialog .team-sync')).toBeNull()
    await check(placeOf(TEAM))
    expect(find('dialog input[name=team-sync]')!.checked).toBe(false)
    await check(placeOf(PERSONAL))
    await fillPassword()
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).not.toHaveProperty('sync_consent_version')
    expect(posted[0]).not.toHaveProperty('workspace_id', TEAM)
  })

  it('offers a reload in place of the box when the server asks about another text than this page shows, and links with sync off', async () => {
    const posted: unknown[] = []
    await connecting({ consented: false, current_version: '2027-01-open-sync-4' }, posted)
    const box = find('dialog .team-sync')!
    expect(find('dialog input[name=team-sync]')).toBeNull()
    expect(find('dialog .team-sync button', 'Reload page')).not.toBeNull()
    expect(words(box)).not.toContain('Who can read the index')
    await fillPassword()
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).not.toHaveProperty('sync_consent_version')
  })
})

describe('a team mailbox’s sync', () => {
  const managed = account({ id: 'acc_managed', email: 'diretoria@example.test', state: 'active', workspace_id: TEAM, access: { ...none, manage: true } })
  let record: MailboxAccess['sync']
  /** Ana, with the role given in Support, is shown its mailbox's sheet; the directory holds record. */
  async function sheetAs(role: string, initial: MailboxAccess['sync'], readers = 2) {
    record = initial
    await signedIn(role, ({ path, method, body }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([{ ...managed, sync: record?.enabled ? syncing() : managed.sync }])
      if (path === '/v1/me/sync-consent' && method === 'GET') return json({ consented: false, current_version: SYNC_TEXT_VERSION })
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role }), member(BEA, { name: 'Bea Lima' })])
      if (path === `/v1/workspaces/${TEAM}/access`) {
        return json([{ account_id: managed.id, email: managed.email, provider: 'gmail', state: 'active', linked_by: BEA, readers, no_reader: readers === 0, sync: record, grants: [{ account_id: managed.id, user_id: BEA, read: true, act: false, send: false, manage: false, updated_at: 1 }] }])
      }
      if (path === `/v1/accounts/${managed.id}/sync` && method === 'PUT') {
        const asked = body as { enabled: boolean; version?: string }
        record = asked.enabled ? { enabled: true, enabled_at: 1_790_000_500, enabled_by: ana.id, version: asked.version, current: asked.version === SYNC_TEXT_VERSION } : { enabled: false, current: false }
        return json(asked.enabled ? syncing() : { enabled: false, running: false, state: 'off', folders_synced: 0, folders_total: 0, messages: 0, initial_progress: 0 })
      }
      return failure('not_found', 404)
    })
    await loadAccounts()
    accounts.detailID = managed.id
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(find('dialog.sheet-backdrop')).not.toBeNull())
    await vi.waitFor(() => expect(find('.team-sync button[role=switch], .team-sync .session-actions')).not.toBeNull())
    await flush()
  }
  const off: MailboxAccess['sync'] = { enabled: false, current: false }
  const lastDialog = () => page.querySelectorAll('dialog').at(-1)!

  it('turns it on for the team after showing the sync text, with the revision shown', async () => {
    await sheetAs('admin', off)
    expect(words(find('.team-sync')!)).toContain('Sync is off. Nothing from this mailbox is stored.')
    await click(find('.team-sync button[role=switch]'))
    expect(words(lastDialog())).toContain('You agree to this text on behalf of Support: diretoria@example.test then syncs under the team’s agreement, for everyone in the team who reads it.')
    expect(words(lastDialog())).toContain('Who can read the index')
    await click(find('dialog button.primary', 'Turn on sync for Support'))
    expect(sent('PUT', `/v1/accounts/${managed.id}/sync`).map(request => request.body)).toEqual([{ enabled: true, version: SYNC_TEXT_VERSION }])
    // The record is read again: who turned it on, and when.
    await vi.waitFor(() => expect(words(find('.team-sync')!)).toContain('turned on by Ana Souza'))
    expect(accounts.list.find(item => item.id === managed.id)?.sync.enabled).toBe(true)
  })

  it('turns it off only once the address is typed, saying how many read the index it deletes, and Cancel sends nothing', async () => {
    await sheetAs('owner', { enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: SYNC_TEXT_VERSION, current: true })
    expect(words(find('.team-sync')!)).toContain('turned on by Bea Lima')
    await click(find('.team-sync button[role=switch]'))
    expect(words(lastDialog())).toContain('The index is deleted for everyone in Support who reads it: 2 now.')
    const confirm = find('dialog button.danger', 'Turn off and delete')!
    expect(confirm.disabled).toBe(true)
    await click(find('dialog button', 'Cancel'))
    expect(sent('PUT', `/v1/accounts/${managed.id}/sync`)).toEqual([])
    await click(find('.team-sync button[role=switch]'))
    await fill(find('dialog input[name=team-sync-confirm]'), 'Diretoria@Example.test')
    expect(find('dialog button.danger', 'Turn off and delete')!.disabled).toBe(false)
    await submit(find('dialog form.team-sync-confirm'))
    expect(sent('PUT', `/v1/accounts/${managed.id}/sync`).map(request => request.body)).toEqual([{ enabled: false }])
    await vi.waitFor(() => expect(words(find('.team-sync')!)).toContain('Sync is off. Nothing from this mailbox is stored.'))
  })

  it('offers confirming for the team an agreement the upgrade carried over from whoever linked the mailbox', async () => {
    await sheetAs('admin', { enabled: true, enabled_at: 1_780_000_000, enabled_by: BEA, version: '2026-10-open-sync-2', migrated: true, current: false })
    expect(words(find('.team-sync')!)).toContain('It is still tied to them')
    expect(find('.team-sync button[role=switch]')).toBeNull()
    await click(find('.team-sync button', 'Confirm for Support…'))
    await click(find('dialog button.primary', 'Turn on sync for Support'))
    expect(sent('PUT', `/v1/accounts/${managed.id}/sync`).map(request => request.body)).toEqual([{ enabled: true, version: SYNC_TEXT_VERSION }])
    await vi.waitFor(() => expect(words(find('.team-sync')!)).not.toContain('It is still tied to them'))
    expect(find('.team-sync button[role=switch]')).not.toBeNull()
  })

  it('says a refused switch in the console’s words, never the server’s', async () => {
    await sheetAs('admin', off)
    serve(request => request.path === `/v1/accounts/${managed.id}/sync` ? failure('not_authorized', 403) : request.path === '/v1/workspaces' ? json([personal, support('admin')]) : failure('not_found', 404))
    await click(find('.team-sync button[role=switch]'))
    await click(find('dialog button.primary', 'Turn on sync for Support'))
    await vi.waitFor(() => expect(find('dialog .alert')).not.toBeNull())
    expect(words(find('dialog .alert')!)).toBe('Only the owners and admins of the team turn its mailboxes’ sync on or off.')
    expect(words()).not.toContain('hunter2')
  })
})

describe('who can use a mailbox', () => {
  const directory = (): MailboxAccess[] => {
    const listed = grants.map(([userID, flags]) => ({ account_id: shared.id, user_id: userID, ...flags, updated_at: 1 }))
    const readers = listed.filter(grant => grant.read).length
    return [{ account_id: shared.id, email: shared.email, provider: 'imap', state: 'active', linked_by: BEA, readers, no_reader: readers === 0, sync: { enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: SYNC_TEXT_VERSION, current: true }, grants: listed }]
  }
  let grants: [string, typeof full][] = []
  const reader = { read: true, act: true, send: true, manage: false }

  /** Ana administers Support and reads a mailbox Bea reads too; Carol holds nothing yet. */
  async function panel(route: Route = () => failure('not_found', 404)) {
    grants = [[BEA, reader], [ana.id, reader]]
    await signedIn('admin', request => {
      const { path, method } = request
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana.id, { email: ana.email, name: ana.name, role: 'admin' }), member(BEA, { name: 'Bea Lima' }), member(CAROL, { name: 'Carol Dias' })])
      if (path === `/v1/workspaces/${TEAM}/access`) return json(directory())
      if (path === '/v1/accounts' && method === 'GET') return json([shared])
      return route(request)
    })
    await loadAccounts()
    await Promise.all([loadMembers(), loadDirectory()])
    mounted = mount(AccessPanel, { accountId: shared.id, email: shared.email })
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
    // The members are read again too: who is the last reader of what may have changed.
    expect(sent('GET', `/v1/workspaces/${TEAM}/members`).length).toBeGreaterThan(1)
  })

  it('takes access away with a revoke naming only what goes, and Cancel sets back what was ticked', async () => {
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

  it('lets an admin drop her own flags, and keeps Read with the last person who can read the mailbox', async () => {
    await panel(({ path, method }) => {
      if (path === `/v1/accounts/${shared.id}/access/${ana.id}` && method === 'DELETE') {
        grants = grants.filter(([userID]) => userID !== ana.id)
        return new Response(null, { status: 204 })
      }
      if (path === `/v1/accounts/${shared.id}` && method === 'GET') return json({ ...shared, access: { ...none, manage: true } })
      return failure('not_found', 404)
    })
    // Two read it: either may lose Read.
    expect(box(BEA, 'read').disabled).toBe(false)
    expect(box(ana.id, 'read').disabled).toBe(false)
    await check(box(ana.id, 'read'), false)
    await check(box(ana.id, 'send'), false)
    await click(find(`li[data-user="${ana.id}"] button`, 'Save access'))
    // Every flag she held goes: a revoke naming none takes them all.
    expect(sent('DELETE', `/v1/accounts/${shared.id}/access/${ana.id}`).map(request => request.query)).toEqual([{}])
    // Bea is the only one left who reads it, and Ana gives no Read she does not hold.
    await vi.waitFor(() => expect(box(BEA, 'read').disabled).toBe(true))
    expect(words(row(BEA))).toContain('The only person who can read this mailbox: give someone else Read before taking theirs.')
    expect(box(CAROL, 'read').disabled).toBe(true)
    expect(words()).toContain('You do not read this mailbox, so you cannot give Read on it, not even to yourself')
  })

  it('says a refused change in the console’s words, never the server’s, and keeps what was ticked', async () => {
    await panel(() => failure('not_authorized', 403))
    await check(box(CAROL, 'manage'))
    await click(find(`li[data-user="${CAROL}"] button`, 'Save access'))
    expect(words(find(`li[data-user="${CAROL}"] .alert`)!)).toBe('Only the owners and admins of the team change who has access, and they give Read only on a mailbox they read themselves.')
    expect(words()).not.toContain('hunter2')
    expect(box(CAROL, 'manage').checked).toBe(true)
  })

  it('says the last reader’s refusal in the console’s words when the server finds one the page did not know of', async () => {
    await panel(({ path, method }) => path === `/v1/accounts/${shared.id}/access/${BEA}` && method === 'DELETE' ? failure('conflict', 409) : failure('not_found', 404))
    await check(box(BEA, 'read'), false)
    await click(find(`li[data-user="${BEA}"] button`, 'Save access'))
    expect(words(find(`li[data-user="${BEA}"] .alert`)!)).toBe('This is the only person who can read this mailbox: give someone else Read on it first.')
  })

  it('offers no take-over and names no linker', async () => {
    await panel()
    expect(words()).not.toMatch(/take over|taking over|linked by|syncs under the agreement of/i)
    // Bea's access is the team's to change, as anyone's is.
    for (const flag of ['read', 'act', 'send']) expect(box(BEA, flag).disabled, flag).toBe(false)
  })
})

describe('the people of a team', () => {
  let members: Member[] = []
  const directory: MailboxAccess[] = [{ account_id: shared.id, email: shared.email, provider: 'imap', state: 'active', readers: 1, no_reader: false, sync: { enabled: false, current: false }, grants: [{ account_id: shared.id, user_id: BEA, read: true, act: false, send: false, manage: false, updated_at: 1 }] }]
  /** Ana has the role given in Support; Bea alone reads one of its mailboxes; Carol is a member. */
  async function membersOf(role: string, route: Route = () => failure('not_found', 404), list?: () => Workspace[], others?: Member[]) {
    members = [member(ana.id, { email: ana.email, name: ana.name, role, last_owner: role === 'owner' && !others }), member(BEA, { name: 'Bea Lima', last_reader_of: [shared.id] }), member(CAROL, { name: 'Carol Dias' })]
    if (others) members.push(...others)
    else if (role !== 'owner') members.push(member('usr_00000000000000d4', { name: 'Dora', role: 'owner', last_owner: true }))
    await signedIn(role, request => {
      const { path, method } = request
      if (path === `/v1/workspaces/${TEAM}/members` && method === 'GET') return json(members)
      if (path === `/v1/workspaces/${TEAM}/invites` && method === 'GET') return json([])
      if (path === `/v1/workspaces/${TEAM}/access` && method === 'GET') return json(directory)
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

  it('asks before disabling or removing someone, saying their invitations stop working, and Cancel changes nothing', async () => {
    await membersOf('owner', ({ path, method }) => {
      if (path !== `/v1/workspaces/${TEAM}/members/${CAROL}` || method !== 'DELETE') return failure('not_found', 404)
      members = members.filter(item => item.user_id !== CAROL)
      return new Response(null, { status: 204 })
    })
    await click(find(`li[data-user="${CAROL}"] button`, 'Disable…'))
    expect(words(find('dialog')!)).toContain('Carol Dias stays listed in Support but loses their access to every mailbox of it now, and the invitations they made for it stop working. Enabling them again gives none of it back.')
    await click(find('dialog button', 'Cancel'))
    await click(find(`li[data-user="${CAROL}"] button`, 'Remove…'))
    expect(words(find('dialog')!)).toContain('Carol Dias leaves Support: their access to its mailboxes goes, invitations to it still waiting for them are deleted, and those they made stop working.')
    expect(seen.filter(request => request.method === 'PATCH' || request.method === 'DELETE')).toEqual([])
    await click(find('dialog button.danger', 'Remove'))
    expect(sent('DELETE', `/v1/workspaces/${TEAM}/members/${CAROL}`)).toHaveLength(1)
    expect(find(`li[data-user="${CAROL}"]`)).toBeNull()
    expect(words(find('.success')!)).toBe('Carol Dias was removed from Support.')
    // The only person who reads a mailbox of the team is offered neither, and the mailbox is named.
    expect(words(row(BEA))).not.toMatch(/Remove…|Disable…/)
    expect(words(row(BEA))).toContain('The only person who can read suporte@example.test: give someone else Read before disabling or removing them.')
  })

  it('says the last reader’s refusal in the console’s words when the server finds one the page did not know of', async () => {
    await membersOf('owner', ({ path, method }) => path === `/v1/workspaces/${TEAM}/members/${CAROL}` && method === 'PATCH' ? failure('conflict', 409) : failure('not_found', 404))
    await click(find(`li[data-user="${CAROL}"] button`, 'Disable…'))
    await click(find('dialog button.danger', 'Disable'))
    expect(words(find('dialog .alert')!)).toBe('The team’s protections refuse this: it keeps an active owner, and a mailbox someone reads keeps someone who can read it. Make another member an owner, or give someone else Read, first.')
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

  it('reads the person’s workspaces again on Refresh, and shows a member none of the team’s people', async () => {
    let role = 'admin'
    await membersOf('admin', () => failure('not_found', 404), () => [personal, support(role)])
    expect(find('.team-actions button', 'Invite someone')).not.toBeNull()
    expect(find(`li[data-user="${ana.id}"] button`, 'Leave the team…')).toBeNull()
    // Another owner made Ana a member.
    role = 'member'
    members = members.map(item => item.user_id === ana.id ? { ...item, role: 'member' } : item)
    const before = sent('GET', '/v1/workspaces').length
    await click(find('.section-actions button', 'Refresh'))
    await vi.waitFor(() => expect(sent('GET', '/v1/workspaces')).toHaveLength(before + 1))
    await vi.waitFor(() => expect(find('.team-actions button', 'Invite someone')).toBeNull())
    expect(find('.team-actions button', 'Rename…')).toBeNull()
    expect(find('li[data-user]')).toBeNull()
    expect(words()).toContain('The people of Support, and who can use each of its mailboxes, are managed by its owners and admins.')
  })

  it('lets an owner leave while another owner remains, after saying what goes, and shows them another of their workspaces', async () => {
    let list = [personal, support('owner')]
    await membersOf('owner', ({ path, method }) => {
      if (path === `/v1/workspaces/${TEAM}/members/${ana.id}` && method === 'DELETE') { list = [personal]; return new Response(null, { status: 204 }) }
      return failure('not_found', 404)
    }, () => list, [member('usr_00000000000000d4', { name: 'Dora', role: 'owner' })])
    await click(find(`li[data-user="${ana.id}"] button`, 'Leave the team…'))
    expect(words(find('dialog')!)).toContain('Your access to the mailboxes of Support goes now, and the invitations you made for it stop working. To come back you need a new invitation.')
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
  /** Ana, in Support with the role given, or in her personal workspace alone, opens the dialog that turns her sync off. */
  async function dialogFor(list: () => Workspace[]) {
    await signedIn('admin', ({ path, method }) => {
      if (path === '/v1/me/sync-consent' && method === 'GET') return json({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/me/sync-consent' && method === 'DELETE') return new Response(null, { status: 204 })
      return failure('not_found', 404)
    }, list)
    await loadConsent()
    mounted = mount(SyncPermission)
    await flush()
    await click(find('button[role=switch]'))
  }

  it('says it deletes the index of the personal workspace’s mailboxes, and that a team’s keep syncing under the team’s agreement', async () => {
    await dialogFor(() => [personal, support('admin')])
    const dialog = words(find('dialog')!)
    expect(dialog).toContain('Mailie stops syncing the mailboxes of your personal workspace and deletes everything it indexed for them')
    expect(dialog).toContain('Your teams’ mailboxes sync under each team’s agreement and keep syncing.')
    // The exception the upgrade left: an agreement carried over from the person who linked the mailbox, until the team gives its own.
    expect(dialog).toContain('a team mailbox you connected before this server was updated')
    // Nothing to name first: it is offered at once, and no mailbox list is read for it.
    expect(find('dialog button.danger', 'Turn off and delete')!.disabled).toBe(false)
    expect(seen.filter(request => request.path === '/v1/accounts')).toEqual([])
    await click(find('dialog button.danger', 'Turn off and delete'))
    expect(sent('DELETE', '/v1/me/sync-consent')).toHaveLength(1)
  })

  it('says nothing of teams to a person in none', async () => {
    await dialogFor(() => [personal])
    const dialog = words(find('dialog')!)
    expect(dialog).toContain('Mailie stops syncing all your mailboxes and deletes everything it indexed for them')
    expect(dialog).not.toMatch(/team/i)
  })
})
