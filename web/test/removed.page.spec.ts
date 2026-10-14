// Removed mailboxes as a person presses them, on a page (test/dom.ts): only
// an owner of the workspace sees them and is asked about them; recovering one
// brings its card back with its sync paused; deleting one for good asks for
// its address typed and sends its id to confirm; removing one says until when
// it is kept and lists it; a link refused for an address removed and kept
// offers its owner recovering it, or deleting it for good and connecting it
// again; and a personal mailbox recovered is turned on again by its person.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, fill, find, flush, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Account, RemovedMailbox, Workspace } from '../src/api/types'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import AddAccountDialog from '../src/components/AddAccountDialog.vue'
import { SYNC_TEXT_VERSION } from '../src/open/versions'
import { accounts, loadAccounts, loadProviders, openDetail, removed } from '../src/state/accounts'
import { adoptSession, signOut } from '../src/state/session'
import { loadConsent } from '../src/state/sync'
import { loadWorkspaces, selectWorkspace } from '../src/state/workspaces'
import { account, failure, json, reply, serve, stubPage, syncing, syncOff, type Route } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const BEA = 'usr_00000000000000b2'
const DAY = 86_400
const full = { read: true, act: true, send: true, manage: true }
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })
const kept = (id: string, email: string, fields: Partial<RemovedMailbox> = {}): RemovedMailbox => ({
  id, workspace_id: TEAM, email, provider: 'imap', removed_at: 1_790_000_000, removed_by: BEA, purge_after: 1_790_000_000 + 30 * DAY, index_kept: true, readers: 2, no_reader: false, ...fields,
})
/** A mailbox recovered, as the server answers it: live again, its sync held. */
const recovered = (item: RemovedMailbox): Account => account({
  id: item.id, email: item.email, provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: item.workspace_id, access: full,
  sync: syncOff({ held: true, held_since: 1_790_100_000 }),
})

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
let mounted: Mounted | null = null
let list: Account[] = []
let removedList: RemovedMailbox[] = []

/** Signed in as Ana, with this role in Support, shown; the server keeps list and removedList as the daemon would. */
async function signedIn(role: string, shown = TEAM, route: Route = () => failure('not_found', 404)) {
  serve(request => {
    const { path, method, body } = request
    seen.push({ path, method, body })
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/workspaces') return json([personal, support(role)])
    if (path === '/v1/accounts' && method === 'GET') return json(list)
    if (path === `/v1/workspaces/${TEAM}/removed-mailboxes` || path === `/v1/workspaces/${PERSONAL}/removed-mailboxes`) {
      // As the daemon does: only an owner sees them.
      return role === 'owner' || path.includes(PERSONAL) ? json(removedList.filter(item => path.includes(item.workspace_id))) : failure('not_authorized', 403)
    }
    const recover = path.match(/^\/v1\/accounts\/([^/]+)\/recover$/)
    if (recover && method === 'POST') {
      const item = removedList.find(entry => entry.id === recover[1])
      if (!item) return failure('not_found', 404)
      removedList = removedList.filter(entry => entry !== item)
      list = [...list, recovered(item)]
      return json(recovered(item))
    }
    const purge = path.match(/^\/v1\/accounts\/([^/]+)\/purge$/)
    if (purge && method === 'POST') {
      // As the daemon does: nothing is deleted unless the id is repeated.
      if ((body as { confirm?: string }).confirm !== purge[1]) return failure('bad_request', 400)
      removedList = removedList.filter(entry => entry.id !== purge[1])
      return new Response(null, { status: 204 })
    }
    if (path === '/v1/events') return failure('not_authorized', 403)
    return route(request)
  })
  await adoptSession(reply())
  await loadWorkspaces()
  selectWorkspace(shown)
}
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)

beforeEach(() => {
  stubPage()
  seen = []
  list = []
  removedList = []
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the removed mailboxes of a workspace', () => {
  it('shows an owner each one with until when it is kept, and recovers one with its card back and its sync paused', async () => {
    removedList = [kept('acc_billing', 'billing@example.test'), kept('acc_old', 'old@example.test', { readers: 0, no_reader: true }),
      kept('acc_waiting', 'waiting@example.test', { readers: 0 })]
    await signedIn('owner')
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(find('.removed-mailboxes')).not.toBeNull())
    const section = words(find('.removed-mailboxes')!)
    expect(section).toContain('Removed mailboxes')
    expect(section).toContain('any owner of Support can recover them')
    const billing = find('.removed-item', 'billing@example.test')!
    expect(words(billing)).toContain('Kept until October 21, 2026.')
    expect(words(billing)).toContain('People in Support who read it again once it is recovered: 2.')
    // Nobody would read it once recovered: the server refuses, so nothing offers it.
    const old = find('.removed-item', 'old@example.test')!
    expect(words(old)).toContain('Nobody in Support who could read it is left, so it cannot be recovered.')
    expect(old.querySelectorAll('button').map(button => words(button))).toEqual(['Delete for good…'])
    // Only someone suspended would read it, once resumed: it waits for them, and is recovered.
    const waiting = find('.removed-item', 'waiting@example.test')!
    expect(words(waiting)).toContain('Nobody in Support reads it now, but someone suspended on this server would once resumed: it can be recovered, and its sync turned on once someone reads it.')
    expect(words(waiting)).not.toContain('cannot be recovered')
    expect(waiting.querySelectorAll('button').map(button => words(button))).toEqual(['Recover', 'Delete for good…'])

    await click(find('.removed-item button', 'Recover'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_billing/recover')).toHaveLength(1))
    await vi.waitFor(() => expect(words(find('.removed-mailboxes')!)).not.toContain('billing@example.test'))
    const card = find('.account-card', 'billing@example.test')!
    expect(words(card)).toContain('Paused since it was recovered on')
    expect(words()).toContain('billing@example.test was recovered. Its sync stays paused until it is turned on.')
  })

  it('deletes one for good only once its address is typed, sending its id to confirm', async () => {
    removedList = [kept('acc_billing', 'billing@example.test')]
    await signedIn('owner')
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(find('.removed-item', 'billing@example.test')).not.toBeNull())
    await click(find('.removed-item button', 'Delete for good…'))
    const dialog = () => find('dialog', 'Delete billing@example.test for good?')!
    expect(words(dialog())).toContain('It cannot be recovered afterwards.')
    const confirm = () => find('dialog button', 'Delete for good')!
    expect(confirm().disabled).toBe(true)
    await fill(find('dialog input[name=purge-confirm]'), 'billing@')
    expect(confirm().disabled).toBe(true)
    expect(sent('POST', '/v1/accounts/acc_billing/purge')).toEqual([])
    await fill(find('dialog input[name=purge-confirm]'), ' Billing@Example.test ')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_billing/purge').map(request => request.body)).toEqual([{ confirm: 'acc_billing' }]))
    await vi.waitFor(() => expect(find('.removed-mailboxes')).toBeNull())
    expect(find('dialog')).toBeNull()
    expect(words()).toContain('billing@example.test was deleted for good.')
  })

  it('shows an admin of the team none of them, asks for none, and says only an owner removes its mailboxes', async () => {
    list = [account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: full, sync: syncing() })]
    await signedIn('admin', TEAM, ({ path }) => path.startsWith(`/v1/workspaces/${TEAM}/`) ? json([]) : failure('not_found', 404))
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await flush()
    openDetail('acc_shared')
    await flush()
    expect(find('.removed-mailboxes')).toBeNull()
    expect(sent('GET', `/v1/workspaces/${TEAM}/removed-mailboxes`)).toEqual([])
    expect(words(find('.sheet')!)).toContain('Only an owner of Support removes its mailboxes.')
    expect(find('.sheet button', 'Remove account…')).toBeNull()
  })

  it('removes a team mailbox as its owner, says until when it is kept and lists it among the removed ones', async () => {
    list = [account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: full, sync: syncing() })]
    await signedIn('owner', TEAM, ({ path, method }) => {
      if (path === '/v1/accounts/acc_shared' && method === 'DELETE') {
        list = []
        removedList = [kept('acc_shared', 'suporte@example.test', { removed_by: 'usr_00000000000000a1', removed_at: 1_790_000_000 })]
        return json({ account_id: 'acc_shared', purged: false, purge_after: 1_790_000_000 + 30 * DAY })
      }
      if (path.startsWith(`/v1/workspaces/${TEAM}/`)) return json([])
      return failure('not_found', 404)
    })
    await loadAccounts()
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(sent('GET', `/v1/workspaces/${TEAM}/removed-mailboxes`)).toHaveLength(1))
    openDetail('acc_shared')
    await flush()
    const sheet = words(find('.sheet')!)
    expect(sheet).toContain('Mailie keeps it for 30 days, with what it stored for it, and any owner of Support can recover it until then')
    expect(sheet).toContain('Everyone in Support who reads it loses it, and is told.')
    await click(find('.danger-zone button', 'Remove account…'))
    await fill(find('.danger-zone input'), 'suporte@example.test')
    await click(find('.sheet-footer button', 'Remove'))
    await vi.waitFor(() => expect(words()).toContain('suporte@example.test was removed. Mailie keeps it until October 21, 2026: until then it can be recovered, under Removed mailboxes.'))
    await vi.waitFor(() => expect(find('.removed-item', 'suporte@example.test')).not.toBeNull())
    expect(words(find('.removed-item')!)).toContain('Removed by you on September 21, 2026.')
  })
})

describe('connecting an address removed and kept', () => {
  /** Ana links into the workspace shown; the server refuses the address as one it keeps removed there. */
  async function refused(role: string) {
    await signedIn(role, TEAM, ({ path, method }) => {
      if (path === '/v1/providers') return json([{ id: 'imap', oauth: false, password: true, flows: [] }])
      if (path === '/v1/me/sync-consent') return json({ consented: false, current_version: SYNC_TEXT_VERSION })
      if (path === '/v1/accounts' && method === 'POST') {
        return removedList.some(item => item.email === 'billing@example.test') ? failure('conflict', 409) : json({ account: account({ id: 'acc_new', email: 'billing@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: full }) }, 201)
      }
      return failure('not_found', 404)
    })
    await Promise.all([loadAccounts(), loadProviders(), loadConsent()])
    mounted = mount(AddAccountDialog, { resume: null })
    await flush()
    await click(find('dialog .provider-option', 'Other provider (IMAP)'))
    await fill(find('dialog input[name=mailbox]'), 'billing@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts')).toHaveLength(1))
  }

  it('offers an owner recovering it, which brings it back instead of a new link', async () => {
    removedList = [kept('acc_billing', 'billing@example.test')]
    await refused('owner')
    await vi.waitFor(() => expect(words(find('dialog')!)).toContain('billing@example.test was removed from this workspace on September 21, 2026, and is kept until October 21, 2026.'))
    await click(find('dialog .dialog-actions button', 'Recover'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_billing/recover')).toHaveLength(1))
    await vi.waitFor(() => expect(accounts.list.map(item => item.id)).toEqual(['acc_billing']))
    expect(accounts.list[0]!.sync.held).toBe(true)
    expect(sent('POST', '/v1/accounts')).toHaveLength(1)
  })

  it('offers an owner deleting it for good, its address typed again, and then connecting it anew', async () => {
    removedList = [kept('acc_billing', 'billing@example.test')]
    await refused('owner')
    await vi.waitFor(() => expect(find('dialog .dialog-actions button', 'Delete for good…')).not.toBeNull())
    await click(find('dialog .dialog-actions button', 'Delete for good…'))
    expect(find('dialog button', 'Delete for good')!.disabled).toBe(true)
    await fill(find('dialog input[name=purge-confirm]'), 'billing@example.test')
    await submit(find('dialog form.removed-offer'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_billing/purge').map(request => request.body)).toEqual([{ confirm: 'acc_billing' }]))
    await vi.waitFor(() => expect(words(find('dialog')!)).toContain('billing@example.test was deleted for good. You can connect it again now.'))
    await click(find('dialog .dialog-actions button', 'Connect it again'))
    // Back to the form as it was filled: connecting now links it anew.
    expect(find('dialog input[name=mailbox]')!.value).toBe('billing@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts')).toHaveLength(2))
    await vi.waitFor(() => expect(words(find('dialog')!)).toContain('Account connected'))
  })

  it('tells an admin an owner can recover it, and asks for no removed mailboxes', async () => {
    removedList = [kept('acc_billing', 'billing@example.test')]
    await refused('admin')
    await vi.waitFor(() => expect(find('dialog .alert')).not.toBeNull())
    expect(words(find('dialog .alert')!)).toContain('or was removed from it and is still kept: an owner of the workspace can recover it, or delete it for good to connect it again.')
    expect(find('dialog .dialog-actions button', 'Recover')).toBeNull()
    expect(sent('GET', `/v1/workspaces/${TEAM}/removed-mailboxes`)).toEqual([])
  })
})

describe('a personal mailbox recovered', () => {
  it('is turned on again by its person, with nothing but enabled sent', async () => {
    const mine = recovered(kept('acc_mine', 'ana@gmail.example', { workspace_id: PERSONAL }))
    list = [mine]
    await signedIn('member', PERSONAL, ({ path, method }) => {
      if (path === '/v1/accounts/acc_mine/sync' && method === 'PUT') {
        list = [{ ...mine, sync: syncing() }]
        return json(syncing())
      }
      if (path === '/v1/me/sync-consent') return json({ consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: SYNC_TEXT_VERSION })
      return failure('not_found', 404)
    })
    await Promise.all([loadAccounts(), loadConsent()])
    mounted = mount(AccountsPanel)
    await flush()
    expect(words(find('.account-card')!)).toContain('Paused since it was recovered on')
    expect(removed.loaded).toBe(true)
    openDetail('acc_mine')
    await flush()
    expect(words(find('.sheet')!)).toContain('Connected. Sync is paused.')
    await click(find('.sheet button', 'Turn sync on again'))
    await vi.waitFor(() => expect(sent('PUT', '/v1/accounts/acc_mine/sync').map(request => request.body)).toEqual([{ enabled: true }]))
    await vi.waitFor(() => expect(words(find('.account-card')!)).toContain('Indexing the last 90 days, newest first.'))
    expect(words(find('.account-card')!)).not.toContain('Paused since it was recovered')
    expect(find('.sheet button', 'Turn sync on again')).toBeNull()
  })
})
