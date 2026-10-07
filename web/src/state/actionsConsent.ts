// The person's consent to actions on their messages.
//
// Changing a mailbox is a new use of it, so it is asked for on its own,
// apart from sync: until the person allows actions, the server refuses every
// one they ask for from a console, and every one a key they made before keys
// belonged to workspaces asks for. A workspace's key acts under the key terms
// its creator agreed to, where it is given Act, never under this consent.
// "Not now" is remembered in memory only, like sync's.
//
// What an edition does with actions (one that shows messages may make them
// from the console) is its own; it hears of every answer here through
// onActionsConsent, to let go of what no longer stands.

import { reactive, watch } from 'vue'
import * as api from '../api/actionsConsent'
import type { ActionsConsent } from '../api/types'
import { edition } from '../edition'
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
  /** The revision the server asks about now; the page's own text is the edition's actions.version. */
  currentVersion: string
  /** "Not now" where the question was asked, for this page's life only. */
  dismissed: boolean
  busy: '' | 'grant' | 'withdraw'
  /** What the last grant or withdrawal failed with, shown where it was asked. */
  problem: Failure | null
}

const freshConsent = (): ConsentState => ({
  loaded: false, loading: false, failure: null, consented: false, version: '', consentedAt: 0, currentVersion: '',
  dismissed: false, busy: '', problem: null,
})

export const actionsConsent = reactive<ConsentState>(freshConsent())

/** Where the person stands on a permission: the words for a refusal depend on it. */
export type ConsentStanding = 'allowed' | 'off' | 'outdated'

/**
 * What happened to the consent: an answer read or received (adopted), or
 * the person agreeing (granted) or taking it back (withdrawn), both after
 * the answer was adopted.
 */
export type ActionsConsentChange = 'adopted' | 'granted' | 'withdrawn'

const listeners = new Set<(change: ActionsConsentChange) => void>()

/** Hears every change of the consent, after the store has it. Returns what stops hearing. */
export function onActionsConsent(listener: (change: ActionsConsentChange) => void): () => void {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

function tell(change: ActionsConsentChange): void {
  for (const listener of listeners) listener(change)
}

let generation = 0
watch(identity, () => {
  generation++
  Object.assign(actionsConsent, freshConsent())
}, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

function adopt(answer: ActionsConsent): void {
  actionsConsent.loaded = true
  actionsConsent.consented = answer.consented
  actionsConsent.version = answer.version ?? ''
  actionsConsent.consentedAt = answer.consented_at ?? 0
  actionsConsent.currentVersion = answer.current_version
  tell('adopted')
}

/**
 * Whether actions may be made: the person allowed them, to the revision the
 * server asks about now. The server refuses an action under a consent to an
 * older text, so nothing offers one either; the person is asked to agree to
 * the new one.
 */
export function actionsAllowed(): boolean {
  return actionsConsent.loaded && actionsConsent.consented && actionsConsent.version === actionsConsent.currentVersion
}

/** Allowed before, to an older text than the server asks about now: actions are paused until the person agrees again. */
export function actionsOutdated(): boolean {
  return actionsConsent.loaded && actionsConsent.consented && actionsConsent.version !== actionsConsent.currentVersion
}

/** Where the person stands, for the words a refusal gets. */
export function actionsStanding(): ConsentStanding {
  return actionsAllowed() ? 'allowed' : actionsOutdated() ? 'outdated' : 'off'
}

/** Whether to ask: the person has not allowed actions, or allowed an older text than the server's. */
export function needsActionsConsent(): boolean {
  return actionsConsent.loaded && (!actionsConsent.consented || actionsConsent.version !== actionsConsent.currentVersion)
}

/**
 * Whether the server asks about another revision than the text this page
 * carries: the text changed while the page was open, and agreeing from here
 * would be to a text the person never saw.
 */
export function actionsTextOutdated(): boolean {
  return actionsConsent.loaded && actionsConsent.currentVersion !== edition().actions.version
}

export async function loadActionsConsent(): Promise<void> {
  const ok = current()
  actionsConsent.loading = true
  actionsConsent.failure = null
  try {
    const answer = await authorized(token => api.getActionsConsent(token))
    if (ok()) adopt(answer)
  } catch (error) {
    if (ok()) actionsConsent.failure = failure('actions-consent', error)
  } finally {
    if (ok()) actionsConsent.loading = false
  }
}

/** "Not now": the question goes away until the page is loaded again. */
export function dismissActionsConsent(): void {
  actionsConsent.dismissed = true
  actionsConsent.problem = null
}

/**
 * allowActions agrees to the text the page showed (the edition's
 * actions.version), never to the revision the server names; when the server
 * already names another, nothing is posted and the page offers a reload.
 */
export async function allowActions(): Promise<boolean> {
  if (actionsConsent.busy || !actionsConsent.loaded || actionsTextOutdated()) return false
  const ok = current()
  actionsConsent.busy = 'grant'
  actionsConsent.problem = null
  try {
    const answer = await authorized(token => api.grantActionsConsent(token, edition().actions.version))
    if (!ok()) return false
    adopt(answer)
    actionsConsent.dismissed = false
    tell('granted')
    return true
  } catch (error) {
    if (ok()) actionsConsent.problem = failure('grant-actions', error)
    return false
  } finally {
    if (ok()) actionsConsent.busy = ''
  }
}

/** Turns actions off. Nothing was stored by them, so nothing is deleted; they stop. */
export async function stopActions(): Promise<boolean> {
  if (actionsConsent.busy) return false
  const ok = current()
  actionsConsent.busy = 'withdraw'
  actionsConsent.problem = null
  try {
    const answer = await authorized(token => api.withdrawActionsConsent(token))
    if (!ok()) return false
    adopt(answer)
    // Asking again straight after the person said no would be pushy; the account section still offers it.
    actionsConsent.dismissed = true
    tell('withdrawn')
    return true
  } catch (error) {
    if (ok()) actionsConsent.problem = failure('withdraw-actions', error)
    return false
  } finally {
    if (ok()) actionsConsent.busy = ''
  }
}
