// Shared scaffolding for the specs: a page origin, browser storage, and a
// fetch that answers like the daemon. Not a spec itself.
import { IDBFactory } from 'fake-indexeddb'
import { vi } from 'vitest'
import type { Account, AccountSync, SessionReply, User } from '../src/api/types'

export const ORIGIN = 'http://localhost:5174'
export const now = () => Math.floor(Date.now() / 1000)

export const ana: User = { id: 'usr_00000000000000a1', email: 'ana@example.test', name: 'Ana Souza', role: 'owner', created_at: 1_790_000_000 }

export function reply(token = 'tok_first_0000000000000000000000000000000000', user: User = ana): SessionReply {
  return { token, expires_at: now() + 14 * 86_400, user }
}

/** The sync block of an account that does not sync: what the daemon answers before consent. */
export function syncOff(fields: Partial<AccountSync> = {}): AccountSync {
  return { enabled: false, running: false, state: 'off', folders_synced: 0, folders_total: 0, messages: 0, initial_progress: 0, ...fields }
}

/** The sync block of an account in its first sync, as contract/sync_status.json has it. */
export function syncing(fields: Partial<AccountSync> = {}): AccountSync {
  return { enabled: true, running: true, state: 'initial', tier: 'condstore', folders_synced: 3, folders_total: 7, messages: 1284, initial_progress: 42, ...fields }
}

export function account(fields: Partial<Account> = {}): Account {
  return { id: 'acc_0000000000000001', email: 'suporte@example.test', provider: 'gmail', auth_kind: 'oauth2', state: 'pending_auth', save_sent_copy: false, created_at: 1_790_000_000, sync: syncOff(), ...fields }
}

export function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

export function failure(code: string, status: number, headers: Record<string, string> = {}): Response {
  return json({ code, message: `upstream said: password=hunter2 for ${code}` }, status, headers)
}

export function memoryStorage(): Storage {
  const items = new Map<string, string>()
  return {
    get length() { return items.size },
    clear: () => items.clear(),
    getItem: key => items.get(key) ?? null,
    key: index => [...items.keys()][index] ?? null,
    removeItem: key => { items.delete(key) },
    setItem: (key, value) => { items.set(key, String(value)) },
  }
}

/** A page at the dev origin with empty storage. */
export function stubPage(path = '/'): { history: { state: unknown; replaceState: ReturnType<typeof vi.fn> } } {
  const history = { state: null as unknown, replaceState: vi.fn() }
  vi.stubGlobal('location', new URL(ORIGIN + path))
  vi.stubGlobal('indexedDB', new IDBFactory())
  vi.stubGlobal('sessionStorage', memoryStorage())
  vi.stubGlobal('localStorage', memoryStorage())
  vi.stubGlobal('history', history)
  return { history }
}

/**
 * One request as a route sees it: a JSON body parsed, or a multipart body as
 * the FormData the page built (form), and the headers it set.
 */
export type Route = (request: { path: string; method: string; body: unknown; form?: FormData; headers: Record<string, string>; token: string; url: string }) => Response | Promise<Response>

/** Replaces fetch with a router over path and method; returns the spy so calls can be inspected. */
export function serve(route: Route) {
  return vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = new URL(String(input))
    const headers = (init?.headers ?? {}) as Record<string, string>
    const token = (headers.Authorization ?? '').replace(/^Bearer /, '')
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
    const form = init?.body instanceof FormData ? init.body : undefined
    return route({ path: url.pathname, method: init?.method ?? 'GET', body, form, headers, token, url: url.toString() })
  })
}

/** A new page load: fresh modules, configured with the open edition as src/main.ts does. */
export async function freshModules(): Promise<void> {
  vi.resetModules()
  const [{ configureEdition }, { openEdition }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
  configureEdition(openEdition)
}

/** Lets pending promise chains (and fake-indexeddb, which runs on setImmediate) settle. */
export async function settle(rounds = 5): Promise<void> {
  for (let i = 0; i < rounds; i++) await new Promise(resolve => setImmediate(resolve))
}
