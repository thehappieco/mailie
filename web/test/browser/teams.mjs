// A real browser over the open console's workspaces and teams (docs/workspaces.md),
// with every /v1 call answered by the core's fake daemon (fakeDaemon.mjs)
// and, through its extend hook, an in-memory model of workspaces, members,
// grants and team invitations. Optional QA, not part of `npm test`:
//
//   npm run build && npx vite preview --port 4174 &
//   QA_ORIGIN=http://localhost:4174 node test/browser/teams.mjs
//
// It walks the workspace switcher; a team's mailboxes, each card with what
// the person may do with it; a mailbox's access, ticked and saved, then taken
// away, with the protections said beforehand; the mailboxes an owner
// administers without holding them; taking a link over; the team's people,
// their roles and invitations, the link shown once; creating a team; turning
// sync off with the team mailboxes it reaches named; an invitation opened
// signed in; and, signed in as a member, the team mailboxes she holds a
// grant on without Read, seen without their folders or messages, the one she
// manages, and the team's people with nothing to administer. On desktop and phone,
// light and dark, then in Portuguese and German. It fails on a page error, a
// CSP violation, horizontal scrolling and any server message drawn on a
// screen.
//
// QA_ORIGIN, QA_SCREENSHOTS, QA_PLAYWRIGHT_MODULE, QA_BROWSER,
// QA_BROWSER_EXECUTABLE and QA_BROWSER_CHANNEL as for console.mjs.
import { mkdir } from 'node:fs/promises'
import { resolve } from 'node:path'
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

const VERSIONS = { sync: '2026-10-open-sync-2', actions: '2026-10-open-actions', keys: '2026-10-open-api-keys' }
const ANA = 'usr_00000000000000a1'
const BEA = 'usr_00000000000000c1'
const CAROL = 'usr_00000000000000c2'
const PERSONAL = { [ANA]: 'wsp_00000000000000a1', [BEA]: 'wsp_00000000000000c1', [CAROL]: 'wsp_00000000000000c2' }
const TEAM = 'wsp_000000000000f001'
const OTHER_TEAM = 'wsp_000000000000f002'
/** The invitation into the other team waiting for Ana, which she accepts signed in. */
const TEAM_INVITE = 'SyntheticTeamInvite_0123456789abcdefghijklmn'
const full = () => ({ read: true, act: true, send: true, manage: true })
const none = () => ({ read: false, act: false, send: false, manage: false })

/**
 * The core's fake daemon with workspaces: Ana's three mailboxes in her
 * personal workspace; the team Atendimento, which Ana owns, Bea administers
 * and Carol is a member of, with a mailbox Bea linked (Ana reads and sends
 * from it, Carol only sends), one Ana linked, and one Bea linked that Ana
 * holds nothing on and Carol only manages; and the team Financeiro, whose
 * invitation waits for Ana.
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
  // Every mailbox, wherever it is linked: the core keeps them by the person who linked them.
  for (const account of accountsByUser.get(ANA)) Object.assign(account, { workspace_id: PERSONAL[ANA], linked_by: ANA })
  const teamMailbox = (id, email, linker) => ({ id, email, provider: 'imap', auth_kind: 'password', state: 'active', save_sent_copy: true, created_at: at, workspace_id: TEAM, linked_by: linker, sync: { state: 'off', tier: '', folders_synced: 0, folders_total: 0, messages: 0, initial_progress: 0, last_synced_at: 0 }, target: 400 })
  accountsByUser.set(BEA, [teamMailbox('acc_000000000000f001', 'suporte@atendimento.example', BEA), teamMailbox('acc_000000000000f003', 'financeiro@atendimento.example', BEA)])
  accountsByUser.set(CAROL, [])
  accountsByUser.get(ANA).push(teamMailbox('acc_000000000000f002', 'vendas@atendimento.example', ANA))
  const grants = new Map([
    ['acc_000000000000f001', new Map([[BEA, full()], [ANA, { ...none(), read: true, send: true }], [CAROL, { ...none(), send: true }]])],
    ['acc_000000000000f002', new Map([[ANA, full()]])],
    ['acc_000000000000f003', new Map([[BEA, full()], [CAROL, { ...none(), manage: true }]])],
  ])
  calls.access = []
  calls.members = []
  calls.invites = []
  /** Every folder listing asked of a team mailbox, by whom. */
  calls.folders = []
  const everyAccount = () => [...accountsByUser.values()].flat()
  const workspaceOf = account => account.workspace_id ?? PERSONAL[account.linked_by ?? ANA]
  const membership = (workspaceID, userID) => (members.get(workspaceID) ?? []).find(item => item.user_id === userID && item.status === 'active')
  const grantOf = (accountID, userID) => {
    const account = everyAccount().find(item => item.id === accountID)
    if (account && workspaceOf(account) === PERSONAL[userID] && account.linked_by === userID) return full()
    return grants.get(accountID)?.get(userID) ?? none()
  }
  const any = flags => flags.read || flags.act || flags.send || flags.manage
  const visible = (user, workspaceID) => everyAccount().filter(account => {
    const where = workspaceOf(account)
    return (!workspaceID || where === workspaceID) && membership(where, user.id) && any(grantOf(account.id, user.id))
  })
  const presentAs = (account, user) => {
    const flags = grantOf(account.id, user.id)
    const shown = present(account)
    return { ...shown, workspace_id: workspaceOf(account), linked_by: account.linked_by, access: flags, send: flags.send ? shown.send : { available: false, reason: 'not_granted' } }
  }
  const listWorkspace = (workspace, user) => {
    const mine = (members.get(workspace.id) ?? []).find(item => item.user_id === user.id)
    return { id: workspace.id, kind: workspace.kind, source: workspace.source, name: workspace.name, role: mine.role, status: mine.status, created_at: workspace.created_at }
  }
  const presentMember = (workspaceID, item) => {
    const person = [...users.values()].find(user => user.id === item.user_id)
    const owners = (members.get(workspaceID) ?? []).filter(other => other.role === 'owner' && other.status === 'active')
    const links = everyAccount().filter(account => workspaceOf(account) === workspaceID && account.linked_by === item.user_id).length
    return { user_id: item.user_id, email: person.email, name: person.name, role: item.role, status: item.status, last_owner: item.role === 'owner' && item.status === 'active' && owners.length === 1, links, joined_at: item.joined_at }
  }
  return {
    grants, members, invites, workspaces,
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
        if (part === 'members' && !item) return json(members.get(id).map(entry => presentMember(id, entry)))
        if (part === 'members' && item) {
          const list = members.get(id)
          const target = list.find(entry => entry.user_id === item)
          if (!target) return fail(404, 'not_found')
          const shown = presentMember(id, target)
          calls.members.push({ method, user: item, body: method === 'PATCH' ? body() : undefined })
          if (method === 'PATCH') {
            const change = body()
            if (me.role !== 'owner' && (target.role !== 'member' || (change.role && change.role !== 'member'))) return fail(403, 'not_authorized')
            if (shown.last_owner && ((change.role && change.role !== 'owner') || change.status === 'disabled')) return fail(409, 'conflict')
            if (change.status === 'disabled' && shown.links) return fail(409, 'conflict')
            Object.assign(target, change)
            return json(presentMember(id, target))
          }
          if (method === 'DELETE') {
            if (item !== user.id && me.role !== 'owner' && !(me.role === 'admin' && target.role === 'member')) return fail(403, 'not_authorized')
            if (shown.last_owner || shown.links) return fail(409, 'conflict')
            list.splice(list.indexOf(target), 1)
            return route.fulfill({ status: 204 })
          }
        }
        if (part === 'invites') {
          if (!admin) return fail(403, 'not_authorized')
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
          const mailboxes = everyAccount().filter(account => workspaceOf(account) === id && (admin || grantOf(account.id, user.id).manage))
          return json(mailboxes.map(account => ({
            account_id: account.id, email: account.email, provider: account.provider, state: account.state, linked_by: account.linked_by,
            grants: [...(grants.get(account.id) ?? new Map()).entries()].map(([userID, flags]) => ({ account_id: account.id, user_id: userID, ...flags, granted_by: account.linked_by, updated_at: now() })),
          })))
        }
        return fail(404, 'not_found')
      }
      const accessMatch = path.match(/^\/v1\/accounts\/([^/]+)\/access\/([^/]+)$/)
      if (accessMatch) {
        const [, accountID, target] = accessMatch
        const account = everyAccount().find(item => item.id === accountID)
        if (!account) return fail(404, 'not_found')
        const where = workspaceOf(account)
        const me = membership(where, user.id)
        if (!me) return fail(404, 'not_found')
        const admin = me.role === 'owner' || me.role === 'admin'
        const mine = grantOf(accountID, user.id)
        const before = grantOf(accountID, target)
        calls.access.push({ method, account: accountID, user: target, body: method === 'PUT' ? body() : undefined, flags: query.get('flags') ?? '' })
        if (target === account.linked_by) return fail(409, 'conflict')
        if (method === 'PUT') {
          const after = body()
          if (!(admin || mine.manage)) return fail(403, 'not_authorized')
          for (const flag of ['read', 'act', 'send']) if (after[flag] && !before[flag] && !mine[flag]) return fail(403, 'not_authorized')
          if (after.act && !after.read) return fail(400, 'bad_request')
          if (!membership(where, target)) return fail(404, 'not_found')
          const map = grants.get(accountID) ?? new Map()
          grants.set(accountID, map)
          map.set(target, { read: !!after.read, act: !!after.act, send: !!after.send, manage: !!after.manage })
          return json({ account_id: accountID, user_id: target, ...map.get(target), granted_by: user.id, updated_at: now() })
        }
        if (method === 'DELETE') {
          if (!(admin || mine.manage || target === user.id)) return fail(403, 'not_authorized')
          const drop = (query.get('flags') || 'read,act,send,manage').split(',')
          const next = { ...before }
          for (const flag of drop) next[flag] = false
          if (drop.includes('read')) next.act = false
          const map = grants.get(accountID) ?? new Map()
          if (any(next)) map.set(target, next)
          else map.delete(target)
          return route.fulfill({ status: 204 })
        }
      }
      const takeOver = path.match(/^\/v1\/accounts\/([^/]+)\/take-over$/)
      if (takeOver && method === 'POST') {
        const account = everyAccount().find(item => item.id === takeOver[1])
        const me = account && membership(workspaceOf(account), user.id)
        if (!account || !me) return fail(404, 'not_found')
        const mine = grantOf(account.id, user.id)
        if (!(mine.read && mine.act && mine.send && mine.manage) || !['owner', 'admin'].includes(me.role) || !user.consent) return fail(409, 'conflict')
        const previous = account.linked_by
        accountsByUser.set(previous, accountsByUser.get(previous).filter(item => item !== account))
        accountsByUser.get(user.id).push(account)
        account.linked_by = user.id
        return json(presentAs(account, user))
      }
      if (path === '/v1/accounts' && method === 'GET') return json(visible(user, query.get('workspace') ?? '').map(account => presentAs(account, user)))
      if (path === '/v1/accounts' && method === 'POST' && body().workspace_id) {
        const request = body()
        const where = request.workspace_id
        const me = membership(where, user.id)
        if (!me) return fail(404, 'not_found')
        if (!['owner', 'admin'].includes(me.role)) return fail(403, 'not_authorized')
        const account = { ...teamMailbox(`acc_000000000000f${String(100 + everyAccount().length)}`, request.email, user.id), workspace_id: where }
        accountsByUser.get(user.id).push(account)
        grants.set(account.id, new Map([[user.id, full()]]))
        return json({ account: presentAs(account, user) }, 201)
      }
      // A mailbox someone else linked: its card and its folders, for whoever may see them.
      const one = path.match(/^\/v1\/accounts\/([^/]+)(\/folders)?$/)
      if (one && method === 'GET') {
        const account = visible(user, '').find(item => item.id === one[1])
        if (!account) return one[2] ? undefined : fail(404, 'not_found')
        if (!one[2]) return json(presentAs(account, user))
        calls.folders.push({ user: user.id, account: account.id })
        if (!grantOf(account.id, user.id).read) return fail(403, 'not_authorized')
        if ((accountsByUser.get(user.id) ?? []).includes(account)) return undefined
        return json([{ name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 40 }])
      }
      if (path === '/v1/me/storage' && method === 'GET') {
        const readable = visible(user, query.get('workspace') ?? '').filter(account => grantOf(account.id, user.id).read)
        const mailboxes = readable.map(account => {
          const messages = (users.get([...users.keys()].find(email => users.get(email).id === account.linked_by)) ?? user).consent ? account.sync.messages : 0
          return { account_id: account.id, workspace_id: workspaceOf(account), email: account.email, messages, bytes: messages * 3200 }
        }).sort((a, b) => a.email < b.email ? -1 : 1)
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

/** The words each pass expects, in its language. */
const WORDS = {
  'en-US': {
    mailboxes: 'Mailboxes', members: 'Members', account: 'Account', workspace: 'Workspace', personal: 'Personal', team: 'Atendimento · Owner',
    inTeam: 'Mailboxes in Atendimento', yourAccess: 'Your access', linkedBy: 'Linked by', others: 'Other mailboxes of Atendimento',
    access: 'Access', save: 'Save access', takeOver: 'Take over the link…', invite: 'Invite someone', copy: 'Copy link', done: 'Done',
    onlyOwner: 'You are the team’s only owner', linkerLock: 'Linked this mailbox: it syncs under their agreement',
    signOut: 'Sign out', member: 'Atendimento · Member', read: 'Read', send: 'Send', manage: 'Manage', leave: 'Leave the team…', showFolders: 'Show folders', syncNow: 'Sync now',
    cannotRead: 'You can see this mailbox, but not read it. Read access comes only from someone who has it and can change who has access.',
    noFolders: 'You do not have read access to this mailbox, so its folders and messages are not shown to you.',
    noInvites: 'No invitation is waiting.', giveOnly: 'You can give only what you hold on this mailbox yourself: not Read, Act, or Send.',
  },
  'pt-BR': {
    mailboxes: 'Caixas de email', members: 'Membros', account: 'Conta', workspace: 'Espaço de trabalho', personal: 'Pessoal', team: 'Atendimento · Proprietário',
    inTeam: 'Caixas de email em Atendimento', yourAccess: 'Seu acesso', linkedBy: 'Vinculada por', others: 'Outras caixas de email de Atendimento',
    access: 'Acesso', save: 'Salvar acesso', takeOver: 'Assumir o vínculo…', invite: 'Convidar alguém', copy: 'Copiar link', done: 'Concluir',
    onlyOwner: 'Você é o único proprietário da equipe', linkerLock: 'Vinculou esta caixa de email',
    signOut: 'Sair', member: 'Atendimento · Membro', read: 'Leitura', send: 'Envio', manage: 'Gestão', leave: 'Sair da equipe…', showFolders: 'Mostrar pastas', syncNow: 'Sincronizar agora',
    cannotRead: 'Você vê esta caixa de email, mas não pode lê-la. O acesso de leitura só vem de alguém que o tem e pode mudar quem tem acesso.',
    noFolders: 'Você não tem acesso de leitura a esta caixa de email, então as pastas e mensagens dela não são mostradas a você.',
    noInvites: 'Nenhum convite pendente.', giveOnly: 'Você só pode dar o que você mesmo tem nesta caixa de email: não Leitura, Ações ou Envio.',
  },
  'de-DE': {
    mailboxes: 'Postfächer', members: 'Mitglieder', account: 'Konto', workspace: 'Arbeitsbereich', personal: 'Persönlich', team: 'Atendimento · Inhaber',
    inTeam: 'Postfächer in Atendimento', yourAccess: 'Ihr Zugriff', linkedBy: 'Verknüpft von', others: 'Weitere Postfächer von Atendimento',
    access: 'Zugriff', save: 'Zugriff speichern', takeOver: 'Verknüpfung übernehmen…', invite: 'Jemanden einladen', copy: 'Link kopieren', done: 'Fertig',
    onlyOwner: 'Sie sind der einzige Inhaber des Teams', linkerLock: 'Hat dieses Postfach verknüpft',
    signOut: 'Abmelden', member: 'Atendimento · Mitglied', read: 'Lesen', send: 'Senden', manage: 'Verwalten', leave: 'Team verlassen…', showFolders: 'Ordner anzeigen', syncNow: 'Jetzt synchronisieren',
    cannotRead: 'Sie sehen dieses Postfach, können es aber nicht lesen. Lesezugriff kann Ihnen nur geben, wer ihn selbst hat und ändern darf, wer Zugriff hat.',
    noFolders: 'Sie haben keinen Lesezugriff auf dieses Postfach, daher werden Ihnen seine Ordner und Nachrichten nicht angezeigt.',
    noInvites: 'Keine Einladung offen.', giveOnly: 'Sie können nur geben, was Sie selbst für dieses Postfach haben: nicht Lesen, Aktionen oder Senden.',
  },
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
  const openSection = async name => {
    if (mobile) await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  const chooseWorkspace = async id => {
    if (mobile) {
      await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
      await page.locator('.mobile-navigation select[name=workspace]').selectOption(id)
      await page.keyboard.press('Escape')
    } else {
      await page.locator('.console-sidebar select[name=workspace]').selectOption(id)
    }
  }
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()

    // --- the personal workspace, and the switcher ---------------------------
    if (mobile) await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
    const select = page.locator('select[name=workspace]:visible')
    assert.deepEqual((await select.locator('option').allTextContents()).map(item => item.trim()), [words.personal, words.team], 'the switcher lists the person’s workspaces, the team with their role')
    if (mobile) await page.keyboard.press('Escape')
    assert.equal(await page.locator('.account-card').count(), 3, 'the personal workspace lists Ana’s own mailboxes')
    await shot('personal')

    // --- a team's mailboxes --------------------------------------------------
    await chooseWorkspace(TEAM)
    await page.getByRole('heading', { name: words.inTeam }).waitFor()
    const shared = page.locator('.account-card').filter({ hasText: 'suporte@atendimento.example' })
    await shared.waitFor()
    assert.match(await shared.innerText(), new RegExp(words.linkedBy))
    assert.match(await shared.innerText(), new RegExp(words.yourAccess))
    await page.getByRole('heading', { name: words.others }).waitFor()
    assert.equal(await page.locator('.other-card').filter({ hasText: 'financeiro@atendimento.example' }).count(), 1, 'an owner sees the team mailbox she holds nothing on')
    const crumb = await page.locator('.console-breadcrumb').innerText()
    assert.match(crumb, /Atendimento/, 'the header names the workspace shown')
    await shot('team-mailboxes')
    await page.locator('.other-card').scrollIntoViewIfNeeded()
    await shot('team-others')

    // --- a mailbox's access ---------------------------------------------------
    await page.locator('.account-card').filter({ hasText: 'vendas@atendimento.example' }).getByRole('button').first().click()
    const sheet = page.locator('dialog.sheet-backdrop[open]')
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.match(await sheet.locator(`li[data-user="${ANA}"]`).innerText(), /./)
    assert.equal(await sheet.locator(`input[name="${ANA}-read"]`).isDisabled(), true, 'the linker’s own grant does not change')
    if (everything) {
      await sheet.locator(`input[name="${CAROL}-act"]`).check()
      assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isChecked(), true, 'act ticks read')
      await shot('access-ticked')
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).click()
      await until(() => fake.calls.access.length === 1, 'the grant to be saved')
      assert.deepEqual(fake.calls.access[0].body, { read: true, act: true, send: false, manage: false })
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).waitFor({ state: 'detached' })
      await sheet.locator(`input[name="${CAROL}-send"]`).check()
      await sheet.locator(`input[name="${CAROL}-act"]`).uncheck()
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).click()
      await until(() => fake.calls.access.length === 2, 'the second change to be saved')
      assert.deepEqual(fake.calls.access[1].body, { read: true, act: false, send: true, manage: false }, 'a change that also adds sets the whole grant')
      // Taking away only: the flags taken are named, and the rest stays.
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).waitFor({ state: 'detached' })
      await sheet.locator(`input[name="${CAROL}-send"]`).uncheck()
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).click()
      await until(() => fake.calls.access.length === 3, 'the revocation to be saved')
      assert.deepEqual([fake.calls.access[2].method, fake.calls.access[2].flags], ['DELETE', 'send'], 'a change that only takes away names the flags taken')
      await sheet.locator(`li[data-user="${CAROL}"]`).getByRole('button', { name: words.save }).waitFor({ state: 'detached' })
      assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isChecked(), true, 'Read stays')
      assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isChecked(), false, 'Send went')
    }
    await sheet.locator(`li[data-user="${CAROL}"]`).scrollIntoViewIfNeeded()
    await shot('access')
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })

    // A mailbox Bea linked: Ana reads and sends, and gives no act she lacks.
    await shared.getByRole('button').first().click()
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.equal(await sheet.locator(`input[name="${CAROL}-act"]`).isDisabled(), true, 'an owner gives no act she does not hold')
    assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isDisabled(), false, 'she gives the read she holds')
    assert.match(await sheet.locator(`li[data-user="${BEA}"]`).innerText(), new RegExp(words.linkerLock), 'the linker’s lock is said beforehand')
    assert.equal(await sheet.getByRole('button', { name: words.takeOver }).isDisabled(), true, 'no take-over without every flag')
    assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isChecked(), true, 'Carol’s Send is shown')
    await sheet.locator('.access-panel').scrollIntoViewIfNeeded()
    await shot('access-shared')
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })

    // A mailbox Ana holds nothing on: only Manage is hers to give.
    await page.locator('.other-card').getByRole('button').click()
    const accessDialog = page.getByRole('dialog').filter({ has: page.locator('.access-panel') })
    await accessDialog.locator('.access-rows').waitFor()
    assert.equal(await accessDialog.locator(`input[name="${CAROL}-read"]`).isDisabled(), true)
    assert.equal(await accessDialog.locator(`input[name="${CAROL}-manage"]`).isDisabled(), false)
    assert.equal(await accessDialog.locator(`input[name="${CAROL}-manage"]`).isChecked(), true, 'Carol manages it')
    await accessDialog.getByText(words.giveOnly).waitFor()
    await shot('access-administered')
    await page.keyboard.press('Escape')

    // --- the team's people ------------------------------------------------------
    await openSection(words.members)
    await page.locator('.member-list').first().waitFor()
    assert.match(await page.locator(`li[data-user="${ANA}"]`).innerText(), new RegExp(words.onlyOwner))
    // The invitations are read beside the members: none waits for this team.
    await page.getByText(words.noInvites, { exact: false }).waitFor()
    await shot('members')
    if (everything) {
      await page.locator(`select[name="role-${CAROL}"]`).selectOption('admin')
      await until(() => fake.calls.members.some(call => call.method === 'PATCH' && call.body?.role === 'admin'), 'the role change')
      // And back: Carol stays a member, for her own pass below.
      await until(async () => !(await page.locator(`select[name="role-${CAROL}"]`).isDisabled()), 'the role to be saved')
      await page.locator(`select[name="role-${CAROL}"]`).selectOption('member')
      await until(() => fake.calls.members.some(call => call.method === 'PATCH' && call.body?.role === 'member'), 'the role changed back')
      await page.getByRole('button', { name: words.invite }).click()
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
      await made.getByRole('button', { name: words.copy }).click()
      assert.equal(await page.evaluate(() => navigator.clipboard.readText()), link, 'the link is copied as shown')
      await made.getByRole('button', { name: words.done }).click()
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

      // --- turning sync off names the team mailboxes Ana linked -------------------
      await openSection(words.account)
      assert.doesNotMatch(await page.locator('.console-breadcrumb').innerText(), /Vendas/, 'the account is the person’s: the header names no workspace on it')
      await page.getByRole('switch', { name: 'Mail sync' }).click()
      const off = page.getByRole('dialog', { name: 'Turn off mail sync?' })
      await off.locator('.team-warning').getByText('vendas@atendimento.example in Atendimento').waitFor()
      assert.equal(await off.locator('.team-warning').getByText('suporte@atendimento.example').count(), 0, 'a mailbox someone else linked is not hers to delete')
      await shot('sync-off-team-warning')
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
    // vendas@) and Manage on another: she sees their cards, never their folders.
    if (mobile) await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
    await page.locator('button:visible').filter({ hasText: new RegExp(`^\\s*${words.signOut}\\s*$`) }).first().click()
    await page.locator('form[name=mailie-login]').waitFor()
    await page.locator('input[name=username]').fill('carol@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    if (mobile) await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
    await page.locator('select[name=workspace]:visible').waitFor()
    assert.deepEqual((await page.locator('select[name=workspace]:visible option').allTextContents()).map(item => item.trim()), [words.personal, words.member], 'Carol is a member of Atendimento')
    if (mobile) {
      await shot('member-drawer')
      await page.keyboard.press('Escape')
    }
    await chooseWorkspace(TEAM)
    await page.getByRole('heading', { name: words.inTeam }).waitFor()
    const sendOnly = page.locator('.account-card').filter({ hasText: 'suporte@atendimento.example' })
    const manageOnly = page.locator('.account-card').filter({ hasText: 'financeiro@atendimento.example' })
    await sendOnly.waitFor()
    await manageOnly.waitFor()
    const plain = value => value.replace(/\s+/g, ' ').trim()
    assert.ok(plain(await sendOnly.innerText()).includes(`${words.yourAccess} ${words.send}`), 'the card says Carol only sends')
    assert.ok(plain(await manageOnly.innerText()).includes(`${words.yourAccess} ${words.manage}`), 'the card says Carol only manages')
    for (const card of [sendOnly, manageOnly]) assert.equal(await card.getByText(words.cannotRead).count(), 1, 'a card without Read says so')
    if (everything) assert.ok(plain(await page.locator('.account-card').filter({ hasText: 'vendas@atendimento.example' }).innerText()).includes(`${words.yourAccess} ${words.read}`), 'the Read Ana left her shows')
    else assert.equal(await page.locator('.account-card').filter({ hasText: 'vendas@atendimento.example' }).count(), 0, 'a mailbox she holds nothing on is not there')
    assert.equal(await page.locator('.other-card').count(), 0, 'a member sees no access directory')
    assert.equal(await page.getByRole('button', { name: /^(Connect an email account|Conectar uma conta de email|E-Mail-Konto verbinden)$/ }).count(), 0, 'a member is offered no connecting into the team')
    if (mobile) await sendOnly.scrollIntoViewIfNeeded()
    await shot('member-mailboxes')

    // Send only: no folders, no sync pass, only her own access, which she may give up.
    await sendOnly.getByRole('button').first().click()
    await sheet.getByText(words.noFolders, { exact: false }).waitFor()
    assert.equal(await sheet.getByRole('button', { name: words.showFolders }).count(), 0, 'no folders without Read')
    assert.equal(await sheet.getByRole('button', { name: words.syncNow }).count(), 0, 'no sync pass without Read')
    await sheet.locator('.access-panel .access-rows').waitFor()
    assert.equal(await sheet.locator('.access-row').count(), 1, 'someone who only uses a mailbox sees only their own access')
    assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isChecked(), true)
    assert.equal(await sheet.locator(`input[name="${CAROL}-send"]`).isDisabled(), false, 'she may give it up')
    assert.equal(await sheet.locator(`input[name="${CAROL}-read"]`).isDisabled(), true, 'nor gives herself Read')
    await sheet.locator('.access-panel').scrollIntoViewIfNeeded()
    await shot('member-send-only')
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })

    // Manage only: everyone's access, of which she gives Manage alone.
    await manageOnly.getByRole('button').first().click()
    await sheet.getByText(words.noFolders, { exact: false }).waitFor()
    await sheet.locator('.access-panel .access-rows').waitFor()
    await sheet.getByText(words.giveOnly).waitFor()
    assert.equal(await sheet.locator(`input[name="${ANA}-manage"]`).isDisabled(), false, 'she gives Manage')
    assert.equal(await sheet.locator(`input[name="${ANA}-read"]`).isDisabled(), true, 'and no Read she does not hold')
    await sheet.locator('.access-panel').scrollIntoViewIfNeeded()
    await shot('member-manages')
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })
    assert.deepEqual(fake.calls.folders.filter(call => call.user === CAROL), [], 'no folders were asked for on her behalf')

    // The team's people: nothing to administer, only leaving.
    await openSection(words.members)
    await page.locator('.member-list').first().waitFor()
    assert.ok(plain(await page.locator(`li[data-user="${CAROL}"]`).innerText()).includes(words.leave.replace('…', '')), 'she may leave')
    assert.equal(await page.getByRole('button', { name: words.invite }).count(), 0, 'a member invites nobody')
    assert.equal(await page.locator('.members-section select').count(), 0, 'a member changes no role')
    assert.equal(await page.locator('.member-list').count(), 1, 'a member sees no invitations')
    await shot('member-members')
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
