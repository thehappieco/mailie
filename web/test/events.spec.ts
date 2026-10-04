// The event stream as the console reads it: a text/event-stream parser that
// does not care where the network cut the body, and a reader that talks to
// /v1/events with fetch, the bearer in a header, and nothing in the URL.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { EventStreamParser, MAX_LINE, readEventStream, type StreamMessage } from '../src/api/events'
import { ApiError } from '../src/api/http'
import { server } from '../src/state/connection'
import { failure, ORIGIN } from './support'

beforeEach(() => { vi.stubGlobal('location', new URL(ORIGIN + '/')); server.reachable = true })
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

/** What the daemon writes (internal/api/events.go): an optional id, the type, one data line. */
const wire = (id: string, type: string, data: object) => `${id ? `id: ${id}\n` : ''}event: ${type}\ndata: ${JSON.stringify(data)}\n\n`
const newMail = { seq: 7, type: 'message.new', account_id: 'acc_0000000000000002', at: 1_790_000_000, payload: { subject: 'Almoço na sexta? 🍝', from: { name: 'Bea Lima', email: 'bea@example.com' } } }
const stream = [': connected\n\n', wire('7', 'message.new', newMail), ': ping\n\n', wire('8', 'sync.progress', { ...newMail, seq: 8, type: 'sync.progress', payload: { progress: 50 } })].join('')

/** Feeds text to a fresh parser in the pieces given, and collects what it dispatched. */
function parse(pieces: string[]): StreamMessage[] {
  const parser = new EventStreamParser()
  return pieces.flatMap(piece => parser.push(piece))
}

/** Every way of cutting text in two, three and more places. */
function cuts(text: string): string[][] {
  const out: string[][] = []
  for (let i = 1; i < text.length; i++) out.push([text.slice(0, i), text.slice(i)])
  for (let size = 1; size <= 7; size++) out.push(Array.from({ length: Math.ceil(text.length / size) }, (_, k) => text.slice(k * size, k * size + size)))
  return out
}

describe('the event stream parser', () => {
  it('finds the same events however the body is cut into chunks', () => {
    const whole = parse([stream])
    expect(whole.map(message => [message.event, message.lastEventID])).toEqual([['message.new', '7'], ['sync.progress', '8']])
    expect(JSON.parse(whole[0]!.data)).toEqual(newMail)
    for (const pieces of cuts(stream)) expect(parse(pieces), JSON.stringify(pieces.slice(0, 3))).toEqual(whole)
  })

  it('takes CRLF, CR and LF line ends, including a CRLF split between two chunks', () => {
    const lf = 'id: 3\nevent: folder.changed\ndata: {"a":1}\n\n'
    const expected = parse([lf])
    expect(expected).toEqual([{ event: 'folder.changed', data: '{"a":1}', lastEventID: '3' }])
    const crlf = lf.replace(/\n/g, '\r\n')
    const cr = lf.replace(/\n/g, '\r')
    for (const text of [crlf, cr]) {
      expect(parse([text])).toEqual(expected)
      for (const pieces of cuts(text)) expect(parse(pieces)).toEqual(expected)
    }
    // A CR at the end of one chunk and its LF at the start of the next is one line end, not two.
    expect(parse(['data: x\r', '\n\r', '\n'])).toEqual([{ event: 'message', data: 'x', lastEventID: '' }])
  })

  it('ignores comments, joins data lines, strips one leading space, and dispatches nothing without data', () => {
    const messages = parse([': ping\n\nevent: lagged\n\nid: 9\ndata:first\ndata:  second\n\nevent: account.state\ndata\n\n'])
    // The event with no data line is dropped, but the id it set is kept.
    expect(messages).toEqual([
      { event: 'message', data: 'first\n second', lastEventID: '9' },
      { event: 'account.state', data: '', lastEventID: '9' },
    ])
  })

  it('keeps the last id across events that carry none, and ignores an id with a NUL', () => {
    const messages = parse(['id: 41\ndata: a\n\nevent: lagged\ndata: {}\n\nid: 4\u00002\ndata: b\n\n'])
    expect(messages.map(message => message.lastEventID)).toEqual(['41', '41', '41'])
  })

  it('drops a byte-order mark at the start of the stream', () => {
    expect(parse(['﻿data: x\n\n'])).toEqual([{ event: 'message', data: 'x', lastEventID: '' }])
  })

  it('refuses a line longer than any event the server sends, instead of buffering it forever', () => {
    const parser = new EventStreamParser()
    expect(() => parser.push('data: ' + 'x'.repeat(MAX_LINE + 1))).toThrow(ApiError)
  })
})

/** A body that arrives in the given byte chunks, then ends, or stays open. */
function body(chunks: Uint8Array[], { open = false } = {}): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk)
      if (!open) controller.close()
    },
  })
}

function sse(chunks: Uint8Array[], options: { open?: boolean } = {}): Response {
  return new Response(body(chunks, options), { status: 200, headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' } })
}

async function read(options: Partial<Parameters<typeof readEventStream>[0]> = {}) {
  const received: StreamMessage[] = []
  let opened = false
  const done = readEventStream({ token: 'tok_value', signal: new AbortController().signal, onOpen: () => { opened = true }, onMessage: message => received.push(message), ...options })
  return { done, received, opened: () => opened }
}

describe('reading the event stream', () => {
  it('asks with fetch and the bearer in a header, never EventSource, never a token in the URL', async () => {
    const EventSource = vi.fn(() => { throw new Error('EventSource must not be used') })
    vi.stubGlobal('EventSource', EventSource)
    const fetch = vi.spyOn(globalThis, 'fetch').mockResolvedValue(sse([new TextEncoder().encode(stream)]))
    const { done, received, opened } = await read({ lastEventID: '6' })
    await done
    expect(EventSource).not.toHaveBeenCalled()
    const [url, init] = fetch.mock.calls[0]!
    expect(url).toBe(ORIGIN + '/v1/events')
    expect(String(url)).not.toContain('tok_value')
    expect(new URL(String(url)).search).toBe('')
    expect(init).toMatchObject({ method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store' })
    const headers = init!.headers as Record<string, string>
    expect(headers.Authorization).toBe('Bearer tok_value')
    expect(headers.Accept).toBe('text/event-stream')
    expect(headers['Last-Event-ID']).toBe('6')
    expect(opened()).toBe(true)
    expect(received.map(message => message.lastEventID)).toEqual(['7', '8'])
  })

  it('decodes a character split between two network chunks', async () => {
    const bytes = new TextEncoder().encode(stream)
    // Cut inside the four bytes of the emoji and inside the ç.
    const emoji = bytes.findIndex((_, i) => bytes[i] === 0xf0)
    const cedilla = bytes.findIndex((_, i) => bytes[i] === 0xc3)
    const chunks = [bytes.slice(0, cedilla + 1), bytes.slice(cedilla + 1, emoji + 2), bytes.slice(emoji + 2)]
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(sse(chunks))
    const { done, received } = await read()
    await done
    expect(JSON.parse(received[0]!.data).payload.subject).toBe('Almoço na sexta? 🍝')
  })

  it('sends no Last-Event-ID that is not an event id', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockResolvedValue(sse([]))
    await (await read({ lastEventID: '7\r\nX-Evil: 1' })).done
    expect((fetch.mock.calls[0]![1]!.headers as Record<string, string>)['Last-Event-ID']).toBeUndefined()
  })

  it('turns a refused credential into unauthorized, before the stream and in the middle of it', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(failure('unauthorized', 401))
    await expect((await read()).done).rejects.toMatchObject({ code: 'unauthorized', status: 401 })
    // The daemon's ping found the session ended: an error event with the usual body.
    const ended = wire('', 'error', { code: 'unauthorized', message: 'upstream said: password=hunter2' })
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(sse([new TextEncoder().encode(wire('7', 'message.new', newMail) + ended)], { open: true }))
    const { done, received } = await read()
    const error = await done.then(() => null, (e: unknown) => e as ApiError)
    expect(error?.code).toBe('unauthorized')
    expect(JSON.stringify(error)).not.toContain('hunter2')
    expect(received).toHaveLength(1)
  })

  it('refuses an answer that is not an event stream', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('<html>login</html>', { status: 200, headers: { 'Content-Type': 'text/html' } }))
    await expect((await read()).done).rejects.toMatchObject({ code: 'invalid_response' })
  })

  it('counts a network failure as unavailable, and a stop as aborted', async () => {
    vi.spyOn(globalThis, 'fetch').mockRejectedValueOnce(new TypeError('network'))
    await expect((await read()).done).rejects.toMatchObject({ code: 'unavailable' })
    expect(server.reachable).toBe(false)
    const controller = new AbortController()
    vi.spyOn(globalThis, 'fetch').mockImplementationOnce((_url, init) => new Promise((_, reject) => {
      init!.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
    }))
    const { done } = await read({ signal: controller.signal })
    controller.abort()
    await expect(done).rejects.toMatchObject({ code: 'aborted' })
  })

  it('gives up on a stream that stays silent past three pings', async () => {
    vi.useFakeTimers()
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (_url, init) => {
      const signal = init!.signal!
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(': connected\n\n'))
          signal.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')))
        },
      }), { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    })
    const { done, opened } = await read({ idleMS: 45_000 })
    const outcome = done.then(() => 'ended', (e: ApiError) => e.code)
    await vi.advanceTimersByTimeAsync(44_000)
    expect(opened()).toBe(true)
    await vi.advanceTimersByTimeAsync(2_000)
    expect(await outcome).toBe('unavailable')
  })
})
