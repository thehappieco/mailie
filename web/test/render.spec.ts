// Server-side renders of the components, for what the markup must and must
// not contain: a mailbox password never becomes part of the document, an
// invitation's or a reset link's code never shows, and the sign-in and
// sign-up cards ask nobody to accept a company's terms.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import AccountFields from '../src/components/AccountFields.vue'
import ConnectButtons from '../src/components/ConnectButtons.vue'
import SignInView from '../src/components/SignInView.vue'
import { accounts, connect } from '../src/state/accounts'
import { session } from '../src/state/session'
import { workspaces } from '../src/state/workspaces'
import { emptyDraft } from '../src/ui/accountDraft'
import { locale } from '../src/ui/i18n'
import { ana } from './support'

const literal = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
/** A provider choice's label, as rendered. */
const choiceLabel = (label: string) => `<strong[^>]*>${literal(label)}</strong>`
/** A provider's button whose label is `label`, as rendered. */
function button(html: string, label: string): string {
  return html.match(new RegExp(`<button[^>]*class="provider-button"[^>]*>(?:(?!</button>).)*${choiceLabel(label)}(?:(?!</button>).)*</button>`, 's'))?.[0] ?? ''
}

/** The page as a browser would get it: the app plus whatever it teleported to <body>. */
async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), context)
  return html + Object.values(context.teleports ?? {}).join('')
}

function signIn(user = ana) {
  Object.assign(session, { phase: 'ready', user, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
}

afterEach(() => {
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0 })
  Object.assign(accounts, { list: [], loaded: false, loading: false, detailID: '', folders: {}, notice: null, providers: [], providersLoaded: false })
  Object.assign(connect, { phase: 'idle', provider: '', email: '', accountID: '', created: false, flow: '', failure: null })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  locale.value = 'en'
  vi.unstubAllGlobals()
})

/** The text of the element matched, without its markup. */
const text = (html: string) => html.replace(/<!--[^]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

describe('what the console renders', () => {
  it('never writes the mailbox password into the add dialog’s markup', async () => {
    const draft = { ...emptyDraft(), email: 'vendas@example.test', password: 'app-password-123', imapHost: 'imap.example.test', smtpHost: 'smtp.example.test' }
    const html = await render(AccountFields, { provider: 'imap', draft })
    expect(html).toContain('vendas@example.test')
    expect(html).not.toContain('app-password-123')
    const password = html.match(/<input[^>]*name="mailbox-password"[^>]*>/)?.[0]
    expect(password).toBeDefined()
    expect(password).not.toContain('value=')
    expect(password).toContain('autocomplete="off"')
  })

  it('draws a button for each provider this server connects, and none for the others', async () => {
    signIn()
    Object.assign(workspaces, { list: [], loaded: true, supported: false, currentID: '' })
    Object.assign(accounts, {
      providersLoaded: true,
      providers: [
        { id: 'gmail', oauth: true, password: false, flows: [] },
        { id: 'microsoft', oauth: true, password: false, flows: ['web'] },
        { id: 'icloud', oauth: false, password: true, flows: [] },
        { id: 'imap', oauth: false, password: true, flows: [] },
      ],
    })
    const html = await render(ConnectButtons)
    expect(html).not.toContain('Add a Google account')
    expect(html).not.toContain('Not configured on this server')
    for (const label of ['Add a Microsoft account', 'Add an iCloud account', 'Other server (IMAP)']) expect(button(html, label), label).not.toBe('')
    Object.assign(accounts, { providers: [] })
    expect(await render(ConnectButtons)).toContain('This server has no way to connect a mailbox yet.')
  })

  it('lists Google, Microsoft, iCloud and IMAP in that order, each described, as one named group', async () => {
    signIn()
    Object.assign(workspaces, { list: [], loaded: true, supported: false, currentID: '' })
    Object.assign(accounts, {
      providersLoaded: true,
      providers: [
        { id: 'gmail', oauth: true, password: false, flows: ['web'] },
        { id: 'microsoft', oauth: true, password: false, flows: ['web'] },
        { id: 'icloud', oauth: false, password: true, flows: [] },
        { id: 'imap', oauth: false, password: true, flows: [] },
      ],
    })
    const html = await render(ConnectButtons)
    const order = ['Add a Google account', 'Add a Microsoft account', 'Add an iCloud account', 'Other server (IMAP)'].map(label => html.search(new RegExp(choiceLabel(label))))
    expect(order).not.toContain(-1)
    expect([...order].sort((a, b) => a - b)).toEqual(order)
    expect(button(html, 'Add an iCloud account')).toMatch(/aria-describedby="[^"]+"/)
    expect(html).toContain('icloud.com, me.com, mac.com or an iCloud+ domain, with an app-specific password')
    // One group, named by its visible heading.
    const heading = html.match(/<h3 id="([^"]+)"[^>]*>Connect an email account<\/h3>/)
    expect(heading).not.toBeNull()
    expect(html).toContain(`role="group" aria-labelledby="${heading![1]}"`)
    expect(html.match(/role="status"/g)).toHaveLength(1)
  })

  it('asks iCloud for the address and an app-specific password, no servers, and never writes the password into the markup', async () => {
    const draft = { ...emptyDraft(), email: 'ana@icloud.com', password: 'abcd-efgh-ijkl-mnop', imapHost: 'imap.elsewhere.example', smtpHost: 'smtp.elsewhere.example' }
    const html = await render(AccountFields, { provider: 'icloud', draft })
    expect(html).toContain('ana@icloud.com')
    expect(html).not.toContain('abcd-efgh-ijkl-mnop')
    const password = html.match(/<input[^>]*name="mailbox-password"[^>]*>/)?.[0]
    expect(password).toBeDefined()
    expect(password).not.toContain('value=')
    expect(password).toContain('autocomplete="off"')
    for (const server of ['imap-host', 'imap-port', 'smtp-host', 'smtp-port', 'smtp-tls', 'Incoming mail', 'Outgoing mail', 'Login name', 'elsewhere.example']) expect(html).not.toContain(server)
    expect(html).toContain('App-specific password')
    expect(html).toContain('Two-factor authentication must be on')
    expect(html).toContain('account.apple.com → Sign-In and Security → App-Specific Passwords')
    const help = html.match(/<a[^>]*href="https:\/\/support\.apple\.com\/102654"[^>]*>/)?.[0]
    expect(help).toContain('target="_blank"')
    expect(help).toContain('rel="noopener noreferrer"')
  })

  it('lets an iCloud+ custom domain name the iCloud address it signs in with, as an address', async () => {
    const html = await render(AccountFields, { provider: 'icloud', draft: { ...emptyDraft(), email: 'ana@lima.example', loginUser: 'ana@icloud.com' } })
    const field = html.match(/<input[^>]*id="[^"]*-icloud-login"[^>]*>/)?.[0]
    expect(field).toBeDefined()
    expect(field).toContain('type="email"')
    expect(field).toContain('value="ana@icloud.com"')
    expect(field).not.toContain('required')
    const hint = field!.match(/aria-describedby="([^"]+)"/)?.[1]
    expect(html).toContain(`id="${hint}"`)
    expect(html).toContain('iCloud address to sign in with (optional)')
    expect(html).toContain('Only for a custom domain on iCloud+')
    // Generic IMAP keeps its own login field, named for any server.
    const imap = await render(AccountFields, { provider: 'imap', draft: emptyDraft() })
    expect(imap).not.toContain('-icloud-login')
    expect(imap).toContain('Login name (optional)')
  })

  it('points an Apple address typed into the IMAP form at iCloud, but only where the server offers iCloud', async () => {
    const draft = { ...emptyDraft(), email: 'ana@me.com' }
    expect(await render(AccountFields, { provider: 'imap', draft, offerIcloud: true })).toContain('Use iCloud Mail')
    expect(await render(AccountFields, { provider: 'imap', draft, offerIcloud: false })).not.toContain('Use iCloud Mail')
    expect(await render(AccountFields, { provider: 'imap', draft: { ...draft, email: 'ana@example.com' }, offerIcloud: true })).not.toContain('Use iCloud Mail')
    expect(await render(AccountFields, { provider: 'icloud', draft, offerIcloud: true })).not.toContain('Use iCloud Mail')
  })

  it('never names a password field after its show/hide button', async () => {
    const pages = [
      await render(AccountFields, { provider: 'imap', draft: emptyDraft() }),
      await render(AccountFields, { provider: 'icloud', draft: emptyDraft() }),
      await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } }),
    ]
    for (const html of pages) {
      expect(html).toContain('password-toggle')
      for (const label of html.match(/<label\b(?:(?!<\/label>).)*<\/label>/gs) ?? []) expect(label).not.toContain('<button')
    }
  })

  it('shows an invitation’s address but never its code', async () => {
    const html = await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } })
    expect(html).toContain('new@example.test')
    expect(html).toContain('readonly')
    expect(html).not.toContain('SyntheticInviteCode')
    expect(html).toContain('Create your account')
  })

  it('shows a reset link’s address but never its code, and asks for a new password of twelve characters twice', async () => {
    const html = await render(SignInView, { invitation: null, reset: { reset: 'SyntheticResetCode_0123456789abcdefghijklmnop', email: 'ana@example.test' } })
    expect(html).toContain('ana@example.test')
    expect(html).toContain('readonly')
    expect(html).not.toContain('SyntheticResetCode')
    expect(text(html)).toContain('Choose a new password')
    expect(html.match(/minlength="12"/g)).toHaveLength(2)
    expect(html).toContain('autocomplete="new-password"')
    // What saving stops besides the sessions, said before the button that confirms it, and tied to it.
    expect(text(html)).toContain('Saving stops every API key this account created. You can create new ones once you are signed in.')
    expect(html.indexOf('id="keys-stop"')).toBeLessThan(html.indexOf('type="submit"'))
    expect(html).toContain('aria-describedby="keys-stop"')
  })

  it('offers recovery with the code from the sign-in card, and asks a new password for twelve characters', async () => {
    const signin = await render(SignInView, { invitation: null })
    expect(text(signin)).toContain('Forgot your password?')
    expect(signin).not.toContain('minlength')
    expect(text(signin)).not.toContain('API key')
    const signup = await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } })
    expect(text(signup)).toContain('Use at least 12 characters.')
    expect(signup.match(/minlength="12"/g)).toHaveLength(2)
  })

  it('asks nobody to accept a company’s terms, and links no policy, on the sign-in and sign-up cards', async () => {
    locale.value = 'pt'
    const signup = await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } })
    expect(signup).not.toContain('class="consent"')
    const signin = await render(SignInView, { invitation: null })
    for (const html of [signup, signin]) {
      expect(html).not.toContain('auth-legal')
      expect(html).not.toMatch(/<a[^>]*href=/)
      expect(html).not.toMatch(/thehappie|Happie|Política de Privacidade|Termos de Uso/)
    }
  })

  it('renders in the chosen language', async () => {
    locale.value = 'pt'
    const html = await render(SignInView, { invitation: null })
    expect(html).toContain('Entrar')
    expect(html).toContain('As contas são criadas por convite.')
  })
})

describe('what a host’s own frame changes', () => {
  it('draws the sign-in card in a host’s frame without the brand or the appearance control, and keeps its notes', async () => {
    Object.assign(session, { phase: 'signed-out', notice: 'expired' })
    const own = await render(SignInView, { invitation: null })
    expect(own).toContain('class="unlock"')
    expect(own).toContain('class="lockup"')
    expect(own).toContain('appearance-trigger')
    const framed = await render(SignInView, { invitation: null, framed: true })
    expect(framed).toMatch(/class="unlock framed"/)
    expect(framed).not.toContain('auth-top')
    expect(framed).not.toContain('class="lockup"')
    expect(framed).not.toContain('appearance-trigger')
    expect(text(framed)).toContain('Sign in')
    expect(text(framed)).toContain('Your session ended. Sign in again.')
    expect(framed).toContain('name="mailie-login"')
    Object.assign(session, { notice: '' })
  })
})
