// Server-side renders of the components, for what the markup must and must
// not contain: text a mailbox or a person chose is text, never markup, and a
// mailbox password never becomes part of the document.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import AccountFields from '../src/components/AccountFields.vue'
import AccountSheet from '../src/components/AccountSheet.vue'
import AccountsPanel from '../src/components/AccountsPanel.vue'
import AddAccountDialog from '../src/components/AddAccountDialog.vue'
import KeysPanel from '../src/components/KeysPanel.vue'
import OpenAccount from '../src/open/OpenAccount.vue'
import SignInView from '../src/components/SignInView.vue'
import { accounts, connect } from '../src/state/accounts'
import { session } from '../src/state/session'
import { actionsConsent } from '../src/state/actionsConsent'
import { apiKeys } from '../src/state/apikeys'
import { mcpAccess } from '../src/state/mcp'
import { consent, syncRequests } from '../src/state/sync'
import { workspaces } from '../src/state/workspaces'
import { emptyDraft } from '../src/ui/accountDraft'
import { locale } from '../src/ui/i18n'
import { ACTIONS_TEXT_VERSION, KEY_TERMS_VERSION, SYNC_TEXT_VERSION } from '../src/open/versions'
import { REASON_MAILBOX_REFUSED, REASON_NOT_GRANTED, REASON_TOKEN_REJECTED } from '../src/ui/reasons'
import type { Workspace } from '../src/api/types'
import { account, ana, syncing } from './support'

const hostile = '<img src=x onerror=alert(1)>'

const literal = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
/** A provider choice's label, as rendered. */
const choiceLabel = (label: string) => `<strong[^>]*>${literal(label)}</strong>`
/** The provider choice's button whose label is `label`, as rendered. */
function option(html: string, label: string): string {
  return html.match(new RegExp(`<button[^>]*class="provider-option"[^>]*>(?:(?!</button>).)*${choiceLabel(label)}(?:(?!</button>).)*</button>`, 's'))?.[0] ?? ''
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
  Object.assign(consent, { loaded: false, consented: false, version: '', consentedAt: 0, currentVersion: '', dismissed: false, busy: '', problem: null, failure: null })
  for (const id of Object.keys(syncRequests)) delete syncRequests[id]
  Object.assign(actionsConsent, { loaded: false, consented: false, version: '', consentedAt: 0, currentVersion: '', dismissed: false, busy: '', problem: null, failure: null })
  Object.assign(apiKeys, { list: [], loaded: false, loading: false, failure: null, revoking: '' })
  Object.assign(mcpAccess, { loaded: false, loading: false, served: false })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  locale.value = 'en'
  vi.unstubAllGlobals()
})

const VERSION = SYNC_TEXT_VERSION
/** The person has not answered: the daemon's contract/sync_consent.json. */
function notConsented() { Object.assign(consent, { loaded: true, consented: false, version: '', currentVersion: VERSION }) }
function consented() { Object.assign(consent, { loaded: true, consented: true, version: VERSION, consentedAt: 1_790_000_000, currentVersion: VERSION }) }
/** The text of the element matched, without its markup. */
const text = (html: string) => html.replace(/<!--[^]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

/** The switch of the actions row, or '' when the row has none. */
const actionsSwitch = (html: string) => html.match(/<div class="security-summary sync-row"(?:(?!<\/div>).)*Actions on my messages(?:(?!<\/div>).)*?(<button class="sync-switch"[^>]*>)/s)?.[1] ?? ''

/** The text of the first element with role="alert", without its markup. */
const alertText = (html: string) => html.match(/<p[^>]*role="alert"[^>]*>((?:(?!<\/p>).)*)<\/p>/s)?.[1]?.replace(/<[^>]+>/g, '').trim()

describe('what the console renders', () => {
  it('shows mailbox names and addresses as text, on the cards and in the sheet', async () => {
    signIn()
    const evil = account({ id: 'acc_evil', email: `x"><script>alert(1)</script>@example.test`, display_name: hostile, state: 'needs_reauth' })
    Object.assign(accounts, { list: [evil], loaded: true, detailID: 'acc_evil' })
    const html = await render(AccountsPanel)
    expect(html).not.toContain('<img')
    expect(html).not.toContain('<script')
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;')
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;')
    expect(html).toContain('Finish authorization')
    expect(html).toContain('Remove account')
  })

  it('never renders what the server said about an account', async () => {
    signIn()
    Object.assign(accounts, { list: [account({ state: 'error', state_reason: 'invalid_grant for user token=abc', last_error: 'upstream: 535 5.7.8 password=hunter2' })], loaded: true, detailID: 'acc_0000000000000001' })
    const html = await render(AccountsPanel)
    expect(html).not.toContain('invalid_grant')
    expect(html).not.toContain('hunter2')
    expect(html).toContain('The last attempt to connect failed.')
  })

  it('tells the person, in their language, what to do when the provider left the mailbox out of the grant', async () => {
    signIn()
    locale.value = 'pt'
    const failing = account({ state: 'error', state_reason: REASON_NOT_GRANTED })
    Object.assign(accounts, { list: [failing], loaded: true })
    Object.assign(connect, { phase: 'failed', provider: 'gmail', email: failing.email, accountID: failing.id, created: true, flow: 'loopback',
      failure: { op: 'wait-auth', code: 'flow_failed', account: { provider: 'gmail', email: failing.email, state: 'error', state_reason: REASON_NOT_GRANTED } } })
    const html = await render(AddAccountDialog, { resume: null })
    expect(alertText(html)).toBe('O Google não deu ao Mailie acesso a esta caixa de email. Tente novamente e, na tela do Google, permita o acesso ao Gmail (marque a caixa, se aparecer uma).')
    expect(html).toContain('Tentar novamente')
    expect(html).not.toContain(REASON_NOT_GRANTED)
  })

  it('names the account to choose when a re-authorization was refused, in the finish-authorization dialog', async () => {
    signIn()
    const refused = account({ provider: 'microsoft', email: 'ana.souza@example.test', state: 'error', state_reason: REASON_MAILBOX_REFUSED })
    Object.assign(accounts, { list: [refused], loaded: true })
    Object.assign(connect, { phase: 'failed', provider: 'microsoft', email: refused.email, accountID: refused.id, flow: 'loopback',
      failure: { op: 'wait-auth', code: 'flow_failed', account: { provider: 'microsoft', email: refused.email, state: 'error', state_reason: REASON_MAILBOX_REFUSED } } })
    const html = await render(AddAccountDialog, { resume: refused })
    expect(html).toContain('Finish authorization')
    expect(alertText(html)).toBe('The mail server refused this authorization. Try again and choose the account ana.souza@example.test on Microsoft’s sign-in screen. If you already did, check that IMAP is turned on for this mailbox.')
    expect(html).toContain('Try again')
  })

  it('explains a mailbox that refused its grant on the card and in the sheet, beside the button that fixes it', async () => {
    signIn()
    const refused = account({ email: 'ana@gmail.example', state: 'needs_reauth', state_reason: REASON_TOKEN_REJECTED })
    Object.assign(accounts, { list: [refused], loaded: true, detailID: refused.id,
      folders: { [refused.id]: { loading: false, loaded: false, list: [], failure: { op: 'folders', code: 'conflict' } } } })
    const html = await render(AccountsPanel)
    const text = 'The mail server refused this authorization. Try again and choose the account ana@gmail.example on Google’s sign-in screen.'
    // Once on the card, once in the sheet's status.
    expect(html.split(text)).toHaveLength(3)
    expect(html.match(/Finish authorization/g)).toHaveLength(2)
    expect(html).toContain('This account needs to be authorized again before its folders can be listed.')
    expect(html).not.toContain('access token')
  })

  it('puts the reason in the notice a failed return leaves at the top of the accounts', async () => {
    signIn()
    locale.value = 'pt'
    const failing = account({ state: 'error', state_reason: REASON_NOT_GRANTED })
    Object.assign(accounts, { list: [failing], loaded: true,
      notice: { kind: 'failed', email: failing.email, failure: { op: 'complete-auth', code: 'bad_request', account: { provider: 'gmail', email: failing.email, state: 'error', state_reason: REASON_NOT_GRANTED } } } })
    const html = await render(AccountsPanel)
    const notice = html.match(/<div[^>]*class="alert notice"[^>]*>(?:(?!<\/div>).)*<\/div>/s)?.[0] ?? ''
    expect(notice).toContain('role="alert"')
    expect(notice.replace(/<[^>]+>/g, '').trim()).toBe('suporte@example.test não foi conectada. O Google não deu ao Mailie acesso a esta caixa de email. Tente novamente e, na tela do Google, permita o acesso ao Gmail (marque a caixa, se aparecer uma).')
    expect(html).toContain('Concluir autorização')
  })

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

  it('offers only the providers this server can connect, and says why the others are off', async () => {
    signIn()
    Object.assign(accounts, {
      providersLoaded: true,
      providers: [
        { id: 'gmail', oauth: true, password: false, flows: [] },
        { id: 'microsoft', oauth: true, password: false, flows: ['web'] },
        { id: 'icloud', oauth: false, password: true, flows: [] },
        { id: 'imap', oauth: false, password: true, flows: [] },
      ],
    })
    const html = await render(AddAccountDialog, { resume: null })
    const gmail = option(html, 'Gmail or Google Workspace')
    expect(gmail).toContain('disabled')
    expect(gmail).toContain('Not configured on this server')
    expect(html.match(/Not configured on this server/g)).toHaveLength(1)
  })

  it('lists iCloud between Microsoft and IMAP, and closes it on a server that does not offer it', async () => {
    signIn()
    const listed = [
      { id: 'gmail', oauth: true, password: false, flows: ['web'] },
      { id: 'microsoft', oauth: true, password: false, flows: ['web'] },
      { id: 'icloud', oauth: false, password: true, flows: [] },
      { id: 'imap', oauth: false, password: true, flows: [] },
    ]
    Object.assign(accounts, { providersLoaded: true, providers: listed })
    let html = await render(AddAccountDialog, { resume: null })
    const order = ['Gmail or Google Workspace', 'Microsoft 365 or Outlook', 'iCloud Mail', 'Other provider (IMAP)'].map(label => html.search(new RegExp(choiceLabel(label))))
    expect(order).not.toContain(-1)
    expect([...order].sort((a, b) => a - b)).toEqual(order)
    expect(option(html, 'iCloud Mail')).not.toContain('disabled')
    expect(option(html, 'iCloud Mail')).toContain('app-specific password')
    Object.assign(accounts, { providers: listed.filter(provider => provider.id !== 'icloud') })
    html = await render(AddAccountDialog, { resume: null })
    expect(option(html, 'iCloud Mail')).toContain('disabled')
    expect(option(html, 'iCloud Mail')).toContain('Not configured on this server')
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

  it('names an iCloud account iCloud on its card and in its sheet', async () => {
    signIn()
    const icloud = account({ email: 'ana@icloud.com', provider: 'icloud', auth_kind: 'password', state: 'active', save_sent_copy: true })
    Object.assign(accounts, { list: [icloud], loaded: true, detailID: icloud.id })
    const html = await render(AccountsPanel)
    expect(html.match(/<dd[^>]*>iCloud<\/dd>/g)).toHaveLength(2)
    expect(html).not.toContain('Other (IMAP)')
    expect(html).toContain('Saved in the Sent folder')
  })

  it('claims no syncing or incoming mail for an account whose owner has not turned sync on', async () => {
    signIn()
    // What the daemon wrote about the connection says nothing about sync.
    Object.assign(accounts, { list: [account({ state: 'active', sync_tier: 'condstore', last_ok_at: Math.floor(Date.now() / 1000) - 180 })], loaded: true, detailID: 'acc_0000000000000001' })
    const html = await render(AccountsPanel)
    for (const claim of ['Syncing', 'receiving new mail', 'Sync mode', 'Last sync', 'Messages indexed', 'Sync now', 'first sync', 'Incremental (CONDSTORE)', 'Mail sync is not available yet']) expect(html).not.toContain(claim)
    expect(html).toContain('Active accounts')
    expect(html).toContain('Connected. Sync is off.')
    expect(html).toContain('Sync is off. Nothing from this mailbox is stored.')
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

  it('tells a password account whose sign-in was refused to reconnect it, not to authorize it', async () => {
    signIn()
    const imap = account({ provider: 'imap', auth_kind: 'password', state: 'active' })
    Object.assign(accounts, { list: [imap], folders: { [imap.id]: { loading: false, loaded: false, list: [], failure: { op: 'folders', code: 'conflict' } } } })
    const html = await render(AccountSheet, { account: imap })
    expect(html).toContain('did not accept the saved password')
    expect(html).not.toContain('authorized again')
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
  })

  it('offers recovery with the code from the sign-in card, and asks a new password for twelve characters', async () => {
    const signin = await render(SignInView, { invitation: null })
    expect(text(signin)).toContain('Forgot your password?')
    expect(signin).not.toContain('minlength')
    const signup = await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } })
    expect(text(signup)).toContain('Use at least 12 characters.')
    expect(signup.match(/minlength="12"/g)).toHaveLength(2)
  })

  it('offers an enrolled person their password and recovery code, and sends one who never enrolled to the administrator for a reset link', async () => {
    signIn()
    const enrolledHTML = text(await render(OpenAccount))
    expect(enrolledHTML).toContain('Change the password you sign in with')
    expect(enrolledHTML).toContain('Recovery code Replace the code that lets you back in if you forget your password')
    expect(enrolledHTML).not.toContain('reset link')
    // Still signed in from before the key scheme: the old password signs in no more, and nothing asks them to sign in again.
    const { public_key: _, ...notEnrolled } = ana
    signIn(notEnrolled)
    const legacyHTML = text(await render(OpenAccount))
    expect(legacyHTML).not.toContain('Change the password you sign in with')
    expect(legacyHTML).not.toContain('Recovery code')
    expect(legacyHTML).toContain('it no longer signs you in: once this session ends, you need a reset link from the administrator of this server')
    expect(legacyHTML).not.toContain('Sign in again')
    signIn({ ...notEnrolled, has_password: false })
    const externalHTML = text(await render(OpenAccount))
    expect(externalHTML).not.toContain('reset link')
    expect(externalHTML).not.toContain('Recovery code')
  })

  it('shows the person’s own name as text', async () => {
    signIn({ ...ana, name: hostile })
    const html = await render(OpenAccount)
    expect(html).not.toContain('<img')
    expect(html).toContain(ana.email)
  })

  it('names the person’s role on this server, which a self-hosted server’s people have', async () => {
    signIn({ ...ana, role: 'member' })
    const words = (await render(OpenAccount)).replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ')
    expect(words).toContain('Role on this server Member')
  })

  it('asks nobody to accept a company’s terms, and links no policy, on the sign-in and sign-up cards or in the account section', async () => {
    locale.value = 'pt'
    const signup = await render(SignInView, { invitation: { invite: 'SyntheticInviteCode_0123456789abcdefghijklmn', email: 'new@example.test' } })
    expect(signup).not.toContain('class="consent"')
    const signin = await render(SignInView, { invitation: null })
    signIn()
    const account = await render(OpenAccount)
    for (const html of [signup, signin, account]) {
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

describe('sync, as the console shows it', () => {
  it('asks a person with a mailbox for consent, in plain words that name this server, with no policy linked', async () => {
    signIn()
    notConsented()
    Object.assign(accounts, { list: [account({ state: 'active' })], loaded: true })
    const html = await render(AccountsPanel)
    const card = html.match(/<section class="consent-card"(?:(?!<\/section>).)*<\/section>/s)?.[0] ?? ''
    const words = text(card)
    for (const item of ['keeps an index of a mailbox’s mail in its own database', 'who sent it and who it was sent to, with their names',
      'its subject, dates and size', 'the folder it is in, and flags such as read or starred', 'the identifiers that tie a reply to its conversation',
      'the type, size and file name of each part, but not what the part contains', 'Message bodies and attachments are never stored.',
      'Sync starts with the last 90 days', 'except All Mail, Starred and Important in Gmail', 'unless it is among the 10,000 most recent on this server',
      // Whose agreement a mailbox syncs under, who reads a team mailbox's index, and what turning sync off reaches (the text's third revision).
      'The mailboxes of your personal workspace sync under your agreement, which you give here.',
      'A team’s mailbox syncs under the team’s agreement: the owner or admin of the team who connects it, or turns its sync on later, agrees to this same text on the team’s behalf.',
      'Of a team’s mailbox, the members given read access to it, which only an owner or an admin of the team who reads it can give',
      'Owners and admins read nothing by their role',
      'Turning your sync off deletes the index of your personal workspace’s mailboxes only',
      'Any owner or admin of a team can turn a team mailbox’s sync off, which deletes its index for everyone who reads it',
      'Closing your account on this server does not remove the mailboxes of a team other people are in, nor their index; a team you are the only member of is deleted with your account, with its mailboxes and their index.',
      // The upgrade's exception: a team mailbox still under the person's own agreement stops with it until the team agrees.
      'a team mailbox that still syncs under the agreement you gave for your own mailboxes stays tied to it until an owner or an admin of the team agrees for the team. Until then, turning your sync off, or closing your account, stops it and deletes its index.',
      'Whoever runs this server can read its database, this index included.']) expect(words).toContain(item)
    expect(card).not.toMatch(/<a[^>]*href=/)
    expect(words).not.toMatch(/Mailie|privacy|policy/i)
    expect(card).toContain('Turn on sync')
    expect(card).toContain('Not now')
  })

  it('does not ask before there is a mailbox, after "not now", or once the person agreed', async () => {
    signIn()
    notConsented()
    Object.assign(accounts, { list: [], loaded: true })
    expect(await render(AccountsPanel)).not.toContain('consent-card')
    Object.assign(accounts, { list: [account({ state: 'active' })] })
    consent.dismissed = true
    expect(await render(AccountsPanel)).not.toContain('consent-card')
    consent.dismissed = false
    consented()
    expect(await render(AccountsPanel)).not.toContain('consent-card')
  })

  it('shows the first sync’s progress, the messages indexed so far, and that a message is read from the mail server and not kept', async () => {
    signIn()
    consented()
    const row = account({ state: 'active', sync: syncing() })
    Object.assign(accounts, { list: [row], loaded: true, detailID: row.id })
    const html = await render(AccountsPanel)
    // Once on the card and once in the sheet, as a native progress bar the CSP needs no inline style for.
    expect(html.match(/<progress class="sync-progress" max="100" value="42" aria-label="First sync progress"/g)).toHaveLength(2)
    expect(html).not.toMatch(/style="/)
    expect(html).toContain('Indexing the last 90 days, newest first.')
    expect(html).toContain('1,284')
    expect(text(html)).toContain('Folders finished 3 of 7')
    expect(html).toContain('Incremental (CONDSTORE)')
    expect(html).toContain('Sync now')
    expect(html).toContain('When a tool reads a message, the server fetches it from the mail server and does not keep it.')
    expect(html).not.toContain('in Mail,')
    expect(html).not.toContain('not available yet')
    expect(html).not.toContain('Connected. Sync is off.')
  })

  it('names the failure of a backing-off sync in the console’s words, and never shows the class itself', async () => {
    signIn()
    consented()
    const now = Math.floor(Date.now() / 1000)
    Object.assign(accounts, { list: [
      account({ id: 'acc_1', state: 'active', sync: syncing({ state: 'backoff', error_class: 'rate_limited', next_retry_at: now + 120 }) }),
      account({ id: 'acc_2', email: 'b@example.test', state: 'active', sync: syncing({ state: 'stopped', error_class: 'something_new<b>' }) }),
    ], loaded: true })
    const html = await render(AccountsPanel)
    expect(html).toContain('The provider is limiting how often Mailie can ask. Mailie tries again in 2 min.')
    expect(html).toContain('The last sync attempt failed.')
    expect(html).not.toContain('rate_limited')
    expect(html).not.toContain('something_new')
  })

  it('calls a mailbox no worker holds yet paused, not failed, when nothing went wrong', async () => {
    signIn()
    consented()
    Object.assign(accounts, { list: [account({ state: 'active', sync: syncing({ state: 'stopped', running: false }) })], loaded: true })
    const html = await render(AccountsPanel)
    expect(html).toContain('Paused')
    expect(html).toContain('Mailie resumes syncing this mailbox shortly.')
    expect(html).not.toContain('failed')
    expect(html).not.toContain('Stopped')
  })

  it('says a team’s mailbox syncs under the team’s agreement, and offers a member none of their own for it', async () => {
    signIn()
    notConsented()
    // A mailbox of Support, whose sync its owners and admins have not turned on: Ana's own agreement would not sync it.
    const team: Workspace = { id: 'wsp_000000000000bbbb', kind: 'team', source: 'local', name: 'Support', role: 'member', status: 'active', created_at: 1_790_000_000 }
    Object.assign(workspaces, { list: [team], loaded: true, supported: true, currentID: team.id })
    const row = account({ state: 'active', workspace_id: team.id, access: { read: true, act: false, send: false, manage: false } })
    Object.assign(accounts, { list: [row], loaded: true, loading: false, workspace: team.id })
    const html = await render(AccountSheet, { account: row })
    expect(html).toContain('Sync is off for this mailbox. The owners and admins of Support turn it on, for the team.')
    expect(html).not.toContain('Turn on sync…')
    expect(html).not.toContain('Sync now')
    // A mailbox of the person's own still offers it.
    Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '' })
    const own = account({ id: 'acc_own', state: 'active' })
    const mine = await render(AccountSheet, { account: own })
    expect(mine).toContain('Sync is off. Nothing from this mailbox is stored.')
    expect(mine).toContain('Turn on sync…')
  })

  it('offers consent from the sheet of a mailbox that does not sync, and says nothing is stored', async () => {
    signIn()
    notConsented()
    const row = account({ state: 'active' })
    Object.assign(accounts, { list: [row], loaded: true })
    const html = await render(AccountSheet, { account: row })
    expect(html).toContain('Sync is off. Nothing from this mailbox is stored.')
    expect(html).toContain('Turn on sync…')
    expect(html).not.toContain('Sync now')
  })

  it('marks folders from the index by their sync state, and a live listing by what sync would take', async () => {
    signIn()
    consented()
    const row = account({ state: 'active', sync: syncing({ state: 'live', initial_progress: 100 }) })
    const indexed = [
      { name: 'INBOX', display_name: 'Inbox', role: 'inbox', selectable: true, synced: true, messages: 1284, unseen: 12, sync_state: 'live' },
      { name: '[Gmail]/All Mail', display_name: 'All Mail', role: 'all', selectable: true, synced: false, sync_state: 'disabled' },
      { name: 'Clientes', display_name: 'Clientes', selectable: true, synced: true, messages: 0, sync_state: 'initial' },
    ]
    Object.assign(accounts, { list: [row], folders: { [row.id]: { loading: false, loaded: true, list: indexed, failure: null } } })
    const html = await render(AccountSheet, { account: row })
    expect(html).toMatch(/<span class="(live pill|pill live)" title="This folder is indexed and kept up to date."[^>]*>Synced<\/span>/)
    expect(html).toMatch(/<span class="pill" title="Mailie does not sync this folder."[^>]*>Not synced<\/span>/)
    expect(html).toMatch(/<span class="(busy pill|pill busy)"[^>]*>Syncing<\/span>/)
    expect(html).toContain('Unread / indexed messages')
    expect(html).toContain('From Mailie’s index')
    // The same mailbox listed live, before consent: only what sync would take.
    notConsented()
    const off = { ...row, sync: { ...row.sync, enabled: false, state: 'off' } }
    const live = indexed.map(({ sync_state: _, ...folder }) => folder)
    Object.assign(accounts, { list: [off], folders: { [row.id]: { loading: false, loaded: true, list: live, failure: null } } })
    const before = await render(AccountSheet, { account: off })
    expect(before.match(/>Included in sync</g)).toHaveLength(2)
    expect(before).not.toContain('Not synced')
    expect(before).toContain('Unread / total messages')
    expect(before).not.toContain('From Mailie’s index')
  })

  it('shows sync as a switch in the account section: off with nothing stored, on with its date', async () => {
    signIn()
    notConsented()
    let html = await render(OpenAccount)
    expect(html).toMatch(/<button class="sync-switch" type="button" role="switch" aria-checked="false"/)
    expect(html).toContain('Off. Mailie stores nothing from your mail.')
    consented()
    html = await render(OpenAccount)
    expect(html).toMatch(/role="switch" aria-checked="true"/)
    expect(html).toContain('On since ')
    expect(html).toContain('Turning sync off deletes it.')
  })

  it('shows actions as a switch in the account section: off with nothing changed, on with its date', async () => {
    signIn()
    consented()
    Object.assign(actionsConsent, { loaded: true, consented: false, currentVersion: ACTIONS_TEXT_VERSION })
    let html = await render(OpenAccount)
    expect(text(html)).toContain('Actions on my messages Off. Mailie does not change your mailboxes when you ask.')
    expect(actionsSwitch(html)).toContain('aria-checked="false"')
    Object.assign(actionsConsent, { consented: true, version: ACTIONS_TEXT_VERSION, consentedAt: Date.UTC(2026, 8, 25, 12) / 1000 })
    html = await render(OpenAccount)
    expect(text(html)).toContain('Allowed since September 25, 2026. Mailie changes your mailbox only when you ask.')
    expect(actionsSwitch(html)).toContain('aria-checked="true"')
  })

  it('says actions are paused, not on, when the person agreed to an older text, and offers the new one to agree to', async () => {
    signIn()
    consented()
    Object.assign(actionsConsent, { loaded: true, consented: true, version: '2026-01-older-actions', consentedAt: Date.UTC(2026, 8, 25, 12) / 1000, currentVersion: ACTIONS_TEXT_VERSION })
    const html = await render(OpenAccount)
    expect(text(html)).toContain('Actions on my messages Paused: this server’s text about actions changed since you allowed them on September 25, 2026. Review the new text and agree to use them again. Review and agree Turn off actions')
    // No switch that says "On" while the server refuses every action: sync's is the only one.
    expect(html.match(/role="switch"/g)).toHaveLength(1)
    expect(actionsSwitch(html)).toBe('')
    expect(html).toMatch(/<button class="primary small" type="button" aria-haspopup="dialog"[^>]*>Review and agree<\/button>/)
  })

  it('says, in the person’s language, when the terms changed since they agreed', async () => {
    signIn()
    locale.value = 'pt'
    Object.assign(consent, { loaded: true, consented: true, version: '2026-01-older', currentVersion: VERSION })
    Object.assign(accounts, { list: [account({ state: 'active' })], loaded: true })
    const html = await render(AccountsPanel)
    expect(html).toContain('Sincronização de emails: os termos mudaram')
    // Sync keeps running under the older consent; the card must not say otherwise.
    expect(html).toContain('ela continua ligada, a menos que você a desligue')
    expect(html).toMatch(/<button class="primary" type="button"[^>]*>\s*Concordo\s*<\/button>/)
    expect(html).not.toMatch(/<a[^>]*href=/)
  })

  it('offers a reload instead of an agree button when the text changed while the page was open', async () => {
    signIn()
    Object.assign(accounts, { list: [account({ state: 'active' })], loaded: true })
    for (const agreed of [false, true]) {
      Object.assign(consent, agreed
        ? { loaded: true, consented: true, version: VERSION, consentedAt: 1_790_000_000, currentVersion: '2027-01-privacy-bodies' }
        : { loaded: true, consented: false, version: '', currentVersion: '2027-01-privacy-bodies' })
      // A refused grant said the same before the page read the new revision: said once.
      consent.problem = { op: 'grant-sync', code: 'bad_request' }
      const html = await render(AccountsPanel)
      const card = html.match(/<section class="consent-card"[^]*?<\/section>/)?.[0] ?? ''
      expect(text(card).split('The text about sync changed on this server while this page was open. Reload the page to read the current text.')).toHaveLength(2)
      expect(card).toMatch(/<button class="primary" type="button"[^>]*>Reload page<\/button>/)
      expect(card).not.toContain('Turn on sync')
      expect(card).not.toContain('I agree')
      // The bundled text is not the one the server asks about, so it is not shown as the terms.
      expect(card).not.toContain('For each message, the index holds:')
    }
  })
})

describe('API keys, as the console shows them', () => {
  const held = (accountID: string, fields = {}) => ({ account_id: accountID, workspace_id: 'wsp_000000000000aaaa', read: true, act: false, send: false, updated_at: 1_790_000_000, ...fields })
  const key = {
    prefix: '3f9a0c1d2e4b5a6c', name: hostile, scope: 'write', workspace_id: 'wsp_000000000000aaaa',
    mailboxes: [held('acc_0000000000000001', { act: true }), held('acc_gone')], created_at: 1_790_000_000, expires_at: 4_000_000_000,
    live: true, terms_version: KEY_TERMS_VERSION, sends: false,
  }

  it('shows a key’s name and what it holds on each mailbox as text, and what it may do in words', async () => {
    vi.stubGlobal('location', new URL('http://localhost:5174/'))
    signIn()
    Object.assign(accounts, { list: [account({ state: 'active' })], loaded: true })
    Object.assign(apiKeys, { list: [key, { ...key, prefix: 'aaaaaaaaaaaaaaaa', name: 'Old', scope: 'read', mailboxes: [], revoked_at: 1_790_000_500, live: false }], loaded: true })
    const html = await render(KeysPanel)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;')
    const [live, revoked] = html.match(/<article class="[^"]*key-card[^]*?<\/article>/g) ?? []
    expect(text(live!)).toContain('Access Read and act')
    expect(text(live!)).toContain('Mailboxes suporte@example.test (Read, Act), A removed mailbox (Read)')
    expect(text(live!)).toContain('Revoke')
    expect(text(revoked!)).toContain('Revoked')
    expect(text(revoked!)).toContain('Mailboxes None yet')
    expect(revoked).not.toContain('>Revoke<')
  })

  it('shows the MCP URL at this server’s own origin and a command whose key is a placeholder, in the person’s language', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    locale.value = 'pt'
    signIn()
    Object.assign(apiKeys, { list: [], loaded: true })
    Object.assign(mcpAccess, { loaded: true, served: true })
    const html = await render(KeysPanel)
    // As a person reads it: the markup's escapes undone.
    const panel = text(html.match(/<section class="connect-panel"[^]*?<\/section>/)?.[0] ?? '').replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>')
    expect(panel).toContain('https://mail.example.org/mcp')
    expect(panel).toContain('claude mcp add --transport http mailie https://mail.example.org/mcp --header "Authorization: Bearer <sua chave>"')
    expect(panel).toContain('Os conectores do claude.ai ainda não são compatíveis')
    expect(html).toContain('Nenhuma chave de API ainda')
  })

  it('shows no MCP address until the server says it answers at /mcp, nor where it says it does not', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    signIn()
    Object.assign(apiKeys, { list: [], loaded: true })
    for (const answer of [{ loaded: false, served: false }, { loaded: true, served: false }]) {
      Object.assign(mcpAccess, answer)
      const html = await render(KeysPanel)
      expect(html).not.toContain('connect-panel')
      expect(html).not.toContain('/mcp')
      expect(html).toContain('No API keys yet')
    }
  })
})
