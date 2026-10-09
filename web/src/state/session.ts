// The signed-in person, and the one bearer token that proves it.
//
// The token is module state, not reactive state: nothing renders it, and a
// reactive copy would be one devtools click from the screen. Everything that
// needs it goes through authorized(), which is also where a 401 in the middle
// of use ends the session — once, for the token that was refused, and never
// for a request that raced a newer sign-in.
//
// The ceremonies that start a session (signing in and up, a recovery, a
// reset link, the upgrade) are state/account.ts's: each ends here, in
// beginSession. Where a session ends without the person asking, the account
// key this browser kept goes with it (docs/key-scheme.md section 7).

import { reactive } from 'vue'
import * as auth from '../api/auth'
import { ApiError, checked } from '../api/http'
import { enrolled, isSessionReply, type SessionReply, type User } from '../api/types'
import { edition } from '../edition'
import { uuid } from '../ui/uuid'
import { serverNow } from './connection'
import { forgetHeldAccountKey, holdsAccountKey, wipeAccountKey } from './accountVault'
import { clearLocalSession, loadLocalSession, localSessionWasCleared, observeLocalSession, saveLocalSession, type BrowserLogin } from './sessionVault'

export type Phase = 'restoring' | 'signed-out' | 'ready'

interface SessionState {
  phase: Phase
  user: User | null
  /** Unix seconds; the server's absolute expiry, never extended by use. */
  expiresAt: number
  /** Why the sign-in form is showing when the person did not ask for it. */
  notice: '' | 'expired'
  /** A remembered session could not be checked (no network, server down): offer a retry, keep the record. */
  restoreFailed: boolean
  /** The browser refused to store the session, so a reload will ask for the password again. */
  notRemembered: boolean
  /**
   * The session's step-up time, the server's unix seconds (0: none): what
   * the step-up guards needs it within the last ten minutes
   * (docs/key-scheme.md section 11), and the console asks for the password
   * again before such a call rather than after a refusal.
   */
  authenticatedAt: number
  /** Whether this browser holds the person's account key (state/accountVault.ts), which replacing the recovery code wraps. */
  keyed: boolean
}

const fresh = (phase: Phase): SessionState => ({
  phase, user: null, expiresAt: 0, notice: '', restoreFailed: false, notRemembered: false, authenticatedAt: 0, keyed: false,
})

export const session = reactive<SessionState>(fresh('restoring'))
let token = ''
let loginID = ''
let generation = 0
let expiryTimer: ReturnType<typeof setTimeout> | undefined

/** The identity other stores reset on: empty while nobody is signed in. A new token for the same person is not a new identity. */
export function identity(): string {
  return session.phase === 'ready' ? session.user?.id ?? '' : ''
}

// Another tab signed out, or replaced this login with a newer one. That tab
// wiped the account key the two shared, or kept the newer login's in its
// place; this page lets go of its own copy.
observeLocalSession(change => {
  if (change.id === loginID && session.phase === 'ready') { drop(''); forgetHeldAccountKey() }
})

function drop(notice: SessionState['notice']): void {
  generation++
  token = ''
  loginID = ''
  clearTimeout(expiryTimer)
  Object.assign(session, fresh('signed-out'), { notice })
}

/**
 * noValidSession wipes the account key this browser kept, where it lasts no
 * longer than a session: a self-hosted server's, where every sign-in opens
 * the password wrap anyway (docs/key-scheme.md section 7). An edition whose
 * key outlives a session that merely ends keeps it (Edition.accountKey).
 */
function noValidSession(): Promise<void> {
  if (edition().accountKey?.outlivesSession) return Promise.resolve()
  return wipeAccountKey()
}

function scheduleExpiry(): void {
  clearTimeout(expiryTimer)
  // The server stops accepting the token at expires_at whatever the page
  // does; ending it here too means an idle console does not sit there looking
  // signed in until its next request fails.
  const ms = session.expiresAt * 1000 - Date.now()
  const expiring = token
  if (ms > 0 && ms < 2 ** 31 - 1) expiryTimer = setTimeout(() => expire(expiring), ms)
}

/** Ends the session because the server refused its token. Only the token that was refused ends anything. */
function expire(refused: string): void {
  if (!token || refused !== token) return
  const id = loginID
  drop('expired')
  if (id) void clearLocalSession(id).catch(() => { /* Nothing stored, nothing to clear. */ })
  void noValidSession()
}

/**
 * adopt makes a login the current one before storing it. The order matters:
 * storing a new login announces that the previous one is cleared, and this tab
 * must already be holding the new id when it hears that, or it would sign
 * itself out of the session it just opened. A sign-out that lands while the
 * write is pending wins: its tombstone stops the write, or its delete follows it.
 */
async function adopt(login: BrowserLogin, user: User, expiresAt: number, authenticatedAt: number, keyed: boolean): Promise<void> {
  const attempt = ++generation
  token = login.token
  loginID = login.id
  // One merged object, so each field is written once: assigning fresh() first
  // would pass user through null, and a new token for the same person (a
  // password change) would read as a sign-out to every store keyed on identity().
  Object.assign(session, { ...fresh('ready'), user, expiresAt, authenticatedAt, keyed })
  scheduleExpiry()
  try { await saveLocalSession(login) }
  catch { if (attempt === generation) session.notRemembered = true }
}

function loginFrom(reply: SessionReply): BrowserLogin {
  return { id: uuid(), token: reply.token, expiresAt: reply.expires_at, userID: reply.user.id }
}

/**
 * restore reopens the session this browser remembered, after the server has
 * confirmed it is still good. A refused token forgets the record; a network
 * failure keeps it, so a flaky connection does not cost the person their
 * sign-in.
 */
export async function restore(): Promise<void> {
  const attempt = ++generation
  session.phase = 'restoring'
  session.restoreFailed = false
  let login: BrowserLogin | null = null
  try { login = await loadLocalSession() } catch { /* No storage, or none this browser can read: sign in instead. */ }
  if (attempt !== generation) return
  if (!login) { drop(''); await noValidSession(); return }
  try {
    const current = await auth.me(login.token)
    if (attempt !== generation) return
    if (current.user.id !== login.userID) throw new ApiError('unauthorized')
    // The account key kept for this person, if any: a record of anyone else is wiped here.
    const keyed = enrolled(current.user) && await holdsAccountKey(current.user.seal_id!, current.user.public_key!)
    if (attempt !== generation) return
    // Adopt without writing: the record is already the one just read.
    token = login.token
    loginID = login.id
    Object.assign(session, {
      ...fresh('ready'), user: current.user, expiresAt: current.session.expires_at,
      authenticatedAt: current.session.authenticated_at ?? 0, keyed,
    })
    scheduleExpiry()
  } catch (error) {
    if (attempt !== generation) return
    if (error instanceof ApiError && error.code === 'unauthorized') {
      const id = login.id
      drop('')
      await clearLocalSession(id).catch(() => {})
      await noValidSession()
      return
    }
    session.restoreFailed = true
  }
}

/**
 * showSignIn skips restoring: an invitation or a reset link explicitly starts
 * a new sign-in, even in a browser that remembers another one. Signing up
 * replaces the remembered login, which also signs its other tabs out.
 */
export function showSignIn(): void {
  drop('')
}

/** Gives up on a remembered session that cannot be checked right now, and shows the sign-in form. */
export async function forgetRemembered(): Promise<void> {
  const login = await loadLocalSession().catch(() => null)
  drop('')
  if (login) await clearLocalSession(login.id).catch(() => {})
  await noValidSession()
}

/**
 * beginSession makes a session the server just issued the current one: every
 * sign-in ends here (state/account.ts, once its ceremony is done; and
 * adoptSession). keyed says whether the ceremony kept the person's account
 * key in this browser.
 */
export async function beginSession(reply: SessionReply, keyed = false): Promise<void> {
  await adopt(loginFrom(reply), reply.user, reply.expires_at, reply.authenticated_at ?? 0, keyed)
}

/**
 * adoptSession is how an edition's own sign-in (Edition.signIn) signs the
 * person in: it hands over what its route answered, a session as POST
 * /v1/auth/signup answers one, and the session begins exactly as a password
 * sign-in's does, stored for this browser and its other tabs, with the person
 * it names. A reply of any other shape is refused (ApiError
 * 'invalid_response') and changes nothing.
 */
export async function adoptSession(reply: unknown): Promise<void> {
  await beginSession(checked(reply, isSessionReply))
}

/**
 * signOut leaves this browser at once. The server is told, but a server that
 * cannot be reached must not keep anyone signed in here.
 *
 * Signing out everywhere is different: its whole point is the other browsers,
 * so the server has to confirm it before this one lets go, or the person would
 * walk away believing a lost laptop was signed out.
 *
 * Either way the account key this browser kept is wiped, in every edition
 * (docs/key-scheme.md section 7).
 */
export async function signOut(options: { everywhere?: boolean } = {}): Promise<void> {
  const current = token
  const id = loginID
  if (options.everywhere) {
    if (!current) return
    try { await auth.logout(current, true) }
    catch (error) { if (!(error instanceof ApiError && error.code === 'unauthorized')) throw error }
    if (current !== token) return
  }
  drop('')
  await Promise.allSettled([
    id ? clearLocalSession(id) : Promise.resolve(),
    current && !options.everywhere ? auth.logout(current) : Promise.resolve(),
    wipeAccountKey(),
  ])
  // Only now: the server has been told, so an edition that navigates away
  // does not cut the request short.
  edition().signedOut?.()
}

/**
 * authorized runs a call with the current token. A 401 means the server no
 * longer accepts it — revoked, expired, the user disabled — so the session
 * ends and the person is sent to sign in with a notice, not left facing a
 * console that fails at every click.
 */
export async function authorized<T>(call: (token: string) => Promise<T>): Promise<T> {
  const current = token
  if (!current) throw new ApiError('unauthorized')
  try { return await call(current) }
  catch (error) {
    if (error instanceof ApiError && error.code === 'unauthorized') expire(current)
    throw error
  }
}

/**
 * replaceSession takes the only session left after a password change ended
 * every other one, this one included. It replaces the stored one, which also
 * tells every other tab that its login is gone. Nothing is taken for anyone
 * but the person signed in.
 */
export async function replaceSession(reply: SessionReply, keyed: boolean): Promise<void> {
  const userID = session.user?.id
  if (!userID || reply.user.id !== userID || identity() !== userID) return
  await adopt(loginFrom(reply), reply.user, reply.expires_at, reply.authenticated_at ?? 0, keyed)
}

/** steppedUp records the session's new step-up time: this session's only, for the person who proved it. */
export function steppedUp(userID: string, authenticatedAt: number): void {
  if (identity() === userID) session.authenticatedAt = authenticatedAt
}

/** markKeyed records that this browser now holds the account key of the person signed in. */
export function markKeyed(userID: string): void {
  if (identity() === userID) session.keyed = true
}

/** The server's ten minutes (docs/key-scheme.md section 11), less a margin for the clocks and the request. */
const STEP_UP_FRESH_S = 10 * 60 - 30

/**
 * freshStepUp says whether the session proved its person recently enough for
 * what the step-up guards. The step-up time is the server's, so it is judged
 * by the server's clock (state/connection.ts serverNow), never this
 * browser's: a browser whose clock runs ahead would otherwise find a step-up
 * it just made already old, and ask for the password again and again.
 */
export function freshStepUp(now = serverNow()): boolean {
  const at = session.authenticatedAt
  return at > 0 && now / 1000 - at < STEP_UP_FRESH_S
}

export async function updateProfile(name: string): Promise<void> {
  const userID = session.user?.id
  const user = await authorized(t => auth.updateProfile(t, name))
  if (userID && user.id === userID && identity() === userID) session.user = user
}

/** A page restored from the back/forward cache may hold a login another tab cleared meanwhile. */
export async function recheckRemembered(): Promise<void> {
  const id = loginID
  if (!id || session.phase !== 'ready') return
  try { if (await localSessionWasCleared(id) && id === loginID) drop('') } catch { /* Storage refused: the server still decides. */ }
}
