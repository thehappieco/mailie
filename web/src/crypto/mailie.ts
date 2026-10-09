// Mailie's profile of the kit's key scheme, for the browser: the labels,
// headers, magic, kinds and additional data with which a person's account key
// is wrapped, a mailbox's private key is granted to a person, and the account
// key is kept in this browser. ../../../docs/key-scheme.md is the
// specification and key-scheme-threat-model.md beside it what it defends
// against. The Go side is internal/keyscheme, which writes the vectors
// web/test/keyscheme.spec.ts opens with this module.
//
// Parameters and the checks around them only, never a primitive of its own:
// Argon2id and the auth/wrap split are the kit's account module, a grant is
// its sealed envelope's direct mode at grantRow, the wrap under the product
// key is its platformwrap under its Mailie profile, the password preparation
// and the recovery code are its platform profile's, and the browser vault is
// its browserAccount. Nothing here is called by the console yet: the account
// and mailbox ceremonies that use it come with the routes that serve them.

import {
  AccountError,
  checkKDFParams,
  checkSalt,
  unwrapPrivateKey,
  wrapPrivateKey,
  type AccountProfile,
  type KDFParams,
} from '@thehappieco/kit/account'
import {
  openBrowserAccountKey,
  sealBrowserAccountKey,
  browserAccountAAD,
  type BrowserAccountProfile,
  type BrowserKeyEnvelope,
} from '@thehappieco/kit/browserAccount'
import { encodeUTF8, equal, parseUUID, toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { publicFromPrivate, type PrivateKey } from '@thehappieco/kit/hpke'
import { canonicalJSON } from '@thehappieco/kit/jcs'
import { checkPlatformWrapShape, openPlatformWrap, sealPlatformWrap, type PlatformWrapBinding } from '@thehappieco/kit/platformwrap'
import { mailiePlatformWrap, PLATFORM_WRAP_PRODUCT } from '@thehappieco/kit/profiles/mailie'
import {
  canonicalRecoveryCode,
  KDF_BOUNDS,
  preparePassword,
  productKeyId,
  SALT_LEN,
} from '@thehappieco/kit/profiles/platform/core'
import {
  aad as sealAAD,
  DIRECT_OVERHEAD,
  info as sealInfo,
  MODE_DIRECT,
  openDirect,
  parseHeader,
  grantRow as kitGrantRow,
  SealError,
  sealDirect,
  SUITE_V1,
  VERSION,
  type SealProfile,
} from '@thehappieco/kit/seal'

export { AccountError, SealError }
export type { Bytes, PrivateKey }

const KEY_LEN = 32

/**
 * KeySchemeError is what this profile refuses on its own: an input outside
 * its spelling (binding), a stored value of the wrong shape (shape), or a
 * browser vault record that does not open for the person named (vault),
 * which the console wipes.
 */
export class KeySchemeError extends Error {
  constructor(
    message: string,
    readonly code: 'binding' | 'shape' | 'vault',
  ) {
    super(message)
    this.name = 'KeySchemeError'
  }
}

function binding(message: string): never {
  throw new KeySchemeError(message, 'binding')
}

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

/**
 * isSealID says whether id is a seal id in its one spelling: a version 4 UUID
 * of the RFC 9562 variant as 36 characters of lowercase hyphenated text. The
 * server draws a person's seal id once (users.seal_id); every wrap and grant
 * binds it, never the address.
 */
export function isSealID(id: unknown): id is string {
  return typeof id === 'string' && UUID_V4.test(id)
}

/** isNamespace says whether ns is a mailbox namespace in its one spelling, the same as a seal id's. */
export function isNamespace(ns: unknown): ns is string {
  return isSealID(ns)
}

/** newNamespace draws a mailbox's namespace: a random UUIDv4. */
export function newNamespace(): string {
  return crypto.randomUUID()
}

// The white space Go's strings.TrimSpace removes (unicode.IsSpace), by code
// point. Not ECMAScript's trim, which also removes U+FEFF and keeps U+0085.
const GO_WHITE_SPACE: ReadonlySet<number> = new Set([
  0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680,
  0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
  0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
])

/**
 * lowerCodePoint is Unicode's simple lowercase mapping of one code point, as
 * Go's unicode.ToLower: ECMAScript's toLowerCase on the code point alone,
 * which applies no context (a capital sigma is always σ, never the final ς
 * toLowerCase gives a whole word), except U+0130, whose full mapping adds a
 * combining dot that the simple one does not.
 */
function lowerCodePoint(cp: number): string {
  if (cp === 0x130) return 'i'
  const one = String.fromCodePoint(cp)
  const lower = one.toLowerCase()
  return Array.from(lower).length === 1 ? lower : one
}

/**
 * normaliseAddress is an address as the server stores it (docs/key-scheme.md
 * section 2; Go keyscheme.NormaliseAddress, held to the same vectors): white
 * space trimmed at both ends as Go's strings.TrimSpace does, and every letter
 * lowered by its simple mapping, as Go's strings.ToLower does. The browser
 * keys its memory of the addresses that enrolled with it, so every spelling
 * the server takes for one account is one record. It validates nothing.
 */
export function normaliseAddress(address: string): string {
  const cps = Array.from(address, c => c.codePointAt(0) ?? 0)
  let start = 0
  let end = cps.length
  while (start < end && GO_WHITE_SPACE.has(cps[start] ?? 0)) start++
  while (end > start && GO_WHITE_SPACE.has(cps[end - 1] ?? 0)) end--
  let out = ''
  for (let i = start; i < end; i++) out += lowerCodePoint(cps[i] ?? 0)
  return out
}

function key32(name: string, k: Uint8Array): void {
  if (!(k instanceof Uint8Array) || k.length !== KEY_LEN) binding(`${name} is not ${KEY_LEN} bytes`)
}

// ---------------------------------------------------------------------------
// The account (docs/key-scheme.md section 5)
// ---------------------------------------------------------------------------

export const PASSWORD_AUTH_LABEL = 'mailie/v1/password/auth'
export const PASSWORD_WRAP_LABEL = 'mailie/v1/password/wrap'
export const RECOVERY_WRAP_LABEL = 'mailie/v1/recovery/wrap'
export const RECOVERY_AUTH_LABEL = 'mailie/v1/recovery/auth'
export const ACCOUNT_WRAP_TAG = 'mailie/account-wrap'
export const ACCOUNT_WRAP_VERSION = 1
/** The first byte of a password or recovery wrap; the platform wrap's is 0x03 and a grant's the seal magic 'M'. */
export const ACCOUNT_WRAP_HEADER = 0x02
/** The header, a 12-byte nonce, the 32-byte account key and a 16-byte tag. */
export const ACCOUNT_WRAP_LEN = 61
// A password is prepared by the platform profile's preparation, for a
// password being presented (no minimum length); a new one is checked with
// preparePassword(password, { isNew: true }). Its name is not re-exported:
// it spells the kit's platform, which the open console's build never names
// (web/test/hosted.ts).
export { SALT_LEN }

/** prepareNewPassword refuses a new password the profile would not take (12 to 256 code points, no control character), before anything is derived. */
export function prepareNewPassword(password: string): void {
  preparePassword(password, { isNew: true }).fill(0)
}

export type WrapKind = 'password' | 'recovery'

/** DEFAULT_KDF is what new accounts derive with: the floor of the bounds. */
export const DEFAULT_KDF: Readonly<KDFParams> = Object.freeze({ alg: 'argon2id', m: KDF_BOUNDS.m.floor, t: KDF_BOUNDS.t.floor, p: KDF_BOUNDS.p.floor })

/**
 * mailieAccount is Mailie's profile for the kit's account module: its
 * labels, the platform's preparation of a presented password, the platform's
 * KDF bounds, base64url text, the platform's canonical recovery code, and the
 * one-byte header 0x02 with no legacy form. derive(mailieAccount, password,
 * salt, kdf) gives the auth key to send and the wrap key that never leaves.
 */
export const mailieAccount: AccountProfile = Object.freeze({
  authLabel: PASSWORD_AUTH_LABEL,
  wrapLabel: PASSWORD_WRAP_LABEL,
  recoveryKeyLabel: RECOVERY_WRAP_LABEL,
  recoveryProofLabel: RECOVERY_AUTH_LABEL,
  wrapHeader: Object.freeze([ACCOUNT_WRAP_HEADER]),
  legacyV1: false,
  encoding: 'base64url',
  prepare: (password: string) => preparePassword(password, { isNew: false }),
  bounds: Object.freeze({
    min: Object.freeze({ m: KDF_BOUNDS.m.floor, t: KDF_BOUNDS.t.floor, p: KDF_BOUNDS.p.floor }),
    max: Object.freeze({ m: KDF_BOUNDS.m.ceiling, t: KDF_BOUNDS.t.ceiling, p: KDF_BOUNDS.p.ceiling }),
    maxCost: KDF_BOUNDS.mt,
    minSaltLen: SALT_LEN,
    maxSaltLen: SALT_LEN,
  }),
  normaliseRecovery: canonicalRecoveryCode,
} as const)

/**
 * checkKDF throws unless what a server answered is a KDF this profile derives
 * with: the kit's bounds, after refusing members other than alg, m, t and p
 * and parameters that are not integers, which the bounds alone would let
 * through to Argon2id.
 */
export function checkKDF(kdf: unknown, salt: Bytes): KDFParams {
  if (typeof kdf !== 'object' || kdf === null || Array.isArray(kdf)) throw new AccountError('not KDF parameters', 'kdf', 'out_of_bounds')
  const o = kdf as Record<string, unknown>
  if (Object.keys(o).some(k => !['alg', 'm', 't', 'p'].includes(k)) || typeof o.alg !== 'string' || ![o.m, o.t, o.p].every(Number.isSafeInteger)) {
    throw new AccountError('not KDF parameters', 'kdf', 'out_of_bounds')
  }
  const params: KDFParams = { alg: o.alg, m: o.m as number, t: o.t as number, p: o.p as number }
  checkKDFParams(mailieAccount, params)
  checkSalt(mailieAccount, salt)
  return params
}

/**
 * accountWrapAAD is the additional data of an account wrap:
 * JCS(["mailie/account-wrap", 1, kind, seal_id, base64url(account public key)]).
 * The seal id, never the address, so an address change needs no re-wrap.
 */
export function accountWrapAAD(kind: WrapKind, sealID: string, accountPublicKey: Uint8Array): Bytes {
  if (kind !== 'password' && kind !== 'recovery') binding('a wrap kind is password or recovery')
  if (!isSealID(sealID)) binding('the seal id is not a lowercase UUIDv4')
  key32('the account public key', accountPublicKey)
  return encodeUTF8(canonicalJSON([ACCOUNT_WRAP_TAG, ACCOUNT_WRAP_VERSION, kind, sealID, toBase64URL(new Uint8Array(accountPublicKey) as Bytes)]))
}

/**
 * sealAccountWrap wraps the 32-byte account key under a wrap key (derive's
 * wrapKey, or recoveryKey(mailieAccount, code)): 0x02, a fresh nonce, and
 * AES-256-GCM under accountWrapAAD; 61 bytes. It opens what it made and
 * compares before it returns it. The account key is the caller's to zero.
 */
export async function sealAccountWrap(kind: WrapKind, wrapKey: CryptoKey, accountKey: Uint8Array, sealID: string): Promise<Bytes> {
  key32('the account key', accountKey)
  const raw = new Uint8Array(accountKey) as Bytes
  try {
    const pub = await publicFromPrivate(raw)
    const wrap = await wrapPrivateKey(mailieAccount, raw, wrapKey, accountWrapAAD(kind, sealID, pub))
    const again = await openAccountWrap(kind, wrapKey, wrap, sealID, pub)
    const same = equal(again, raw)
    again.fill(0)
    if (!same) throw new AccountError('the new wrap failed its self-test', 'wrap', 'wrong_key')
    return wrap
  } finally {
    raw.fill(0)
  }
}

/**
 * openAccountWrap opens an account wrap and returns the account key, which
 * the caller zeroes. The binding first (KeySchemeError binding); then a wrap
 * shorter than 61 bytes is truncated, and one longer, one not starting with
 * 0x02, one that does not authenticate, and one that opens to anything but the
 * private half of accountPublicKey are all wrong_key (AccountError wrap).
 */
export async function openAccountWrap(kind: WrapKind, wrapKey: CryptoKey, wrap: Uint8Array, sealID: string, accountPublicKey: Uint8Array): Promise<Bytes> {
  const aad = accountWrapAAD(kind, sealID, accountPublicKey)
  if (!(wrap instanceof Uint8Array) || wrap.length < ACCOUNT_WRAP_LEN) throw new AccountError('the wrap is truncated', 'wrap', 'truncated')
  if (wrap.length !== ACCOUNT_WRAP_LEN || wrap[0] !== ACCOUNT_WRAP_HEADER) throw new AccountError('not an account wrap', 'wrap', 'wrong_key')
  const { privateKey } = await unwrapPrivateKey(mailieAccount, new Uint8Array(wrap) as Bytes, wrapKey, aad)
  let ok = false
  try {
    ok = privateKey.length === KEY_LEN && equal(await publicFromPrivate(privateKey), new Uint8Array(accountPublicKey) as Bytes)
  } catch {
    ok = false
  }
  if (!ok) {
    privateKey.fill(0)
    throw new AccountError('the wrap names another account key', 'wrap', 'wrong_key')
  }
  return privateKey
}

/** checkAccountWrapShape is the server's check of a wrap it cannot open: 61 bytes starting with 0x02. */
export function checkAccountWrapShape(wrap: Uint8Array): void {
  if (!(wrap instanceof Uint8Array) || wrap.length !== ACCOUNT_WRAP_LEN || wrap[0] !== ACCOUNT_WRAP_HEADER) {
    throw new KeySchemeError('not an account wrap', 'shape')
  }
}

// ---------------------------------------------------------------------------
// The seal domain and the grants (docs/key-scheme.md sections 9 and 10)
// ---------------------------------------------------------------------------

/** Kind is what a sealed value is. Phase 3 seals only MailboxGrant; the others named are reserved for phase 4. */
export enum Kind {
  Headers = 0x01,
  Snippet = 0x02,
  Body = 0x03,
  AttachmentKey = 0x04,
  SearchIndex = 0x05,
  ContentKey = 0x06, // the kit's KIND_CONTENT_KEY
  MailboxGrant = 0x07, // the kit's KIND_GRANT
  UserWrap = 0x08, // the kit's KIND_USER_WRAP, never sealed
  FolderName = 0x09,
  Draft = 0x0a,
}

// The names go into a direct envelope's HPKE info: wire format.
const kindNames: Readonly<Record<number, string>> = Object.freeze({
  [Kind.Headers]: 'headers',
  [Kind.Snippet]: 'snippet',
  [Kind.Body]: 'body',
  [Kind.AttachmentKey]: 'attachment_key',
  [Kind.SearchIndex]: 'search_index',
  [Kind.ContentKey]: 'content_key',
  [Kind.MailboxGrant]: 'mailbox_grant',
  [Kind.UserWrap]: 'user_wrap',
  [Kind.FolderName]: 'folder_name',
  [Kind.Draft]: 'draft',
})

/** kindName matches Go's Kind.String: kind(0x..) without leading zeros for an unnamed byte. */
export function kindName(kind: number): string {
  return kindNames[kind] ?? `kind(0x${kind.toString(16)})`
}

export const SEAL_MAGIC: readonly [number, number] = Object.freeze([0x4d, 0x4c] as const) // "ML"
export const SEAL_LABEL = 'mlv1'
/** mailieSeal is Mailie's envelope: magic "ML", label "mlv1", its kinds' names. */
export const mailieSeal: SealProfile = Object.freeze({ magic: SEAL_MAGIC, label: SEAL_LABEL, kindName })

export const MIN_EPOCH = 1
export const MAX_EPOCH = 0xffff
/** A grant: the 8-byte header, the 32-byte encapsulated key, and the 32-byte mailbox key with its tag. */
export const GRANT_LEN = DIRECT_OVERHEAD + KEY_LEN

function mailboxBinding(namespace: string, epoch: number): void {
  if (!isNamespace(namespace)) binding('the namespace is not a lowercase UUIDv4')
  if (!Number.isInteger(epoch) || epoch < MIN_EPOCH || epoch > MAX_EPOCH) binding(`an epoch is from ${MIN_EPOCH} to ${MAX_EPOCH}`)
}

function grantBinding(namespace: string, sealID: string, epoch: number): void {
  mailboxBinding(namespace, epoch)
  if (!isSealID(sealID)) binding('the seal id is not a lowercase UUIDv4')
}

/** grantRow is the kit's grantRow with the namespace as tenant and device: Row(namespace, namespace ‖ seal_id ‖ u16be(epoch)). */
export async function grantRow(namespace: string, sealID: string, epoch: number): Promise<Bytes> {
  grantBinding(namespace, sealID, epoch)
  const ns = parseUUID(namespace)
  return kitGrantRow(ns, ns, parseUUID(sealID), epoch)
}

/** grantInfo is a grant's HPKE info: "mlv1/mailbox_grant/<namespace>/<epoch>". */
export function grantInfo(namespace: string, epoch: number): Bytes {
  mailboxBinding(namespace, epoch)
  return sealInfo(mailieSeal, Kind.MailboxGrant, parseUUID(namespace), epoch)
}

/** grantAAD is a grant's additional data: "mlv1" ‖ 0x07 ‖ namespace ‖ grantRow ‖ the direct header at that epoch. */
export async function grantAAD(namespace: string, sealID: string, epoch: number): Promise<Bytes> {
  const row = await grantRow(namespace, sealID, epoch)
  const header = new Uint8Array([SEAL_MAGIC[0], SEAL_MAGIC[1], VERSION, SUITE_V1, MODE_DIRECT, epoch >> 8, epoch & 0xff, 0]) as Bytes
  return sealAAD(mailieSeal, Kind.MailboxGrant, parseUUID(namespace), row, header)
}

/**
 * sealGrant seals a mailbox's 32-byte private key to a person's account
 * public key: the kit's direct envelope, kind 0x07, at grantRow, at the
 * mailbox key's epoch; 88 bytes. A low-order public key is refused
 * (SealError invalid_key): whoever handed it out could open the grant. The
 * recipient's key is the one the server serves for them; nothing here can
 * check it further (docs/key-scheme-threat-model.md, section 5.3).
 */
export async function sealGrant(recipientPublicKey: Uint8Array, namespace: string, sealID: string, epoch: number, mailboxKey: Uint8Array): Promise<Bytes> {
  grantBinding(namespace, sealID, epoch)
  key32('the mailbox key', mailboxKey)
  const row = await grantRow(namespace, sealID, epoch)
  return sealDirect(mailieSeal, new Uint8Array(recipientPublicKey) as Bytes, Kind.MailboxGrant, parseUUID(namespace), row, epoch, new Uint8Array(mailboxKey) as Bytes)
}

/**
 * openGrant opens a grant with the person's account key and returns the
 * mailbox's private key, which the caller zeroes. The kit's header checks
 * come first (short, magic, version, suite, mode); everything else is
 * authentication: another person, mailbox, epoch or kind, a changed byte, and
 * a grant that opens to anything but the private half of mailboxPublicKey,
 * the key the server holds for the mailbox at that epoch. HPKE's base mode
 * does not authenticate the sender, so that last check is what a grant sealed
 * by someone who knows only the person's public key cannot pass.
 */
export async function openGrant(account: PrivateKey, namespace: string, sealID: string, epoch: number, mailboxPublicKey: Uint8Array, grant: Uint8Array): Promise<Bytes> {
  grantBinding(namespace, sealID, epoch)
  key32('the mailbox public key', mailboxPublicKey)
  const row = await grantRow(namespace, sealID, epoch)
  const key = await openDirect(mailieSeal, account, Kind.MailboxGrant, parseUUID(namespace), row, new Uint8Array(grant) as Bytes)
  let ok = false
  try {
    ok = key.length === KEY_LEN && equal(await publicFromPrivate(key), new Uint8Array(mailboxPublicKey) as Bytes)
  } catch {
    ok = false
  }
  if (!ok) {
    key.fill(0)
    throw new SealError('authentication failed', 'authentication')
  }
  return key
}

/** checkGrantShape is the server's check of a grant it cannot open: 88 bytes, Mailie's header in direct mode at the current epoch. */
export function checkGrantShape(grant: Uint8Array, epoch: number): void {
  const shape = () => new KeySchemeError('not a grant at the current epoch', 'shape')
  if (!(grant instanceof Uint8Array) || grant.length !== GRANT_LEN) throw shape()
  let h
  try {
    h = parseHeader(mailieSeal, new Uint8Array(grant) as Bytes)
  } catch {
    throw shape()
  }
  if (h.mode !== MODE_DIRECT || h.epoch !== epoch || grant[7] !== 0) throw shape()
}

// ---------------------------------------------------------------------------
// The wrap under the product key (docs/key-scheme.md section 6)
// ---------------------------------------------------------------------------

export { checkPlatformWrapShape, mailiePlatformWrap }

/**
 * platformWrapBinding is what Mailie binds a platform wrap to: the seal id as
 * the product's user id, id.'s sub, the pinned product key id
 * "mailie:<epoch>", and the account public key the server holds. The user
 * id is the seal id for every person, never the sub, which departs from the
 * kit's rule for an account created through id. (docs/key-scheme.md section
 * 6.1): a seal id equal to the sub is refused, whatever the sub's version.
 */
export function platformWrapBinding(sealID: string, sub: string, productKeyEpoch: number, accountPublicKey: Uint8Array): PlatformWrapBinding {
  if (!isSealID(sealID)) binding('the seal id is not a lowercase UUIDv4')
  if (sealID === sub) binding('the user id is the seal id, never the sub')
  if (!Number.isInteger(productKeyEpoch) || productKeyEpoch < 1 || productKeyEpoch > 2 ** 31 - 1) binding('a product key epoch is from 1 to 2^31 - 1')
  return { userId: sealID, sub, productKeyId: productKeyId(PLATFORM_WRAP_PRODUCT, productKeyEpoch), accountPublicKey }
}

/** sealMailiePlatformWrap is the kit's sealPlatformWrap under Mailie's labels. */
export function sealMailiePlatformWrap(productKey: Uint8Array, accountKey: Uint8Array, b: PlatformWrapBinding): Promise<Bytes> {
  return sealPlatformWrap(mailiePlatformWrap, productKey, accountKey, b)
}

/** openMailiePlatformWrap is the kit's openPlatformWrap under Mailie's labels. */
export function openMailiePlatformWrap(productKey: Uint8Array, wrap: Uint8Array, b: PlatformWrapBinding): Promise<Bytes> {
  return openPlatformWrap(mailiePlatformWrap, productKey, wrap, b)
}

// ---------------------------------------------------------------------------
// The browser vault (docs/key-scheme.md section 7)
// ---------------------------------------------------------------------------

export const BROWSER_VAULT_TAG = 'mailie/browser-account-key'
export const BROWSER_VAULT_VERSION = 1
export const mailieBrowserVault: BrowserAccountProfile = Object.freeze({ tag: BROWSER_VAULT_TAG, version: BROWSER_VAULT_VERSION })

/**
 * browserVaultAAD is JCS(["mailie/browser-account-key", 1, seal_id, base64(account public key)]):
 * the kit's JSON AAD of its key at rest, not the restricted one of the other
 * bindings, since standard base64's '+', '/' and '=' are outside its alphabet.
 */
export function browserVaultAAD(sealID: string, accountPublicKey: Uint8Array): Bytes {
  if (!isSealID(sealID)) binding('the seal id is not a lowercase UUIDv4')
  key32('the account public key', accountPublicKey)
  return browserAccountAAD(mailieBrowserVault, sealID, new Uint8Array(accountPublicKey) as Bytes)
}

/** sealBrowserVault keeps the account key at rest under a fresh non-extractable AES key, bound to the seal id and the public key. */
export async function sealBrowserVault(accountKey: Uint8Array, accountPublicKey: Uint8Array, sealID: string): Promise<BrowserKeyEnvelope> {
  key32('the account key', accountKey)
  browserVaultAAD(sealID, accountPublicKey)
  return sealBrowserAccountKey(mailieBrowserVault, new Uint8Array(accountKey) as Bytes, new Uint8Array(accountPublicKey) as Bytes, sealID)
}

/**
 * openBrowserVault opens the record for the person the server says is signed
 * in. The caller first compares the record's public key with the one the
 * server holds for the person (users.public_key); a record of anyone else is
 * wiped, never opened. A record that does not open (another seal id, another
 * public key, not a record at all) is KeySchemeError vault, never the kit's
 * own error, so the console has one code to wipe it on.
 */
export async function openBrowserVault(envelope: BrowserKeyEnvelope, sealID: string): Promise<PrivateKey> {
  if (!isSealID(sealID)) binding('the seal id is not a lowercase UUIDv4')
  try {
    return await openBrowserAccountKey(mailieBrowserVault, envelope, sealID)
  } catch {
    throw new KeySchemeError('the vault record does not open for this person', 'vault')
  }
}
