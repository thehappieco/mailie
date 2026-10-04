// /v1/auth: the signed-in person. Sign-in and sign-up are the only calls in the
// console made without a bearer token.

import { checked, request } from './http'
import { isMe, isSessionReply, isUser, type Me, type SessionReply, type User } from './types'

/** Restoring a remembered session waits this long before offering a retry. */
export const RESTORE_TIMEOUT_MS = 15_000
/** The server refuses shorter passwords; the forms say so before asking it. */
export const MIN_PASSWORD = 10

export async function login(email: string, password: string): Promise<SessionReply> {
  return checked(await request('/v1/auth/login', { body: { email, password } }), isSessionReply)
}

export async function signup(input: { invite: string; email: string; name: string; password: string }): Promise<SessionReply> {
  return checked(await request('/v1/auth/signup', { body: input }), isSessionReply)
}

export async function me(token: string, signal?: AbortSignal): Promise<Me> {
  return checked(await request('/v1/auth/me', { token, signal, timeoutMS: RESTORE_TIMEOUT_MS }), isMe)
}

export async function logout(token: string, everywhere = false): Promise<void> {
  await request<void>('/v1/auth/logout', { token, body: { everywhere } })
}

/** The reply carries a new token: the server ends every session of the user, this one included. */
export async function changePassword(token: string, current: string, next: string): Promise<SessionReply> {
  return checked(await request('/v1/auth/password', { token, body: { current, next } }), isSessionReply)
}

export async function updateProfile(token: string, name: string): Promise<User> {
  return checked(await request('/v1/auth/profile', { token, method: 'PUT', body: { name } }), isUser)
}
