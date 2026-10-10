// The console's half of mailbox keys and grants (docs/key-scheme.md sections
// 8, 9 and 12.12 to 12.14), with the kit's real cryptography and a fake of
// the routes: a grant this console seals opens for its recipient, with their
// account key, to the mailbox key whose public half the server holds; Read
// on a mailbox that has a key goes with a grant only to someone with an
// account key; the key is handed to a member who waits for it; every grant
// is sealed to its recipient as the server serves them just before, never as
// the page listed them before their reset, names the key it was sealed to,
// and is sealed once more on a conflict; a Read refused because what the page
// read went stale is read again and given once more; first keys are written
// right after a sign-in and never on a restored session; a personal mailbox
// gets a new key at the next epoch; every write asks for the step-up first
// when the session's is stale, sending nothing until it is given, and never
// in a browser that does not hold the person's account key; and only the
// paths the vault names read the raw account key out of it.
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { generateKeyPair, importPrivateKey, type PrivateKey } from '@thehappieco/kit/hpke'
import { checkGrantShape, openGrant, sealGrant } from '@thehappieco/kit/profiles/mailie'
import type { Account, GrantChange, KeyRecipient, MailboxAccess, MailboxKeyState, Member, User, Workspace } from '../src/api/types'
import type { Failure } from '../src/state/failure'
import { account, ana, failure, freshModules, json, now, reply, serve, settle, stubPage, type Route } from './support'

const TEAM = 'wsp_000000000000bbbb'
const PERSONAL = 'wsp_000000000000aaaa'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support: Workspace = { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 }

/** A person with a real account key: the public half the server serves, and the private half only their browser holds. */
interface Person { user: User; account: PrivateKey; raw: Bytes }

async function person(id: string, email: string, sealID: string = crypto.randomUUID()): Promise<Person> {
  const keys = await generateAccountKeys()
  return {
    user: { ...ana, id, email, name: email.split('@')[0]!, seal_id: sealID, public_key: toBase64URL(keys.publicKey) },
    account: await importPrivateKey(keys.privateKey.slice() as Bytes), raw: keys.privateKey,
  }
}

const recipient = (p: Person): KeyRecipient => ({ user_id: p.user.id, email: p.user.email, name: p.user.name, seal_id: p.user.seal_id!, public_key: p.user.public_key! })
const memberOf = (p: Person, role = 'member'): Member => ({ user_id: p.user.id, email: p.user.email, name: p.user.name, role, status: 'active', last_owner: false, last_reader_of: [], joined_at: 1, seal_id: p.user.seal_id, public_key: p.user.public_key })

/** A mailbox's key as its linker's browser made it: the private half, which the test keeps to check what grants open to. */
interface MailboxKey { epoch: number; namespace: string; publicKey: string; privateKey: Bytes }

async function mailboxKey(epoch = 1, namespace: string = crypto.randomUUID()): Promise<MailboxKey> {
  const pair = await generateKeyPair()
  return { epoch, namespace, publicKey: toBase64URL(pair.publicKey), privateKey: pair.privateKey }
}

async function grantTo(p: Person, key: MailboxKey): Promise<string> {
  return toBase64URL(await sealGrant(fromBase64URL(p.user.public_key!, 32), key.namespace, p.user.seal_id!, key.epoch, key.privateKey))
}

/** Whether a grant opens for p, at the key's namespace and epoch, to exactly the key's private half. */
async function opensFor(p: Person, key: Pick<MailboxKey, 'epoch' | 'namespace' | 'publicKey'>, grant: string): Promise<Bytes> {
  const sealed = fromBase64URL(grant, 88)
  checkGrantShape(sealed, key.epoch)
  return openGrant(p.account, key.namespace, p.user.seal_id!, key.epoch, fromBase64URL(key.publicKey, 32), sealed)
}

const same = (a: Uint8Array, b: Uint8Array) => a.length === b.length && a.every((byte, i) => byte === b[i])

/** A fresh page: every store of one module graph. */
async function load() {
  await freshModules()
  const [session, vault, keys, grants, team, stepUp, workspaces, accounts, mailbox, errors] = await Promise.all([
    import('../src/state/session'), import('../src/state/accountVault'), import('../src/state/mailboxKeys'), import('../src/state/grants'),
    import('../src/state/team'), import('../src/state/stepUp'), import('../src/state/workspaces'), import('../src/state/accounts'),
    import('../src/crypto/mailbox'), import('../src/crypto/errors'),
  ])
  return { session, vault, keys, grants, team, stepUp, workspaces, accounts, mailbox, CeremonyError: errors.CeremonyError }
}
type Page = Awaited<ReturnType<typeof load>>

/**
 * Signs p in on the page, as a sign-in ends: their account key kept in this
 * browser (keyed) and their session begun with its step-up time.
 */
async function signIn(page: Page, p: Person, options: { keyed?: boolean; authenticatedAt?: number } = {}): Promise<void> {
  const keyed = options.keyed ?? true
  if (keyed) await page.vault.keepAccountKey(p.raw.slice(), fromBase64URL(p.user.public_key!, 32), p.user.seal_id!)
  await page.session.beginSession({ ...reply(undefined, p.user), authenticated_at: options.authenticatedAt ?? now() }, keyed)
}

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)

function route(answer: Route) {
  return serve(request => {
    seen.push({ path: request.path, method: request.method, body: request.body })
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    return answer(request)
  })
}

let ana2: Person
let bea: Person
let carol: Person
/** Carol after a reset: the same person and seal id, another account key. */
let carolAfterReset: Person
beforeEach(async () => {
  stubPage()
  seen = []
  ana2 = await person(ana.id, ana.email, ana.seal_id)
  bea = await person('usr_00000000000000b2', 'bea@example.test')
  carol = await person('usr_00000000000000c3', 'carol@example.test')
  carolAfterReset = await person(carol.user.id, carol.user.email, carol.user.seal_id)
})
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('a mailbox’s keys in this browser', () => {
  it('seals a mailbox key that opens only for its recipient, at its namespace and epoch, to the mailbox’s own public key', async () => {
    const { mailbox } = await load()
    const made = await mailbox.newMailboxKey()
    const kept = made.privateKey.slice()
    const grant = await mailbox.sealTo(recipient(bea), made.namespace, made.epoch, made.privateKey)
    const key = { epoch: made.epoch, public_key: made.publicKey, namespace: made.namespace }
    expect(same(await mailbox.openOwnGrant(bea.account, bea.user.seal_id!, key, grant), kept)).toBe(true)
    // Another person, another seal id, another epoch, another namespace: none opens it.
    await expect(mailbox.openOwnGrant(carol.account, carol.user.seal_id!, key, grant)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.openOwnGrant(bea.account, carol.user.seal_id!, key, grant)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.openOwnGrant(bea.account, bea.user.seal_id!, { ...key, epoch: 2 }, grant)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.openOwnGrant(bea.account, bea.user.seal_id!, { ...key, namespace: crypto.randomUUID() }, grant)).rejects.toMatchObject({ code: 'security' })
  })

  it('refuses a grant that opens to a key other than the mailbox’s, as one sealed by whoever knows only the person’s public key', async () => {
    const { mailbox } = await load()
    const real = await mailboxKey()
    const chosen = await mailboxKey(real.epoch, real.namespace)
    // Rightly bound, sealed to Bea's public key, of a key the sealer chose.
    const forged = await grantTo(bea, chosen)
    await expect(mailbox.openOwnGrant(bea.account, bea.user.seal_id!, { epoch: 1, public_key: real.publicKey, namespace: real.namespace }, forged)).rejects.toMatchObject({ code: 'security' })
  })

  it('refuses to seal to a key of low order, or to a seal id or a public key outside its spelling', async () => {
    const { mailbox } = await load()
    const made = await mailboxKey()
    const lowOrder = toBase64URL(new Uint8Array(32) as Bytes)
    await expect(mailbox.sealTo({ seal_id: bea.user.seal_id, public_key: lowOrder }, made.namespace, 1, made.privateKey)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.sealTo({ seal_id: 'B8CBC8A8-0C90-48AC-9233-FBDACE9D7BF4', public_key: bea.user.public_key }, made.namespace, 1, made.privateKey)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.sealTo({ seal_id: bea.user.seal_id, public_key: `${bea.user.public_key}=` }, made.namespace, 1, made.privateKey)).rejects.toMatchObject({ code: 'security' })
    await expect(mailbox.sealTo(recipient(bea), made.namespace, 0, made.privateKey)).rejects.toMatchObject({ code: 'security' })
  })

  it('zeroes a key it made as soon as its grants are sealed, whatever happens', async () => {
    const { mailbox } = await load()
    const made = await mailbox.newMailboxKey()
    await mailbox.sealAll(made, [recipient(bea)])
    expect(made.privateKey.every(byte => byte === 0)).toBe(true)
    const refused = await mailbox.newMailboxKey()
    await expect(mailbox.sealAll(refused, [recipient(bea), { seal_id: 'not a seal id', public_key: bea.user.public_key }])).rejects.toMatchObject({ code: 'security' })
    expect(refused.privateKey.every(byte => byte === 0)).toBe(true)
  })
})

describe('the account key that opens grants', () => {
  it('is read raw out of the vault only on the paths the vault’s own description names', () => {
    const src = fileURLToPath(new URL('../src', import.meta.url))
    const readers = new Set<string>()
    for (const entry of readdirSync(src, { withFileTypes: true, recursive: true })) {
      if (!entry.isFile() || !/\.(ts|vue)$/.test(entry.name)) continue
      const text = readFileSync(join(entry.parentPath, entry.name), 'utf8')
      for (const call of text.matchAll(/\baccountKeyOf\(/g)) {
        if (/function accountKeyOf\($/.test(text.slice(0, call.index! + call[0].length))) continue
        const declared = [...text.slice(0, call.index).matchAll(/function (\w+)/g)].at(-1)
        readers.add(declared?.[1] ?? '(top level)')
      }
    }
    expect([...readers].sort()).toEqual(['changePassword', 'holdsAccountKey', 'replaceRecoveryCode'])
    const vault = readFileSync(join(src, 'state', 'accountVault.ts'), 'utf8')
    const header = vault.slice(0, vault.indexOf('\nimport '))
    for (const reader of readers) expect(header, reader).toContain(reader)
  })

  it('opens as the kit’s private key for the person named only, and a record of anyone else is wiped', async () => {
    const page = await load()
    await page.vault.keepAccountKey(ana2.raw.slice(), fromBase64URL(ana2.user.public_key!, 32), ana2.user.seal_id!)
    const opened = await page.vault.accountPrivateKeyOf(ana2.user.seal_id!, ana2.user.public_key!)
    expect(opened && same(opened.publicRaw, fromBase64URL(ana2.user.public_key!, 32))).toBe(true)
    expect(opened!.key.extractable).toBe(false)
    expect(await page.vault.accountPrivateKeyOf(bea.user.seal_id!, bea.user.public_key!)).toBeNull()
    expect(await page.vault.accountPrivateKeyOf(ana2.user.seal_id!, ana2.user.public_key!)).toBeNull()
  })
})

/**
 * Ana administers Support and reads its mailbox, keyed at epoch 1; who else
 * holds what is the test's: the members as each read lists them, and who
 * waits for the key as each read of it does.
 */
async function teamMailbox(page: Page, members: Member[] | (() => Member[]), answer: Route = () => failure('not_found', 404), waiting: () => KeyRecipient[] = () => []) {
  const key = await mailboxKey()
  const own = await grantTo(ana2, key)
  const shared = account({ id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', auth_kind: 'password', state: 'active', workspace_id: TEAM, access: { read: true, act: true, send: true, manage: true }, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace } })
  const state = (): MailboxKeyState => ({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: own, waiting: waiting(), suppliers: [], keyless_readers: [] })
  const directory = (): MailboxAccess[] => [{
    account_id: shared.id, email: shared.email, provider: 'imap', state: 'active', readers: 1, no_reader: false, epoch: 1,
    grants: [{ account_id: shared.id, user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1, sealed: true }],
  }]
  const listed = typeof members === 'function' ? members : () => members
  route(request => {
    const { path, method } = request
    if (path === '/v1/workspaces' && method === 'GET') return json([personal, support])
    if (path === `/v1/workspaces/${TEAM}/members`) return json([memberOf(ana2, 'admin'), ...listed()])
    if (path === `/v1/workspaces/${TEAM}/access`) return json(directory())
    if (path === '/v1/accounts' && method === 'GET') return json([shared])
    if (path === `/v1/accounts/${shared.id}` && method === 'GET') return json(shared)
    if (path === `/v1/accounts/${shared.id}/mailbox-key` && method === 'GET') return json(state())
    return answer(request)
  })
  await page.workspaces.loadWorkspaces()
  page.workspaces.selectWorkspace(TEAM)
  await Promise.all([page.team.loadMembers(), page.team.loadDirectory()])
  return { key, shared, state }
}

const reader = { read: true, act: false, send: false, manage: false }
const none = { read: false, act: false, send: false, manage: false }

describe('giving Read on a mailbox that has a key', () => {
  it('sends the member’s grant, sealed here from the giver’s own at the current epoch, which opens for them to the mailbox key', async () => {
    const page = await load()
    await signIn(page, ana2)
    const { key, shared } = await teamMailbox(page, [memberOf(carol)], ({ path, method, body }) => path === `/v1/accounts/acc_shared/access/${carol.user.id}` && method === 'PUT'
      ? json({ account_id: 'acc_shared', user_id: carol.user.id, ...(body as object), grant: undefined, updated_at: 2, sealed: true })
      : failure('not_found', 404))
    expect(await page.team.saveGrant(shared.id, carol.user.id, none, reader)).toBeNull()
    const [put] = sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)
    const body = put!.body as GrantChange
    expect(Object.keys(body).sort()).toEqual(['act', 'grant', 'manage', 'public_key', 'read', 'send'])
    expect(body.public_key).toBe(carol.user.public_key)
    expect(same(await opensFor(carol, key, body.grant!), key.privateKey)).toBe(true)
    await expect(opensFor(bea, key, body.grant!)).rejects.toThrow()
  })

  it('seals to the member as the server lists them when it is saved, not as the page listed them before their reset', async () => {
    const page = await load()
    await signIn(page, ana2)
    let listed = carol
    const { key, shared } = await teamMailbox(page, () => [memberOf(listed)], ({ path, method, body }) => path === `/v1/accounts/acc_shared/access/${carol.user.id}` && method === 'PUT'
      ? json({ account_id: 'acc_shared', user_id: carol.user.id, ...(body as object), grant: undefined, public_key: undefined, updated_at: 2, sealed: true })
      : failure('not_found', 404))
    // Carol is reset once the page has read her: a new account key, the same seal id.
    listed = carolAfterReset
    expect(page.team.memberOf(carol.user.id)!.public_key).toBe(carol.user.public_key)
    expect(await page.team.saveGrant(shared.id, carol.user.id, none, reader)).toBeNull()
    const body = sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)[0]!.body as GrantChange
    expect(body.public_key).toBe(carolAfterReset.user.public_key)
    expect(same(await opensFor(carolAfterReset, key, body.grant!), key.privateKey)).toBe(true)
    await expect(opensFor(carol, key, body.grant!)).rejects.toThrow()
  })

  it('reads the member again and seals once more when the server finds the key it names is not theirs now', async () => {
    const page = await load()
    await signIn(page, ana2)
    let current = carol
    let reads = 0
    const { key, shared } = await teamMailbox(page, () => {
      const listed = current
      // Carol is reset right after the read that saving makes before it seals.
      if (++reads === 2) current = carolAfterReset
      return [memberOf(listed)]
    }, ({ path, method, body }) => {
      if (path !== `/v1/accounts/acc_shared/access/${carol.user.id}` || method !== 'PUT') return failure('not_found', 404)
      if ((body as GrantChange).public_key !== current.user.public_key) return failure('conflict', 409)
      return json({ account_id: 'acc_shared', user_id: carol.user.id, read: true, act: false, send: false, manage: false, updated_at: 2, sealed: true })
    })
    expect(await page.team.saveGrant(shared.id, carol.user.id, none, reader)).toBeNull()
    const puts = sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`).map(request => request.body as GrantChange)
    expect(puts.map(body => body.public_key)).toEqual([carol.user.public_key, carolAfterReset.user.public_key])
    expect(same(await opensFor(carolAfterReset, key, puts[1]!.grant!), key.privateKey)).toBe(true)
  })

  it('reads the directory and the members again when a Read sent without a grant is refused, and gives it with the grant the mailbox’s new key needs', async () => {
    const page = await load()
    await signIn(page, ana2)
    const key = await mailboxKey()
    const own = await grantTo(ana2, key)
    // Read by the flag when the page read the directory; keyed by another reader since.
    let keyed = false
    const directory = (): MailboxAccess[] => [{
      account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', readers: 1, no_reader: false, ...(keyed ? { epoch: 1 } : {}),
      grants: [{ account_id: 'acc_shared', user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1, sealed: keyed }],
    }]
    route(({ path, method, body }) => {
      if (path === '/v1/workspaces') return json([personal, support])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([memberOf(ana2, 'admin'), memberOf(carol)])
      if (path === `/v1/workspaces/${TEAM}/access`) return json(directory())
      if (path === '/v1/accounts/acc_shared/mailbox-key') return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: own, waiting: [], suppliers: [], keyless_readers: [] } satisfies MailboxKeyState)
      if (path === `/v1/accounts/acc_shared/access/${carol.user.id}` && method === 'PUT') {
        const sentBody = body as GrantChange
        if (!sentBody.grant) return failure('bad_request', 400)
        return json({ account_id: 'acc_shared', user_id: carol.user.id, read: true, act: false, send: false, manage: false, updated_at: 2, sealed: true })
      }
      return failure('not_found', 404)
    })
    await page.workspaces.loadWorkspaces()
    page.workspaces.selectWorkspace(TEAM)
    await Promise.all([page.team.loadMembers(), page.team.loadDirectory()])
    keyed = true
    const directoryReads = sent('GET', `/v1/workspaces/${TEAM}/access`).length
    expect(await page.team.saveGrant('acc_shared', carol.user.id, none, reader)).toBeNull()
    const puts = sent('PUT', `/v1/accounts/acc_shared/access/${carol.user.id}`).map(request => request.body as GrantChange)
    expect(puts).toHaveLength(2)
    expect(puts[0]).toEqual(reader)
    expect(puts[1]!.public_key).toBe(carol.user.public_key)
    expect(same(await opensFor(carol, key, puts[1]!.grant!), key.privateKey)).toBe(true)
    expect(sent('GET', `/v1/workspaces/${TEAM}/access`).length).toBeGreaterThan(directoryReads)
  })

  it('gives Read with the grant to a member who enrolled since the page listed them, once the flags alone are refused', async () => {
    const page = await load()
    await signIn(page, ana2)
    let enrolled = false
    const { key, shared } = await teamMailbox(page, () => [enrolled ? memberOf(carol) : { ...memberOf(carol), public_key: undefined }], ({ path, method, body }) => {
      if (path !== `/v1/accounts/acc_shared/access/${carol.user.id}` || method !== 'PUT') return failure('not_found', 404)
      if (!(body as GrantChange).grant) return failure('bad_request', 400)
      return json({ account_id: 'acc_shared', user_id: carol.user.id, read: true, act: false, send: false, manage: false, updated_at: 2, sealed: true })
    })
    enrolled = true
    expect(await page.team.saveGrant(shared.id, carol.user.id, none, reader)).toBeNull()
    const puts = sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`).map(request => request.body as GrantChange)
    expect(puts.map(body => !!body.grant)).toEqual([false, true])
    expect(same(await opensFor(carol, key, puts[1]!.grant!), key.privateKey)).toBe(true)
  })

  it('sends nothing again when what was read again changes nothing, and says why it was refused', async () => {
    const page = await load()
    await signIn(page, ana2)
    const { shared } = await teamMailbox(page, [memberOf(carol)], ({ method }) => method === 'PUT' ? failure('bad_request', 400) : failure('not_found', 404))
    const refusal = await page.team.saveGrant(shared.id, carol.user.id, none, reader)
    expect(refusal).toEqual({ op: 'give-read', code: 'bad_request' })
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)).toHaveLength(1)
    const { describe: describeFailure } = await import('../src/ui/errors')
    expect(describeFailure(refusal as Failure)).toBe('The server did not accept this access: Act needs Read, Manage is given to members only, and only active members of the team can be given access.')
  })

  it('says the member was given Read meanwhile, and sends nothing again, when the directory read again shows it', async () => {
    const page = await load()
    await signIn(page, ana2)
    const key = await mailboxKey()
    const own = await grantTo(ana2, key)
    // Bea gives Carol Read, with her grant, once the page has read the directory.
    let given = false
    const grants = () => [
      { account_id: 'acc_shared', user_id: ana.id, read: true, act: true, send: true, manage: false, updated_at: 1, sealed: true },
      ...(given ? [{ account_id: 'acc_shared', user_id: carol.user.id, read: true, act: false, send: false, manage: false, updated_at: 2, sealed: true }] : []),
    ]
    route(({ path, method }) => {
      if (path === '/v1/workspaces') return json([personal, support])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([memberOf(ana2, 'admin'), memberOf(carol)])
      if (path === `/v1/workspaces/${TEAM}/access`) return json([{ account_id: 'acc_shared', email: 'suporte@example.test', provider: 'imap', state: 'active', readers: given ? 2 : 1, no_reader: false, epoch: 1, grants: grants() }] satisfies MailboxAccess[])
      if (path === '/v1/accounts/acc_shared/mailbox-key') return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: own, waiting: [], suppliers: [], keyless_readers: [] } satisfies MailboxKeyState)
      // A grant with a change that gives no Read, as the server now finds it.
      if (path === `/v1/accounts/acc_shared/access/${carol.user.id}` && method === 'PUT') return failure('bad_request', 400)
      return failure('not_found', 404)
    })
    await page.workspaces.loadWorkspaces()
    page.workspaces.selectWorkspace(TEAM)
    await Promise.all([page.team.loadMembers(), page.team.loadDirectory()])
    given = true
    // Ana ticked Read and Act; Bea gave Read alone: what Carol holds now is not what Ana saw.
    const refusal = await page.team.saveGrant('acc_shared', carol.user.id, none, { ...reader, act: true })
    expect(sent('PUT', `/v1/accounts/acc_shared/access/${carol.user.id}`)).toHaveLength(1)
    expect(refusal).toEqual({ op: 'give-read', code: 'conflict' })
    expect(page.team.directoryEntry('acc_shared')!.grants.some(grant => grant.user_id === carol.user.id && grant.read)).toBe(true)
  })

  it('gives Read by the flag alone to a member without an account key, asking for no step-up', async () => {
    const page = await load()
    await signIn(page, ana2, { authenticatedAt: now() - 3600 })
    const dan: Member = { ...memberOf(carol), user_id: 'usr_00000000000000d4', email: 'dan@example.test', public_key: undefined }
    const { shared } = await teamMailbox(page, [dan], ({ method, body }) => method === 'PUT' ? json({ account_id: 'acc_shared', user_id: dan.user_id, ...(body as object), updated_at: 2, sealed: false }) : failure('not_found', 404))
    expect(await page.team.saveGrant(shared.id, dan.user_id, none, reader)).toBeNull()
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${dan.user_id}`).map(request => request.body)).toEqual([reader])
    expect(page.stepUp.stepUpPrompt.open).toBe(false)
    expect(sent('GET', `/v1/accounts/${shared.id}/mailbox-key`)).toEqual([])
  })

  it('gives Read on a mailbox without a key by the flag alone, and changes without Read need no grant', async () => {
    const page = await load()
    await signIn(page, ana2, { authenticatedAt: now() - 3600 })
    route(({ path, method, body }) => {
      if (path === '/v1/workspaces') return json([personal, support])
      if (path === `/v1/workspaces/${TEAM}/members`) return json([memberOf(ana2, 'admin'), memberOf(carol)])
      if (path === `/v1/workspaces/${TEAM}/access`) return json([{ account_id: 'acc_keyless', email: 'k@example.test', provider: 'imap', state: 'active', readers: 1, no_reader: false, grants: [] }])
      if (method === 'PUT') return json({ account_id: 'acc_keyless', user_id: carol.user.id, ...(body as object), updated_at: 2, sealed: false })
      return failure('not_found', 404)
    })
    await page.workspaces.loadWorkspaces()
    page.workspaces.selectWorkspace(TEAM)
    await Promise.all([page.team.loadMembers(), page.team.loadDirectory()])
    expect(await page.team.saveGrant('acc_keyless', carol.user.id, none, reader)).toBeNull()
    expect(sent('PUT', `/v1/accounts/acc_keyless/access/${carol.user.id}`).map(request => request.body)).toEqual([reader])
    expect(page.stepUp.stepUpPrompt.open).toBe(false)
  })

  it('asks for the step-up before reading or sealing anything when the session’s is stale, and sends nothing when it is closed', async () => {
    const page = await load()
    await signIn(page, ana2, { authenticatedAt: now() - 11 * 60 })
    const { shared } = await teamMailbox(page, [memberOf(carol)])
    const saving = page.team.saveGrant(shared.id, carol.user.id, none, reader)
    await vi.waitFor(() => expect(page.stepUp.stepUpPrompt.open).toBe(true))
    expect(sent('GET', `/v1/accounts/${shared.id}/mailbox-key`)).toEqual([])
    page.stepUp.stepUpRefused()
    expect(await saving).toBe('cancelled')
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)).toEqual([])
  })

  it('refuses to give it from a browser that does not hold the giver’s account key, sending nothing', async () => {
    const page = await load()
    await signIn(page, ana2, { keyed: false })
    const { shared } = await teamMailbox(page, [memberOf(carol)])
    expect(await page.team.saveGrant(shared.id, carol.user.id, none, reader)).toEqual({ op: 'give-read', code: 'no_account_key' })
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)).toEqual([])
  })
})

describe('handing the key to a member who waits for it', () => {
  it('seals it from the person’s own grant to the member’s key at the current epoch, which opens for them, and reads the key again', async () => {
    const page = await load()
    await signIn(page, ana2)
    const { key, shared } = await teamMailbox(page, [memberOf(carol)], ({ path, method, body }) => path === `/v1/accounts/acc_shared/grants/${carol.user.id}` && method === 'PUT'
      ? json({ account_id: 'acc_shared', user_id: carol.user.id, epoch: 1, grant: (body as { grant: string }).grant, granted_by: ana.id, created_at: 2 })
      : failure('not_found', 404), () => [recipient(carol)])
    await page.keys.loadMailboxKey(shared.id)
    expect(await page.keys.supplyKey(shared.id, recipient(carol))).toBe(true)
    const [put] = sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`)
    const body = put!.body as { epoch: number; grant: string; public_key: string }
    expect(Object.keys(body).sort()).toEqual(['epoch', 'grant', 'public_key'])
    expect(body.epoch).toBe(1)
    expect(body.public_key).toBe(carol.user.public_key)
    expect(same(await opensFor(carol, key, body.grant), key.privateKey)).toBe(true)
    expect(sent('GET', `/v1/accounts/${shared.id}/mailbox-key`).length).toBeGreaterThan(1)
    expect(page.keys.keyView(shared.id)).toMatchObject({ busy: '', problem: null })
  })

  it('says the person’s own key does not open, and sends nothing, when the grant the server serves is not theirs to open', async () => {
    const page = await load()
    await signIn(page, ana2)
    const chosen = await mailboxKey()
    const forged = await grantTo(ana2, chosen)
    const key = await mailboxKey(1, chosen.namespace)
    route(({ path }) => path === '/v1/accounts/acc_shared/mailbox-key'
      ? json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant: forged, waiting: [recipient(carol)], suppliers: [], keyless_readers: [] })
      : failure('not_found', 404))
    expect(await page.keys.supplyKey('acc_shared', recipient(carol))).toBe(false)
    expect(page.keys.keyView('acc_shared').problem).toEqual({ op: 'supply-key', code: 'security' })
    expect(sent('PUT', `/v1/accounts/acc_shared/grants/${carol.user.id}`)).toEqual([])
  })

  it('asks for the step-up first when the session’s is stale, and hands the key over once it is given', async () => {
    const page = await load()
    await signIn(page, ana2, { authenticatedAt: now() - 11 * 60 })
    const { shared } = await teamMailbox(page, [memberOf(carol)], ({ method, body }) => method === 'PUT'
      ? json({ account_id: 'acc_shared', user_id: carol.user.id, epoch: 1, grant: (body as { grant: string }).grant, created_at: 2 })
      : failure('not_found', 404), () => [recipient(carol)])
    const handing = page.keys.supplyKey(shared.id, recipient(carol))
    await vi.waitFor(() => expect(page.stepUp.stepUpPrompt.open).toBe(true))
    expect(sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`)).toEqual([])
    page.session.steppedUp(ana.id, now())
    page.stepUp.stepUpGiven()
    expect(await handing).toBe(true)
    expect(sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`)).toHaveLength(1)
  })
})

describe('sealing to a member read before their reset', () => {
  /** Carol waits for the key of Support's mailbox, as each read of it says; the server takes a grant only sealed to her key now. */
  async function waitingCarol(page: Page, waiting: () => Person, current: () => Person) {
    return teamMailbox(page, [memberOf(carol)], ({ path, method, body }) => {
      if (path !== `/v1/accounts/acc_shared/grants/${carol.user.id}` || method !== 'PUT') return failure('not_found', 404)
      const sentBody = body as { grant: string; public_key: string }
      if (sentBody.public_key !== current().user.public_key) return failure('conflict', 409)
      return json({ account_id: 'acc_shared', user_id: carol.user.id, epoch: 1, grant: sentBody.grant, granted_by: ana.id, created_at: 2 })
    }, () => [recipient(waiting())])
  }

  it('hands the key to the member as the answer read in the same call lists them, not as the sheet showed them', async () => {
    const page = await load()
    await signIn(page, ana2)
    let now = carol
    const { key, shared } = await waitingCarol(page, () => now, () => now)
    await page.keys.loadMailboxKey(shared.id)
    const shown = page.keys.keyView(shared.id).state!.waiting[0]!
    // Carol is reset while the sheet is open: she still waits, with another account key.
    now = carolAfterReset
    expect(await page.keys.supplyKey(shared.id, shown)).toBe(true)
    const body = sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`)[0]!.body as { grant: string; public_key: string }
    expect(body.public_key).toBe(carolAfterReset.user.public_key)
    expect(same(await opensFor(carolAfterReset, key, body.grant), key.privateKey)).toBe(true)
    await expect(opensFor(carol, key, body.grant)).rejects.toThrow()
  })

  it('reads the key again and hands it over once more when the server finds the key it names is not the member’s now', async () => {
    const page = await load()
    await signIn(page, ana2)
    let current = carol
    let reads = 0
    const { key, shared } = await waitingCarol(page, () => {
      const listed = current
      // Carol is reset right after the read the hand-over seals from.
      if (++reads === 1) current = carolAfterReset
      return listed
    }, () => current)
    expect(await page.keys.supplyKey(shared.id, recipient(carol))).toBe(true)
    const bodies = sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`).map(request => request.body as { grant: string; public_key: string })
    expect(bodies.map(body => body.public_key)).toEqual([carol.user.public_key, carolAfterReset.user.public_key])
    expect(same(await opensFor(carolAfterReset, key, bodies[1]!.grant), key.privateKey)).toBe(true)
  })

  it('says the member no longer waits, and seals nothing, when the answer read in the same call does not list them', async () => {
    const page = await load()
    await signIn(page, ana2)
    let waiting = true
    const { shared } = await teamMailbox(page, [memberOf(carol)], () => failure('not_found', 404), () => waiting ? [recipient(carol)] : [])
    await page.keys.loadMailboxKey(shared.id)
    // Someone else handed it to her meanwhile.
    waiting = false
    expect(await page.keys.supplyKey(shared.id, recipient(carol))).toBe(false)
    expect(page.keys.keyView(shared.id).problem).toEqual({ op: 'supply-key', code: 'conflict' })
    expect(sent('PUT', `/v1/accounts/${shared.id}/grants/${carol.user.id}`)).toEqual([])
  })
})

describe('a browser that does not hold the person’s account key', () => {
  it('says so before asking for the step-up to give Read with a grant, and sends nothing', async () => {
    const page = await load()
    await signIn(page, ana2, { keyed: false, authenticatedAt: now() - 11 * 60 })
    const { shared } = await teamMailbox(page, [memberOf(carol)])
    const saving = page.team.saveGrant(shared.id, carol.user.id, none, reader)
    await settle()
    expect(page.stepUp.stepUpPrompt.open).toBe(false)
    expect(await saving).toEqual({ op: 'give-read', code: 'no_account_key' })
    expect(sent('GET', `/v1/accounts/${shared.id}/mailbox-key`)).toEqual([])
    expect(sent('PUT', `/v1/accounts/${shared.id}/access/${carol.user.id}`)).toEqual([])
  })

  it('says so before asking for the step-up to hand the key over, and sends nothing', async () => {
    const page = await load()
    await signIn(page, ana2, { keyed: false, authenticatedAt: now() - 11 * 60 })
    const { shared } = await teamMailbox(page, [memberOf(carol)], () => failure('not_found', 404), () => [recipient(carol)])
    const handing = page.keys.supplyKey(shared.id, recipient(carol))
    await settle()
    expect(page.stepUp.stepUpPrompt.open).toBe(false)
    expect(await handing).toBe(false)
    expect(page.keys.keyView(shared.id).problem).toEqual({ op: 'supply-key', code: 'no_account_key' })
    expect(sent('GET', `/v1/accounts/${shared.id}/mailbox-key`)).toEqual([])
  })
})

describe('first keys', () => {
  /** Ana reads two mailboxes without a key and one with; she sees one more she does not read, and an operator's. Bea reads the first with an account key. */
  function keyless(answer: Route = () => failure('not_found', 404)) {
    const mailboxes: Account[] = [
      account({ id: 'acc_a', state: 'active', workspace_id: TEAM, access: { read: true, act: true, send: true, manage: true } }),
      account({ id: 'acc_b', state: 'active', workspace_id: PERSONAL, access: { read: true, act: true, send: true, manage: true } }),
      account({ id: 'acc_keyed', state: 'active', workspace_id: PERSONAL, access: { read: true, act: true, send: true, manage: true }, mailbox_key: { epoch: 1, public_key: bea.user.public_key!, namespace: crypto.randomUUID() } }),
      account({ id: 'acc_card', state: 'active', workspace_id: TEAM, access: { read: false, act: false, send: false, manage: true } }),
      // An operator's mailbox, which a person is never listed; were one listed, it stays without a key.
      account({ id: 'acc_operator', state: 'active', workspace_id: 'wsp_operator', access: { read: true, act: true, send: true, manage: true } }),
    ]
    const readers: Record<string, KeyRecipient[]> = { acc_a: [recipient(bea)], acc_b: [] }
    route(request => {
      const { path, method } = request
      if (path === '/v1/accounts' && method === 'GET') return json(mailboxes)
      const id = /^\/v1\/accounts\/(acc_[a-z]+)\/mailbox-key$/.exec(path)?.[1]
      if (id && method === 'GET') return json({ waiting: [], suppliers: [], keyless_readers: readers[id] ?? [] })
      return answer(request)
    })
    return { readers }
  }
  const written = () => seen.filter(request => request.method === 'POST' && request.path.endsWith('/mailbox-key'))
  const pair = (body: unknown) => body as { public_key: string; namespace: string; grants: { user_id: string; grant: string; public_key: string }[] }

  it('writes one for every mailbox the person reads without one, right after a sign-in, sealed to everyone who reads it with an account key', async () => {
    const page = await load()
    keyless(({ body }) => json({ epoch: 1, public_key: pair(body).public_key, namespace: pair(body).namespace }, 201))
    const stop = page.keys.firstKeysAfterSignIn()
    await signIn(page, ana2)
    await vi.waitFor(() => expect(written()).toHaveLength(2))
    stop()
    expect(written().map(request => request.path)).toEqual(['/v1/accounts/acc_a/mailbox-key', '/v1/accounts/acc_b/mailbox-key'])
    const [a, b] = written().map(request => pair(request.body))
    expect(a!.grants.map(grant => grant.user_id)).toEqual([ana.id, bea.user.id])
    expect(a!.grants.map(grant => grant.public_key)).toEqual([ana2.user.public_key, bea.user.public_key])
    expect(b!.grants.map(grant => grant.user_id)).toEqual([ana.id])
    // Each opens for its recipient to the one key whose public half was sent; the keys differ, and so do the namespaces.
    const keyA = { epoch: 1, namespace: a!.namespace, publicKey: a!.public_key }
    const fromAna = await opensFor(ana2, keyA, a!.grants[0]!.grant)
    expect(same(await opensFor(bea, keyA, a!.grants[1]!.grant), fromAna)).toBe(true)
    await opensFor(ana2, { epoch: 1, namespace: b!.namespace, publicKey: b!.public_key }, b!.grants[0]!.grant)
    expect(a!.namespace).not.toBe(b!.namespace)
    expect(Object.keys(a!).sort()).toEqual(['grants', 'namespace', 'public_key'])
  })

  it('never writes one on the page load of an older session, nor after a sign-in without the key here or with a stale step-up', async () => {
    const page = await load()
    keyless(() => json({ epoch: 1, public_key: bea.user.public_key, namespace: crypto.randomUUID() }, 201))
    const stop = page.keys.firstKeysAfterSignIn()
    // A restored session: the browser remembered it, the server confirms it.
    const sessionVault = await import('../src/state/sessionVault')
    await sessionVault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_stored', expiresAt: now() + 3600, userID: ana.id })
    await page.vault.keepAccountKey(ana2.raw.slice(), fromBase64URL(ana2.user.public_key!, 32), ana2.user.seal_id!)
    route(({ path }) => path === '/v1/auth/me' ? json({ user: ana2.user, session: { id: 'ses_1', created_at: now(), expires_at: now() + 3600, authenticated_at: now() } }) : failure('not_found', 404))
    await page.session.restore()
    expect(page.session.session.phase).toBe('ready')
    expect(page.session.session.keyed).toBe(true)
    await settle()
    expect(seen.map(request => request.path)).toEqual(['/v1/auth/me'])
    // A sign-in whose key this browser does not keep, and one whose step-up time has passed.
    seen = []
    keyless(() => failure('not_found', 404))
    await signIn(page, ana2, { keyed: false })
    await signIn(page, ana2, { authenticatedAt: now() - 11 * 60 })
    await settle()
    stop()
    expect(seen.filter(request => request.path !== '/v1/auth/logout')).toEqual([])
  })

  it('reads the mailbox again and tries once more when the server finds someone it was not sealed to', async () => {
    const page = await load()
    let posts = 0
    const { readers } = keyless(({ body }) => {
      if (++posts === 1) {
        // Carol enrolled meanwhile: the grants are not exactly the readers' any more.
        readers.acc_a = [recipient(bea), recipient(carol)]
        return failure('conflict', 409)
      }
      return json({ epoch: 1, public_key: pair(body).public_key, namespace: pair(body).namespace }, 201)
    })
    await signIn(page, ana2)
    expect(await page.keys.writeFirstKeys()).toEqual(['acc_a', 'acc_b'])
    const [refused, accepted] = written().filter(request => request.path === '/v1/accounts/acc_a/mailbox-key').map(request => pair(request.body))
    expect(refused!.grants.map(grant => grant.user_id)).toEqual([ana.id, bea.user.id])
    expect(accepted!.grants.map(grant => grant.user_id)).toEqual([ana.id, bea.user.id, carol.user.id])
    expect(accepted!.public_key).not.toBe(refused!.public_key)
    await opensFor(carol, { epoch: 1, namespace: accepted!.namespace, publicKey: accepted!.public_key }, accepted!.grants[2]!.grant)
  })

  it('writes one from a mailbox’s sheet after a step-up, and asks for it first when the session’s is stale', async () => {
    const page = await load()
    keyless(({ body }) => json({ epoch: 1, public_key: pair(body).public_key, namespace: pair(body).namespace }, 201))
    await signIn(page, ana2, { authenticatedAt: now() - 11 * 60 })
    const writing = page.keys.writeFirstKey('acc_b')
    await vi.waitFor(() => expect(page.stepUp.stepUpPrompt.open).toBe(true))
    expect(written()).toEqual([])
    page.session.steppedUp(ana.id, now())
    page.stepUp.stepUpGiven()
    expect(await writing).toBe(true)
    expect(written().map(request => request.path)).toEqual(['/v1/accounts/acc_b/mailbox-key'])
  })
})

describe('a personal mailbox’s new key', () => {
  it('writes the next epoch in the mailbox’s own namespace, sealed to its person alone, and every older grant is the server’s to drop', async () => {
    const page = await load()
    const key = await mailboxKey()
    const mine = account({ id: 'acc_mine', state: 'active', workspace_id: PERSONAL, access: { read: false, act: false, send: true, manage: true, waiting_key: true }, mailbox_key: { epoch: 1, public_key: key.publicKey, namespace: key.namespace } })
    route(({ path, method, body }) => {
      if (path === '/v1/accounts/acc_mine/mailbox-key' && method === 'GET') return json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, waiting: [], suppliers: [], keyless_readers: [] })
      if (path === '/v1/accounts/acc_mine/mailbox-key' && method === 'PUT') {
        const next = body as { epoch: number; public_key: string }
        return json({ epoch: next.epoch, public_key: next.public_key, namespace: key.namespace })
      }
      if (path === '/v1/accounts/acc_mine') return json(mine)
      return failure('not_found', 404)
    })
    await signIn(page, ana2)
    expect(await page.keys.writeNewKey(mine)).toBe(true)
    const [put] = sent('PUT', '/v1/accounts/acc_mine/mailbox-key')
    const body = put!.body as { epoch: number; public_key: string; grant: string }
    expect(Object.keys(body).sort()).toEqual(['epoch', 'grant', 'public_key'])
    expect(body.epoch).toBe(2)
    expect(body.public_key).not.toBe(key.publicKey)
    await opensFor(ana2, { epoch: 2, namespace: key.namespace, publicKey: body.public_key }, body.grant)
    await expect(opensFor(ana2, { epoch: 1, namespace: key.namespace, publicKey: body.public_key }, body.grant)).rejects.toThrow()
  })

  it('finds out whether the person’s own key of the mailbox opens in this browser', async () => {
    const page = await load()
    const key = await mailboxKey()
    let grant = await grantTo(ana2, key)
    route(({ path }) => path === '/v1/accounts/acc_mine/mailbox-key'
      ? json({ epoch: 1, public_key: key.publicKey, namespace: key.namespace, grant, waiting: [], suppliers: [], keyless_readers: [] })
      : failure('not_found', 404))
    await signIn(page, ana2)
    await page.keys.loadMailboxKey('acc_mine')
    await page.keys.checkOwnGrant('acc_mine')
    expect(page.keys.keyView('acc_mine').opens).toBe('yes')
    // A grant rightly bound to Ana, of a key someone else chose.
    grant = await grantTo(ana2, await mailboxKey(1, key.namespace))
    await page.keys.loadMailboxKey('acc_mine')
    expect(page.keys.keyView('acc_mine').opens).toBe('unknown')
    await page.keys.checkOwnGrant('acc_mine')
    expect(page.keys.keyView('acc_mine').opens).toBe('no')
  })
})
