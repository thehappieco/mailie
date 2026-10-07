// API keys (docs/workspaces.md, "API keys"): the keys of the workspace shown,
// for its owners and admins (in a personal workspace, its person), and the
// keys the signed-in person created, in every workspace, which they may
// revoke from their account.
//
// Creating a key is its creator's agreement to the key terms, for whatever
// tool they give it to: the dialog shows the text first (the edition's
// keyTerms), and the request names the revision of that text, which the
// server records on the key. The answer carries the key's secret, the only
// time it exists outside the server's hash. This store hands it to the
// caller, the creation dialog, which shows it once and lets go of it when it
// closes; the lists keep only what the server lists. Nothing about a key is
// written to browser storage, the address bar or history.
//
// What a key holds stands on its own: whoever gave it, and its creator, may
// lose their own access, and the key keeps its mailboxes until an owner or an
// admin takes them out or revokes it. So every change here is read again
// where else it shows: the access directory lists each mailbox's keys, and
// the person's own list each key they created.
//
// Everything resets when the person changes, as in the other stores, and the
// workspace's keys when the console shows another workspace: every async step
// captures a generation and drops its result if it moved on.

import { reactive, watch } from 'vue'
import * as api from '../api/apikeys'
import type { CreateKeyRequest, CreatedKey, KeyFlags, KeyLifetime, KeyMailboxRequest, KeyScope, KeySend, WorkspaceKey } from '../api/types'
import { edition } from '../edition'
import { administersKeys } from '../ui/access'
import { MAX_LIVE_KEYS, holdsAnyKeyFlag, keyStanding, sameKeyFlags } from '../ui/apikeys'
import { accounts } from './accounts'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'
import { loadDirectory, team } from './team'
import { loadWorkspaces, workspaces } from './workspaces'

interface KeysState {
  /** The workspace these were read for. */
  workspace: string
  list: WorkspaceKey[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
  /** The prefix of the key being revoked; one at a time. */
  revoking: string
}

const fresh = (): KeysState => ({ workspace: '', list: [], loaded: false, loading: false, failure: null, revoking: '' })

/** The keys of the workspace shown. */
export const apiKeys = reactive<KeysState>(fresh())

interface MyKeysState {
  list: WorkspaceKey[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
  revoking: string
}

const freshMine = (): MyKeysState => ({ list: [], loaded: false, loading: false, failure: null, revoking: '' })

/** The keys the person created, in every workspace. */
export const myKeys = reactive<MyKeysState>(freshMine())

let generation = 0
let mineGeneration = 0
function reset(): void {
  generation++
  Object.assign(apiKeys, fresh())
}
watch(identity, () => {
  reset()
  mineGeneration++
  Object.assign(myKeys, freshMine())
}, { flush: 'sync' })
watch(() => workspaces.currentID, id => { if (apiKeys.workspace !== id) reset() }, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

/** The workspace shown, when the person may see its keys there: its owners and admins, or the person of a personal one. */
function keysWorkspace(): string {
  const shown = workspaces.list.find(item => item.id === workspaces.currentID)
  return administersKeys(shown) ? shown!.id : ''
}

// A mailbox gone from the list of the same workspace was removed (or its
// workspace's role lost): the keys holding it lost it with it, and a key
// carried over from before that held nothing else was revoked.
watch(() => ({ workspace: accounts.workspace, loaded: accounts.loaded, list: accounts.list }), (now, before) => {
  if (!apiKeys.loaded || !now.loaded || !before.loaded || now.workspace !== before.workspace) return
  const gone = before.list.filter(item => !now.list.some(account => account.id === item.id))
  if (gone.some(item => apiKeys.list.some(key => key.mailboxes.some(held => held.account_id === item.id)))) void loadKeys()
})

/**
 * Keys of the workspace that still work and count against its limit, as the
 * server counts them: a key carried over from before belongs to none, and the
 * persons' keys the upgrade moved in (origin) are not counted, as a team may
 * hold more of them than the limit.
 */
export function liveKeys(now = Date.now()): number {
  return apiKeys.list.filter(key => !key.carried_over && !key.origin && keyStanding(key, now) === 'live').length
}

/** Whether the workspace holds as many working keys as it may: a new one waits for one to be revoked or expire. */
export function atKeyLimit(now = Date.now()): boolean {
  return apiKeys.loaded && liveKeys(now) >= MAX_LIVE_KEYS
}

/** Replaces the key with the server's latest word on it, or adds it first. */
function upsert(key: WorkspaceKey): void {
  const index = apiKeys.list.findIndex(item => item.prefix === key.prefix)
  if (index >= 0) apiKeys.list.splice(index, 1, key)
  else apiKeys.list.unshift(key)
}

/**
 * A refusal for want of a role means the person's role changed elsewhere:
 * their workspaces are read again, and what the console offers follows the
 * role they have now.
 */
function refused(op: Failure['op'], error: unknown, ok: () => boolean): Failure {
  const found = failure(op, error)
  if (ok() && found.code === 'not_authorized') void loadWorkspaces()
  return found
}

/** What else shows keys, read again after a change: the access directory, and the person's own list. */
function followKeyChange(): void {
  if (team.directory.loaded || team.directory.loading) void loadDirectory()
  if (myKeys.loaded || myKeys.loading) void loadMyKeys()
}

export async function loadKeys(): Promise<void> {
  const id = keysWorkspace()
  if (!id) return
  const ok = current()
  apiKeys.workspace = id
  apiKeys.loading = true
  apiKeys.failure = null
  try {
    const list = await authorized(token => api.listWorkspaceKeys(token, id))
    if (!ok()) return
    apiKeys.list = list
    apiKeys.loaded = true
  } catch (error) {
    if (ok()) apiKeys.failure = refused('load-keys', error, ok)
  } finally {
    if (ok()) apiKeys.loading = false
  }
}

/** What the person chose in the dialog: what the key is given on each mailbox, none for a key that reaches nothing yet. */
export interface NewKey { name: string; scope: KeyScope; mailboxes: KeyMailboxRequest[]; lifetime: KeyLifetime }

/** A key made, with its secret, for the caller alone; or why not. */
export type Creation = { created: CreatedKey } | { failure: Failure }

/**
 * createKey asks for a key of the workspace shown, under the text the dialog
 * showed. A 409 is either the limit (a key made meanwhile in another tab) or
 * a newer text on the server; the list, read again, tells which. A failure
 * with no answer may have made the key all the same, and an answer this
 * console cannot read (a proxy's page, a field it does not know) most likely
 * did, so for both the list is read again to show it.
 */
export async function createKey(input: NewKey): Promise<Creation> {
  const id = keysWorkspace()
  if (!id) return { failure: { op: 'create-key', code: 'not_found' } }
  if (atKeyLimit()) return { failure: { op: 'create-key', code: 'key_limit' } }
  const ok = current()
  const body: CreateKeyRequest = { name: input.name, scope: input.scope, ttl_days: input.lifetime, terms_version: edition().keyTerms.version }
  const mailboxes = input.mailboxes.filter(holdsAnyKeyFlag).map(item => ({ account_id: item.account_id, read: item.read, act: item.act, send: item.send }))
  if (mailboxes.length) body.mailboxes = mailboxes
  try {
    const created = await authorized(token => api.createWorkspaceKey(token, id, body))
    // Another person or workspace now: the key is theirs to revoke, and its secret goes nowhere.
    if (!ok()) return { failure: { op: 'create-key', code: 'aborted' } }
    upsert(api.listed(created))
    followKeyChange()
    return { created }
  } catch (error) {
    const found = refused('create-key', error, ok)
    if (!ok()) return { failure: found }
    if (found.code === 'conflict') {
      await loadKeys()
      if (ok() && apiKeys.loaded && !apiKeys.failure) return { failure: { op: 'create-key', code: atKeyLimit() ? 'key_limit' : 'terms_changed' } }
    } else if (found.code === 'unavailable' || found.code === 'internal' || found.code === 'invalid_response') {
      void loadKeys()
    }
    return { failure: found }
  }
}

/**
 * revokeKey revokes a key of the workspace shown (a key carried over from
 * before loses this workspace's mailboxes instead). Once the server confirms
 * it, the key is drawn revoked at once and the list is read again; a key that
 * is gone is read away too.
 */
export async function revokeKey(prefix: string): Promise<Failure | null> {
  const id = keysWorkspace()
  if (!id || apiKeys.revoking) return null
  const ok = current()
  apiKeys.revoking = prefix
  try {
    await authorized(token => api.revokeWorkspaceKey(token, id, prefix))
    if (!ok()) return null
    const listed = apiKeys.list.find(item => item.prefix === prefix)
    if (listed && !listed.revoked_at && !listed.carried_over) upsert({ ...listed, revoked_at: Math.floor(Date.now() / 1000), live: false })
    void loadKeys()
    followKeyChange()
    return null
  } catch (error) {
    const found = refused('revoke-key', error, ok)
    if (ok() && found.code === 'not_found') void loadKeys()
    return found
  } finally {
    if (ok()) apiKeys.revoking = ''
  }
}

/**
 * Sets what a key of the workspace shown holds on one of its mailboxes, from
 * what it holds now to what the caller ticked: exactly those flags, or, when
 * none is left, the mailbox taken out of the key. The key's row takes the
 * answer, and the list is read again on a refusal that says it changed.
 */
export async function setKeyMailbox(prefix: string, accountID: string, before: KeyFlags, after: KeyFlags): Promise<Failure | null> {
  const id = keysWorkspace()
  if (!id || sameKeyFlags(before, after)) return null
  const ok = current()
  try {
    if (holdsAnyKeyFlag(after)) {
      const held = await authorized(token => api.setKeyAccess(token, id, prefix, accountID, after))
      if (!ok()) return null
      const key = apiKeys.list.find(item => item.prefix === prefix)
      if (key) upsert({ ...key, mailboxes: [...key.mailboxes.filter(item => item.account_id !== accountID), held] })
    } else {
      await authorized(token => api.dropKeyAccess(token, id, prefix, accountID))
      if (!ok()) return null
      const key = apiKeys.list.find(item => item.prefix === prefix)
      if (key) upsert({ ...key, mailboxes: key.mailboxes.filter(item => item.account_id !== accountID) })
      // A key carried over from before is revoked with its last mailbox.
      if (key?.carried_over) void loadKeys()
    }
  } catch (error) {
    const found = refused('change-key-access', error, ok)
    // Changed elsewhere meanwhile, revoked, or gone: what is shown is read again.
    if (ok() && (found.code === 'conflict' || found.code === 'not_found' || found.code === 'bad_request')) void loadKeys()
    return found
  }
  followKeyChange()
  return null
}

/**
 * Takes a mailbox of the team shown out of a key, from the access directory,
 * whose owners and admins see each mailbox's keys: the directory is read
 * again, and the workspace's keys when they were read.
 */
export async function dropKeyMailbox(prefix: string, accountID: string): Promise<Failure | null> {
  const id = keysWorkspace()
  if (!id) return null
  const ok = current()
  try {
    await authorized(token => api.dropKeyAccess(token, id, prefix, accountID))
  } catch (error) {
    const found = refused('change-key-access', error, ok)
    if (ok() && found.code === 'not_found') void loadDirectory()
    return found
  }
  if (!ok()) return null
  if (apiKeys.loaded || apiKeys.loading) void loadKeys()
  followKeyChange()
  return null
}

/** A key's sends, or why they could not be read. */
export type Sends = { list: KeySend[] } | { failure: Failure }

/** Reads a key's sends from the workspace shown's mailboxes, newest first. */
export async function loadKeySends(prefix: string): Promise<Sends> {
  const id = keysWorkspace()
  if (!id) return { failure: { op: 'load-key-sends', code: 'not_found' } }
  const ok = current()
  try {
    const list = await authorized(token => api.listKeySends(token, id, prefix))
    if (!ok()) return { failure: { op: 'load-key-sends', code: 'aborted' } }
    return { list }
  } catch (error) {
    return { failure: refused('load-key-sends', error, ok) }
  }
}

/** Reads the keys the person created, in every workspace. */
export async function loadMyKeys(): Promise<void> {
  const at = mineGeneration
  myKeys.loading = true
  myKeys.failure = null
  try {
    const list = await authorized(token => api.listMyKeys(token))
    if (at !== mineGeneration) return
    myKeys.list = list
    myKeys.loaded = true
  } catch (error) {
    if (at === mineGeneration) myKeys.failure = failure('load-my-keys', error)
  } finally {
    if (at === mineGeneration) myKeys.loading = false
  }
}

/**
 * Revokes a key the person created, wherever it is (a key carried over from
 * before too, whole). Drawn revoked once the server confirms it, and read
 * again here and wherever else it shows.
 */
export async function revokeMyKey(prefix: string): Promise<Failure | null> {
  if (myKeys.revoking) return null
  const at = mineGeneration
  myKeys.revoking = prefix
  try {
    await authorized(token => api.revokeMyKey(token, prefix))
    if (at !== mineGeneration) return null
    const index = myKeys.list.findIndex(item => item.prefix === prefix)
    const listed = myKeys.list[index]
    if (listed && !listed.revoked_at) myKeys.list.splice(index, 1, { ...listed, revoked_at: Math.floor(Date.now() / 1000), live: false })
    void loadMyKeys()
    if (apiKeys.loaded || apiKeys.loading) void loadKeys()
    if (team.directory.loaded || team.directory.loading) void loadDirectory()
    return null
  } catch (error) {
    const found = failure('revoke-key', error)
    if (at === mineGeneration && found.code === 'not_found') void loadMyKeys()
    return found
  } finally {
    if (at === mineGeneration) myKeys.revoking = ''
  }
}
