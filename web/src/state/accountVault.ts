// Where this browser keeps the person's account key between page loads
// (docs/key-scheme.md section 7).
//
// The vault is the kit's key at rest under Mailie's profile
// (crypto/mailie.ts sealBrowserVault): the 32-byte account key encrypted
// under a fresh non-extractable AES-GCM key, bound to the person's seal id
// and public key, in IndexedDB beside the session's record
// (state/sessionVault.ts), one per browser profile. It is opened only for the
// person the server says is signed in: a record of anyone else is wiped,
// never opened. It is never sent anywhere. What opens the person's grants
// takes it as the kit's non-extractable private key (accountPrivateKeyOf).
// Three paths read the raw 32 bytes back out (accountKeyOf), each zeroing
// them once done: a session restored on a page load, to find whether this
// browser holds the key at all (holdsAccountKey, state/session.ts); a
// password change, which checks the key kept here is the one the server's
// wrap holds (state/account.ts changePassword); and a new recovery code,
// which wraps it again (replaceRecoveryCode). This page also keeps the newest
// record it wrote in its own memory, and asks that copy first: a browser that
// refuses IndexedDB (a private window), or opens it and then refuses the
// write (its storage full), keeps the key for this page only, and a reload
// forgets it.
//
// This browser no longer remembers which addresses it saw enrol. That
// memory was the upgrade's defence (section 12.7): an address it held never
// sent its password in clear again. The upgrade left in the release after
// the one that brought the key scheme, and with it every request that could
// carry a password, so the memory defended nothing, and it is gone with its
// store: version 2 of the database deletes the store of version 1, and the
// addresses in it, the first time a page of this release opens it. A page of
// the release before, still open in another tab, then finds the database
// newer than the one it asks for, and keeps the key in its own memory only,
// as in a browser that refuses IndexedDB, until it is reloaded.

import { toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { isMailieError, openBrowserVault, openBrowserVaultKey, sealBrowserVault, type BrowserKeyEnvelope, type PrivateKey } from '../crypto/mailie'

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
/** Version 2: the vault alone; version 1 also kept the enrolled addresses (formerEnrolledStore). */
const version = 2
const vaultStore = 'vault'
/** Version 1's store of the addresses this browser saw enrol, for the upgrade: deleted at the upgrade to version 2, never written. */
const formerEnrolledStore = 'enrolled'
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
    const request = indexedDB.open(database, version)
    request.onupgradeneeded = () => {
      // A new browser gets the vault; one of version 1 keeps its vault, and its record, and loses the enrolled addresses.
      const db = request.result
      if (!db.objectStoreNames.contains(vaultStore)) db.createObjectStore(vaultStore)
      if (db.objectStoreNames.contains(formerEnrolledStore)) db.deleteObjectStore(formerEnrolledStore)
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
 * openRecord opens the vault for the person the server names (their seal id
 * and public key, as GET /v1/auth/me answers them) with open, or answers null
 * when this browser holds none of theirs. The record this page wrote comes
 * first, when it is theirs: the slot may hold an older one, if the write was
 * refused. A record of anyone else, and one that does not open, is wiped.
 */
async function openRecord<T>(sealID: string, publicKey: string, open: (envelope: BrowserKeyEnvelope) => Promise<T>): Promise<T | null> {
  const record = held !== null && belongsTo(held, sealID, publicKey) ? held : await storedRecord()
  if (!record) return null
  if (!belongsTo(record, sealID, publicKey)) {
    await wipeAccountKey()
    return null
  }
  try {
    return await open(record.envelope)
  } catch (error) {
    if (isMailieError(error)) await wipeAccountKey()
    return null
  }
}

/**
 * accountKeyOf returns the raw account key of the person named, which the
 * caller zeroes, or null (openRecord): how the console reads the raw key back
 * out of the vault, for three callers only (the header above): whether this
 * browser holds it (holdsAccountKey), a password change's check of it, and a
 * new recovery code, which wraps it again (docs/key-scheme.md section 12.5).
 */
export async function accountKeyOf(sealID: string, publicKey: string): Promise<Bytes | null> {
  return openRecord(sealID, publicKey, envelope => openBrowserVaultKey(envelope, sealID))
}

/**
 * accountPrivateKeyOf returns the account key of the person named as the
 * kit's non-extractable private key (openBrowserVault, which also recomputes
 * the public half), or null (openRecord): what opens their grants
 * (docs/key-scheme.md section 9.2), never the raw bytes.
 */
export async function accountPrivateKeyOf(sealID: string, publicKey: string): Promise<PrivateKey | null> {
  return openRecord(sealID, publicKey, envelope => openBrowserVault(envelope, sealID))
}

/**
 * holdsAccountKey says whether this browser holds the account key of the
 * person named, opening it to be sure: the raw key is read out and zeroed at
 * once.
 */
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
