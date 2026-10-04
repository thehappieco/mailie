// The email accounts the signed-in person owns, and connecting new ones.
//
// Everything here resets when the person changes: a sign-out, a different
// sign-in. Every async step captures a generation first and drops its result
// if the generation moved on, so a slow reply for one person can never be
// drawn in front of the next.
//
// Connecting is a conversation with a provider, so it has phases rather than a
// result: starting → (redirecting | waiting) → done | failed. Waiting means
// asking for the account until it leaves pending_auth or the flow's window
// closes: every 3 s, or, while the event stream is open (state/live.ts), as
// soon as an account.state event names it, with a slow poll kept only as a
// safety net.

import { reactive, watch } from 'vue'
import * as api from '../api/accounts'
import { ApiError } from '../api/http'
import { listProviders } from '../api/providers'
import type { Account, AccountState, AccountSync, AddAccountRequest, AuthFlow, Folder, Provider, ProviderID } from '../api/types'
import { announce } from '../ui/announce'
import { noticeText } from '../ui/notices'
import type { ReasonFacts } from '../ui/reasons'
import { server } from './connection'
import { failure, type Failure, type Operation } from './failure'
import { FLOW_LIFETIME_MS, rememberOAuthStart, takeOAuthReturn, takeOAuthStart } from './oauthReturn'
import { authorized, identity } from './session'

export const POLL_INTERVAL_MS = 3_000
/** While the event stream is open, account.state events end the wait; this poll only covers one that went missing. */
export const STREAM_POLL_INTERVAL_MS = 15_000

export interface FolderView { loading: boolean; loaded: boolean; list: Folder[]; failure: Failure | null }

export type Notice =
  /** syncing: the person has turned sync on, so the new mailbox starts syncing at once. */
  | { kind: 'connected'; email: string; syncing: boolean }
  | { kind: 'removed'; email: string }
  | { kind: 'failed'; email: string; failure: Failure }
  | { kind: 'sync-on' }
  | { kind: 'sync-off' }

interface AccountsState {
  list: Account[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
  providers: Provider[]
  providersLoaded: boolean
  providersFailure: Failure | null
  /** The one message at the top of the accounts section: a connection finished, failed, or an account went. */
  notice: Notice | null
  /** The account whose sheet is open, by id; the sheet reads the live row from the list. */
  detailID: string
  removing: boolean
  removeFailure: Failure | null
  folders: Record<string, FolderView>
}

export type ConnectPhase = 'idle' | 'starting' | 'redirecting' | 'waiting' | 'done' | 'failed'

export interface ConnectState {
  phase: ConnectPhase
  provider: ProviderID | ''
  email: string
  accountID: string
  /** This attempt created the account, so abandoning it removes the account again. */
  created: boolean
  flow: AuthFlow['flow'] | ''
  /** The provider page, for a flow the person opens themselves (loopback). Always https. */
  authURL: string
  userCode: string
  verificationURI: string
  /** Unix seconds when the provider flow stops being accepted. */
  expiresAt: number
  failure: Failure | null
}

const freshAccounts = (): AccountsState => ({
  list: [], loaded: false, loading: false, failure: null,
  providers: [], providersLoaded: false, providersFailure: null,
  notice: null, detailID: '', removing: false, removeFailure: null, folders: {},
})
const freshConnect = (): ConnectState => ({
  phase: 'idle', provider: '', email: '', accountID: '', created: false, flow: '',
  authURL: '', userCode: '', verificationURI: '', expiresAt: 0, failure: null,
})

export const accounts = reactive<AccountsState>(freshAccounts())
export const connect = reactive<ConnectState>(freshConnect())

/** How the page leaves for the provider. A seam, so tests can watch it instead of navigating. */
export const navigation = { assign: (url: string) => { location.assign(url) } }

let accountsGeneration = 0
let connectGeneration = 0
let wake: (() => void) | null = null
/** Ends the wait's current pause early, to ask for the account now. */
let poke: (() => void) | null = null
/** A nudge that came while the wait was asking, not pausing: the next pause is skipped. */
let nudged = false

watch(identity, () => {
  accountsGeneration++
  stopConnect()
  Object.assign(accounts, freshAccounts())
  Object.assign(connect, freshConnect())
}, { flush: 'sync' })

function currentAccounts(): () => boolean {
  const generation = accountsGeneration
  return () => generation === accountsGeneration
}

function stopConnect(): void {
  connectGeneration++
  nudged = false
  wake?.()
}

/** Replaces the row with the server's latest copy, or adds it. */
export function upsert(account: Account): void {
  const index = accounts.list.findIndex(item => item.id === account.id)
  if (index >= 0) accounts.list.splice(index, 1, account)
  else accounts.list.push(account)
}

function forget(id: string): void {
  accounts.list = accounts.list.filter(item => item.id !== id)
  delete accounts.folders[id]
  if (accounts.detailID === id) accounts.detailID = ''
}

export async function loadAccounts(): Promise<void> {
  const current = currentAccounts()
  accounts.loading = true
  accounts.failure = null
  try {
    const list = await authorized(token => api.listAccounts(token))
    if (!current()) return
    accounts.list = list
    accounts.loaded = true
  } catch (error) {
    if (current()) accounts.failure = failure('load-accounts', error)
  } finally {
    if (current()) accounts.loading = false
  }
}

export async function loadProviders(): Promise<void> {
  const current = currentAccounts()
  accounts.providersFailure = null
  try {
    const providers = await authorized(token => listProviders(token))
    if (!current()) return
    accounts.providers = providers
    accounts.providersLoaded = true
  } catch (error) {
    if (current()) accounts.providersFailure = failure('load-providers', error)
  }
}

/** Asks for one account again, quietly: the card simply updates. Answers the new row, if there is one. */
export async function refreshAccount(id: string): Promise<Account | undefined> {
  const current = currentAccounts()
  try {
    const account = await authorized(token => api.getAccount(token, id))
    if (!current()) return undefined
    upsert(account)
    return account
  } catch (error) {
    if (current() && error instanceof ApiError && error.code === 'not_found') forget(id)
    return undefined
  }
}

/** Replaces an account's sync block with a newer one the server answered. */
export function mergeSync(id: string, sync: AccountSync): void {
  const row = accounts.list.find(item => item.id === id)
  if (row) upsert({ ...row, sync })
}

/**
 * The index of every mailbox was deleted (sync turned off): folder lists read
 * from it are gone too. A sheet showing one goes back to "Show folders".
 */
export function forgetIndexedFolders(): void {
  for (const [id, view] of Object.entries(accounts.folders)) {
    if (view.list.some(folder => folder.sync_state)) delete accounts.folders[id]
  }
}

/**
 * Sync was turned on: folder lists read live from the mail server give way
 * to the index's, which an edition that lists messages needs (only an indexed
 * folder has an id to list by). They are read again when next wanted; a list still being read live is
 * left to land, and is replaced as the index lists the folders
 * (reloadIndexedFolders).
 */
export function forgetLiveFolders(): void {
  for (const [id, view] of Object.entries(accounts.folders)) {
    if (!view.loading && !view.list.some(folder => folder.sync_state)) delete accounts.folders[id]
  }
}

/** A notice the sync store raises: consent given or withdrawn. */
export function notifySync(kind: 'sync-on' | 'sync-off'): void {
  notify({ kind })
}

export function openDetail(id: string): void {
  accounts.detailID = id
  accounts.removeFailure = null
}

export function closeDetail(): void {
  if (accounts.removing) return
  accounts.detailID = ''
  accounts.removeFailure = null
}

export function dismissNotice(): void {
  accounts.notice = null
}

/**
 * Shows a notice and, when it is good news, says it too. A failure is drawn as
 * role="alert", which screen readers announce on their own.
 */
function notify(notice: Notice): void {
  accounts.notice = notice
  if (notice.kind !== 'failed') announce(noticeText(notice))
}

export async function removeAccount(id: string): Promise<boolean> {
  const current = currentAccounts()
  const email = accounts.list.find(item => item.id === id)?.email ?? ''
  accounts.removing = true
  accounts.removeFailure = null
  try {
    await authorized(token => api.removeAccount(token, id))
  } catch (error) {
    // Already gone is what was asked for.
    if (!(error instanceof ApiError && error.code === 'not_found')) {
      if (current()) { accounts.removeFailure = failure('remove-account', error); accounts.removing = false }
      return false
    }
  }
  if (!current()) return false
  accounts.removing = false
  forget(id)
  notify({ kind: 'removed', email })
  return true
}

/**
 * Asks for a folder list read from the index again, without the loading
 * state: counts that moved just change. Only a list that came from the index
 * is refreshed this way, or a live one of a mailbox that syncs now: live.ts
 * calls this on the account's folder and message events, and the first of
 * those (folder.changed, as the index lists the folders) means the index can
 * answer, cheaply. A live listing of a mailbox that does not sync dials the
 * mail server, and is only done when asked for.
 */
export async function reloadIndexedFolders(id: string): Promise<void> {
  const view = accounts.folders[id]
  if (!view?.loaded || view.loading) return
  const indexed = view.list.some(folder => folder.sync_state)
  if (!indexed && !accounts.list.find(item => item.id === id)?.sync.enabled) return
  const current = currentAccounts()
  try {
    const list = await authorized(token => api.listFolders(token, id))
    const now = accounts.folders[id]
    if (current() && now?.loaded && !now.loading) accounts.folders[id] = { loading: false, loaded: true, list, failure: null }
  } catch { /* The list on screen stays; the next change, or the Refresh button, asks again. */ }
}

export async function loadFolders(id: string): Promise<void> {
  const current = currentAccounts()
  const view: FolderView = accounts.folders[id] ?? { loading: false, loaded: false, list: [], failure: null }
  accounts.folders[id] = { ...view, loading: true, failure: null }
  try {
    const list = await authorized(token => api.listFolders(token, id))
    if (current() && accounts.folders[id]) accounts.folders[id] = { loading: false, loaded: true, list, failure: null }
  } catch (error) {
    const failed = failure('folders', error)
    // A mail server that refuses the grant moves the account to needs_reauth
    // as the 409 is sent. The account is asked for before the failure is
    // shown, so the sheet explains it and offers the authorization at once,
    // rather than first blaming a mailbox that looked active.
    if (failed.code === 'conflict' && current()) await refreshAccount(id)
    if (current() && accounts.folders[id]) accounts.folders[id] = { ...accounts.folders[id]!, loading: false, failure: failed }
  }
}

// --- connecting ---------------------------------------------------------------

/**
 * The provider pages this console will send a person to. Only https, and never
 * a URL carrying credentials: the address comes from the server, and a server
 * that has been tampered with must not be able to point the page at a
 * javascript: URL or somewhere that looks like a sign-in but is not.
 */
export function providerURL(value: string | undefined): string {
  if (!value) return ''
  try {
    const url = new URL(value)
    return url.protocol === 'https:' && !url.username && !url.password ? url.toString() : ''
  } catch { return '' }
}

/**
 * The account a failure left failing, as far as its text needs it; nothing for
 * one that is not failing, whose state_reason says nothing about this attempt.
 */
function failing(account: Account | undefined): ReasonFacts | undefined {
  if (!account || (account.state !== 'error' && account.state !== 'needs_reauth')) return undefined
  return { provider: account.provider, email: account.email, state: account.state, state_reason: account.state_reason }
}

function begin(fields: Partial<ConnectState>): number {
  stopConnect()
  Object.assign(connect, freshConnect(), fields, { phase: 'starting' })
  return connectGeneration
}

function fail(generation: number, op: Operation, code: Failure['code'], account?: ReasonFacts): void {
  if (generation !== connectGeneration) return
  connect.phase = 'failed'
  connect.failure = account ? { op, code, account } : { op, code }
}

/**
 * Sleeps between polls. Stopping the attempt wakes it at once and it answers
 * false; a nudge (an account.state event for the account) ends it early and
 * it answers true, so the poll happens now.
 */
function sleep(ms: number, generation: number): Promise<boolean> {
  if (nudged) { nudged = false; return Promise.resolve(generation === connectGeneration) }
  return new Promise(resolve => {
    const done = (value: boolean) => { clearTimeout(timer); wake = null; poke = null; resolve(value) }
    const timer = setTimeout(() => done(generation === connectGeneration), ms)
    wake = () => done(false)
    poke = () => done(generation === connectGeneration)
  })
}

/** The account changed state on the server: a wait for it asks now instead of at its next poll. */
export function nudge(accountID: string): void {
  if (connect.phase !== 'waiting' || connect.accountID !== accountID) return
  if (poke) poke()
  else nudged = true
}

function pollInterval(): number {
  return server.stream === 'open' ? STREAM_POLL_INTERVAL_MS : POLL_INTERVAL_MS
}

interface Baseline { state: AccountState; reason: string }

/**
 * follow takes the flow the server chose and does what it needs.
 *
 * web: the page leaves for the provider, having noted which mailbox it was.
 * loopback and device: the person opens the provider themselves (a link a
 * popup blocker cannot eat, a code to type) and this page waits for the
 * account to change. pasted is a CLI flow; a console has nowhere to paste.
 */
function follow(generation: number, flow: AuthFlow, baseline: Baseline): void {
  if (generation !== connectGeneration) return
  connect.flow = flow.flow
  connect.expiresAt = flow.expires_at ?? 0
  switch (flow.flow) {
    case 'web': {
      const url = providerURL(flow.auth_url)
      if (!url || !connect.provider) { fail(generation, 'start-auth', 'flow_unsupported'); return }
      rememberOAuthStart({ account_id: connect.accountID, provider: connect.provider, email: connect.email })
      connect.phase = 'redirecting'
      navigation.assign(url)
      return
    }
    case 'loopback': {
      connect.authURL = providerURL(flow.auth_url)
      if (!connect.authURL) { fail(generation, 'start-auth', 'flow_unsupported'); return }
      break
    }
    case 'device': {
      connect.verificationURI = providerURL(flow.verification_uri)
      connect.userCode = flow.user_code ?? ''
      if (!connect.verificationURI || !connect.userCode) { fail(generation, 'start-auth', 'flow_unsupported'); return }
      break
    }
    default:
      fail(generation, 'start-auth', 'flow_unsupported')
      return
  }
  connect.phase = 'waiting'
  void wait(generation, connect.accountID, baseline)
}

/**
 * wait polls the account until the flow ends one way or the other.
 *
 * Success is the account turning active. Failure is the account moving to a
 * failed state — after having been pending, or to a state or reason different
 * from the one it had when "finish authorization" was pressed, because an
 * account that was already in error must not read as a new failure on the
 * first poll. A transient failure to ask is just another poll; a rate limit
 * waits as long as the server says.
 */
async function wait(generation: number, accountID: string, baseline: Baseline): Promise<void> {
  const deadline = connect.expiresAt > 0 ? connect.expiresAt * 1000 : Date.now() + FLOW_LIFETIME_MS
  let sawPending = baseline.state === 'pending_auth'
  let delay = pollInterval()
  for (;;) {
    if (!await sleep(Math.min(delay, Math.max(0, deadline - Date.now())), generation)) return
    delay = pollInterval()
    if (Date.now() >= deadline) { fail(generation, 'wait-auth', 'flow_expired'); return }
    let account: Account
    try {
      account = await authorized(token => api.getAccount(token, accountID))
    } catch (error) {
      if (generation !== connectGeneration) return
      if (error instanceof ApiError) {
        // A refused token already sent the person to sign in; the reset follows.
        if (error.code === 'unauthorized' || error.code === 'aborted') return
        // Removed elsewhere (another tab, the CLI): the card goes too, or every
        // action on it would answer 404 again.
        if (error.code === 'not_found') { forget(accountID); fail(generation, 'wait-auth', 'not_found'); return }
        if (error.code === 'rate_limited') delay = Math.max(pollInterval(), (error.retryAfter ?? 0) * 1000)
      }
      continue
    }
    if (generation !== connectGeneration) return
    upsert(account)
    if (account.state === 'active') { connect.phase = 'done'; return }
    if (account.state === 'pending_auth') { sawPending = true; continue }
    if (sawPending || account.state !== baseline.state || (account.state_reason ?? '') !== baseline.reason) {
      // The account records why; the dialog says it when it is a reason the console knows.
      fail(generation, 'wait-auth', 'flow_failed', failing(account))
      return
    }
  }
}

const refusedBeforeStoring: string[] = ['bad_request', 'not_authorized', 'unauthorized', 'rate_limited', 'aborted']

/** Connects a mailbox that signs in with Google or Microsoft. */
export async function connectOAuthAccount(input: { provider: ProviderID; email: string; displayName?: string }): Promise<void> {
  const generation = begin({ provider: input.provider, email: input.email })
  const body: AddAccountRequest = { email: input.email, provider: input.provider }
  if (input.displayName) body.display_name = input.displayName
  let result
  try {
    result = await authorized(token => api.addAccount(token, body))
  } catch (error) {
    if (generation !== connectGeneration) return
    const code = error instanceof ApiError ? error.code : ''
    // These are refused before anything is stored. Anything else (internal,
    // unavailable, conflict) may have left a row behind: the server creates
    // the account before it starts consent, and the reply never said its id.
    if (!refusedBeforeStoring.includes(code)) {
      await loadAccounts()
      if (generation !== connectGeneration) return
      const email = input.email.trim().toLowerCase()
      const row = accounts.list.find(item => item.email.toLowerCase() === email)
      if (row && code !== 'conflict' && row.auth_kind === 'oauth2' && row.state === 'pending_auth') {
        // Made by this attempt: Try again resumes it, and closing removes it.
        connect.accountID = row.id
        connect.created = true
      }
    }
    connect.phase = 'failed'
    connect.failure = failure('add-account', error)
    return
  }
  if (generation !== connectGeneration) {
    // Abandoned while the server was creating it: nothing may be left behind.
    void removeQuietly(result.account.id)
    return
  }
  upsert(result.account)
  connect.accountID = result.account.id
  connect.created = true
  if (result.account.state === 'active') { connect.phase = 'done'; return }
  if (!result.auth) { fail(generation, 'add-account', 'flow_unsupported'); return }
  follow(generation, result.auth, { state: result.account.state, reason: result.account.state_reason ?? '' })
}

/**
 * Connects a mailbox by IMAP and SMTP with a password: a server the person
 * named, or iCloud's. The server signs in before it stores anything, so this
 * either ends with an active account or with nothing to clean up.
 */
export async function connectPasswordAccount(body: AddAccountRequest): Promise<boolean> {
  const generation = begin({ provider: body.provider, email: body.email })
  try {
    const result = await authorized(token => api.addAccount(token, body))
    if (generation !== connectGeneration) return false
    upsert(result.account)
    connect.accountID = result.account.id
    connect.phase = 'done'
    return true
  } catch (error) {
    // iCloud's refusal has one likely cause worth naming; a server the person typed has several.
    if (generation === connectGeneration) { connect.phase = 'failed'; connect.failure = failure(body.provider === 'icloud' ? 'test-login-icloud' : 'test-login', error) }
    return false
  }
}

/**
 * Starts consent again for an existing account: one that never finished, lost
 * its grant, or failed. It keeps whether this dialog created the account, so a
 * retry after a failure is still cleaned up if abandoned.
 */
export async function resumeAuthorization(account: Account, created = false): Promise<void> {
  const generation = begin({ provider: account.provider, email: account.email, accountID: account.id, created })
  try {
    const flow = await authorized(token => api.startAuth(token, account.id))
    follow(generation, flow, { state: account.state, reason: account.state_reason ?? '' })
  } catch (error) {
    if (generation !== connectGeneration) return
    if (error instanceof ApiError && error.code === 'not_found') forget(account.id)
    connect.phase = 'failed'
    connect.failure = failure('start-auth', error)
  }
}

/** Try again after a failure: the same account if one exists, otherwise back to the form. */
export async function retryConnect(): Promise<boolean> {
  const account = accounts.list.find(item => item.id === connect.accountID)
  if (!account || account.auth_kind !== 'oauth2') { Object.assign(connect, freshConnect()); return false }
  await resumeAuthorization(account, connect.created)
  return true
}

async function removeQuietly(id: string): Promise<void> {
  const current = currentAccounts()
  try { await authorized(token => api.removeAccount(token, id)) }
  catch (error) { if (!(error instanceof ApiError && error.code === 'not_found')) return }
  if (current()) forget(id)
}

/**
 * cancelConnect stops waiting and, when this attempt created the account and
 * it never became active, removes it — like an abandoned pairing, an abandoned
 * connection leaves nothing behind. The server stops its loopback listener
 * when the account goes.
 */
export function cancelConnect(): void {
  const { accountID, created, phase } = connect
  stopConnect()
  if (created && accountID && phase !== 'done') {
    const account = accounts.list.find(item => item.id === accountID)
    if (!account || account.state !== 'active') void removeQuietly(accountID)
  }
  Object.assign(connect, freshConnect())
}

/** Closes a finished attempt without touching the account. */
export function resetConnect(): void {
  stopConnect()
  Object.assign(connect, freshConnect())
}

/**
 * finishOAuthReturn posts the provider's redirect, which this tab captured on
 * load, and reports the outcome at the top of the accounts section. It runs
 * once there is a session: straight after restoring one, or after sign-in.
 */
export async function finishOAuthReturn(now = Date.now()): Promise<void> {
  const found = takeOAuthReturn()
  const start = takeOAuthStart(now)
  if (!found) return
  const current = currentAccounts()
  const email = start?.email ?? ''
  if (now - found.at > FLOW_LIFETIME_MS) { accounts.notice = { kind: 'failed', email, failure: { op: 'complete-auth', code: 'return_expired' } }; return }
  if (!found.url) { accounts.notice = { kind: 'failed', email, failure: { op: 'complete-auth', code: 'return_invalid' } }; return }
  try {
    const account = await authorized(token => api.completeOAuth(token, found.url))
    if (!current()) return
    upsert(account)
    if (account.state === 'active') notify({ kind: 'connected', email: account.email, syncing: account.sync.enabled })
    // A grant the server stored and then found unusable is not a connection.
    else accounts.notice = { kind: 'failed', email: account.email, failure: { op: 'complete-auth', code: 'flow_failed', account: failing(account) } }
  } catch (error) {
    if (!current()) return
    const failed = failure('complete-auth', error)
    if (start?.account_id) {
      // The server records a refused consent on the account, with a reason
      // the notice may be able to name; and no answer, or a broken one, says
      // nothing about the exchange: the server may have stored the grant
      // after this page stopped waiting. Either way the account decides.
      await refreshAccount(start.account_id)
      if (!current()) return
      const account = accounts.list.find(item => item.id === start.account_id)
      if (account?.state === 'active' && (failed.code === 'unavailable' || failed.code === 'internal')) {
        notify({ kind: 'connected', email: account.email, syncing: account.sync.enabled })
        return
      }
      const left = failing(account)
      if (left) failed.account = left
    }
    accounts.notice = { kind: 'failed', email, failure: failed }
  }
}
