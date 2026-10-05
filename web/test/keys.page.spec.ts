// API & MCP as a person presses it, on a page (test/dom.ts): creating a key
// asks nothing of the server until Create key, under the text of what the
// key authorizes; the key is shown once, copied on request, and gone from the
// page when the dialog closes, which only Done or the close button do while
// the key is shown; the command with the key in it goes to the clipboard and
// never onto the page; Read and act exists only while actions are allowed;
// revoking asks first.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { check, click, fill, find, fire, flush, keydown, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { CreatedKey, PersonalKey } from '../src/api/types'
import KeysPanel from '../src/components/KeysPanel.vue'
import { loadAccounts } from '../src/state/accounts'
import { loadActionsConsent } from '../src/state/actionsConsent'
import { apiKeys } from '../src/state/apikeys'
import { signIn, signOut } from '../src/state/session'
import { ACTIONS_TEXT_VERSION, KEY_TERMS_VERSION } from '../src/open/versions'
import { account, failure, json, now, reply, serve, stubPage, type Route } from './support'

const PREFIX = '3f9a0c1d2e4b5a6c'
const SECRET = `${PREFIX}.Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg`
const COMMAND = `claude mcp add --transport http mailie http://localhost:5174/mcp --header "Authorization: Bearer ${SECRET}"`
const ONE = 'acc_0000000000000001'
const TWO = 'acc_0000000000000002'
const given = { consented: true, consented_at: 1_790_000_000, version: ACTIONS_TEXT_VERSION, current_version: ACTIONS_TEXT_VERSION }
const notYet = { consented: false, current_version: ACTIONS_TEXT_VERSION }

function listed(prefix: string, fields: Partial<PersonalKey> = {}): PersonalKey {
  return { prefix, name: `Key ${prefix.slice(0, 4)}`, scope: 'read', created_at: now() - 86_400, expires_at: now() + 89 * 86_400, terms_version: KEY_TERMS_VERSION, ...fields }
}

let keys: PersonalKey[] = []
/** What GET /v1/me/mcp answers: whether the server serves MCP over HTTP (MAIL_MCP_HTTP). */
let mcpServed = true
let mounted: Mounted | null = null
let clipboard: string[] = []

/** Signed in, with two mailboxes, actions allowed or not; route answers the key routes. */
async function signedIn(consent: object, route: Route) {
  const fetch = serve(request => {
    const { path, method } = request
    if (path === '/v1/auth/login') return json(reply())
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/accounts') return json([account({ id: ONE, state: 'active' }), account({ id: TWO, email: 'loja@example.test', provider: 'imap', state: 'active' })])
    if (path === '/v1/me/actions-consent' && method === 'GET') return json(consent)
    if (path === '/v1/me/apikeys' && method === 'GET') return json(keys)
    if (path === '/v1/me/mcp' && method === 'GET') return json({ http: mcpServed })
    return route(request)
  })
  await signIn('ana@example.test', 'correct-password')
  await loadAccounts()
  await loadActionsConsent()
  mounted = mount(KeysPanel)
  await vi.waitFor(() => expect(apiKeys.loaded).toBe(true))
  await flush()
  return fetch
}
const posts = (fetch: ReturnType<typeof serve>) => fetch.mock.calls
  .filter(([url, init]) => new URL(String(url)).pathname === '/v1/me/apikeys' && init?.method === 'POST')
  .map(([, init]) => JSON.parse(String(init!.body)))
const deletes = (fetch: ReturnType<typeof serve>) => fetch.mock.calls
  .filter(([, init]) => init?.method === 'DELETE').map(([url]) => new URL(String(url)).pathname)

/** A daemon that makes the key it is asked for. */
const creates: Route = ({ path, method, body }) => {
  if (path !== '/v1/me/apikeys' || method !== 'POST') return failure('not_found', 404)
  const request = body as { name: string; scope: string; account_ids?: string[]; ttl_days: number }
  const created: CreatedKey = { ...listed(PREFIX, { name: request.name, scope: request.scope, created_at: now(), expires_at: now() + request.ttl_days * 86_400 }), key: SECRET }
  if (request.account_ids) created.account_ids = request.account_ids
  keys = [created, ...keys]
  return json(created, 201)
}

const dialog = () => find('dialog')
const createButton = () => find('.section-actions button', 'Create key') ?? find('.empty-card button', 'Create key')

beforeEach(() => {
  stubPage()
  keys = []
  mcpServed = true
  clipboard = []
  vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async (text: string) => { clipboard.push(text) }) } })
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('creating a key', () => {
  it('asks the server nothing until Create key, under the text of what the key allows, and shows the key once', async () => {
    const fetch = await signedIn(given, creates)
    await click(createButton())
    const form = find('dialog form')!
    const text = words(dialog()!)
    expect(text).toContain('Whoever holds this key can reach the mailboxes it names through this server’s API and MCP server, until the key expires or you revoke it.')
    expect(text).toContain('read any of those messages, text and attachments included, which this server fetches from the mail server when the tool asks')
    // What the MCP server keeps to resume a dropped stream is said, with its bound (internal/mcp DefaultEventStoreAge).
    expect(text).toContain('This server does not store what it fetches for the tool. So that the tool can pick up a dropped connection, the MCP server holds what it sent in memory only, never on disk, for at most five minutes.')
    expect(text).not.toContain('does not keep')
    expect(text).toContain('An AI assistant such as Claude, for example, sends it to Anthropic to answer you.')
    // The open console's text belongs to whoever runs the server: it links to no company policy.
    expect(find('dialog .key-terms a')).toBeNull()
    // The actions line belongs to Read and act, and shows with it.
    expect(text).not.toContain('mark messages as read or unread, star them')
    await fill(find('dialog input[name=key-name]'), '  Claude Code  ')
    await check(find('dialog input[value=write]'))
    expect(words(dialog()!)).toContain('mark messages as read or unread, star them, archive them, and move them to another folder or to the trash, for as long as you allow actions on your messages')
    await check(find('dialog label.option', 'Only the ones I choose')!.querySelector('input'))
    await check(find('dialog .mailbox-choices label', 'loja@example.test')!.querySelector('input'))
    await check(find('dialog label.lifetime', '30 days')!.querySelector('input'))
    expect(posts(fetch)).toEqual([])

    await submit(form)
    expect(posts(fetch)).toEqual([{ name: 'Claude Code', scope: 'write', account_ids: [TWO], ttl_days: 30, terms_version: KEY_TERMS_VERSION }])
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    expect(find('dialog .key-secret')!.getAttribute('translate')).toBe('no')
    expect(words(dialog()!)).toContain('Mailie shows it only this once and cannot show it again.')
    expect(words(find('dialog .key-facts')!)).toContain('loja@example.test')
    // The list has the key, without its secret.
    expect(words(find('.key-card')!)).toContain('Claude Code')
    expect(words(find('.key-card')!)).not.toContain(SECRET)
    expect(page.activeElement).toBe(find('dialog button', 'Copy key'))
  })

  it('copies the key, and the command with the key only to the clipboard, never onto the page', async () => {
    await signedIn(notYet, creates)
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    await click(find('dialog button', 'Copy key'))
    await click(find('dialog button', 'Copy the Claude Code command with this key'))
    expect(clipboard).toEqual([SECRET, COMMAND])
    expect(words()).not.toContain(`Bearer ${SECRET}`)
    expect(words(find('dialog')!)).toContain('Command copied')
    // The command on the page carries the placeholder, never a key.
    expect(words(find('.connect-panel')!)).toContain('--header "Authorization: Bearer <your key>"')
    expect(words(find('.connect-panel')!)).not.toContain(PREFIX)
  })

  it('offers no Claude Code command, and shows no MCP address, on a server that does not serve MCP over HTTP', async () => {
    mcpServed = false
    const fetch = await signedIn(notYet, creates)
    expect(fetch.mock.calls.some(([url]) => new URL(String(url)).pathname === '/v1/me/mcp')).toBe(true)
    expect(find('.connect-panel')).toBeNull()
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    expect(find('dialog button', 'Copy the Claude Code command with this key')).toBeNull()
    expect(words(dialog()!)).not.toContain('The command connects Claude Code')
    expect(words()).not.toContain('/mcp')
  })

  it('lets go of the key when the dialog closes, however it closes, and opens on a new form after', async () => {
    await signedIn(notYet, creates)
    for (const close of [() => click(find('dialog button', 'Done')), () => click(find('dialog button[aria-label=Close]'))]) {
      await click(createButton())
      await fill(find('dialog input[name=key-name]'), 'Claude Code')
      await submit(find('dialog form'))
      expect(words()).toContain(SECRET)
      await close()
      expect(dialog()).toBeNull()
      expect(words()).not.toContain(SECRET)
      expect(JSON.stringify(apiKeys)).not.toContain(SECRET)
    }
    await click(createButton())
    expect(find('dialog form')).not.toBeNull()
    expect(find('dialog .key-secret')).toBeNull()
  })

  it('keeps the key on screen through Escape or a tap beside the dialog, which close the form as any dialog', async () => {
    await signedIn(notYet, creates)
    // The form: Escape closes it.
    await click(createButton())
    expect(keydown({ key: 'Escape' }).defaultPrevented).toBe(false)
    await fire(dialog(), 'cancel')
    expect(dialog()).toBeNull()

    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    // Escape's keydown is held back, so the browser never asks to close; a cancel it sends anyway changes nothing.
    expect(keydown({ key: 'Escape' }).defaultPrevented).toBe(true)
    await fire(dialog(), 'cancel')
    // A tap on the backdrop is a press and a click on the <dialog> itself.
    await fire(dialog(), 'pointerdown')
    await fire(dialog(), 'click')
    expect(dialog()?.open).toBe(true)
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    await click(find('dialog button', 'Done'))
    expect(dialog()).toBeNull()
    expect(words()).not.toContain(SECRET)
  })

  it('offers Read and act only while actions are allowed, and says where to allow them', async () => {
    const fetch = await signedIn(notYet, creates)
    await click(createButton())
    expect(find('dialog input[value=write]')).toBeNull()
    expect(words(dialog()!)).toContain('To create a key that can also change messages, first allow actions on your messages in Account.')
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(posts(fetch).map(body => body.scope)).toEqual(['read'])
  })

  it('asks for a mailbox when the person chose to pick them and picked none', async () => {
    const fetch = await signedIn(notYet, creates)
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await check(find('dialog label.option', 'Only the ones I choose')!.querySelector('input'))
    await submit(find('dialog form'))
    expect(posts(fetch)).toEqual([])
    expect(words(find('dialog .alert')!)).toBe('Choose at least one mailbox.')
  })

  it('says why a key was not made in the console’s words, never the server’s', async () => {
    await signedIn(notYet, () => failure('not_found', 404))
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(words(find('dialog .alert')!)).toBe('A chosen mailbox cannot be given to a key: it was removed, or you no longer have access to it. Choose again.')
    expect(words()).not.toContain('hunter2')
    expect(find('dialog form')).not.toBeNull()
  })

  it('offers a reload instead of Create key when the terms changed while the page was open', async () => {
    const fetch = await signedIn(notYet, () => failure('conflict', 409))
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(words(find('dialog .alert')!)).toBe('The terms for API keys changed while this page was open. Reload the page to read the current text.')
    expect(find('dialog button', 'Reload page')).not.toBeNull()
    expect(find('dialog button[type=submit]')).toBeNull()
    // The text shown is not the one the server asks about now, so it no longer stands as the terms.
    expect(find('dialog .key-terms')).toBeNull()
    expect(posts(fetch)).toHaveLength(1)
  })

  it('offers no new key at the limit, and says why', async () => {
    keys = Array.from({ length: 20 }, (_, i) => listed(`a${String(i).padStart(15, '0')}`))
    await signedIn(notYet, creates)
    expect(createButton()!.disabled).toBe(true)
    expect(words()).toContain('You have 20 active keys, the most you can have. Revoke one to create another.')
  })
})

describe('the list', () => {
  it('shows a key made for mailboxes all removed since as reaching only those, never all of them', async () => {
    keys = [
      listed(PREFIX, { name: 'Script', account_ids: [], restricted: true, revoked_at: now() - 60 }),
      listed('0a0b0c0d0e0f0a0b', { name: 'Everything', account_ids: [], restricted: false }),
    ]
    await signedIn(notYet, () => failure('not_found', 404))
    expect(words(find('.key-card', 'Script')!)).toContain('Only mailboxes removed since')
    expect(words(find('.key-card', 'Script')!)).not.toContain('All your mailboxes')
    expect(words(find('.key-card', 'Everything')!)).toContain('All your mailboxes')
  })
})

describe('revoking a key', () => {
  it('asks first, and Cancel revokes nothing', async () => {
    keys = [listed(PREFIX, { name: 'Claude Code' })]
    const fetch = await signedIn(notYet, ({ path, method }) => {
      if (method === 'DELETE' && path === `/v1/me/apikeys/${PREFIX}`) {
        keys = [listed(PREFIX, { name: 'Claude Code', revoked_at: now() })]
        return new Response(null, { status: 204 })
      }
      return failure('not_found', 404)
    })
    await click(find('.key-card button', 'Revoke'))
    expect(words(dialog()!)).toContain('Claude Code stops working at once: a tool using it loses access to your mail at its next request. This cannot be undone.')
    expect(deletes(fetch)).toEqual([])
    await click(find('dialog button', 'Cancel'))
    expect(dialog()).toBeNull()
    expect(deletes(fetch)).toEqual([])
    await click(find('.key-card button', 'Revoke'))
    await click(find('dialog button.danger', 'Revoke key'))
    expect(deletes(fetch)).toEqual([`/v1/me/apikeys/${PREFIX}`])
    expect(dialog()).toBeNull()
    await flush()
    expect(words(find('.key-card .status-chip')!)).toBe('Revoked')
    expect(find('.key-card button', 'Revoke')).toBeNull()
    expect(words(find('.success')!)).toBe('Claude Code was revoked. A tool using it can no longer reach your mail.')
  })
})
