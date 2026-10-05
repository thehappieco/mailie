// Consent to sync, and asking an account for a pass.
//
// Nothing from a person's mail is kept until they agree in the console, and
// turning sync off deletes what was kept: the edition's sync text says so.
// The daemon enforces both; this store asks the person, shows their answer,
// and after either change reloads the accounts so every card says what is now
// true. "Not now" is remembered in memory only: the next visit asks again.

import { reactive, watch } from 'vue'
import * as api from '../api/sync'
import type { SyncConsent } from '../api/types'
import type { SyncStanding } from '../ui/access'
import { announce } from '../ui/announce'
import { edition } from '../edition'
import { t } from '../ui/i18n'
import { forgetIndexedFolders, forgetLiveFolders, loadAccounts, mergeSync, notifySync } from './accounts'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

interface ConsentState {
  loaded: boolean
  loading: boolean
  failure: Failure | null
  consented: boolean
  /** The text revision the person agreed to; empty until they do. */
  version: string
  consentedAt: number
  /** The revision the server asks about now; the page's own text is the edition's sync.version. */
  currentVersion: string
  /** "Not now" on the accounts page, for this page's life only. */
  dismissed: boolean
  busy: '' | 'grant' | 'withdraw'
  /** What the last grant or withdrawal failed with, shown where it was asked. */
  problem: Failure | null
  /** The consent card should take focus when it can (a sheet's "Turn on sync…" sent the person there). */
  focusPending: boolean
}

interface SyncRequest { busy: boolean; failure: Failure | null; requested: boolean }

const freshConsent = (): ConsentState => ({
  loaded: false, loading: false, failure: null, consented: false, version: '', consentedAt: 0, currentVersion: '',
  dismissed: false, busy: '', problem: null, focusPending: false,
})

export const consent = reactive<ConsentState>(freshConsent())
/** Per account: a "Sync now" in flight, or how the last one ended. */
export const syncRequests = reactive<Record<string, SyncRequest>>({})

let generation = 0
watch(identity, () => {
  generation++
  Object.assign(consent, freshConsent())
  for (const id of Object.keys(syncRequests)) delete syncRequests[id]
}, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

function adopt(answer: SyncConsent): void {
  // Turned off elsewhere (another tab, another device): the index is gone,
  // and so is every folder list read from it.
  if (consent.loaded && consent.consented && !answer.consented) forgetIndexedFolders()
  consent.loaded = true
  consent.consented = answer.consented
  consent.version = answer.version ?? ''
  consent.consentedAt = answer.consented_at ?? 0
  consent.currentVersion = answer.current_version
}

/**
 * Whether to ask: the person has not agreed, or agreed to an older text than
 * the one the server now describes sync with.
 */
export function needsConsent(): boolean {
  return consent.loaded && (!consent.consented || consent.version !== consent.currentVersion)
}

/**
 * Whether the server now asks about another revision than the text this
 * page carries — the text changed while the page was open. Agreeing from
 * here would record consent to a text the person never saw, so the console
 * offers a reload instead of the agree button.
 */
export function consentTextOutdated(): boolean {
  return consent.loaded && consent.currentVersion !== edition().sync.version
}

/** Whether sync is on for the person: they agreed, to whichever revision. */
export function syncOn(): boolean {
  return consent.loaded && consent.consented
}

/**
 * Where the person stands on sync, for what waits on it: taking a link over,
 * connecting a mailbox to a team (ui/access.ts). Not read yet, the server
 * decides.
 */
export function syncStanding(): SyncStanding {
  if (!consent.loaded) return { consented: true, current: true }
  return { consented: consent.consented, current: consent.consented && consent.version === consent.currentVersion }
}

export async function loadConsent(): Promise<void> {
  const ok = current()
  consent.loading = true
  consent.failure = null
  try {
    const answer = await authorized(token => api.getSyncConsent(token))
    if (ok()) adopt(answer)
  } catch (error) {
    if (ok()) consent.failure = failure('sync-consent', error)
  } finally {
    if (ok()) consent.loading = false
  }
}

/** "Not now": the card goes away until the page is loaded again. */
export function dismissConsent(): void {
  consent.dismissed = true
  consent.problem = null
}

/** Shows the consent card again and asks it to take focus. */
export function reviewConsent(): void {
  consent.dismissed = false
  consent.focusPending = true
}

/**
 * grantConsent agrees to the text the console showed: it posts that text's
 * revision, the edition's sync.version, never the one the server names. When
 * the server already names another, it posts nothing (the page offers a
 * reload); when the server moved on without this page reading it yet, the
 * server's 400 says the text changed. On success every mailbox of the person may start
 * syncing, so the accounts are read again, and folder lists read live give
 * way to the index's.
 */
export async function grantConsent(): Promise<boolean> {
  if (consent.busy || !consent.loaded || consentTextOutdated()) return false
  const ok = current()
  consent.busy = 'grant'
  consent.problem = null
  try {
    const answer = await authorized(token => api.grantSyncConsent(token, edition().sync.version))
    if (!ok()) return false
    adopt(answer)
    consent.dismissed = false
    forgetLiveFolders()
    notifySync('sync-on')
    void loadAccounts()
    return true
  } catch (error) {
    if (ok()) consent.problem = failure('grant-sync', error)
    return false
  } finally {
    if (ok()) consent.busy = ''
  }
}

/**
 * withdrawConsent turns sync off: the server stops every worker of the
 * person's mailboxes and deletes what it indexed for them before answering.
 * Folder lists read from that index go with it.
 */
export async function withdrawConsent(): Promise<boolean> {
  if (consent.busy) return false
  const ok = current()
  consent.busy = 'withdraw'
  consent.problem = null
  try {
    const answer = await authorized(token => api.withdrawSyncConsent(token))
    if (!ok()) return false
    adopt(answer)
    // Asking again straight after the person said no would be pushy; the
    // sheet and the account section still offer to turn it back on.
    consent.dismissed = true
    forgetIndexedFolders()
    notifySync('sync-off')
    void loadAccounts()
    return true
  } catch (error) {
    if (ok()) consent.problem = failure('withdraw-sync', error)
    return false
  } finally {
    if (ok()) consent.busy = ''
  }
}

/** Asks the account for a pass now. The reply is the status as it stands; the pass itself shows up through events. */
export async function syncNow(id: string): Promise<void> {
  if (syncRequests[id]?.busy) return
  const ok = current()
  syncRequests[id] = { busy: true, failure: null, requested: false }
  try {
    const status = await authorized(token => api.triggerSync(token, id))
    if (!ok()) return
    mergeSync(id, status)
    syncRequests[id] = { busy: false, failure: null, requested: true }
    announce(t('Sync requested.'))
  } catch (error) {
    if (ok()) syncRequests[id] = { busy: false, failure: failure('sync-now', error), requested: false }
  }
}
