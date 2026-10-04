import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { getAccount, listAccounts } from '../src/api/accounts'
import { endpoint, segment } from '../src/api/endpoint'
import { ApiError, parseRetryAfter, request, serverCodes } from '../src/api/http'
import { server } from '../src/state/connection'
import { account, failure, json, ORIGIN } from './support'

beforeEach(() => { vi.stubGlobal('location', new URL(ORIGIN + '/')); server.reachable = true })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

const statuses: Record<string, number> = { unauthorized: 401, not_authorized: 403, bad_request: 400, not_found: 404, conflict: 409, rate_limited: 429, internal: 500 }

describe('the one request wrapper', () => {
  it('sends the token only as a bearer header, to this origin, with no cookies, redirects or cache', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json([]))
    await listAccounts('tok_value')
    const [url, init] = fetch.mock.calls[0]!
    expect(url).toBe(ORIGIN + '/v1/accounts')
    expect(init).toMatchObject({ method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store' })
    expect((init!.headers as Record<string, string>).Authorization).toBe('Bearer tok_value')
    expect(init!.signal).toBeInstanceOf(AbortSignal)
    expect(String(url)).not.toContain('tok_value')
  })

  it.each(serverCodes)('turns %s into its code and never keeps what the server said', async code => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(failure(code, statuses[code]!))
    const error = await request('/v1/accounts', { token: 't' }).then(() => { throw new Error('expected a failure') }, (e: unknown) => e as ApiError)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe(code)
    expect(error.status).toBe(statuses[code])
    for (const text of [error.message, JSON.stringify(error), String(error), ...Object.values(error).map(String)]) {
      expect(text).not.toContain('hunter2')
      expect(text).not.toContain('upstream said')
    }
  })

  it('answers a 204 with nothing', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }))
    await expect(request('/v1/accounts/acc_1', { token: 't', method: 'DELETE' })).resolves.toBeUndefined()
  })

  it('reads an error body that is not JSON by its status instead of crashing', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch')
    fetch.mockResolvedValueOnce(new Response('<html>Bad Gateway</html>', { status: 502 }))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'unavailable', status: 502 })
    fetch.mockResolvedValueOnce(new Response('oops', { status: 500 }))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'internal' })
    fetch.mockResolvedValueOnce(new Response('', { status: 404 }))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'not_found' })
    fetch.mockResolvedValueOnce(json({ code: 'bad_credentials', message: 'x' }, 401))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'unauthorized' })
  })

  it('calls a success it cannot read an invalid response', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('<html>index</html>', { status: 200 }))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'invalid_response' })
  })

  it('rejects a reply of the wrong shape rather than drawing it', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json([{ id: 1, email: 'x' }]))
    await expect(listAccounts('t')).rejects.toMatchObject({ code: 'invalid_response' })
  })

  it('keeps how long a rate limit asks to wait', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(failure('rate_limited', 429, { 'Retry-After': '7' }))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'rate_limited', retryAfter: 7 })
    expect(parseRetryAfter(new Date(Date.now() + 30_000).toUTCString())).toBeGreaterThanOrEqual(29)
    expect(parseRetryAfter('soon')).toBeUndefined()
    expect(parseRetryAfter(null)).toBeUndefined()
  })

  it('treats no answer as unavailable and a caller cancelling as aborted', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch')
    fetch.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'unavailable' })
    expect(server.reachable).toBe(false)
    const controller = new AbortController()
    controller.abort()
    fetch.mockRejectedValueOnce(new DOMException('aborted', 'AbortError'))
    await expect(request('/v1/accounts', { token: 't', signal: controller.signal })).rejects.toMatchObject({ code: 'aborted' })
    fetch.mockResolvedValueOnce(failure('not_found', 404))
    await expect(request('/v1/accounts', { token: 't' })).rejects.toMatchObject({ code: 'not_found' })
    expect(server.reachable).toBe(true)
  })

  it('never sends a token that could inject a header', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch')
    await expect(request('/v1/accounts', { token: 'a\r\nX-Evil: 1' })).rejects.toMatchObject({ code: 'unauthorized' })
    await expect(request('/v1/accounts', { token: '' })).rejects.toMatchObject({ code: 'unauthorized' })
    expect(fetch).not.toHaveBeenCalled()
  })

  it('builds paths only from escaped identifiers and never leaves the API', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(account()))
    await getAccount('t', 'a/../../apikeys')
    expect(String(fetch.mock.calls[0]![0])).toBe(ORIGIN + '/v1/accounts/a%2F..%2F..%2Fapikeys')
    expect(() => segment('..')).toThrow()
    expect(() => endpoint('/v1/accounts/../apikeys')).toThrow()
    expect(() => endpoint('//evil.example/v1/x')).toThrow()
    expect(() => endpoint('/v1/accounts?x=1')).toThrow()
  })

  it('never sends a key that could inject a header', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch')
    await expect(request('/v1/messages/send', { token: 't', idempotencyKey: 'k\r\nX-Evil: 1', body: {} })).rejects.toMatchObject({ code: 'bad_request' })
    expect(fetch).not.toHaveBeenCalled()
  })

})
