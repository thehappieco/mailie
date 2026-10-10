// The HTTP contract, checked against what the Go handlers actually answer.
//
// `go test ./internal/api -run Contract -update` writes these fixtures from the
// real handlers; this spec holds them to the types the console is written
// against, in strict mode (unknown keys fail too, so a rename on either side
// fails here instead of on a screen). The fixtures are the Go side's to
// write: a missing one is a failure, never something to create from here.
// The handlers run with the daemon's default consent revisions, which are the
// open console's texts (src/open/versions.ts). The fixtures of the routes the
// open console never calls (messages, actions on them, sending) are checked
// by the edition that reads them; a key's sends, which its workspace's owners
// and admins list, are the console's.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { EventStreamParser } from '../src/api/events'
import { fromBase64URL } from '@thehappieco/kit/bytes'
import { checkKDF } from '../src/crypto/mailie'
import {
  isChallenge, isLoginReply, isOpening, isPasswordBegin, isRecoverOpen, isStepUpReply, enrolled,
  type Challenge, type LoginReply, type Opening, type RecoverOpen,
  isAccount, isAccountSync, isAddAccountResult, isAuthFlow, isCreatedKey, isErrorBody, isFolder, isGrant, isInvite, isKeyMailbox, isKeySendList,
  isMailboxAccessList, isMe, isMember, isMemberList, isMessageNew, isMcpAccess, isProviderList, isServerEvent, isSessionReply, isStorage,
  isSyncConsent, isTeamInvite, isTeamInviteList, isToken, isUser, isWaitResult, isWorkspace, isWorkspaceKeyList, isWorkspaceList, hasPassword,
  isGrantText, isMailboxKeyPair, isMailboxKeyState, isSealedGrant, isSealIDText, readsNow,
  type Account, type AccountSync, type ActionsConsent, type AddAccountResult, type AuthFlow, type CreatedKey, type Folder, type Grant, type Invite,
  type KeyMailbox, type KeySend, type MailboxAccess, type MailboxKeyPair, type MailboxKeyState, type McpAccess, type Me, type Member, type Provider,
  type SealedGrant, type ServerEvent, type SessionReply, type Storage, type SyncConsent, type TeamInvite, type User, type WaitResult, type Workspace,
  type WorkspaceKey,
} from '../src/api/types'
import { invitationLink } from '../src/api/workspaces'
import { grantChange } from '../src/ui/access'
import { heldOn, listed } from '../src/api/apikeys'
import { keyStanding, scopeActs } from '../src/ui/apikeys'
import { ACTIONS_TEXT_VERSION, KEY_TERMS_VERSION, SEND_TEXT_VERSION, SYNC_TEXT_VERSION as CONSENT_TEXT_VERSION } from '../src/open/versions'
import { signupLink } from '../src/ui/signupLink'

const directory = new URL('../../internal/api/testdata/contract/', import.meta.url)

function fixture(name: string): unknown {
  const path = fileURLToPath(new URL(`${name}.json`, directory))
  let text: string
  try { text = readFileSync(path, 'utf8') } catch {
    throw new Error(`missing contract fixture ${path}: run \`go test ./internal/api -run Contract -update\` on the Go side`)
  }
  return JSON.parse(text)
}

const shapes: [string, (value: unknown) => boolean][] = [
  ['session', value => isSessionReply(value, true)],
  ['login', value => isLoginReply(value, true)],
  ['login_rederive', value => isLoginReply(value, true)],
  ['challenge', value => isChallenge(value, true)],
  ['signup_open', value => isOpening(value, true)],
  ['reset_open', value => isOpening(value, true)],
  ['password_begin', value => isPasswordBegin(value, true)],
  ['recover_open', value => isRecoverOpen(value, true)],
  ['stepup', value => isStepUpReply(value, true)],
  ['me', value => isMe(value, true)],
  ['me_without_password', value => isMe(value, true)],
  ['user', value => isUser(value, true)],
  ['account', value => isAccount(value, true)],
  ['account_icloud', value => isAccount(value, true)],
  ['add_account', value => isAddAccountResult(value, true)],
  ['auth_flow_web', value => isAuthFlow(value, true)],
  ['auth_flow_loopback', value => isAuthFlow(value, true)],
  ['providers', value => isProviderList(value, true)],
  ['invite', value => isInvite(value, true)],
  ['error', value => isErrorBody(value, true)],
  ['sync_consent', value => isSyncConsent(value, true)],
  ['sync_consent_given', value => isSyncConsent(value, true)],
  ['sync_status', value => isAccountSync(value, true)],
  ['sync_triggered', value => isAccountSync(value, true)],
  ['account_syncing', value => isAccount(value, true)],
  ['event', value => isServerEvent(value, true)],
  ['events_wait', value => isWaitResult(value, true)],
  ['folders_indexed', value => Array.isArray(value) && value.every(item => isFolder(item, true))],
  ['actions_consent', value => isSyncConsent(value, true)],
  ['actions_consent_given', value => isSyncConsent(value, true)],
  ['apikeys', value => isWorkspaceKeyList(value, true)],
  ['my_apikeys', value => isWorkspaceKeyList(value, true)],
  ['apikey_created', value => isCreatedKey(value, true)],
  ['key_access', value => isKeyMailbox(value, true)],
  ['key_sends', value => isKeySendList(value, true)],
  ['send_consent', value => isSyncConsent(value, true)],
  ['send_consent_given', value => isSyncConsent(value, true)],
  ['storage', value => isStorage(value, true)],
  ['mcp', value => isMcpAccess(value, true)],
  ['workspaces', value => isWorkspaceList(value, true)],
  ['workspace_created', value => isWorkspace(value, true)],
  ['invite_accepted', value => isWorkspace(value, true)],
  ['members', value => isMemberList(value, true)],
  ['member', value => isMember(value, true)],
  ['team_invite', value => isTeamInvite(value, true)],
  ['team_invites', value => isTeamInviteList(value, true)],
  ['access', value => isMailboxAccessList(value, true)],
  ['grant', value => isGrant(value, true)],
  ['account_waiting', value => isAccount(value, true)],
  ['mailbox_key', value => isMailboxKeyState(value, true)],
  ['mailbox_key_waiting', value => isMailboxKeyState(value, true)],
  ['mailbox_key_keyless', value => isMailboxKeyState(value, true)],
  ['grant_supplied', value => isSealedGrant(value, true)],
  ['first_key', value => isMailboxKeyPair(value, true)],
  ['new_key', value => isMailboxKeyPair(value, true)],
]

describe('the HTTP contract the Go handlers answer with', () => {
  it.each(shapes)('%s.json has exactly the shape the console reads', (name, valid) => {
    const value = fixture(name)
    expect(valid(value), JSON.stringify(value, null, 2)).toBe(true)
  })

  it('says whether the person has a password: everyone signed up with one, nobody who signs in only another way', () => {
    expect((fixture('session') as SessionReply).user.has_password).toBe(true)
    expect((fixture('me') as Me).user.has_password).toBe(true)
    expect((fixture('user') as User).has_password).toBe(true)
    const external = fixture('me_without_password') as Me
    expect(external.user.has_password).toBe(false)
    expect(hasPassword(external.user)).toBe(false)
    expect(hasPassword((fixture('me') as Me).user)).toBe(true)
    // A session of the length the extension asked for, never a password's fourteen days.
    expect(external.session.expires_at - external.session.created_at).toBe(86_400)
  })

  it('keeps a plain session apart from a sign-in’s answer, which alone carries the password wrap', () => {
    const plain = fixture('session') as SessionReply
    expect(plain).not.toHaveProperty('password_wrap')
    expect(isLoginReply(plain, true)).toBe(false)
    expect(isSessionReply(fixture('login'), true)).toBe(false)
    expect(plain.authenticated_at).toBeGreaterThan(0)
    expect(enrolled(plain.user)).toBe(true)
    // A person who signs in only another way has a seal id, and no account key yet.
    const external = fixture('me_without_password') as Me
    expect(external.user.seal_id).toBeTruthy()
    expect(enrolled(external.user)).toBe(false)
  })

  it('names every salt and parameter set a browser derives under as one it accepts', () => {
    const targets: { salt: string; kdf: unknown }[] = [
      fixture('challenge') as Challenge, fixture('signup_open') as Opening, fixture('reset_open') as Opening,
      fixture('recover_open') as RecoverOpen,
      (fixture('login_rederive') as LoginReply).rederive!,
    ]
    for (const target of targets) expect(() => checkKDF(target.kdf, fromBase64URL(target.salt, 16))).not.toThrow()
    // The challenge answers a salt and parameters, and nothing more: the upgrade's answer left with it.
    expect(Object.keys(fixture('challenge') as Challenge).sort()).toEqual(['kdf', 'salt'])
    expect(isChallenge({ ...(fixture('challenge') as Challenge), upgrade: true }, true)).toBe(false)
    expect(fixture('login')).not.toHaveProperty('rederive')
  })

  it('tells every enrolment the seal id it binds the wraps to before it seals one', () => {
    for (const name of ['signup_open', 'reset_open', 'recover_open']) expect((fixture(name) as Opening).seal_id, name).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('a session token is an opaque bearer, not an API key', () => {
    const { token } = fixture('session') as SessionReply
    expect(isToken(token)).toBe(true)
    expect(token).not.toContain('.')
  })

  it('every flow says which flow it is', () => {
    expect((fixture('auth_flow_loopback') as AuthFlow).flow).toBe('loopback')
    expect((fixture('add_account') as AddAccountResult).auth?.flow).toMatch(/^(web|loopback|device|pasted)$/)
  })

  it('a web flow leaves for the provider over https and comes back to the console route', () => {
    const web = fixture('auth_flow_web') as AuthFlow
    expect(web.flow).toBe('web')
    expect(new URL(web.auth_url!).protocol).toBe('https:')
    expect(decodeURIComponent(web.auth_url!)).toContain('/oauth/return')
  })

  it('lists the providers in order, with iCloud and IMAP by password only and Microsoft never by password', () => {
    const providers = fixture('providers') as Provider[]
    expect(providers.map(provider => provider.id)).toEqual(['gmail', 'microsoft', 'icloud', 'imap'])
    expect(providers[2]).toEqual({ id: 'icloud', oauth: false, password: true, flows: [] })
    expect(providers[3]).toMatchObject({ oauth: false, password: true, flows: [] })
    expect(providers[1]!.password).toBe(false)
  })

  it('names no linker on a mailbox: who connected one is the access directory’s record, never the card’s', () => {
    for (const name of ['account', 'account_icloud', 'account_syncing']) expect(fixture(name), name).not.toHaveProperty('linked_by')
    expect((fixture('add_account') as AddAccountResult).account).not.toHaveProperty('linked_by')
    expect(isAccount({ ...(fixture('account') as Account), linked_by: 'usr_0000000000000001' }, true)).toBe(false)
  })

  it('takes iCloud as a provider of its own, as the server presents an IMAP account on Apple’s servers', () => {
    const stored = fixture('account') as Account
    expect(isAccount({ ...stored, provider: 'icloud' }, true)).toBe(true)
    expect(isAccount({ ...stored, provider: 'yahoo' }, true)).toBe(false)
    // The handler's own answer for an account stored as imap on imap.mail.me.com.
    const icloud = fixture('account_icloud') as Account
    expect(icloud.provider).toBe('icloud')
    expect(icloud.auth_kind).toBe('password')
  })

  it('an invitation link carries its code in the fragment, where the console reads it', () => {
    const invite = fixture('invite') as Invite
    const url = new URL(invite.url)
    expect(url.search).toBe('')
    const linked = signupLink(invite.url)
    expect(linked.invite).not.toBe('')
    expect(linked.email).toBe(invite.email)
  })

  it('asks for consent to the revision the open console’s sync text is, and says when it was given', () => {
    const before = fixture('sync_consent') as SyncConsent
    const after = fixture('sync_consent_given') as SyncConsent
    expect(before).toEqual({ consented: false, current_version: CONSENT_TEXT_VERSION })
    expect(after.consented).toBe(true)
    expect(after.version).toBe(after.current_version)
    expect(after.consented_at).toBeGreaterThan(0)
  })

  it('shows the consent text of the very revision the daemon asks about', () => {
    // The console agrees to the revision its own text is. A daemon whose
    // default (config.DefaultSyncConsentVersion) moved without a new
    // open/SyncText.vue (and SYNC_TEXT_VERSION with it) would ask people to
    // reload into the same old text, so the build fails here instead.
    expect((fixture('sync_consent') as SyncConsent).current_version).toBe(CONSENT_TEXT_VERSION)
    expect((fixture('sync_consent_given') as SyncConsent).current_version).toBe(CONSENT_TEXT_VERSION)
  })

  it('carries each account’s sync in the same block the sync route answers, off until consent', () => {
    for (const name of ['account', 'account_icloud']) {
      expect((fixture(name) as Account).sync).toMatchObject({ enabled: false, running: false, state: 'off', messages: 0 })
    }
    expect((fixture('add_account') as AddAccountResult).account.sync.enabled).toBe(false)
    const status = fixture('sync_status') as AccountSync
    expect((fixture('account_syncing') as Account).sync).toEqual(status)
    // A pass on request answers 202 with the status as it stands.
    expect(fixture('sync_triggered')).toEqual(status)
    expect(status.initial_progress).toBeGreaterThanOrEqual(0)
    expect(status.initial_progress).toBeLessThanOrEqual(100)
  })

  it('writes one event the same way in the stream and in the long poll, with message.new’s documented payload', () => {
    const event = fixture('event') as ServerEvent
    const waited = fixture('events_wait') as WaitResult
    expect(waited.events).toEqual([event])
    expect(waited.next_cursor).toBe(event.seq)
    expect(event.type).toBe('message.new')
    expect(event.payload.account_id).toBe(event.account_id)
    expect(isMessageNew(event.payload, true)).toBe(true)
    // As the daemon writes it on the wire (internal/api/events.go), the parser gets it back whole.
    const wire = `id: ${event.seq}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`
    const [message] = new EventStreamParser().push(wire)
    expect(message!.lastEventID).toBe(String(event.seq))
    expect(JSON.parse(message!.data)).toEqual(event)
  })
  it('lists indexed folders with the id a message search filters by', () => {
    const folders = fixture('folders_indexed') as Folder[]
    const synced = folders.filter(folder => folder.synced)
    expect(synced.length).toBeGreaterThan(0)
    for (const folder of folders) {
      expect(folder.id, folder.name).toBeGreaterThan(0)
      expect(folder.sync_state, folder.name).toBeDefined()
    }
    expect(folders.find(folder => folder.role === 'inbox')?.id).toBeGreaterThan(0)
  })

  it('asks for consent to actions on the revision the console’s text describes, apart from sync’s', () => {
    // As with sync: a daemon whose default moved without a new
    // open/ActionsText.vue (and ACTIONS_TEXT_VERSION) fails the build here.
    const before = fixture('actions_consent') as ActionsConsent
    const after = fixture('actions_consent_given') as ActionsConsent
    expect(before).toEqual({ consented: false, current_version: ACTIONS_TEXT_VERSION })
    expect(after.consented).toBe(true)
    expect(after.version).toBe(ACTIONS_TEXT_VERSION)
    expect(after.current_version).toBe(ACTIONS_TEXT_VERSION)
    expect(after.consented_at).toBeGreaterThan(0)
    expect(ACTIONS_TEXT_VERSION).not.toBe(CONSENT_TEXT_VERSION)
  })

  it('says on every account whether it has an archive and a trash to move to', () => {
    for (const name of ['account', 'account_icloud', 'account_syncing']) {
      const actions = (fixture(name) as Account).actions
      expect(actions, name).toBeDefined()
      expect(Object.keys(actions!).sort(), name).toEqual(['archive', 'trash'])
    }
    expect(Object.keys((fixture('add_account') as AddAccountResult).account.actions ?? {}).sort()).toEqual(['archive', 'trash'])
  })

  it('names why there is no archive only with a reason the console knows, and only where there is none', () => {
    const stored = fixture('account') as Account
    const actions = (value: unknown) => isAccount({ ...stored, actions: value }, true)
    expect(actions({ archive: false, trash: true, archive_reason: 'all_mail_hidden' })).toBe(true)
    expect(actions({ archive: false, trash: true, archive_reason: 'folder_gone' })).toBe(false)
    expect(actions({ archive: true, trash: true, archive_reason: 'all_mail_hidden' })).toBe(false)
    // The running console reads a reason it does not know as none, rather than refusing the account.
    expect(isAccount({ ...stored, actions: { archive: false, trash: true, archive_reason: 'folder_gone' } })).toBe(true)
    expect(isAccount({ ...stored, actions: { archive: false, trash: true, archive_reason: 7 } })).toBe(false)
  })

  it('hands a new key’s secret over once, under its own prefix, with the text it was created under, and never lists it', () => {
    const created = fixture('apikey_created') as CreatedKey
    const lists = [fixture('apikeys'), fixture('my_apikeys')] as WorkspaceKey[][]
    expect(created.terms_version).toBe(KEY_TERMS_VERSION)
    expect(created.key.startsWith(`${created.prefix}.`)).toBe(true)
    for (const list of lists) {
      // The list the console keeps is exactly what the handler lists: nothing dropped, and no secret to drop.
      expect(list.length).toBeGreaterThan(0)
      for (const key of list) {
        expect(key, key.prefix).not.toHaveProperty('key')
        expect(listed(key), key.prefix).toEqual(key)
        expect(JSON.stringify(key)).not.toContain(created.key.split('.')[1])
      }
      // Live keys first, then the ones revoked or expired, which stay listed.
      const live = list.map(key => key.live)
      expect(live).toEqual([...live].sort((a, b) => Number(b) - Number(a)))
      expect(live).toContain(false)
      for (const key of list) expect(key.live, key.prefix).toBe(keyStanding(key, 1_790_000_000_000) === 'live')
    }
  })

  it('makes every key its workspace’s, holding only that workspace’s mailboxes, each flag within its scope', () => {
    const keys = [fixture('apikey_created'), ...(fixture('apikeys') as WorkspaceKey[]), ...(fixture('my_apikeys') as WorkspaceKey[])] as WorkspaceKey[]
    const workspaces = new Set((fixture('workspaces') as Workspace[]).map(item => item.id))
    for (const key of keys) {
      expect(key.carried_over, key.prefix).toBeUndefined()
      expect(workspaces.has(key.workspace_id!), key.prefix).toBe(true)
      for (const held of key.mailboxes) {
        expect(held.workspace_id, key.prefix).toBe(key.workspace_id)
        if (held.act) expect(scopeActs(key.scope) && held.read, key.prefix).toBe(true)
        if (held.send) expect(key.scope, key.prefix).toBe('send')
      }
      // The daemon that writes the fixtures lets its keys send: only a key of the send scope can.
      expect(key.sends, key.prefix).toBe(key.scope === 'send')
    }
    // A key carried over from before has no workspace, and only it counts the others.
    const listedKey = (fixture('apikeys') as WorkspaceKey[])[0]!
    const { workspace_id: _, ...carried } = listedKey
    expect(isWorkspaceKeyList([{ ...carried, carried_over: true, other_workspaces: 2, mailboxes: [] }], true)).toBe(true)
    expect(isWorkspaceKeyList([{ ...listedKey, other_workspaces: 2 }], true)).toBe(false)
    expect(isWorkspaceKeyList([{ ...listedKey, carried_over: true }], true)).toBe(false)
    // Never act without read, and never a mailbox held with nothing.
    const held = listedKey.mailboxes[0]!
    expect(isKeyMailbox({ ...held, read: false, act: true }, true)).toBe(false)
    expect(isKeyMailbox({ ...held, read: false, act: false, send: false }, true)).toBe(false)
  })

  it('answers what a key holds on a mailbox with exactly the flags set, and who set them', () => {
    const held = fixture('key_access') as KeyMailbox
    expect(heldOn(held)).toEqual(held)
    expect(held.act && held.read).toBe(true)
    expect(held.granted_by).toMatch(/^usr_/)
  })

  it('lists a key’s sends as the record the server keeps: never who a message went to, its subject or its text', () => {
    const sends = fixture('key_sends') as KeySend[]
    expect(sends.length).toBeGreaterThan(0)
    for (const send of sends) {
      expect(Object.keys(send).sort()).toEqual(['account_id', 'attempts', 'created_at', 'idempotency_key', 'message_id', 'recipients', 'sent_at', 'sent_copy', 'state', 'updated_at'])
      expect(send.recipients).toBeGreaterThan(0)
    }
  })

  it('says whether this server’s keys may send, beside whether it serves MCP over HTTP', () => {
    const access = fixture('mcp') as McpAccess
    expect(access).toEqual({ http: true, keys_send: true })
    // An older server says only the first: the running console reads that as keys that do not send.
    expect(isMcpAccess({ http: true })).toBe(true)
    expect(isMcpAccess({ http: true }, true)).toBe(false)
  })

  it('lists each mailbox’s live keys in the access directory, and never counts them as readers', () => {
    const directory = fixture('access') as MailboxAccess[]
    const keyed = directory.filter(entry => (entry.keys ?? []).length > 0)
    expect(keyed.length).toBeGreaterThan(0)
    for (const entry of directory) {
      expect(entry.keys, entry.account_id).toBeDefined()
      expect(entry.readers, entry.account_id).toBe(entry.grants.filter(grant => readsNow(entry, grant)).length)
      for (const key of entry.keys!) if (key.act) expect(scopeActs(key.scope) && key.read, key.prefix).toBe(true)
    }
    // Always listed, [] for none: a directory without them is not this daemon's.
    const { keys: _, ...older } = directory[0]!
    expect(isMailboxAccessList([older], true)).toBe(false)
    expect(isMailboxAccessList([older])).toBe(true)
  })

  it('names the revision of a sending text the open console does not show, apart from the others', () => {
    // The open console asks nobody about sending, but the daemon's default
    // still names a revision of its own, which a deployment's console must
    // replace with its text's. The answer has sync's shape.
    const before = fixture('send_consent') as SyncConsent
    expect(isSyncConsent(before, true)).toBe(true)
    expect(before).toEqual({ consented: false, current_version: SEND_TEXT_VERSION })
    expect((fixture('send_consent_given') as SyncConsent).current_version).toBe(SEND_TEXT_VERSION)
    expect(new Set([SEND_TEXT_VERSION, ACTIONS_TEXT_VERSION, CONSENT_TEXT_VERSION, KEY_TERMS_VERSION]).size).toBe(4)
  })

  it('counts what each of the caller’s mailboxes takes up, sums it, and tells an owner the database’s size', () => {
    const storage = fixture('storage') as Storage
    expect(storage.mailboxes.length).toBeGreaterThan(0)
    expect(storage.total.messages).toBe(storage.mailboxes.reduce((sum, item) => sum + item.messages, 0))
    expect(storage.total.bytes).toBe(storage.mailboxes.reduce((sum, item) => sum + item.bytes, 0))
    // Captured for an owner signed in to the console.
    expect(storage.database_bytes).toBeGreaterThan(0)
    // By address, as the section lists them.
    const emails = storage.mailboxes.map(item => item.email)
    expect(emails).toEqual([...emails].sort())
    // Summed per workspace, over the same mailboxes and nothing else.
    expect(storage.workspaces?.reduce((sum, item) => sum + item.mailboxes, 0)).toBe(storage.mailboxes.length)
    expect(storage.workspaces?.reduce((sum, item) => sum + item.bytes, 0)).toBe(storage.total.bytes)
  })

  it('lists the person’s workspaces with their role in each, their own personal one among them', () => {
    const list = fixture('workspaces') as Workspace[]
    expect(list.filter(item => item.kind === 'personal')).toHaveLength(1)
    // The console names a personal workspace itself; a team carries its own name.
    expect(list.find(item => item.kind === 'personal')?.name).toBe('')
    for (const item of list) {
      expect(item.role, item.id).toMatch(/^(owner|admin|member)$/)
      expect(item.status, item.id).toBe('active')
      // Counts are the operator's listing only.
      expect(item, item.id).not.toHaveProperty('members')
    }
    // Every list the console narrows names the workspace a row belongs to.
    const ids = new Set(list.map(item => item.id))
    expect(ids.has((fixture('account') as Account).workspace_id!)).toBe(true)
    for (const mailbox of (fixture('storage') as Storage).mailboxes) expect(ids.has(mailbox.workspace_id!)).toBe(true)
  })

  it('makes the person who creates a team its owner, and joins an invited person with the role the invitation gives', () => {
    const created = fixture('workspace_created') as Workspace
    expect(created).toMatchObject({ kind: 'team', source: 'local', role: 'owner', status: 'active' })
    expect(created.name).not.toBe('')
    const joined = fixture('invite_accepted') as Workspace
    expect(joined.kind).toBe('team')
    expect(joined.role).toMatch(/^(owner|admin|member)$/)
  })

  it('marks the team’s protections in its member list before anyone tries a change', () => {
    const members = fixture('members') as Member[]
    const owners = members.filter(member => member.role === 'owner' && member.status === 'active')
    // The team's only active owner is marked, and nobody else.
    expect(owners).toHaveLength(1)
    expect(members.filter(member => member.last_owner).map(member => member.user_id)).toEqual(owners.map(member => member.user_id))
    // The last reader of a mailbox is marked with it: the one person the directory says reads it.
    const directory = fixture('access') as MailboxAccess[]
    const marked = members.flatMap(member => (member.last_reader_of ?? []).map(id => [member.user_id, id] as const))
    expect(marked.length).toBeGreaterThan(0)
    for (const [userID, id] of marked) {
      const entry = directory.find(item => item.account_id === id)
      expect(entry, id).toBeDefined()
      expect(entry!.readers, id).toBe(1)
      expect(entry!.grants.filter(grant => readsNow(entry!, grant)).map(grant => grant.user_id), id).toEqual([userID])
    }
    // And a mailbox two people read marks neither.
    for (const entry of directory.filter(item => (item.readers ?? 0) > 1)) {
      expect(marked.filter(([, id]) => id === entry.account_id), entry.account_id).toEqual([])
    }
    expect(isMember(fixture('member'), true)).toBe(true)
    // Always named, [] for none: a list without it is not this daemon's.
    const { last_reader_of: _, ...older } = fixture('member') as Member
    expect(isMember(older, true)).toBe(false)
    expect(isMember(older)).toBe(true)
  })

  it('hands an invitation’s link over once, in its fragment, and never lists it', () => {
    const made = fixture('team_invite') as TeamInvite
    expect(invitationLink(made.url)).not.toBe('')
    const url = new URL(made.url!)
    expect(url.search).toBe('')
    const linked = signupLink(made.url!)
    expect(linked.invite).not.toBe('')
    expect(linked.email).toBe(made.email)
    for (const pending of fixture('team_invites') as TeamInvite[]) {
      expect(pending, pending.id).not.toHaveProperty('url')
      expect(pending.workspace_id).toBe(made.workspace_id)
    }
  })

  it('lists who holds what on each mailbox of a team, how many read it, and its own agreement to sync', () => {
    const directory = fixture('access') as MailboxAccess[]
    const members = fixture('members') as Member[]
    expect(directory.length).toBeGreaterThan(0)
    const roleOf = (userID: string) => members.find(member => member.user_id === userID)?.role
    for (const entry of directory) {
      // Every member listed is active: each grant with read is a reader, on a
      // mailbox that has a key with the key too, and only those.
      expect(entry.readers, entry.account_id).toBe(entry.grants.filter(grant => readsNow(entry, grant)).length)
      expect(entry.no_reader, entry.account_id).toBe(entry.readers === 0)
      // Owners and admins manage by their role: Manage is stored for members only.
      for (const grant of entry.grants.filter(grant => grant.manage)) expect(roleOf(grant.user_id), grant.user_id).toBe('member')
      // Whoever linked it is named for the record, and holds no flag for having done it.
      expect(entry.linked_by, entry.account_id).toMatch(/^usr_/)
      // A team mailbox's agreement is the team's, given to the current text or not at all.
      const sync = entry.sync!
      expect(sync, entry.account_id).toBeDefined()
      if (sync.enabled) expect(sync, entry.account_id).toMatchObject({ version: CONSENT_TEXT_VERSION, current: true })
      else expect(sync, entry.account_id).toEqual({ enabled: false, current: false })
    }
    // One team mailbox linked with the team's agreement, one without.
    expect(directory.map(entry => entry.sync?.enabled).sort()).toEqual([false, true])
    // A grant answered is one of the grants listed, and a read without act is one the directory can hold.
    const grant = fixture('grant') as Grant
    expect(directory.flatMap(entry => entry.grants)).toContainEqual(grant)
    expect(grant.read && !grant.act).toBe(true)
    // Act never comes without read: the check refuses it, as the server's own constraint does.
    expect(isGrant({ ...grant, read: false, act: true }, true)).toBe(false)
    expect(isGrant({ ...grant, read: false, act: false, send: false, manage: false }, true)).toBe(false)
  })

  it('changes a grant with a request that only adds what the caller gives, or only takes away', () => {
    const grant = fixture('grant') as Grant
    const now = { read: grant.read, act: grant.act, send: grant.send, manage: grant.manage }
    expect(grantChange(now, { ...now, send: false })).toEqual({ kind: 'revoke', flags: ['send'] })
    expect(grantChange(now, { read: false, act: false, send: false, manage: false })).toEqual({ kind: 'revoke', flags: [] })
    expect(grantChange(now, { ...now, act: true })).toEqual({ kind: 'set', flags: { ...now, act: true } })
  })

  it('shows a mailbox’s key to a person signed in, the same namespace at every epoch, and none for a mailbox without one', () => {
    const linked = (fixture('add_account') as AddAccountResult).account
    const own = fixture('account') as Account
    for (const account of [linked, own, fixture('account_syncing') as Account]) {
      expect(account.mailbox_key?.epoch, account.id).toBe(1)
      expect(isSealIDText(account.mailbox_key?.namespace), account.id).toBe(true)
    }
    // A mailbox from before the key scheme is read by the flag alone, and shows no key.
    expect(fixture('account_icloud')).not.toHaveProperty('mailbox_key')
    // A personal mailbox's next key keeps its namespace, at the next epoch.
    const next = fixture('new_key') as MailboxKeyPair
    expect(next.epoch).toBe(own.mailbox_key!.epoch + 1)
    expect(next.namespace).toBe(own.mailbox_key!.namespace)
    expect(next.public_key).not.toBe(own.mailbox_key!.public_key)
    const first = fixture('first_key') as MailboxKeyPair
    expect(first.epoch).toBe(1)
    expect(new Set([linked, own].map(account => account.mailbox_key!.namespace).concat(first.namespace)).size).toBe(3)
  })

  it('says a member who holds read without the key waits for it, reads nothing of it, and who can supply it', () => {
    const waiting = fixture('account_waiting') as Account
    expect(waiting.access).toMatchObject({ read: false, act: false, waiting_key: true })
    expect(waiting.mailbox_key).toBeDefined()
    // Nobody waits for the key of a mailbox without one.
    expect(isAccount({ ...waiting, mailbox_key: undefined }, true)).toBe(false)
    const theirs = fixture('mailbox_key_waiting') as MailboxKeyState
    expect(theirs).not.toHaveProperty('grant')
    expect(theirs.namespace).toBe(waiting.mailbox_key!.namespace)
    expect(theirs.suppliers.length).toBeGreaterThan(0)
    expect(theirs.waiting).toEqual([])
    // Whoever reads it holds their grant, and is offered whoever waits.
    const reader = fixture('mailbox_key') as MailboxKeyState
    expect(isGrantText(reader.grant)).toBe(true)
    expect(reader.suppliers).toEqual([])
    expect(reader.waiting.map(person => person.user_id)).toContain((fixture('grant_supplied') as SealedGrant).user_id)
    for (const person of reader.waiting) expect(isSealIDText(person.seal_id), person.user_id).toBe(true)
    expect(theirs.suppliers.map(person => person.user_id)).toContain((fixture('grant_supplied') as SealedGrant).granted_by)
    // The key supplied is at the mailbox key's current epoch, 88 bytes the server cannot open.
    const supplied = fixture('grant_supplied') as SealedGrant
    expect(supplied.epoch).toBe(reader.epoch)
    expect(fromBase64URL(supplied.grant, 88)).toHaveLength(88)
  })

  it('offers the first key of a mailbox without one to everyone else who holds read with an account key', () => {
    const keyless = fixture('mailbox_key_keyless') as MailboxKeyState
    expect(keyless).not.toHaveProperty('epoch')
    expect(keyless.keyless_readers.length).toBeGreaterThan(0)
    for (const person of keyless.keyless_readers) expect(isSealIDText(person.seal_id), person.user_id).toBe(true)
    expect(isMailboxKeyState({ ...keyless, grant: (fixture('mailbox_key') as MailboxKeyState).grant }, true)).toBe(false)
  })

  it('marks who holds a mailbox’s key in the directory, and counts a reader only with it', () => {
    const directory = fixture('access') as MailboxAccess[]
    for (const entry of directory) {
      expect(entry.epoch, entry.account_id).toBe(1)
      for (const grant of entry.grants) expect(grant.sealed, grant.user_id).toBe(grant.read)
    }
    expect((fixture('grant') as Grant).sealed).toBe(true)
    // A member who holds read without the key reads nothing yet.
    const grant = directory[0]!.grants[0]!
    expect(readsNow({ epoch: 1 }, { ...grant, sealed: false })).toBe(false)
    expect(readsNow({}, { ...grant, sealed: false })).toBe(true)
    // Members carry what a grant to them is sealed to.
    for (const member of fixture('members') as Member[]) {
      expect(isSealIDText(member.seal_id), member.user_id).toBe(true)
      expect(member.public_key, member.user_id).toBeDefined()
    }
  })

  it('says on every account whether it can send, and why not only when it cannot', () => {
    for (const name of ['account', 'account_icloud', 'account_syncing']) {
      const send = (fixture(name) as Account).send
      expect(send, name).toBeDefined()
      expect(send!.available || Boolean(send!.reason), name).toBe(true)
    }
    const stored = fixture('account') as Account
    expect(isAccount({ ...stored, send: { available: false, reason: 'needs_reauth' } }, true)).toBe(true)
    expect(isAccount({ ...stored, send: { available: true, reason: 'needs_reauth' } }, true)).toBe(false)
  })
})
