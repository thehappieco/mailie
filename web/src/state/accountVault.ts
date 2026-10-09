// Where this browser keeps the person's account key between page loads, and
// which addresses it has seen enrol (docs/key-scheme.md sections 7 and 12.7).
//
// The vault is the kit's key at rest under Mailie's profile
// (crypto/mailie.ts sealBrowserVault): the 32-byte account key encrypted
// under a fresh non-extractable AES-GCM key, bound to the person's seal id
// and public key, in IndexedDB beside the session's record
// (state/sessionVault.ts), one per browser profile. It is opened only for the
// person the server says is signed in: a record of anyone else is wiped,
// never opened. It is never sent anywhere. When the browser refuses
// IndexedDB (a private window), the record is kept in this page's memory
// only, and a reload forgets it.
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
import { KeySchemeError, normaliseAddress, openBrowserVaultKey, sealBrowserVault, type BrowserKeyEnvelope } from '../crypto/mailie'

interface VaultRecord {
  version: 1
  origin: string
  sealID: string
  /** base64url of the account public key, as the server serves it. */
  publicKey: string
  envelope: BrowserKeyEnvelope
}

const database = 'mailie-browser-account'
const vaultStore = 'vault'
const enrolledStore = 'enrolled'
const slot = 'current'

/** The record kept for this page when IndexedDB is refused; also the newest one this page wrote. */
let held: VaultRecord | null = null

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
 * the person it belongs to, replacing whatever this browser kept. The caller
 * still zeroes its own copy.
 */
export async function keepAccountKey(accountKey: Uint8Array, publicKey: Uint8Array, sealID: string): Promise<void> {
  const envelope = await sealBrowserVault(accountKey, publicKey, sealID)
  const record: VaultRecord = { version: 1, origin: currentOrigin(), sealID, publicKey: toBase64URL(new Uint8Array(publicKey) as Bytes), envelope }
  held = record
  try {
    await transaction<void>(vaultStore, 'readwrite', (store, done) => { store.put(record, slot); done(undefined) })
  } catch { /* Kept in this page's memory: a reload forgets it. */ }
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
 * of theirs. A record of anyone else, and one that does not open, is wiped.
 */
export async function accountKeyOf(sealID: string, publicKey: string): Promise<Bytes | null> {
  const record = await storedRecord()
  if (!record) return null
  if (!belongsTo(record, sealID, publicKey)) {
    await wipeAccountKey()
    return null
  }
  try {
    return await openBrowserVaultKey(record.envelope, sealID)
  } catch (error) {
    if (error instanceof KeySchemeError) await wipeAccountKey()
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
