// /v1/auth: the signed-in person, and the key scheme's ceremonies
// (docs/key-scheme.md section 12). The password never travels: what does is
// an auth key derived from it in this browser (crypto/account.ts), with the
// one exception of the upgrade's old password, in the release that brings
// the scheme only. The calls made without a bearer token are the ones that
// start a session or prove a secret for an address.

import { checked, request } from './http'
import {
  isChallenge, isLoginReply, isMe, isOpening, isPasswordBegin, isRecoverOpen, isSessionReply, isStepUpReply, isUpgradeTicket, isUser,
  type Challenge, type KDFWire, type LoginReply, type Me, type Opening, type PasswordBegin, type RecoverOpen, type SessionReply,
  type StepUpReply, type UpgradeTicket, type User,
} from './types'

/** Restoring a remembered session waits this long before offering a retry. */
export const RESTORE_TIMEOUT_MS = 15_000
/** A new password has at least this many code points (the platform's preparation, crypto/mailie.ts prepareNewPassword). */
export const MIN_PASSWORD = 12

/** What enrols a person: everything the server keeps of the account key, the password and the recovery code, none of which it can open. */
export interface Enrolment {
  auth_key: string
  kdf: KDFWire
  public_key: string
  password_wrap: string
  recovery_wrap: string
  recovery_proof: string
}

export async function challenge(email: string): Promise<Challenge> {
  return checked(await request('/v1/auth/challenge', { body: { email } }), isChallenge)
}

export async function login(email: string, authKey: string): Promise<LoginReply> {
  return checked(await request('/v1/auth/login', { body: { email, auth_key: authKey } }), isLoginReply)
}

export async function openSignUp(invite: string, email: string): Promise<Opening> {
  return checked(await request('/v1/auth/signup/open', { body: { invite, email } }), isOpening)
}

export async function signup(input: { invite: string; email: string; name: string; seal_id: string } & Enrolment): Promise<SessionReply> {
  return checked(await request('/v1/auth/signup', { body: input }), isSessionReply)
}

export async function openReset(reset: string, email: string): Promise<Opening> {
  return checked(await request('/v1/auth/reset/open', { body: { reset, email } }), isOpening)
}

export async function reset(input: { reset: string; email: string } & Enrolment): Promise<SessionReply> {
  return checked(await request('/v1/auth/reset', { body: input }), isSessionReply)
}

export async function openRecovery(email: string, recoveryProof: string): Promise<RecoverOpen> {
  return checked(await request('/v1/auth/recover/open', { body: { email, recovery_proof: recoveryProof } }), isRecoverOpen)
}

/** Every session of the person ends; they sign in with the new password. */
export async function finishRecovery(input: { ticket: string } & Omit<Enrolment, 'public_key'>): Promise<void> {
  await request<void>('/v1/auth/recover/finish', { body: input })
}

/** The upgrade's one password in clear (docs/key-scheme.md section 12.7), in the release that brings the key scheme only. */
export async function upgradeLogin(email: string, password: string): Promise<UpgradeTicket> {
  return checked(await request('/v1/auth/upgrade/login', { body: { email, password } }), isUpgradeTicket)
}

export async function upgradeEnrol(input: { ticket: string } & Enrolment): Promise<SessionReply> {
  return checked(await request('/v1/auth/upgrade/enrol', { body: input }), isSessionReply)
}

export async function me(token: string, signal?: AbortSignal): Promise<Me> {
  return checked(await request('/v1/auth/me', { token, signal, timeoutMS: RESTORE_TIMEOUT_MS }), isMe)
}

export async function logout(token: string, everywhere = false): Promise<void> {
  await request<void>('/v1/auth/logout', { token, body: { everywhere } })
}

/** The current password wrap and the target of a new password: answered only to the current auth key, never to the session alone. */
export async function beginPasswordChange(token: string, currentAuthKey: string): Promise<PasswordBegin> {
  return checked(await request('/v1/auth/password/begin', { token, body: { current_auth_key: currentAuthKey } }), isPasswordBegin)
}

/**
 * Stores a new auth key and password wrap. A password change answers the
 * only session left, every other one having ended; a sign-in's
 * re-derivation answers nothing (204), and ends nothing.
 */
export async function finishPasswordChange(token: string, input: { ticket: string; auth_key: string; kdf: KDFWire; password_wrap: string }): Promise<SessionReply | undefined> {
  const reply = await request<unknown>('/v1/auth/password/finish', { token, body: input })
  return reply === undefined ? undefined : checked(reply, isSessionReply)
}

/** Replaces the recovery code; needs a step-up within the last ten minutes. */
export async function replaceRecovery(token: string, input: { recovery_wrap: string; recovery_proof: string }): Promise<void> {
  await request<void>('/v1/auth/recovery', { token, body: input })
}

export async function stepUp(token: string, authKey: string): Promise<StepUpReply> {
  return checked(await request('/v1/auth/stepup', { token, body: { auth_key: authKey } }), isStepUpReply)
}

export async function updateProfile(token: string, name: string): Promise<User> {
  return checked(await request('/v1/auth/profile', { token, method: 'PUT', body: { name } }), isUser)
}
