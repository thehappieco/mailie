// Consent to sync, and each account's sync. Who may do what is decided by the
// server (internal/service); this file only asks.

import { segment } from './endpoint'
import { checked, request } from './http'
import { isAccountSync, isSyncConsent, type AccountSync, type SyncConsent } from './types'

/**
 * Withdrawing deletes everything indexed for the person's mailboxes and then
 * compacts the files that held it; the route allows 150 s for that. The
 * client waits a little longer, so the server's answer decides the outcome.
 */
export const WITHDRAW_TIMEOUT_MS = 160_000

export async function getSyncConsent(token: string, signal?: AbortSignal): Promise<SyncConsent> {
  return checked(await request('/v1/me/sync-consent', { token, signal }), isSyncConsent)
}

/** version names the revision of the text the person was shown; the server refuses any other. */
export async function grantSyncConsent(token: string, version: string): Promise<SyncConsent> {
  return checked(await request('/v1/me/sync-consent', { token, body: { version } }), isSyncConsent)
}

export async function withdrawSyncConsent(token: string): Promise<SyncConsent> {
  return checked(await request('/v1/me/sync-consent', { token, method: 'DELETE', timeoutMS: WITHDRAW_TIMEOUT_MS }), isSyncConsent)
}

export async function getSyncStatus(token: string, id: string, signal?: AbortSignal): Promise<AccountSync> {
  return checked(await request(`/v1/accounts/${segment(id)}/sync`, { token, signal }), isAccountSync)
}

/** Asks for a pass soon. The answer (202) is the status as it stands, not the pass's outcome. */
export async function triggerSync(token: string, id: string): Promise<AccountSync> {
  return checked(await request(`/v1/accounts/${segment(id)}/sync`, { token, body: {} }), isAccountSync)
}
