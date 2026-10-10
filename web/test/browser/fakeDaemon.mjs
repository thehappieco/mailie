// The daemon as far as the console's core can tell, in memory, for the
// browser QA of every edition: the open console's (test/browser/console.mjs)
// and any other built on this core, which imports this file and adds its own
// routes and state through extend. Not a spec, and nothing here reaches a
// daemon, a provider or a personal browser profile. Also the helpers both
// QA scripts use.
import assert from 'node:assert/strict'
import { enrolment, KDF, saltOf } from './keyScheme.mjs'

export const now = () => Math.floor(Date.now() / 1000)
export const sleep = ms => new Promise(done => setTimeout(done, ms))
export const PASSWORD = 'synthetic-password-1'
/** What a person's account public key and wraps are bound to (docs/key-scheme.md section 3.1). Synthetic. */
export const ANA_SEAL_ID = 'b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4'
/** A reset link's code, as `user password --bootstrap` prints one. Synthetic. */
export const RESET = 'SyntheticResetCode_0123456789abcdefghijklmnop'
/** The shape Apple gives an app-specific password. Synthetic. */
export const APPLE_APP_PASSWORD = 'abcd-efgh-ijkl-mnop'
export const INVITE = 'SyntheticInviteCode_0123456789abcdefghijklmn'
/** state_reason strings the console explains itself (src/ui/reasons.ts), as the daemon writes them. */
export const REASON_NOT_GRANTED = 'the provider did not grant access to the mailbox'
export const REASON_TOKEN_REJECTED = "the mail server rejected the account's access token"
/** A Gmail consent for this address comes back without access to the mailbox, as an unticked box on Google's screen leaves it. */
export const NOT_GRANTED_EMAIL = 'recusada@example.test'
/** An account's sync before anything has run: what the daemon keeps for a mailbox nobody consented for. */
export const idle = () => ({ state: 'off', tier: '', folders_synced: 0, folders_total: 0, messages: 0, initial_progress: 0, last_synced_at: 0 })
/** The Gmail account: the one whose folders a sync pass reads again, the one a key is made for, and the one whose mail an edition with Mail reads. */
export const GMAIL_ACCOUNT = 'acc_0000000000000001'

/**
 * The daemon, as far as the console's core can tell: users, sessions,
 * accounts and their OAuth flows, sync and its consent, the event stream,
 * actions consent, API keys, whether MCP is served over HTTP, and storage.
 *
 * origin: where the console runs, which the Microsoft web flow returns to.
 * versions: { sync, actions, keys }, the revisions of the edition's texts the
 * daemon asks about (its MAIL_CONSENT_VERSION_*).
 * refuseFolders: account ids whose mail server refuses the grant when their
 * folders are listed, as Gmail does for a token issued to another account.
 * consented: Ana turned sync on a day ago. actionsAgreed: the revision of the
 * actions text Ana agreed to three days ago; '' for none. allMailHidden: Show
 * in IMAP is off for All Mail in the Gmail account's settings, so its IMAP
 * lists no All Mail. mcpHTTP and keysSend: what GET /v1/me/mcp says
 * (MAIL_MCP_HTTP, MAIL_KEYS_MAY_SEND). personal: the server lists each
 * person's personal workspace (GET /v1/workspaces), as every server with
 * workspaces does, and its owner keeps that workspace's API keys; left out,
 * it answers 404, as a server older than workspaces would. notEnrolled: the
 * people seeded with a password signed up before the key scheme and never
 * enrolled: nothing signs them in, a sign-in failing as a wrong password's,
 * and a reset link enrols them (the upgrade, docs/key-scheme.md section 12.7,
 * left after the release that brought the scheme); otherwise they enrolled
 * with that password, and the daemon holds only what their browser would
 * have sent (an auth key, wraps it cannot open), made the first time
 * anything asks.
 *
 * extend(core): an edition's own state and routes, called once with the
 * core's state and helpers. It may return methods (added to the daemon),
 * route(request), which sees every authenticated request before the core
 * does and returns undefined for one it leaves to the core,
 * trashMessages(), what the index holds in the Gmail account's Trash, and
 * keyWorkspace(user, id), a workspace whose API keys the core's key routes
 * keep: { admin, mailboxes: [{ id, reads }] } for a member (admin: an owner
 * or an admin of it; reads: the person reads that mailbox), or null.
 */
export function fakeDaemon({ origin, versions, refuseFolders = [], progressMS = 600, consented = false, actionsAgreed = '', allMailHidden = false, mcpHTTP = true, keysSend = true, personal = false, notEnrolled = false, extend } = {}) {
  if (!origin || !versions?.sync || !versions.actions || !versions.keys) throw new Error('fakeDaemon: origin and versions { sync, actions, keys } are required')
  const users = new Map([['ana@example.test', { id: 'usr_00000000000000a1', seal_id: ANA_SEAL_ID, email: 'ana@example.test', name: 'Ana Souza', role: 'owner', created_at: now() - 86400 * 30, password: PASSWORD, consent: consented ? now() - 86400 : 0, ...(consented ? { consentVersion: versions.sync } : {}), actions: actionsAgreed ? now() - 3 * 86400 : 0, actionsVersion: actionsAgreed }]])
  /** The ceremonies' single-use tickets, and the reset links waiting: { purpose, user, token?, salt }. */
  const tickets = new Map()
  const resets = new Map([[RESET, 'ana@example.test']])
  /** The seal id drawn for the invitation when it is first opened. */
  let inviteSeal = ''
  const sessions = new Map()
  const flows = new Map()
  let serial = 0
  // Account ids have their own counter, past the fixtures below: sharing the
  // tokens' counter gave a new account the id of an existing one.
  let accountSerial = 100
  const calls = { login: 0, challenge: 0, stepUp: 0, recovery: 0, auth: [], me: 0, polls: 0, callback: [], removed: [], created: [], streams: [], consent: [], actionsConsent: [], syncNow: [], keys: [], mailboxKeys: [], storage: 0, mcp: 0 }
  /** Every workspace's API keys as the daemon stores them: never the secret, which only the creating answer carries. */
  const keys = []
  /** The record of each key's sends, by prefix (service.SendStatus): never who a message went to, its subject or its text. */
  const keySends = new Map()
  // The journal, as internal/events keeps it: every event names its account,
  // and a stream only carries the events of the caller's accounts.
  const journal = []
  let seq = 0
  let closed = false
  /** Set to have an open stream notice a revoked session, as the daemon's ping does. */
  const recheck = { streams: false }
  /** The policy revision the daemon asks consent for (service.SyncConsentVersion); a pass moves it to model a policy change. */
  const policy = { version: versions.sync }
  /**
   * Whether Gmail's IMAP lists All Mail (the person's Show in IMAP setting),
   * and whether the index has it: the daemon learns a change when it reads
   * the folder list again, every 30 minutes or at a "Sync now".
   */
  const allMail = { listed: !allMailHidden, indexed: !allMailHidden }
  const tickers = new Map()
  // No sync_tier or last_ok_at: nothing in the daemon writes them until the
  // sync engine exists, and these screens should look like what it serves today.
  const accountsByUser = new Map([['usr_00000000000000a1', [
    { id: 'acc_0000000000000001', email: 'suporte@example.test', display_name: 'Suporte', provider: 'gmail', auth_kind: 'oauth2', state: 'active', save_sent_copy: false, created_at: now() - 86400 * 12, sync: idle(), target: 1284 },
    { id: 'acc_0000000000000002', email: 'ana.souza@example.test', provider: 'microsoft', auth_kind: 'oauth2', state: 'needs_reauth', state_reason: 'refresh token revoked', save_sent_copy: false, created_at: now() - 86400 * 40, sync: idle(), target: 90 },
    { id: 'acc_0000000000000003', email: 'contato@<b>loja</b>.example.test', display_name: '<img src=x onerror=alert(1)>', provider: 'imap', auth_kind: 'password', state: 'active', save_sent_copy: true, created_at: now() - 86400 * 3, sync: idle(), target: 356 },
  ]]])
  const owner = id => [...users.values()].find(user => (accountsByUser.get(user.id) ?? []).some(account => account.id === id))
  /** An account as the daemon presents it: the sync block says off unless its owner consented and it is active. */
  const present = account => {
    const { sync, target: _, key, ...rest } = account
    const enabled = Boolean(owner(account.id)?.consent)
    const on = enabled && account.state === 'active'
    const block = { enabled, running: on && sync.state !== 'off', state: on ? sync.state : 'off', folders_synced: on ? sync.folders_synced : 0, folders_total: on ? sync.folders_total : 0, messages: on ? sync.messages : 0, initial_progress: on ? sync.initial_progress : 0 }
    if (on && sync.tier) block.tier = sync.tier
    if (on && sync.last_synced_at) block.last_synced_at = sync.last_synced_at
    // Where a move can go: Gmail archives to All Mail, while the index has it, and has a Trash; this IMAP server has no Archive folder.
    // Without All Mail the daemon says why (service errNoAllMail), and only then does the console say where to turn it on.
    const archive = account.provider === 'gmail' ? allMail.indexed : account.provider !== 'imap'
    // Whether it can send for this caller (service.sendOf): the owner's mailbox, working.
    const send = account.state === 'active' ? { available: true, from_name: 'Ana Souza' } : { available: false, reason: account.state, from_name: 'Ana Souza' }
    // Its key pair at the current epoch (service.Account's mailbox_key), once a person's browser made one.
    const mailboxKey = key ? { mailbox_key: { epoch: key.epoch, public_key: key.public_key, namespace: key.namespace } } : {}
    return { ...rest, sync: block, actions: { archive, trash: true, ...(account.provider === 'gmail' && !archive ? { archive_reason: 'all_mail_hidden' } : {}) }, send, ...mailboxKey }
  }
  const emit = (user, type, accountID, payload = {}) => journal.push({ user: user.id, seq: ++seq, type, account_id: accountID, at: now(), payload: { account_id: accountID, ...payload } })
  /** The first sync of an account: the last 90 days in steps, sync.progress on each, folder.changed{initial_done} at the end. */
  const startSync = (user, account) => {
    account.sync = { ...idle(), state: 'initial', tier: account.provider === 'microsoft' ? 'uidpoll' : 'condstore', folders_total: 5 }
    clearInterval(tickers.get(account.id))
    tickers.set(account.id, setInterval(() => {
      const sync = account.sync
      sync.initial_progress = Math.min(100, sync.initial_progress + 10)
      sync.messages = Math.round(account.target * sync.initial_progress / 100)
      sync.folders_synced = Math.floor(sync.initial_progress / 20)
      emit(user, 'sync.progress', account.id, { progress: sync.initial_progress })
      if (sync.initial_progress < 100) return
      clearInterval(tickers.get(account.id))
      Object.assign(sync, { state: 'live', last_synced_at: now() })
      emit(user, 'folder.changed', account.id, { folder_id: 1, name: 'INBOX', role: 'inbox', change: 'initial_done', count: sync.messages })
    }, progressMS))
  }
  /** Folders as the index lists them once sync has run: counts are what is indexed, and All Mail is not synced. */
  const indexedFolders = account => {
    const others = 30 + 3 + 7
    // Each with its id in the index, which is what GET /v1/messages filters by.
    return [
      { id: 1, name: 'INBOX', display_name: 'Inbox', role: 'inbox', role_source: 'special-use', selectable: true, synced: true, messages: Math.max(0, account.sync.messages - others), unseen: 12, sync_state: account.sync.state === 'live' ? 'live' : 'initial' },
      { id: 2, name: '[Gmail]/Sent Mail', display_name: 'Sent Mail', role: 'sent', role_source: 'special-use', selectable: true, synced: true, messages: 30, sync_state: 'live' },
      { id: 3, name: '[Gmail]/Drafts', display_name: 'Drafts', role: 'drafts', role_source: 'special-use', selectable: true, synced: true, messages: 3, sync_state: 'live' },
      { id: 4, name: '[Gmail]/All Mail', display_name: 'All Mail', role: 'all', role_source: 'special-use', selectable: true, synced: false, sync_state: 'disabled' },
      { id: 5, name: 'Clientes/2026', display_name: 'Clientes/2026', selectable: true, synced: true, messages: 7, sync_state: 'live' },
      { id: 6, name: '[Gmail]/Trash', display_name: 'Trash', role: 'trash', role_source: 'special-use', selectable: true, synced: true, messages: extension.trashMessages?.() ?? 0, sync_state: 'live' },
    ].filter(folder => folder.role !== 'all' || allMail.indexed)
  }
  if (consented) {
    for (const account of accountsByUser.get('usr_00000000000000a1')) {
      if (account.state === 'active') account.sync = { state: 'live', tier: 'condstore', folders_synced: 5, folders_total: 5, messages: account.target, initial_progress: 100, last_synced_at: now() - 120 }
      // Synced before its grant was revoked: the daemon stopped syncing it
      // and keeps what it indexed (service.Storage), which its sync block
      // does not show while it is not running.
      if (account.state === 'needs_reauth') account.sync = { ...idle(), messages: account.target }
    }
  }
  const providers = [
    { id: 'gmail', oauth: true, password: false, flows: ['loopback'] },
    { id: 'microsoft', oauth: true, password: false, flows: ['web', 'loopback'] },
    { id: 'icloud', oauth: false, password: true, flows: [] },
    { id: 'imap', oauth: false, password: true, flows: [] },
  ]
  const folders = [
    { name: 'INBOX', display_name: 'Inbox', role: 'inbox', role_source: 'special-use', selectable: true, synced: true, messages: 1284, unseen: 12 },
    { name: '[Gmail]/Sent Mail', display_name: 'Sent Mail', role: 'sent', role_source: 'special-use', selectable: true, synced: true, messages: 411 },
    { name: '[Gmail]/Drafts', display_name: 'Drafts', role: 'drafts', selectable: true, synced: true, messages: 3 },
    // As the Gmail profile has it: labels and Trash are synced; All Mail is not.
    { name: 'Clientes/2026', display_name: 'Clientes/2026', selectable: true, synced: true, messages: 87, unseen: 2 },
    { name: '[Gmail]/Trash', display_name: 'Trash', role: 'trash', selectable: true, synced: true, messages: 25 },
    { name: '[Gmail]/All Mail', display_name: 'All Mail', role: 'all', role_source: 'special-use', selectable: true, synced: false, messages: 9120 },
  ]
  const token = () => `tok_${String(++serial).padStart(4, '0')}_synthetic_bearer_token_value_xyz`.slice(0, 43)
  const issue = (user, authenticatedAt = now()) => {
    const value = token()
    const session = { id: `ses_${String(serial).padStart(16, '0')}`, user_id: user.id, created_at: now(), expires_at: now() + 14 * 86400, authenticated_at: authenticatedAt }
    sessions.set(value, session)
    return { token: value, expires_at: session.expires_at, authenticated_at: authenticatedAt, user: publicUser(user) }
  }
  const publicUser = ({ id, email, name, role, created_at, seal_id: sealID, publicKey }) => ({
    id, email, name, role, created_at, has_password: true, seal_id: sealID ?? `00000000-0000-4000-8000-${id.slice(-12)}`, ...(publicKey ? { public_key: publicKey } : {}),
  })
  /** An address's target: the salt the server's salt key gives it (docs/key-scheme.md section 5.3). */
  const targetOf = email => saltOf(`target|${String(email).trim().toLowerCase()}`)
  /**
   * A person seeded with a password enrolled with it, as their browser would
   * have: made the first time anything asks, unless they never enrolled.
   */
  const enrolled = async user => {
    if (!user || user.authKey || !user.password || notEnrolled) return user
    user.seal_id ??= publicUser(user).seal_id
    user.salt = targetOf(user.email)
    Object.assign(user, await enrolment(user.password, user.salt, user.seal_id))
    return user
  }
  const ticketOf = (purpose, user, salt, sessionToken, proof) => {
    // As the daemon's: base64url of 32 random bytes; a password change's is
    // bound to the session and to the auth key that earned it, a recovery's
    // to the recovery proof that opened it.
    const value = Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString('base64url')
    tickets.set(value, { purpose, user, salt, token: sessionToken, proof })
    return value
  }
  const takeTicket = (value, ...purposes) => {
    const ticket = tickets.get(value)
    if (!ticket || !purposes.includes(ticket.purpose)) return undefined
    tickets.delete(value)
    return ticket
  }
  /** What an enrolment stores of what the browser sent (section 5.7). */
  const store = (user, sent, salt) => Object.assign(user, {
    salt, authKey: sent.auth_key, publicKey: sent.public_key ?? user.publicKey, passwordWrap: sent.password_wrap,
    ...(sent.recovery_wrap ? { recoveryWrap: sent.recovery_wrap, recoveryProof: sent.recovery_proof } : {}), password: undefined,
  })
  const endSessions = user => { for (const [value, other] of sessions) { if (other.user_id === user.id) sessions.delete(value) } }
  /** A person's personal workspace, as the daemon names it. */
  const personalOf = user => `wsp_${user.id.slice(4)}`
  /** A key's mailbox, as listed (service.KeyMailbox). */
  const presentHeld = item => ({ account_id: item.account_id, workspace_id: item.workspace_id, read: item.read, act: item.act, send: item.send, ...(item.granted_by ? { granted_by: item.granted_by } : {}), updated_at: item.updated_at })
  /** A key as its workspace lists it (service.WorkspaceKey): sends only with the send scope, on a server whose keys send. */
  const presentKey = key => {
    const { mailboxes, revoked_at: revoked, last_used_at: used, ...rest } = key
    return { ...rest, mailboxes: mailboxes.map(presentHeld), ...(revoked ? { revoked_at: revoked } : {}), ...(used ? { last_used_at: used } : {}), live: !revoked && key.expires_at > now(), sends: key.scope === 'send' && keysSend }
  }
  const extension = extend?.({ users, calls, accountsByUser, journal, allMail, versions, emit, owner, present, indexedFolders, keys, keySends, presentKey }) ?? {}
  return {
    ...extension.methods,
    calls, sessions, flows, accountsByUser, journal, recheck, policy, keys, keySends, users,
    /** The person ticks Show in IMAP for All Mail in Gmail's settings; the daemon notices at its next look at the folders. */
    showAllMail() { allMail.listed = true },
    /** New mail arriving in an account's inbox, as the engine would journal it. */
    deliver(accountID, subject, from) {
      const user = owner(accountID)
      const account = accountsByUser.get(user.id).find(item => item.id === accountID)
      account.sync.messages++
      emit(user, 'message.new', accountID, { message_id: account.sync.messages, folder_id: 1, folder_role: 'inbox', subject, from, internal_date: now(), first_copy: true, first_inbox_copy: true })
    },
    close() { closed = true; for (const ticker of tickers.values()) clearInterval(ticker) },
    async handle(route) {
      const request = route.request()
      const url = new URL(request.url())
      const path = url.pathname
      const method = request.method()
      const json = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', headers: { 'Cache-Control': 'no-store' }, body: JSON.stringify(body) })
      const fail = (status, code) => json({ code, message: `synthetic ${code}: detail the console must never show` }, status)
      const body = () => { try { return request.postDataJSON() ?? {} } catch { return {} } }
      // The key scheme's ceremonies (docs/key-scheme.md section 12): what
      // the daemon checks and stores, never a password.
      if (path.startsWith('/v1/auth/')) calls.auth.push({ path, body: body() })
      const named = method === 'POST' && body().email !== undefined ? await enrolled(users.get(String(body().email).trim().toLowerCase())) : undefined
      if (path === '/v1/auth/challenge' && method === 'POST') {
        calls.challenge++
        if (named?.authKey) return json({ salt: named.salt, kdf: KDF })
        return json({ salt: targetOf(body().email), kdf: KDF })
      }
      if (path === '/v1/auth/login' && method === 'POST') {
        calls.login++
        if (!named?.authKey || body().auth_key !== named.authKey) return fail(401, 'unauthorized')
        return json({ ...issue(named), password_wrap: named.passwordWrap })
      }
      if (path === '/v1/auth/signup/open' && method === 'POST') {
        const { invite, email } = body()
        if (invite !== INVITE || email !== 'new@example.test') return fail(403, 'not_authorized')
        inviteSeal ||= crypto.randomUUID()
        return json({ salt: targetOf(email), kdf: KDF, seal_id: inviteSeal })
      }
      if (path === '/v1/auth/signup' && method === 'POST') {
        const { invite, email, name, seal_id: sealID } = body()
        if (invite !== INVITE || email !== 'new@example.test') return fail(403, 'not_authorized')
        if (!inviteSeal || sealID !== inviteSeal) return fail(409, 'conflict')
        const user = { id: 'usr_00000000000000b2', seal_id: sealID, email, name, role: 'member', created_at: now(), consent: 0 }
        store(user, body(), targetOf(email))
        users.set(email, user)
        accountsByUser.set(user.id, [])
        return json(issue(user), 201)
      }
      if (path === '/v1/auth/reset/open' && method === 'POST') {
        const { reset, email } = body()
        if (resets.get(reset) !== email || !named) return fail(403, 'not_authorized')
        return json({ salt: targetOf(email), kdf: KDF, seal_id: publicUser(named).seal_id })
      }
      if (path === '/v1/auth/reset' && method === 'POST') {
        const { reset, email } = body()
        if (resets.get(reset) !== email || !named) return fail(403, 'not_authorized')
        resets.delete(reset)
        named.seal_id = publicUser(named).seal_id
        store(named, body(), targetOf(email))
        endSessions(named)
        return json(issue(named))
      }
      if (path === '/v1/auth/recover/open' && method === 'POST') {
        if (!named?.recoveryProof || body().recovery_proof !== named.recoveryProof) return fail(401, 'unauthorized')
        const salt = targetOf(named.email)
        return json({ seal_id: named.seal_id, public_key: named.publicKey, recovery_wrap: named.recoveryWrap, salt, kdf: KDF, ticket: ticketOf('recover', named, salt, undefined, named.recoveryProof) })
      }
      if (path === '/v1/auth/recover/finish' && method === 'POST') {
        // Only with the proof that opened the recovery, which stays its own otherwise.
        const held = tickets.get(body().ticket)
        if (!held?.proof || held.proof !== body().current_recovery_proof) return fail(403, 'not_authorized')
        const ticket = takeTicket(body().ticket, 'recover')
        if (!ticket) return fail(403, 'not_authorized')
        store(ticket.user, body(), ticket.salt)
        endSessions(ticket.user)
        return route.fulfill({ status: 204 })
      }
      const header = request.headers().authorization ?? ''
      const session = sessions.get(header.replace(/^Bearer /, ''))
      if (!session || session.expires_at <= now()) return fail(401, 'unauthorized')
      const user = [...users.values()].find(item => item.id === session.user_id)
      const mine = accountsByUser.get(user.id) ?? []
      // An edition's own routes first. Its handler answers synchronously
      // with undefined for a request it leaves to the core.
      const own = extension.route?.({ route, request, url, path, method, json, fail, body, header, user, mine })
      if (own !== undefined) return own
      if (path === '/v1/auth/me') { calls.me++; return json({ user: publicUser(user), session: { id: session.id, created_at: session.created_at, expires_at: session.expires_at, authenticated_at: session.authenticated_at } }) }
      if (path === '/v1/auth/logout') {
        if (body().everywhere) for (const [value, other] of sessions) { if (other.user_id === user.id) sessions.delete(value) }
        else sessions.delete(header.replace(/^Bearer /, ''))
        return route.fulfill({ status: 204 })
      }
      if (path === '/v1/auth/profile' && method === 'PUT') { user.name = body().name; return json(publicUser(user)) }
      if (path === '/v1/auth/password/begin' && method === 'POST') {
        if (!user.authKey || body().current_auth_key !== user.authKey) return fail(403, 'not_authorized')
        const salt = targetOf(user.email)
        return json({ password_wrap: user.passwordWrap, salt, kdf: KDF, ticket: ticketOf('password', user, salt, header.replace(/^Bearer /, ''), user.authKey) })
      }
      if (path === '/v1/auth/password/finish' && method === 'POST') {
        // Only with the auth key that earned the ticket, which stays its own otherwise.
        const held = tickets.get(body().ticket)
        if (!held || held.token !== header.replace(/^Bearer /, '') || held.proof !== body().current_auth_key) return fail(403, 'not_authorized')
        const ticket = takeTicket(body().ticket, 'password', 'rederive')
        if (!ticket) return fail(403, 'not_authorized')
        store(user, body(), ticket.salt)
        if (ticket.purpose === 'rederive') return route.fulfill({ status: 204 })
        // A change ends every session; the new one is as old as this one's step-up.
        endSessions(user)
        return json(issue(user, session.authenticated_at))
      }
      if (path === '/v1/auth/stepup' && method === 'POST') {
        calls.stepUp++
        if (body().auth_key !== user.authKey) return fail(403, 'not_authorized')
        session.authenticated_at = now()
        return json({ authenticated_at: session.authenticated_at })
      }
      if (path === '/v1/auth/recovery' && method === 'POST') {
        // The current auth key, in this request: the session alone sets no secret.
        if (!user.authKey || body().current_auth_key !== user.authKey) return fail(403, 'not_authorized')
        calls.recovery++
        Object.assign(user, { recoveryWrap: body().recovery_wrap, recoveryProof: body().recovery_proof })
        return route.fulfill({ status: 204 })
      }
      if (path === '/v1/events' && method === 'GET') return stream()
      if (path === '/v1/me/sync-consent') {
        if (method === 'POST') {
          if (body().version !== policy.version) return fail(400, 'bad_request')
          calls.consent.push('grant')
          user.consentVersion = policy.version
          if (!user.consent) {
            user.consent = now()
            for (const account of mine) if (account.state === 'active') startSync(user, account)
          }
        } else if (method === 'DELETE') {
          calls.consent.push('withdraw')
          user.consent = 0
          for (const account of mine) { clearInterval(tickers.get(account.id)); account.sync = idle() }
          // The index goes, and the change log of these mailboxes with it.
          for (let i = journal.length - 1; i >= 0; i--) if (journal[i].user === user.id) journal.splice(i, 1)
        }
        return json(user.consent
          ? { consented: true, consented_at: user.consent, current_version: policy.version, version: user.consentVersion }
          : { consented: false, current_version: policy.version })
      }
      if (personal && path === '/v1/workspaces' && method === 'GET') {
        return json([{ id: personalOf(user), kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: user.created_at }])
      }
      if (path === '/v1/me/apikeys' || path.startsWith('/v1/me/apikeys/') || /^\/v1\/workspaces\/[^/]+\/apikeys(\/|$)/.test(path)) return apiKeys()
      // GET /v1/me/mcp (service.MCPAccess): whether /mcp is served, and whether keys may send, the same for every caller.
      if (path === '/v1/me/mcp' && method === 'GET') { calls.mcp++; return json({ http: mcpHTTP, keys_send: keysSend }) }
      // GET /v1/me/storage (service.Storage): the caller's mailboxes by address, what the index holds
      // for each, and the database's size for an owner. The index is the owner's to keep while sync
      // is on, whether or not the account works now: zeros only once sync is off, which deletes it.
      if (path === '/v1/me/storage' && method === 'GET') {
        calls.storage++
        const mailboxes = mine.map(account => {
          const messages = user.consent ? account.sync.messages : 0
          return { account_id: account.id, email: account.email, messages, bytes: messages * 3200 }
        }).sort((a, b) => a.email < b.email ? -1 : a.email > b.email ? 1 : 0)
        const total = mailboxes.reduce((sum, item) => ({ messages: sum.messages + item.messages, bytes: sum.bytes + item.bytes }), { messages: 0, bytes: 0 })
        return json({ mailboxes, total, ...(user.role === 'owner' ? { database_bytes: 48_234_496 } : {}) })
      }
      if (path === '/v1/me/actions-consent') {
        if (method === 'POST') {
          if (body().version !== versions.actions) return fail(400, 'bad_request')
          calls.actionsConsent.push('grant')
          user.actionsVersion = versions.actions
          user.actions ||= now()
        } else if (method === 'DELETE') {
          // Nothing was stored by actions: turning them off only stops them.
          calls.actionsConsent.push('withdraw')
          user.actions = 0
          user.actionsVersion = ''
        }
        return json(user.actions
          ? { consented: true, consented_at: user.actions, current_version: versions.actions, version: user.actionsVersion }
          : { consented: false, current_version: versions.actions })
      }
      const syncMatch = path.match(/^\/v1\/accounts\/([^/]+)\/sync$/)
      if (syncMatch && method === 'POST') {
        const account = mine.find(item => item.id === syncMatch[1])
        if (!account) return fail(404, 'not_found')
        if (!user.consent || account.state !== 'active') return fail(409, 'conflict')
        calls.syncNow.push(account.id)
        // A pass reads the folder list again: All Mail, shown in the meantime, joins the index.
        if (account.id === GMAIL_ACCOUNT && allMail.listed !== allMail.indexed) {
          allMail.indexed = allMail.listed
          emit(user, 'folder.changed', account.id, { folder_id: 4, name: '[Gmail]/All Mail', role: 'all', change: allMail.indexed ? 'added' : 'removed', count: 0 })
        }
        return json(present(account).sync, 202)
      }
      if (path === '/v1/providers') return json(providers)
      if (path === '/v1/accounts' && method === 'GET') return json(mine.map(present))
      if (path === '/v1/accounts' && method === 'POST') {
        const request_ = body()
        calls.created.push(request_)
        if (mine.some(item => item.email === request_.email)) return fail(409, 'conflict')
        const password = request_.provider === 'imap' || request_.provider === 'icloud'
        // The daemon fills in Apple's servers itself and refuses any sent for iCloud.
        if (request_.provider === 'icloud' && ['imap_host', 'imap_port', 'smtp_host', 'smtp_port', 'smtp_tls'].some(key => key in request_)) return fail(400, 'bad_request')
        // A person's link carries the mailbox's first key and their own grant (docs/key-scheme.md section 12.11).
        if (!request_.public_key || !request_.namespace || !request_.grant) return fail(400, 'bad_request')
        const account = { id: `acc_${String(++accountSerial).padStart(16, '0')}`, email: request_.email, provider: request_.provider, auth_kind: password ? 'password' : 'oauth2', state: 'pending_auth', save_sent_copy: password, created_at: now(), sync: idle(), target: 120 }
        account.key = { epoch: 1, public_key: request_.public_key, namespace: request_.namespace, grant: request_.grant }
        if (request_.display_name) account.display_name = request_.display_name
        if (password) {
          await new Promise(done => setTimeout(done, 700))
          if (request_.password !== (request_.provider === 'icloud' ? APPLE_APP_PASSWORD : 'app-password-123')) return fail(400, 'bad_request')
          // As Apple does: a custom-domain address is refused as the sign-in; the account's iCloud address is not.
          if (request_.provider === 'icloud' && !/@(icloud|me|mac)\.com$/i.test(request_.login_user || request_.email)) return fail(400, 'bad_request')
          account.state = 'active'
          mine.push(account)
          if (user.consent) startSync(user, account)
          return json({ account: present(account) }, 201)
        }
        mine.push(account)
        return json({ account: present(account), auth: startFlow(account) }, 201)
      }
      // A mailbox's key (docs/console.md, "Mailbox keys"): the person's own grant, nobody waiting; its first key, and a new one.
      const keyRoute = path.match(/^\/v1\/accounts\/([^/]+)\/mailbox-key$/)
      if (keyRoute) {
        const account = mine.find(item => item.id === keyRoute[1])
        if (!account) return fail(404, 'not_found')
        const pair = () => ({ epoch: account.key.epoch, public_key: account.key.public_key, namespace: account.key.namespace })
        if (method === 'GET') return json({ ...(account.key ? { ...pair(), grant: account.key.grant } : {}), waiting: [], suppliers: [], keyless_readers: [] })
        const sent = body()
        calls.mailboxKeys.push({ account: account.id, method, epoch: sent.epoch ?? 1 })
        if (method === 'POST') {
          if (account.key) return fail(409, 'conflict')
          account.key = { epoch: 1, public_key: sent.public_key, namespace: sent.namespace, grant: sent.grants?.find(grant => grant.user_id === user.id)?.grant }
          return json(pair(), 201)
        }
        if (method === 'PUT') {
          if (!account.key || sent.epoch !== account.key.epoch + 1) return fail(409, 'conflict')
          account.key = { epoch: sent.epoch, public_key: sent.public_key, namespace: account.key.namespace, grant: sent.grant }
          return json(pair())
        }
      }
      const start = path.match(/^\/v1\/accounts\/([^/]+)\/oauth\/start$/)
      if (start && method === 'POST') {
        const account = mine.find(item => item.id === start[1])
        if (!account) return fail(404, 'not_found')
        return json(startFlow(account))
      }
      if (path === '/v1/accounts/oauth/callback' && method === 'POST') {
        const redirect = new URL(body().redirect_url)
        calls.callback.push(redirect.toString())
        const flow = flows.get(redirect.searchParams.get('state'))
        if (!flow || flow.user !== user.id) return fail(404, 'not_found')
        flows.delete(redirect.searchParams.get('state'))
        const account = mine.find(item => item.id === flow.account)
        if (redirect.searchParams.get('error')) { transition(account, 'error'); return fail(400, 'bad_request') }
        transition(account, 'active')
        return json(present(account))
      }
      const folderMatch = path.match(/^\/v1\/accounts\/([^/]+)\/folders$/)
      if (folderMatch) {
        const account = mine.find(item => item.id === folderMatch[1])
        if (!account) return fail(404, 'not_found')
        // Once sync has listed the folders, the index answers, and quickly.
        if (user.consent && account.state === 'active' && account.sync.state !== 'off') return json(indexedFolders(account))
        await new Promise(done => setTimeout(done, 400))
        // As the daemon does when the mail server refuses a working account's
        // token: the account needs authorizing again, and the listing is a 409.
        if (refuseFolders.includes(account.id)) {
          account.state_reason = REASON_TOKEN_REJECTED
          transition(account, 'needs_reauth')
          return fail(409, 'conflict')
        }
        return json(folders)
      }
      const one = path.match(/^\/v1\/accounts\/([^/]+)$/)
      if (one) {
        const index = mine.findIndex(item => item.id === one[1])
        if (index < 0) return fail(404, 'not_found')
        if (method === 'DELETE') {
          // As the daemon does: nothing is removed unless the id is repeated.
          if (url.searchParams.get('confirm') !== one[1]) return fail(400, 'bad_request')
          calls.removed.push(mine[index].email)
          mine.splice(index, 1)
          return route.fulfill({ status: 204 })
        }
        const account = mine[index]
        calls.polls++
        // A loopback consent "completes" on the third poll, or fails on the
        // first when the grant it produced does not reach the mailbox.
        const flow = [...flows.values()].find(item => item.account === account.id && item.flow === 'loopback')
        if (flow?.refuse) { account.state_reason = flow.refuse; flows.delete(flow.state); transition(account, 'error') }
        else if (flow && ++flow.polls >= 3) { flows.delete(flow.state); transition(account, 'active') }
        return json(present(account))
      }
      return fail(404, 'not_found')

      /** A state change, journaled as the daemon does, and the first sync of a mailbox that became active under consent. */
      function transition(account, state) {
        const previous = account.state
        if (previous === state) return
        account.state = state
        emit(user, 'account.state', account.id, { state, previous_state: previous })
        if (state === 'active' && user.consent) startSync(user, account)
      }

      /**
       * The event stream. A route can only answer with a whole body, so each
       * connection is held until there is something to send (or 8 s pass),
       * answered with it, and closed; the console reconnects with
       * Last-Event-ID, which is what this checks. Without a cursor a stream
       * starts from now, as the daemon's does.
       */
      async function stream() {
        const headers = request.headers()
        const cursor = headers['last-event-id']
        const entry = { lastEventID: cursor ?? '', authorization: headers.authorization ?? '', url: request.url(), type: request.resourceType(), delivered: 0 }
        calls.streams.push(entry)
        const from = cursor !== undefined ? Number(cursor) : seq
        const token = header.replace(/^Bearer /, '')
        const deadline = Date.now() + 8_000
        let text = ': connected\n\n'
        for (;;) {
          const pending = journal.filter(event => event.user === user.id && event.seq > from)
          if (pending.length) {
            text += pending.map(({ user: _, ...event }) => `id: ${event.seq}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join('')
            entry.delivered = pending.at(-1).seq
            break
          }
          if (recheck.streams && !sessions.has(token)) {
            text += `event: error\ndata: ${JSON.stringify({ code: 'unauthorized', message: 'synthetic unauthorized: detail the console must never show' })}\n\n`
            break
          }
          if (closed || Date.now() >= deadline) { text += ': ping\n\n'; break }
          await sleep(100)
        }
        try { await route.fulfill({ status: 200, contentType: 'text/event-stream', headers: { 'Cache-Control': 'no-store' }, body: text }) }
        catch { entry.delivered = 0 /* The page went away, or cancelled the request, first. */ }
      }

      /**
       * internal/service workspacekeys.go: a workspace's keys are its owners'
       * and admins', signed in; read only where the person giving it reads
       * the mailbox, act with read and a scope that acts, send with the send
       * scope on a server whose keys send; the current text; at most 20 live
       * keys per workspace; the keys a person created are theirs to list and
       * revoke; POST /v1/me/apikeys is refused.
       */
      function apiKeys() {
        const live = key => !key.revoked_at && key.expires_at > now()
        const ordered = list => [...list].sort((a, b) => Number(live(b)) - Number(live(a)) || b.created_at - a.created_at)
        const revoke = key => { key.revoked_at ||= now() }
        if (path === '/v1/me/apikeys' && method === 'GET') return json(ordered(keys.filter(key => key.created_by === user.id)).map(presentKey))
        if (path === '/v1/me/apikeys' && method === 'POST') return fail(400, 'bad_request')
        const own = path.match(/^\/v1\/me\/apikeys\/([^/]+)$/)
        if (own && method === 'DELETE') {
          calls.keys.push({ method, path })
          const key = keys.find(item => item.prefix === own[1] && item.created_by === user.id)
          if (!key) return fail(404, 'not_found')
          revoke(key)
          return route.fulfill({ status: 204 })
        }
        const match = path.match(/^\/v1\/workspaces\/([^/]+)\/apikeys(?:\/([^/]+)(?:\/(accounts|sends)(?:\/([^/]+))?)?)?$/)
        if (!match) return fail(404, 'not_found')
        const [, id, prefix, part, accountID] = match
        const place = extension.keyWorkspace?.(user, id)
          ?? (personal && id === personalOf(user) ? { admin: true, mailboxes: mine.map(account => ({ id: account.id, reads: true })) } : null)
        if (!place) return fail(404, 'not_found')
        if (!place.admin) return fail(403, 'not_authorized')
        const inPlace = accountID_ => place.mailboxes.find(item => item.id === accountID_)
        const acts = scope => scope === 'write' || scope === 'send'
        /** What flags given to a key must be: one at least, act with read and a scope that acts, send with send where keys send. */
        const refusal = (scope, flags) => {
          if (!flags.read && !flags.act && !flags.send) return [400, 'bad_request']
          if (flags.act && !flags.read) return [400, 'bad_request']
          if (flags.send && !keysSend) return [403, 'not_authorized']
          if ((flags.act && !acts(scope)) || (flags.send && scope !== 'send')) return [400, 'bad_request']
          return null
        }
        if (!prefix && method === 'GET') return json(ordered(keys.filter(key => key.workspace_id === id)).map(presentKey))
        if (!prefix && method === 'POST') {
          const request_ = body()
          calls.keys.push({ method, path, body: request_ })
          if (request_.terms_version !== versions.keys) return fail(409, 'conflict')
          if (!String(request_.name ?? '').trim() || ![30, 90, 365].includes(request_.ttl_days)) return fail(400, 'bad_request')
          if (!['read', 'write', 'send'].includes(request_.scope) || (request_.scope === 'send' && !keysSend)) return fail(400, 'bad_request')
          const given = request_.mailboxes ?? []
          for (const item of given) {
            const mailbox = inPlace(item.account_id)
            if (!mailbox) return fail(404, 'not_found')
            const refused = refusal(request_.scope, item)
            if (refused) return fail(...refused)
            if (item.read && !mailbox.reads) return fail(403, 'not_authorized')
          }
          if (keys.filter(key => key.workspace_id === id && live(key)).length >= 20) return fail(409, 'conflict')
          const newPrefix = [...crypto.getRandomValues(new Uint8Array(8))].map(byte => byte.toString(16).padStart(2, '0')).join('')
          const secret = Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString('base64url')
          const key = {
            prefix: newPrefix, name: String(request_.name).trim(), scope: request_.scope, workspace_id: id,
            mailboxes: given.map(item => ({ account_id: item.account_id, workspace_id: id, read: !!item.read, act: !!item.act, send: !!item.send, granted_by: user.id, updated_at: now() })),
            created_by: user.id, created_at: now(), expires_at: now() + request_.ttl_days * 86400, terms_version: request_.terms_version,
          }
          keys.unshift(key)
          calls.keys.at(-1).key = `${newPrefix}.${secret}`
          return json({ ...presentKey(key), key: `${newPrefix}.${secret}` }, 201)
        }
        const key = keys.find(item => item.prefix === prefix && item.workspace_id === id)
        if (!key) return fail(404, 'not_found')
        if (!part && method === 'DELETE') {
          calls.keys.push({ method, path })
          revoke(key)
          return route.fulfill({ status: 204 })
        }
        if (part === 'sends' && !accountID && method === 'GET') return json(keySends.get(prefix) ?? [])
        if (part === 'accounts' && accountID) {
          const mailbox = inPlace(accountID)
          if (!mailbox) return fail(404, 'not_found')
          const index = key.mailboxes.findIndex(item => item.account_id === accountID)
          if (method === 'DELETE') {
            calls.keys.push({ method, path })
            if (index < 0) return fail(404, 'not_found')
            key.mailboxes.splice(index, 1)
            return route.fulfill({ status: 204 })
          }
          if (method === 'PUT') {
            const flags = body()
            calls.keys.push({ method, path, body: flags })
            if (![flags.read, flags.act, flags.send].every(flag => typeof flag === 'boolean')) return fail(400, 'bad_request')
            if (!live(key)) return fail(409, 'conflict')
            const refused = refusal(key.scope, flags)
            if (refused) return fail(...refused)
            const before = key.mailboxes[index]
            if (flags.read && !before?.read && !mailbox.reads) return fail(403, 'not_authorized')
            const held = { account_id: accountID, workspace_id: id, read: flags.read, act: flags.act, send: flags.send, granted_by: user.id, updated_at: now() }
            if (index < 0) key.mailboxes.push(held)
            else key.mailboxes.splice(index, 1, held)
            return json(presentHeld(held))
          }
        }
        return fail(404, 'not_found')
      }

      function startFlow(account) {
        const choice = providers.find(item => item.id === account.provider)
        const flow = choice.flows.includes('web') ? 'web' : 'loopback'
        const state = `state_${String(++serial).padStart(20, '0')}`
        flows.set(state, { state, account: account.id, user: user.id, flow, polls: 0, refuse: account.email === NOT_GRANTED_EMAIL ? REASON_NOT_GRANTED : '' })
        // A loopback consent also finishes on its own after a few seconds, as
        // a person at Google's screen would; the account.state event it
        // journals is what ends the console's wait when its stream is open.
        if (flow === 'loopback' && account.email !== NOT_GRANTED_EMAIL) setTimeout(() => {
          if (!flows.has(state) || !mine.includes(account)) return
          flows.delete(state)
          transition(account, 'active')
        }, 5_000)
        const authURL = account.provider === 'gmail'
          ? `https://accounts.google.com/o/oauth2/v2/auth?client_id=synthetic&state=${state}&redirect_uri=http%3A%2F%2F127.0.0.1%3A53123%2Foauth%2Fcallback`
          : `https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=synthetic&state=${state}&redirect_uri=${encodeURIComponent(origin + '/oauth/return')}`
        return { flow, auth_url: authURL, state, expires_at: now() + 600 }
      }
    },
  }
}


/** What has focus, as a short description: tag, name or class, and text. */
export const focused = page => page.evaluate(() => {
  const element = document.activeElement
  if (!element || element === document.body) return 'body'
  return `${element.tagName.toLowerCase()}${element.getAttribute('name') ? `[name=${element.getAttribute('name')}]` : ''}${element.className ? `.${String(element.className).split(' ').join('.')}` : ''} ${element.textContent?.trim().slice(0, 40) ?? ''}`.trim()
})

/** The text of the page's polite live regions, which a screen reader would have been told. */
export const spoken = page => page.evaluate(() => [...document.querySelectorAll('[role=status][aria-live=polite]')].map(element => element.textContent).join(' | '))

export async function noHorizontalOverflow(page, where) {
  const offenders = await page.evaluate(() => [...document.querySelectorAll('body *')].filter(element => {
    const box = element.getBoundingClientRect()
    return box.width > 0 && box.height > 0 && box.right > window.innerWidth + 1 && getComputedStyle(element).visibility !== 'hidden'
  }).slice(0, 5).map(element => `${element.tagName.toLowerCase()}.${[...element.classList].join('.')}`))
  assert.deepEqual(offenders, [], `${where}: elements overflow the viewport horizontally`)
}


/** Waits for check() to hold (it may be async), polling, or fails naming what it waited for. */
export async function until(check, what, timeout = 15_000) {
  const end = Date.now() + timeout
  while (!(await check())) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`)
    await sleep(50)
  }
}
