// The API keys a person creates for their own tools, and revoking them.
//
// Creating a key is the person's authorization for whatever tool they give
// it to: the dialog shows the text first (the edition's keyTerms), and the
// request names the revision of that text, which the server records on the
// key. The answer carries the key's secret, the only time it
// exists outside the server's hash. This store hands it to the caller, the
// creation dialog, which shows it once and lets go of it when it closes; the
// list keeps only what GET /v1/me/apikeys would say. Nothing about a key is
// written to browser storage, the address bar or history.
//
// Everything resets when the person changes, as in the other stores: every
// async step captures a generation and drops its result if it moved on.
//
// A mailbox removed leaves every key's restriction, and revokes a key that
// was for it alone (0005_key_terms.sql). The list is read again when one a
// key names goes, so that key is not drawn as still working.

import { reactive, watch } from 'vue'
import * as api from '../api/apikeys'
import type { CreateKeyRequest, CreatedKey, KeyLifetime, KeyScope, PersonalKey } from '../api/types'
import { edition } from '../edition'
import { MAX_LIVE_KEYS, keyStanding } from '../ui/apikeys'
import { accounts } from './accounts'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

interface KeysState {
  list: PersonalKey[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
  /** The prefix of the key being revoked; one at a time. */
  revoking: string
}

const fresh = (): KeysState => ({ list: [], loaded: false, loading: false, failure: null, revoking: '' })

export const apiKeys = reactive<KeysState>(fresh())

let generation = 0
watch(identity, () => {
  generation++
  Object.assign(apiKeys, fresh())
}, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

watch(() => accounts.list, (now, before) => {
  if (!apiKeys.loaded) return
  const gone = before.filter(item => !now.some(account => account.id === item.id))
  if (gone.some(item => apiKeys.list.some(key => key.account_ids?.includes(item.id)))) void loadKeys()
})

/** Keys that still work: neither revoked nor past their expiry. The server counts these against the limit. */
export function liveKeys(now = Date.now()): number {
  return apiKeys.list.filter(key => keyStanding(key, now) === 'live').length
}

/** Whether the person holds as many working keys as they may: a new one waits for one to be revoked or expire. */
export function atKeyLimit(now = Date.now()): boolean {
  return apiKeys.loaded && liveKeys(now) >= MAX_LIVE_KEYS
}

/** Replaces the key with the server's latest word on it, or adds it first. */
function upsert(key: PersonalKey): void {
  const index = apiKeys.list.findIndex(item => item.prefix === key.prefix)
  if (index >= 0) apiKeys.list.splice(index, 1, key)
  else apiKeys.list.unshift(key)
}

export async function loadKeys(): Promise<void> {
  const ok = current()
  apiKeys.loading = true
  apiKeys.failure = null
  try {
    const list = await authorized(token => api.listKeys(token))
    if (!ok()) return
    apiKeys.list = list
    apiKeys.loaded = true
  } catch (error) {
    if (ok()) apiKeys.failure = failure('load-keys', error)
  } finally {
    if (ok()) apiKeys.loading = false
  }
}

/** What the person chose in the dialog. accountIDs null: every mailbox of theirs, the ones connected later too. */
export interface NewKey { name: string; scope: KeyScope; accountIDs: string[] | null; lifetime: KeyLifetime }

/** A key made, with its secret, for the caller alone; or why not. */
export type Creation = { created: CreatedKey } | { failure: Failure }

/**
 * createKey asks for a key under the text the dialog showed. A 409 is either
 * the limit (a key made meanwhile in another tab) or a newer text on the
 * server; the list, read again, tells which. A failure with no answer may
 * have made the key all the same, and an answer this console cannot read
 * (a proxy's page, a field it does not know) most likely did, so for both
 * the list is read again to show it.
 */
export async function createKey(input: NewKey): Promise<Creation> {
  if (atKeyLimit()) return { failure: { op: 'create-key', code: 'key_limit' } }
  const ok = current()
  const body: CreateKeyRequest = { name: input.name, scope: input.scope, ttl_days: input.lifetime, terms_version: edition().keyTerms.version }
  if (input.accountIDs) body.account_ids = [...input.accountIDs]
  try {
    const created = await authorized(token => api.createKey(token, body))
    // Another person now: the key is theirs to revoke, and its secret goes nowhere.
    if (!ok()) return { failure: { op: 'create-key', code: 'aborted' } }
    upsert(api.listed(created))
    return { created }
  } catch (error) {
    const found = failure('create-key', error)
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
 * revokeKey revokes one of the person's keys. Once the server confirms it,
 * the key is drawn revoked at once, and the list is read again unless the
 * answer already said how it stands. A key that is gone is read away too.
 */
export async function revokeKey(prefix: string): Promise<Failure | null> {
  if (apiKeys.revoking) return null
  const ok = current()
  apiKeys.revoking = prefix
  try {
    const answer = await authorized(token => api.revokeKey(token, prefix))
    if (!ok()) return null
    if (answer) upsert(answer)
    else {
      const listed = apiKeys.list.find(item => item.prefix === prefix)
      if (listed && !listed.revoked_at) upsert({ ...listed, revoked_at: Math.floor(Date.now() / 1000) })
      void loadKeys()
    }
    return null
  } catch (error) {
    const found = failure('revoke-key', error)
    if (ok() && found.code === 'not_found') void loadKeys()
    return found
  } finally {
    if (ok()) apiKeys.revoking = ''
  }
}
