// Where a signed-in browser keeps its session between page loads.
//
// While the page runs the token lives in memory. To survive a reload it is
// written to IndexedDB encrypted under a non-extractable AES-GCM-256 key that
// is itself stored in IndexedDB: this origin's script can use that key but
// never read it out, so a copy of the database (a backup, a synced profile, a
// disk image) carries ciphertext and a handle only this browser profile can
// use. It does not stop script running inside the page — nothing does — which
// is why the strict CSP matters as much as this file.
//
// Never cookies, localStorage, sessionStorage or a URL. The additional data
// binds the ciphertext to its record id, this origin, the user and the expiry,
// so moving a ciphertext between records or editing the expiry fails to
// decrypt instead of restoring someone.

export interface BrowserLogin {
  /** A random id per sign-in, safe to broadcast; never the token. */
  id: string
  token: string
  /** Unix seconds, the server's absolute expiry. */
  expiresAt: number
  userID: string
}

interface StoredLogin {
  version: 1
  id: string
  origin: string
  userID: string
  expiresAt: number
  wrappingKey: CryptoKey
  nonce: Uint8Array<ArrayBuffer>
  ciphertext: ArrayBuffer
}

export interface SessionChange { id: string; kind: 'cleared' }

const database = 'mailie-browser-session'
const storeName = 'session'
const slot = 'current'
/** Sessions end 14 days after sign-in, so a tombstone older than this guards nothing any more. */
const tombstoneLifetimeMS = 15 * 86_400_000
const listeners = new Set<(change: SessionChange) => void>()
let channel: BroadcastChannel | undefined

function notify(change: SessionChange) {
  for (const listener of listeners) listener(change)
  getChannel()?.postMessage(change)
}

function getChannel() {
  if (!channel && typeof window !== 'undefined' && typeof BroadcastChannel !== 'undefined') {
    channel = new BroadcastChannel('mailie-browser-session')
    channel.onmessage = event => {
      if (event.data?.kind !== 'cleared' || typeof event.data.id !== 'string') return
      for (const listener of listeners) listener({ kind: 'cleared', id: event.data.id })
    }
  }
  return channel
}

/** Tells this tab when a login is cleared here or in another tab of the same origin. */
export function observeLocalSession(listener: (change: SessionChange) => void): () => void {
  getChannel()
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

function currentOrigin(): string {
  return typeof location === 'undefined' ? '' : location.origin
}

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    if (typeof indexedDB === 'undefined') { reject(new Error('browser storage unavailable')); return }
    const request = indexedDB.open(database, 1)
    request.onupgradeneeded = () => request.result.createObjectStore(storeName)
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('browser storage blocked'))
  })
}

async function transaction<T>(mode: IDBTransactionMode, operation: (store: IDBObjectStore, done: (result: T) => void) => void): Promise<T> {
  const db = await openDB()
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, mode)
    let result: T
    tx.oncomplete = () => { db.close(); resolve(result) }
    tx.onabort = tx.onerror = () => { db.close(); reject(tx.error ?? new Error('browser storage failed')) }
    try { operation(tx.objectStore(storeName), value => { result = value }) }
    catch (error) { tx.abort(); reject(error) }
  })
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function validBrowserLogin(value: unknown): value is BrowserLogin {
  if (!value || typeof value !== 'object') return false
  const login = value as BrowserLogin
  return typeof login.id === 'string' && uuid.test(login.id)
    && typeof login.token === 'string' && login.token.length > 0 && login.token.length <= 4096 && !/\s/.test(login.token)
    && Number.isSafeInteger(login.expiresAt) && login.expiresAt > 0
    && typeof login.userID === 'string' && login.userID.length > 0 && login.userID.length <= 64
}

function aad(login: Pick<BrowserLogin, 'id' | 'userID' | 'expiresAt'>, origin: string): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(JSON.stringify(['mailie/browser-session', 1, login.id, origin, login.userID, login.expiresAt]))
}

const nowSeconds = () => Math.floor(Date.now() / 1000)

/** Replaces whatever login this browser remembered. A login cleared meanwhile stays cleared. */
export async function saveLocalSession(login: BrowserLogin): Promise<void> {
  if (!validBrowserLogin(login) || login.expiresAt <= nowSeconds()) throw new Error('invalid browser session')
  const origin = currentOrigin()
  const wrappingKey = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt'])
  const nonce = crypto.getRandomValues(new Uint8Array(12))
  const ciphertext = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad(login, origin) }, wrappingKey, new TextEncoder().encode(login.token))
  const stored: StoredLogin = { version: 1, id: login.id, origin, userID: login.userID, expiresAt: login.expiresAt, wrappingKey, nonce, ciphertext }
  let replaced: string | undefined
  const saved = await transaction<boolean>('readwrite', (store, done) => {
    // A delayed write must not restore a login that sign-out already cleared.
    const tombstone = store.get('revoked:' + login.id)
    tombstone.onsuccess = () => {
      if (tombstone.result) { done(false); return }
      const before = store.get(slot)
      before.onsuccess = () => {
        const old = before.result as StoredLogin | undefined
        if (old && old.id !== login.id) {
          replaced = old.id
          store.put(Date.now(), 'revoked:' + old.id)
        }
        store.put(stored, slot)
        done(true)
      }
    }
  })
  if (!saved) throw new Error('browser session was cleared')
  if (replaced) notify({ kind: 'cleared', id: replaced })
}

export async function loadLocalSession(): Promise<BrowserLogin | null> {
  const stored = await transaction<StoredLogin | undefined>('readonly', (store, done) => {
    const request = store.get(slot)
    request.onsuccess = () => done(request.result as StoredLogin | undefined)
  })
  if (!stored) return null
  try {
    if (stored.version !== 1 || stored.origin !== currentOrigin() || !(stored.expiresAt > nowSeconds())) throw new Error('expired or foreign browser session')
    if (typeof CryptoKey === 'undefined' || !(stored.wrappingKey instanceof CryptoKey) || stored.wrappingKey.extractable) throw new Error('invalid browser session key')
    const bytes = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: stored.nonce, additionalData: aad(stored, stored.origin) }, stored.wrappingKey, stored.ciphertext)
    const token = new TextDecoder().decode(bytes)
    new Uint8Array(bytes).fill(0)
    const login: BrowserLogin = { id: stored.id, token, expiresAt: stored.expiresAt, userID: stored.userID }
    if (!validBrowserLogin(login) || await localSessionWasCleared(login.id)) throw new Error('invalid browser session')
    return login
  } catch (error) {
    // A browser lacking an algorithm cannot establish corruption. Keep the
    // ciphertext for a compatible browser version or a later retry.
    if (error instanceof DOMException && ['NotSupportedError', 'InvalidAccessError', 'UnknownError'].includes(error.name)) throw error
    await clearLocalSession(stored.id)
    return null
  }
}

/**
 * The person the login this browser remembers is for, read without opening it
 * and changing nothing: '' when it remembers none, or only one that expired,
 * was cleared or is another origin's. Another tab's sign-in is remembered
 * here as soon as it is stored.
 */
export async function rememberedPerson(): Promise<string> {
  const stored = await transaction<StoredLogin | undefined>('readonly', (store, done) => {
    const request = store.get(slot)
    request.onsuccess = () => done(request.result as StoredLogin | undefined)
  })
  if (!stored || stored.origin !== currentOrigin() || !(stored.expiresAt > nowSeconds()) || typeof stored.userID !== 'string') return ''
  return await localSessionWasCleared(stored.id) ? '' : stored.userID
}

export async function localSessionWasCleared(id: string): Promise<boolean> {
  return transaction<boolean>('readonly', (store, done) => {
    const request = store.get('revoked:' + id)
    request.onsuccess = () => done(Boolean(request.result))
  })
}

/** Conditional deletion cannot erase a newer login while an old tab is closing. */
export async function clearLocalSession(id: string): Promise<void> {
  const cutoff = Date.now() - tombstoneLifetimeMS
  await transaction<void>('readwrite', (store, done) => {
    store.put(Date.now(), 'revoked:' + id)
    const request = store.openCursor()
    request.onsuccess = () => {
      const cursor = request.result
      if (!cursor) { done(undefined); return }
      const key = String(cursor.key)
      if ((cursor.value as StoredLogin | undefined)?.id === id) cursor.delete()
      else if (key.startsWith('revoked:') && typeof cursor.value === 'number' && cursor.value < cutoff) cursor.delete()
      cursor.continue()
    }
  })
  notify({ kind: 'cleared', id })
}
