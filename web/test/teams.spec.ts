// Workspaces and teams as the console renders them (server-side, as
// render.spec.ts does): what each mailbox card says the person may do, what a
// mailbox's access panel offers and says it will not, the team's people with
// their protections, the switcher and the sections it changes, and the
// invitation shown to someone signed in. Every rule shown here is one the
// server enforces; these hold the console to saying it beforehand.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import type { MailboxAccess, Member, TeamInvite, Workspace } from '../src/api/types'
import AccessPanel from '../src/components/AccessPanel.vue'
import AccountSheet from '../src/components/AccountSheet.vue'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import InvitationDialog from '../src/components/InvitationDialog.vue'
import StoragePanel from '../src/components/StoragePanel.vue'
import OpenConsole from '../src/open/OpenConsole.vue'
import OpenMembers from '../src/open/OpenMembers.vue'
import TeamInviteDialog from '../src/open/TeamInviteDialog.vue'
import { SYNC_TEXT_VERSION } from '../src/open/versions'
import { accounts, connect } from '../src/state/accounts'
import { invitation } from '../src/state/invitation'
import { session } from '../src/state/session'
import { storage } from '../src/state/storage'
import { consent } from '../src/state/sync'
import { team } from '../src/state/team'
import { workspaces } from '../src/state/workspaces'
import { locale } from '../src/ui/i18n'
import { account, ana, syncing } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const BEA = 'usr_00000000000000b2'
const CAROL = 'usr_00000000000000c3'
const full = { read: true, act: true, send: true, manage: true }
const none = { read: false, act: false, send: false, manage: false }
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string, fields: Partial<Workspace> = {}): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000, ...fields })
const member = (userID: string, fields: Partial<Member> = {}): Member => ({ user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1_790_000_000, ...fields })
const anaMember = (fields: Partial<Member> = {}) => member(ana.id, { email: ana.email, name: ana.name, ...fields })
const beaMember = (fields: Partial<Member> = {}) => member(BEA, { email: 'bea@example.test', name: 'Bea Lima', ...fields })

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), context)
  return html + Object.values(context.teleports ?? {}).join('')
}
/** The text of the markup, without its tags. */
const text = (html: string) => html.replace(/<!--[^]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/&#39;/g, '\'').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim()

/** Signed in as Ana, showing workspace id, with these workspaces. */
function showing(id: string, list: Workspace[]) {
  Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
  Object.assign(workspaces, { list, loaded: true, supported: true, currentID: id, failure: null, lost: null })
}

function teamState(members: Member[], directory: MailboxAccess[] = [], invites: TeamInvite[] = []) {
  Object.assign(team, {
    workspace: TEAM,
    members: { list: members, loaded: true, loading: false, failure: null },
    directory: { list: directory, loaded: true, loading: false, failure: null },
    invites: { list: invites, loaded: true, loading: false, failure: null },
  })
}

afterEach(() => {
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0 })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  Object.assign(accounts, { list: [], workspace: '', loaded: false, loading: false, detailID: '', folders: {}, notice: null, providers: [], providersLoaded: false })
  Object.assign(connect, { phase: 'idle', provider: '', email: '', accountID: '', created: false, flow: '', failure: null })
  Object.assign(consent, { loaded: false, consented: false, version: '', consentedAt: 0, currentVersion: '', dismissed: false, busy: '', problem: null, failure: null })
  Object.assign(storage, { workspace: '', loaded: false, loading: false, failure: null, usage: null })
  Object.assign(invitation, { pending: null, busy: false, problem: null, joined: null })
  Object.assign(team, { workspace: '', members: { list: [], loaded: false, loading: false, failure: null }, directory: { list: [], loaded: false, loading: false, failure: null }, invites: { list: [], loaded: false, loading: false, failure: null } })
  locale.value = 'en'
  vi.unstubAllGlobals()
})

describe('mailboxes in a team', () => {
  const readOnly = account({ id: 'acc_read', email: 'suporte@example.test', state: 'needs_reauth', auth_kind: 'oauth2', workspace_id: TEAM, access: { ...none, read: true } })
  const cardOnly = account({ id: 'acc_card', email: 'vendas@example.test', state: 'active', workspace_id: TEAM, access: { ...none, send: true } })
  /** An owner's or an admin's card of a team mailbox they hold no grant on: manage, by their role. */
  const managed = account({ id: 'acc_managed', email: 'diretoria@example.test', state: 'active', workspace_id: TEAM, access: { ...none, manage: true }, sync: syncing() })
  const entryOf = (item: { id: string; email: string }, fields: Partial<MailboxAccess> = {}): MailboxAccess => ({
    account_id: item.id, email: item.email, provider: 'gmail', state: 'active', linked_by: BEA, readers: 1, no_reader: false,
    sync: { enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: SYNC_TEXT_VERSION, current: true },
    grants: [{ account_id: item.id, user_id: BEA, read: true, act: true, send: true, manage: false, updated_at: 1 }], ...fields,
  })

  it('shows a member on each card what they may do with it, offers authorizing only to whoever manages it, and says who manages the team', async () => {
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [readOnly, cardOnly], loaded: true, workspace: TEAM })
    const html = await render(AccountsPanel)
    expect(text(html)).toContain('Mailboxes in Support')
    expect(text(html)).toContain('Your access Read')
    expect(text(html)).toContain('Your access Send')
    // Who connected a mailbox is the directory's record, for its owners and admins: never on a member's card.
    expect(text(html)).not.toContain('Linked by')
    // Needs authorizing, and Ana does not manage it: nothing she could press would work, and nothing tells her to.
    expect(html).not.toContain('Finish authorization')
    expect(text(html)).toContain('The provider asks for this mailbox to be authorized again, by someone who manages it. Mail is not syncing.')
    expect(text(html)).toContain('You can see this mailbox, but not read it. Read comes only from an owner or an admin of the team who reads it.')
    // In place of the sections a member is not shown.
    expect(text(html)).toContain('The people of Support, and who can use each of its mailboxes, are managed by its owners and admins.')
  })

  it('offers connecting a mailbox only where the person may link one: their personal workspace, or a team they own or administer', async () => {
    const mine = account({ id: 'acc_mine', email: 'ana@gmail.example', state: 'active', workspace_id: PERSONAL, access: full })
    // A member of Support: the button would only connect to her personal workspace, so it is not on the team's list.
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [readOnly, cardOnly], loaded: true, workspace: TEAM })
    expect(text(await render(AccountsPanel))).not.toContain('Connect an email account')
    for (const role of ['owner', 'admin']) {
      showing(TEAM, [personal, support(role)])
      teamState([anaMember({ role }), beaMember()], [])
      expect(text(await render(AccountsPanel)), role).toContain('Connect an email account')
    }
    showing(PERSONAL, [personal, support('member')])
    Object.assign(accounts, { list: [mine], loaded: true, workspace: PERSONAL })
    expect(text(await render(AccountsPanel))).toContain('Connect an email account')
  })

  it('lists for an owner or an admin every mailbox of the team as a card they manage by their role, never what it holds', async () => {
    for (const role of ['owner', 'admin']) {
      showing(TEAM, [personal, support(role)])
      teamState([anaMember({ role }), beaMember({ last_reader_of: [managed.id] })], [entryOf(managed)])
      Object.assign(accounts, { list: [managed], loaded: true, workspace: TEAM })
      const words = text(await render(AccountsPanel))
      expect(words, role).toContain('diretoria@example.test')
      expect(words, role).toContain('Your access Manage')
      // How far its sync has got is on the card; what it holds is not.
      expect(words, role).toContain('Messages indexed 1,284')
      expect(words, role).toContain('You manage this mailbox by your role, but do not read it. Read comes only from an owner or an admin who reads it.')
      // No second list of mailboxes "held by nobody": every card is in the one list.
      expect(words, role).not.toContain('Other mailboxes of Support')
      expect(words, role).not.toContain('The people of Support, and who can use each of its mailboxes, are managed by its owners and admins.')
    }
  })

  it('marks for its owners and admins a team mailbox nobody can read, and what is left to do about it', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true })], [entryOf(managed, { readers: 0, no_reader: true, grants: [] })])
    Object.assign(accounts, { list: [managed], loaded: true, workspace: TEAM })
    const words = text(await render(AccountsPanel))
    expect(words).toContain('Nobody can read this mailbox any more, so it does not sync. Remove it, or remove it and connect it again.')
    const sheet = text(await render(AccountSheet, { account: managed }))
    expect(sheet).toContain('Nobody in Support can read this mailbox any more, so it does not sync.')
    expect(sheet).toContain('Remove account')
  })

  it('asks no one in a team to agree to sync for its mailboxes: they sync under the team’s agreement', async () => {
    const notAgreed = () => Object.assign(consent, { loaded: true, consented: false, currentVersion: SYNC_TEXT_VERSION })
    for (const role of ['owner', 'member']) {
      showing(TEAM, [personal, support(role)])
      notAgreed()
      teamState([anaMember({ role })], [])
      Object.assign(accounts, { list: [cardOnly], loaded: true, workspace: TEAM })
      expect(await render(AccountsPanel), role).not.toContain('consent-card')
    }
    // Her personal workspace's mailboxes sync under her own.
    showing(PERSONAL, [personal, support('member')])
    notAgreed()
    Object.assign(accounts, { list: [account({ id: 'acc_mine', state: 'active', workspace_id: PERSONAL, access: full })], loaded: true, workspace: PERSONAL })
    expect(await render(AccountsPanel)).toContain('consent-card')
  })

  it('tells a member of a team with nothing shared yet where their own mailboxes go, and offers to show it rather than connect one there', async () => {
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    const html = await render(AccountsPanel)
    expect(text(html)).toContain('Nothing shared with you here yet')
    expect(text(html)).toContain('A mailbox of Support appears here once you are given access to it. Only its owners and admins connect mailboxes to it; yours are connected in your personal workspace.')
    expect(text(html)).toContain('Show your personal workspace')
    expect(text(html)).not.toContain('Connect an email account')
    // Not a word saying that managing a mailbox is enough to pass its mail on.
    expect(text(html)).not.toMatch(/whoever manages|someone who manages/i)
  })

  it('shows a member a mailbox seen without read as such in its sheet: no folders, no access, no sync switch, no removing', async () => {
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [cardOnly], loaded: true, workspace: TEAM })
    const html = await render(AccountSheet, { account: cardOnly })
    const words = text(html)
    expect(html).not.toContain('Show folders')
    expect(words).toContain('You do not have read access to this mailbox, so its folders and messages are not shown to you. It comes only from an owner or an admin of the team who reads it.')
    expect(words).toContain('Sync is off for this mailbox. The owners and admins of Support turn it on, for the team.')
    expect(html).not.toContain('Remove account')
    expect(html).not.toContain('Access to ')
    expect(html).not.toContain('role="switch"')
    expect(words).not.toContain('Linked by')
    // The person's own agreement does not sync a team's mailbox, so none is offered for it.
    expect(words).not.toContain('Turn on sync…')
  })

  it('shows an owner or an admin who does not read a team mailbox its card, access, sync and removal, never its folders', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember({ last_reader_of: [managed.id] })], [entryOf(managed)])
    Object.assign(accounts, { list: [managed], loaded: true, workspace: TEAM })
    const html = await render(AccountSheet, { account: managed })
    const words = text(html)
    expect(words).toContain('You manage this mailbox as an owner or an admin of Support, but do not read it, so its folders and messages are not shown to you.')
    expect(html).not.toContain('Show folders')
    // Asking for a pass is a reader's: the counters are the card's.
    expect(html).not.toContain('Sync now')
    expect(words).toContain('Linked by Bea Lima')
    expect(words).toContain('On for Support since')
    expect(words).toContain('turned on by Bea Lima')
    expect(words).toContain(`Agreed to the sync text of revision ${SYNC_TEXT_VERSION}.`)
    expect(html).toContain('role="switch"')
    expect(html).toContain('aria-label="Access to diretoria@example.test"')
    expect(words).toContain('Remove account')
    expect(words).toContain('Everyone in Support who has access to it loses it, and its index is deleted for all of them.')
  })

  it('says where a team mailbox’s agreement to sync stands: an earlier text, one carried over from whoever linked it, or stopped with its index', async () => {
    showing(TEAM, [personal, support('owner')])
    Object.assign(accounts, { list: [managed], loaded: true, workspace: TEAM })
    const sheet = async (sync: MailboxAccess['sync'], unread = false) => {
      const read = unread ? { readers: 0, no_reader: true, grants: [] } : {}
      teamState([anaMember({ role: 'owner', last_owner: true }), beaMember({ last_reader_of: unread ? [] : [managed.id] })], [entryOf(managed, { sync, ...read })])
      return render(AccountSheet, { account: managed })
    }
    const earlier = await sheet({ enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: '2026-10-open-sync-2', current: false })
    expect(text(earlier)).toContain('under an earlier text of sync. It keeps syncing: confirm it under the current text, or turn it off.')
    expect(text(earlier)).toContain('Confirm for Support…')
    expect(earlier).not.toContain('role="switch"')
    const migrated = await sheet({ enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: '2026-10-open-sync-2', migrated: true, current: false })
    expect(text(migrated)).toContain('under the agreement Bea Lima gave for their own mailboxes before this server was updated. It is still tied to them')
    expect(text(migrated)).toContain('Confirm for Support…')
    const kept = await sheet({ enabled: false, enabled_by: BEA, migrated: true, current: false })
    // Kept while stopped, and still tied to whoever connected it: deleting them deletes it.
    expect(text(kept)).toContain('Its index is kept while it stays stopped: turning its sync on for Support resumes from it, and turning it off, removing the mailbox or deleting their account on this server deletes it.')
    expect(kept).toContain('role="switch"')
    const off = await sheet({ enabled: false, current: false })
    expect(text(off)).toContain('Sync is off. Nothing from this mailbox is stored.')
    // Nobody can read it, nor be given Read on it: never turned on, only off while something of it is kept or on.
    const keptUnread = await sheet({ enabled: false, enabled_by: BEA, migrated: true, current: false }, true)
    expect(text(keptUnread)).toContain('Its index is kept while it stays stopped, until its sync is turned off, the mailbox is removed, or their account on this server is deleted.')
    expect(text(keptUnread)).toContain('Nobody in Support can read this mailbox, so it does not sync, and its sync cannot be turned on: Read is given only by someone who reads it.')
    expect(keptUnread).not.toContain('role="switch"')
    expect(text(keptUnread)).toContain('Turn off…')
    expect(text(keptUnread)).not.toContain('Confirm for Support…')
    const migratedUnread = await sheet({ enabled: true, enabled_at: 1_790_000_000, enabled_by: BEA, version: '2026-10-open-sync-2', migrated: true, current: false }, true)
    expect(text(migratedUnread)).toContain('Turn off…')
    expect(text(migratedUnread)).not.toContain('Confirm for Support…')
    const offUnread = await sheet({ enabled: false, current: false }, true)
    expect(offUnread).toMatch(/role="switch"[^>]*disabled/)
    // Once the person who gave it was deleted, the record names nobody.
    const anonymous = await sheet({ enabled: true, enabled_at: 1_790_000_000, version: SYNC_TEXT_VERSION, current: true })
    expect(text(anonymous)).toContain('On for Support since')
    expect(text(anonymous)).not.toContain('turned on by')
  })
})

describe('who can use a mailbox', () => {
  const entry = (grants: [string, Partial<typeof full>][], fields: Partial<MailboxAccess> = {}): MailboxAccess => {
    const listed = grants.map(([userID, flags]) => ({ account_id: 'acc_shared', user_id: userID, ...none, ...flags, updated_at: 1 }))
    const readers = listed.filter(grant => grant.read).length
    return { account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', linked_by: BEA, readers, no_reader: readers === 0, sync: { enabled: false, current: false }, grants: listed, ...fields }
  }
  /** The checkbox of a person's flag, as rendered. */
  const box = (html: string, userID: string, flag: string) => html.match(new RegExp(`<input[^>]*name="${userID}-${flag}"[^>]*>`))?.[0] ?? ''
  const reader = { read: true, act: true, send: true }

  it('lets an admin who does not read it turn on Send and give Manage, never Read, not even to herself, and says why', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember(), member(CAROL, { name: 'Carol' })], [entry([[BEA, reader]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('Each person needs their own access: being an owner or an admin of the team gives Manage, never its mail.')
    expect(text(html)).toContain('You do not read this mailbox, so you cannot give Read on it, not even to yourself: only an owner or an admin who reads it can.')
    expect(box(html, CAROL, 'read')).toContain('disabled')
    // Act goes only to someone who reads: Carol does not, and Ana cannot give her Read.
    expect(box(html, CAROL, 'act')).toContain('disabled')
    expect(box(html, CAROL, 'send')).not.toContain('disabled')
    expect(box(html, CAROL, 'manage')).not.toContain('disabled')
    // Bea reads it: Act may be turned on for her, by an admin who does not hold it.
    expect(box(html, BEA, 'act')).not.toContain('disabled')
    // Her own row: no Read, Send for herself, and Manage ticked by her role, not hers to change.
    expect(box(html, ana.id, 'read')).toContain('disabled')
    expect(box(html, ana.id, 'send')).not.toContain('disabled')
    expect(box(html, ana.id, 'manage')).toMatch(/checked/)
    expect(box(html, ana.id, 'manage')).toContain('disabled')
  })

  it('lets an owner who reads it give Read and Act to anyone in the team', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true }), beaMember(), member(CAROL, { name: 'Carol', role: 'admin' })], [entry([[ana.id, reader], [BEA, reader]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).not.toContain('You do not read this mailbox')
    for (const flag of ['read', 'act', 'send']) expect(box(html, CAROL, flag), flag).not.toContain('disabled')
    // Carol is an admin: she manages it by her role, and Manage is not given to her.
    expect(box(html, CAROL, 'manage')).toMatch(/checked/)
    expect(box(html, CAROL, 'manage')).toContain('disabled')
  })

  it('keeps Read with the only person who can read the mailbox, and says why', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true }), beaMember({ last_reader_of: ['acc_shared'] })], [entry([[BEA, reader]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('The only person who can read this mailbox: give someone else Read before taking theirs.')
    expect(box(html, BEA, 'read')).toContain('disabled')
    expect(box(html, BEA, 'act')).not.toContain('disabled')
    expect(box(html, BEA, 'send')).not.toContain('disabled')
  })

  it('says a mailbox nobody can read does not sync, and what is left to do about it', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true }), beaMember()], [entry([[BEA, { send: true }]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('Nobody in Support can read this mailbox any more, so it does not sync. Read is given only by someone who reads it: remove the mailbox, or remove it and connect it again, which gives Read to whoever connects it.')
    expect(box(html, BEA, 'read')).toContain('disabled')
  })

  it('offers no take-over and names no linker: a team’s mailbox is the team’s', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember()], [entry([[BEA, reader], [ana.id, reader]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).not.toMatch(/take over|taking over|linked by|syncs under the agreement of/i)
    for (const flag of ['read', 'act', 'send']) expect(box(html, BEA, flag), flag).not.toContain('disabled')
  })

  it('says so when a mailbox opened from the team’s list is no longer in it, and offers nothing for it', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember(), member(CAROL, { name: 'Carol' })], [])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('suporte@example.test is no longer a mailbox of Support: it was removed.')
    expect(html).not.toContain(`data-user="${CAROL}"`)
  })
})

describe('the people of a team', () => {
  const invites: TeamInvite[] = [{ id: 'inv_0000000000000002', email: 'dan@example.test', workspace_id: TEAM, role: 'admin', created_at: 1_790_000_000, expires_at: 1_790_604_800 }]
  const shared: MailboxAccess = { account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', readers: 1, no_reader: false, grants: [{ account_id: 'acc_shared', user_id: BEA, read: true, act: false, send: false, manage: false, updated_at: 1 }] }

  it('lets an owner change roles and remove people, and says who the team’s protections keep', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true }), beaMember({ last_reader_of: ['acc_shared'] }), member(CAROL, { name: 'Carol' })], [shared], invites)
    const html = await render(OpenMembers)
    const words = text(html)
    expect(words).toContain('Your role: Owner')
    expect(words).toContain('You are the team’s only owner: make another member an owner before you leave or step down.')
    expect(words).toContain('The only person who can read suporte@example.test: give someone else Read before disabling or removing them.')
    expect(words).toContain('Only reader: 1')
    // The last owner keeps the role; others can be given any, the last reader too.
    expect(html).not.toContain(`name="role-${ana.id}"`)
    expect(html).toContain(`name="role-${CAROL}"`)
    expect(html).toContain(`name="role-${BEA}"`)
    // Bea alone reads a mailbox: she is not disabled or removed; Carol may be.
    const row = (userID: string) => html.match(new RegExp(`<li[^>]*data-user="${userID}"[^]*?</li>`))?.[0] ?? ''
    expect(text(row(BEA))).not.toMatch(/Remove…|Disable…/)
    expect(text(row(CAROL))).toContain('Remove…')
    expect(text(row(CAROL))).toContain('Disable…')
    expect(text(row(ana.id))).not.toContain('Leave the team…')
    // Pending invitations, with their role, never their link.
    expect(words).toContain('dan@example.test')
    expect(words).toContain('Admin · expires on')
    expect(words).toContain('Rename…')
    expect(words).toContain('Invite someone')
    expect(words).not.toMatch(/linked by|take.* over/i)
  })

  it('lets an owner leave while another owner remains', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner' }), beaMember({ role: 'owner' })], [], [])
    const html = await render(OpenMembers)
    const row = html.match(new RegExp(`<li[^>]*data-user="${ana.id}"[^]*?</li>`))?.[0] ?? ''
    expect(text(row)).toContain('Leave the team…')
  })

  it('lets an admin change and remove members only, see member invitations only, and never leave by herself', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([member(CAROL, { role: 'owner', last_owner: true, name: 'Carol' }), anaMember({ role: 'admin' }), beaMember()], [], [])
    const html = await render(OpenMembers)
    expect(html).not.toMatch(/name="role-/)
    const row = (userID: string) => html.match(new RegExp(`<li[^>]*data-user="${userID}"[^]*?</li>`))?.[0] ?? ''
    expect(text(row(BEA))).toContain('Remove…')
    expect(text(row(CAROL))).not.toContain('Remove…')
    expect(text(html)).not.toContain('Leave the team…')
    expect(text(html)).toContain('As an admin, you see and make invitations for members only.')
  })

  it('shows a member none of the team’s people, and says who manages them', async () => {
    showing(TEAM, [personal, support('member')])
    teamState([member(CAROL, { role: 'owner', last_owner: true, name: 'Carol' }), anaMember(), beaMember()])
    const html = await render(OpenMembers)
    expect(text(html)).toContain('The people of Support, and who can use each of its mailboxes, are managed by its owners and admins.')
    expect(html).not.toContain('data-user=')
    expect(text(html)).not.toMatch(/Leave the team…|Remove…|Disable…|Rename…|Invite someone|Pending invitations/)
  })

  it('offers creating a team in the personal workspace, and the teams the person is in', async () => {
    showing(PERSONAL, [personal, support('member')])
    const html = await render(OpenMembers)
    expect(text(html)).toContain('Your personal workspace')
    expect(text(html)).toContain('Create a team…')
    expect(text(html)).toContain('Support')
  })

  it('says who an invitation link works for before it is made: an owner of the server’s also creates an account', async () => {
    showing(TEAM, [personal, support('owner')])
    expect(text(await render(TeamInviteDialog))).toContain('Someone without one can create an account with it, because you are an owner of this server.')
    Object.assign(session, { user: { ...ana, role: 'member' } })
    const html = await render(TeamInviteDialog)
    expect(text(html)).toContain('Someone without one needs an invitation to this server first, from one of its owners.')
    // An owner of the team may invite any role.
    expect(html).toContain('value="owner"')
    showing(TEAM, [personal, support('admin')])
    const admin = await render(TeamInviteDialog)
    expect(admin).not.toContain('value="owner"')
    expect(text(admin)).toContain('As an admin, you invite members. Owners invite admins and owners.')
  })
})

describe('the workspace shown', () => {
  it('names the workspaces to choose from in the sidebar and the one shown in the header, and offers Members where people are changed here', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    showing(TEAM, [personal, support('admin')])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    const html = await render(OpenConsole)
    const select = html.match(/<select[^>]*name="workspace"[^]*?<\/select>/)?.[0] ?? ''
    expect(text(select)).toBe('Personal Support · Admin')
    expect(select).toMatch(new RegExp(`<option value="${TEAM}" selected`))
    const breadcrumb = (html.match(/<div class="console-breadcrumb"[^>]*>(.*?)<\/div>/)?.[1] ?? '').replace(/<[^>]+>/g, '').replace(/\s+/g, ' ')
    expect(breadcrumb).toBe('Console / Support / Mailboxes')
    const nav = html.match(/<nav class="console-nav"[^]*?<\/nav>/)?.[0] ?? ''
    expect(text(nav)).toBe('Mailboxes 0 Members API keys & MCP Storage Account')
  })

  it('offers a member of a team no Members, and describes its mailboxes by what each role does with them', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    const html = await render(OpenConsole)
    expect(text(html.match(/<nav class="console-nav"[^]*?<\/nav>/)?.[0] ?? '')).toBe('Mailboxes 0 API keys & MCP Storage Account')
    expect(text(html)).toContain('The mailboxes of Support you have access to')
    showing(TEAM, [personal, support('owner')])
    expect(text(await render(OpenConsole))).toContain('Every mailbox of Support: you manage them by your role, and read those you are given Read on.')
  })

  it('offers no Members for a team whose people are changed elsewhere, and no switcher with one workspace', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    showing(TEAM, [{ ...personal, source: 'platform' }, support('owner', { source: 'platform' })])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    expect(text((await render(OpenConsole)).match(/<nav class="console-nav"[^]*?<\/nav>/)?.[0] ?? '')).toBe('Mailboxes 0 API keys & MCP Storage Account')
    showing(PERSONAL, [personal])
    Object.assign(accounts, { list: [], loaded: true, workspace: PERSONAL })
    const alone = await render(OpenConsole)
    expect(alone).not.toContain('name="workspace"')
    expect(text(alone.match(/<nav class="console-nav"[^]*?<\/nav>/)?.[0] ?? '')).toBe('Mailboxes 0 Members API keys & MCP Storage Account')
  })

  it('says a workspace the person is no longer in went, and which one is shown now', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    showing(PERSONAL, [personal])
    Object.assign(workspaces, { lost: support('member') })
    Object.assign(accounts, { list: [], loaded: true, workspace: PERSONAL })
    expect(text(await render(OpenConsole))).toContain('You are no longer a member of Support. This page now shows Personal.')
  })

  it('shows the figures of the workspace shown, and each workspace’s when the answer has several', async () => {
    showing(TEAM, [personal, support('member')])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    Object.assign(storage, { workspace: TEAM, loaded: true, usage: {
      mailboxes: [{ account_id: 'acc_shared', workspace_id: TEAM, email: 'suporte@example.test', messages: 4, bytes: 4096 }],
      workspaces: [{ workspace_id: TEAM, mailboxes: 1, messages: 4, bytes: 4096 }],
      total: { messages: 4, bytes: 4096 },
    } })
    const html = await render(StoragePanel)
    expect(text(html)).toContain('In Support')
    expect(text(html)).toContain('Only the mailboxes you can read are counted: in a team, its other mailboxes are not.')
    expect(html).not.toContain('By workspace')
    storage.usage!.workspaces = [{ workspace_id: TEAM, mailboxes: 1, messages: 4, bytes: 4096 }, { workspace_id: PERSONAL, mailboxes: 2, messages: 10, bytes: 1000 }]
    const both = await render(StoragePanel)
    const list = both.match(/<ul class="storage-list"[^>]*aria-label="By workspace"[^]*?<\/ul>/)?.[0] ?? ''
    expect(text(list)).toContain('Support Mailboxes 1 Messages 4')
    expect(text(list)).toContain('Personal Mailboxes 2 Messages 10')
  })
})

describe('an invitation opened signed in', () => {
  it('asks to join, saying joining gives access to no mailbox', async () => {
    showing(PERSONAL, [personal])
    Object.assign(invitation, { pending: { invite: 'code', email: 'Ana@Example.test' } })
    const html = await render(InvitationDialog)
    expect(text(html)).toContain('This invitation is for Ana@Example.test.')
    expect(text(html)).toContain('Joining gives you access to no mailbox')
    expect(text(html)).toContain('Join the team')
    expect(html).not.toContain('>code<')
  })

  it('says an invitation for another address works only for it, and offers signing out instead of joining', async () => {
    showing(PERSONAL, [personal])
    Object.assign(invitation, { pending: { invite: 'code', email: 'bea@example.test' } })
    const html = await render(InvitationDialog)
    expect(text(html)).toContain('This invitation is for bea@example.test, and you are signed in as ana@example.test. It works only for its own address: sign out to use it.')
    expect(text(html)).not.toContain('Join the team')
    expect(text(html)).toContain('Sign out')
  })
})
