// Live updates: the event stream, kept open while the console is.
//
// The stream carries the journal's events for the person's mailboxes (the
// server decides which). The console does not rebuild its state from them:
// an event says "this account changed", and the account is read again, at
// most once a second per account, so whatever the events said and whatever
// arrived out of order, the card shows what the server holds. A folder list
// read from the index is read again the same way. An edition hears every
// event too (onLiveEvent), and that events were lost (onLiveLagged): an
// edition that shows messages patches the row a message event names, or
// hands a send's outcome to whatever waits for it.
//
// Reconnection is the console's own: after the server ends the stream, or the
// network drops it, the next attempt sends Last-Event-ID — kept in memory,
// never stored — and the server replays what was missed. Failures back off
// (1 s doubling to 30 s, with jitter, or longer when a rate limit's
// Retry-After asks for it). A refused credential is never retried:
// authorized() ends the session, and with it this loop; only a token replaced
// in the meantime (a password change) reconnects, with the new token.

import { watch } from 'vue'
import { ApiError } from '../api/http'
import { readEventStream, type StreamMessage } from '../api/events'
import { isServerEvent, type ServerEvent, type eventTypes } from '../api/types'
import { announce } from '../ui/announce'
import { t } from '../ui/i18n'
import { accounts, loadAccounts, nudge, refreshAccount, reloadIndexedFolders } from './accounts'
import { setStream } from './connection'
import { authorized, identity, session } from './session'

export const RETRY_BASE_MS = 1_000
export const RETRY_MAX_MS = 30_000
/** A stream that lasted this long, or delivered anything, was healthy: the next failure starts the backoff over. */
export const HEALTHY_MS = 5_000
/** At most one read of an account per this interval, however many events name it. */
export const ACCOUNT_REFRESH_MS = 1_000
/** And of a folder list read from the index. */
export const FOLDERS_REFRESH_MS = 3_000

/** A seam for tests: the jitter. */
export const timing = { random: Math.random }

/** An event type of the journal (internal/events). */
export type EventType = typeof eventTypes[number]

const eventHandlers = new Map<string, Set<(event: ServerEvent) => void>>()
const laggedHandlers = new Set<() => void>()

/**
 * Hears every event of this type, after the core has scheduled its own
 * reads. A handler runs in the stream's loop: it only changes state, and
 * leaves any request to a timer or a promise of its own. Returns what stops
 * hearing.
 */
export function onLiveEvent(type: EventType, handler: (event: ServerEvent) => void): () => void {
  const handlers = eventHandlers.get(type) ?? new Set()
  handlers.add(handler)
  eventHandlers.set(type, handlers)
  return () => { handlers.delete(handler) }
}

/** Hears that events were lost (the server pruned past the cursor): what the page shows may be stale. */
export function onLiveLagged(handler: () => void): () => void {
  laggedHandlers.add(handler)
  return () => { laggedHandlers.delete(handler) }
}

let generation = 0
let controller: AbortController | null = null
/** The last event id received, for Last-Event-ID. Memory only: a reload starts from now. */
let cursor = ''
let wake: (() => void) | null = null
const accountTimers = new Map<string, ReturnType<typeof setTimeout>>()
const folderTimers = new Map<string, ReturnType<typeof setTimeout>>()

// Another person's stream must not resume from this one's cursor.
watch(identity, () => stopLive({ forget: true }), { flush: 'sync' })

/** The Last-Event-ID the next connection sends. */
export function lastEventID(): string {
  return cursor
}

/** Starts the stream, unless it is already running. */
export function startLive(): void {
  if (controller) return
  const run = ++generation
  controller = new AbortController()
  void loop(run, controller.signal)
}

/**
 * Stops the stream and forgets its cursor when the person changes. The same
 * person's cursor survives a stop, so a console shown again resumes where it
 * left off.
 */
export function stopLive(options: { forget?: boolean } = {}): void {
  generation++
  controller?.abort()
  controller = null
  wake?.()
  for (const timer of [...accountTimers.values(), ...folderTimers.values()]) clearTimeout(timer)
  accountTimers.clear()
  folderTimers.clear()
  if (options.forget) cursor = ''
  setStream('off')
}

/** Cuts a pending reconnection short: the network came back, the page became visible. */
export function wakeLive(): void {
  wake?.()
}

function backoff(attempt: number): number {
  const ceiling = Math.min(RETRY_MAX_MS, RETRY_BASE_MS * 2 ** Math.min(attempt, 10))
  return Math.round(ceiling * (0.5 + timing.random() / 2))
}

function pause(ms: number, run: number): Promise<boolean> {
  return new Promise(resolve => {
    const done = () => { clearTimeout(timer); wake = null; resolve(run === generation) }
    const timer = setTimeout(done, ms)
    wake = done
  })
}

async function loop(run: number, signal: AbortSignal): Promise<void> {
  let attempt = 0
  let refusals = 0
  while (run === generation) {
    setStream('connecting')
    let opened = 0
    let delivered = false
    /** A rate limit's Retry-After: the next attempt waits at least this long. */
    let asked = 0
    try {
      await authorized(token => readEventStream({
        token, lastEventID: cursor, signal,
        onOpen: () => { opened = Date.now(); setStream('open') },
        onMessage: message => { delivered = true; receive(message) },
      }))
      // The server ended the stream, as a restart does; resume from the cursor.
    } catch (error) {
      if (run !== generation) return
      const code = error instanceof ApiError ? error.code : 'internal'
      if (code === 'aborted') return
      if (code === 'unauthorized') {
        // authorized() has already ended the session if this was its token.
        // Still signed in means the token was replaced while the stream was
        // open; reconnect with the new one, but not forever.
        if (session.phase !== 'ready' || ++refusals > 2) { setStream('stopped'); return }
        continue
      }
      // A credential that may not read events will not start to: stop.
      if (code === 'not_authorized') { setStream('stopped'); return }
      // A cursor the server will not take is dropped; the next stream starts from now.
      if (code === 'bad_request') { cursor = ''; void loadAccounts() }
      if (code === 'rate_limited' && error instanceof ApiError) asked = Math.min(RETRY_MAX_MS * 10, (error.retryAfter ?? 0) * 1000)
    }
    if (run !== generation) return
    refusals = 0
    if (delivered || (opened && Date.now() - opened >= HEALTHY_MS)) attempt = 0
    const delay = Math.max(backoff(attempt++), asked)
    setStream('retrying')
    if (!await pause(delay, run)) return
  }
}

function receive(message: StreamMessage): void {
  if (message.event === 'lagged') {
    // Events after the cursor were pruned: what the page shows may be
    // missing changes no event will bring back. Read it all again.
    void loadAccounts()
    for (const id of Object.keys(accounts.folders)) scheduleFolders(id)
    for (const handler of laggedHandlers) handler()
    return
  }
  let event: unknown
  try { event = JSON.parse(message.data) } catch { return }
  if (!isServerEvent(event)) return
  if (/^\d+$/.test(message.lastEventID)) cursor = message.lastEventID
  const id = event.account_id
  switch (event.type) {
    case 'account.state':
      // Ends a wait for an authorization at once, instead of at its next poll.
      nudge(id)
      scheduleAccount(id)
      break
    case 'folder.changed':
    case 'message.new':
    case 'message.moved':
    case 'message.deleted':
    case 'message.flags':
      scheduleAccount(id)
      scheduleFolders(id)
      break
    case 'send.finished':
      // A send settled: the account did not change, and whoever waits for it hears below.
      break
    default:
      scheduleAccount(id)
  }
  for (const handler of eventHandlers.get(event.type) ?? []) handler(event)
}

function scheduleAccount(id: string): void {
  if (accountTimers.has(id)) return
  const run = generation
  accountTimers.set(id, setTimeout(() => {
    accountTimers.delete(id)
    if (run === generation) void refresh(id)
  }, ACCOUNT_REFRESH_MS))
}

function scheduleFolders(id: string): void {
  if (folderTimers.has(id) || !accounts.folders[id]?.loaded) return
  const run = generation
  folderTimers.set(id, setTimeout(() => {
    folderTimers.delete(id)
    if (run === generation) void reloadIndexedFolders(id)
  }, FOLDERS_REFRESH_MS))
}

/** Reads the account again, and says so when its first sync has just finished. */
async function refresh(id: string): Promise<void> {
  const before = accounts.list.find(item => item.id === id)?.sync.state
  const after = await refreshAccount(id)
  if (before === 'initial' && after?.sync.state === 'live') announce(t('{email} finished its first sync.', { email: after.email }))
}
