// API keys (docs/workspaces.md, "API keys"): a workspace's, which its owners
// and admins list, create, revoke and give mailboxes to
// (/v1/workspaces/{id}/apikeys), and the ones a person created, in every
// workspace, which they list and revoke (/v1/me/apikeys). Only a console
// session reaches these routes; a key can never list, create or change keys.
// Who may give a key what, how many a workspace may hold and which text the
// creator must have agreed to are decided by the server (internal/service);
// this file only asks.

import { segment } from './endpoint'
import { checked, request } from './http'
import {
  isCreatedKey, isKeyMailbox, isKeySendList, isWorkspaceKeyList, type CreateKeyRequest, type CreatedKey, type KeyFlags, type KeyMailbox,
  type KeySend, type WorkspaceKey,
} from './types'

/**
 * listed keeps only the fields a listed key has. The list is drawn and kept
 * for the page's life, so a field a server should never have sent (a secret
 * above all) does not stay in memory because it came along.
 */
export function listed(key: WorkspaceKey): WorkspaceKey {
  const out: WorkspaceKey = {
    prefix: key.prefix, name: key.name, scope: key.scope, mailboxes: key.mailboxes.map(heldOn), created_at: key.created_at,
    expires_at: key.expires_at, live: key.live, terms_version: key.terms_version, sends: key.sends,
  }
  if (key.workspace_id !== undefined) out.workspace_id = key.workspace_id
  if (key.carried_over !== undefined) out.carried_over = key.carried_over
  if (key.origin !== undefined) out.origin = key.origin
  if (key.other_workspaces !== undefined) out.other_workspaces = key.other_workspaces
  if (key.created_by !== undefined) out.created_by = key.created_by
  if (key.last_used_at !== undefined) out.last_used_at = key.last_used_at
  if (key.revoked_at !== undefined) out.revoked_at = key.revoked_at
  return out
}

/** What a key holds on a mailbox, with only the fields that has. */
export function heldOn(item: KeyMailbox): KeyMailbox {
  const out: KeyMailbox = { account_id: item.account_id, workspace_id: item.workspace_id, read: item.read, act: item.act, send: item.send, updated_at: item.updated_at }
  if (item.granted_by !== undefined) out.granted_by = item.granted_by
  return out
}

const keysOf = (workspaceID: string) => `/v1/workspaces/${segment(workspaceID)}/apikeys`

/** A workspace's keys, revoked and expired ones too: for its owners and admins. */
export async function listWorkspaceKeys(token: string, workspaceID: string, signal?: AbortSignal): Promise<WorkspaceKey[]> {
  return checked(await request(keysOf(workspaceID), { token, signal }), isWorkspaceKeyList).map(listed)
}

/**
 * The answer carries the key's secret, the only time it exists outside the
 * server's hash. The caller shows it once and lets go of it.
 */
export async function createWorkspaceKey(token: string, workspaceID: string, input: CreateKeyRequest): Promise<CreatedKey> {
  return checked(await request(keysOf(workspaceID), { token, body: input }), isCreatedKey)
}

/**
 * Revokes a key of the workspace; a key carried over from before loses this
 * workspace's mailboxes instead. The answer is 204: the caller reads the list
 * again rather than draw what it guesses.
 */
export async function revokeWorkspaceKey(token: string, workspaceID: string, prefix: string): Promise<void> {
  await request<void>(`${keysOf(workspaceID)}/${segment(prefix)}`, { token, method: 'DELETE' })
}

/** Sets exactly what a key holds on a mailbox: every flag is sent, so leaving one out never takes it away. */
export async function setKeyAccess(token: string, workspaceID: string, prefix: string, accountID: string, flags: KeyFlags): Promise<KeyMailbox> {
  const body: KeyFlags = { read: flags.read, act: flags.act, send: flags.send }
  const path = `${keysOf(workspaceID)}/${segment(prefix)}/accounts/${segment(accountID)}`
  return heldOn(checked(await request(path, { token, method: 'PUT', body }), isKeyMailbox))
}

/** Takes a mailbox out of a key: everything it held there. */
export async function dropKeyAccess(token: string, workspaceID: string, prefix: string, accountID: string): Promise<void> {
  await request<void>(`${keysOf(workspaceID)}/${segment(prefix)}/accounts/${segment(accountID)}`, { token, method: 'DELETE' })
}

/** A key's sends from the workspace's mailboxes, newest first: never a subject, an address or a text. */
export async function listKeySends(token: string, workspaceID: string, prefix: string, signal?: AbortSignal): Promise<KeySend[]> {
  return checked(await request(`${keysOf(workspaceID)}/${segment(prefix)}/sends`, { token, signal }), isKeySendList)
}

/** The keys the person signed in created, in every workspace. */
export async function listMyKeys(token: string, signal?: AbortSignal): Promise<WorkspaceKey[]> {
  return checked(await request('/v1/me/apikeys', { token, signal }), isWorkspaceKeyList).map(listed)
}

/** Revokes a key the person created, in whichever workspace. */
export async function revokeMyKey(token: string, prefix: string): Promise<void> {
  await request<void>(`/v1/me/apikeys/${segment(prefix)}`, { token, method: 'DELETE' })
}
