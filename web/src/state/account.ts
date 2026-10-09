// The key scheme's ceremonies, as this console runs them (docs/key-scheme.md
// section 12): what is asked of the server, in which order, and what is kept.
// The cryptography is crypto/account.ts's; the session is state/session.ts's,
// where each ceremony that signs someone in ends.
//
// The password never leaves this page, but once: the upgrade of an account
// made before the key scheme sends it in clear to the server that already
// checks it (section 12.7), and only for an address this browser has never
// seen enrol. A new recovery code is shown once (recoveryCode, below) and
// kept nowhere.

import { reactive } from 'vue'
import { fromBase64URL } from '@thehappieco/kit/bytes'
import * as auth from '../api/auth'
import { ApiError } from '../api/http'
import { enrolled, type LoginReply, type Opening, type SessionReply, type UpgradeTicket, type User } from '../api/types'
import { checkNewPassword, deriveKeys, enrol, newRecovery, openWrap, recoveryKeys, rewrap, type Enrolled } from '../crypto/account'
import { CeremonyError } from '../crypto/errors'
import { accountKeyOf, keepAccountKey, rememberEnrolled, rememberedEnrolled } from './accountVault'
import { authorized, beginSession, markKeyed, replaceSession, session, steppedUp } from './session'

/**
 * The recovery code a ceremony just made, until the person says they saved
 * it: in this page's memory only, and shown by components/RecoveryCodeDialog
 * over whatever is on screen. Lost on a reload; the person replaces it then
 * from their account.
 */
export const recoveryCode = reactive<{ code: string; reason: '' | 'new-account' | 'recovered' | 'replaced' }>({ code: '', reason: '' })

function showRecoveryCode(code: string, reason: typeof recoveryCode.reason): void {
  Object.assign(recoveryCode, { code, reason })
}

/** The person saved the code: it is forgotten here. */
export function recoveryCodeSaved(): void {
  Object.assign(recoveryCode, { code: '', reason: '' })
}

const keyOf = (text: string) => fromBase64URL(text, 32)

/** Ends, on the server, a session this page will not use: one whose wrap did not open, or that names another key. */
function refuse(reply: SessionReply): never {
  void auth.logout(reply.token).catch(() => { /* It expires on its own. */ })
  throw new CeremonyError('security')
}

/** The person signed in must be the one the ceremony enrolled: the seal id it bound, and the public key it made. */
function sameKey(user: User, sealID: string, publicKey: string): boolean {
  return user.seal_id === sealID && user.public_key === publicKey
}

/**
 * A sign-in that leaves the person to choose a new password: the upgrade of
 * an account whose old password the platform's preparation refuses (a
 * control character, more than 256 code points; section 12.7, step 4). The
 * ticket stays in this page's memory, for ten minutes at most.
 */
export interface PendingUpgrade { email: string; ticket: UpgradeTicket }

/**
 * signIn signs a person in (docs/key-scheme.md section 12.2): the challenge,
 * the derivation under its salt and parameters, the auth key, and the
 * password wrap opened with the wrap key. A wrap that does not open after the
 * server accepted the auth key is a security error, and the session it opened
 * is ended. When the account is off its target, the same password is
 * derived again under the target before the console opens; if that fails,
 * the next sign-in asks again.
 *
 * An address the challenge says has not upgraded is upgraded here (section
 * 12.7), unless this browser remembers it enrolled: then nothing is sent, and
 * it is a security error. It answers a PendingUpgrade when the old password
 * cannot be used as it is, and null once signed in.
 */
export async function signIn(email: string, password: string): Promise<PendingUpgrade | null> {
  const answer = await auth.challenge(email)
  if (answer.upgrade) return upgrade(email, password)
  const keys = await deriveKeys(password, answer, 'presented')
  const reply = await auth.login(email, keys.authKey)
  let accountKey: Uint8Array
  try {
    accountKey = await openWrap('password', keys.wrapKey, reply.password_wrap, reply.user.seal_id!, reply.user.public_key!)
  } catch {
    refuse(reply)
  }
  try {
    await rederive(reply, password, accountKey)
    await keepAccountKey(accountKey, keyOf(reply.user.public_key!), reply.user.seal_id!)
  } finally {
    accountKey.fill(0)
  }
  await beginSession(reply, true)
  await rememberEnrolled(email)
  return null
}

/** The sign-in's re-derivation (section 12.2, step 5): nothing ends, and a failure only leaves it for the next sign-in. */
async function rederive(reply: LoginReply, password: string, accountKey: Uint8Array): Promise<void> {
  const target = reply.rederive
  if (!target) return
  try {
    const next = await rewrap(password, target, 'presented', accountKey, reply.user.seal_id!)
    await auth.finishPasswordChange(reply.token, { ticket: target.ticket, ...next })
  } catch { /* The account stays where it was; its next sign-in names the target again. */ }
}

async function upgrade(email: string, password: string): Promise<PendingUpgrade | null> {
  // The one password in clear, never to an address this browser saw enrol:
  // a server that says otherwise is not believed.
  if (await rememberedEnrolled(email)) throw new CeremonyError('security')
  const ticket = await auth.upgradeLogin(email, password)
  try {
    await finishEnrolment(email, password, ticket, 'presented')
  } catch (error) {
    if (error instanceof CeremonyError && error.code === 'password_rejected') return { email, ticket }
    throw error
  }
  return null
}

/** finishUpgrade enrols with a new password, when the old one cannot be used as it is (PendingUpgrade). */
export async function finishUpgrade(pending: PendingUpgrade, password: string): Promise<void> {
  checkNewPassword(password)
  await finishEnrolment(pending.email, password, pending.ticket, 'new')
}

async function finishEnrolment(email: string, password: string, ticket: UpgradeTicket, use: 'new' | 'presented'): Promise<void> {
  const made = await enrol(password, ticket, use)
  try {
    const reply = await auth.upgradeEnrol({ ticket: ticket.ticket, ...made.enrolment })
    await startEnrolled(email, reply, made, ticket.seal_id)
  } finally {
    made.accountKey.fill(0)
  }
}

/** startEnrolled signs in the person an enrolment made, keeps their account key, and shows the recovery code once. */
async function startEnrolled(email: string, reply: SessionReply, made: Enrolled, sealID: string): Promise<void> {
  if (!sameKey(reply.user, sealID, made.enrolment.public_key)) refuse(reply)
  await keepAccountKey(made.accountKey, made.publicKey, sealID)
  await beginSession(reply, true)
  await rememberEnrolled(email)
  showRecoveryCode(made.recoveryCode, 'new-account')
}

/** signUp creates an account by invitation (section 12.1). The new password is checked before anything is asked of the server. */
export async function signUp(input: { invite: string; email: string; name: string; password: string }): Promise<void> {
  checkNewPassword(input.password)
  const opened: Opening = await auth.openSignUp(input.invite, input.email)
  const made = await enrol(input.password, opened, 'new')
  try {
    const reply = await auth.signup({ invite: input.invite, email: input.email, name: input.name, seal_id: opened.seal_id, ...made.enrolment })
    await startEnrolled(input.email, reply, made, opened.seal_id)
  } finally {
    made.accountKey.fill(0)
  }
}

/**
 * openResetLink checks a reset link before the person chooses a password,
 * so that a link that is not valid, or that would take the last reader of a
 * team mailbox, says so first.
 */
export async function openResetLink(reset: string, email: string): Promise<Opening> {
  return auth.openReset(reset, email)
}

/**
 * resetPassword uses a reset link (section 12.6): a new password, recovery
 * code and account key, derived under the target the link answers, which the
 * reset stores (never the challenge's answer, which for an account off its
 * target is the salt it stores now).
 */
export async function resetPassword(input: { reset: string; email: string; password: string }, opened?: Opening): Promise<void> {
  checkNewPassword(input.password)
  const target = opened ?? await auth.openReset(input.reset, input.email)
  const made = await enrol(input.password, target, 'new')
  try {
    const reply = await auth.reset({ reset: input.reset, email: input.email, ...made.enrolment })
    await startEnrolled(input.email, reply, made, target.seal_id)
  } finally {
    made.accountKey.fill(0)
  }
}

/**
 * recover replaces a forgotten password with the recovery code (section
 * 12.4): the code's proof opens the recovery, the code's key opens the
 * account key, which is wrapped under the new password and a new code. Every
 * session of the person ends, and this browser signs in with the new
 * password; the new code is shown once.
 */
export async function recover(input: { email: string; code: string; password: string }): Promise<void> {
  checkNewPassword(input.password)
  const keys = await recoveryKeys(input.code)
  const opened = await auth.openRecovery(input.email, keys.proof)
  const accountKey = await openWrap('recovery', keys.key, opened.recovery_wrap, opened.seal_id, opened.public_key)
  try {
    const next = await rewrap(input.password, opened, 'new', accountKey, opened.seal_id)
    const recovery = await newRecovery(accountKey, opened.seal_id)
    await auth.finishRecovery({ ticket: opened.ticket, ...next, recovery_wrap: recovery.recoveryWrap, recovery_proof: recovery.recoveryProof })
    // Shown even if the sign-in below fails: the old code is gone.
    showRecoveryCode(recovery.code, 'recovered')
    // The new password, under the target the recovery stored: its auth key is the one just sent.
    const reply = await auth.login(input.email, next.auth_key)
    if (!sameKey(reply.user, opened.seal_id, opened.public_key)) refuse(reply)
    await keepAccountKey(accountKey, keyOf(opened.public_key), opened.seal_id)
    await beginSession(reply, true)
    await rememberEnrolled(input.email)
  } finally {
    accountKey.fill(0)
  }
}

/** The signed-in person, enrolled; anyone else has no account key to change anything of. */
function enrolledPerson(): User & { seal_id: string; public_key: string } {
  const user = session.user
  if (!user || !enrolled(user)) throw new ApiError('not_authorized')
  return user as User & { seal_id: string; public_key: string }
}

/**
 * changePassword changes the password in two steps (section 12.3): the
 * current one, derived under the account's own salt and parameters, gets the
 * password wrap; the same account key is wrapped under the new one, derived
 * under the target. Every other session ends; this browser goes on with the
 * session the server answers. The account key, the grants and the recovery
 * code do not change.
 */
export async function changePassword(current: string, next: string): Promise<void> {
  const user = enrolledPerson()
  checkNewPassword(next)
  const answer = await auth.challenge(user.email)
  const keys = await deriveKeys(current, answer, 'presented')
  const begun = await authorized(token => auth.beginPasswordChange(token, keys.authKey))
  const accountKey = await openWrap('password', keys.wrapKey, begun.password_wrap, user.seal_id, user.public_key)
  try {
    // The key this browser kept, if any, must be the one the server's wrap holds.
    const kept = await accountKeyOf(user.seal_id, user.public_key)
    const same = kept === null || kept.every((byte, i) => byte === accountKey[i])
    kept?.fill(0)
    if (!same) throw new CeremonyError('security')
    const rewrapped = await rewrap(next, begun, 'new', accountKey, user.seal_id)
    const reply = await authorized(token => auth.finishPasswordChange(token, { ticket: begun.ticket, ...rewrapped }))
    if (!reply) throw new ApiError('invalid_response')
    await keepAccountKey(accountKey, keyOf(user.public_key), user.seal_id)
    await replaceSession(reply, true)
  } finally {
    accountKey.fill(0)
  }
  await rememberEnrolled(user.email)
}

/**
 * stepUp proves the session's own person again with their password (section
 * 11): derived under their own salt and parameters, checked against this
 * session's person only, and refreshing this session's step-up time only.
 */
export async function stepUp(password: string): Promise<void> {
  const user = enrolledPerson()
  const answer = await auth.challenge(user.email)
  const keys = await deriveKeys(password, answer, 'presented')
  const reply = await authorized(token => auth.stepUp(token, keys.authKey))
  steppedUp(user.id, reply.authenticated_at)
  await rememberEnrolled(user.email)
}

/**
 * replaceRecoveryCode makes a new recovery code over the account key this
 * browser kept, and replaces the old one (section 12.5); the server needs a
 * step-up within the last ten minutes, which the caller asks for first
 * (freshStepUp). Without the key here (another browser's sign-in, storage
 * refused), it is no_account_key: signing in again on this browser keeps it.
 */
export async function replaceRecoveryCode(): Promise<void> {
  const user = enrolledPerson()
  const accountKey = await accountKeyOf(user.seal_id, user.public_key)
  if (!accountKey) {
    session.keyed = false
    throw new CeremonyError('no_account_key')
  }
  try {
    markKeyed(user.id)
    const recovery = await newRecovery(accountKey, user.seal_id)
    await authorized(token => auth.replaceRecovery(token, { recovery_wrap: recovery.recoveryWrap, recovery_proof: recovery.recoveryProof }))
    showRecoveryCode(recovery.code, 'replaced')
  } finally {
    accountKey.fill(0)
  }
}
