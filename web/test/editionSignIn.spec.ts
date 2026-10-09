// The core's hooks for an edition whose people sign in another way: its own
// sign-in in place of the password card (Edition.signIn), the session its
// route answers adopted as a password sign-in's is (adoptSession), a word
// after a deliberate sign-out (Edition.signedOut), and no password to change
// for a person who has none. The open edition sets none of them, and its
// screens stay as they are.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, defineComponent, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import AccountPanel from '../src/components/AccountPanel.vue'
import SignInView from '../src/components/SignInView.vue'
import { configureEdition, type Edition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import OpenAccount from '../src/open/OpenAccount.vue'
import { session } from '../src/state/session'
import { ana, failure, json, reply, serve, settle, stubPage } from './support'

/** The page as a browser would get it, without scoped-style attributes and Vue's comment anchors. */
async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), context)
  return (html + Object.values(context.teleports ?? {}).join('')).replace(/ data-v-[0-9a-f]+/g, '').replace(/<!--[^]*?-->/g, '')
}

const ProviderSignIn = defineComponent({
  name: 'ProviderSignIn',
  render: () => h('div', { class: 'provider-sign-in' }, [h('h1', 'Continue with your provider'), h('button', { type: 'button' }, 'Continue')]),
})
const Legal = defineComponent({ name: 'Legal', render: () => h('a', { href: '/legal' }, 'The legal text') })
const invitation = { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' }

afterEach(() => {
  configureEdition(openEdition)
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0, notice: '' })
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('an edition’s own sign-in', () => {
  it('takes the password card’s place, in the same frame, with the brand above and the legal footer below', async () => {
    configureEdition({ ...openEdition, signIn: ProviderSignIn, legal: { signInFooter: Legal } })
    for (const props of [{ invitation: null }, { invitation }]) {
      const html = await render(SignInView, props)
      expect(html).toMatch(/<div class="unlock"><div class="unlock-card"><div class="auth-top">/)
      expect(html).toContain('class="lockup"')
      expect(html).toContain('appearance-trigger')
      expect(html).toContain('<div class="provider-sign-in"><h1>Continue with your provider</h1>')
      expect(html).toMatch(/<div class="auth-footer"><p class="auth-legal"><a href="\/legal">The legal text<\/a><\/p><\/div>/)
      // Nothing of the password card, and no invitation signs anyone up here.
      for (const card of ['name="password"', 'mailie-login', 'mailie-signup', 'Create your account', 'Accounts are created by invitation', 'Use my invitation']) {
        expect(html).not.toContain(card)
      }
    }
  })

  it('keeps the core’s notes about a session that ended and a mailbox waiting to finish connecting', async () => {
    configureEdition({ ...openEdition, signIn: ProviderSignIn })
    Object.assign(session, { notice: 'expired' })
    const html = await render(SignInView, { invitation: null })
    expect(html).toContain('Your session ended. Sign in again.')
    expect(html.indexOf('Your session ended')).toBeLessThan(html.indexOf('provider-sign-in'))
    // Without legal texts, nothing is drawn below it.
    expect(html).not.toContain('auth-footer')
  })

  it('is not there when the edition sets none: the open edition’s card signs in with a password, as before', async () => {
    for (const edition of [openEdition, { ...openEdition, signIn: undefined } satisfies Edition]) {
      configureEdition(edition)
      const html = await render(SignInView, { invitation: null })
      expect(html).toContain('<h1>Sign in</h1>')
      expect(html).toContain('name="mailie-login"')
      expect(html).toContain('name="password"')
      expect(html).toContain('Accounts are created by invitation. Ask the administrator of this server for a link.')
      expect(html).not.toContain('provider-sign-in')
      expect(await render(SignInView, { invitation })).toContain('Create your account')
    }
  })
})

describe('a person without a password', () => {
  const changePassword = 'Change the password you sign in with'

  it('is not offered to change one, and keeps everything else of the account section', async () => {
    Object.assign(session, { phase: 'ready', user: { ...ana, has_password: false }, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
    for (const component of [AccountPanel, OpenAccount]) {
      const html = await render(component)
      expect(html).not.toContain(changePassword)
      expect(html).not.toContain('mailie-password-change')
      expect(html).toContain('Sign out everywhere')
      expect(html).toContain(ana.email)
    }
  })

  it('is told apart from everyone else, who keeps the change, from a server that says so or one too old to', async () => {
    for (const user of [{ ...ana, has_password: true }, ana]) {
      Object.assign(session, { phase: 'ready', user, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
      expect(await render(OpenAccount)).toContain(changePassword)
    }
  })
})

describe('the session an edition’s sign-in adopts', () => {
  async function load(signedOut?: () => void) {
    vi.resetModules()
    const [{ configureEdition: configure }, { openEdition: open }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
    configure({ ...open, signedOut })
    const state = await import('../src/state/session')
    const vault = await import('../src/state/sessionVault')
    const { ApiError } = await import('../src/api/http')
    return { ...state, vault, ApiError }
  }

  beforeEach(() => { stubPage() })

  it('begins as a password sign-in’s does: the person, the expiry, this browser’s record, and the token for every call', async () => {
    const s = await load()
    s.showSignIn()
    const fetch = serve(() => failure('internal', 500))
    const answer = reply('tok_from_the_edition_000000000000000000000000', { ...ana, has_password: false })
    await s.adoptSession(answer)
    expect(s.session.phase).toBe('ready')
    expect(s.session.user).toEqual(answer.user)
    expect(s.session.expiresAt).toBe(answer.expires_at)
    expect(s.identity()).toBe(ana.id)
    expect((await s.vault.loadLocalSession())?.token).toBe(answer.token)
    const tokens: string[] = []
    await s.authorized(async token => { tokens.push(token) })
    expect(tokens).toEqual([answer.token])
    // Adopting asks the server nothing: the reply is what its route said.
    expect(fetch).not.toHaveBeenCalled()
  })

  it('replaces a login this browser had, as signing in again does', async () => {
    const s = await load()
    serve(() => json(reply()))
    await s.adoptSession(reply())
    const before = await s.vault.loadLocalSession()
    const cleared: string[] = []
    s.vault.observeLocalSession(change => cleared.push(change.id))
    await s.adoptSession(reply('tok_second_000000000000000000000000000000000'))
    const after = await s.vault.loadLocalSession()
    expect(after?.token).toBe('tok_second_000000000000000000000000000000000')
    expect(after?.id).not.toBe(before?.id)
    expect(s.session.phase).toBe('ready')
  })

  it('refuses a reply of any other shape, and nobody is signed in', async () => {
    const s = await load()
    s.showSignIn()
    for (const bad of [null, {}, { token: 'tok', expires_at: 1, user: { id: 'usr_1' } }, { ...reply(), token: 'has a space' }, { ...reply(), user: { ...ana, has_password: 'no' } }]) {
      await expect(s.adoptSession(bad)).rejects.toMatchObject({ code: 'invalid_response' })
      expect(s.session.phase).toBe('signed-out')
    }
    expect(await s.vault.loadLocalSession()).toBeNull()
  })
})

describe('the edition’s word after signing out', () => {
  async function load(signedOut: () => void) {
    vi.resetModules()
    const [{ configureEdition: configure }, { openEdition: open }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
    configure({ ...open, signedOut })
    const state = await import('../src/state/session')
    const vault = await import('../src/state/sessionVault')
    const { ApiError } = await import('../src/api/http')
    return { ...state, vault, ApiError }
  }

  beforeEach(() => { stubPage() })

  it('comes once the person signed out on purpose, after the session ended here and the server was told', async () => {
    for (const everywhere of [false, true]) {
      const seen: string[] = []
      const loaded: { state?: Awaited<ReturnType<typeof load>> } = {}
      const s = loaded.state = await load(() => { seen.push(`edition: ${loaded.state?.session.phase}`) })
      serve(({ path }) => {
        if (path === '/v1/auth/logout') seen.push('server told')
        return path === '/v1/auth/login' ? json(reply()) : new Response(null, { status: 204 })
      })
      await s.adoptSession(reply())
      await s.signOut({ everywhere })
      expect(seen, everywhere ? 'everywhere' : 'here').toEqual(['server told', 'edition: signed-out'])
      expect(await s.vault.loadLocalSession()).toBeNull()
      vi.restoreAllMocks()
    }
  })

  it('never comes when a session ends without the person asking, or a sign-out did not happen', async () => {
    const signedOut = vi.fn()
    const s = await load(signedOut)
    serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : failure('unauthorized', 401))

    // The server refused the token mid-use.
    await s.adoptSession(reply())
    await expect(s.authorized(async () => { throw new s.ApiError('unauthorized', 401) })).rejects.toMatchObject({ code: 'unauthorized' })
    await settle()
    expect(s.session.notice).toBe('expired')

    // Another tab cleared the login.
    await s.adoptSession(reply())
    const stored = await s.vault.loadLocalSession()
    await s.vault.clearLocalSession(stored!.id)
    expect(s.session.phase).toBe('signed-out')

    // Signing out everywhere could not reach the server: still signed in.
    vi.restoreAllMocks()
    serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : Promise.reject(new TypeError('offline')))
    await s.adoptSession(reply())
    await expect(s.signOut({ everywhere: true })).rejects.toMatchObject({ code: 'unavailable' })
    expect(s.session.phase).toBe('ready')

    expect(signedOut).not.toHaveBeenCalled()
  })
})
