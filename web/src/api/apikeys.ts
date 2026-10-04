// /v1/me/apikeys: the API keys a person creates for their own tools. Only a
// console session reaches these routes; a key can never list, create or
// revoke keys. Which mailboxes a key may name, how many a person may hold and
// which text they must have agreed to are decided by the server
// (internal/service); this file only asks.

import { segment } from './endpoint'
import { checked, request } from './http'
import { isCreatedKey, isPersonalKey, isPersonalKeyList, type CreateKeyRequest, type CreatedKey, type PersonalKey } from './types'

/**
 * listed keeps only the fields a listed key has. The list is drawn and kept
 * for the page's life, so a field a server should never have sent (a secret
 * above all) does not stay in memory because it came along.
 */
export function listed(key: PersonalKey): PersonalKey {
  const out: PersonalKey = { prefix: key.prefix, name: key.name, scope: key.scope, created_at: key.created_at, expires_at: key.expires_at, terms_version: key.terms_version }
  if (key.account_ids !== undefined) out.account_ids = [...key.account_ids]
  if (key.restricted !== undefined) out.restricted = key.restricted
  if (key.last_used_at !== undefined) out.last_used_at = key.last_used_at
  if (key.revoked_at !== undefined) out.revoked_at = key.revoked_at
  return out
}

export async function listKeys(token: string, signal?: AbortSignal): Promise<PersonalKey[]> {
  return checked(await request('/v1/me/apikeys', { token, signal }), isPersonalKeyList).map(listed)
}

/**
 * The answer carries the key's secret, the only time it exists outside the
 * server's hash. The caller shows it once and lets go of it.
 */
export async function createKey(token: string, input: CreateKeyRequest): Promise<CreatedKey> {
  return checked(await request('/v1/me/apikeys', { token, body: input }), isCreatedKey)
}

/**
 * Revokes one of the person's keys. A success is the answer that matters:
 * the key as it is now when the server sends it, undefined for anything else
 * (204, or a body this console does not read), and the caller then reads the
 * list again rather than call a revoked key live.
 */
export async function revokeKey(token: string, prefix: string): Promise<PersonalKey | undefined> {
  const answer = await request<unknown>(`/v1/me/apikeys/${segment(prefix)}`, { token, method: 'DELETE' })
  return isPersonalKey(answer) && answer.prefix === prefix ? listed(answer) : undefined
}
