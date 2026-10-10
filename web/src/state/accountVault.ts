// Where this browser keeps the person's account key between page loads, and
// which addresses it has seen enrol (docs/key-scheme.md sections 7 and 12.7).
//
// The vault is the kit's key at rest under Mailie's profile
// (crypto/mailie.ts sealBrowserVault): the 32-byte account key encrypted
// under a fresh non-extractable AES-GCM key, bound to the person's seal id
// and public key, in IndexedDB beside the session's record
// (state/sessionVault.ts), one per browser profile. It is opened only for the
// person the server says is signed in: a record of anyone else is wiped,
// never opened. It is never sent anywhere. This page also keeps the newest
// record it wrote in its own memory, and asks that copy first: a browser that
// refuses IndexedDB (a private window), or opens it and then refuses the
// write (its storage full), keeps the key for this page only, and a reload
// forgets it.
//
// The memory of enrolled addresses is the upgrade's defence (section 12.7):
// an address this browser saw enrol in the key scheme never sends its
// password in clear again, whatever a challenge answers. It is keyed by the
// origin and the address as the server stores it (normaliseAddress), so
// every spelling of one account is one record, and it is not wiped at
// sign-out: it says nothing secret, and forgetting it would reopen the door
// it closes. This page also keeps its own copy, which nothing clears, so a
// browser that refuses IndexedDB still never sends the password of an
// address it saw enrol for as long as the page lives.

import { toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { isMailieError, normaliseAddress, openBrowserVaultKey, sealBrowserVault, type BrowserKeyEnvelope } from '../crypto/mailie'

interface VaultRecord {
  version: 1
  /**
   * The session that holds the record: its browser login's id
   * (state/sessionVault.ts BrowserLogin.id), bound when the session begins
   * (sessionBegan), so that what a session's end does reaches only the record
   * it held, never one a sign-in since wrote, in this page or another tab.
   * 'pending:' and a random id while a ceremony that kept the key has not
   * begun its session yet: no ending reaches it. None on a record written
   * before holders, which the first session to end settles.
   */
  holder?: string
  origin: string
  sealID: string
  /** base64url of the account public key, as the server serves it. */
  publicKey: string
  envelope: BrowserKeyEnvelope
  /**
   * Set when the record outlived its session's expiry, which a page
   * established by the server's clock, on an edition that keeps the key
   * past one (Edition.accountKey, docs/key-scheme.md section 7): the one
   * case where the key stays with no session to go with it. A session that
   * begins with the record clears it.
   */
  outlivedExpiry?: true
}

const database = 'mailie-browser-account'
const vaultStore = 'vault'
const enrolledStore = 'enrolled'
const slot = 'current'

/** The record kept for this page when IndexedDB is refused; also the newest one this page wrote. */
let held: VaultRecord | null = null

/** Whether an ending of the session with this login id reaches the record: the session's own, or one written before holders. */
const heldBy = (record: VaultRecord, loginID: string): boolean => record.holder === undefined || record.holder === loginID

function currentOrigin(): string {
  return typeof location === 'undefined' ? '' : location.origin
}

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    if (typeof indexedDB === 'undefined') { reject(new Error('browser storage unavailable')); return }
    const request = indexedDB.open(database, 1)
    request.onupgradeneeded = () => {
      request.result.createObjectStore(vaultStore)
      request.result.createObjectStore(enrolledStore)
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('browser storage blocked'))
  })
}

async function transaction<T>(name: string, mode: IDBTransactionMode, operation: (store: IDBObjectStore, done: (result: T) => void) => void): Promise<T> {
  const db = await openDB()
  return new Promise((resolve, reject) => {
    const tx = db.transaction(name, mode)
    let result: T
    tx.oncomplete = () => { db.close(); resolve(result) }
    tx.onabort = tx.onerror = () => { db.close(); reject(tx.error ?? new Error('browser storage failed')) }
    try { operation(tx.objectStore(name), value => { result = value }) }
    catch (error) { tx.abort(); reject(error) }
  })
}

/**
 * keepAccountKey keeps the account key a ceremony just opened or made, for
 * the person it belongs to, replacing whatever this browser kept, and answers
 * the record's pending holder (VaultRecord.holder) until the ceremony's
 * session begins with it: a ceremony whose session does not begin settles it
 * with that. The caller still zeroes its own copy.
 */
export async function keepAccountKey(accountKey: Uint8Array, publicKey: Uint8Array, sealID: string): Promise<string> {
  const envelope = await sealBrowserVault(accountKey, publicKey, sealID)
  const record: VaultRecord = {
    version: 1, holder: `pending:${crypto.randomUUID()}`, origin: currentOrigin(), sealID,
    publicKey: toBase64URL(new Uint8Array(publicKey) as Bytes), envelope,
  }
  held = record
  try {
    await transaction<void>(vaultStore, 'readwrite', (store, done) => { store.put(record, slot); done(undefined) })
  } catch { /* Kept in this page's memory: a reload forgets it. */ }
  return record.holder!
}

async function storedRecord(): Promise<VaultRecord | null> {
  try {
    const stored = await transaction<VaultRecord | undefined>(vaultStore, 'readonly', (store, done) => {
      const request = store.get(slot)
      request.onsuccess = () => done(request.result as VaultRecord | undefined)
    })
    return stored ?? null
  } catch {
    return held
  }
}

function belongsTo(record: VaultRecord, sealID: string, publicKey: string): boolean {
  return record.version === 1 && record.origin === currentOrigin() && record.sealID === sealID && record.publicKey === publicKey
}

/**
 * accountKeyOf opens the vault for the person the server names (their seal
 * id and public key, as GET /v1/auth/me answers them) and returns the raw
 * account key, which the caller zeroes, or null when this browser holds none
 * of theirs. The record this page wrote comes first, when it is theirs: the
 * slot may hold an older one, if the write was refused. A record of anyone
 * else, and one that does not open, is wiped.
 */
export async function accountKeyOf(sealID: string, publicKey: string): Promise<Bytes | null> {
  const record = held !== null && belongsTo(held, sealID, publicKey) ? held : await storedRecord()
  if (!record) return null
  if (!belongsTo(record, sealID, publicKey)) {
    await wipeAccountKey()
    return null
  }
  try {
    return await openBrowserVaultKey(record.envelope, sealID)
  } catch (error) {
    if (isMailieError(error)) await wipeAccountKey()
    return null
  }
}

/** holdsAccountKey says whether this browser holds the account key of the person named, opening it to be sure. */
export async function holdsAccountKey(sealID: string, publicKey: string): Promise<boolean> {
  const key = await accountKeyOf(sealID, publicKey)
  key?.fill(0)
  return key !== null
}

/** wipeAccountKey forgets the account key this browser kept, in every tab: sign-out, and a session that is no longer valid (section 7). */
export async function wipeAccountKey(): Promise<void> {
  held = null
  try {
    await transaction<void>(vaultStore, 'readwrite', (store, done) => { store.delete(slot); done(undefined) })
  } catch { /* Nothing stored, nothing to wipe. */ }
}

/**
 * sessionBegan binds the record this browser keeps to the session that just
 * began in this page (VaultRecord.holder), and clears its mark of an expiry
 * outlived: the session holds the record now, and the record goes with it
 * when it ends. This page's own copy is bound first, so a browser that
 * refuses the write still settles it with the session. A session restored
 * from this browser's record (restored) takes only a record that is its
 * own, from before holders, or kept past an expiry: never one a session
 * begun since holds, nor one a ceremony is about to begin its session with.
 */
export async function sessionBegan(loginID: string, options: { restored?: boolean } = {}): Promise<void> {
  const takes = (record: VaultRecord) => !options.restored || heldBy(record, loginID) || record.outlivedExpiry === true
  if (held && takes(held)) { const { outlivedExpiry: _, ...rest } = held; held = { ...rest, holder: loginID } }
  try {
    await transaction<void>(vaultStore, 'readwrite', (store, done) => {
      const request = store.get(slot)
      request.onsuccess = () => {
        const record = request.result as VaultRecord | undefined
        if (record && takes(record)) { const { outlivedExpiry: _, ...rest } = record; store.put({ ...rest, holder: loginID }, slot) }
        done(undefined)
      }
    })
  } catch { /* Storage refused: this page's own copy is the session's. */ }
}

/**
 * settleRecord settles the record the session with this login id held, and
 * no other: marked as outliving its expiry (VaultRecord.outlivedExpiry) when
 * keep is set, wiped otherwise, and left alone when it is another session's
 * since, or a ceremony's that has not begun its session yet. This page's own
 * copy goes the same way. A page that ends a session settles the key before
 * it clears the session's own record, so a page that stops in between leaves
 * that record to be checked again.
 */
export async function settleRecord(loginID: string, keep: boolean): Promise<void> {
  if (held && heldBy(held, loginID)) held = keep ? { ...held, outlivedExpiry: true } : null
  try {
    await transaction<void>(vaultStore, 'readwrite', (store, done) => {
      const request = store.get(slot)
      request.onsuccess = () => {
        const record = request.result as VaultRecord | undefined
        if (record && heldBy(record, loginID)) {
          if (keep) store.put({ ...record, outlivedExpiry: true }, slot)
          else store.delete(slot)
        }
        done(undefined)
      }
    })
  } catch { /* Not marked: with no session record, the next page wipes the key. */ }
}

/** outlivedExpiry says whether the account key this browser kept outlived its session's expiry, and nothing since began a session with it. */
export async function outlivedExpiry(): Promise<boolean> {
  return (await storedRecord())?.outlivedExpiry === true
}

/** forgetHeldAccountKey drops this page's own copy only, for a session another tab ended (and wiped already). */
export function forgetHeldAccountKey(): void {
  held = null
}

const enrolledKey = (email: string) => `${currentOrigin()}|${normaliseAddress(email)}`

/**
 * The addresses this page saw enrol, as enrolledKey spells them: what it
 * remembers even when IndexedDB is refused. Never cleared, not at sign-out
 * either; a reload starts it again from what IndexedDB kept.
 */
const enrolledHere = new Set<string>()

/** rememberEnrolled records that an address enrolled, or proved a zero-knowledge secret, in this browser (section 12.7). */
export async function rememberEnrolled(email: string): Promise<void> {
  let key: string
  try { key = enrolledKey(email) } catch { return }
  enrolledHere.add(key)
  try {
    await transaction<void>(enrolledStore, 'readwrite', (store, done) => { store.put(Date.now(), key); done(undefined) })
  } catch { /* A browser that refuses storage remembers in this page only (enrolledHere) until a reload. */ }
}

/** rememberedEnrolled says whether this browser saw the address enrol, in any of the spellings the server takes for it. */
export async function rememberedEnrolled(email: string): Promise<boolean> {
  let key: string
  try { key = enrolledKey(email) } catch { return false }
  if (enrolledHere.has(key)) return true
  try {
    return await transaction<boolean>(enrolledStore, 'readonly', (store, done) => {
      const request = store.get(key)
      request.onsuccess = () => done(request.result !== undefined)
    })
  } catch {
    return false
  }
}
