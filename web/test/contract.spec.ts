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
// by the edition that reads them.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { EventStreamParser } from '../src/api/events'
import {
  isAccount, isAccountSync, isAddAccountResult, isAuthFlow, isCreatedKey, isErrorBody, isFolder, isInvite, isMe, isMessageNew,
  isMcpAccess, isPersonalKeyList, isProviderList, isServerEvent, isSessionReply, isStorage, isSyncConsent, isToken, isUser, isWaitResult,
  type Account, type AccountSync, type ActionsConsent, type AddAccountResult, type AuthFlow, type CreatedKey, type Folder, type Invite,
  type PersonalKey, type Provider, type ServerEvent, type SessionReply, type Storage, type SyncConsent, type WaitResult,
} from '../src/api/types'
import { listed } from '../src/api/apikeys'
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
  ['me', value => isMe(value, true)],
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
  ['apikeys', value => isPersonalKeyList(value, true)],
  ['apikey_created', value => isCreatedKey(value, true)],
  ['send_consent', value => isSyncConsent(value, true)],
  ['send_consent_given', value => isSyncConsent(value, true)],
  ['storage', value => isStorage(value, true)],
  ['mcp', value => isMcpAccess(value, true)],
]

describe('the HTTP contract the Go handlers answer with', () => {
  it.each(shapes)('%s.json has exactly the shape the console reads', (name, valid) => {
    const value = fixture(name)
    expect(valid(value), JSON.stringify(value, null, 2)).toBe(true)
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
    const list = fixture('apikeys') as PersonalKey[]
    expect(created.terms_version).toBe(KEY_TERMS_VERSION)
    expect(created.key.startsWith(`${created.prefix}.`)).toBe(true)
    // The list the console keeps is exactly what the handler lists: nothing dropped, and no secret to drop.
    expect(list.length).toBeGreaterThan(0)
    for (const key of list) {
      expect(key, key.prefix).not.toHaveProperty('key')
      expect(listed(key), key.prefix).toEqual(key)
      expect(JSON.stringify(key)).not.toContain(created.key.split('.')[1])
    }
    // A person's keys read or act; sending is not a person's scope yet.
    expect([created, ...list].every(key => key.scope === 'read' || key.scope === 'write')).toBe(true)
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
