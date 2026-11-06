// Linking a Gmail or Microsoft mailbox at the provider, as a person presses
// it, on a page (test/dom.ts): where the server finishes the link at the
// provider (the web flow), the form asks for no address, says the provider
// names it, and starts a link that carries none; Microsoft keeps its address
// form one small link away, for a shared mailbox and an organization whose
// administrator must approve, with the way back; and a server that offers
// only the installed client's flow asks for the address, as before.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, fill, find, flush, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { FlowKind, ProviderID } from '../src/api/types'
import AddAccountDialog from '../src/components/AddAccountDialog.vue'
import { loadAccounts, loadProviders, navigation } from '../src/state/accounts'
import { adoptSession, signOut } from '../src/state/session'
import { account, failure, json, now, reply, serve, stubPage } from './support'

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
let mounted: Mounted | null = null
const navigate = vi.fn()
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path).map(request => request.body)
const CHOOSER = 'https://accounts.google.com/o/oauth2/auth?prompt=select_account+consent&state=x'

/**
 * Ana signed in on a server with these flows for Gmail and Microsoft, and
 * the connecting dialog open on the provider chosen. The server starts a
 * link, and creates a mailbox from an address, as the daemon would.
 */
async function choosing(provider: ProviderID, flows: FlowKind[]) {
  serve(({ path, method, body }) => {
    seen.push({ path, method, body })
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/providers') return json([{ id: 'gmail', oauth: true, password: false, flows }, { id: 'microsoft', oauth: true, password: false, flows }])
    if (path === '/v1/accounts' && method === 'GET') return json([])
    if (path === '/v1/accounts/oauth/links' && method === 'POST') {
      return json({ link: { id: 'lnk_0000000000000001', provider, state: 'waiting', expires_at: now() + 600 }, auth: { flow: 'web', auth_url: CHOOSER, state: 'x', expires_at: now() + 600 } }, 201)
    }
    if (path === '/v1/accounts' && method === 'POST') {
      return json({ account: account({ provider, email: (body as { email: string }).email }), auth: { flow: flows[0], auth_url: CHOOSER, state: 'x', expires_at: now() + 600 } }, 201)
    }
    return failure('not_found', 404)
  })
  await adoptSession(reply())
  await Promise.all([loadAccounts(), loadProviders()])
  mounted = mount(AddAccountDialog, { resume: null })
  await flush()
  await click(find('dialog .provider-option', provider === 'gmail' ? 'Gmail or Google Workspace' : 'Microsoft 365 or Outlook'))
}

beforeEach(() => {
  stubPage()
  seen = []
  navigate.mockReset()
  navigation.assign = navigate
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('connecting Gmail or Microsoft at the provider', () => {
  it.each([
    ['gmail', 'You will choose the account on Google’s page, and Google tells Mailie its address.'],
    ['microsoft', 'You will choose the account on Microsoft’s page, and Microsoft tells Mailie its address.'],
  ] as const)('draws no address field for %s where the server finishes the link at the provider, and starts one that carries none', async (provider, subtitle) => {
    await choosing(provider, ['web'])
    expect(find('dialog input[name=mailbox]')).toBeNull()
    expect(words(page.querySelector('dialog')!)).toContain(subtitle)
    await fill(find('dialog input[maxlength="120"]'), 'Vendas')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(new URL(CHOOSER).toString()))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([{ provider, flow: 'web', display_name: 'Vendas' }])
    expect(sent('POST', '/v1/accounts')).toEqual([])
  })

  it('offers Gmail no address form: a Google account has no shared mailbox to name', async () => {
    await choosing('gmail', ['web'])
    expect(find('dialog .address-switch')).toBeNull()
    expect(words(page.querySelector('dialog')!)).not.toContain('Enter the mailbox’s address instead')
  })

  it('offers Microsoft’s address form one small link away, for a shared mailbox, and the way back to the account chooser', async () => {
    await choosing('microsoft', ['web'])
    const instead = find('dialog .address-switch button', 'Enter the mailbox’s address instead')!
    expect(instead).not.toBeNull()
    // Said beside it, and read with it.
    const hint = page.querySelector(`[id="${instead.getAttribute('aria-describedby')}"]`)
    expect(words(hint!)).toBe('For a shared mailbox, or when Microsoft says your organization’s administrator must approve Mailie.')
    await click(instead)
    const address = find('dialog input[name=mailbox]')
    expect(address).not.toBeNull()
    expect(page.activeElement).toBe(address)
    expect(words(page.querySelector('dialog')!)).toContain('You will sign in at Microsoft 365 to let Mailie read and send mail for this address.')
    await click(find('dialog .address-switch button', 'Use the account I choose at Microsoft'))
    expect(find('dialog input[name=mailbox]')).toBeNull()
    expect(page.activeElement).toBe(find('dialog .address-switch button', 'Enter the mailbox’s address instead'))
    // Through the address form: today's link, with the address, the mailbox created at once.
    await click(find('dialog .address-switch button', 'Enter the mailbox’s address instead'))
    await fill(find('dialog input[name=mailbox]'), 'compartilhada@example.test')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(1))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
    expect(sent('POST', '/v1/accounts')).toEqual([expect.objectContaining({ email: 'compartilhada@example.test', provider: 'microsoft' })])
  })

  it.each(['gmail', 'microsoft'] as const)('asks %s for the address where the server offers only the installed client’s flow', async provider => {
    await choosing(provider, ['loopback'])
    expect(find('dialog input[name=mailbox]')).not.toBeNull()
    expect(find('dialog .address-switch')).toBeNull()
    expect(words(page.querySelector('dialog')!)).toContain('to let Mailie read and send mail for this address.')
    await fill(find('dialog input[name=mailbox]'), 'suporte@example.test')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts')).toHaveLength(1))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
  })
})
