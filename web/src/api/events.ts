// The event stream (GET /v1/events), read with fetch.
//
// Never EventSource: it cannot send a header, so the bearer token would have
// to travel in the URL, where proxies log it and the browser keeps it in its
// history. fetch sends it in Authorization like every other call, and the
// body is read as it arrives and cut into events by the parser below, which
// follows the text/event-stream rules (HTML Living Standard, "Server-sent
// events") without the reconnection those leave to EventSource: the console
// decides for itself when to reconnect (state/live.ts).

import { endpoint } from './endpoint'
import { ApiError, failureOf, isServerCode, parseRetryAfter } from './http'
import { markReachable } from '../state/connection'

/** One dispatched event: its type ("message" when the stream named none), its data, and the last id the stream set. */
export interface StreamMessage { event: string; data: string; lastEventID: string }

/**
 * A line longer than this is not an event this server sends; the parser
 * refuses it rather than buffer a misbehaving stream until the tab runs out
 * of memory.
 */
export const MAX_LINE = 1 << 20

/**
 * EventStreamParser turns the stream's text, in whatever pieces it arrives,
 * into events. A piece may end anywhere: inside a field name, between a CR
 * and its LF, in the middle of a line of data.
 */
export class EventStreamParser {
  /** The id the next event carries: set by an id field, kept until another replaces it. */
  lastEventID = ''
  private pending = ''
  private data: string[] = []
  private event = ''
  private started = false
  /** The previous piece ended with a CR, so an LF at the start of this one belongs to it. */
  private afterCR = false

  push(piece: string): StreamMessage[] {
    let text = piece
    if (!text) return []
    if (!this.started) {
      this.started = true
      if (text.charCodeAt(0) === 0xfeff) text = text.slice(1)
    }
    if (this.afterCR && text.charCodeAt(0) === 10) text = text.slice(1)
    this.afterCR = false
    const out: StreamMessage[] = []
    let buffer = this.pending + text
    let start = 0
    for (let i = 0; i < buffer.length; i++) {
      const c = buffer.charCodeAt(i)
      if (c !== 10 && c !== 13) continue
      const line = buffer.slice(start, i)
      if (c === 13) {
        if (i + 1 < buffer.length) { if (buffer.charCodeAt(i + 1) === 10) i++ }
        else this.afterCR = true
      }
      start = i + 1
      this.line(line, out)
    }
    buffer = buffer.slice(start)
    if (buffer.length > MAX_LINE) throw new ApiError('invalid_response')
    this.pending = buffer
    return out
  }

  private line(line: string, out: StreamMessage[]): void {
    if (line === '') { this.dispatch(out); return }
    // A comment: the server's keep-alive pings.
    if (line.charCodeAt(0) === 58) return
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    if (value.charCodeAt(0) === 32) value = value.slice(1)
    switch (field) {
      case 'event': this.event = value; break
      case 'data': this.data.push(value); break
      // An id with a NUL is ignored, as the standard says.
      case 'id': if (!value.includes('\0')) this.lastEventID = value; break
      // retry and anything unknown: the console keeps its own reconnection schedule.
      default: break
    }
  }

  private dispatch(out: StreamMessage[]): void {
    const event = this.event || 'message'
    const data = this.data
    this.event = ''
    this.data = []
    if (!data.length) return
    out.push({ event, data: data.join('\n'), lastEventID: this.lastEventID })
  }
}

export interface StreamOptions {
  token: string
  /** The last event id this page received; the server resumes after it. */
  lastEventID?: string
  signal: AbortSignal
  /** The server answered 200 with an event stream. */
  onOpen?: () => void
  onMessage: (message: StreamMessage) => void
  /**
   * How long the stream may stay silent before it counts as dead. The server
   * pings every 15 s, so three missed pings is a connection that went away
   * without closing: a laptop that slept, a network that changed.
   */
  idleMS?: number
}

export const STREAM_IDLE_MS = 45_000

/** An event id is a journal sequence number; anything else is not sent back. */
const eventID = /^\d{1,19}$/

/**
 * readEventStream reads the stream until the server ends it, which resolves,
 * or until it fails, which rejects with an ApiError: aborted when the caller
 * stopped it, unauthorized when the credential is refused (at the start, or
 * later in an `error` event, which the server sends when a session ends or a
 * key is revoked while the stream is open), and unavailable for a network
 * that failed or a stream that went silent.
 */
export async function readEventStream(options: StreamOptions): Promise<void> {
  if (!options.token || /\s/.test(options.token)) throw new ApiError('unauthorized')
  const headers: Record<string, string> = { Accept: 'text/event-stream', Authorization: `Bearer ${options.token}` }
  if (options.lastEventID && eventID.test(options.lastEventID)) headers['Last-Event-ID'] = options.lastEventID

  // The caller's signal stops everything; the watchdog stops a silent stream.
  const watchdog = new AbortController()
  const stop = () => watchdog.abort()
  if (options.signal.aborted) throw new ApiError('aborted')
  options.signal.addEventListener('abort', stop, { once: true })
  let timer: ReturnType<typeof setTimeout> | undefined
  let silent = false
  const idle = options.idleMS ?? STREAM_IDLE_MS
  const arm = () => {
    clearTimeout(timer)
    timer = setTimeout(() => { silent = true; watchdog.abort() }, idle)
  }
  const failure = () => options.signal.aborted ? new ApiError('aborted') : new ApiError('unavailable')

  try {
    arm()
    let response: Response
    try {
      response = await fetch(endpoint('/v1/events'), {
        method: 'GET', headers, credentials: 'omit', redirect: 'error', cache: 'no-store', signal: watchdog.signal,
      })
    } catch {
      if (!options.signal.aborted) markReachable(false)
      throw failure()
    }
    const gateway = response.status === 502 || response.status === 503 || response.status === 504
    markReachable(!gateway)
    if (!response.ok) {
      const text = await response.text().catch(() => '')
      throw failureOf(response.status, text, parseRetryAfter(response.headers.get('Retry-After')))
    }
    const type = response.headers.get('Content-Type') ?? ''
    if (!type.toLowerCase().startsWith('text/event-stream') || !response.body) {
      await response.body?.cancel().catch(() => {})
      throw new ApiError('invalid_response', response.status)
    }
    options.onOpen?.()

    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    const parser = new EventStreamParser()
    parser.lastEventID = options.lastEventID ?? ''
    const deliver = (messages: StreamMessage[]) => {
      for (const message of messages) {
        if (message.event === 'error') throw ended(message.data)
        options.onMessage(message)
      }
    }
    try {
      for (;;) {
        arm()
        const { done, value } = await reader.read()
        if (done) break
        deliver(parser.push(decoder.decode(value, { stream: true })))
      }
      deliver(parser.push(decoder.decode()))
    } catch (error) {
      reader.cancel().catch(() => {})
      if (error instanceof ApiError && !silent) throw error
      throw failure()
    }
  } finally {
    clearTimeout(timer)
    options.signal.removeEventListener('abort', stop)
  }
}

/**
 * ended reads the error event the server sends when the credential stopped
 * working mid-stream: the same {code, message} body as every error, of which
 * only the code is kept.
 */
function ended(data: string): ApiError {
  try {
    const body: unknown = JSON.parse(data)
    const code = typeof body === 'object' && body !== null ? (body as { code?: unknown }).code : undefined
    if (isServerCode(code)) return new ApiError(code)
  } catch { /* Not JSON: an ending all the same. */ }
  return new ApiError('internal')
}
