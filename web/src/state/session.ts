// The signed-in person, and the one bearer token that proves it.
//
// The token is module state, not reactive state: nothing renders it, and a
// reactive copy would be one devtools click from the screen. Everything that
// needs it goes through authorized(), which is also where a 401 in the middle
// of use ends the session — once, for the token that was refused, and never
// for a request that raced a newer sign-in.
//
// The ceremonies that start a session (signing in and up, a recovery, a
// reset link) are state/account.ts's: each ends here, in
// beginSession. Where a session ends without the person asking, the account
// key this browser kept goes with it (docs/key-scheme.md section 7).

import { reactive } from 'vue'
import * as auth from '../api/auth'
import { ApiError, checked } from '../api/http'
import { enrolled, isSessionReply, type SessionReply, type User } from '../api/types'
import { edition } from '../edition'
import { uuid } from '../ui/uuid'
import { serverNow } from './connection'
import { forgetHeldAccountKey, holdsAccountKey, outlivedExpiry, sessionBegan, settleRecord, wipeAccountKey } from './accountVault'
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

// The account key this browser kept goes with the session it was kept for
// (docs/key-scheme.md section 7): a self-hosted server's rule, where every
// sign-in opens the password wrap anyway, and the vault is wiped at once at
// every ending. An edition that keeps it past a session that merely expires
// (Edition.accountKey.outlivesExpiry) keeps it only where this page
// established that expiry by the server's clock, and marks the record so
// before the session's own record goes; everything else wipes it: a
// sign-out, a refusal before the expiry, a session given up on unchecked,
// and a page that finds no session record next to an unmarked key. What an
// ending does reaches only the vault record bound to that session's login
// (state/accountVault.ts sessionBegan, settleRecord), never one a sign-in
// since wrote, in this page or another tab. A page that stops halfway leaves
// either the session's record, which the next page checks again, or an
// unmarked key, which it wipes.

const keepsPastExpiry = (): boolean => edition().accountKey?.outlivesExpiry === true

/** pastExpiry says whether the server's clock, as its latest answer's Date gives it (state/connection.ts), has reached expiresAt (unix seconds). */
function pastExpiry(expiresAt: number): boolean {
  return expiresAt > 0 && serverNow() >= expiresAt * 1000
}

/**
 * expiredAtServer asks the server again about a token it refused without a
 * Date (the event stream's error event), so the answer's Date says whether
 * its clock had passed expiresAt. A refusal at or after expiresAt is an
 * expiry; a token the server still takes, an answer that is not the daemon's
 * (the network, a gateway) and a refusal before it are not.
 */
async function expiredAtServer(refused: string, expiresAt: number): Promise<boolean> {
  try { await auth.me(refused) }
  catch (error) { return error instanceof ApiError && error.code === 'unauthorized' && pastExpiry(expiresAt) }
  return false
}

/** noSessionRecord settles the account key when this browser remembers no session it can use: kept only if marked as outliving an expiry. */
async function noSessionRecord(): Promise<void> {
  if (keepsPastExpiry() && await outlivedExpiry()) return
  await wipeAccountKey()
}

function scheduleExpiry(): void {
  clearTimeout(expiryTimer)
  // The server stops accepting the token at expires_at whatever the page
  // does; ending it here too means an idle console does not sit there looking
  // signed in until its next request fails. expires_at is the server's, so
  // the wait is by its clock.
  const ms = session.expiresAt * 1000 - serverNow()
  const expiring = token
  if (ms > 0 && ms < 2 ** 31 - 1) expiryTimer = setTimeout(() => expire(expiring, 'reached'), ms)
}

/**
 * How a session ended: its expiry came while the page held it ('reached'),
 * or the server refused its token, in an answer whose Date the page noted
 * ('dated': every HTTP answer's) or in one without (the event stream's error
 * event).
 */
type Ending = 'reached' | 'dated' | 'undated'

/**
 * Ends the session because its expiry came or the server refused its token.
 * Only the token that was refused ends anything. On a self-hosted server the
 * account key goes at once. Where the edition keeps it past an expiry, a
 * refusal is an expiry when the server's clock had passed expires_at, judged
 * by the refusal's own Date or, without one, by asking again; the record
 * that session held is settled before the session's own record is cleared.
 */
function expire(refused: string, ending: Ending): void {
  if (!token || refused !== token) return
  const id = loginID
  const expiresAt = session.expiresAt
  drop('expired')
  const clear = () => id ? clearLocalSession(id).catch(() => { /* Nothing stored, nothing to clear. */ }) : Promise.resolve()
  if (!keepsPastExpiry()) {
    void wipeAccountKey()
    void clear()
    return
  }
  const judged = ending === 'reached' || (ending === 'dated' && pastExpiry(expiresAt))
  void (async () => {
    const expired = judged || (ending === 'undated' && await expiredAtServer(refused, expiresAt))
    if (id) await settleRecord(id, expired)
    await clear()
  })()
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
  await sessionBegan(login.id)
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
  // Where the edition keeps the key past an expiry, a record past it by this
  // browser's clock still goes to the server, whose clock decides.
  try { login = await loadLocalSession({ expired: keepsPastExpiry() }) } catch { /* No storage, or none this browser can read: sign in instead. */ }
  if (attempt !== generation) return
  if (!login) { drop(''); await noSessionRecord(); return }
  try {
    const current = await auth.me(login.token)
    if (attempt !== generation) return
    if (current.user.id !== login.userID) throw new ApiError('unauthorized')
    // The account key kept for this person, if any: a record of anyone else is wiped here.
    const keyed = enrolled(current.user) && await holdsAccountKey(current.user.seal_id!, current.user.public_key!)
    if (attempt !== generation) return
    // Another tab signed in or out while the server answered: that tab settled what this login held,
    // and this page starts again from what the browser remembers now.
    if (await localSessionWasCleared(login.id).catch(() => false)) { if (attempt === generation) await restore(); return }
    if (attempt !== generation) return
    // Adopt without writing: the record is already the one just read.
    token = login.token
    loginID = login.id
    Object.assign(session, {
      ...fresh('ready'), user: current.user, expiresAt: current.session.expires_at,
      authenticatedAt: current.session.authenticated_at ?? 0, keyed,
    })
    scheduleExpiry()
    await sessionBegan(login.id, { restored: true })
  } catch (error) {
    if (attempt !== generation) return
    if (error instanceof ApiError && error.code === 'unauthorized') {
      // The refusal's Date (state/connection.ts) says whether it came past the expiry.
      const { id, expiresAt } = login
      drop('')
      if (keepsPastExpiry()) await settleRecord(id, pastExpiry(expiresAt))
      else await wipeAccountKey()
      await clearLocalSession(id).catch(() => {})
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

/**
 * Gives up on a remembered session that cannot be checked right now, and
 * shows the sign-in form. Unchecked, it is never an expiry: the account key
 * goes with it.
 */
export async function forgetRemembered(): Promise<void> {
  const login = await loadLocalSession({ expired: true }).catch(() => null)
  drop('')
  await wipeAccountKey()
  if (login) await clearLocalSession(login.id).catch(() => {})
}

/** What runs right after a sign-in in this page (onSignIn). */
const signInListeners = new Set<() => void>()

/**
 * onSignIn runs listener right after every sign-in this page makes
 * (beginSession), once the session is the current one; never after a
 * session restored from this browser's record, nor a password change's
 * replacement. It answers what stops it. What only a sign-in's step-up time
 * allows without asking for the password again, a mailbox's first key
 * (docs/key-scheme.md section 12.14), starts here.
 */
export function onSignIn(listener: () => void): () => void {
  signInListeners.add(listener)
  return () => { signInListeners.delete(listener) }
}

/**
 * beginSession makes a session the server just issued the current one: every
 * sign-in ends here (state/account.ts, once its ceremony is done; and
 * adoptSession). keyed says whether the ceremony kept the person's account
 * key in this browser.
 */
export async function beginSession(reply: SessionReply, keyed = false): Promise<void> {
  await adopt(loginFrom(reply), reply.user, reply.expires_at, reply.authenticated_at ?? 0, keyed)
  for (const listener of [...signInListeners]) {
    try { listener() } catch { /* What follows a sign-in never undoes it. */ }
  }
}

/**
 * adoptSession is how an edition's own sign-in (Edition.signIn) signs the
 * person in: it hands over what its route answered, a session as POST
 * /v1/auth/signup answers one, and the session begins exactly as a password
 * sign-in's does, stored for this browser and its other tabs, with the person
 * it names. It begins keyed when this browser holds the account key of that
 * person, as a remembered session is restored: the key the edition's sign-in
 * kept for them just before (keepAccountKey), or one a session of theirs
 * kept past its expiry; a key of anyone else is wiped first. A reply of any
 * other shape is refused (ApiError 'invalid_response') and changes nothing.
 */
export async function adoptSession(reply: unknown): Promise<void> {
  const answer = checked(reply, isSessionReply)
  const keyed = enrolled(answer.user) && await holdsAccountKey(answer.user.seal_id!, answer.user.public_key!)
  await beginSession(answer, keyed)
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
    // An HTTP answer's refusal carries its status, and its Date was noted (api/http.ts); the event stream's error event carries neither.
    if (error instanceof ApiError && error.code === 'unauthorized') expire(current, error.status ? 'dated' : 'undated')
    throw error
  }
}

/**
 * replaceSession takes the only session left after a password change ended
 * every other one, this one included. It replaces the stored one, which also
 * tells every other tab that its login is gone. Nothing is taken for anyone
 * but the person signed in: false when the session it replaces ended first.
 */
export async function replaceSession(reply: SessionReply, keyed: boolean): Promise<boolean> {
  const userID = session.user?.id
  if (!userID || reply.user.id !== userID || identity() !== userID) return false
  await adopt(loginFrom(reply), reply.user, reply.expires_at, reply.authenticated_at ?? 0, keyed)
  return true
}

/** steppedUp records the session's new step-up time: this session's only, for the person who proved it. */
export function steppedUp(userID: string, authenticatedAt: number): void {
  if (identity() === userID) session.authenticatedAt = authenticatedAt
}

/** markKeyed records that this browser now holds the account key of the person signed in. */
export function markKeyed(userID: string): void {
  if (identity() === userID) session.keyed = true
}

/** The server's ten minutes (docs/key-scheme.md section 11). */
const STEP_UP_WINDOW_S = 10 * 60
/** What freshStepUp leaves of them by default: a margin for the clocks and the request. */
export const STEP_UP_MARGIN_S = 30

/**
 * freshStepUp says whether the session proved its person recently enough for
 * what the step-up guards, with margin seconds of the ten minutes still to
 * spare: a call that takes long before the server checks it again (linking
 * a mailbox, which signs in to the mail server first) asks for more. The
 * step-up time is the server's, so it is judged by the server's clock
 * (state/connection.ts serverNow), never this browser's: a browser whose
 * clock runs ahead would otherwise find a step-up it just made already old,
 * and ask for the password again and again.
 */
export function freshStepUp(now = serverNow(), margin = STEP_UP_MARGIN_S): boolean {
  const at = session.authenticatedAt
  return at > 0 && now / 1000 - at < STEP_UP_WINDOW_S - margin
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
