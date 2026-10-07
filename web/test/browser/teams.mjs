// A real browser over the open console's workspaces and teams (docs/workspaces.md),
// with every /v1 call answered by the core's fake daemon (fakeDaemon.mjs)
// and, through its extend hook, an in-memory model of workspaces, members,
// grants, each team mailbox's own agreement to sync, and team invitations.
// Optional QA, not part of `npm test`:
//
//   npm run build && npx vite preview --port 4174 &
//   QA_ORIGIN=http://localhost:4174 node test/browser/teams.mjs
//
// It walks the workspace switcher; a team's mailboxes as its owner sees them,
// every one a card she manages by her role, and one she does not read without
// its folders; a mailbox's access, ticked and saved, then taken away, with
// the last reader's Read kept and what she cannot give said beforehand; a
// team mailbox's sync, turned on for the team after its text, and an
// agreement the upgrade carried over confirmed; the team's people, their
// roles and invitations, the link shown once; creating a team; turning her
// own sync off, which reaches her personal workspace's mailboxes; an
// invitation opened signed in; and, signed in as a member, the team
// mailboxes she holds a grant on, seen without their folders, messages,
// access or sync switch, with a line saying who manages the team's people,
// and no Members. On desktop and phone, light and dark, then in Portuguese
// and German. It fails on a page error, a CSP violation, horizontal
// scrolling and any server message drawn on a screen.
//
// QA_ORIGIN, QA_SCREENSHOTS, QA_PLAYWRIGHT_MODULE, QA_BROWSER,
// QA_BROWSER_EXECUTABLE and QA_BROWSER_CHANNEL as for console.mjs.
import { mkdir, readdir, readFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'
import { PASSWORD, fakeDaemon as coreDaemon, noHorizontalOverflow, now, until } from './fakeDaemon.mjs'

const playwright = await import(process.env.QA_PLAYWRIGHT_MODULE || 'playwright')
const engine = process.env.QA_BROWSER || 'chromium'
const origin = (process.env.QA_ORIGIN || 'http://localhost:5174').replace(/\/$/, '')
const screenshots = process.env.QA_SCREENSHOTS ? resolve(process.env.QA_SCREENSHOTS) : ''
const browser = await playwright[engine].launch({
  headless: true,
  ...(process.env.QA_BROWSER_EXECUTABLE ? { executablePath: process.env.QA_BROWSER_EXECUTABLE } : {}),
  ...(process.env.QA_BROWSER_CHANNEL ? { channel: process.env.QA_BROWSER_CHANNEL } : {}),
})
if (screenshots) await mkdir(screenshots, { recursive: true })

/** The open console's revisions, the daemon's defaults (src/open/versions.ts). */
const VERSIONS = { sync: '2026-10-open-sync-3', actions: '2026-10-open-actions-2', keys: '2026-10-open-api-keys' }
const ANA = 'usr_00000000000000a1'
const BEA = 'usr_00000000000000c1'
const CAROL = 'usr_00000000000000c2'
const PERSONAL = { [ANA]: 'wsp_00000000000000a1', [BEA]: 'wsp_00000000000000c1', [CAROL]: 'wsp_00000000000000c2' }
const TEAM = 'wsp_000000000000f001'
const OTHER_TEAM = 'wsp_000000000000f002'
/** The invitation into the other team waiting for Ana, which she accepts signed in. */
const TEAM_INVITE = 'SyntheticTeamInvite_0123456789abcdefghijklmn'
const SUPPORT = 'acc_000000000000f001'
const SALES = 'acc_000000000000f002'
const FINANCE = 'acc_000000000000f003'
const none = () => ({ read: false, act: false, send: false, manage: false })
const flags = fields => ({ ...none(), ...fields })

/**
 * The console's catalogs, each key with its translations in the order
 * src/ui/i18n.ts reads them: what a pass in another language expects, said
 * the way the console says it.
 */
const LANGUAGES = { 'pt-BR': 0, 'es-ES': 1, 'fr-FR': 2, 'de-DE': 3 }
const localeDir = resolve(fileURLToPath(import.meta.url), '../../../src/ui/locales')
const catalog = Object.assign({}, ...await Promise.all((await readdir(localeDir)).filter(name => name.endsWith('.json'))
  .map(async name => JSON.parse(await readFile(resolve(localeDir, name), 'utf8')))))
function say(language, key, values = {}) {
  const index = LANGUAGES[language]
  if (index !== undefined && !catalog[key]) throw new Error(`teams.mjs: no translation of ${key}`)
  // A key may carry a context after "|", which picks the translation and is never shown.
  const text = index === undefined ? key.split('|')[0] : catalog[key][index]
  return text.replace(/\{(\w+)\}/g, (_, name) => String(values[name]))
}

/**
 * The core's fake daemon with workspaces: Ana's three mailboxes in her
 * personal workspace; the team Atendimento, which Ana owns, Bea administers
 * and Carol is a member of, with three mailboxes of the team's: one Bea
 * linked and synced for the team (Bea reads it, Ana reads and sends from it,
 * Carol only sends), one Ana linked with sync off (only Ana reads it), and
 * one only Bea reads, under the agreement the upgrade carried over from her
 * (Carol manages it, Ana holds nothing on it); and the team Financeiro,
 * whose invitation waits for Ana.
 */
function daemon() {
  let team = null
  const instance = coreDaemon({
    origin, versions: VERSIONS, consented: true,
    extend: core => {
      team = teamModel(core)
      return { methods: { team }, route: request => team.route(request) }
    },
  })
  return instance
}

function teamModel({ users, accountsByUser, present, calls }) {
  const at = now() - 86400 * 20
  users.set('bea@example.test', { id: BEA, email: 'bea@example.test', name: 'Bea Lima', role: 'member', created_at: at, password: PASSWORD, consent: 0 })
  users.set('carol@example.test', { id: CAROL, email: 'carol@example.test', name: 'Carol Dias', role: 'member', created_at: at, password: PASSWORD, consent: 0 })
  const workspaces = new Map([
    ...Object.values(PERSONAL).map(id => [id, { id, kind: 'personal', source: 'local', name: '', created_at: at }]),
    [TEAM, { id: TEAM, kind: 'team', source: 'local', name: 'Atendimento', created_at: at }],
    [OTHER_TEAM, { id: OTHER_TEAM, kind: 'team', source: 'local', name: 'Financeiro', created_at: at }],
  ])
  const members = new Map([
    ...Object.entries(PERSONAL).map(([user, id]) => [id, [{ user_id: user, role: 'owner', status: 'active', joined_at: at }]]),
    [TEAM, [{ user_id: ANA, role: 'owner', status: 'active', joined_at: at }, { user_id: BEA, role: 'admin', status: 'active', joined_at: at }, { user_id: CAROL, role: 'member', status: 'active', joined_at: at }]],
    [OTHER_TEAM, [{ user_id: BEA, role: 'owner', status: 'active', joined_at: at }]],
  ])
  const invites = [{ id: 'inv_0000000000000009', code: TEAM_INVITE, email: 'ana@example.test', workspace_id: OTHER_TEAM, role: 'member', created_by: BEA, created_at: at, expires_at: now() + 86400 * 6 }]
  let inviteSerial = 10
  let workspaceSerial = 3
  let accountSerial = 100
  // A person's mailboxes are the core's, kept by their person; a team's are the team's, kept here.
  for (const account of accountsByUser.get(ANA)) account.workspace_id = PERSONAL[ANA]
  accountsByUser.set(BEA, [])
  accountsByUser.set(CAROL, [])
  const teamMailbox = (id, email, workspaceID, linker, sync) => ({ id, email, provider: 'imap', auth_kind: 'password', state: 'active', save_sent_copy: true, created_at: at, workspace_id: workspaceID, linked_by: linker, consent: sync, target: 400 })
  const teamAccounts = [
    teamMailbox(SUPPORT, 'suporte@atendimento.example', TEAM, BEA, { enabled_at: at, enabled_by: BEA, version: VERSIONS.sync }),
    teamMailbox(SALES, 'vendas@atendimento.example', TEAM, ANA, null),
    teamMailbox(FINANCE, 'financeiro@atendimento.example', TEAM, BEA, { enabled_at: at - 86400, enabled_by: BEA, version: '2026-10-open-sync-2', migrated: true }),
  ]
  const grants = new Map([
    [SUPPORT, new Map([[BEA, flags({ read: true, act: true, send: true })], [ANA, flags({ read: true, send: true })], [CAROL, flags({ send: true })]])],
    [SALES, new Map([[ANA, flags({ read: true, act: true, send: true })]])],
    [FINANCE, new Map([[BEA, flags({ read: true, act: true, send: true })], [CAROL, flags({ manage: true })]])],
  ])
  calls.access = []
  calls.members = []
  calls.invites = []
  calls.teamSync = []
  calls.teamRemoved = []
  /** Every folder listing asked of a team mailbox, by whom. */
  calls.folders = []
  const membership = (workspaceID, userID) => (members.get(workspaceID) ?? []).find(item => item.user_id === userID && item.status === 'active')
  const managesByRole = (workspaceID, userID) => ['owner', 'admin'].includes(membership(workspaceID, userID)?.role)
  const stored = (accountID, userID) => grants.get(accountID)?.get(userID) ?? none()
  /** What a person holds on a team mailbox: their grant, and manage by an owner's or an admin's role. */
  const grantOf = (account, userID) => {
    if (!membership(account.workspace_id, userID)) return none()
    const held = stored(account.id, userID)
    return { ...held, manage: held.manage || managesByRole(account.workspace_id, userID) }
  }
  const any = held => held.read || held.act || held.send || held.manage
  const readersOf = account => [...(grants.get(account.id) ?? new Map())].filter(([userID, held]) => held.read && membership(account.workspace_id, userID)).map(([userID]) => userID)
  /** Sync runs under the team's agreement, while someone can read the mailbox. */
  const syncing = account => !!account.consent?.enabled_at && readersOf(account).length > 0
  const teamOf = id => teamAccounts.find(account => account.id === id)
  const visibleTeam = (user, workspaceID) => teamAccounts.filter(account => (!workspaceID || account.workspace_id === workspaceID) && any(grantOf(account, user.id)))
  const presentTeam = (account, user) => {
    const held = grantOf(account, user.id)
    const { consent: _, linked_by: __, target, ...rest } = account
    const on = syncing(account) && account.state === 'active'
    const sync = { enabled: syncing(account), running: on, state: on ? 'live' : 'off', folders_synced: on ? 5 : 0, folders_total: on ? 5 : 0, messages: on ? target : 0, initial_progress: on ? 100 : 0, ...(on ? { tier: 'condstore', last_synced_at: now() - 300 } : {}) }
    return { ...rest, sync, actions: { archive: false, trash: true }, access: held, send: held.send ? { available: true, from_name: user.name } : { available: false, reason: 'not_granted' } }
  }
  const presentOwn = (account, user) => ({ ...present(account), workspace_id: PERSONAL[user.id], access: flags({ read: true, act: true, send: true, manage: true }) })
  const listWorkspace = (workspace, user) => {
    const mine = (members.get(workspace.id) ?? []).find(item => item.user_id === user.id)
    return { id: workspace.id, kind: workspace.kind, source: workspace.source, name: workspace.name, role: mine.role, status: mine.status, created_at: workspace.created_at }
  }
  const presentMember = (workspaceID, item) => {
    const person = [...users.values()].find(user => user.id === item.user_id)
    const owners = (members.get(workspaceID) ?? []).filter(other => other.role === 'owner' && other.status === 'active')
    const lastReaderOf = teamAccounts.filter(account => account.workspace_id === workspaceID && item.status === 'active')
      .filter(account => { const readers = readersOf(account); return readers.length === 1 && readers[0] === item.user_id }).map(account => account.id)
    return { user_id: item.user_id, email: person.email, name: person.name, role: item.role, status: item.status, last_owner: item.role === 'owner' && item.status === 'active' && owners.length === 1, last_reader_of: lastReaderOf, joined_at: item.joined_at }
  }
  const presentConsent = account => {
    const consent = account.consent
    if (!consent) return { enabled: false, current: false }
    return { enabled: !!consent.enabled_at, ...(consent.enabled_at ? { enabled_at: consent.enabled_at } : {}), ...(consent.enabled_by ? { enabled_by: consent.enabled_by } : {}), ...(consent.version ? { version: consent.version } : {}), ...(consent.migrated ? { migrated: true } : {}), current: consent.version === VERSIONS.sync }
  }
  /** The invitations a person made in a team stop working when their role, status or place there changes. */
  const endInvites = (workspaceID, userID) => { for (const invite of invites) if (invite.workspace_id === workspaceID && invite.created_by === userID) invite.used = true }
  /** Who would be left reading each mailbox of a team without this person: a mailbox someone reads keeps a reader. */
  const lastReaderAnywhere = (workspaceID, userID) => teamAccounts.some(account => account.workspace_id === workspaceID && readersOf(account).length === 1 && readersOf(account)[0] === userID)
  return {
    grants, members, invites, workspaces, teamAccounts,
    route({ path, method, json, fail, body, user, url, route }) {
      const query = url.searchParams
      if (path === '/v1/workspaces' && method === 'GET') {
        return json([...workspaces.values()].filter(workspace => membership(workspace.id, user.id)).map(workspace => listWorkspace(workspace, user)))
      }
      if (path === '/v1/workspaces' && method === 'POST') {
        const name = String(body().name ?? '').trim()
        if (!name || name.length > 80) return fail(400, 'bad_request')
        const id = `wsp_000000000000f${String(++workspaceSerial).padStart(3, '0')}`
        workspaces.set(id, { id, kind: 'team', source: 'local', name, created_at: now() })
        members.set(id, [{ user_id: user.id, role: 'owner', status: 'active', joined_at: now() }])
        return json(listWorkspace(workspaces.get(id), user), 201)
      }
      if (path === '/v1/auth/invites/accept' && method === 'POST') {
        const invite = invites.find(item => item.code === body().invite && !item.used && item.email === user.email)
        if (!invite) return fail(403, 'not_authorized')
        if (membership(invite.workspace_id, user.id)) return fail(409, 'conflict')
        invite.used = true
        members.get(invite.workspace_id).push({ user_id: user.id, role: invite.role, status: 'active', joined_at: now() })
        return json(listWorkspace(workspaces.get(invite.workspace_id), user))
      }
      const workspaceMatch = path.match(/^\/v1\/workspaces\/([^/]+)(?:\/(members|invites|access)(?:\/([^/]+))?)?$/)
      if (workspaceMatch) {
        const [, id, part, item] = workspaceMatch
        const me = membership(id, user.id)
        const workspace = workspaces.get(id)
        if (!me || !workspace) return fail(404, 'not_found')
        const admin = me.role === 'owner' || me.role === 'admin'
        if (!part && method === 'PATCH') {
          if (!admin) return fail(403, 'not_authorized')
          workspace.name = String(body().name ?? '').trim()
          return json(listWorkspace(workspace, user))
        }
        // A team's people, its access directory and its invitations are its owners' and admins'.
        if (part && !admin) return fail(403, 'not_authorized')
        if (part === 'members' && !item) return json(members.get(id).map(entry => presentMember(id, entry)))
        if (part === 'members' && item) {
          const list = members.get(id)
          const target = list.find(entry => entry.user_id === item)
          if (!target) return fail(404, 'not_found')
          const shown = presentMember(id, target)
          calls.members.push({ method, user: item, body: method === 'PATCH' ? body() : undefined })
          if (method === 'PATCH') {
            const change = body()
            if (item === user.id || (me.role !== 'owner' && (target.role !== 'member' || (change.role && change.role !== 'member')))) return fail(403, 'not_authorized')
            if (shown.last_owner && ((change.role && change.role !== 'owner') || change.status === 'disabled')) return fail(409, 'conflict')
            if (change.status === 'disabled' && lastReaderAnywhere(id, item)) return fail(409, 'conflict')
            if ((change.role && change.role !== target.role) || (change.status && change.status !== target.status)) endInvites(id, item)
            Object.assign(target, change)
            // Owners and admins manage by their role: Manage is stored for members only.
            if (target.role !== 'member') for (const map of grants.values()) { const held = map.get(item); if (held) map.set(item, { ...held, manage: false }) }
            if (target.status === 'disabled') for (const map of grants.values()) map.delete(item)
            return json(presentMember(id, target))
          }
          if (method === 'DELETE') {
            if (item === user.id ? me.role !== 'owner' : me.role !== 'owner' && !(me.role === 'admin' && target.role === 'member')) return fail(403, 'not_authorized')
            if (shown.last_owner || lastReaderAnywhere(id, item)) return fail(409, 'conflict')
            endInvites(id, item)
            for (const map of grants.values()) map.delete(item)
            list.splice(list.indexOf(target), 1)
            return route.fulfill({ status: 204 })
          }
        }
        if (part === 'invites') {
          const visibleInvite = invite => !invite.used && invite.workspace_id === id && (me.role === 'owner' || invite.role === 'member')
          const shown = ({ code: _, used: __, ...invite }) => invite
          if (!item && method === 'GET') return json(invites.filter(visibleInvite).map(shown))
          if (!item && method === 'POST') {
            const { email, role = 'member' } = body()
            calls.invites.push({ email, role })
            if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(String(email))) return fail(400, 'bad_request')
            if (me.role === 'admin' && role !== 'member') return fail(403, 'not_authorized')
            const code = `SyntheticTeamInvite_${String(++inviteSerial).padStart(24, '0')}`
            const invite = { id: `inv_${String(inviteSerial).padStart(16, '0')}`, code, email, workspace_id: id, role, created_by: user.id, created_at: now(), expires_at: now() + 7 * 86400 }
            invites.push(invite)
            return json({ ...shown(invite), url: `${origin}/#invite=${code}&email=${encodeURIComponent(email)}` }, 201)
          }
          if (item && method === 'DELETE') {
            const invite = invites.find(entry => entry.id === item && visibleInvite(entry))
            if (!invite) return fail(404, 'not_found')
            invites.splice(invites.indexOf(invite), 1)
            return route.fulfill({ status: 204 })
          }
        }
        if (part === 'access' && method === 'GET') {
          return json(teamAccounts.filter(account => account.workspace_id === id).map(account => {
            const readers = readersOf(account).length
            return {
              account_id: account.id, email: account.email, provider: account.provider, state: account.state, linked_by: account.linked_by,
              readers, no_reader: readers === 0, sync: presentConsent(account),
              grants: [...(grants.get(account.id) ?? new Map()).entries()].map(([userID, held]) => ({ account_id: account.id, user_id: userID, ...held, granted_by: account.linked_by, updated_at: now() })),
            }
          }))
        }
        return fail(404, 'not_found')
      }
      const accessMatch = path.match(/^\/v1\/accounts\/([^/]+)\/access\/([^/]+)$/)
      if (accessMatch) {
        const [, accountID, target] = accessMatch
        const account = teamOf(accountID)
        if (!account || !any(grantOf(account, user.id))) return fail(404, 'not_found')
        calls.access.push({ method, account: accountID, user: target, body: method === 'PUT' ? body() : undefined, flags: query.get('flags') ?? '' })
        // Only owners and admins change who holds what.
        if (!managesByRole(account.workspace_id, user.id)) return fail(403, 'not_authorized')
        if (!membership(account.workspace_id, target)) return fail(404, 'not_found')
        const before = stored(accountID, target)
        const map = grants.get(accountID) ?? new Map()
        grants.set(accountID, map)
        const keepsReader = after => !(before.read && !after.read && readersOf(account).length === 1)
        if (method === 'PUT') {
          const after = body()
          // Read passes only from someone who reads it now; act only to someone who reads it.
          if (after.read && !before.read && !stored(accountID, user.id).read) return fail(403, 'not_authorized')
          if (after.act && !after.read) return fail(400, 'bad_request')
          if (after.manage && managesByRole(account.workspace_id, target)) return fail(400, 'bad_request')
          if (!keepsReader(after)) return fail(409, 'conflict')
          map.set(target, { read: !!after.read, act: !!after.act, send: !!after.send, manage: !!after.manage })
          return json({ account_id: accountID, user_id: target, ...map.get(target), granted_by: user.id, updated_at: now() })
        }
        if (method === 'DELETE') {
          const drop = (query.get('flags') || 'read,act,send,manage').split(',')
          const next = { ...before }
          for (const flag of drop) next[flag] = false
          if (drop.includes('read')) next.act = false
          if (!keepsReader(next)) return fail(409, 'conflict')
          if (any(next)) map.set(target, next)
          else map.delete(target)
          return route.fulfill({ status: 204 })
        }
      }
      // A team mailbox's own agreement to sync: its owners' and admins', to the current text.
      const syncMatch = path.match(/^\/v1\/accounts\/([^/]+)\/sync$/)
      if (syncMatch && teamOf(syncMatch[1])) {
        const account = teamOf(syncMatch[1])
        const held = grantOf(account, user.id)
        if (!any(held)) return fail(404, 'not_found')
        if (method === 'PUT') {
          const asked = body()
          calls.teamSync.push({ account: account.id, user: user.id, body: asked })
          if (!managesByRole(account.workspace_id, user.id)) return fail(403, 'not_authorized')
          if (asked.enabled && asked.version !== VERSIONS.sync) return fail(400, 'bad_request')
          // Nobody could read what it stores, nor be given Read on it: it is turned off, never on.
          if (asked.enabled && readersOf(account).length === 0) return fail(409, 'conflict')
          account.consent = asked.enabled ? { enabled_at: now(), enabled_by: user.id, version: asked.version } : null
          return json(presentTeam(account, user).sync)
        }
        if (method === 'GET') return json(presentTeam(account, user).sync)
        if (method === 'POST') return held.read && syncing(account) ? json(presentTeam(account, user).sync, 202) : fail(409, 'conflict')
      }
      if (path === '/v1/accounts' && method === 'GET') {
        const where = query.get('workspace') ?? ''
        const own = !where || where === PERSONAL[user.id] ? (accountsByUser.get(user.id) ?? []).map(account => presentOwn(account, user)) : []
        return json([...own, ...visibleTeam(user, where).map(account => presentTeam(account, user))])
      }
      if (path === '/v1/accounts' && method === 'POST' && body().workspace_id && body().workspace_id !== PERSONAL[user.id]) {
        const request = body()
        const where = request.workspace_id
        const me = membership(where, user.id)
        if (!me) return fail(404, 'not_found')
        if (!['owner', 'admin'].includes(me.role)) return fail(403, 'not_authorized')
        if ('sync_consent_version' in request && request.sync_consent_version !== VERSIONS.sync) return fail(400, 'bad_request')
        const id = `acc_000000000000f${String(++accountSerial)}`
        const account = teamMailbox(id, request.email, where, user.id, request.sync_consent_version ? { enabled_at: now(), enabled_by: user.id, version: request.sync_consent_version } : null)
        teamAccounts.push(account)
        // Whoever links it reads, acts on and sends from it.
        grants.set(id, new Map([[user.id, flags({ read: true, act: true, send: true })]]))
        return json({ account: presentTeam(account, user) }, 201)
      }
      // A team mailbox: its card, its folders, and removing it.
      const one = path.match(/^\/v1\/accounts\/([^/]+)(\/folders)?$/)
      if (one && teamOf(one[1])) {
        const account = teamOf(one[1])
        const held = grantOf(account, user.id)
        if (!any(held)) return fail(404, 'not_found')
        if (!one[2] && method === 'DELETE') {
          calls.teamRemoved.push({ account: account.id, confirm: query.get('confirm') ?? '' })
          if (!managesByRole(account.workspace_id, user.id)) return fail(403, 'not_authorized')
          if (query.get('confirm') !== account.id) return fail(400, 'bad_request')
          teamAccounts.splice(teamAccounts.indexOf(account), 1)
          grants.delete(account.id)
          return route.fulfill({ status: 204 })
        }
        if (!one[2]) return json(presentTeam(account, user))
        calls.folders.push({ user: user.id, account: account.id })
        if (!held.read) return fail(403, 'not_authorized')
        return json([{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 40 }])
      }
      if (path === '/v1/me/storage' && method === 'GET') {
        const where = query.get('workspace') ?? ''
        const own = !where || where === PERSONAL[user.id] ? (accountsByUser.get(user.id) ?? []).map(account => ({ account, workspace: PERSONAL[user.id], messages: account.sync.messages })) : []
        const team = visibleTeam(user, where).filter(account => grantOf(account, user.id).read).map(account => ({ account, workspace: account.workspace_id, messages: syncing(account) ? account.target : 0 }))
        const mailboxes = [...own, ...team].map(({ account, workspace, messages }) => ({ account_id: account.id, workspace_id: workspace, email: account.email, messages, bytes: messages * 3200 }))
          .sort((a, b) => a.email < b.email ? -1 : 1)
        const sums = new Map()
        for (const item of mailboxes) {
          const sum = sums.get(item.workspace_id) ?? { workspace_id: item.workspace_id, mailboxes: 0, messages: 0, bytes: 0 }
          sums.set(item.workspace_id, { ...sum, mailboxes: sum.mailboxes + 1, messages: sum.messages + item.messages, bytes: sum.bytes + item.bytes })
        }
        const total = mailboxes.reduce((sum, item) => ({ messages: sum.messages + item.messages, bytes: sum.bytes + item.bytes }), { messages: 0, bytes: 0 })
        return json({ mailboxes, workspaces: [...sums.values()], total, ...(user.role === 'owner' ? { database_bytes: 48_234_496 } : {}) })
      }
      return undefined
    },
  }
}

/** The words each pass expects that are not the catalogs' alone, in its language. */
const WORDS = {
  'en-US': { personal: 'Personal', owner: 'Owner', member: 'Member', signOut: 'Sign out', menu: 'Open menu' },
  'pt-BR': { personal: 'Pessoal', owner: 'Proprietário', member: 'Membro', signOut: 'Sair', menu: 'Abrir menu' },
  'de-DE': { personal: 'Persönlich', owner: 'Inhaber', member: 'Mitglied', signOut: 'Abmelden', menu: 'Menü öffnen' },
}

const failures = []
const passes = [
  { language: 'en-US', mobile: false, scheme: 'light', full: true },
  { language: 'en-US', mobile: true, scheme: 'dark', full: true },
  { language: 'en-US', mobile: false, scheme: 'dark', full: false },
  { language: 'en-US', mobile: true, scheme: 'light', full: false },
  { language: 'pt-BR', mobile: false, scheme: 'light', full: false },
  { language: 'de-DE', mobile: true, scheme: 'light', full: false },
]
for (const { language, mobile, scheme, full: everything } of passes) {
  const words = WORDS[language]
  const tr = (key, values) => say(language, key, values)
  const team = 'Atendimento'
  const label = `${language}-${mobile ? 'mobile' : 'desktop'}-${scheme}`
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: language, colorScheme: scheme, reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
    permissions: ['clipboard-read', 'clipboard-write'],
  })
  const fake = daemon()
  const errors = []
  await context.route('**/v1/**', route => fake.handle(route))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    assert.equal(await page.getByText('detail the console must never show').count(), 0, `${label} ${name}: no server message drawn`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `teams-${name}-${label}.png`), fullPage: !mobile })
  }
  const openMenu = () => page.getByRole('button', { name: words.menu, exact: true }).click()
  const navItems = async () => {
    if (mobile) await openMenu()
    const items = (await page.locator('.console-nav:visible .console-nav-item').allInnerTexts()).map(item => item.replace(/\s+\d+$/, '').trim())
    if (mobile) await page.keyboard.press('Escape')
    return items
  }
  const openSection = async name => {
    if (mobile) await openMenu()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  const chooseWorkspace = async id => {
    if (mobile) {
      await openMenu()
      await page.locator('.mobile-navigation select[name=workspace]').selectOption(id)
      await page.keyboard.press('Escape')
    } else {
      await page.locator('.console-sidebar select[name=workspace]').selectOption(id)
    }
  }
  const plain = value => value.replace(/\s+/g, ' ').trim()
  const card = email => page.locator('.account-card').filter({ hasText: email })
  const sheet = page.locator('dialog.sheet-backdrop[open]')
  const openSheet = async email => {
    await card(email).getByRole('button').first().click()
    await sheet.waitFor()
  }
  const closeSheet = async () => {
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })
  }
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()

    // --- the personal workspace, and the switcher ---------------------------
    if (mobile) await openMenu()
    const select = page.locator('select[name=workspace]:visible')
    assert.deepEqual((await select.locator('option').allTextContents()).map(item => item.trim()), [words.personal, `${team} · ${words.owner}`], 'the switcher lists the person’s workspaces, the team with their role')
    if (mobile) await page.keyboard.press('Escape')
    assert.equal(await page.locator('.account-card').count(), 3, 'the personal workspace lists Ana’s own mailboxes')
    await shot('personal')

    // --- a team's mailboxes, as its owner sees them -----------------------------
    await chooseWorkspace(TEAM)
    await page.getByRole('heading', { name: tr('Mailboxes in {team}', { team }) }).waitFor()
    await card('financeiro@atendimento.example').waitFor()
    assert.equal(await page.locator('.account-card').count(), 3, 'an owner sees every mailbox of the team as a card')
    assert.equal(await page.locator('.other-card').count(), 0, 'no second list of mailboxes held by nobody')
    assert.ok(plain(await card('financeiro@atendimento.example').innerText()).includes(`${tr('Your access')} ${tr('Manage')}`), 'the card she holds nothing on says she manages it')
    await card('financeiro@atendimento.example').getByText(tr('You manage this mailbox by your role, but do not read it. Read comes only from an owner or an admin who reads it.')).waitFor()
    assert.doesNotMatch(await card('suporte@atendimento.example').innerText(), new RegExp(tr('Linked by')), 'a card names no linker')
    assert.match(await page.locator('.console-breadcrumb').innerText(), /Atendimento/, 'the header names the workspace shown')
    await shot('team-mailboxes')

    // --- a mailbox only she reads: its access, Read kept with its last reader ------
    await openSheet('vendas@atendimento.example')
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.equal(await sheet.locator(`input[name="${ANA}-read"]`).isDisabled(), true, 'the only person who reads it keeps Read')
    await sheet.locator(`li[data-user="${ANA}"]`).getByText(tr('You are the only person who can read this mailbox: give someone else Read before you give yours up.')).waitFor()
    assert.equal(await sheet.locator(`input[name="${BEA}-manage"]`).isChecked(), true, 'an admin manages it by her role')
    assert.equal(await sheet.locator(`input[name="${BEA}-manage"]`).isDisabled(), true, 'and Manage is not given to her')
    if (everything) {
      await sheet.locator(`input[name="${CAROL}-act"]`).check()
      assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isChecked(), true, 'act ticks read')
      await shot('access-ticked')
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).click()
      await until(() => fake.calls.access.length === 1, 'the grant to be saved')
      assert.deepEqual(fake.calls.access[0].body, { read: true, act: true, send: false, manage: false })
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).waitFor({ state: 'detached' })
      // Two read it now: Ana may give hers up.
      await until(async () => !(await sheet.locator(`input[name="${ANA}-read"]`).isDisabled()), 'Ana’s Read to be hers to give up')
      await sheet.locator(`input[name="${CAROL}-send"]`).check()
      await sheet.locator(`input[name="${CAROL}-act"]`).uncheck()
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).click()
      await until(() => fake.calls.access.length === 2, 'the second change to be saved')
      assert.deepEqual(fake.calls.access[1].body, { read: true, act: false, send: true, manage: false }, 'a change that also adds sets the whole grant')
      // Taking away only: the flags taken are named, and the rest stays.
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).waitFor({ state: 'detached' })
      await sheet.locator(`input[name="${CAROL}-send"]`).uncheck()
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).click()
      await until(() => fake.calls.access.length === 3, 'the revocation to be saved')
      assert.deepEqual([fake.calls.access[2].method, fake.calls.access[2].flags], ['DELETE', 'send'], 'a change that only takes away names the flags taken')
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: tr('Save access') }).waitFor({ state: 'detached' })
      assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isChecked(), true, 'Read stays')
      assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isChecked(), false, 'Send went')

      // Its sync, off: turned on for the team after the text, to its revision.
      await sheet.locator('.team-sync button[role=switch]').click()
      const on = page.getByRole('dialog', { name: tr('Turn on sync for {team}?', { team }) })
      await on.getByText('Who can read the index', { exact: false }).first().waitFor()
      await shot('team-sync-on')
      await on.getByRole('button', { name: tr('Turn on sync for {team}', { team }) }).click()
      await on.waitFor({ state: 'detached' })
      assert.deepEqual(fake.calls.teamSync.map(call => call.body), [{ enabled: true, version: VERSIONS.sync }], 'the team’s agreement names the revision shown')
      await until(async () => /Ana Souza/.test(await sheet.locator('.team-sync').innerText()), 'who turned it on to be on the record')
    }
    await sheet.locator('.team-sync').scrollIntoViewIfNeeded()
    await shot('team-sync')
    await sheet.locator(`li[data-user="${CAROL}"]`).scrollIntoViewIfNeeded()
    await shot('access')
    await closeSheet()

    // A mailbox Bea linked and synced for the team: Ana reads it, and turns on Act and Send for anyone.
    await openSheet('suporte@atendimento.example')
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.equal(await sheet.locator(`input[name="${CAROL}-act"]`).isDisabled(), false, 'an owner who reads it turns on Act, with the Read it needs')
    assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isDisabled(), false, 'she gives the Read she holds')
    assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isChecked(), true, 'Carol’s Send is shown')
    assert.match(await sheet.locator('.facts').first().innerText(), /Bea Lima/, 'who connected it is on the record, for its owners and admins')
    assert.match(await sheet.locator('.team-sync').innerText(), /Bea Lima/, 'who turned its sync on for the team')
    assert.equal(await sheet.locator('.team-sync button[role=switch]').getAttribute('aria-checked'), 'true')
    await sheet.locator('.access-panel').scrollIntoViewIfNeeded()
    await shot('access-shared')
    await closeSheet()

    // A mailbox Ana holds nothing on: its card, access and sync, never its folders; and no Read is hers to give.
    await openSheet('financeiro@atendimento.example')
    await sheet.getByText(tr('You manage this mailbox as an owner or an admin of {team}, but do not read it, so its folders and messages are not shown to you. Read comes only from an owner or an admin who reads it.', { team })).waitFor()
    assert.equal(await sheet.getByRole('button', { name: tr('Show folders') }).count(), 0, 'no folders without Read')
    assert.equal(await sheet.getByRole('button', { name: tr('Sync now') }).count(), 0, 'no sync pass without Read')
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isDisabled(), true, 'no Read from someone who does not read it')
    assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isDisabled(), false, 'Send, from any owner or admin')
    assert.equal(await sheet.locator(`input[name="${CAROL}-manage"]`).isChecked(), true, 'Carol manages it')
    assert.equal(await sheet.locator(`input[name="${BEA}-read"]`).isDisabled(), true, 'Bea is the only one who reads it, and keeps Read')
    await sheet.getByText(tr('You do not read this mailbox, so you cannot give Read on it, not even to yourself: only an owner or an admin who reads it can.')).waitFor()
    // The agreement the upgrade carried over from Bea, still tied to her: confirmed for the team, or not.
    const confirmLabel = tr('Confirm for {team}…', { team })
    await sheet.locator('.team-sync').getByRole('button', { name: confirmLabel }).waitFor()
    assert.equal(await sheet.locator('.team-sync button[role=switch]').count(), 0, 'confirming stands in for the switch')
    await sheet.locator('.team-sync').scrollIntoViewIfNeeded()
    await shot('team-sync-migrated')
    if (everything) {
      await sheet.locator('.team-sync').getByRole('button', { name: confirmLabel }).click()
      const on = page.getByRole('dialog', { name: tr('Turn on sync for {team}?', { team }) })
      await on.getByRole('button', { name: tr('Turn on sync for {team}', { team }) }).click()
      await on.waitFor({ state: 'detached' })
      assert.deepEqual(fake.calls.teamSync.at(-1).body, { enabled: true, version: VERSIONS.sync }, 'confirming gives the team’s agreement to the current text')
      await sheet.locator('.team-sync button[role=switch]').waitFor()
    }
    await sheet.locator('.access-panel').scrollIntoViewIfNeeded()
    await shot('access-administered')
    await closeSheet()
    assert.deepEqual(fake.calls.folders.filter(call => call.account === FINANCE), [], 'no folders were asked for on her behalf')

    // --- the team's people ------------------------------------------------------
    await openSection(tr('Members'))
    await page.locator('.member-list').first().waitFor()
    assert.match(await page.locator(`li[data-user="${ANA}"]`).innerText(), new RegExp(tr('You are the team’s only owner: make another member an owner before you leave or step down.').split(':')[0]))
    // Bea alone reads a mailbox there: she is kept, and the mailbox is named.
    const bea = page.locator(`li[data-user="${BEA}"]`)
    assert.ok(plain(await bea.innerText()).includes(tr('Only reader: {count}', { count: 1 })), 'the last reader is marked')
    assert.equal(await bea.getByRole('button', { name: tr('Remove…') }).count(), 0, 'the last reader is not offered for removal')
    assert.equal(await bea.getByRole('button', { name: tr('Disable…') }).count(), 0, 'nor for disabling')
    assert.ok(plain(await bea.innerText()).includes('financeiro@atendimento.example'), 'the mailbox she alone reads is named')
    // The invitations are read beside the members: none waits for this team.
    await page.getByText(tr('No invitation is waiting. A link you make is shown once, when you make it.').split('.')[0], { exact: false }).waitFor()
    await shot('members')
    if (everything) {
      // A new role takes no Read away: Bea, the last reader of a mailbox, is made a member and an admin again.
      await page.locator(`select[name="role-${BEA}"]`).selectOption('member')
      await until(() => fake.calls.members.some(call => call.method === 'PATCH' && call.body?.role === 'member'), 'the role change')
      await until(async () => !(await page.locator(`select[name="role-${BEA}"]`).isDisabled()), 'the role to be saved')
      await page.locator(`select[name="role-${BEA}"]`).selectOption('admin')
      await until(() => fake.calls.members.some(call => call.method === 'PATCH' && call.body?.role === 'admin'), 'the role changed back')
      await page.getByRole('button', { name: tr('Invite someone') }).click()
      const invite = page.getByRole('dialog').filter({ has: page.locator('form[name=mailie-team-invite]') })
      await invite.locator('input[name=invite-email]').fill('dan@example.test')
      await invite.locator('input[name=invite-role][value=member]').check()
      await shot('invite-form')
      await invite.getByRole('button', { name: 'Make the invitation' }).click()
      const made = page.getByRole('dialog').filter({ has: page.locator('.invite-link') })
      const link = await made.locator('.invite-link').innerText()
      assert.match(link, /#invite=SyntheticTeamInvite_/, 'the link carries its code in the fragment')
      await page.keyboard.press('Escape')
      assert.equal(await made.locator('.invite-link').count(), 1, 'Escape keeps the link on screen')
      await shot('invite-link')
      await made.getByRole('button', { name: tr('Copy link') }).click()
      assert.equal(await page.evaluate(() => navigator.clipboard.readText()), link, 'the link is copied as shown')
      await made.getByRole('button', { name: tr('Done') }).click()
      await made.waitFor({ state: 'detached' })
      assert.equal(await page.evaluate(code => document.documentElement.outerHTML.includes(code), link.split('#invite=')[1].split('&')[0]), false, 'the link leaves the page with its dialog')
      await page.locator('.member-list').nth(1).getByText('dan@example.test').waitFor()
    }

    // --- the personal workspace: creating a team ---------------------------------
    if (everything) {
      await chooseWorkspace(PERSONAL[ANA])
      await page.getByRole('button', { name: 'Create a team…' }).click()
      const create = page.getByRole('dialog', { name: 'Create a team' })
      await create.locator('input[name=team-name]').fill('Vendas')
      await shot('create-team')
      await create.getByRole('button', { name: 'Create the team' }).click()
      await create.waitFor({ state: 'detached' })
      await until(async () => (await page.locator('.console-breadcrumb').innerText()).includes('Vendas'), 'the new team to be shown')
      await shot('team-created')

      // --- turning her own sync off reaches her personal workspace's mailboxes ----
      await openSection(tr('Account'))
      assert.doesNotMatch(await page.locator('.console-breadcrumb').innerText(), /Vendas/, 'the account is the person’s: the header names no workspace on it')
      await page.getByRole('switch', { name: 'Mail sync' }).click()
      const off = page.getByRole('dialog', { name: 'Turn off mail sync?' })
      await off.getByText('Mailie stops syncing the mailboxes of your personal workspace', { exact: false }).waitFor()
      await off.locator('.team-note').getByText('Your teams’ mailboxes sync under each team’s agreement and keep syncing.', { exact: false }).waitFor()
      assert.equal(await off.getByRole('button', { name: 'Turn off and delete' }).isDisabled(), false, 'nothing to name first: it is offered at once')
      await shot('sync-off-teams')
      await off.getByRole('button', { name: 'Cancel' }).click()

      // --- an invitation opened signed in -------------------------------------------
      // A new page load with the link, the session remembered.
      await page.goto('about:blank')
      await page.goto(`${origin}/#invite=${TEAM_INVITE}&email=ana%40example.test`)
      const join = page.getByRole('dialog', { name: 'Join a team?' })
      await join.waitFor()
      assert.equal(new URL(page.url()).hash, '', 'the code leaves the address bar at once')
      await shot('invitation')
      await join.getByRole('button', { name: 'Join the team' }).click()
      await join.waitFor({ state: 'detached' })
      await page.locator('.workspace-notice').getByText('You joined Financeiro as Member.', { exact: false }).waitFor()
      await shot('joined')
    }

    // --- a member, on team mailboxes she does not read -----------------------------------
    // Carol holds Send on one (and, in the passes where Ana gave it, Read on
    // vendas@) and Manage on another: she sees their cards, never their
    // folders, and none of the team's people or access.
    if (mobile) await openMenu()
    await page.locator('button:visible').filter({ hasText: new RegExp(`^\\s*${words.signOut}\\s*$`) }).first().click()
    await page.locator('form[name=mailie-login]').waitFor()
    await page.locator('input[name=username]').fill('carol@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    if (mobile) await openMenu()
    await page.locator('select[name=workspace]:visible').waitFor()
    assert.deepEqual((await page.locator('select[name=workspace]:visible option').allTextContents()).map(item => item.trim()), [words.personal, `${team} · ${words.member}`], 'Carol is a member of Atendimento')
    if (mobile) {
      await shot('member-drawer')
      await page.keyboard.press('Escape')
    }
    await chooseWorkspace(TEAM)
    await page.getByRole('heading', { name: tr('Mailboxes in {team}', { team }) }).waitFor()
    const sendOnly = card('suporte@atendimento.example')
    const manageOnly = card('financeiro@atendimento.example')
    await sendOnly.waitFor()
    await manageOnly.waitFor()
    assert.ok(plain(await sendOnly.innerText()).includes(`${tr('Your access')} ${tr('Send|access')}`), 'the card says Carol only sends')
    assert.ok(plain(await manageOnly.innerText()).includes(`${tr('Your access')} ${tr('Manage')}`), 'the card says Carol only manages')
    const cannotRead = tr('You can see this mailbox, but not read it. Read comes only from an owner or an admin of the team who reads it.')
    for (const item of [sendOnly, manageOnly]) assert.equal(await item.getByText(cannotRead).count(), 1, 'a card without Read says so')
    if (everything) assert.ok(plain(await card('vendas@atendimento.example').innerText()).includes(`${tr('Your access')} ${tr('Read')}`), 'the Read Ana left her shows')
    else assert.equal(await card('vendas@atendimento.example').count(), 0, 'a mailbox she holds nothing on is not there')
    await page.getByText(tr('The people of {team}, and who can use each of its mailboxes, are managed by its owners and admins.', { team })).first().waitFor()
    assert.equal(await page.getByRole('button', { name: new RegExp(`^${tr('Connect an email account')}$`) }).count(), 0, 'a member is offered no connecting into the team')
    assert.ok(!(await navItems()).includes(tr('Members')), 'a member is offered no Members')
    if (mobile) await sendOnly.scrollIntoViewIfNeeded()
    await shot('member-mailboxes')

    // Send only, and Manage only: no folders, no sync pass or switch, no access, no removing.
    for (const [item, name] of [[sendOnly, 'member-send-only'], [manageOnly, 'member-manages']]) {
      await item.getByRole('button').first().click()
      await sheet.waitFor()
      await sheet.getByText(tr('You do not have read access to this mailbox, so its folders and messages are not shown to you. It comes only from an owner or an admin of the team who reads it.')).waitFor()
      assert.equal(await sheet.getByRole('button', { name: tr('Show folders') }).count(), 0, 'no folders without Read')
      assert.equal(await sheet.getByRole('button', { name: tr('Sync now') }).count(), 0, 'no sync pass without Read')
      assert.equal(await sheet.locator('.access-panel').count(), 0, 'a member is shown no one’s access')
      assert.equal(await sheet.locator('.team-sync').count(), 0, 'nor the team’s sync switch')
      assert.equal(await sheet.locator('.danger-zone').count(), 0, 'nor removing it')
      await shot(name)
      await closeSheet()
    }
    assert.deepEqual(fake.calls.folders.filter(call => call.user === CAROL && call.account !== SALES), [], 'no folders were asked for on her behalf')
    assert.deepEqual(fake.calls.teamSync.filter(call => call.user === CAROL), [], 'nor any sync switched')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok teams ${label}`)
  } catch (error) {
    failures.push(`teams ${label}: ${error.message}`)
    console.log(`FAIL teams ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-teams-${label}.png`) }).catch(() => {})
  } finally {
    fake.close()
    await context.close()
  }
}

await browser.close()
if (failures.length) { console.error(failures.join('\n')); process.exit(1) }
