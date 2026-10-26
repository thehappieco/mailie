// Pausing one mailbox's sync as a person presses it, on a page (test/dom.ts),
// against a server that answers as the daemon does (docs/workspaces.md,
// "Pausing a mailbox's sync"): a person pauses their own mailbox after
// reading what a pause keeps and what waits for the resume, and resumes it
// with no text named; a team's owner pauses its mailbox, sees who paused it,
// and resumes it through the team's sync text, sending the revision shown,
// or is offered a reload once the server names another; a team mailbox
// nobody reads, or only a suspended member would, offers no Resume; a member
// is told an owner or an admin paused it, never who, and is offered nothing;
// a paused mailbox offers no Sync now, and its team's row only turning it
// off, which says it ends the pause, and says the resume is what agrees
// again; a resume or a turn-off by a reader told of the pause takes the
// notice away; a recovered mailbox comes back paused, held or off, and says
// which, and is resumed in an edition that does not offer the pause too,
// focus kept in the sheet when nothing takes Resume's place; and a pause
// refused is said in the console's words.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { click, fill, find, flush, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Account, AccountSync, MailboxAccess, Member, PersonNotice, RemovedMailbox, Workspace } from '../src/api/types'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import PersonNotices from '../src/components/PersonNotices.vue'
import { configureEdition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import { SYNC_TEXT_VERSION } from '../src/open/versions'
import { accounts, loadAccounts, openDetail } from '../src/state/accounts'
import { adoptSession, signOut } from '../src/state/session'
import { loadConsent } from '../src/state/sync'
import { loadWorkspaces, selectWorkspace } from '../src/state/workspaces'
import { announcement } from '../src/ui/announce'
import { account, ana, failure, json, reply, serve, stubPage, syncOff, syncing, type Route } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const BEA = 'usr_00000000000000b2'
const DAY = 86_400
/** A text revision the server names after this page was loaded. */
const NEWER = '2027-01-open-sync-5'
const full = { read: true, act: true, send: true, manage: true }
const none = { read: false, act: false, send: false, manage: false }
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })
const member = (userID: string, fields: Partial<Member> = {}): Member => ({ user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1_790_000_000, ...fields })

/** A mailbox that syncs, up to date, with what it indexed. */
const live = (): AccountSync => syncing({ running: true, state: 'live', initial_progress: 100, last_synced_at: 1_790_000_000 })
/** The same, paused since then: no worker, its index and agreement kept. */
const pausedSync = (fields: Partial<AccountSync> = {}): AccountSync => ({ ...live(), running: false, state: 'stopped', paused: true, paused_since: 1_790_100_000, ...fields })
const mine = (sync: AccountSync = live()): Account => account({ id: 'acc_mine', email: 'ana@gmail.example', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: PERSONAL, access: full, sync })
const shared = (sync: AccountSync = live(), access = full): Account => account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access, sync })
const agreed = (fields: Partial<NonNullable<MailboxAccess['sync']>> = {}): NonNullable<MailboxAccess['sync']> =>
  ({ enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: SYNC_TEXT_VERSION, current: true, ...fields })
/** What Ana was told when Bea paused the team's mailbox she reads. */
const pausedNotice = (): PersonNotice => ({
  id: 'mbe_0000000000000004', kind: 'paused', workspace_id: TEAM, workspace_name: 'Support', account_id: 'acc_shared', mailbox: 'suporte@example.test', at: 1_790_100_000, state: 'live',
})

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
let mounted: Mounted | null = null
/** What the server holds: the accounts, the team's directory entry of each, and the text revision it asks about. */
let list: Account[] = []
let entries: Record<string, MailboxAccess> = {}
let removedList: RemovedMailbox[] = []
let currentVersion = SYNC_TEXT_VERSION
let consented = true

function entryOf(item: Account, fields: Partial<MailboxAccess> = {}): MailboxAccess {
  return {
    account_id: item.id, email: item.email, provider: 'imap', state: 'active', linked_by: BEA, readers: 2, no_reader: false, sync: agreed(),
    grants: [{ account_id: item.id, user_id: BEA, read: true, act: true, send: true, manage: false, updated_at: 1 }, { account_id: item.id, user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1 }],
    keys: [], ...fields,
  }
}

/**
 * Signed in as Ana, with this role in Support and the workspace shown; the
 * server pauses, resumes and recovers as the daemon does: a pause only of a
 * mailbox whose agreement stands and that nothing holds, a team's resume
 * only with the current revision and someone who reads it, recording the
 * agreement by whoever resumed it.
 */
async function signedIn(role: string, shown: string, route: Route = () => failure('not_found', 404)) {
  serve(request => {
    const { path, method, body } = request
    seen.push({ path, method, body })
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/workspaces') return json([personal, support(role)])
    if (path === '/v1/accounts' && method === 'GET') return json(list.filter(item => item.workspace_id === new URL(request.url).searchParams.get('workspace')))
    if (path === '/v1/me/sync-consent') return json(consented ? { consented: true, consented_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current_version: currentVersion } : { consented: false, current_version: currentVersion })
    if (path === `/v1/workspaces/${TEAM}/members`) return role === 'member' ? failure('not_authorized', 403) : json([member(ana.id, { email: ana.email, name: ana.name, role }), member(BEA, { name: 'Bea Lima' })])
    if (path === `/v1/workspaces/${TEAM}/access`) return role === 'member' ? failure('not_authorized', 403) : json(Object.values(entries))
    if (path === `/v1/workspaces/${TEAM}/removed-mailboxes` || path === `/v1/workspaces/${PERSONAL}/removed-mailboxes`) return json(removedList.filter(item => path.includes(item.workspace_id)))
    if (path === '/v1/events') return failure('not_authorized', 403)
    const act = path.match(/^\/v1\/accounts\/([^/]+)\/sync\/(pause|resume)$/)
    const item = act ? list.find(entry => entry.id === act[1]) : undefined
    if (act && method === 'POST') {
      const custom = route(request)
      if (custom instanceof Response && custom.status !== 404) return custom
      if (!item) return failure('not_found', 404)
      const team = item.workspace_id === TEAM
      if (act[2] === 'pause') {
        if (item.sync.held || !item.sync.enabled) return failure('conflict', 409)
        if (!item.sync.paused) {
          item.sync = { ...item.sync, running: false, state: 'stopped', paused: true, paused_since: 1_790_200_000 }
          if (team) entries[item.id] = { ...entries[item.id]!, paused_at: 1_790_200_000, paused_by: ana.id }
        }
        return json(item.sync)
      }
      const version = (body as { version?: string } | undefined)?.version
      if (team ? version !== currentVersion : version !== undefined) return failure('bad_request', 400)
      if (team && entries[item.id]?.readers === 0) return failure('conflict', 409)
      if (item.sync.paused) {
        const { paused: _, paused_since: __, ...rest } = item.sync
        item.sync = { ...rest, running: true, state: 'live' }
        if (team) {
          const { paused_at: _at, paused_by: _by, ...entry } = entries[item.id]!
          entries[item.id] = { ...entry, sync: agreed({ enabled_at: 1_790_200_000, enabled_by: ana.id, version }) }
        }
      }
      return json(item.sync)
    }
    return route(request)
  })
  await adoptSession(reply())
  await loadWorkspaces()
  selectWorkspace(shown)
  await Promise.all([loadAccounts(), loadConsent()])
}
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)
const sheet = () => find('.sheet')!
const card = () => find('.account-card')!
const lastDialog = () => page.querySelectorAll('dialog').at(-1)!

/** The sheet of one mailbox, with the accounts panel behind it, once the team's directory (for its owners and admins) is read. */
async function openSheet(id: string, directoryRead: boolean) {
  mounted = mount(AccountsPanel)
  await flush()
  openDetail(id)
  await vi.waitFor(() => expect(find('dialog.sheet-backdrop')).not.toBeNull())
  if (directoryRead) await vi.waitFor(() => expect(find('.team-sync button[role=switch], .team-sync .session-actions')).not.toBeNull())
  await flush()
}

/** An edition's sync text that says whom it is drawn for. */
const SyncProbe = defineComponent({
  props: { team: { type: String, default: '' } },
  setup: props => () => h('p', { class: 'sync-probe' }, props.team ? `drawn for the team ${props.team}` : 'drawn for my own mailboxes'),
})

beforeEach(() => {
  stubPage()
  seen = []
  list = []
  entries = {}
  removedList = []
  currentVersion = SYNC_TEXT_VERSION
  consented = true
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  configureEdition(openEdition)
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('pausing a personal mailbox', () => {
  it('a person pauses their own mailbox after reading what a pause keeps, and resumes it', async () => {
    list = [mine()]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    expect(find('.sheet button', 'Sync now')).not.toBeNull()
    await click(find('.sheet button', 'Pause sync…'))
    const dialog = words(lastDialog())
    expect(dialog).toContain('Pause sync of ana@gmail.example?')
    expect(dialog).toContain('Mailie stops indexing new mail for this mailbox and keeps what it already indexed: you can still search it, open its messages, act on them and send from it. Nothing is deleted. Resume sync to catch up.')
    // Cancel sends nothing.
    await click(find('dialog button', 'Cancel'))
    expect(sent('POST', '/v1/accounts/acc_mine/sync/pause')).toEqual([])
    await click(find('.sheet button', 'Pause sync…'))
    await click(find('dialog button.primary', 'Pause sync'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_mine/sync/pause').map(request => request.body)).toEqual([{}]))
    await vi.waitFor(() => expect(words(card())).toContain('Paused since '))
    expect(words(card())).toContain('Mailie keeps what it indexed, so it can still be searched and read, but indexes nothing new until sync is resumed.')
    expect(words(sheet())).toContain('Sync paused on ')
    expect(words(sheet())).toContain('Connected. Sync is paused.')
    expect(announcement.value).toBe('Sync of ana@gmail.example is paused. What Mailie indexed is kept.')
    // The dialog closed; focus is on the button that took the place of the one pressed.
    expect(find('dialog.console-dialog')).toBeNull()
    expect(words(page.activeElement!)).toBe('Resume sync')

    await click(find('.sheet button', 'Resume sync'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_mine/sync/resume').map(request => request.body)).toEqual([{}]))
    await vi.waitFor(() => expect(words(card())).toContain('Up to date'))
    expect(announcement.value).toBe('Sync of ana@gmail.example is resumed.')
    expect(find('.sheet button', 'Pause sync…')).not.toBeNull()
    expect(words(page.activeElement!)).toBe('Pause sync…')
    // Neither act is a consent: sync was never switched.
    expect(seen.filter(request => request.method === 'PUT' || request.method === 'DELETE')).toEqual([])
  })

  it('the pause dialog says what waits for the resume', async () => {
    list = [mine()]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    await click(find('.sheet button', 'Pause sync…'))
    expect(words(find('dialog .note')!)).toBe('While it is paused, what changes on the mail server — new mail, messages moved or deleted there — shows in Mailie only once sync is resumed, and a message there may not open until then.')
  })

  it('a paused mailbox offers no Sync now', async () => {
    list = [mine(pausedSync())]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    expect(words(find('.sheet .sync-facts')!)).toContain('1,284')
    expect(find('.sheet button', 'Sync now')).toBeNull()
    expect(find('.sheet button', 'Pause sync…')).toBeNull()
    expect(sent('POST', '/v1/accounts/acc_mine/sync')).toEqual([])
  })

  it('a Sync now refused for a mailbox paused elsewhere reads it again and says it is paused, not to try again', async () => {
    list = [mine()]
    await signedIn('member', PERSONAL, ({ path, method }) => {
      // Paused in another tab meanwhile: no event says so, and the daemon refuses the pass.
      if (path === '/v1/accounts/acc_mine/sync' && method === 'POST') { list[0]!.sync = pausedSync(); return failure('conflict', 409) }
      if (path === '/v1/accounts/acc_mine' && method === 'GET') return json(list[0])
      return failure('not_found', 404)
    })
    await openSheet('acc_mine', false)
    await click(find('.sheet button', 'Sync now'))
    await vi.waitFor(() => expect(words(card())).toContain('Paused since '))
    expect(sent('GET', '/v1/accounts/acc_mine')).toHaveLength(1)
    expect(find('.sheet button', 'Sync now')).toBeNull()
    expect(words(find('.sheet .alert[role=alert]')!)).toBe('Sync of this mailbox is paused, so Mailie does not sync it now. Resume it to catch up.')
    // Once resumed, the refusal is no longer the news.
    await click(find('.sheet button', 'Resume sync'))
    await vi.waitFor(() => expect(find('.sheet button', 'Sync now')).not.toBeNull())
    expect(words(sheet())).not.toContain('Mailie cannot sync this mailbox right now')
    expect(words(sheet())).not.toContain('Resume it to catch up')
  })

  it('a paused mailbox that kept no index says nothing is indexed and that a resume indexes the last 90 days', async () => {
    list = [mine(pausedSync({ state: 'off', messages: 0, folders_synced: 0, folders_total: 0, initial_progress: 0, last_synced_at: undefined }))]
    await signedIn('member', PERSONAL)
    mounted = mount(AccountsPanel)
    await flush()
    expect(words(card())).toContain('Paused since September 22, 2026. Nothing of this mailbox is indexed: once sync is resumed, Mailie indexes the last 90 days.')
    expect(words(card())).not.toContain('keeps what it indexed')
  })

  it('a pause refused for a mailbox whose sync is off says so, in the console’s words', async () => {
    list = [mine()]
    // Turned off meanwhile, elsewhere: the server has nothing to pause.
    await signedIn('member', PERSONAL, ({ path }) => path.endsWith('/sync/pause') ? failure('conflict', 409) : failure('not_found', 404))
    await openSheet('acc_mine', false)
    await click(find('.sheet button', 'Pause sync…'))
    await click(find('dialog button.primary', 'Pause sync'))
    await vi.waitFor(() => expect(find('dialog.console-dialog .alert')).not.toBeNull())
    expect(words(find('dialog.console-dialog .alert')!)).toBe('This mailbox’s sync is off, so there is nothing to pause.')
    expect(words()).not.toContain('hunter2')
    // The dialog stays, so the person reads why.
    expect(words(lastDialog())).toContain('Pause sync of ana@gmail.example?')
  })

  it('the open edition offers the pause, and an edition that does not offers none on a mailbox that syncs', async () => {
    expect(openEdition.syncPause).toBe(true)
    configureEdition({ ...openEdition, syncPause: undefined })
    list = [mine()]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    expect(find('.sheet button', 'Sync now')).not.toBeNull()
    expect(find('.sheet button', 'Pause sync…')).toBeNull()
  })
})

describe('a recovered mailbox', () => {
  /** Removed and kept in Ana's personal workspace. */
  const kept = (id: string, email: string): RemovedMailbox => ({
    id, workspace_id: PERSONAL, email, provider: 'imap', removed_at: 1_790_000_000, removed_by: ana.id, purge_after: 1_790_000_000 + 30 * DAY, index_kept: true, readers: 1, no_reader: false,
  })

  it('a recovered mailbox reads Paused and offers Resume, in an edition that does not offer the pause too', async () => {
    configureEdition({ ...openEdition, syncPause: undefined })
    list = [mine(pausedSync())]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    expect(words(card())).toContain('Paused since ')
    await click(find('.sheet button', 'Resume sync'))
    await vi.waitFor(() => expect(words(card())).toContain('Up to date'))
    expect(sent('POST', '/v1/accounts/acc_mine/sync/resume')).toHaveLength(1)
    // Syncing again, and the edition offers no pause.
    expect(find('.sheet button', 'Pause sync…')).toBeNull()
  })

  it('a resume that leaves no Pause in its place puts focus on the Sync heading, in the sheet', async () => {
    // No pause offered, or one the mailbox cannot take once resumed (it needs authorization): nothing replaces Resume.
    for (const [syncPause, state] of [[undefined, 'active'], [true, 'needs_reauth']] as const) {
      configureEdition({ ...openEdition, syncPause })
      list = [account({ ...mine(pausedSync()), state })]
      seen = []
      await signedIn('member', PERSONAL)
      await openSheet('acc_mine', false)
      await click(find('.sheet button', 'Resume sync'))
      await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_mine/sync/resume')).toHaveLength(1))
      await vi.waitFor(() => expect(find('.sheet button', 'Resume sync')).toBeNull())
      await flush()
      expect(find('.sheet button', 'Pause sync…')).toBeNull()
      const focused = page.activeElement!
      expect(focused.isConnected).toBe(true)
      expect(sheet().contains(focused)).toBe(true)
      expect(focused.tagName).toBe('H3')
      expect(words(focused)).toBe('Sync')
      mounted?.unmount()
      mounted = null
      await signOut()
    }
  })

  it('a team resume that leaves no Pause in its place puts focus on the Sync heading, never on a block that draws nothing', async () => {
    configureEdition({ ...openEdition, syncPause: undefined })
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    await click(find('.sheet button', 'Resume sync for Support…'))
    await click(find('dialog button.primary', 'Resume sync for Support'))
    await vi.waitFor(() => expect(words(card())).toContain('Up to date'))
    await flush()
    expect(find('dialog.console-dialog')).toBeNull()
    // The pause block draws nothing now, and is hidden (.sync-pause:empty).
    expect(words(sheet().querySelector('.sync-pause')!)).toBe('')
    const focused = page.activeElement!
    expect(focused.isConnected).toBe(true)
    expect(sheet().contains(focused)).toBe(true)
    expect(focused.tagName).toBe('H3')
    expect(words(focused)).toBe('Sync')
  })

  it('a recovered notice says paused, turned off or off, as the mailbox came back', async () => {
    removedList = [kept('acc_paused', 'paused@example.test'), kept('acc_held', 'held@example.test'), kept('acc_off', 'off@example.test')]
    const came: Record<string, AccountSync> = {
      acc_paused: pausedSync({ messages: 840 }),
      acc_held: syncOff({ held: true, held_since: 1_790_200_000 }),
      acc_off: syncOff(),
    }
    await signedIn('member', PERSONAL, ({ path, method }) => {
      const recover = path.match(/^\/v1\/accounts\/([^/]+)\/recover$/)
      if (recover && method === 'POST') {
        const item = removedList.find(entry => entry.id === recover[1])!
        removedList = removedList.filter(entry => entry !== item)
        const back = account({ id: item.id, email: item.email, provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: PERSONAL, access: full, sync: came[item.id]! })
        list = [...list, back]
        return json(back)
      }
      return failure('not_found', 404)
    })
    mounted = mount(AccountsPanel)
    await vi.waitFor(() => expect(find('.removed-item', 'paused@example.test')).not.toBeNull())
    const recover = async (email: string, said: string) => {
      const item = page.querySelectorAll('.removed-item').find(entry => words(entry).includes(email))!
      await click(item.querySelectorAll('button').find(button => words(button) === 'Recover')!)
      await vi.waitFor(() => expect(words()).toContain(said))
    }
    await recover('paused@example.test', 'paused@example.test was recovered. Its sync stays paused until it is resumed.')
    await recover('held@example.test', 'held@example.test was recovered. Its sync stays off until it is turned on again.')
    await recover('off@example.test', 'off@example.test was recovered. Its sync is off until it is turned on.')
    expect(announcement.value).toBe('off@example.test was recovered. Its sync is off until it is turned on.')
  })

  it('a personal mailbox recovered while its person’s sync is off reads sync off and offers turning sync on, never Resume', async () => {
    consented = false
    list = [mine(syncOff())]
    await signedIn('member', PERSONAL)
    await openSheet('acc_mine', false)
    const text = words(sheet())
    expect(text).toContain('Sync is off. Nothing from this mailbox is stored.')
    expect(text).not.toContain('Paused')
    expect(find('.sheet button', 'Turn on sync…')).not.toBeNull()
    expect(find('.sheet button', 'Resume sync')).toBeNull()
    expect(find('.sheet button', 'Pause sync…')).toBeNull()
  })
})

describe('pausing a team mailbox', () => {
  it('a team’s owner pauses its mailbox, says the others who read it are told, and sees who paused it', async () => {
    list = [shared()]
    entries = { acc_shared: entryOf(list[0]!) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    await click(find('.sheet button', 'Pause sync…'))
    expect(words(lastDialog())).toContain('Mailie stops indexing new mail for this mailbox, for everyone in Support who reads it, and keeps what it already indexed: they can still search it, open its messages, act on them and send from it. Nothing is deleted, and the others who read it are told. Resume sync to catch up.')
    await click(find('dialog button.primary', 'Pause sync'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_shared/sync/pause').map(request => request.body)).toEqual([{}]))
    // The directory is read again: it names who paused it.
    await vi.waitFor(() => expect(words(sheet())).toContain('Sync paused by Ana Souza on September 23, 2026.'))
    expect(find('.sheet button', 'Resume sync for Support…')).not.toBeNull()
    expect(find('.sheet button', 'Sync now')).toBeNull()
  })

  it('a team’s owner resumes it after agreeing to the team’s sync text, and the revision shown is the one sent', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    expect(words(sheet())).toContain('Sync paused by Bea Lima on ')
    await click(find('.sheet button', 'Resume sync for Support…'))
    const dialog = words(lastDialog())
    expect(dialog).toContain('Resume sync for Support?')
    expect(dialog).toContain('You agree to this text on behalf of Support: suporte@example.test then syncs again under the team’s agreement, for everyone in the team who reads it, from where it stopped.')
    expect(dialog).toContain('Who can read the index')
    await click(find('dialog button.primary', 'Resume sync for Support'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_shared/sync/resume').map(request => request.body)).toEqual([{ version: SYNC_TEXT_VERSION }]))
    await vi.waitFor(() => expect(words(card())).toContain('Up to date'))
    expect(announcement.value).toBe('Sync of suporte@example.test is resumed for Support.')
    // The agreement is recorded by whoever resumed it: the team's row says so once read again.
    await vi.waitFor(() => expect(words(find('.team-sync')!)).toContain('turned on by Ana Souza'))
    expect(find('.sheet button', 'Pause sync…')).not.toBeNull()
  })

  it('draws the edition’s sync text for the team a resume agrees for', async () => {
    configureEdition({ ...openEdition, sync: { ...openEdition.sync, component: SyncProbe } })
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('admin', TEAM)
    await openSheet('acc_shared', true)
    await click(find('.sheet button', 'Resume sync for Support…'))
    expect(words(lastDialog())).toContain('drawn for the team Support')
  })

  it('a team resume refused for an older text offers a reload, and sends nothing more', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('admin', TEAM)
    await openSheet('acc_shared', true)
    // The server moved to another text after this page read it.
    currentVersion = NEWER
    await click(find('.sheet button', 'Resume sync for Support…'))
    await click(find('dialog button.primary', 'Resume sync for Support'))
    await vi.waitFor(() => expect(find('dialog button.primary', 'Reload page')).not.toBeNull())
    expect(sent('POST', '/v1/accounts/acc_shared/sync/resume').map(request => request.body)).toEqual([{ version: SYNC_TEXT_VERSION }])
    expect(words(lastDialog())).toContain('The text about sync changed on this server while this page was open. Reload the page to read the current text.')
    expect(find('dialog button.primary', 'Resume sync for Support')).toBeNull()
    expect(find('dialog.console-dialog .alert')).toBeNull()
    expect(words(card())).toContain('Paused since ')
  })

  it('a team mailbox nobody reads offers no Resume', async () => {
    list = [shared(pausedSync({ enabled: false }), { ...none, manage: true })]
    entries = { acc_shared: entryOf(list[0]!, { readers: 0, no_reader: true, grants: [], paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    expect(words(sheet())).toContain('Sync paused by Bea Lima on ')
    expect(find('.sheet button', 'Resume sync for Support…')).toBeNull()
    expect(words(sheet())).toContain('Nobody in Support can read this mailbox, so it does not sync')
  })

  it('a team mailbox only a suspended member would read offers no Resume and says it waits for them', async () => {
    list = [shared(pausedSync({ enabled: false }), { ...none, manage: true })]
    entries = { acc_shared: entryOf(list[0]!, { readers: 0, no_reader: false, paused_at: 1_790_100_000 }) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    expect(words(sheet())).toContain('Sync paused on ')
    expect(find('.sheet button', 'Resume sync for Support…')).toBeNull()
    expect(words(sheet())).toContain('Nobody in Support reads it now, but someone suspended on this server would once resumed: its sync is resumed once someone reads it.')
  })

  it('a member sees that an owner or an admin paused it and is offered nothing', async () => {
    list = [shared(pausedSync(), { ...none, read: true })]
    await signedIn('member', TEAM)
    await openSheet('acc_shared', false)
    const text = words(sheet())
    expect(text).toContain('Sync paused by an owner or an admin of Support on ')
    expect(text).toContain('The owners and admins of Support resume it, for the team.')
    expect(text).not.toContain('Bea Lima')
    expect(find('.sheet button', 'Resume sync')).toBeNull()
    expect(find('.sheet button', 'Resume sync for Support…')).toBeNull()
    expect(find('.sheet button', 'Pause sync…')).toBeNull()
    expect(find('.sheet button', 'Sync now')).toBeNull()
    // Who paused it is the access directory's, which a member never reads.
    expect(sent('GET', `/v1/workspaces/${TEAM}/access`)).toEqual([])
  })

  it('a paused team mailbox offers neither turning on nor confirming in the team’s sync row', async () => {
    // An agreement to an earlier text would offer confirming it; while paused, the resume is the one way back, and it agrees.
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { sync: agreed({ version: '2026-10-open-sync-3', current: false }), paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('admin', TEAM)
    await openSheet('acc_shared', true)
    const row = find('.team-sync')!
    expect(row.querySelector('button[role=switch]')).toBeNull()
    expect(find('.team-sync button', 'Confirm for Support…')).toBeNull()
    expect(find('.team-sync button', 'Turn off…')).not.toBeNull()
    // It says the resume above is what agrees, never that it keeps syncing or to confirm it here.
    expect(words(row)).toContain('On for Support since September 21, 2026, under an earlier text of sync, and paused. Resuming its sync agrees to the current text for Support; turning it off deletes its index for everyone who reads it.')
    expect(words(row)).not.toContain('keeps syncing')
    expect(words(row)).not.toContain('confirm it')
  })

  it('a paused team mailbox still tied to whoever connected it says its resume confirms it for the team', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { sync: agreed({ migrated: true, version: '2026-10-open-sync-3', current: false }), paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('owner', TEAM)
    await openSheet('acc_shared', true)
    const row = find('.team-sync')!
    expect(words(row)).toContain('On since September 21, 2026 under the agreement Bea Lima gave for their own mailboxes before this server was updated, and paused. It is still tied to them: if they turn their sync off or their account is closed, it stops and its index is deleted. Resuming its sync confirms it for Support, under the current text.')
    expect(words(row)).not.toContain('to keep it syncing')
    expect(find('.team-sync button', 'Confirm for Support…')).toBeNull()
    expect(find('.team-sync button', 'Turn off…')).not.toBeNull()
    expect(find('.sheet button', 'Resume sync for Support…')).not.toBeNull()
  })

  it('a reader told of the pause who resumes it no longer sees the notice', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    let given = [pausedNotice()]
    await signedIn('admin', TEAM, ({ path, method }) => {
      if (path === '/v1/me/notices' && method === 'GET') return json(given)
      // The resume withdraws the pause's notices in its transaction; no event says so.
      if (path === '/v1/accounts/acc_shared/sync/resume' && method === 'POST') given = []
      return failure('not_found', 404)
    })
    const notes = mount(PersonNotices)
    try {
      await vi.waitFor(() => expect(find('.person-notice', 'suporte@example.test')).not.toBeNull())
      await openSheet('acc_shared', true)
      await click(find('.sheet button', 'Resume sync for Support…'))
      await click(find('dialog button.primary', 'Resume sync for Support'))
      await vi.waitFor(() => expect(words(card())).toContain('Up to date'))
      await vi.waitFor(() => expect(find('.person-notices')).toBeNull())
      expect(sent('GET', '/v1/me/notices')).toHaveLength(2)
    } finally {
      notes.unmount()
    }
  })

  it('a reader told of the pause who turns its sync off no longer sees the notice', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    let given = [pausedNotice()]
    await signedIn('owner', TEAM, ({ path, method }) => {
      if (path === '/v1/me/notices' && method === 'GET') return json(given)
      if (path === '/v1/accounts/acc_shared/sync' && method === 'PUT') {
        // Turning it off ends the pause, and withdraws its notices with it.
        given = []
        list = [shared(syncOff())]
        const { paused_at: _at, paused_by: _by, ...entry } = entries.acc_shared!
        entries = { acc_shared: { ...entry, sync: { enabled: false, current: false } } }
        return json(syncOff())
      }
      return failure('not_found', 404)
    })
    const notes = mount(PersonNotices)
    try {
      await vi.waitFor(() => expect(find('.person-notice', 'suporte@example.test')).not.toBeNull())
      await openSheet('acc_shared', true)
      await click(find('.team-sync button', 'Turn off…'))
      await fill(find('dialog input[name=team-sync-confirm]'), 'suporte@example.test')
      await submit(find('dialog form.team-sync-confirm'))
      await vi.waitFor(() => expect(sent('PUT', '/v1/accounts/acc_shared/sync')).toHaveLength(1))
      await vi.waitFor(() => expect(find('.person-notices')).toBeNull())
    } finally {
      notes.unmount()
    }
  })

  it('turning a paused team mailbox off says it ends its pause', async () => {
    list = [shared(pausedSync())]
    entries = { acc_shared: entryOf(list[0]!, { paused_at: 1_790_100_000, paused_by: BEA }) }
    await signedIn('owner', TEAM, ({ path, method }) => {
      if (path === '/v1/accounts/acc_shared/sync' && method === 'PUT') {
        list = [shared(syncOff())]
        const { paused_at: _at, paused_by: _by, ...entry } = entries.acc_shared!
        entries = { acc_shared: { ...entry, sync: { enabled: false, current: false } } }
        return json(syncOff())
      }
      return failure('not_found', 404)
    })
    await openSheet('acc_shared', true)
    await click(find('.team-sync button', 'Turn off…'))
    expect(words(lastDialog())).toContain('This also ends its pause.')
    await fill(find('dialog input[name=team-sync-confirm]'), 'suporte@example.test')
    await submit(find('dialog form.team-sync-confirm'))
    await vi.waitFor(() => expect(sent('PUT', '/v1/accounts/acc_shared/sync').map(request => request.body)).toEqual([{ enabled: false }]))
    await vi.waitFor(() => expect(words(card())).not.toContain('Paused since'))
    expect(accounts.list[0]!.sync.paused).toBeUndefined()
  })
})
