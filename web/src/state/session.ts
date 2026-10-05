// The signed-in person, and the one bearer token that proves it.
//
// The token is module state, not reactive state: nothing renders it, and a
// reactive copy would be one devtools click from the screen. Everything that
// needs it goes through authorized(), which is also where a 401 in the middle
// of use ends the session — once, for the token that was refused, and never
// for a request that raced a newer sign-in.

import { reactive } from 'vue'
import * as auth from '../api/auth'
import { ApiError, checked } from '../api/http'
import { isSessionReply, type SessionReply, type User } from '../api/types'
import { edition } from '../edition'
import { uuid } from '../ui/uuid'
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
}

const fresh = (phase: Phase): SessionState => ({ phase, user: null, expiresAt: 0, notice: '', restoreFailed: false, notRemembered: false })

export const session = reactive<SessionState>(fresh('restoring'))
let token = ''
let loginID = ''
let generation = 0
let expiryTimer: ReturnType<typeof setTimeout> | undefined

/** The identity other stores reset on: empty while nobody is signed in. A new token for the same person is not a new identity. */
export function identity(): string {
  return session.phase === 'ready' ? session.user?.id ?? '' : ''
}

// Another tab signed out, or replaced this login with a newer one.
observeLocalSession(change => {
  if (change.id === loginID && session.phase === 'ready') drop('')
})

function drop(notice: SessionState['notice']): void {
  generation++
  token = ''
  loginID = ''
  clearTimeout(expiryTimer)
  Object.assign(session, fresh('signed-out'), { notice })
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
}

/**
 * adopt makes a login the current one before storing it. The order matters:
 * storing a new login announces that the previous one is cleared, and this tab
 * must already be holding the new id when it hears that, or it would sign
 * itself out of the session it just opened. A sign-out that lands while the
 * write is pending wins: its tombstone stops the write, or its delete follows it.
 */
async function adopt(login: BrowserLogin, user: User, expiresAt: number): Promise<void> {
  const attempt = ++generation
  token = login.token
  loginID = login.id
  // One merged object, so each field is written once: assigning fresh() first
  // would pass user through null, and a new token for the same person (a
  // password change) would read as a sign-out to every store keyed on identity().
  Object.assign(session, { ...fresh('ready'), user, expiresAt })
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
  if (!login) { drop(''); return }
  try {
    const current = await auth.me(login.token)
    if (attempt !== generation) return
    if (current.user.id !== login.userID) throw new ApiError('unauthorized')
    // Adopt without writing: the record is already the one just read.
    token = login.token
    loginID = login.id
    Object.assign(session, { ...fresh('ready'), user: current.user, expiresAt: current.session.expires_at })
    scheduleExpiry()
  } catch (error) {
    if (attempt !== generation) return
    if (error instanceof ApiError && error.code === 'unauthorized') {
      const id = login.id
      drop('')
      await clearLocalSession(id).catch(() => {})
      return
    }
    session.restoreFailed = true
  }
}

/**
 * showSignIn skips restoring: an invitation link explicitly starts a new
 * account, even in a browser that remembers another one. Signing up replaces
 * the remembered login, which also signs its other tabs out.
 */
export function showSignIn(): void {
  drop('')
}

/** Gives up on a remembered session that cannot be checked right now, and shows the sign-in form. */
export async function forgetRemembered(): Promise<void> {
  const login = await loadLocalSession().catch(() => null)
  drop('')
  if (login) await clearLocalSession(login.id).catch(() => {})
}

/** begin makes a session the server just issued the current one: every sign-in ends here. */
async function begin(reply: SessionReply): Promise<void> {
  await adopt(loginFrom(reply), reply.user, reply.expires_at)
}

export async function signIn(email: string, password: string): Promise<void> {
  await begin(await auth.login(email, password))
}

export async function signUp(input: { invite: string; email: string; name: string; password: string }): Promise<void> {
  await begin(await auth.signup(input))
}

/**
 * adoptSession is how an edition's own sign-in (Edition.signIn) signs the
 * person in: it hands over what its route answered, which is what POST
 * /v1/auth/login answers, and the session begins exactly as a password
 * sign-in's does, stored for this browser and its other tabs, with the person
 * it names. A reply of any other shape is refused (ApiError
 * 'invalid_response') and changes nothing.
 */
export async function adoptSession(reply: unknown): Promise<void> {
  await begin(checked(reply, isSessionReply))
}

/**
 * signOut leaves this browser at once. The server is told, but a server that
 * cannot be reached must not keep anyone signed in here.
 *
 * Signing out everywhere is different: its whole point is the other browsers,
 * so the server has to confirm it before this one lets go, or the person would
 * walk away believing a lost laptop was signed out.
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
 * changePassword ends every session of the person on the server, this one
 * included, and answers with the only token left. It replaces the stored one,
 * which also tells every other tab that its login is gone.
 */
export async function changePassword(current: string, next: string): Promise<void> {
  const userID = session.user?.id
  const reply = await authorized(t => auth.changePassword(t, current, next))
  if (!userID || reply.user.id !== userID || identity() !== userID) return
  await adopt(loginFrom(reply), reply.user, reply.expires_at)
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
