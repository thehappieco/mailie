// The one way this console talks to the daemon.
//
// Every call goes through request() (JSON), requestAnswer() (JSON from a
// route whose refusal carries an answer of its own) or requestFile() (a
// download), all over send(): same origin, bearer token in the header and
// never anywhere else, no cookies, no redirects followed, no HTTP cache, and
// a deadline. An error becomes an ApiError carrying the server's code and
// nothing it said: the console maps codes to its own translated text.

import { endpoint } from './endpoint'
import { markReachable, noteServerDate } from '../state/connection'

/** The seven codes the daemon answers with (internal/service/errors.go). */
export const serverCodes = ['unauthorized', 'not_authorized', 'bad_request', 'not_found', 'conflict', 'rate_limited', 'internal'] as const
export type ServerCode = typeof serverCodes[number]
/**
 * What a caller can be told. Beyond the server's codes: `unavailable` is no
 * answer at all (network, timeout, a proxy with no daemon behind it),
 * `invalid_response` is an answer this client cannot read, and `aborted` is the
 * caller cancelling, which is never an error to show.
 */
export type ErrorCode = ServerCode | 'unavailable' | 'invalid_response' | 'aborted'

export class ApiError extends Error {
  constructor(readonly code: ErrorCode, readonly status = 0, readonly retryAfter?: number) {
    // Server diagnostics may echo credentials or private upstream detail, so the
    // message is built here from the code and status alone. The body's own
    // `message` is never read into this object, so it cannot reach a screen or
    // a log by accident.
    super(`Request failed (${code}${status ? `, HTTP ${status}` : ''}).`)
    this.name = 'ApiError'
  }
}

export function isServerCode(value: unknown): value is ServerCode {
  return typeof value === 'string' && (serverCodes as readonly string[]).includes(value)
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  body?: unknown
  /**
   * A multipart body (a message and its files), encoded by the browser with
   * its own boundary. Never together with body.
   */
  form?: FormData
  /**
   * The request's own id (Idempotency-Key): the server does what a request
   * asks once per key, however often the request arrives.
   */
  idempotencyKey?: string
  /** The bearer credential. Omitted only for sign-in and sign-up. */
  token?: string
  timeoutMS?: number
  /** Cancels the request; the rejection is then an ApiError('aborted'). */
  signal?: AbortSignal
  /**
   * Query parameters, encoded here. A path never carries a query of its own
   * (endpoint() refuses one), so what a caller filters by is always a value
   * in this map and never text spliced into the path.
   */
  query?: Record<string, string>
}

export const DEFAULT_TIMEOUT_MS = 30_000

/** A status without a readable body still says something; this is what. */
function codeForStatus(status: number): ErrorCode {
  switch (status) {
    case 400: return 'bad_request'
    case 401: return 'unauthorized'
    case 403: return 'not_authorized'
    case 404: return 'not_found'
    case 409: return 'conflict'
    // Too large for the server, or for a proxy in front of it: refused, never half done.
    case 413: return 'bad_request'
    case 429: return 'rate_limited'
    // A reverse proxy answers these when the daemon behind it is down.
    case 502: case 503: case 504: return 'unavailable'
    default: return 'internal'
  }
}

/** Retry-After in seconds, from either form the header allows; undefined when absent or nonsense. */
export function parseRetryAfter(value: string | null, now = Date.now()): number | undefined {
  if (!value) return undefined
  const trimmed = value.trim()
  if (/^\d+$/.test(trimmed)) {
    const seconds = Number(trimmed)
    return Number.isSafeInteger(seconds) ? seconds : undefined
  }
  const at = Date.parse(trimmed)
  return Number.isFinite(at) ? Math.max(0, Math.ceil((at - now) / 1000)) : undefined
}

function combine(signals: AbortSignal[]): AbortSignal {
  if (typeof AbortSignal.any === 'function') return AbortSignal.any(signals)
  const controller = new AbortController()
  for (const signal of signals) {
    if (signal.aborted) { controller.abort(signal.reason); break }
    signal.addEventListener('abort', () => controller.abort(signal.reason), { once: true })
  }
  return controller.signal
}

/** The URL a request goes to: the page's origin, the API path, and the query encoded by URLSearchParams. */
export function requestURL(path: string, query?: Record<string, string>): string {
  const url = new URL(endpoint(path))
  for (const [name, value] of Object.entries(query ?? {})) url.searchParams.set(name, value)
  return url.toString()
}

/**
 * send makes the call every request shares: the bearer in its header and
 * nowhere else, no cookies, no redirects, no cache, a deadline. It answers the
 * response, whatever its status; a failure to get one is an ApiError here.
 */
async function send(path: string, options: RequestOptions, accept: string): Promise<Response> {
  const url = requestURL(path, options.query)
  const headers: Record<string, string> = { Accept: accept }
  if (options.token !== undefined) {
    // A token with whitespace would be a header injection or a corrupt store;
    // either way it is no credential, and the server is not asked about it.
    if (!options.token || /\s/.test(options.token)) throw new ApiError('unauthorized')
    headers.Authorization = `Bearer ${options.token}`
  }
  if (options.idempotencyKey !== undefined) {
    // Built by the console from a UUID; anything else would be a header
    // injection or a bug, and is not sent.
    if (!/^[A-Za-z0-9-]{1,128}$/.test(options.idempotencyKey)) throw new ApiError('bad_request')
    headers['Idempotency-Key'] = options.idempotencyKey
  }
  let body: string | FormData | undefined
  if (options.form !== undefined) {
    // No Content-Type here: the browser writes it, with the boundary it chose.
    body = options.form
  } else if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(options.body)
  }
  const deadline = AbortSignal.timeout(options.timeoutMS ?? DEFAULT_TIMEOUT_MS)
  const signal = options.signal ? combine([deadline, options.signal]) : deadline

  let response: Response
  try {
    response = await fetch(url, {
      method: options.method ?? (body === undefined ? 'GET' : 'POST'),
      headers, body, credentials: 'omit', redirect: 'error', cache: 'no-store', signal,
    })
  } catch {
    if (options.signal?.aborted) throw new ApiError('aborted')
    markReachable(false)
    throw new ApiError('unavailable')
  }
  const gateway = response.status === 502 || response.status === 503 || response.status === 504
  markReachable(!gateway)
  // A proxy with no daemon behind it answers with its own clock.
  if (!gateway) noteServerDate(response.headers.get('Date'))
  return response
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const response = await send(path, options, 'application/json')
  const retryAfter = parseRetryAfter(response.headers.get('Retry-After'))
  if (response.status === 204) return undefined as T

  let text: string
  try { text = await response.text() }
  catch { throw new ApiError(options.signal?.aborted ? 'aborted' : 'unavailable', response.status) }

  if (!response.ok) throw failureOf(response.status, text, retryAfter)
  // Tolerant on purpose: a proxy's HTML error page is an answer too, and must
  // become a code rather than a crash in JSON.parse.
  let parsed: unknown
  try { parsed = JSON.parse(text) } catch { throw new ApiError('invalid_response', response.status) }
  return parsed as T
}

/**
 * requestAnswer is request() for a route whose refusal can be an answer of
 * its own, such as a message the mail server refused, with the addresses it
 * refused: a reply that holds to `answer` is returned whatever its status.
 * Any other failure is an ApiError, as with request(); a success that does not
 * hold to it is one the console cannot read.
 */
export async function requestAnswer<T>(path: string, options: RequestOptions, answer: (value: unknown) => value is T): Promise<T> {
  const response = await send(path, options, 'application/json')
  let text: string
  try { text = await response.text() }
  catch { throw new ApiError(options.signal?.aborted ? 'aborted' : 'unavailable', response.status) }
  let parsed: unknown
  try { parsed = JSON.parse(text) } catch { /* Not JSON: a proxy's page, or nothing. */ }
  if (answer(parsed)) return parsed
  if (!response.ok) throw failureOf(response.status, text, parseRetryAfter(response.headers.get('Retry-After')))
  throw new ApiError('invalid_response', response.status)
}

/** A file the daemon answered: its bytes, in memory, and the name its Content-Disposition gave, if any. */
export interface FileReply { blob: Blob; filename: string }

/**
 * requestFile downloads a file into memory with the same rules as request():
 * the bearer in the header, so a download needs no token in a URL and no
 * cookie. The bytes stay in a Blob the caller hands to the browser and lets go.
 * An answer that is JSON is a failure, whatever its status: a file route never
 * answers a JSON success.
 */
export async function requestFile(path: string, options: RequestOptions = {}): Promise<FileReply> {
  const response = await send(path, options, '*/*')
  if (!response.ok) {
    let text = ''
    try { text = await response.text() } catch { /* The status decides. */ }
    throw failureOf(response.status, text, parseRetryAfter(response.headers.get('Retry-After')))
  }
  try {
    const blob = await response.blob()
    return { blob, filename: dispositionFilename(response.headers.get('Content-Disposition')) }
  } catch {
    throw new ApiError(options.signal?.aborted ? 'aborted' : 'unavailable', response.status)
  }
}

/**
 * The file name a Content-Disposition header names: filename* (RFC 6266,
 * UTF-8 percent-encoded) before filename. Empty when there is none or it
 * cannot be decoded; the caller has its own name to fall back on.
 */
export function dispositionFilename(header: string | null): string {
  if (!header) return ''
  const extended = header.match(/filename\*\s*=\s*UTF-8'[^']*'([^;\s]+)/i)
  if (extended) {
    try { return decodeURIComponent(extended[1]!) } catch { /* Fall through to the plain form. */ }
  }
  const plain = header.match(/filename\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^;\s]+))/i)
  return plain ? (plain[1] ?? plain[2] ?? '').replace(/\\(.)/g, '$1') : ''
}

/**
 * failureOf turns a failed answer into an ApiError: the code the body names
 * when it is one of the server's, else what the status says. The body's
 * message is never kept. The event stream reads its failures through this
 * too, so both ways of talking to the daemon agree on what a failure is.
 */
export function failureOf(status: number, text: string, retryAfter?: number): ApiError {
  let code: ErrorCode = codeForStatus(status)
  if (text) {
    try {
      const parsed: unknown = JSON.parse(text)
      if (typeof parsed === 'object' && parsed !== null && isServerCode((parsed as { code?: unknown }).code)) code = (parsed as { code: ServerCode }).code
    } catch { /* Not JSON; the status decides. */ }
  }
  return new ApiError(code, status, retryAfter)
}

/** checked runs a reply through its shape check, so a drifted server is an error here and not a blank screen later. */
export function checked<T>(value: unknown, valid: (value: unknown) => value is T): T {
  if (!valid(value)) throw new ApiError('invalid_response', 200)
  return value
}
