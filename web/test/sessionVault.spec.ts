import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { clearLocalSession, loadLocalSession, localSessionWasCleared, observeLocalSession, saveLocalSession, type BrowserLogin } from '../src/state/sessionVault'
import { now, stubPage } from './support'

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

function login(fields: Partial<BrowserLogin> = {}): BrowserLogin {
  return { id: crypto.randomUUID(), token: 'tok_vault_000000000000000000000000000000000', expiresAt: now() + 3600, userID: 'usr_00000000000000a1', ...fields }
}

function raw(): Promise<Record<string, unknown> | undefined> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open('mailie-browser-session', 1)
    request.onerror = () => reject(request.error)
    request.onsuccess = () => {
      const db = request.result
      const tx = db.transaction('session', 'readonly')
      const record = tx.objectStore('session').get('current')
      record.onsuccess = () => resolve(record.result as Record<string, unknown> | undefined)
      tx.oncomplete = () => db.close()
    }
  })
}

function rewrite(change: (record: Record<string, unknown>) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open('mailie-browser-session', 1)
    request.onerror = () => reject(request.error)
    request.onsuccess = () => {
      const db = request.result
      const tx = db.transaction('session', 'readwrite')
      const store = tx.objectStore('session')
      const read = store.get('current')
      read.onsuccess = () => { const record = read.result as Record<string, unknown>; change(record); store.put(record, 'current') }
      tx.oncomplete = () => { db.close(); resolve() }
    }
  })
}

describe('the browser session vault', () => {
  it('stores the token only as ciphertext, under a key the page cannot export', async () => {
    const saved = login()
    await saveLocalSession(saved)
    const record = await raw()
    expect(record).not.toHaveProperty('token')
    expect(JSON.stringify(record)).not.toContain(saved.token)
    expect(new TextDecoder().decode(record!.ciphertext as ArrayBuffer)).not.toContain(saved.token)
    const key = record!.wrappingKey as CryptoKey
    expect(key.extractable).toBe(false)
    await expect(crypto.subtle.exportKey('raw', key)).rejects.toThrow()
    expect(await loadLocalSession()).toEqual(saved)
  })

  it('refuses a record whose bound fields were edited, and forgets it', async () => {
    await saveLocalSession(login())
    await rewrite(record => { record.userID = 'usr_someone_else00' })
    expect(await loadLocalSession()).toBeNull()
    expect(await raw()).toBeUndefined()
  })

  it('refuses a record written by another origin', async () => {
    await saveLocalSession(login())
    await rewrite(record => { record.origin = 'https://evil.example' })
    expect(await loadLocalSession()).toBeNull()
  })

  it('forgets a record once its expiry has passed', async () => {
    await saveLocalSession(login({ expiresAt: now() + 60 }))
    vi.spyOn(Date, 'now').mockReturnValue((now() + 120) * 1000)
    expect(await loadLocalSession()).toBeNull()
    expect(await raw()).toBeUndefined()
  })

  it('answers a record past its expiry by this browser’s clock only when asked to, for the server to judge, and never one of another origin', async () => {
    const saved = login({ expiresAt: now() + 60 })
    await saveLocalSession(saved)
    vi.spyOn(Date, 'now').mockReturnValue((now() + 120) * 1000)
    expect(await loadLocalSession({ expired: true })).toEqual(saved)
    expect(await raw()).toBeDefined()
    await rewrite(record => { record.origin = 'https://evil.example' })
    expect(await loadLocalSession({ expired: true })).toBeNull()
    expect(await raw()).toBeUndefined()
  })

  it('never lets a late write bring back a login that was signed out', async () => {
    const saved = login()
    await clearLocalSession(saved.id)
    await expect(saveLocalSession(saved)).rejects.toThrow()
    expect(await localSessionWasCleared(saved.id)).toBe(true)
    expect(await loadLocalSession()).toBeNull()
  })

  it('tells listeners when a newer login replaces the stored one, and when one is cleared', async () => {
    const changes: string[] = []
    const stop = observeLocalSession(change => changes.push(change.id))
    const first = login()
    const second = login()
    await saveLocalSession(first)
    await saveLocalSession(second)
    await clearLocalSession(second.id)
    stop()
    expect(changes).toEqual([first.id, second.id])
  })

  it('refuses to store a login that is already expired or malformed', async () => {
    await expect(saveLocalSession(login({ expiresAt: now() - 1 }))).rejects.toThrow()
    await expect(saveLocalSession(login({ token: 'has space' }))).rejects.toThrow()
    await expect(saveLocalSession(login({ id: 'not-a-uuid' }))).rejects.toThrow()
  })
})

describe('the sign-out broadcast', () => {
  it('reaches other tabs of the origin, and never carries the token', async () => {
    const posted: unknown[] = []
    const channels: { name: string; onmessage: ((event: MessageEvent) => void) | null }[] = []
    class FakeChannel {
      onmessage: ((event: MessageEvent) => void) | null = null
      constructor(readonly name: string) { channels.push(this) }
      postMessage(message: unknown) { posted.push(message) }
      close() {}
    }
    vi.stubGlobal('window', {})
    vi.stubGlobal('BroadcastChannel', FakeChannel)
    vi.resetModules()
    const vault = await import('../src/state/sessionVault')
    const heard: string[] = []
    vault.observeLocalSession(change => heard.push(change.id))
    const saved = login()
    await vault.saveLocalSession(saved)
    await vault.clearLocalSession(saved.id)
    expect(channels.map(channel => channel.name)).toEqual(['mailie-browser-session'])
    expect(posted).toEqual([{ kind: 'cleared', id: saved.id }])
    expect(JSON.stringify(posted)).not.toContain(saved.token)
    // Another tab's sign-out arrives on the channel.
    channels[0]!.onmessage?.({ data: { kind: 'cleared', id: 'other-login' } } as MessageEvent)
    channels[0]!.onmessage?.({ data: { kind: 'something-else', id: 'x' } } as MessageEvent)
    expect(heard).toEqual([saved.id, 'other-login'])
  })
})
