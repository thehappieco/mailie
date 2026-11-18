// Connecting a mailbox from the accounts section's buttons, as a person
// presses them, on a page (test/dom.ts): one button per provider the server
// offers, drawn once the workspaces are read, as one named group; Google's
// and Microsoft's, where the server finishes the link at the provider (the
// web flow), go straight to the provider's account chooser, with no address,
// no name and no workspace to choose; Microsoft keeps its address form one
// small link away, for a shared mailbox and an organization whose
// administrator must approve; a server that offers only the installed
// client's flow, and a browser that keeps nothing for the trip, ask for the
// address instead; and the mailbox this tab just connected asks for its
// nickname, which it may skip.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, fill, find, flush, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { FlowKind } from '../src/api/types'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import StepUpPrompt from '../src/components/StepUpPrompt.vue'
import { accounts, arrival, connect, finishOAuthReturn, loadAccounts, navigation, resetConnect } from '../src/state/accounts'
import { captureOAuthReturn, rememberOAuthStart } from '../src/state/oauthReturn'
import { adoptSession, signOut } from '../src/state/session'
import { stepUpPrompt } from '../src/state/stepUp'
import { account, failure, json, now, ORIGIN, reply, type Route, serve, stubPage } from './support'

interface Seen { path: string; method: string; body: unknown }
let seen: Seen[] = []
let mounted: Mounted[] = []
const navigate = vi.fn()
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path).map(request => request.body)
const CHOOSER = 'https://accounts.google.com/o/oauth2/auth?prompt=select_account+consent&state=x'
const existing = account({ id: 'acc_0000000000000001', email: 'ana@example.test', provider: 'imap', auth_kind: 'password', state: 'active' })

interface Options {
  flows?: FlowKind[]
  /** The mailboxes listed: one by default, so the buttons sit above the cards. */
  list?: ReturnType<typeof account>[]
  /** A route tried before the defaults. */
  route?: Route
  authenticatedAt?: number
}

/**
 * Ana signed in on a server without workspaces, whose Gmail and Microsoft
 * take these flows, with the accounts section shown. The server starts a
 * link, and creates a mailbox from an address, as the daemon would.
 */
async function section(options: Options = {}) {
  const flows = options.flows ?? ['web']
  serve(async request => {
    const { path, method, body } = request
    seen.push({ path, method, body })
    const answered = await options.route?.(request)
    if (answered) return answered
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/providers') {
      return json([
        { id: 'gmail', oauth: true, password: false, flows }, { id: 'microsoft', oauth: true, password: false, flows },
        { id: 'icloud', oauth: false, password: true, flows: [] }, { id: 'imap', oauth: false, password: true, flows: [] },
      ])
    }
    if (path === '/v1/accounts' && method === 'GET') return json(options.list ?? [existing])
    if (path === '/v1/accounts/oauth/links' && method === 'POST') {
      const provider = (body as { provider: string }).provider
      return json({ link: { id: 'lnk_0000000000000001', provider, state: 'waiting', expires_at: now() + 600 }, auth: { flow: 'web', auth_url: CHOOSER, state: 'x', expires_at: now() + 600 } }, 201)
    }
    if (path === '/v1/accounts' && method === 'POST') {
      const asked = body as { email: string; provider: 'gmail' | 'microsoft' }
      return json({ account: account({ id: 'acc_0000000000000002', provider: asked.provider, email: asked.email }), auth: { flow: flows[0], auth_url: CHOOSER, state: 'x', expires_at: now() + 600 } }, 201)
    }
    return failure('not_found', 404)
  })
  await adoptSession({ ...reply(), ...(options.authenticatedAt !== undefined ? { authenticated_at: options.authenticatedAt } : {}) })
  void loadAccounts()
  mounted.push(mount(StepUpPrompt), mount(AccountsPanel))
  await flush()
}
const button = (label: string) => find('.provider-button', label)

/**
 * What the page hears of the back/forward cache: the listeners the core
 * installs on pageshow, once for the whole file (state/accounts.ts installs
 * its own at the first attempt that leaves), which a spec sends.
 */
const pageshow: ((event: { persisted: boolean }) => void)[] = []
const restoreFromCache = () => { for (const listener of pageshow) listener({ persisted: true }) }

/**
 * The provider's return to this page for a consent of mailbox, which the
 * attempt that left created (the address form's), captured as the page loads
 * it: finishOAuthReturn then posts it.
 */
function returnFor(mailbox: ReturnType<typeof account>) {
  vi.stubGlobal('location', new URL(`${ORIGIN}/oauth/return?code=c0de&state=${'s'.repeat(43)}`))
  expect(rememberOAuthStart({ account_id: mailbox.id, provider: mailbox.provider, email: mailbox.email, created: true, back: '/' })).toBe(true)
  expect(captureOAuthReturn()).toBe(true)
  vi.stubGlobal('location', new URL(`${ORIGIN}/`))
}

beforeEach(() => {
  stubPage()
  seen = []
  navigate.mockReset()
  navigation.assign = navigate
  vi.spyOn(window, 'addEventListener').mockImplementation(((type: string, listener: (event: { persisted: boolean }) => void) => {
    if (type === 'pageshow') pageshow.push(listener)
  }) as typeof window.addEventListener)
})
afterEach(async () => {
  for (const each of mounted) each.unmount()
  mounted = []
  resetConnect()
  await signOut().catch(() => {})
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the buttons that connect a mailbox', () => {
  it('draws the buttons once, as one named group, each described by its line', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add a Google account')).not.toBeNull())
    expect(page.querySelectorAll('.connect-buttons')).toHaveLength(1)
    const group = find('.connect-buttons')!
    expect(group.getAttribute('role')).toBe('group')
    expect(words(page.querySelector(`[id="${group.getAttribute('aria-labelledby')}"]`)!)).toBe('Connect an email account')
    expect(group.querySelectorAll('.provider-button').map(item => words(item.querySelector('strong')!)))
      .toEqual(['Add a Google account', 'Add a Microsoft account', 'Add an iCloud account', 'Other server (IMAP)'])
    const google = button('Add a Google account')!
    // Named by its label alone, and described by its line: a screen reader says the line once.
    expect(words(page.querySelector(`[id="${google.getAttribute('aria-labelledby')}"]`)!)).toBe('Add a Google account')
    expect(words(page.querySelector(`[id="${google.getAttribute('aria-describedby')}"]`)!)).toBe('Gmail or Google Workspace')
    expect(group.querySelectorAll('[role=status]')).toHaveLength(1)
    expect(group.querySelectorAll('[role=alert]')).toHaveLength(1)
  })

  it('draws them once inside the first mailbox’s card while the workspace has none', async () => {
    await section({ list: [] })
    await vi.waitFor(() => expect(button('Add a Google account')).not.toBeNull())
    expect(page.querySelectorAll('.connect-buttons')).toHaveLength(1)
    expect(find('.empty-card .connect-buttons')).not.toBeNull()
  })

  it('draws no button before the workspaces are read, and says it is loading', async () => {
    await section({ route: ({ path }) => path === '/v1/workspaces' ? new Promise<Response>(() => {}) : undefined as never })
    await flush()
    expect(find('.provider-button')).toBeNull()
  })

  it('says the providers could not be read after a while, and tries again', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    let answer: (() => void) | undefined
    await section({ route: ({ path }) => path === '/v1/providers' && !answer
      ? new Promise<Response>(resolve => { answer = () => resolve(failure('unavailable', 503)) })
      : undefined as never })
    await vi.waitFor(() => expect(find('.connect-buttons')).not.toBeNull())
    expect(words(find('.connect-buttons')!)).toContain('Loading…')
    vi.advanceTimersByTime(15_000)
    await flush()
    expect(words(find('.connect-buttons [role=alert] span')!)).toBe('Could not check which providers this server connects. Try again in a moment.')
    await click(find('.connect-buttons button', 'Try again'))
    await vi.waitFor(() => expect(button('Add a Google account')).not.toBeNull())
    // The late answer of the first read changes nothing it did not know.
    answer!()
    await flush()
    expect(button('Add a Google account')).not.toBeNull()
  })

  it('goes from the Google button straight to Google’s account chooser, naming no address, no name and no workspace', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add a Google account')).not.toBeNull())
    await click(button('Add a Google account'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(new URL(CHOOSER).toString()))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([{ provider: 'gmail', flow: 'web' }])
    expect(sent('POST', '/v1/accounts')).toEqual([])
    expect(find('dialog')).toBeNull()
    expect(words(find('.connect-buttons [role=status]')!)).toBe('Opening Google…')
    expect(button('Add a Microsoft account')!.disabled).toBe(true)
  })

  it('asks for the step-up before leaving when the last sign-in is too old, and puts the buttons back when it is closed', async () => {
    await section({ authenticatedAt: now() - 6 * 60 })
    await vi.waitFor(() => expect(button('Add a Google account')).not.toBeNull())
    await click(button('Add a Google account'))
    await vi.waitFor(() => expect(stepUpPrompt.open).toBe(true))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
    await click(find('form[name=mailie-step-up] button', 'Cancel'))
    await flush()
    expect(connect.phase).toBe('idle')
    expect(button('Add a Google account')!.disabled).toBe(false)
    expect(navigate).not.toHaveBeenCalled()
  })

  it('keeps Microsoft’s address form one link away from its button, with the way back to the account chooser', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add a Microsoft account')).not.toBeNull())
    const instead = find('.connect-buttons .address-switch button', 'Enter the mailbox’s address instead')!
    expect(words(page.querySelector(`[id="${instead.getAttribute('aria-describedby')}"]`)!))
      .toBe('For a shared mailbox, or when Microsoft says your organization’s administrator must approve Mailie.')
    // Google's has none: a Google account has no shared mailbox to name.
    expect(page.querySelectorAll('.connect-buttons .address-switch')).toHaveLength(1)
    await click(instead)
    await flush()
    expect(find('dialog input[name=mailbox]')).not.toBeNull()
    expect(words(page.querySelector('dialog')!)).toContain('You will sign in at Microsoft 365 to let Mailie read and send mail for this address.')
    await fill(find('dialog input[name=mailbox]'), 'compartilhada@example.test')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(1))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
    expect(sent('POST', '/v1/accounts')).toEqual([expect.objectContaining({ email: 'compartilhada@example.test', provider: 'microsoft' })])
    expect(sent('POST', '/v1/accounts')[0]).not.toHaveProperty('display_name')
  })

  it('starts the account chooser from Microsoft’s address form when the person goes back to it', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add a Microsoft account')).not.toBeNull())
    await click(find('.connect-buttons .address-switch button', 'Enter the mailbox’s address instead'))
    await flush()
    await click(find('dialog .address-switch button', 'Use the account I choose at Microsoft'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(1))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([{ provider: 'microsoft', flow: 'web' }])
  })

  it('comes back to Microsoft’s address form when the chooser it went back to could not be started', async () => {
    await section({ route: ({ path, method }) => path === '/v1/accounts/oauth/links' && method === 'POST' ? failure('rate_limited', 429) : undefined as never })
    await vi.waitFor(() => expect(button('Add a Microsoft account')).not.toBeNull())
    await click(find('.connect-buttons .address-switch button', 'Enter the mailbox’s address instead'))
    await flush()
    await click(find('dialog .address-switch button', 'Use the account I choose at Microsoft'))
    await vi.waitFor(() => expect(find('dialog [role=alert]')).not.toBeNull())
    expect(words(find('dialog [role=alert]')!)).toBe('Too many connections were started. Wait a few minutes and try again.')
    await click(find('dialog .dialog-actions button', 'Try again'))
    await flush()
    expect(find('dialog input[name=mailbox]')).not.toBeNull()
  })

  it('shows Microsoft’s address form again, as it was filled, when the page comes back from the account chooser it went back to', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add a Microsoft account')).not.toBeNull())
    await click(find('.connect-buttons .address-switch button', 'Enter the mailbox’s address instead'))
    await flush()
    await fill(find('dialog input[name=mailbox]'), 'compartilhada@example.test')
    await click(find('dialog .address-switch button', 'Use the account I choose at Microsoft'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(1))
    // Back at Microsoft: the same page, from the cache. A link stored nothing, so its attempt is back where it began.
    restoreFromCache()
    await flush()
    expect(connect.phase).toBe('idle')
    expect(find('dialog input[name=mailbox]')?.value).toBe('compartilhada@example.test')
    expect(words(find('dialog')!)).not.toContain('Preparing the sign-in…')
  })

  it('keeps the mailbox Microsoft’s address form made when the page comes back from Microsoft: Try again goes back, and Close removes it', async () => {
    const web = { flow: 'web', auth_url: CHOOSER, state: 'x', expires_at: now() + 600 }
    await section({
      route: ({ path, method }) => {
        if (path === '/v1/accounts/acc_0000000000000002/oauth/start' && method === 'POST') return json(web)
        if (path === '/v1/accounts/acc_0000000000000002' && method === 'DELETE') return new Response(null, { status: 204 })
        return undefined as never
      },
    })
    await vi.waitFor(() => expect(button('Add a Microsoft account')).not.toBeNull())
    await click(find('.connect-buttons .address-switch button', 'Enter the mailbox’s address instead'))
    await flush()
    await fill(find('dialog input[name=mailbox]'), 'compartilhada@example.test')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(1))
    restoreFromCache()
    await flush()
    expect(words(find('dialog [role=alert]')!)).toBe('You came back from the provider before the authorization was finished. Try again to finish it.')
    // Not the form again, which the server would refuse for the address its pending mailbox holds.
    expect(find('dialog input[name=mailbox]')).toBeNull()
    await click(find('dialog .dialog-actions button', 'Try again'))
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledTimes(2))
    expect(sent('POST', '/v1/accounts')).toHaveLength(1)
    expect(sent('POST', '/v1/accounts/acc_0000000000000002/oauth/start')).toHaveLength(1)
    restoreFromCache()
    await flush()
    await click(find('dialog .dialog-actions button', 'Close'))
    await vi.waitFor(() => expect(sent('DELETE', '/v1/accounts/acc_0000000000000002')).toHaveLength(1))
    expect(find('dialog')).toBeNull()
  })

  it.each(['Add a Google account', 'Add a Microsoft account'])('asks for the address behind %s on a server with only the installed client’s flow', async label => {
    await section({ flows: ['loopback'] })
    await vi.waitFor(() => expect(button(label)).not.toBeNull())
    expect(find('.connect-buttons .address-switch')).toBeNull()
    await click(button(label))
    await flush()
    expect(find('dialog input[name=mailbox]')).not.toBeNull()
    await fill(find('dialog input[name=mailbox]'), 'suporte@example.test')
    await submit(find('dialog form'))
    await vi.waitFor(() => expect(sent('POST', '/v1/accounts')).toHaveLength(1))
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
  })

  it.each(['Add a Google account', 'Add a Microsoft account'])('asks for the address behind %s in a browser that keeps nothing for the trip', async label => {
    vi.stubGlobal('sessionStorage', { getItem() { throw new Error('denied') }, setItem() { throw new Error('denied') }, removeItem() { throw new Error('denied') } })
    await section()
    await vi.waitFor(() => expect(button(label)).not.toBeNull())
    await click(button(label))
    await flush()
    expect(find('dialog input[name=mailbox]')).not.toBeNull()
    expect(sent('POST', '/v1/accounts/oauth/links')).toEqual([])
  })

  it('opens the iCloud and IMAP forms straight away, with no step to choose the provider and no name to give', async () => {
    await section()
    await vi.waitFor(() => expect(button('Add an iCloud account')).not.toBeNull())
    await click(button('Add an iCloud account'))
    await flush()
    expect(words(find('dialog h2')!)).toBe('iCloud Mail')
    expect(find('dialog input[name=mailbox-password]')).not.toBeNull()
    expect(find('dialog .provider-option')).toBeNull()
    expect(find('dialog input[name=nickname]')).toBeNull()
  })

  it('shows the new mailbox with a nickname to fill or skip once the provider’s return connected it, and gives it the focus', async () => {
    const made = account({ id: 'acc_0000000000000009', email: 'ana.lima@gmail.com', state: 'active' })
    await section({ list: [existing, made] })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    arrival.accountID = made.id
    await flush()
    const card = find('.new-mailbox')!
    expect(words(card)).toContain('ana.lima@gmail.com is connected.')
    expect(page.activeElement).toBe(card.querySelector('.new-mailbox-heading'))
    expect(find('.new-mailbox input[name=nickname]')!.getAttribute('maxlength')).toBe('200')
    await click(find('.new-mailbox button', 'Skip'))
    expect(find('.new-mailbox')).toBeNull()
    expect(arrival.accountID).toBe('')
    expect(sent('PATCH', `/v1/accounts/${made.id}`)).toEqual([])
    await flush()
    expect(page.activeElement).toBe(find('.section-title h2'))
  })

  it('names the new mailbox on Done, and says a refusal under its field, keeping the card', async () => {
    const made = account({ id: 'acc_0000000000000009', email: 'ana.lima@gmail.com', state: 'active' })
    let refuse = true
    await section({
      list: [existing, made],
      route: ({ path, method, body }) => path === `/v1/accounts/${made.id}` && method === 'PATCH'
        ? (refuse ? failure('bad_request', 400) : json({ ...made, display_name: (body as { display_name: string }).display_name }))
        : undefined as never,
    })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    arrival.accountID = made.id
    await flush()
    await fill(find('.new-mailbox input[name=nickname]'), 'Vendas')
    await submit(find('.new-mailbox form'))
    await vi.waitFor(() => expect(find('.new-mailbox .alert')).not.toBeNull())
    expect(words(find('.new-mailbox .alert')!)).toBe('Mailie did not accept this nickname: use at most 200 characters, on one line.')
    const field = find('.new-mailbox input[name=nickname]')!
    expect(field.getAttribute('aria-describedby')).toContain(find('.new-mailbox .alert')!.getAttribute('id'))
    refuse = false
    await submit(find('.new-mailbox form'))
    await vi.waitFor(() => expect(find('.new-mailbox')).toBeNull())
    expect(sent('PATCH', `/v1/accounts/${made.id}`)).toEqual([{ display_name: 'Vendas' }, { display_name: 'Vendas' }])
    expect(accounts.list.find(item => item.id === made.id)!.display_name).toBe('Vendas')
  })
})

describe('a mailbox connected again', () => {
  it('leaves no notice behind once the new mailbox is done with, after Skip or Done', async () => {
    const made = account({ id: 'acc_0000000000000009', email: 'ana.lima@gmail.com', provider: 'microsoft', state: 'active', connected: true })
    await section({
      route: ({ path, method, body }) => {
        if (path === '/v1/accounts/oauth/callback' && method === 'POST') return json(made)
        if (path === `/v1/accounts/${made.id}` && method === 'PATCH') return json({ ...made, display_name: (body as { display_name: string }).display_name })
        return undefined as never
      },
    })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    for (const press of ['Skip', 'Done']) {
      returnFor(made)
      await finishOAuthReturn()
      await flush()
      // The card stands for the notice that says it connected.
      expect(find('.new-mailbox'), press).not.toBeNull()
      expect(find('.notice'), press).toBeNull()
      if (press === 'Done') {
        await fill(find('.new-mailbox input[name=nickname]'), 'Vendas')
        await submit(find('.new-mailbox form'))
      } else await click(find('.new-mailbox button', 'Skip'))
      await vi.waitFor(() => expect(find('.new-mailbox'), press).toBeNull())
      await flush()
      expect(find('.notice'), press).toBeNull()
      expect(accounts.notice, press).toBeNull()
    }
  })

  it('says a mailbox whose authorization was finished is connected, and asks nothing a new one is asked', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    const lapsed = account({ id: 'acc_0000000000000005', email: 'ana.lima@gmail.com', state: 'needs_reauth', connected: true, access: { read: true, act: true, send: true, manage: true } })
    let state: 'needs_reauth' | 'pending_auth' | 'active' = 'needs_reauth'
    await section({
      flows: ['loopback'],
      list: [lapsed],
      route: ({ path, method }) => {
        if (path === `/v1/accounts/${lapsed.id}/oauth/start` && method === 'POST') {
          state = 'pending_auth'
          return json({ flow: 'loopback', auth_url: CHOOSER, state: 'x', expires_at: now() + 600 })
        }
        if (path === `/v1/accounts/${lapsed.id}` && method === 'GET') {
          const answer = json({ ...lapsed, state })
          if (state === 'pending_auth') state = 'active'
          return answer
        }
        return undefined as never
      },
    })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    await click(find('.account-card button.authorize', 'Finish authorization'))
    await vi.waitFor(() => expect(connect.phase).toBe('waiting'))
    await vi.advanceTimersByTimeAsync(7_000)
    await vi.waitFor(() => expect(connect.phase).toBe('done'))
    await flush()
    expect(words(find('dialog')!)).toContain('ana.lima@gmail.com is connected.')
    expect(find('dialog .new-mailbox')).toBeNull()
    expect(find('dialog input[name=nickname]')).toBeNull()
    expect(find('dialog input[name=team-sync]')).toBeNull()
    await click(find('dialog .dialog-actions button', 'Done'))
    expect(find('dialog')).toBeNull()
  })
})

describe('naming a mailbox in its sheet', () => {
  it('renames a mailbox in its sheet, moving the focus into the field and back', async () => {
    const mine = { ...existing, access: { read: true, act: true, send: true, manage: true } }
    await section({
      list: [mine],
      route: ({ path, method, body }) => path === `/v1/accounts/${mine.id}` && method === 'PATCH'
        ? json({ ...mine, display_name: (body as { display_name: string }).display_name })
        : undefined as never,
    })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    const { openDetail } = await import('../src/state/accounts')
    openDetail(mine.id)
    await flush()
    expect(words(find('.sheet .nickname')!)).toContain('None')
    await click(find('.sheet .nickname button', 'Add'))
    const field = find('.sheet input[name=nickname]')!
    expect(page.activeElement).toBe(field)
    await fill(field, ' Pessoal ')
    await submit(find('.sheet .nickname-edit'))
    await vi.waitFor(() => expect(find('.sheet input[name=nickname]')).toBeNull())
    expect(sent('PATCH', `/v1/accounts/${mine.id}`)).toEqual([{ display_name: 'Pessoal' }])
    expect(words(find('.sheet .nickname')!)).toContain('Pessoal')
    expect(page.activeElement).toBe(find('.sheet .nickname button', 'Change'))
    // Removing it clears the name.
    await click(find('.sheet .nickname button', 'Remove'))
    await vi.waitFor(() => expect(sent('PATCH', `/v1/accounts/${mine.id}`)).toHaveLength(2))
    expect(sent('PATCH', `/v1/accounts/${mine.id}`)[1]).toEqual({ display_name: '' })
  })

  it('shows a member who does not manage the mailbox its nickname, and nothing to change it with', async () => {
    const shown = { ...existing, display_name: 'Suporte', access: { read: true, act: false, send: false, manage: false } }
    await section({ list: [shown] })
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    const { openDetail } = await import('../src/state/accounts')
    openDetail(shown.id)
    await flush()
    expect(words(find('.sheet .nickname')!)).toContain('Suporte')
    expect(find('.sheet .nickname button')).toBeNull()
  })
})
