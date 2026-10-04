// /v1/accounts: the mailboxes the signed-in person owns. Ownership is decided
// by the server; this file only asks.

import { segment } from './endpoint'
import { checked, request } from './http'
import {
  isAccount, isAccountList, isAddAccountResult, isAuthFlow, isFolderList,
  type Account, type AddAccountRequest, type AddAccountResult, type AuthFlow, type FlowKind, type Folder,
} from './types'

/**
 * A password account is signed in to before anything is stored, with a 30 s
 * budget on the server; the client waits a little longer than that so the
 * server's own answer, not our deadline, decides the outcome.
 */
export const ADD_ACCOUNT_TIMEOUT_MS = 45_000
/** Listing folders dials the mail server live (60 s server budget). */
export const FOLDERS_TIMEOUT_MS = 70_000
/**
 * Completing consent exchanges the code with the provider: up to 40 s on the
 * server, inside a 45 s route. The client waits longer, so the server's answer
 * decides and a slow provider is not reported as a failure that then succeeds.
 */
export const COMPLETE_OAUTH_TIMEOUT_MS = 50_000

export async function listAccounts(token: string, signal?: AbortSignal): Promise<Account[]> {
  return checked(await request('/v1/accounts', { token, signal }), isAccountList)
}

export async function getAccount(token: string, id: string, signal?: AbortSignal): Promise<Account> {
  return checked(await request(`/v1/accounts/${segment(id)}`, { token, signal }), isAccount)
}

export async function addAccount(token: string, body: AddAccountRequest): Promise<AddAccountResult> {
  return checked(await request('/v1/accounts', { token, body, timeoutMS: ADD_ACCOUNT_TIMEOUT_MS }), isAddAccountResult)
}

/** Starts consent again for an account that has none, or lost it. The reply is a bare flow, not {account, auth}. */
export async function startAuth(token: string, id: string, flow?: FlowKind): Promise<AuthFlow> {
  return checked(await request(`/v1/accounts/${segment(id)}/oauth/start`, { token, body: flow ? { flow } : {} }), isAuthFlow)
}

/** Hands the provider's redirect back to the server, which checks it belongs to this user before exchanging the code. */
export async function completeOAuth(token: string, redirectURL: string): Promise<Account> {
  return checked(await request('/v1/accounts/oauth/callback', { token, body: { redirect_url: redirectURL }, timeoutMS: COMPLETE_OAUTH_TIMEOUT_MS }), isAccount)
}

export async function removeAccount(token: string, id: string): Promise<void> {
  await request<void>(`/v1/accounts/${segment(id)}`, { token, method: 'DELETE' })
}

export async function listFolders(token: string, id: string, signal?: AbortSignal): Promise<Folder[]> {
  return checked(await request(`/v1/accounts/${segment(id)}/folders`, { token, signal, timeoutMS: FOLDERS_TIMEOUT_MS }), isFolderList)
}
