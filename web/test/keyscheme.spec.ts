// Mailie's key scheme, held to the golden vectors the Go side writes
// (internal/keyscheme/testdata, `go test ./internal/keyscheme -run
// TestTheVectorsAreWhatTheProfileWrites -update`): every case marked for
// TypeScript, or for every language, is computed, opened, replayed or refused
// here by src/crypto/mailie.ts, which is how the browser will do it. The
// files are the Go side's to write: a missing one is a failure, never
// something to create from here. docs/key-scheme.md is the specification.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { derive, recoveryKey, recoveryProof } from '@thehappieco/kit/account'
import { equal, formatUUID, fromBase64, fromBase64URL, toBase64, type Bytes } from '@thehappieco/kit/bytes'
import { importPrivateKey, publicFromPrivate } from '@thehappieco/kit/hpke'
import { isPlatformWrapError, platformWrapAAD, platformWrapInfo } from '@thehappieco/kit/platformwrap'
import { deriveProductKey, isPlatformError, PASSWORD_PROFILE, recoveryCodeFromBytes } from '@thehappieco/kit/profiles/platform/core'
import { KIND_CONTENT_KEY, KIND_GRANT, KIND_USER_WRAP } from '@thehappieco/kit/seal'
import {
  ACCOUNT_WRAP_HEADER, ACCOUNT_WRAP_LEN, ACCOUNT_WRAP_TAG, ACCOUNT_WRAP_VERSION, AccountError, BROWSER_VAULT_TAG, BROWSER_VAULT_VERSION,
  DEFAULT_KDF, GRANT_LEN, Kind, MailieError, MAX_EPOCH, MIN_EPOCH, PASSWORD_AUTH_LABEL, PASSWORD_WRAP_LABEL,
  RECOVERY_AUTH_LABEL, RECOVERY_WRAP_LABEL, SALT_LEN, SEAL_LABEL, SEAL_MAGIC, SealError, accountWrapAAD, browserVaultAAD,
  checkAccountWrapShape, checkGrantShape, checkKDF, grantAAD, grantInfo, grantRow, isNamespace, isSealID, kindName, mailieAccount,
  mailieBrowserVault, mailiePlatformWrap, mailieSeal, newNamespace, normaliseAddress, openAccountWrap, openBrowserVault, openGrant,
  openBrowserVaultKey, openMailiePlatformWrap, platformWrapBinding, prepareNewPassword, sealAccountWrap, sealBrowserVault, sealGrant,
  sealMailiePlatformWrap,
} from '../src/crypto/mailie'

interface Case {
  id: string
  op: string
  langs?: string[]
  in: Record<string, unknown>
  out?: Record<string, unknown>
  error?: string
  reason?: string
}

interface VectorFile {
  format: string
  module: string
  profile: string
  generated_by: { lang: string; toolchain: string }
  keys?: Record<string, { private_key_b64: string; public_key_b64: string }>
  cases: Case[]
}

const directory = new URL('../../internal/keyscheme/testdata/', import.meta.url)

function vectors(name: string): VectorFile {
  const path = fileURLToPath(new URL(name, directory))
  let text: string
  try { text = readFileSync(path, 'utf8') } catch {
    throw new Error(`missing vectors ${path}: run \`go test ./internal/keyscheme -run TestTheVectorsAreWhatTheProfileWrites -update\` on the Go side`)
  }
  const file = JSON.parse(text) as VectorFile
  if (file.format !== 'thehappieco-kit-vectors/1' || file.profile !== 'mailie' || file.generated_by.lang !== 'go') throw new Error(`${name} is not Mailie's Go vectors`)
  return file
}

type Out = Record<string, unknown>
type Op = (input: Input, file: VectorFile, c: Case) => Promise<Out>

/** Input reads a case's inputs, failing on a missing or mistyped one. */
class Input {
  constructor(readonly c: Case) {}
  has(k: string): boolean { return k in this.c.in }
  str(k: string): string {
    const v = this.c.in[k]
    if (typeof v !== 'string') throw new Error(`${this.c.id}: ${k} is not a string`)
    return v
  }
  num(k: string): number {
    const v = this.c.in[k]
    if (typeof v !== 'number') throw new Error(`${this.c.id}: ${k} is not a number`)
    return v
  }
  bytes(k: string): Bytes { return fromBase64(this.str(k)) }
  obj(k: string): Record<string, unknown> {
    const v = this.c.in[k]
    if (typeof v !== 'object' || v === null) throw new Error(`${this.c.id}: ${k} is not an object`)
    return v as Record<string, unknown>
  }
}

const b64 = (b: Uint8Array) => toBase64(new Uint8Array(b) as Bytes)

/** sameAESKey says whether a non-extractable AES-GCM key is the raw key: both encrypt one block alike. */
async function sameAESKey(key: CryptoKey, raw: Uint8Array): Promise<boolean> {
  const probe = { name: 'AES-GCM', iv: new Uint8Array(12) }
  const want = await crypto.subtle.importKey('raw', new Uint8Array(raw), 'AES-GCM', false, ['encrypt'])
  const a = new Uint8Array(await crypto.subtle.encrypt(probe, key, new Uint8Array(16))) as Bytes
  const b = new Uint8Array(await crypto.subtle.encrypt(probe, want, new Uint8Array(16))) as Bytes
  return equal(a, b)
}

function aesKey(raw: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', new Uint8Array(raw), 'AES-GCM', false, ['encrypt', 'decrypt'])
}

/** withNonce runs f with the next 12-byte draw of crypto.getRandomValues replaced by nonce: a replay of a recorded seal. */
async function withNonce<T>(nonce: Uint8Array, f: () => Promise<T>): Promise<T> {
  const original = crypto.getRandomValues.bind(crypto)
  let used = false
  const spy = vi.spyOn(crypto, 'getRandomValues').mockImplementation(<A extends ArrayBufferView | null>(array: A): A => {
    if (!used && array instanceof Uint8Array && array.length === 12) {
      used = true
      array.set(nonce)
      return array
    }
    return original(array as never) as A
  })
  try {
    const result = await f()
    if (!used) throw new Error('the seal drew no nonce')
    return result
  } finally {
    spy.mockRestore()
  }
}

function errorOf(err: unknown): { code: string; reason?: string } {
  if (err instanceof MailieError) return { code: err.code }
  if (err instanceof AccountError) return { code: err.code, reason: err.reason }
  if (err instanceof SealError) return { code: err.code }
  if (isPlatformWrapError(err)) return { code: 'platform_wrap' }
  if (isPlatformError(err)) return { code: err.code }
  return { code: `unclassified: ${String(err)}` }
}

async function wrapKeyOf(input: Input): Promise<CryptoKey> {
  if (input.has('wrap_key_b64')) return aesKey(input.bytes('wrap_key_b64'))
  if (input.has('recovery_code')) return recoveryKey(mailieAccount, input.str('recovery_code'))
  if (input.has('password')) {
    const params = checkKDF(input.obj('params'), input.bytes('salt_b64'))
    return (await derive(mailieAccount, input.str('password'), input.bytes('salt_b64'), params)).wrapKey
  }
  throw new Error(`${input.c.id}: no wrap key`)
}

async function privateKeyOf(file: VectorFile, name: string) {
  const key = file.keys?.[name]
  if (!key) throw new Error(`no key ${name}`)
  return importPrivateKey(fromBase64(key.private_key_b64))
}

const ops: Record<string, Op> = {
  // The account.
  'mailie.normalise_address': async input => ({ address: normaliseAddress(input.str('address')) }),
  'mailie.account_profile': async () => {
    const b = mailieAccount.bounds!
    return {
      auth_label: mailieAccount.authLabel, wrap_label: mailieAccount.wrapLabel,
      recovery_key_label: mailieAccount.recoveryKeyLabel, recovery_proof_label: mailieAccount.recoveryProofLabel,
      wrap_header_b64: b64(new Uint8Array(mailieAccount.wrapHeader)), wrap_len: ACCOUNT_WRAP_LEN, legacy_v1: mailieAccount.legacyV1,
      wrap_aad_tag: ACCOUNT_WRAP_TAG, wrap_aad_version: ACCOUNT_WRAP_VERSION, wrap_kinds: ['password', 'recovery'],
      encoding: mailieAccount.encoding, password_preparation: PASSWORD_PROFILE, recovery_normalisation: 'platform-canonical',
      kdf: { ...DEFAULT_KDF },
      bounds: { min: { ...b.min }, max: { ...b.max }, max_cost: b.maxCost, min_salt_len: b.minSaltLen, max_salt_len: b.maxSaltLen },
      salt_label: 'mailie/v1/kdf-salt',
    }
  },
  'account.derive': async (input, _, c) => {
    const params = input.obj('params') as never
    const d = await derive(mailieAccount, input.str('password'), input.bytes('salt_b64'), params)
    const out = c.out!
    if (!(await sameAESKey(d.wrapKey, fromBase64(out.wrap_b64 as string)))) throw new Error('another wrap key')
    return { auth_b64: b64(fromBase64URL(d.authKey, 32)), auth_key: d.authKey, wrap_b64: out.wrap_b64 }
  },
  'mailie.recovery_code': async input => {
    const code = recoveryCodeFromBytes(input.bytes('bytes_b64'))
    return { code: code.canonical, display: code.display }
  },
  'account.normalise_recovery_code': async input => ({ code: mailieAccount.normaliseRecovery!(input.str('code')) }),
  'account.recovery_key': async (input, _, c) => {
    const key = await recoveryKey(mailieAccount, input.str('code'))
    if (!(await sameAESKey(key, fromBase64(c.out!.key_b64 as string)))) throw new Error('another recovery key')
    return { key_b64: c.out!.key_b64 }
  },
  'account.recovery_proof': async input => ({ proof: await recoveryProof(mailieAccount, input.str('code')) }),
  'mailie.account_wrap_aad': async input => ({ aad_b64: b64(accountWrapAAD(input.str('kind') as never, input.str('seal_id'), input.bytes('account_public_key_b64'))) }),
  'mailie.account_wrap': async input => {
    const kind = input.str('kind') as never
    const wrapKey = await aesKey(input.bytes('wrap_key_b64'))
    const key = input.bytes('account_key_b64')
    if (!input.has('nonce_b64')) {
      await sealAccountWrap(kind, wrapKey, key, input.str('seal_id'))
      return {}
    }
    const wrap = await withNonce(input.bytes('nonce_b64'), () => sealAccountWrap(kind, wrapKey, key, input.str('seal_id')))
    const pub = await publicFromPrivate(key)
    return { account_public_key_b64: b64(pub), aad_b64: b64(accountWrapAAD(kind, input.str('seal_id'), pub)), wrap_b64: b64(wrap) }
  },
  'mailie.account_unwrap': async input => {
    const key = await openAccountWrap(input.str('kind') as never, await wrapKeyOf(input), input.bytes('wrap_b64'), input.str('seal_id'), input.bytes('account_public_key_b64'))
    return { account_key_b64: b64(key) }
  },
  'mailie.account_wrap_shape': async input => {
    checkAccountWrapShape(input.bytes('wrap_b64'))
    return { accepted: true }
  },

  // The grants.
  'mailie.seal_profile': async () => ({
    magic_b64: b64(new Uint8Array(mailieSeal.magic)), label: mailieSeal.label, grant_kind: Kind.MailboxGrant, content_key_kind: Kind.ContentKey,
    grant_len: GRANT_LEN, min_epoch: MIN_EPOCH, max_epoch: MAX_EPOCH,
  }),
  'seal.kind_name': async input => ({ name: kindName(input.num('kind')) }),
  'mailie.grant_row': async input => ({ row: formatUUID(await grantRow(input.str('namespace'), input.str('seal_id'), input.num('epoch'))) }),
  'mailie.grant_info': async input => {
    const info = grantInfo(input.str('namespace'), input.num('epoch'))
    return { info_b64: b64(info), info: new TextDecoder().decode(info) }
  },
  'mailie.grant_aad': async input => ({ aad_b64: b64(await grantAAD(input.str('namespace'), input.str('seal_id'), input.num('epoch'))) }),
  'mailie.grant_seal': async (input, file, c) => {
    const [pub, ns, sealID, epoch, key] = [input.bytes('recipient_public_key_b64'), input.str('namespace'), input.str('seal_id'), input.num('epoch'), input.bytes('mailbox_key_b64')]
    if (!input.has('recipient')) {
      await sealGrant(pub, ns, sealID, epoch, key)
      return {}
    }
    // Go's grant, sealed under a seed this side cannot replay, opened here;
    // then one sealed here, opened here.
    const recipient = await privateKeyOf(file, input.str('recipient'))
    const mailboxPub = await publicFromPrivate(key)
    const recorded = fromBase64(c.out!.grant_b64 as string)
    checkGrantShape(recorded, epoch)
    const fromGo = await openGrant(recipient, ns, sealID, epoch, mailboxPub, recorded)
    const ours = await sealGrant(pub, ns, sealID, epoch, key)
    checkGrantShape(ours, epoch)
    const fromTS = await openGrant(recipient, ns, sealID, epoch, mailboxPub, ours)
    if (!equal(fromGo, key) || !equal(fromTS, key)) throw new Error('a grant opens to another key')
    return { grant_b64: c.out!.grant_b64, mailbox_public_key_b64: b64(mailboxPub) }
  },
  'mailie.grant_open': async (input, file) => {
    const key = await openGrant(await privateKeyOf(file, input.str('key')), input.str('namespace'), input.str('seal_id'), input.num('epoch'),
      input.bytes('mailbox_public_key_b64'), input.bytes('grant_b64'))
    return { mailbox_key_b64: b64(key) }
  },
  'mailie.grant_shape': async input => {
    checkGrantShape(input.bytes('grant_b64'), input.num('epoch'))
    return { accepted: true }
  },

  // The wrap under the product key.
  'mailie.product_key': async input => {
    const k = await deriveProductKey(input.bytes('root_b64'), 'mailie', input.num('epoch'))
    return { product_key_b64: b64(k.sk), product_public_key_b64: b64(k.pub), product_key_id: k.id }
  },
  'mailie.platform_wrap_binding': async input => {
    const b = platformWrapBinding(input.str('seal_id'), input.str('sub'), input.num('product_key_epoch'), input.bytes('account_public_key_b64'))
    return { user_id: b.userId, sub: b.sub, product_key_id: b.productKeyId }
  },
  'mailie.platform_wrap_info': async input => ({ info_b64: b64(platformWrapInfo(mailiePlatformWrap, wrapBinding(input))) }),
  'mailie.platform_wrap_aad': async input => ({ aad_b64: b64(platformWrapAAD(mailiePlatformWrap, wrapBinding(input))) }),
  'mailie.platform_wrap_seal': async (input, _, c) => {
    const seal = () => sealMailiePlatformWrap(input.bytes('product_key_b64'), input.bytes('account_key_b64'), wrapBinding(input))
    const wrap = c.error ? await seal() : await withNonce(input.bytes('nonce_b64'), seal)
    // K_pw is a non-extractable CryptoKey here; Go proves k_pw_b64.
    return { wrap_b64: b64(wrap), k_pw_b64: c.out?.k_pw_b64 }
  },
  'mailie.platform_wrap_open': async input => ({
    account_key_b64: b64(await openMailiePlatformWrap(input.bytes('product_key_b64'), input.bytes('wrap_b64'), wrapBinding(input))),
  }),

  // The browser vault.
  'mailie.browser_vault_profile': async () => ({ tag: mailieBrowserVault.tag, version: mailieBrowserVault.version }),
  'mailie.browser_vault_aad': async input => ({ aad_b64: b64(browserVaultAAD(input.str('seal_id'), input.bytes('account_public_key_b64'))) }),
}

function wrapBinding(input: Input) {
  return { userId: input.str('user_id'), sub: input.str('sub'), productKeyId: input.str('product_key_id'), accountPublicKey: input.bytes('account_public_key_b64') }
}

/** runFile runs every case of a file marked for TypeScript and returns what went wrong, case by case. */
async function runFile(name: string): Promise<{ ran: number; failures: string[] }> {
  const file = vectors(name)
  const failures: string[] = []
  let ran = 0
  for (const c of file.cases) {
    if (c.langs && !c.langs.includes('ts')) continue
    const op = ops[c.op]
    if (!op) { failures.push(`${c.id}: no handler for ${c.op}`); continue }
    ran++
    let out: Out | undefined
    let caught: unknown
    try { out = await op(new Input(c), file, c) } catch (err) { caught = err }
    if (c.error) {
      if (caught === undefined) { failures.push(`${c.id}: no error, want ${c.error} ${c.reason ?? ''}`); continue }
      const got = errorOf(caught)
      if (got.code !== c.error || (got.reason ?? '') !== (c.reason ?? '')) failures.push(`${c.id}: ${got.code} ${got.reason ?? ''}, want ${c.error} ${c.reason ?? ''}`)
      continue
    }
    if (caught !== undefined) { failures.push(`${c.id}: ${String(caught)}`); continue }
    try { expect(out).toEqual(c.out) } catch { failures.push(`${c.id}: ${JSON.stringify(out)}, want ${JSON.stringify(c.out)}`) }
  }
  return { ran, failures }
}

afterEach(() => { vi.restoreAllMocks() })

describe('the key scheme against the Go side’s vectors', () => {
  it('derives, wraps and opens the account key as the Go side does, and refuses what it refuses', async () => {
    const { ran, failures } = await runFile('account-go.json')
    expect(failures).toEqual([])
    expect(ran).toBeGreaterThan(70)
  }, 180_000)

  it('opens the Go side’s grants, seals its own, and refuses another person, mailbox, epoch, kind and a low-order key', async () => {
    const { ran, failures } = await runFile('grant-go.json')
    expect(failures).toEqual([])
    expect(ran).toBeGreaterThan(90)
  }, 60_000)

  it('wraps the account key under the product key with Mailie’s labels and the seal id, and never opens Wappie’s wrap as Mailie’s', async () => {
    const { ran, failures } = await runFile('platform-wrap-go.json')
    expect(failures).toEqual([])
    expect(ran).toBeGreaterThan(25)
  }, 60_000)

  it('binds the browser vault to the seal id and the public key, as the Go side does', async () => {
    const { ran, failures } = await runFile('browser-vault-go.json')
    expect(failures).toEqual([])
    expect(ran).toBe(6)
  })
})

describe('the key scheme in the browser', () => {
  const sealID = 'd1a4c6e8-2b3f-4a5d-9e7f-0a1b2c3d4e5f'

  it('keeps the account key at rest only for the person and the public key it was sealed for', async () => {
    const key = crypto.getRandomValues(new Uint8Array(32)) as Bytes
    const pub = await publicFromPrivate(key)
    const record = await sealBrowserVault(key, pub, sealID)
    expect((await openBrowserVault(record, sealID)).publicRaw).toEqual(pub)
    await expect(openBrowserVault(record, 'd1a4c6e8-2b3f-4a5d-9e7f-0a1b2c3d4e50')).rejects.toMatchObject({ code: 'vault' })
    const other = await publicFromPrivate(crypto.getRandomValues(new Uint8Array(32)) as Bytes)
    await expect(openBrowserVault({ ...record, publicRaw: other }, sealID)).rejects.toMatchObject({ code: 'vault' })
    await expect(openBrowserVault({ ...record, nonce: new Uint8Array(11) as Bytes }, sealID)).rejects.toMatchObject({ code: 'vault' })
    expect(() => browserVaultAAD('ana@example.com', pub)).toThrow(MailieError)
    await expect(sealBrowserVault(key.subarray(0, 31), pub, sealID)).rejects.toMatchObject({ code: 'binding' })
  })

  it('reads the raw account key back only for its person, and refuses anyone else with the kit’s vault error the console wipes on', async () => {
    const key = crypto.getRandomValues(new Uint8Array(32)) as Bytes
    const pub = await publicFromPrivate(key)
    const record = await sealBrowserVault(key, pub, sealID)
    expect(await openBrowserVaultKey(record, sealID)).toEqual(key)
    const other = openBrowserVaultKey(record, 'd1a4c6e8-2b3f-4a5d-9e7f-0a1b2c3d4e50')
    await expect(other).rejects.toBeInstanceOf(MailieError)
    await expect(other).rejects.toMatchObject({ code: 'vault' })
    await expect(openBrowserVaultKey(record, 'ana@example.com')).rejects.toMatchObject({ code: 'binding' })
  })

  it('keys an address as the server stores it, so another spelling of an enrolled address is the same address', () => {
    const key = normaliseAddress('ana@example.com')
    for (const typed of ['Ana@Example.COM', '  ana@example.com\t', 'ANA@EXAMPLE.COM\r\n']) expect(normaliseAddress(typed)).toBe(key)
    expect(normaliseAddress('bo@example.com')).not.toBe(key)
    // Not ECMAScript's own case mapping of a whole string, which ends a word in a final sigma.
    const sigmas = String.fromCodePoint(0x3a3, 0x3a3) + '@example.com'
    expect(normaliseAddress(sigmas)).toBe(String.fromCodePoint(0x3c3, 0x3c3) + '@example.com')
    expect(normaliseAddress(sigmas)).not.toBe(sigmas.toLowerCase())
  })

  it('wraps a fresh account key that opens under its own binding only', async () => {
    const key = crypto.getRandomValues(new Uint8Array(32)) as Bytes
    const pub = await publicFromPrivate(key)
    const wrapKey = await aesKey(crypto.getRandomValues(new Uint8Array(32)))
    const wrap = await sealAccountWrap('password', wrapKey, key, sealID)
    expect(wrap.length).toBe(ACCOUNT_WRAP_LEN)
    expect(wrap[0]).toBe(ACCOUNT_WRAP_HEADER)
    expect(await openAccountWrap('password', wrapKey, wrap, sealID, pub)).toEqual(key)
    await expect(openAccountWrap('recovery', wrapKey, wrap, sealID, pub)).rejects.toMatchObject({ code: 'wrap', reason: 'wrong_key' })
  })

  it('refuses a new password under twelve code points before anything is derived, and keeps no minimum for one being presented', () => {
    expect(() => prepareNewPassword('eleven char')).toThrow(expect.objectContaining({ code: 'password_too_short' }))
    expect(() => prepareNewPassword('twelve chars')).not.toThrow()
    expect(() => prepareNewPassword('twelve\u0007chars')).toThrow(expect.objectContaining({ code: 'password_invalid' }))
    expect(() => prepareNewPassword('a'.repeat(257))).toThrow(expect.objectContaining({ code: 'password_too_long' }))
    expect(mailieAccount.prepare!('short')).toEqual(new TextEncoder().encode('short'))
  })

  it('draws namespaces and recognises seal ids in their one spelling', () => {
    const ns = newNamespace()
    expect(isNamespace(ns)).toBe(true)
    for (const bad of [ns.toUpperCase(), `{${ns}}`, `urn:uuid:${ns}`, '019a8b2c-3d4e-7f60-8a71-b2c3d4e5f607', '00000000-0000-0000-0000-000000000000', ''])
      expect(isSealID(bad)).toBe(false)
  })

  it('names its profile as the specification does', () => {
    expect([PASSWORD_AUTH_LABEL, PASSWORD_WRAP_LABEL, RECOVERY_WRAP_LABEL, RECOVERY_AUTH_LABEL]).toEqual([
      'mailie/v1/password/auth', 'mailie/v1/password/wrap', 'mailie/v1/recovery/wrap', 'mailie/v1/recovery/auth'])
    expect([Kind.ContentKey, Kind.MailboxGrant, Kind.UserWrap]).toEqual([KIND_CONTENT_KEY, KIND_GRANT, KIND_USER_WRAP])
    expect(String.fromCharCode(...SEAL_MAGIC)).toBe('ML')
    expect(SEAL_LABEL).toBe('mlv1')
    expect([BROWSER_VAULT_TAG, BROWSER_VAULT_VERSION]).toEqual(['mailie/browser-account-key', 1])
    expect(SALT_LEN).toBe(16)
    expect(() => checkKDF({ ...DEFAULT_KDF, m: 65536.5 }, new Uint8Array(16) as Bytes)).toThrow(AccountError)
    expect(() => checkKDF({ ...DEFAULT_KDF, extra: 1 }, new Uint8Array(16) as Bytes)).toThrow(AccountError)
    expect(checkKDF({ ...DEFAULT_KDF }, new Uint8Array(16) as Bytes)).toEqual(DEFAULT_KDF)
  })
})
