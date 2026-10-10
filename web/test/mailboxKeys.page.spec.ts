// Mailbox keys as a person meets them, on a page (test/dom.ts), with the
// kit's real cryptography: a link whose step-up is stale asks for the
// password over the connecting dialog before anything is sent, and links
// once it is given; a card and a sheet tell waiting for the key from not
// reading; a member waiting is told who can hand the key over, and someone
// who reads it hands it over after the password, in a grant that opens for
// the member; a personal mailbox waiting for its key is offered a new one, a
// team's never; a mailbox without a key is offered its first; the key
// section reads the key when the sheet opens, not each time a sync replaces
// the card; and the access panel marks who waits, and says when saving Read
// hands the key over, or that this browser cannot.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive } from 'vue'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { generateKeyPair, importPrivateKey, type PrivateKey } from '@thehappieco/kit/hpke'
import { openGrant, sealGrant } from '@thehappieco/kit/profiles/mailie'
import { check, click, fill, find, flush, page, submit, words, type FakeElement } from './dom'
import { mount, type Mounted } from './mount'
import type { Account, KeyRecipient, MailboxAccess, MailboxKeyState, Member, User, Workspace } from '../src/api/types'
import AccessPanel from '../src/components/AccessPanel.vue'
import AccountSheet from '../src/components/AccountSheet.vue'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import AddAccountDialog from '../src/components/AddAccountDialog.vue'
import MailboxKeyPanel from '../src/components/MailboxKeyPanel.vue'
import StepUpPrompt from '../src/components/StepUpPrompt.vue'
import { accounts, loadAccounts, loadProviders } from '../src/state/accounts'
import { keepAccountKey } from '../src/state/accountVault'
import { beginSession, session, signOut } from '../src/state/session'
import { stepUpPrompt } from '../src/state/stepUp'
import { loadDirectory, loadMembers } from '../src/state/team'
import { loadWorkspaces, selectWorkspace } from '../src/state/workspaces'
import { DEFAULT_KDF, saltOf } from './accountServer'
import { account, ana, failure, json, now, reply, serve, stubPage, syncing, type Route } from './support'

const PASSWORD = 'correct horse battery staple'
const TEAM = 'wsp_000000000000bbbb'
const PERSONAL = 'wsp_000000000000aaaa'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })
const full = { read: true, act: true, send: true, manage: true }

/** The browser's FormData over test/dom.ts's form: the named fields' values, as the step-up dialog reads them at submit. */
class FormOf {
  private readonly fields = new Map<string, string>()
  constructor(form: FakeElement) {
    for (const field of form.querySelectorAll('input')) {
      const name = field.getAttribute('name')
      if (name) this.fields.set(name, field.value)
    }
  }
  has(name: string): boolean { return this.fields.has(name) }
  get(name: string): string | null { return this.fields.get(name) ?? null }
}

interface Person { user: User; account: PrivateKey; raw: Bytes }
async function person(fields: Partial<User>): Promise<Person> {
  const keys = await generateAccountKeys()
  return { user: { ...ana, ...fields, public_key: toBase64URL(keys.publicKey) }, account: await importPrivateKey(keys.privateKey.slice() as Bytes), raw: keys.privateKey }
}
const recipient = (p: Person): KeyRecipient => ({ user_id: p.user.id, email: p.user.email, name: p.user.name, seal_id: p.user.seal_id!, public_key: p.user.public_key! })

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
let mounted: Mounted[] = []
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)

/**
 * Ana signed in, her account key kept in this browser, with this step-up
 * time; her workspaces are her personal one and Support, with the role
 * given; route answers the rest, and the step-up's challenge and proof are
 * answered here (the password is derived for real, and never sent).
 */
async function signedIn(me: Person, route: Route, options: { role?: string; authenticatedAt?: number; keyed?: boolean } = {}) {
  serve(request => {
    seen.push({ path: request.path, method: request.method, body: request.body })
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (request.path === '/v1/workspaces' && request.method === 'GET') return json([personal, support(options.role ?? 'member')])
    if (request.path === '/v1/auth/challenge') return json({ salt: saltOf('ana'), kdf: DEFAULT_KDF })
    if (request.path === '/v1/auth/stepup') return json({ authenticated_at: now() })
    return route(request)
  })
  const keyed = options.keyed ?? true
  if (keyed) await keepAccountKey(me.raw.slice(), fromBase64URL(me.user.public_key!, 32), me.user.seal_id!)
  await beginSession({ ...reply(undefined, me.user), authenticated_at: options.authenticatedAt ?? now() }, keyed)
  await loadWorkspaces()
}

/** Gives the step-up over whatever asked for it: the password typed in its dialog, and sent on. */
async function giveStepUp() {
  await fill(find('form[name=mailie-step-up] input[name=password]'), PASSWORD)
  await submit(find('form[name=mailie-step-up]'))
  await vi.waitFor(() => expect(stepUpPrompt.open).toBe(false), { timeout: 20_000 })
  await flush()
}

let ana2: Person
beforeEach(async () => {
  stubPage()
  vi.stubGlobal('FormData', FormOf)
  seen = []
  ana2 = await person({})
})
afterEach(async () => {
  for (const each of mounted) each.unmount()
  mounted = []
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('linking a mailbox', { timeout: 30_000 }, () => {
  /** The connecting dialog, IMAP chosen, in the personal workspace; the server takes what is posted. */
  async function connecting(authenticatedAt: number, posted: unknown[]) {
    await signedIn(ana2, ({ path, method, body }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([])
      if (path === '/v1/providers') return json([{ id: 'imap', oauth: false, password: true, flows: [] }])
      if (path === '/v1/accounts' && method === 'POST') {
        posted.push(body)
        return json({ account: account({ id: 'acc_new', email: 'vendas@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: PERSONAL, access: full }) }, 201)
      }
      return failure('not_found', 404)
    }, { authenticatedAt })
    selectWorkspace(PERSONAL)
    await Promise.all([loadAccounts(), loadProviders()])
    mounted.push(mount(StepUpPrompt), mount(AddAccountDialog, { resume: null }))
    await flush()
    await click(find('dialog .provider-option', 'Other provider (IMAP)'))
    await fill(find('dialog input[name=mailbox]'), 'vendas@example.test')
    await fill(find('dialog input[name=mailbox-password]'), 'app-password-123')
    await submit(find('dialog form'))
  }

  it('asks for the password over the connecting dialog before a link whose step-up is stale, and links once it is given', async () => {
    const posted: unknown[] = []
    await connecting(now() - 9 * 60, posted)
    expect(find('dialog h2', 'Enter your password again')).not.toBeNull()
    expect(posted).toEqual([])
    await giveStepUp()
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    const body = posted[0] as { public_key: string; namespace: string; grant: string }
    await openGrant(ana2.account, body.namespace, ana2.user.seal_id!, 1, fromBase64URL(body.public_key, 32), fromBase64URL(body.grant, 88))
    await vi.waitFor(() => expect(words()).toContain('Account connected'))
    expect(seen.some(request => JSON.stringify(request.body ?? '').includes(PASSWORD))).toBe(false)
    expect(sent('POST', '/v1/auth/stepup')).toHaveLength(1)
  })

  it('leaves the form as it was, and sends nothing, when the step-up is closed', async () => {
    const posted: unknown[] = []
    await connecting(0, posted)
    expect(find('dialog h2', 'Enter your password again')).not.toBeNull()
    await click(find('form[name=mailie-step-up] button', 'Cancel'))
    // The step-up dialog was the one on top; the connecting form is back, filled in.
    expect(stepUpPrompt.open).toBe(false)
    expect(posted).toEqual([])
    expect(find('dialog input[name=mailbox]')!.value).toBe('vendas@example.test')
    expect(find('dialog button[type=submit]')!.disabled).toBe(false)
  })
})

/** A team mailbox with a key, at epoch 1, as its linker's browser made it. */
async function keyedMailbox() {
  const pair = await generateKeyPair()
  return { namespace: crypto.randomUUID(), publicKey: toBase64URL(pair.publicKey), privateKey: pair.privateKey }
}

describe('a mailbox whose key the person waits for', () => {
  const waitingAccess = { read: false, act: false, send: true, manage: false, waiting_key: true }

  it('says so on its card, apart from a mailbox the person does not read, in a team and in the personal workspace', async () => {
    const key = await keyedMailbox()
    const mailbox_key = { epoch: 1, public_key: key.publicKey, namespace: key.namespace }
    const list: Record<string, Account[]> = {
      [TEAM]: [
        account({ id: 'acc_wait', email: 'suporte@example.test', state: 'active', workspace_id: TEAM, access: waitingAccess, mailbox_key }),
        account({ id: 'acc_card', email: 'diretoria@example.test', state: 'active', workspace_id: TEAM, access: { read: false, act: false, send: false, manage: true } }),
      ],
      [PERSONAL]: [account({ id: 'acc_mine', email: 'ana@example.test', state: 'active', workspace_id: PERSONAL, access: { ...waitingAccess, manage: true }, mailbox_key, sync: syncing() })],
    }
    await signedIn(ana2, ({ path, url }) => {
      if (path === '/v1/accounts') return json(list[new URL(url).searchParams.get('workspace') ?? ''] ?? [])
      return failure('not_found', 404)
    })
    selectWorkspace(TEAM)
    await loadAccounts()
    mounted.push(mount(AccountsPanel))
    await flush()
    const card = (email: string) => page.querySelectorAll('.account-card').find(item => words(item).includes(email))!
    expect(words(card('suporte@example.test'))).toContain('You hold Read on this mailbox, but are waiting for its key. Anyone who reads it can hand it to you.')
    expect(words(card('suporte@example.test'))).not.toContain('You can see this mailbox, but not read it.')
    expect(words(card('diretoria@example.test'))).toContain('You can see this mailbox, but not read it.')
    expect(words(card('diretoria@example.test'))).not.toContain('waiting for its key')
    selectWorkspace(PERSONAL)
    await vi.waitFor(() => expect(accounts.list.map(item => item.id)).toEqual(['acc_mine']))
    await flush()
    expect(words(card('ana@example.test'))).toContain('This mailbox is waiting for its key. Create a new one from its details to read it again.')
  })

  it('tells a member of a team who can hand the key over, shows nothing to read, and offers no new key', async () => {
    const key = await keyedMailbox()
    const waiting = account({ id: 'acc_wait', email: 'suporte@example.test', state: 'active', workspace_id: TEAM, access: waitingAccess, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace } })
    await signedIn(ana2, ({ path }) => {
      if (path === '/v1/accounts/acc_wait/mailbox-key') {
        return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, waiting: [], keyless_readers: [], suppliers: [{ user_id: 'usr_00000000000000b2', email: 'bea@example.test', name: 'Bea Lima' }] } satisfies MailboxKeyState)
      }
      return failure('not_found', 404)
    })
    selectWorkspace(TEAM)
    mounted.push(mount(AccountSheet, { account: waiting }))
    await vi.waitFor(() => expect(words(find('.key-section')!)).toContain('Bea Lima'))
    const section = words(find('.key-section')!)
    expect(section).toContain('You hold Read on this mailbox, but not its key yet, so you do not read it.')
    expect(section).toContain('Anyone who reads it can hand the key to you:')
    expect(find('.key-section button', 'Create a new key')).toBeNull()
    expect(words()).toContain('You are waiting for this mailbox’s key, so its folders and messages are not shown to you yet.')
    expect(find('button', 'Show folders')).toBeNull()
  })

  it('offers its person a new key for a personal mailbox, in a grant that opens for them at the next epoch', async () => {
    const key = await keyedMailbox()
    const mine = account({ id: 'acc_mine', email: 'ana@example.test', state: 'active', workspace_id: PERSONAL, access: { ...waitingAccess, manage: true }, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace } })
    await signedIn(ana2, ({ path, method, body }) => {
      if (path === '/v1/accounts/acc_mine/mailbox-key' && method === 'GET') return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, waiting: [], suppliers: [], keyless_readers: [] })
      if (path === '/v1/accounts/acc_mine/mailbox-key' && method === 'PUT') return json({ epoch: 2, public_key: (body as { public_key: string }).public_key, namespace: key.namespace })
      if (path === '/v1/accounts/acc_mine') return json({ ...mine, access: full, mailbox_key: { epoch: 2, public_key: (sent('PUT', '/v1/accounts/acc_mine/mailbox-key')[0]!.body as { public_key: string }).public_key, namespace: key.namespace } })
      return failure('not_found', 404)
    })
    selectWorkspace(PERSONAL)
    mounted.push(mount(StepUpPrompt), mount(AccountSheet, { account: mine }))
    await flush()
    expect(words(find('.key-section')!)).toContain('your account key changed after its key was created')
    await click(find('.key-section button', 'Create a new key'))
    await vi.waitFor(() => expect(sent('PUT', '/v1/accounts/acc_mine/mailbox-key')).toHaveLength(1))
    const [put] = sent('PUT', '/v1/accounts/acc_mine/mailbox-key')
    const body = put!.body as { epoch: number; public_key: string; grant: string }
    expect(body.epoch).toBe(2)
    await openGrant(ana2.account, key.namespace, ana2.user.seal_id!, 2, fromBase64URL(body.public_key, 32), fromBase64URL(body.grant, 88))
  })
})

describe('handing the key over', { timeout: 30_000 }, () => {
  it('lists who waits for it to someone who reads the mailbox, and hands it over after the password, in a grant that opens for them', async () => {
    const key = await keyedMailbox()
    const carol = await person({ id: 'usr_00000000000000c3', email: 'carol@example.test', name: 'Carol Dias', seal_id: crypto.randomUUID() })
    const own = toBase64URL(await sealGrant(fromBase64URL(ana2.user.public_key!, 32), key.namespace, ana2.user.seal_id!, 1, key.privateKey))
    const shared = account({ id: 'acc_shared', email: 'suporte@example.test', state: 'active', workspace_id: TEAM, access: { read: true, act: true, send: true, manage: false }, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace } })
    let handed = false
    await signedIn(ana2, ({ path, method, body }) => {
      if (path === '/v1/accounts/acc_shared/mailbox-key') {
        return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: own, waiting: handed ? [] : [recipient(carol)], suppliers: [], keyless_readers: [] } satisfies MailboxKeyState)
      }
      if (path === `/v1/accounts/acc_shared/grants/${carol.user.id}` && method === 'PUT') {
        handed = true
        return json({ account_id: 'acc_shared', user_id: carol.user.id, epoch: 1, grant: (body as { grant: string }).grant, granted_by: ana.id, created_at: 2 })
      }
      if (path === '/v1/accounts/acc_shared') return json(shared)
      return failure('not_found', 404)
    }, { authenticatedAt: now() - 11 * 60 })
    selectWorkspace(TEAM)
    mounted.push(mount(StepUpPrompt), mount(AccountSheet, { account: shared }))
    await vi.waitFor(() => expect(find('.key-section li', 'Carol Dias')).not.toBeNull())
    expect(words(find('.key-section')!)).toContain('These people hold Read on this mailbox but not its key yet')
    await click(find('.key-section li button', 'Hand over the key'))
    expect(find('dialog h2', 'Enter your password again')).not.toBeNull()
    expect(sent('PUT', `/v1/accounts/acc_shared/grants/${carol.user.id}`)).toEqual([])
    await giveStepUp()
    await vi.waitFor(() => expect(sent('PUT', `/v1/accounts/acc_shared/grants/${carol.user.id}`)).toHaveLength(1))
    const body = sent('PUT', `/v1/accounts/acc_shared/grants/${carol.user.id}`)[0]!.body as { epoch: number; grant: string }
    const opened = await openGrant(carol.account, key.namespace, carol.user.seal_id!, 1, fromBase64URL(key.publicKey, 32), fromBase64URL(body.grant, 88))
    expect(opened.every((byte, i) => byte === key.privateKey[i])).toBe(true)
    await vi.waitFor(() => expect(find('.key-section')).toBeNull())
  })
})

describe('a mailbox without a key', () => {
  it('offers its first key to someone who reads it, naming who else it is sealed to, and writes it', async () => {
    const bea = await person({ id: 'usr_00000000000000b2', email: 'bea@example.test', name: 'Bea Lima', seal_id: crypto.randomUUID() })
    const shared = account({ id: 'acc_shared', email: 'suporte@example.test', state: 'active', workspace_id: TEAM, access: { read: true, act: true, send: true, manage: false } })
    let keyed = false
    await signedIn(ana2, ({ path, method, body }) => {
      if (path === '/v1/accounts/acc_shared/mailbox-key' && method === 'GET') return json(keyed ? { epoch: 1, public_key: bea.user.public_key, namespace: crypto.randomUUID(), grant: toBase64URL(new Uint8Array(88) as Bytes), waiting: [], suppliers: [], keyless_readers: [] } : { waiting: [], suppliers: [], keyless_readers: [recipient(bea)] })
      if (path === '/v1/accounts/acc_shared/mailbox-key' && method === 'POST') {
        keyed = true
        const pair = body as { public_key: string; namespace: string }
        return json({ epoch: 1, public_key: pair.public_key, namespace: pair.namespace }, 201)
      }
      if (path === '/v1/accounts/acc_shared') return json(shared)
      return failure('not_found', 404)
    })
    selectWorkspace(TEAM)
    mounted.push(mount(AccountSheet, { account: shared }))
    await vi.waitFor(() => expect(find('.key-section li', 'Bea Lima')).not.toBeNull())
    expect(words(find('.key-section')!)).toContain('Creating its key seals it to you and to the people below')
    await click(find('.key-section button', 'Create its key'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts/acc_shared/mailbox-key')).toHaveLength(1))
    const [post] = sent('POST', '/v1/accounts/acc_shared/mailbox-key')
    expect((post!.body as { grants: { user_id: string }[] }).grants.map(grant => grant.user_id)).toEqual([ana.id, bea.user.id])
  })
})

describe('the access panel of a mailbox that has a key', () => {
  it('marks who holds Read without the key, and says saving Read for someone with an account key hands it over', async () => {
    const bea = await person({ id: 'usr_00000000000000b2', email: 'bea@example.test', name: 'Bea Lima', seal_id: crypto.randomUUID() })
    const member = (p: Person, role = 'member'): Member => ({ user_id: p.user.id, email: p.user.email, name: p.user.name, role, status: 'active', last_owner: false, last_reader_of: [], joined_at: 1, seal_id: p.user.seal_id, public_key: p.user.public_key })
    const carol: Member = { user_id: 'usr_00000000000000c3', email: 'carol@example.test', name: 'Carol Dias', role: 'member', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1, seal_id: crypto.randomUUID() }
    const directory: MailboxAccess[] = [{
      account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', readers: 1, no_reader: false, epoch: 1,
      grants: [
        { account_id: 'acc_shared', user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1, sealed: true },
        { account_id: 'acc_shared', user_id: carol.user_id, read: true, act: false, send: false, manage: false, updated_at: 1, sealed: false },
      ],
    }]
    await signedIn(ana2, ({ path }) => {
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana2, 'admin'), member(bea), carol])
      if (path === `/v1/workspaces/${TEAM}/access`) return json(directory)
      return failure('not_found', 404)
    }, { role: 'admin' })
    selectWorkspace(TEAM)
    await Promise.all([loadMembers(), loadDirectory()])
    mounted.push(mount(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' }))
    await flush()
    const row = (id: string) => find(`li[data-user="${id}"]`)!
    expect(words(row(carol.user_id))).toContain('Waiting for the key')
    expect(words(row(ana.id))).not.toContain('Waiting for the key')
    // Carol holds Read without the key: Ana, who reads it, is its last reader.
    expect(find(`input[name="${ana.id}-read"]`)!.disabled).toBe(true)
    await check(find(`input[name="${bea.user.id}-read"]`))
    expect(words(row(bea.user.id))).toContain('Saving also hands them this mailbox’s key, sealed in this browser.')
    expect(session.keyed).toBe(true)
  })

  it('says, beside a Read that would hand the key over, that this browser does not hold the giver’s account key', async () => {
    const bea = await person({ id: 'usr_00000000000000b2', email: 'bea@example.test', name: 'Bea Lima', seal_id: crypto.randomUUID() })
    const member = (p: Person, role = 'member'): Member => ({ user_id: p.user.id, email: p.user.email, name: p.user.name, role, status: 'active', last_owner: false, last_reader_of: [], joined_at: 1, seal_id: p.user.seal_id, public_key: p.user.public_key })
    const directory: MailboxAccess[] = [{
      account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', readers: 1, no_reader: false, epoch: 1,
      grants: [{ account_id: 'acc_shared', user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1, sealed: true }],
    }]
    await signedIn(ana2, ({ path }) => {
      if (path === `/v1/workspaces/${TEAM}/members`) return json([member(ana2, 'admin'), member(bea)])
      if (path === `/v1/workspaces/${TEAM}/access`) return json(directory)
      return failure('not_found', 404)
    }, { role: 'admin', keyed: false })
    selectWorkspace(TEAM)
    await Promise.all([loadMembers(), loadDirectory()])
    mounted.push(mount(AccessPanel, { accountId: 'acc_shared', email: 'suporte@example.test' }))
    await flush()
    await check(find(`input[name="${bea.user.id}-read"]`))
    const row = words(find(`li[data-user="${bea.user.id}"]`)!)
    expect(row).toContain('This browser does not hold your account key. Sign out, sign in again here, and try again.')
    expect(row).not.toContain('Saving also hands them this mailbox’s key')
  })
})

describe('the key section of a sheet', () => {
  it('reads the key and opens the person’s own when the sheet opens, and not again when a sync replaces the card', async () => {
    const key = await keyedMailbox()
    const own = toBase64URL(await sealGrant(fromBase64URL(ana2.user.public_key!, 32), key.namespace, ana2.user.seal_id!, 1, key.privateKey))
    await signedIn(ana2, ({ path, method }) => path === '/v1/accounts/acc_mine/mailbox-key' && method === 'GET'
      ? json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: own, waiting: [], suppliers: [], keyless_readers: [] } satisfies MailboxKeyState)
      : failure('not_found', 404))
    selectWorkspace(PERSONAL)
    const props = reactive({ account: account({ id: 'acc_mine', email: 'ana@example.test', state: 'active', workspace_id: PERSONAL, access: full, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace }, sync: syncing() }) })
    mounted.push(mount(MailboxKeyPanel, props))
    const reads = () => sent('GET', '/v1/accounts/acc_mine/mailbox-key').length
    await vi.waitFor(() => expect(reads()).toBe(1))
    // A sync replaces the card whole, five times, saying nothing new of its key.
    for (let i = 1; i <= 5; i++) {
      props.account = { ...props.account, sync: syncing({ messages: 1284 + i }) }
      await flush()
    }
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(reads()).toBe(1)
    // Another epoch is something new: it is read again.
    props.account = { ...props.account, mailbox_key: { ...props.account.mailbox_key!, epoch: 2 } }
    await vi.waitFor(() => expect(reads()).toBe(2))
  })
})
