// Coming back from Google or Microsoft.
//
// The web flow leaves the console: the provider sends the browser back to
// /oauth/return with a code and a state in the query. The page never trusts
// that URL further than checking its shape; the server decides whether the
// flow exists and belongs to the signed-in person before it exchanges anything.
//
// Two things are kept in this tab's sessionStorage, never elsewhere:
// - the start, {account_id, provider, email, back}, written just before
//   leaving, so the return can name the mailbox it was about, and come back
//   to the page it left from (back: that page's path alone, never its query
//   or fragment; a host whose address names a workspace takes it again from
//   the mailbox, once the return is finished);
// - the return itself, written the moment the page loads and removed from the
//   address bar at once, so a person who has to sign in first (a new tab, an
//   expired session) can still finish, and a reload never replays the code.
//   The address becomes the start's back, while the start is fresh and back is
//   a path of this origin; else /.
// Both expire with the provider's flow, ten minutes, and are taken, not read,
// when used: one return is posted once. One that is never used is removed once
// its ten minutes are up (sweepOAuthStorage), as soon as the console is running
// in the tab; sessionStorage itself goes when the tab closes. A browser that
// refuses site storage
// still finishes a return on the page load that received it: the return is
// also held in memory until it is taken (it cannot survive a reload there,
// and the start cannot name the mailbox).

import type { ProviderID } from '../api/types'
import { providerIDs } from '../api/types'

export const RETURN_PATH = '/oauth/return'
const START_KEY = 'mailie.oauthStart'
const RETURN_KEY = 'mailie.oauthReturn'
/** The server keeps a pending flow for ten minutes; nothing here outlives it. */
export const FLOW_LIFETIME_MS = 10 * 60_000

export interface OAuthStart {
  account_id: string
  provider: ProviderID
  email: string
  started_at: number
  /** The path of the page the flow left from, where its return comes back to: a path alone (oauthBack). */
  back?: string
}
export interface OAuthReturn {
  /** What is posted to the server: the return URL without its fragment. Empty when the shape was wrong. */
  url: string
  /** When the page received it, in ms. */
  at: number
}

const statePattern = /^[A-Za-z0-9_-]{16,512}$/
const codePattern = /^[\x21-\x7e]{1,4096}$/
const errorPattern = /^[a-z_]{1,64}$/

/**
 * parseOAuthReturn checks what the provider sent back. It answers null when
 * the page is not an OAuth return at all, and a return with an empty url when
 * it is one whose shape is wrong — still a return, so the person is told,
 * but nothing is posted.
 *
 * Only the query counts. The server reads the code and state from the query
 * (a response_mode the console never asks for would put them in the
 * fragment), so the fragment is dropped rather than forwarded.
 */
export function parseOAuthReturn(href: string, now = Date.now()): OAuthReturn | null {
  let url: URL
  try { url = new URL(href) } catch { return null }
  if (url.pathname !== RETURN_PATH) return null
  const params = url.searchParams
  const state = params.getAll('state')
  const code = params.getAll('code')
  const error = params.getAll('error')
  const valid = state.length === 1 && statePattern.test(state[0]!)
    && ((code.length === 1 && error.length === 0 && codePattern.test(code[0]!))
      || (error.length === 1 && code.length === 0 && errorPattern.test(error[0]!)))
  url.hash = ''
  return { url: valid ? url.toString() : '', at: now }
}

function read<T>(key: string): T | null {
  try {
    const raw = sessionStorage.getItem(key)
    return raw ? JSON.parse(raw) as T : null
  } catch { return null }
}

function write(key: string, value: unknown): void {
  try { sessionStorage.setItem(key, JSON.stringify(value)) } catch { /* Storage refused: the in-memory copy from captureOAuthReturn still finishes a return on this page load. */ }
}

function remove(key: string): void {
  try { sessionStorage.removeItem(key) } catch { /* Nothing was stored, so nothing is left behind. */ }
}

/** The return this page load captured, for when storage refused to keep it. Cleared with the stored copy. */
let captured: OAuthReturn | null = null

function currentReturn(): OAuthReturn | null {
  return read<OAuthReturn>(RETURN_KEY) ?? captured
}

/** The longest back kept, in bytes. */
const BACK_MAX_BYTES = 1024
/** A control character, or a backslash, which a browser reads as a slash. */
const unsafeInPath = /[\x00-\x1f\x7f\\]/

/**
 * The path a flow may come back to: a path of this origin as the browser
 * spells it (location.pathname), nothing else. Not the provider's return
 * itself, never another origin ('//host', a backslash a browser reads as a
 * slash), and no query or fragment, which may carry what only the page that
 * had them should (a workspace, a code): null for anything else.
 */
function checkBack(back: unknown, origin: string): string | null {
  if (typeof back !== 'string' || !back.startsWith('/') || back.startsWith('//') || unsafeInPath.test(back)
    || new TextEncoder().encode(back).length > BACK_MAX_BYTES || back === RETURN_PATH) return null
  let url: URL
  try { url = new URL(back, origin) } catch { return null }
  // The browser's own spelling of it, and only a path: no query, no fragment, no dot segments, nothing it would encode.
  return url.origin === origin && url.pathname === back && !url.search && !url.hash ? back : null
}

/** Whether a start record is one this page wrote, for a flow still open. */
function freshStart(start: unknown, now: number): start is OAuthStart {
  if (!start || typeof start !== 'object') return false
  const record = start as Partial<OAuthStart>
  return typeof record.account_id === 'string' && typeof record.email === 'string' && providerIDs.includes(record.provider!)
    && typeof record.started_at === 'number' && now - record.started_at <= FLOW_LIFETIME_MS && record.started_at <= now
}

/**
 * Where a provider's return comes back to: the start's back, when the start
 * is fresh and its back a path of this origin (checkBack); null otherwise,
 * and the return goes to /.
 */
export function oauthBack(start: unknown, origin: string, now = Date.now()): string | null {
  return freshStart(start, now) ? checkBack(start.back, origin) : null
}

/**
 * Kept by the tab that leaves for the provider, so the tab that comes back can
 * say which mailbox it was, and come back to back: the path of the page it
 * left from (location.pathname), kept only when it is one checkBack takes.
 */
export function rememberOAuthStart(start: Omit<OAuthStart, 'started_at'>, now = Date.now()): void {
  const { back, ...rest } = start
  const kept = typeof location === 'undefined' ? null : checkBack(back, location.origin)
  write(START_KEY, { ...rest, started_at: now, ...(kept ? { back: kept } : {}) } satisfies OAuthStart)
}

/** Takes the start record, if this tab left one recently. */
export function takeOAuthStart(now = Date.now()): OAuthStart | null {
  const start = read<OAuthStart>(START_KEY)
  remove(START_KEY)
  return freshStart(start, now) ? start : null
}

/**
 * captureOAuthReturn runs once, before anything renders. When the page is an
 * OAuth return it keeps the URL in this tab and replaces the address with the
 * page the flow left from (oauthBack: the start, read, not taken), or /, so
 * the code is no longer in the address bar, the history entry, or anything a
 * later navigation could leak. Running it again on the same page load changes
 * nothing: the address is no return any more.
 */
export function captureOAuthReturn(now = Date.now()): boolean {
  if (typeof location === 'undefined') return false
  const found = parseOAuthReturn(location.href, now)
  if (!found) return false
  captured = found
  write(RETURN_KEY, found)
  const back = oauthBack(read<unknown>(START_KEY), location.origin, now) ?? '/'
  try { history.replaceState(null, '', back) } catch { /* The stored copy is what gets used either way. */ }
  return true
}

/**
 * sweepOAuthStorage removes a start or a return that has outlived the
 * provider's flow, or that is not one this page wrote, and answers how many ms
 * remain until the next one kept here expires — null when nothing is left.
 * Ignoring them would not be enough: the return holds the provider's code and
 * the start names the mailbox, and neither may stay longer than ten minutes,
 * or than the tab when the person never comes back to the console in it —
 * this sweep only runs while the console is open.
 */
export function sweepOAuthStorage(now = Date.now()): number | null {
  let next: number | null = null
  const keep = (at: unknown): boolean => {
    if (typeof at !== 'number' || !Number.isFinite(at) || at > now || now - at >= FLOW_LIFETIME_MS) return false
    const left = FLOW_LIFETIME_MS - (now - at)
    next = next === null ? left : Math.min(next, left)
    return true
  }
  for (const [key, field] of [[START_KEY, 'started_at'], [RETURN_KEY, 'at']] as const) {
    let raw: string | null
    try { raw = sessionStorage.getItem(key) } catch { continue }
    if (raw === null) continue
    let value: unknown
    try { value = JSON.parse(raw) } catch { value = null }
    if (!value || typeof value !== 'object' || !keep((value as Record<string, unknown>)[field])) remove(key)
  }
  if (captured && !keep(captured.at)) captured = null
  return next
}

/** Whether a return is waiting to be finished — after sign-in, when there was no session to finish it with. */
export function pendingOAuthReturn(now = Date.now()): boolean {
  const found = currentReturn()
  return !!found && typeof found.at === 'number' && now - found.at <= FLOW_LIFETIME_MS
}

/** Takes the waiting return: whoever takes it posts it, and nobody else can. */
export function takeOAuthReturn(): OAuthReturn | null {
  const found = currentReturn()
  captured = null
  remove(RETURN_KEY)
  if (!found || typeof found !== 'object' || typeof found.url !== 'string' || typeof found.at !== 'number') return null
  return found
}
