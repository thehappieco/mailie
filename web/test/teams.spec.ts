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
const member = (userID: string, fields: Partial<Member> = {}): Member => ({ user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, links: 0, joined_at: 1_790_000_000, ...fields })
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
  const readOnly = account({ id: 'acc_read', email: 'suporte@example.test', state: 'needs_reauth', auth_kind: 'oauth2', workspace_id: TEAM, linked_by: BEA, access: { ...none, read: true } })
  const cardOnly = account({ id: 'acc_card', email: 'vendas@example.test', state: 'active', workspace_id: TEAM, linked_by: BEA, access: { ...none, send: true } })

  it('shows on each card what the person may do with it and who linked it, and offers authorizing only to whoever manages it', async () => {
    showing(TEAM, [personal, support('member')])
    teamState([anaMember(), beaMember({ links: 2 })])
    Object.assign(accounts, { list: [readOnly, cardOnly], loaded: true, workspace: TEAM })
    const html = await render(AccountsPanel)
    expect(text(html)).toContain('Mailboxes in Support')
    expect(text(html)).toContain('Your access Read')
    expect(text(html)).toContain('Your access Send')
    expect(text(html)).toContain('Linked by Bea Lima')
    // Needs authorizing, and Ana does not manage it: nothing she could press would work, and nothing tells her to.
    expect(html).not.toContain('Finish authorization')
    expect(text(html)).toContain('The provider asks for this mailbox to be authorized again, by someone who manages it. Mail is not syncing.')
    expect(text(html)).toContain('You can see this mailbox, but not read it. Read access comes only from someone who has it and can change who has access.')
    // A member links nothing into the team; administered mailboxes are an owner's or an admin's list.
    expect(html).not.toContain('Other mailboxes of Support')
  })

  it('offers connecting a mailbox only where the person may link one: their personal workspace, or a team they own or administer', async () => {
    const mine = account({ id: 'acc_mine', email: 'ana@gmail.example', state: 'active', workspace_id: PERSONAL, linked_by: ana.id, access: full })
    // A member of Support: the button would only connect to her personal workspace, so it is not on the team's list.
    showing(TEAM, [personal, support('member')])
    teamState([anaMember(), beaMember({ links: 2 })])
    Object.assign(accounts, { list: [readOnly, cardOnly], loaded: true, workspace: TEAM })
    expect(text(await render(AccountsPanel))).not.toContain('Connect an email account')
    for (const role of ['owner', 'admin']) {
      showing(TEAM, [personal, support(role)])
      expect(text(await render(AccountsPanel)), role).toContain('Connect an email account')
    }
    showing(PERSONAL, [personal, support('member')])
    Object.assign(accounts, { list: [mine], loaded: true, workspace: PERSONAL })
    expect(text(await render(AccountsPanel))).toContain('Connect an email account')
  })

  it('lists for an owner or an admin the team’s mailboxes they hold nothing on, with who has access, never what they hold', async () => {
    showing(TEAM, [personal, support('admin')])
    const hidden: MailboxAccess = { account_id: 'acc_hidden', email: 'diretoria@example.test', provider: 'gmail', state: 'active', linked_by: BEA, grants: [{ account_id: 'acc_hidden', user_id: BEA, ...full, updated_at: 1 }] }
    teamState([anaMember({ role: 'admin' }), beaMember({ links: 1 })], [hidden])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    const html = await render(AccountsPanel)
    expect(text(html)).toContain('Other mailboxes of Support')
    expect(text(html)).toContain('diretoria@example.test')
    expect(text(html)).toContain('Your access No access')
    expect(text(html)).toContain('Access…')
    // An admin connects mailboxes into the team.
    expect(text(html)).toContain('Connect the first mailbox of Support')
  })

  it('lists none of the team’s mailboxes as held by nobody before the person’s own list of the team is in, nor one the directory says they hold', async () => {
    showing(TEAM, [personal, support('owner')])
    const held: MailboxAccess = { account_id: 'acc_held', email: 'suporte@example.test', provider: 'gmail', state: 'active', linked_by: ana.id, grants: [{ account_id: 'acc_held', user_id: ana.id, ...full, updated_at: 1 }] }
    const hidden: MailboxAccess = { account_id: 'acc_hidden', email: 'diretoria@example.test', provider: 'gmail', state: 'active', linked_by: BEA, grants: [{ account_id: 'acc_hidden', user_id: BEA, ...full, updated_at: 1 }] }
    teamState([anaMember({ role: 'owner', links: 1 }), beaMember({ links: 1 })], [held, hidden])
    // Still reading her list, or it failed: nothing is "No access" yet.
    Object.assign(accounts, { list: [], loaded: false, loading: true, workspace: '' })
    expect(text(await render(AccountsPanel))).not.toContain('Other mailboxes of Support')
    // Her list is in, without the mailbox she just removed, which the directory, not read again yet, still names with her grant.
    Object.assign(accounts, { list: [], loaded: true, loading: false, workspace: TEAM })
    const html = text(await render(AccountsPanel))
    expect(html).toContain('Other mailboxes of Support')
    expect(html).toContain('diretoria@example.test')
    expect(html).not.toContain('suporte@example.test')
    // Another workspace's list says nothing about this one.
    Object.assign(accounts, { workspace: PERSONAL })
    expect(text(await render(AccountsPanel))).not.toContain('Other mailboxes of Support')
  })

  it('tells a member of a team with nothing shared yet where their own mailboxes go, and offers to show it rather than connect one there', async () => {
    showing(TEAM, [personal, support('member')])
    teamState([anaMember()])
    Object.assign(accounts, { list: [], loaded: true, workspace: TEAM })
    const html = await render(AccountsPanel)
    expect(text(html)).toContain('Nothing shared with you here yet')
    expect(text(html)).toContain('A mailbox of Support appears here once you are given access to it. Only its owners and admins connect mailboxes to it; yours are connected in your personal workspace.')
    expect(text(html)).toContain('Show your personal workspace')
    expect(text(html)).not.toContain('Connect an email account')
    // Not a word saying that managing a mailbox is enough to pass its mail on.
    expect(text(html)).not.toMatch(/whoever manages|someone who manages/i)
  })

  it('shows a mailbox seen without read and without manage as such in its sheet: no folders, no removing', async () => {
    showing(TEAM, [personal, support('member')])
    teamState([anaMember(), beaMember({ links: 1 })])
    Object.assign(accounts, { list: [cardOnly], loaded: true, workspace: TEAM })
    const html = await render(AccountSheet, { account: cardOnly })
    expect(html).not.toContain('Show folders')
    expect(text(html)).toContain('You do not have read access to this mailbox, so its folders and messages are not shown to you.')
    expect(html).not.toContain('Remove account')
    expect(text(html)).toContain('Linked by Bea Lima')
    expect(text(html)).toContain('Access')
  })

  it('asks a manager of a team mailbox, before removing it, to know everyone in the team loses it', async () => {
    showing(TEAM, [personal, support('owner')])
    const managed = account({ id: 'acc_managed', state: 'active', workspace_id: TEAM, linked_by: ana.id, access: full, sync: syncing() })
    teamState([anaMember({ role: 'owner', last_owner: true, links: 1 })], [{ account_id: managed.id, email: managed.email, provider: 'gmail', state: 'active', linked_by: ana.id, grants: [{ account_id: managed.id, user_id: ana.id, ...full, updated_at: 1 }] }])
    const html = await render(AccountSheet, { account: managed })
    expect(text(html)).toContain('Remove account')
    expect(text(html)).toContain('Everyone in Support who has access to it loses it.')
    expect(text(html)).toContain('Sync now')
  })
})

describe('who can use a mailbox', () => {
  const entry = (grants: [string, Partial<typeof full>][]): MailboxAccess => ({
    account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', linked_by: BEA,
    grants: grants.map(([userID, flags]) => ({ account_id: 'acc_shared', user_id: userID, ...none, ...flags, updated_at: 1 })),
  })
  /** The checkbox of a person's flag, as rendered. */
  const box = (html: string, userID: string, flag: string) => html.match(new RegExp(`<input[^>]*name="${userID}-${flag}"[^>]*>`))?.[0] ?? ''

  it('lets an admin who holds nothing give Manage, and says what they cannot give', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember({ links: 1 }), member(CAROL, { name: 'Carol' })], [entry([[BEA, full]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('Each person needs their own access: being an owner or an admin of the team gives none.')
    expect(text(html)).toContain('You can give only what you hold on this mailbox yourself: not Read, Act, or Send.')
    expect(box(html, CAROL, 'manage')).not.toContain('disabled')
    for (const flag of ['read', 'act', 'send']) expect(box(html, CAROL, flag), flag).toContain('disabled')
    // Her own row too: an admin gives herself no read.
    expect(box(html, ana.id, 'read')).toContain('disabled')
  })

  it('keeps the grant of the person it is linked by as it is, and says why', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner' }), beaMember({ links: 1 })], [entry([[BEA, full], [ana.id, full]])])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('Linked by Bea Lima: it syncs under their agreement to sync.')
    expect(text(html)).toContain('Linked this mailbox: it syncs under their agreement, so their access stays complete while it is linked.')
    for (const flag of ['read', 'act', 'send', 'manage']) expect(box(html, BEA, flag), flag).toContain('disabled')
  })

  it('keeps Manage with the only person who holds it', async () => {
    showing(TEAM, [personal, support('owner')])
    // Linked by someone no longer listed: Carol is the only manager.
    teamState([anaMember({ role: 'owner' }), member(CAROL, { name: 'Carol' })], [{ ...entry([[CAROL, { read: true, manage: true }], [ana.id, { read: true }]]), linked_by: undefined }])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('The only person who manages this mailbox: give Manage to someone else before taking it away.')
    expect(box(html, CAROL, 'manage')).toContain('disabled')
    expect(box(html, CAROL, 'read')).not.toContain('disabled')
  })

  it('shows someone who only uses a mailbox their own access, which they may give up, and nobody else’s', async () => {
    showing(TEAM, [personal, support('member')])
    const shared = account({ id: 'acc_shared', workspace_id: TEAM, linked_by: BEA, state: 'active', access: { ...none, read: true, send: true } })
    teamState([anaMember(), beaMember({ links: 1 })])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: shared.email, account: shared })
    expect(html).toContain(`data-user="${ana.id}"`)
    expect(html).not.toContain(`data-user="${BEA}"`)
    expect(box(html, ana.id, 'read')).not.toContain('disabled')
    expect(box(html, ana.id, 'manage')).toContain('disabled')
    expect(html).not.toContain('Take over the link')
  })

  it('offers taking a link over to whoever may, and says beforehand why to anyone else who manages it', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember({ links: 1 })], [entry([[BEA, full], [ana.id, full]])])
    Object.assign(consent, { loaded: true, consented: true })
    const ready = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(ready)).toContain('It syncs under the agreement of Bea Lima: turning their sync off deletes its index, and they cannot leave the team while it is linked.')
    expect(ready.match(/<button[^>]*>Take over the link…<\/button>/)?.[0]).not.toContain('disabled')
    Object.assign(consent, { consented: false })
    const noSync = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(noSync)).toContain('Turn on mail sync first: the mailbox would sync under your agreement.')
    expect(noSync.match(/<button[^>]*>Take over the link…<\/button>/)?.[0]).toContain('disabled')
    // Agreed to an earlier text, which may not say who reads a team mailbox: it would sync under it.
    Object.assign(consent, { consented: true, version: '2026-01-older', currentVersion: SYNC_TEXT_VERSION })
    const earlier = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(earlier)).toContain('Agree to the current text of mail sync first: the mailbox would sync under your agreement, which was to an earlier text.')
    expect(text(earlier)).toContain('Read the current text…')
    expect(earlier.match(/<button[^>]*>Take over the link…<\/button>/)?.[0]).toContain('disabled')
    Object.assign(consent, { consented: true, version: '', currentVersion: '' })
    teamState([anaMember({ role: 'admin' }), beaMember({ links: 1 })], [entry([[BEA, full], [ana.id, { read: true, manage: true }]])])
    const partial = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(partial)).toContain('To take the link over you need Read, Act, Send and Manage on this mailbox.')
  })

  it('says so when a mailbox opened from the team’s list is no longer in it, and offers nothing for it', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([anaMember({ role: 'admin' }), beaMember({ links: 0 }), member(CAROL, { name: 'Carol' })], [])
    const html = await render(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' })
    expect(text(html)).toContain('suporte@example.test is no longer a mailbox of Support: it was removed.')
    expect(html).not.toContain(`data-user="${CAROL}"`)
    expect(html).not.toContain('Take over the link')
  })
})

describe('the people of a team', () => {
  const invites: TeamInvite[] = [{ id: 'inv_0000000000000002', email: 'dan@example.test', workspace_id: TEAM, role: 'admin', created_at: 1_790_000_000, expires_at: 1_790_604_800 }]

  it('lets an owner change roles and remove people, and says who the team’s protections keep', async () => {
    showing(TEAM, [personal, support('owner')])
    teamState([anaMember({ role: 'owner', last_owner: true }), beaMember({ links: 2 }), member(CAROL, { name: 'Carol' })], [], invites)
    const html = await render(OpenMembers)
    const words = text(html)
    expect(words).toContain('Your role: Owner')
    expect(words).toContain('You are the team’s only owner: make another member an owner before you leave or step down.')
    expect(words).toContain('Mailboxes here are linked by them (2): they stay in the team until those are removed or another member takes their links over.')
    // The last owner keeps the role; others can be given any.
    expect(html).not.toContain(`name="role-${ana.id}"`)
    expect(html).toContain(`name="role-${CAROL}"`)
    // Bea linked mailboxes there: she is not disabled or removed; Carol may be.
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
  })

  it('lets an admin change and remove members only, and see member invitations only', async () => {
    showing(TEAM, [personal, support('admin')])
    teamState([member(CAROL, { role: 'owner', last_owner: true, name: 'Carol' }), anaMember({ role: 'admin' }), beaMember()], [], [])
    const html = await render(OpenMembers)
    expect(html).not.toMatch(/name="role-/)
    const row = (userID: string) => html.match(new RegExp(`<li[^>]*data-user="${userID}"[^]*?</li>`))?.[0] ?? ''
    expect(text(row(BEA))).toContain('Remove…')
    expect(text(row(CAROL))).not.toContain('Remove…')
    expect(text(html)).toContain('As an admin, you see and make invitations for members only.')
  })

  it('lets a member only leave, and shows them no invitations', async () => {
    showing(TEAM, [personal, support('member')])
    teamState([member(CAROL, { role: 'owner', last_owner: true, name: 'Carol' }), anaMember(), beaMember()])
    const html = await render(OpenMembers)
    expect(text(html)).toContain('Leave the team…')
    expect(text(html)).not.toMatch(/Remove…|Disable…|Rename…|Invite someone|Pending invitations/)
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
