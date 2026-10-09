// A real browser over the open console (src/main.ts), with every /v1 call
// answered by an in-memory fake of the HTTP contract and every provider page
// intercepted. Nothing here reaches a daemon, Google, Microsoft, Apple or a
// personal browser profile. Optional QA, not part of `npm test`:
//
//   npm run dev &   # or `npm run build && npx vite preview --port 4174` with QA_ORIGIN=http://localhost:4174
//   node test/browser/console.mjs
//
// It walks what a self-hosted server's console offers: signing in and up
// with a password the server never receives (docs/key-scheme.md: the auth
// key, the recovery code shown once and replaced with the password,
// recovery, a reset link and the one-time upgrade), connecting mailboxes,
// sync, the person's account and permissions, API keys and the MCP endpoint,
// and Storage. The console names no company and links
// to no policy; there is no mail to read or send, and the pass fails on any
// request to the message or sending routes.
//
// QA_ORIGIN (default http://localhost:5174), QA_SCREENSHOTS (directory),
// QA_ONLY (console: the account passes; keys-scheme: the upgrade, recovery
// and a reset link; sync; actions: the permission in Account; keys: API keys
// and the MCP endpoint; storage),
// QA_PLAYWRIGHT_MODULE (path to playwright's index.mjs when it is not
// installed here), QA_BROWSER (chromium|firefox|webkit), QA_BROWSER_EXECUTABLE
// or QA_BROWSER_CHANNEL (e.g. chrome) to use an installed browser.
import { mkdir, readdir, readFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'
import {
  APPLE_APP_PASSWORD, GMAIL_ACCOUNT, INVITE, NOT_GRANTED_EMAIL, PASSWORD, REASON_NOT_GRANTED, REASON_TOKEN_REJECTED, RESET,
  fakeDaemon as coreDaemon, focused, noHorizontalOverflow, now, sleep, spoken, until,
} from './fakeDaemon.mjs'

const playwright = await import(process.env.QA_PLAYWRIGHT_MODULE || 'playwright')
const engine = process.env.QA_BROWSER || 'chromium'
const origin = (process.env.QA_ORIGIN || 'http://localhost:5174').replace(/\/$/, '')
const screenshots = process.env.QA_SCREENSHOTS ? resolve(process.env.QA_SCREENSHOTS) : ''
const only = process.env.QA_ONLY || ''
const browser = await playwright[engine].launch({
  headless: true,
  ...(process.env.QA_BROWSER_EXECUTABLE ? { executablePath: process.env.QA_BROWSER_EXECUTABLE } : {}),
  ...(process.env.QA_BROWSER_CHANNEL ? { channel: process.env.QA_BROWSER_CHANNEL } : {}),
})
if (screenshots) await mkdir(screenshots, { recursive: true })

/** The revision of the open console's sync text, the daemon's default (src/open/versions.ts). */
const SYNC_VERSION = '2026-10-open-sync-3'
/** The one that describes actions on messages. */
const ACTIONS_VERSION = '2026-10-open-actions-3'
/** The text a person agrees to by creating an API key. */
const KEY_TERMS_VERSION = '2026-10-open-api-keys-2'

/**
 * The core's fake daemon (fakeDaemon.mjs), asking about the open console's
 * texts, and recording every request to the routes the open console never
 * calls (messages, actions on them, sending) in calls.unoffered, for the
 * passes to find none; those are not answered.
 */
const fakeDaemon = (options = {}) => coreDaemon({
  origin, versions: { sync: SYNC_VERSION, actions: ACTIONS_VERSION, keys: KEY_TERMS_VERSION }, ...options,
  extend: ({ calls }) => {
    calls.unoffered = []
    return {
      route({ path, method, fail }) {
        if (path === '/v1/me/send-consent' || path === '/v1/messages' || path.startsWith('/v1/messages/') || path.startsWith('/v1/sends/')) {
          calls.unoffered.push(`${method} ${path}`)
          return fail(404, 'not_found')
        }
        return undefined
      },
    }
  },
})

/**
 * The open console names no company and links to no policy: no legal links,
 * no sign-up acceptance, no access note, no "Privacy and terms" row, and no
 * byline under the wordmark.
 */
async function noLegal(page, where) {
  assert.equal(await page.locator('a[href*="thehappie"], .legal-link, .auth-legal, .consent, .access-note, .legal, .lockup-by').count(), 0, `${where}: no legal link, acceptance or byline`)
  assert.equal(await page.getByText(/Happie|Privacy Policy|Terms of Use|Política de Privacidade|Datenschutzerklärung/).count(), 0, `${where}: no company or policy named`)
}

/**
 * What each pass expects the console to say for the two reasons it explains:
 * a Gmail consent that left the mailbox out, and the Gmail account whose
 * mail server refused the grant when its folders were listed.
 */
const REFUSED = {
  'en-US': {
    notGranted: 'Google did not give Mailie access to this mailbox. Try again and, on Google’s screen, allow access to Gmail (tick the box if one is shown).',
    mailbox: 'The mail server refused this authorization. Try again and choose the account suporte@example.test on Google’s sign-in screen.',
    folders: 'This account needs to be authorized again before its folders can be listed.',
  },
  'pt-BR': {
    notGranted: 'O Google não deu ao Mailie acesso a esta caixa de email. Tente novamente e, na tela do Google, permita o acesso ao Gmail (marque a caixa, se aparecer uma).',
    mailbox: 'O servidor de email recusou esta autorização. Tente novamente e escolha a conta suporte@example.test na tela de acesso do Google.',
    folders: 'Esta conta precisa ser autorizada de novo antes que suas pastas possam ser listadas.',
  },
  'de-DE': {
    notGranted: 'Google hat Mailie keinen Zugriff auf dieses Postfach gegeben. Versuchen Sie es erneut und erlauben Sie auf der Seite von Google den Zugriff auf Gmail (setzen Sie das Häkchen, falls ein Kästchen angezeigt wird).',
    mailbox: 'Der Mailserver hat diese Autorisierung abgelehnt. Versuchen Sie es erneut und wählen Sie auf der Anmeldeseite von Google das Konto suporte@example.test.',
    folders: 'Dieses Konto muss erneut autorisiert werden, bevor seine Ordner aufgelistet werden können.',
  },
}

/**
 * The recovery code a ceremony just showed: once, in a dialog that stays
 * until the person says they saved it. Returns the code.
 */
async function saveRecoveryCode(page, shot) {
  const dialog = page.getByRole('dialog', { name: 'Save your recovery code' })
  await dialog.waitFor({ timeout: 20_000 })
  await shot?.()
  const code = (await dialog.locator('.code-secret').textContent()).trim()
  assert.match(code, /^[0-9A-Z]{5}(-[0-9A-Z]{5}){5}$/, 'a recovery code is six groups of five')
  assert.equal(await dialog.getByRole('button', { name: 'Continue' }).isDisabled(), true, 'it stays until the person says they saved it')
  await dialog.getByLabel('I saved my recovery code somewhere safe.').check()
  await dialog.getByRole('button', { name: 'Continue' }).click()
  await dialog.waitFor({ state: 'hidden' })
  return code
}

/** Whether any request of a pass carried this text: a password, or a recovery code. */
const sent = (daemon, secret) => JSON.stringify(daemon.calls.auth).includes(secret)

const failures = []
for (const mobile of only && only !== 'console' ? [] : [false, true]) {
  for (const scheme of ['light', 'dark']) {
    const label = `${mobile ? 'mobile' : 'desktop'}-${scheme}`
    const context = await browser.newContext({
      serviceWorkers: 'block', locale: 'en-US', colorScheme: scheme, reducedMotion: 'reduce',
      viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
    })
    const daemon = fakeDaemon()
    const errors = []
    // Requests being answered, the event stream aside, which stays open.
    let answering = 0
    await context.route('**/v1/**', async route => {
      const streaming = new URL(route.request().url()).pathname === '/v1/events'
      if (!streaming) answering++
      try { await daemon.handle(route) } finally { if (!streaming) answering-- }
    })
    /** Waits until everything the page asked for is answered, and it asks nothing more for a moment. */
    const settled = async () => {
      for (;;) {
        await until(() => answering === 0, 'the page to settle')
        await sleep(300)
        if (answering === 0) return
      }
    }
    // Provider pages. Google's goes nowhere (the loopback flow never returns
    // to the console); Microsoft's answers like a consent granted, straight
    // back to the console's return route.
    await context.route('https://accounts.google.com/**', route => route.fulfill({ status: 200, contentType: 'text/html', body: '<!doctype html><title>Google</title><p>Synthetic consent page</p>' }))
    // The legal pages: a link that opens one must leave the console where it was.
    await context.route('https://thehappie.co/**', route => route.fulfill({ status: 200, contentType: 'text/html', body: '<!doctype html><title>The Happie Co</title><p>Synthetic legal page</p>' }))
    await context.route('https://login.microsoftonline.com/**', route => {
      const state = new URL(route.request().url()).searchParams.get('state')
      return route.fulfill({ status: 302, headers: { location: `${origin}/oauth/return?code=synthetic-code-0001&state=${state}&session_state=x` } })
    })
    const page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
    const shot = async name => {
      await noHorizontalOverflow(page, `${label} ${name}`)
      if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
    }
    const openSection = async name => {
      if (mobile) await page.getByRole('button', { name: 'Open menu', exact: true }).click()
      await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
    }
    try {
      // --- signing in -------------------------------------------------------
      await page.goto(origin + '/')
      await page.locator('form[name=mailie-login]').waitFor()
      await noLegal(page, 'sign-in')
      await shot('signin')
      await page.locator('input[name=username]').fill('ana@example.test')
      await page.locator('input[name=password]').fill('wrong password')
      await page.locator('form[name=mailie-login] button[type=submit]').click()
      await page.getByRole('alert').filter({ hasText: 'The email or password is incorrect.' }).waitFor()
      assert.equal(await page.getByText('detail the console must never show').count(), 0, 'server messages are never rendered')
      await page.locator('input[name=password]').fill(PASSWORD)
      await page.locator('form[name=mailie-login] button[type=submit]').click()
      await page.locator('.console-main').waitFor()
      await page.locator('.account-card').first().waitFor()
      assert.equal(await page.locator('.account-card').count(), 3)
      // An auth key derived here went, never the password, wrong or right.
      assert.equal(sent(daemon, PASSWORD) || sent(daemon, 'wrong password'), false, 'no password reaches the server')
      assert.ok(daemon.calls.auth.some(call => call.path === '/v1/auth/login' && /^[A-Za-z0-9_-]{43}$/.test(call.body.auth_key)), 'the sign-in sends an auth key')
      // The server's sections, and nothing to read or send mail with. API keys
      // are a workspace's, and this fake server has none (the keys pass has).
      if (mobile) await page.getByRole('button', { name: 'Open menu', exact: true }).click()
      assert.deepEqual((await page.locator('.console-nav:visible .console-nav-item > span:not(.nav-count)').allTextContents()).map(item => item.trim()), ['Mailboxes', 'Storage', 'Account'])
      if (mobile) await page.keyboard.press('Escape')
      await noLegal(page, 'mailboxes')
      assert.equal(await page.locator('img[src=x]').count(), 0, 'a display name is text, never markup')
      await page.getByText('<img src=x onerror=alert(1)>', { exact: true }).first().waitFor()
      for (const claim of ['Syncing now', 'receiving new mail', 'first sync', 'Last sync']) assert.equal(await page.getByText(claim).count(), 0, `no claim of a sync that does not exist: ${claim}`)
      await page.getByText('Active accounts').waitFor()
      // A reason the console does not know is never shown; the state speaks for it.
      await page.locator('.account-card').filter({ hasText: 'ana.souza@example.test' }).getByText('The provider asks for this account to be authorized again.', { exact: false }).waitFor()
      assert.equal(await page.getByText('refresh token revoked').count(), 0, 'an unknown state_reason is never rendered')
      await shot('accounts')

      // --- reload keeps the session (encrypted IndexedDB vault) --------------
      await page.reload()
      await page.locator('.account-card').first().waitFor()
      assert.equal(daemon.calls.login, 2, 'a reload restores the session without signing in again')
      assert.ok(daemon.calls.me >= 1, 'a restored session is checked with /v1/auth/me')

      // --- connecting an IMAP mailbox --------------------------------------
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      const dialog = page.getByRole('dialog', { name: 'Connect an email account' })
      await dialog.waitFor()
      await shot('add-choose')
      // Chosen with the keyboard, as the person the focus fix is for would.
      await dialog.getByRole('button', { name: /Other provider \(IMAP\)/ }).focus()
      await page.keyboard.press('Enter')
      const imap = page.getByRole('dialog', { name: 'Other provider (IMAP)' })
      await imap.getByLabel('Email address').waitFor()
      await page.waitForFunction(() => document.activeElement?.getAttribute('name') === 'mailbox')
      assert.match(await imap.ariaSnapshot(), /textbox "Password"\n/, 'the IMAP password field is named Password, not after its toggle')
      await imap.getByLabel('Email address').fill('vendas@example.test')
      assert.equal(await imap.getByLabel('Server').first().inputValue(), 'imap.example.test', 'the IMAP server is guessed from the address')
      await imap.getByLabel('Password', { exact: true }).fill('not-the-app-password')
      await imap.getByRole('button', { name: 'Connect', exact: true }).click()
      await imap.getByText('Testing the connection…').first().waitFor()
      await imap.getByRole('alert').filter({ hasText: 'The mail server refused the sign-in' }).waitFor()
      await page.waitForFunction(() => document.activeElement?.textContent?.trim() === 'Try again')
      await imap.getByRole('button', { name: 'Try again' }).click()
      assert.equal(await imap.getByLabel('Email address').inputValue(), 'vendas@example.test', 'a failed test keeps the form')
      await imap.getByLabel('Password', { exact: true }).fill('app-password-123')
      assert.equal(await page.evaluate(() => document.documentElement.outerHTML.includes('app-password-123')), false, 'the mailbox password is never in the markup')
      await noLegal(page, 'IMAP form')
      await shot('add-imap')
      await imap.getByRole('button', { name: 'Connect', exact: true }).click()
      await imap.getByText('Account connected').waitFor()
      await imap.locator('.success').getByText('vendas@example.test is connected. You can list its folders now. Sync is off until you turn it on.').waitFor()
      await page.waitForFunction(() => document.activeElement?.textContent?.trim() === 'Done')
      assert.match(await spoken(page), /vendas@example\.test is connected\./, 'the live region inside the dialog says the account connected')
      await shot('add-done')
      await imap.getByRole('button', { name: 'Done' }).click()
      await page.locator('.account-card').filter({ hasText: 'vendas@example.test' }).waitFor()
      assert.equal(daemon.calls.created.at(-1).password, 'app-password-123')
      assert.equal(daemon.calls.created.at(-1).smtp_tls, 'implicit')

      // --- connecting iCloud Mail -------------------------------------------
      // An Apple address typed into the IMAP form is pointed at iCloud, which
      // knows Apple's servers; the address and password come along.
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Other provider \(IMAP\)/ }).click()
      const imapForApple = page.getByRole('dialog', { name: 'Other provider (IMAP)' })
      await imapForApple.getByLabel('Email address').fill('ana.souza@icloud.com')
      const useICloud = imapForApple.getByRole('button', { name: 'Use iCloud Mail' })
      await useICloud.waitFor()
      assert.equal(await imapForApple.getByLabel('Server').first().inputValue(), '', 'no server name is guessed for an Apple address')
      await shot('add-imap-apple-address')
      await useICloud.click()
      const icloud = page.getByRole('dialog', { name: 'iCloud Mail' })
      await icloud.getByLabel('Email address').waitFor()
      await page.waitForFunction(() => document.activeElement?.getAttribute('name') === 'mailbox')
      assert.equal(await icloud.getByLabel('Email address').inputValue(), 'ana.souza@icloud.com', 'switching to iCloud keeps the address')
      // Back to the list, and in through the iCloud option itself.
      await icloud.getByRole('button', { name: 'Back' }).click()
      const choices = page.getByRole('dialog', { name: 'Connect an email account' })
      const labels = await choices.locator('.provider-option strong').allTextContents()
      assert.deepEqual(labels, ['Gmail or Google Workspace', 'Microsoft 365 or Outlook', 'iCloud Mail', 'Other provider (IMAP)'], 'iCloud sits between Microsoft and IMAP')
      await choices.getByRole('button', { name: /iCloud Mail/ }).click()
      await icloud.getByLabel('Email address').waitFor()
      assert.equal(await icloud.getByLabel('Server').count(), 0, 'iCloud asks for no server names')
      assert.equal(await icloud.getByRole('spinbutton').count(), 0, 'iCloud asks for no ports')
      assert.match(await icloud.ariaSnapshot(), /textbox "App-specific password"\n/, 'the iCloud password field is named for what it wants')
      const help = icloud.getByRole('link', { name: 'How to create an app-specific password' })
      assert.equal(await help.getAttribute('href'), 'https://support.apple.com/102654')
      assert.equal(await help.getAttribute('target'), '_blank')
      assert.equal(await help.getAttribute('rel'), 'noopener noreferrer')
      await icloud.getByText('account.apple.com → Sign-In and Security → App-Specific Passwords', { exact: false }).waitFor()
      await icloud.getByText('Two-factor authentication must be on', { exact: false }).waitFor()
      // The Apple Account password, which iCloud refuses over IMAP.
      await icloud.getByLabel('App-specific password', { exact: true }).fill('apple-account-password')
      await icloud.getByRole('button', { name: 'Connect', exact: true }).click()
      await icloud.getByText('Testing the connection…').first().waitFor()
      await icloud.getByRole('alert').filter({ hasText: 'iCloud refused the sign-in. Check the address and that you used an app-specific password, not your Apple Account password.' }).waitFor()
      await shot('add-icloud-refused')
      await icloud.getByRole('button', { name: 'Try again' }).click()
      assert.equal(daemon.calls.created.at(-1).login_user, undefined, 'an iCloud address signs in as itself')
      // A custom domain on iCloud+, which Apple refuses as the sign-in even
      // with the right password: it signs in as the account's iCloud address.
      await icloud.getByLabel('Email address').fill('ana@souza.example')
      await icloud.getByLabel('App-specific password', { exact: true }).fill(APPLE_APP_PASSWORD)
      assert.equal(await page.evaluate(secret => document.documentElement.outerHTML.includes(secret), APPLE_APP_PASSWORD), false, 'the app-specific password is never in the markup')
      await icloud.getByRole('button', { name: 'Connect', exact: true }).click()
      await icloud.getByRole('alert').filter({ hasText: 'iCloud refused the sign-in.' }).waitFor()
      await icloud.getByRole('button', { name: 'Try again' }).click()
      await icloud.getByLabel('iCloud address to sign in with (optional)').fill('ana.souza@icloud.com')
      await icloud.getByText('Only for a custom domain on iCloud+', { exact: false }).waitFor()
      await noLegal(page, 'iCloud form')
      await shot('add-icloud')
      await icloud.getByRole('button', { name: 'Connect', exact: true }).click()
      await icloud.getByText('Account connected').waitFor()
      await icloud.getByRole('button', { name: 'Done' }).click()
      const icloudCard = page.locator('.account-card').filter({ hasText: 'ana@souza.example' })
      await icloudCard.waitFor()
      await icloudCard.locator('dd', { hasText: /^iCloud$/ }).waitFor()
      assert.deepEqual(daemon.calls.created.at(-1), { email: 'ana@souza.example', provider: 'icloud', password: APPLE_APP_PASSWORD, login_user: 'ana.souza@icloud.com' }, 'an iCloud request carries no server names or ports, and a custom domain its iCloud sign-in')

      // --- connecting Gmail with the loopback flow ------------------------
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Gmail or Google Workspace/ }).click()
      const gmail = page.getByRole('dialog', { name: 'Gmail or Google Workspace' })
      await gmail.getByLabel('Email address').fill('financeiro@example.test')
      await noLegal(page, 'Gmail form')
      await shot('add-gmail')
      await gmail.getByRole('button', { name: 'Continue to Gmail' }).click()
      const link = gmail.getByRole('link', { name: 'Open Gmail sign-in' })
      await link.waitFor()
      await page.waitForFunction(() => document.activeElement?.matches('a.primary'))
      assert.equal(await link.getAttribute('target'), '_blank')
      assert.match(await link.getAttribute('rel'), /noopener/)
      assert.match(await link.getAttribute('href'), /^https:\/\/accounts\.google\.com\//)
      await shot('add-waiting')
      await gmail.getByText('Account connected').waitFor({ timeout: 20_000 })
      assert.ok(daemon.calls.polls >= 1, 'the dialog asks for the account until it is active')
      await gmail.getByRole('button', { name: 'Done' }).click()

      // --- abandoning a flow removes the half-made account -------------------
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Gmail or Google Workspace/ }).click()
      await gmail.getByLabel('Email address').fill('abandonada@example.test')
      await gmail.getByRole('button', { name: 'Continue to Gmail' }).click()
      await gmail.getByRole('link', { name: 'Open Gmail sign-in' }).waitFor()
      await gmail.getByRole('button', { name: 'Cancel' }).click()
      await page.waitForFunction(() => !document.querySelector('.account-card')?.parentElement?.textContent?.includes('abandonada@example.test'))
      assert.deepEqual(daemon.calls.removed, ['abandonada@example.test'], 'cancelling deletes the pending account')

      // --- a consent that came back without the mailbox -----------------------
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Gmail or Google Workspace/ }).click()
      await gmail.getByLabel('Email address').fill(NOT_GRANTED_EMAIL)
      await gmail.getByRole('button', { name: 'Continue to Gmail' }).click()
      await gmail.getByRole('alert').filter({ hasText: REFUSED['en-US'].notGranted }).waitFor({ timeout: 15_000 })
      assert.equal((await gmail.getByRole('alert').textContent()).trim(), REFUSED['en-US'].notGranted, 'the dialog says what to do on Google’s screen')
      assert.equal(await page.getByText(REASON_NOT_GRANTED).count(), 0, 'the server’s reason is never rendered')
      await page.waitForFunction(() => document.activeElement?.textContent?.trim() === 'Try again')
      await shot('add-refused')
      await gmail.locator('.dialog-actions').getByRole('button', { name: 'Close' }).click()
      await page.waitForFunction(email => ![...document.querySelectorAll('.account-card')].some(card => card.textContent.includes(email)), NOT_GRANTED_EMAIL)
      assert.deepEqual(daemon.calls.removed, ['abandonada@example.test', NOT_GRANTED_EMAIL], 'closing a failed first attempt removes the account it made')

      // --- Microsoft's form says the same before its consent step --------------
      await page.getByRole('button', { name: 'Connect an email account' }).first().click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Microsoft 365 or Outlook/ }).click()
      const outlook = page.getByRole('dialog', { name: 'Microsoft 365 or Outlook' })
      await outlook.getByRole('button', { name: 'Continue to Microsoft' }).waitFor()
      await noLegal(page, 'Microsoft form')
      await outlook.getByRole('button', { name: 'Back' }).click()
      await page.keyboard.press('Escape')
      await page.getByRole('dialog', { name: 'Connect an email account' }).waitFor({ state: 'hidden' })

      // --- finishing Microsoft's authorization through the web flow ------------
      const microsoft = page.locator('.account-card').filter({ hasText: 'ana.souza@example.test' })
      await microsoft.getByRole('button', { name: 'Finish authorization' }).click()
      await page.waitForURL(origin + '/', { timeout: 15_000 })
      await page.locator('.success').filter({ hasText: 'ana.souza@example.test is connected' }).waitFor()
      assert.equal(daemon.calls.callback.length, 1, 'the provider return is posted once')
      assert.match(daemon.calls.callback[0], /\/oauth\/return\?code=synthetic-code-0001&state=state_/)
      assert.equal(await page.evaluate(() => location.search + location.hash), '', 'the code is gone from the address bar')
      await shot('oauth-returned')

      // --- the account sheet: facts, folders, removal ----------------------
      await page.locator('.account-card').filter({ hasText: 'suporte@example.test' }).getByRole('button', { name: 'Details' }).click()
      const sheet = page.getByRole('dialog', { name: 'Suporte' })
      await sheet.waitFor()
      await sheet.getByRole('button', { name: 'Show folders' }).click()
      await sheet.getByText('Clientes/2026').first().waitFor()
      await shot('sheet')
      assert.equal(await sheet.getByText('Included in sync').count() > 0, true, 'a folder sync would take is marked as planned, not done')
      assert.equal(await sheet.getByText('From Mailie’s index').count(), 0, 'without consent the list comes from the mail server')
      await sheet.getByRole('button', { name: 'Remove account…' }).click()
      const remove = sheet.getByRole('button', { name: 'Remove permanently' })
      assert.equal(await remove.isDisabled(), true, 'removal is armed only by typing the address')
      await sheet.getByLabel('Type suporte@example.test to confirm').scrollIntoViewIfNeeded()
      await shot('sheet-remove')
      await sheet.getByLabel('Type suporte@example.test to confirm').fill('suporte@example.test')
      await remove.click()
      await page.locator('.success').filter({ hasText: 'suporte@example.test was removed' }).waitFor()
      // The Details button that opened the sheet went with the card.
      await page.waitForFunction(() => document.activeElement?.textContent?.trim() === 'Your mailboxes')
      assert.match(await spoken(page), /suporte@example\.test was removed from Mailie\./, 'the removal is said, not only drawn')

      // --- the account section ---------------------------------------------
      await openSection('Account')
      await page.locator('.account-settings').waitFor()
      await page.getByLabel('Your name').fill('Ana S.')
      await page.getByRole('button', { name: 'Save name' }).click()
      await page.getByText('Name saved.').first().waitFor()
      assert.match(await spoken(page), /Name saved\./, 'saving the name is said, not only drawn')
      await noLegal(page, 'account')
      // Sync and actions, and no sending: the open console sends nothing.
      assert.deepEqual(await page.getByRole('switch').evaluateAll(switches => switches.map(item => item.getAttribute('aria-labelledby'))), ['sync-switch-label', 'actions-switch-label'])
      await shot('profile')
      await page.getByRole('button', { name: /Password Change the password/ }).click()
      const change = page.getByRole('dialog', { name: 'Change password' })
      assert.equal(await change.locator('input[type=password]').count(), 3)
      const fields = await change.ariaSnapshot()
      for (const name of ['Current password', 'New password', 'Repeat the new password']) assert.ok(fields.includes(`textbox "${name}"`), `the field is named "${name}" alone:\n${fields}`)
      await change.getByLabel('Current password').fill(PASSWORD)
      await change.getByLabel('New password', { exact: true }).fill('another-password-2')
      await change.getByLabel('Repeat the new password').fill('another-password-2')
      await shot('password')
      await change.getByRole('button', { name: 'Change password' }).click()
      await page.getByText('Password changed.', { exact: false }).first().waitFor()
      assert.equal(sent(daemon, PASSWORD) || sent(daemon, 'another-password-2'), false, 'a password change sends auth keys only')

      // --- the recovery code, with the password every time ------------------
      // A session alone, however recent its sign-in, does not replace the
      // code: the password is asked for, and proved in the same request.
      await page.getByRole('button', { name: /Recovery code Replace the code/ }).click()
      const replaceCode = page.getByRole('dialog', { name: 'Replace recovery code' })
      await replaceCode.waitFor()
      await replaceCode.getByLabel('Password', { exact: true }).fill('not the password at all')
      await replaceCode.getByRole('button', { name: 'Make a new code' }).click()
      await replaceCode.getByRole('alert').filter({ hasText: 'The password is incorrect.' }).waitFor()
      assert.equal(daemon.calls.recovery, 0, 'a wrong password replaces nothing')
      await replaceCode.getByLabel('Password', { exact: true }).fill('another-password-2')
      await shot('recovery-code-password')
      await replaceCode.getByRole('button', { name: 'Make a new code' }).click()
      await saveRecoveryCode(page)
      await page.getByText('Recovery code replaced.', { exact: false }).first().waitFor()
      assert.equal(daemon.calls.stepUp, 0, 'no step-up: the password went with the request')
      assert.equal(daemon.calls.recovery, 1, 'the code was replaced once')
      assert.equal(sent(daemon, 'another-password-2'), false, 'replacing the code sends the auth key only')
      // A new token for the same person keeps the accounts section as it was.
      await openSection('Mailboxes')
      assert.equal(await page.locator('.account-card').count(), 5, 'a password change does not empty the accounts section')
      await page.reload()
      await page.locator('.console-main').waitFor()

      // --- the mobile drawer ------------------------------------------------
      if (mobile) {
        await page.getByRole('button', { name: 'Open menu', exact: true }).click()
        await page.locator('.mobile-navigation').waitFor()
        await shot('drawer')
        await page.keyboard.press('Escape')
      }

      // --- a revoked session sends the person to sign in ---------------------
      // Once the reload's requests are answered: one still on its way would
      // meet the revoked session first and end it before the click below.
      await settled()
      daemon.sessions.clear()
      const login = page.locator('form[name=mailie-login]')
      // The event stream reconnects by itself every few seconds, and may
      // still meet it first: either way the person ends at sign-in, told why.
      if (!(await login.isVisible())) {
        try {
          await openSection('Mailboxes')
          await page.getByRole('button', { name: 'Refresh' }).click()
        } catch (error) {
          if (!(await login.isVisible())) throw error
        }
      }
      await login.waitFor()
      await page.getByText('Your session ended. Sign in again.').waitFor()
      await shot('expired')

      // --- an invitation --------------------------------------------------
      // A fresh document, as a link from an email would be: a fragment-only
      // navigation on the same page would not load the app again.
      await page.goto('about:blank')
      await page.goto(`${origin}/#invite=${INVITE}&email=new%40example.test`)
      // The session was revoked: there is nothing to restore, and the link opens the sign-up form.
      await page.locator('form[name=mailie-signup]').waitFor()
      assert.equal(await page.evaluate(() => location.hash), '', 'the invitation code leaves the address bar at once')
      assert.equal(await page.locator('input[name=username]').inputValue(), 'new@example.test')
      await noLegal(page, 'sign-up')
      await shot('signup')
      await page.locator('input[name=name]').fill('Bruno Lima')
      await page.locator('input[name=password]').fill('bruno-password-1')
      await page.locator('input[name=confirm-password]').fill('bruno-password-1')
      await page.locator('form[name=mailie-signup] button[type=submit]').click()
      await saveRecoveryCode(page, () => shot('recovery-code'))
      assert.equal(sent(daemon, 'bruno-password-1'), false, 'signing up sends no password')
      await page.getByRole('heading', { name: 'Connect your first email account' }).waitFor()
      await shot('empty')
      // The empty state's button opens the dialog and is gone once the first
      // account exists; closing returns focus to the section, not to <body>.
      await page.locator('.empty-card').getByRole('button', { name: 'Connect an email account' }).click()
      await page.getByRole('dialog', { name: 'Connect an email account' }).getByRole('button', { name: /Other provider \(IMAP\)/ }).click()
      const first = page.getByRole('dialog', { name: 'Other provider (IMAP)' })
      await first.getByLabel('Email address').fill('bruno@example.test')
      await first.getByLabel('Password', { exact: true }).fill('app-password-123')
      await first.getByRole('button', { name: 'Connect', exact: true }).click()
      await first.getByRole('button', { name: 'Done' }).click()
      await page.locator('.account-card').filter({ hasText: 'bruno@example.test' }).waitFor()
      await page.waitForFunction(() => document.activeElement?.textContent?.trim() === 'Your mailboxes')

      assert.deepEqual(daemon.calls.unoffered, [], 'no request to the mail or sending routes')
      assert.deepEqual(errors, [], 'no page, script or CSP errors')
      console.log(`ok ${label}`)
    } catch (error) {
      failures.push(`${label}: ${error.message}`)
      console.log(`FAIL ${label}: ${error.stack}`)
      if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-${label}.png`) }).catch(() => {})
    } finally {
      daemon.close()
      await context.close()
    }
  }
}
// Longer languages, by structure rather than by label: German and Portuguese
// strings are what overflow first, and the English pass cannot see that.
for (const language of only && only !== 'console' ? [] : ['pt-BR', 'de-DE']) {
  for (const mobile of [false, true]) {
    const label = `${language}-${mobile ? 'mobile' : 'desktop'}`
    const context = await browser.newContext({
      serviceWorkers: 'block', locale: language, colorScheme: 'light', reducedMotion: 'reduce',
      viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
    })
    // The Gmail account's mail server refuses the grant, as it did in production.
    const daemon = fakeDaemon({ refuseFolders: ['acc_0000000000000001'] })
    const errors = []
    await context.route('**/v1/**', route => daemon.handle(route))
    const page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    const shot = async name => {
      await noHorizontalOverflow(page, `${label} ${name}`)
      if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
    }
    try {
      await page.goto(origin + '/')
      await page.locator('form[name=mailie-login]').waitFor()
      assert.equal(await page.evaluate(() => document.documentElement.lang), language === 'pt-BR' ? 'pt-BR' : 'de')
      await noLegal(page, 'sign-in')
      await shot('signin')
      await page.locator('input[name=username]').fill('ana@example.test')
      await page.locator('input[name=password]').fill(PASSWORD)
      await page.locator('form[name=mailie-login] button[type=submit]').click()
      await page.locator('.account-card').first().waitFor()
      await shot('accounts')
      await page.locator('.section-actions .primary').click()
      await page.locator('.provider-option').first().waitFor()
      await page.locator('.provider-option').nth(2).click()
      await page.locator('input[name=mailbox-password]').waitFor()
      assert.equal(await page.locator('.console-dialog input[type=number]').count(), 0, 'the iCloud form has no server fields')
      await noLegal(page, 'iCloud form')
      await shot('add-icloud')
      await page.locator('.console-dialog .dialog-actions .ghost').click()
      await page.locator('.provider-option').nth(3).click()
      await page.locator('input[name=mailbox-password]').waitFor()
      await noLegal(page, 'IMAP form')
      await shot('add-imap')
      await page.locator('.console-dialog .dialog-actions .ghost').click()
      await page.locator('.provider-option').first().click()
      await page.locator('input[name=mailbox]').fill('financeiro@example.test')
      await noLegal(page, 'Gmail form')
      await shot('add-gmail')
      // A consent that came back without the mailbox: the dialog says what to tick.
      const refused = REFUSED[language]
      await page.locator('input[name=mailbox]').fill(NOT_GRANTED_EMAIL)
      await page.locator('.console-dialog form button[type=submit]').click()
      const outcome = page.locator('.console-dialog .outcome [role=alert]')
      await outcome.waitFor({ timeout: 15_000 })
      assert.equal((await outcome.textContent()).trim(), refused.notGranted, 'the refused consent, explained in the console’s language')
      await shot('add-refused')
      await page.locator('.console-dialog .dialog-actions .ghost').click()
      await page.locator('.console-dialog').waitFor({ state: 'hidden' })
      await page.locator('.account-card .row-actions .ghost').first().click()
      await page.locator('.sheet').waitFor()
      await shot('sheet')
      // The mail server refuses the grant as the folders are listed: the sheet
      // says which account to choose and offers the authorization at once.
      await page.locator('.sheet .section-head button').click()
      await page.locator('.sheet [role=alert]').waitFor()
      assert.equal((await page.locator('.sheet [role=alert]').textContent()).trim(), refused.folders)
      assert.equal((await page.locator('.sheet .fact-note').textContent()).trim(), refused.mailbox, 'the sheet names the account to choose')
      await page.locator('.sheet .connection-action .primary').waitFor()
      assert.equal(await page.getByText(REASON_TOKEN_REJECTED).count(), 0, 'the server’s reason is never rendered')
      await page.locator('.sheet .connection-action').scrollIntoViewIfNeeded()
      await shot('sheet-refused')
      await page.keyboard.press('Escape')
      await page.locator('.account-card').first().getByText(refused.mailbox).waitFor()
      await page.locator(mobile ? '.mobile-profile' : '.profile-trigger').first().click()
      await page.locator('.account-settings').waitFor()
      await noLegal(page, 'account')
      await page.locator('.account-settings .sync-row').last().scrollIntoViewIfNeeded()
      await shot('profile')
      await page.goto('about:blank')
      await page.goto(`${origin}/#invite=${INVITE}&email=new%40example.test`)
      // Still signed in: teams are made here, so an invitation may be one to
      // join a team, and the remembered session is asked about it. This one
      // is for another address, which signing out lets use.
      const invited = page.locator('dialog .invitation')
      await invited.waitFor()
      assert.match(await invited.textContent(), /new@example\.test/, 'the invitation names its address')
      assert.equal(await invited.locator('button.primary').count(), 1, 'only signing out, no joining, for another address')
      await shot('invitation-signed-in')
      await invited.locator('button.primary').click()
      await page.locator('form[name=mailie-signup]').waitFor()
      await noLegal(page, 'sign-up')
      await shot('signup')
      assert.deepEqual(errors, [], 'no page errors')
      console.log(`ok ${label}`)
    } catch (error) {
      failures.push(`${label}: ${error.message}`)
      console.log(`FAIL ${label}: ${error.stack}`)
      if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-${label}.png`) }).catch(() => {})
    } finally {
      daemon.close()
      await context.close()
    }
  }
}

// --- the key scheme's other ways in --------------------------------------------
// The one-time upgrade of an account made before the key scheme, recovery
// with the code it shows, and a reset link from the operator.

for (const mobile of only && only !== 'keys-scheme' && only !== 'console' ? [] : [false, true]) {
  const label = `key-scheme-${mobile ? 'mobile' : 'desktop'}`
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: 'en-US', reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
  })
  const daemon = fakeDaemon({ notUpgraded: true })
  const errors = []
  await context.route('**/v1/**', route => daemon.handle(route))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
  }
  const signIn = async password => {
    await page.locator('form[name=mailie-login]').waitFor()
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(password)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
  }
  try {
    // The upgrade: the old password goes once, and the account is enrolled with it.
    await page.goto(origin + '/')
    await signIn(PASSWORD)
    const code = await saveRecoveryCode(page)
    await page.locator('.account-card').first().waitFor()
    assert.equal(daemon.calls.upgrade, 1, 'the old password is sent once, to the upgrade')
    await page.locator(mobile ? '.mobile-profile' : '.profile-trigger').first().click()
    await page.locator('.account-settings').getByRole('button', { name: 'Sign out', exact: true }).click()
    // A server that asks again for the password in clear is not believed.
    const ana = daemon.users.get('ana@example.test')
    const enrolled = { ...ana }
    Object.assign(ana, { authKey: undefined, password: PASSWORD })
    await signIn(PASSWORD)
    await page.getByRole('alert').filter({ hasText: 'This browser already set up this account so that your password never leaves it' }).waitFor()
    assert.equal(daemon.calls.upgrade, 1, 'never a second password in clear for an address that enrolled here')
    await shot('upgrade-refused')
    Object.assign(ana, enrolled)

    // Recovery with the code the upgrade showed: a new password, a new code.
    await page.getByRole('button', { name: 'Forgot your password?' }).click()
    const recover = page.locator('form[name=mailie-recover]')
    await recover.waitFor()
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=recovery-code]').fill(code.toLowerCase())
    await page.locator('input[name=password]').fill('a recovered password')
    await page.locator('input[name=confirm-password]').fill('a recovered password')
    await shot('recover')
    await recover.locator('button[type=submit]').click()
    const next = await saveRecoveryCode(page)
    assert.notEqual(next, code, 'a recovery shows a new code')
    await page.locator('.account-card').first().waitFor()
    assert.equal(sent(daemon, code) || sent(daemon, 'a recovered password'), false, 'neither the code nor the new password is sent')

    // A reset link from the operator: a new password, a new code, a new key.
    await page.locator(mobile ? '.mobile-profile' : '.profile-trigger').first().click()
    await page.locator('.account-settings').getByRole('button', { name: 'Sign out', exact: true }).click()
    await page.locator('form[name=mailie-login]').waitFor()
    const before = ana.publicKey
    await page.goto('about:blank')
    await page.goto(`${origin}/#reset=${RESET}&email=ana%40example.test`)
    const reset = page.locator('form[name=mailie-reset]')
    await reset.waitFor()
    assert.equal(await page.evaluate(() => location.hash), '', 'the reset code leaves the address bar at once')
    assert.equal(await page.locator('input[name=username]').inputValue(), 'ana@example.test')
    await shot('reset')
    await page.locator('input[name=password]').fill('a password after the reset')
    await page.locator('input[name=confirm-password]').fill('a password after the reset')
    await reset.locator('button[type=submit]').click()
    await saveRecoveryCode(page)
    await page.locator('.console-main').waitFor()
    assert.notEqual(ana.publicKey, before, 'a reset gives a new account key')
    assert.equal(sent(daemon, 'a password after the reset'), false, 'a reset sends no password')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok ${label}`)
  } catch (error) {
    failures.push(`${label}: ${error.message}`)
    console.log(`FAIL ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-${label}.png`) }).catch(() => {})
  } finally {
    daemon.close()
    await context.close()
  }
}

// --- sync ----------------------------------------------------------------------
// Consent, the first sync's progress, new mail arriving live, and turning sync
// off, over the event stream the console reads with fetch. English on both
// form factors and themes, then the longer languages.

/** What each pass expects the console to say about sync, and the names of the controls it uses. */
const SYNC_TEXT = {
  'en-US': {
    title: 'Turn on mail sync?', stored: 'who sent it and who it was sent to, with their names', notStored: 'Message bodies and attachments are never stored.', operator: 'Whoever runs this server can read its database, this index included.',
    turnOn: 'Turn on sync', notNow: 'Not now', sheetTurnOn: 'Turn on sync…', off: 'Connected. Sync is off.', onDone: 'Sync is on. Mailie is indexing your mailboxes, starting with the last 90 days.',
    upToDate: 'Up to date', syncNow: 'Sync now', requested: 'Sync requested. The counts update as it runs.', mailSync: 'Mail sync', offTitle: 'Turn off mail sync?', offDelete: 'Turn off and delete',
    offDone: 'Sync is off. Mailie deleted the index of your mail.', myAccount: 'Account', accounts: 'Mailboxes', details: 'Details', showFolders: 'Show folders',
    fromIndex: 'From Mailie’s index', notSynced: 'Not synced', expired: 'Your session ended. Sign in again.',
    refresh: 'Refresh', outdated: 'The text about sync changed on this server while this page was open. Reload the page to read the current text.', reload: 'Reload page',
  },
  'pt-BR': {
    title: 'Ligar a sincronização de emails?', stored: 'quem a enviou e para quem foi enviada, com os nomes', notStored: 'O corpo das mensagens e os anexos nunca são guardados.', operator: 'Quem administra este servidor pode ler o banco de dados dele, inclusive este índice.',
    turnOn: 'Ligar a sincronização', notNow: 'Agora não', sheetTurnOn: 'Ligar a sincronização…', off: 'Conectada. A sincronização está desligada.', onDone: 'A sincronização está ligada. O Mailie está indexando suas caixas de email, começando pelos últimos 90 dias.',
    upToDate: 'Em dia', syncNow: 'Sincronizar agora', requested: 'Sincronização pedida. As contagens se atualizam enquanto ela roda.', mailSync: 'Sincronização de emails', offTitle: 'Desligar a sincronização de emails?', offDelete: 'Desligar e apagar',
    offDone: 'A sincronização está desligada. O Mailie apagou o índice dos seus emails.', myAccount: 'Conta', accounts: 'Caixas de email', details: 'Detalhes', showFolders: 'Mostrar pastas',
    fromIndex: 'Do índice do Mailie', notSynced: 'Não sincronizada', expired: 'Sua sessão terminou. Entre novamente.',
    refresh: 'Atualizar', outdated: 'O texto sobre a sincronização mudou neste servidor enquanto esta página estava aberta. Recarregue a página para ler o texto atual.', reload: 'Recarregar a página',
  },
  'de-DE': {
    title: 'E-Mail-Synchronisierung einschalten?', stored: 'wer sie gesendet hat und an wen, mit Namen', notStored: 'Nachrichtentexte und Anhänge werden nie gespeichert.', operator: 'Wer diesen Server betreibt, kann seine Datenbank lesen, einschließlich dieses Index.',
    turnOn: 'Synchronisierung einschalten', notNow: 'Nicht jetzt', sheetTurnOn: 'Synchronisierung einschalten…', off: 'Verbunden. Die Synchronisierung ist ausgeschaltet.', onDone: 'Die Synchronisierung ist eingeschaltet. Mailie indexiert Ihre Postfächer, beginnend mit den letzten 90 Tagen.',
    upToDate: 'Aktuell', syncNow: 'Jetzt synchronisieren', requested: 'Synchronisierung angefordert. Die Zahlen werden während des Laufs aktualisiert.', mailSync: 'E-Mail-Synchronisierung', offTitle: 'E-Mail-Synchronisierung ausschalten?', offDelete: 'Ausschalten und löschen',
    offDone: 'Die Synchronisierung ist ausgeschaltet. Mailie hat den Index Ihrer E-Mails gelöscht.', myAccount: 'Konto', accounts: 'Postfächer', details: 'Details', showFolders: 'Ordner anzeigen',
    fromIndex: 'Aus dem Index von Mailie', notSynced: 'Nicht synchronisiert', expired: 'Ihre Sitzung ist beendet. Melden Sie sich erneut an.',
    refresh: 'Aktualisieren', outdated: 'Der Text zur Synchronisierung hat sich auf diesem Server geändert, während diese Seite geöffnet war. Laden Sie die Seite neu, um den aktuellen Text zu lesen.', reload: 'Seite neu laden',
  },
}


for (const { language, mobile, scheme } of only && only !== 'sync' ? [] : [
  { language: 'en-US', mobile: false, scheme: 'light' },
  { language: 'en-US', mobile: true, scheme: 'dark' },
  { language: 'pt-BR', mobile: false, scheme: 'dark' },
  { language: 'de-DE', mobile: true, scheme: 'light' },
]) {
  const label = `${language}-${mobile ? 'mobile' : 'desktop'}-${scheme}`
  const text = SYNC_TEXT[language]
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: language, colorScheme: scheme, reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
  })
  const daemon = fakeDaemon({ progressMS: 800 })
  const errors = []
  await context.route('**/v1/**', route => daemon.handle(route))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
  }
  const openSection = async name => {
    if (mobile) await page.getByRole('button', { name: /^(Open menu|Abrir menu|Menü öffnen)$/ }).click()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()

    // --- asked before anything is stored ----------------------------------------
    const card = page.locator('.consent-card')
    await card.waitFor()
    assert.equal((await card.locator('h2').textContent()).trim(), text.title)
    const words = await card.textContent()
    for (const item of [text.stored, text.notStored, text.operator]) assert.ok(words.includes(item), `the consent card says: ${item}`)
    await noLegal(page, 'consent card')
    assert.equal(await page.locator('.sync-status').count(), 0, 'nothing claims to sync before consent')
    assert.deepEqual(daemon.calls.consent, [], 'nothing is agreed to by loading the page')
    await shot('sync-consent')
    // "Not now" hides the card for this page only.
    await card.getByRole('button', { name: text.notNow, exact: true }).click()
    await card.waitFor({ state: 'detached' })
    await page.locator('.account-card').filter({ hasText: 'suporte@example.test' }).getByText(text.off).waitFor()
    await page.reload()
    await page.locator('.consent-card').waitFor()
    await page.locator('.consent-card').getByRole('button', { name: text.notNow, exact: true }).click()
    // A mailbox's sheet offers it again, and sends the person to the card.
    const gmail = page.locator('.account-card').filter({ hasText: 'suporte@example.test' })
    await gmail.getByRole('button', { name: text.details, exact: true }).click()
    const sheet = page.locator('.sheet')
    await sheet.getByRole('button', { name: text.sheetTurnOn }).click()
    await page.locator('.consent-card').waitFor()
    await page.waitForFunction(() => document.activeElement?.id === 'sync-consent-title')

    // --- turning it on: the first sync, live -------------------------------------
    const firstStream = daemon.calls.streams.length
    await page.locator('.consent-card').getByRole('button', { name: text.turnOn, exact: true }).click()
    await page.locator('.success.notice').filter({ hasText: text.onDone }).waitFor()
    await page.locator('.consent-card').waitFor({ state: 'detached' })
    assert.deepEqual(daemon.calls.consent, ['grant'])
    await page.waitForFunction(() => {
      const bar = document.querySelector('.account-card progress.sync-progress')
      return bar && bar.value > 0 && bar.value < 100
    }, null, { timeout: 15_000 })
    await shot('sync-progress')
    await gmail.getByText(text.upToDate, { exact: true }).waitFor({ timeout: 25_000 })
    assert.equal(await gmail.locator('progress').count(), 0, 'no progress bar once the first sync is done')
    await gmail.locator('dd', { hasText: /^1[.,]284$/ }).waitFor()
    // An account that needs authorizing says so, not that it syncs.
    assert.equal(await page.locator('.account-card').filter({ hasText: 'ana.souza@example.test' }).locator('.sync-status').count(), 0)
    // New mail, arriving while the page is open.
    daemon.deliver('acc_0000000000000001', 'Lunch on Friday?', { name: 'Bea Lima', email: 'bea@example.com' })
    await gmail.locator('dd', { hasText: /^1[.,]285$/ }).waitFor({ timeout: 10_000 })
    assert.equal(await page.getByText('Lunch on Friday?').count(), 0, 'the accounts page lists no messages; Mail does')
    await shot('sync-live')

    // --- the sheet: sync now, folders from the index --------------------------
    await gmail.getByRole('button', { name: text.details, exact: true }).click()
    await sheet.waitFor()
    await sheet.getByRole('button', { name: text.syncNow }).click()
    await until(() => daemon.calls.syncNow.length === 1, 'the sync request')
    await sheet.getByText(text.requested).waitFor()
    await sheet.getByRole('button', { name: text.showFolders }).click()
    await sheet.getByText(text.fromIndex, { exact: false }).waitFor()
    await sheet.locator('.folder-list li').filter({ hasText: 'All Mail' }).getByText(text.notSynced).waitFor()
    const inbox = sheet.locator('.folder-list li').filter({ hasText: 'INBOX' })
    await inbox.locator('.folder-count', { hasText: /1[.,]245$/ }).waitFor()
    daemon.deliver('acc_0000000000000001', 'Re: Lunch on Friday?', { name: 'Bea Lima', email: 'bea@example.com' })
    await inbox.locator('.folder-count', { hasText: /1[.,]246$/ }).waitFor({ timeout: 15_000 })
    await sheet.locator('.sync-section').scrollIntoViewIfNeeded()
    await shot('sync-sheet')
    await sheet.locator('.folder-list').scrollIntoViewIfNeeded()
    await shot('sync-folders')
    await page.keyboard.press('Escape')
    await sheet.waitFor({ state: 'detached' })

    // Every reconnection resumed after the last event the page had received.
    const streams = daemon.calls.streams.slice(firstStream)
    assert.ok(streams.length >= 3, `the stream reconnected (${streams.length})`)
    for (const [index, entry] of streams.entries()) {
      assert.equal(entry.type, 'fetch', 'the stream is read with fetch, never EventSource')
      assert.match(entry.authorization, /^Bearer \S+$/, 'the stream sends the bearer in its header')
      assert.equal(new URL(entry.url).search, '', 'no token, or anything else, in the stream URL')
      const previous = streams[index - 1]
      if (!previous) continue
      const expected = previous.delivered ? String(previous.delivered) : previous.lastEventID
      assert.equal(entry.lastEventID, expected, `stream ${index} resumes after event ${expected || '(none)'}`)
    }
    assert.ok(streams.some(entry => entry.lastEventID), 'reconnections carry Last-Event-ID')

    // --- turning it off, from My account -----------------------------------------
    await openSection(text.myAccount)
    const toggle = page.getByRole('switch', { name: text.mailSync })
    await toggle.waitFor()
    assert.equal(await toggle.getAttribute('aria-checked'), 'true')
    await toggle.click()
    const confirm = page.getByRole('dialog', { name: text.offTitle })
    await confirm.waitFor()
    await shot('sync-off-confirm')
    await confirm.getByRole('button', { name: text.offDelete }).click()
    await confirm.waitFor({ state: 'hidden' })
    await page.locator('.account-settings .success').filter({ hasText: text.offDone }).waitFor()
    assert.equal(await toggle.getAttribute('aria-checked'), 'false')
    assert.deepEqual(daemon.calls.consent, ['grant', 'withdraw'])
    await shot('sync-off')
    // Turning it back on shows the same text first.
    await toggle.click()
    const again = page.getByRole('dialog', { name: text.title })
    await again.waitFor()
    assert.ok((await again.textContent()).includes(text.stored))
    await noLegal(page, 'consent dialog')
    await shot('sync-on-dialog')
    await again.getByRole('button', { name: text.notNow, exact: true }).click()
    await again.waitFor({ state: 'hidden' })
    assert.deepEqual(daemon.calls.consent, ['grant', 'withdraw'], '"Not now" agrees to nothing')
    await openSection(text.accounts)
    await gmail.getByText(text.off).waitFor()
    assert.equal(await page.locator('.consent-card').count(), 0, 'turning sync off does not ask again straight away')
    assert.equal(await page.locator('.sync-status').count(), 0, 'no card claims to sync after sync was turned off')
    await shot('sync-after-off')

    // --- the policy changes while the page is open ---------------------------------
    // The daemon moves to a newer revision; Refresh reads the consent again. The
    // console agrees only to the text it shows, so it offers a reload instead.
    daemon.policy.version = '2027-01-open-sync-bodies'
    await page.getByRole('button', { name: text.refresh, exact: true }).click()
    await openSection(text.myAccount)
    await toggle.click()
    const outdated = page.getByRole('dialog', { name: text.title })
    await outdated.getByText(text.outdated).waitFor()
    assert.equal(await outdated.getByRole('button', { name: text.turnOn, exact: true }).count(), 0, 'no agreeing to a text the page does not show')
    assert.ok(!(await outdated.textContent()).includes(text.stored), 'the old text is not offered as the terms')
    await shot('sync-outdated-dialog')
    await outdated.getByRole('button', { name: text.reload, exact: true }).click()
    // The same bundle comes back, still with the older text: the card says so again.
    const stale = page.locator('.consent-card')
    await stale.getByText(text.outdated).waitFor()
    assert.equal(await stale.getByRole('button', { name: text.turnOn, exact: true }).count(), 0)
    await stale.getByRole('button', { name: text.reload, exact: true }).waitFor()
    assert.deepEqual(daemon.calls.consent, ['grant', 'withdraw'], 'nothing agreed to a revision the page did not show')
    await shot('sync-outdated-card')
    daemon.policy.version = SYNC_VERSION

    // --- a session revoked while the page sits idle -------------------------------
    daemon.recheck.streams = true
    daemon.sessions.clear()
    await page.locator('form[name=mailie-login]').waitFor({ timeout: 15_000 })
    await page.getByText(text.expired).waitFor()
    assert.equal(await page.getByText('detail the console must never show').count(), 0, 'server messages are never rendered')
    assert.deepEqual(daemon.calls.unoffered, [], 'no request to the mail or sending routes')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok sync ${label}`)
  } catch (error) {
    failures.push(`sync ${label}: ${error.message}`)
    console.log(`FAIL sync ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-sync-${label}.png`) }).catch(() => {})
  } finally {
    daemon.close()
    await context.close()
  }
}


// --- actions on messages ----------------------------------------------------------
// The open console makes no change to a mailbox itself; it lets the person
// allow, or stop, the changes they make themselves on the mailboxes where
// they may act. In Account: the switch asks first, with this server's text
// and no policy linked; allowed, it says so; turned off, after saying what
// stops. A consent to an older text is paused, and the new text is one click
// away.

const ACTIONS_TEXT = {
  'en-US': {
    account: 'Account', switch: 'Actions on my messages', title: 'Allow actions on your messages?', allow: 'Allow actions', lead: 'This server changes a mailbox only when someone allowed to act on it asks: you, under this agreement, on the mailboxes where you may act; anyone else allowed to act there, under their own; or a tool with an API key given Act there, under the terms the key was created with.',
    onDone: 'Actions are on. Mailie changes your mailbox only when you ask.', offTitle: 'Turn off actions?', off: 'Turn off actions', offDone: 'Actions are off. Mailie no longer changes your mailboxes when you ask.',
    reviewAgree: 'Review and agree', agree: 'I agree', renewTitle: 'Actions on your messages: the terms changed', paused: 'Paused: this server’s text about actions changed since you allowed them on',
    openMenu: 'Open menu',
  },
  'pt-BR': {
    account: 'Conta', switch: 'Ações nas minhas mensagens', title: 'Permitir ações nas suas mensagens?', allow: 'Permitir ações', lead: 'Este servidor muda uma caixa de email só quando alguém com permissão para agir nela pede: você, sob este consentimento, nas caixas de email em que pode agir; outra pessoa com permissão para agir ali, sob o consentimento dela; ou uma ferramenta com uma chave de API que recebeu Ações ali, sob os termos com que a chave foi criada.',
    onDone: 'As ações estão ligadas. O Mailie muda sua caixa de email só quando você pede.', offTitle: 'Desligar as ações?', off: 'Desligar as ações', offDone: 'As ações estão desligadas. O Mailie não muda mais suas caixas de email quando você pede.',
    reviewAgree: 'Revisar e concordar', agree: 'Concordo', renewTitle: 'Ações nas suas mensagens: os termos mudaram', paused: 'Pausadas: o texto deste servidor sobre as ações mudou desde que você as permitiu',
    openMenu: 'Abrir menu',
  },
  'de-DE': {
    account: 'Konto', switch: 'Aktionen für meine Nachrichten', title: 'Aktionen für Ihre Nachrichten erlauben?', allow: 'Aktionen erlauben', lead: 'Dieser Server ändert ein Postfach nur, wenn jemand darum bittet, der darin handeln darf: Sie, unter dieser Zustimmung, in den Postfächern, in denen Sie handeln dürfen; jemand anderes, der dort handeln darf, unter seiner eigenen; oder ein Tool mit einem API-Schlüssel, der dort Aktionen erhalten hat, unter den Bedingungen, unter denen der Schlüssel erstellt wurde.',
    onDone: 'Aktionen sind eingeschaltet. Mailie ändert Ihr Postfach nur, wenn Sie es verlangen.', offTitle: 'Aktionen ausschalten?', off: 'Aktionen ausschalten', offDone: 'Aktionen sind ausgeschaltet. Mailie ändert Ihre Postfächer nicht mehr, wenn Sie darum bitten.',
    reviewAgree: 'Prüfen und zustimmen', agree: 'Ich stimme zu', renewTitle: 'Aktionen für Ihre Nachrichten: Die Bedingungen haben sich geändert', paused: 'Pausiert: Der Text dieses Servers zu Aktionen hat sich geändert, seit Sie sie am',
    openMenu: 'Menü öffnen',
  },
}

for (const { language, mobile, scheme, older } of only && only !== 'actions' ? [] : [
  { language: 'en-US', mobile: false, scheme: 'light', older: false },
  { language: 'pt-BR', mobile: true, scheme: 'dark', older: false },
  { language: 'en-US', mobile: true, scheme: 'light', older: true },
  { language: 'de-DE', mobile: false, scheme: 'dark', older: true },
]) {
  const label = `${language}-${mobile ? 'mobile' : 'desktop'}-${scheme}${older ? '-older' : ''}`
  const text = ACTIONS_TEXT[language]
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: language, colorScheme: scheme, reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
  })
  const daemon = fakeDaemon({ consented: true, actionsAgreed: older ? '2026-01-older-actions' : '' })
  const errors = []
  await context.route('**/v1/**', route => daemon.handle(route))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
  }
  const openSection = async name => {
    if (mobile) await page.getByRole('button', { name: text.openMenu }).click()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()
    await openSection(text.account)
    const row = page.locator('.sync-row').filter({ hasText: text.switch })
    await row.waitFor()
    if (older) {
      // Paused, not on: no switch while the server refuses every action.
      await row.getByText(text.paused, { exact: false }).waitFor()
      assert.equal(await row.getByRole('switch').count(), 0, 'no switch that says On while actions are refused')
      await row.scrollIntoViewIfNeeded()
      await shot('actions-paused')
      await row.getByRole('button', { name: text.reviewAgree }).click()
      const renew = page.getByRole('dialog', { name: text.renewTitle })
      await renew.getByText(text.lead).waitFor()
      await noLegal(page, 'actions dialog')
      assert.deepEqual(daemon.calls.actionsConsent, [], 'nothing agreed to before the button')
      await renew.getByRole('button', { name: text.agree }).click()
      await renew.waitFor({ state: 'hidden' })
    } else {
      const toggle = page.getByRole('switch', { name: text.switch })
      assert.equal(await toggle.getAttribute('aria-checked'), 'false')
      await toggle.click()
      const dialog = page.getByRole('dialog', { name: text.title })
      await dialog.getByText(text.lead).waitFor()
      await noLegal(page, 'actions dialog')
      await shot('actions-dialog')
      assert.deepEqual(daemon.calls.actionsConsent, [], 'nothing agreed to before the button')
      await dialog.getByRole('button', { name: text.allow, exact: true }).click()
      await dialog.waitFor({ state: 'hidden' })
    }
    await page.locator('.account-settings .success').filter({ hasText: text.onDone }).waitFor()
    assert.deepEqual(daemon.calls.actionsConsent, ['grant'])
    const toggle = page.getByRole('switch', { name: text.switch })
    assert.equal(await toggle.getAttribute('aria-checked'), 'true')
    await shot('actions-on')

    // Turned off, after saying what stops.
    await openSection(text.account)
    await toggle.click()
    const off = page.getByRole('dialog', { name: text.offTitle })
    await off.waitFor()
    await shot('actions-off-confirm')
    await off.getByRole('button', { name: text.off, exact: true }).click()
    await off.waitFor({ state: 'hidden' })
    await page.locator('.account-settings .success').filter({ hasText: text.offDone }).waitFor()
    assert.equal(await toggle.getAttribute('aria-checked'), 'false')
    assert.deepEqual(daemon.calls.actionsConsent, ['grant', 'withdraw'])
    assert.deepEqual(daemon.calls.unoffered, [], 'no request to the mail or sending routes')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok actions ${label}`)
  } catch (error) {
    failures.push(`actions ${label}: ${error.message}`)
    console.log(`FAIL actions ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-actions-${label}.png`) }).catch(() => {})
  } finally {
    daemon.close()
    await context.close()
  }
}


// --- API keys and connecting an AI assistant ---------------------------------------
// Ana creates a key of her personal workspace for a tool: nothing is asked of
// the daemon before Create key, under this server's text of what the key
// authorizes (no policy linked); the send scope is offered only where the
// server says its keys may send, and what the key holds is ticked mailbox by
// mailbox. The key is shown once: Copy key, and the Claude Code command with
// the key, go to the clipboard (recorded here instead of the machine's own)
// and the command with the key is never drawn. Escape, pressed twice, and a
// tap beside the dialog leave it on screen; Done lets go of it: it is gone
// from the page, and nothing about it reached storage, the address bar or
// history. Then the list; the key's sheet, where a mailbox is given to it and
// its sends are listed; revoking after a confirmation; and the key, revoked,
// among those she created in her account. The connect panel shows the MCP URL
// (this origin's /mcp) and a command with a placeholder where the key goes.

/**
 * The console's catalogs, each key with its translations in the order
 * src/ui/i18n.ts reads them: what a pass in another language expects, said
 * the way the console says it.
 */
const COLUMNS = { 'pt-BR': 0, 'es-ES': 1, 'fr-FR': 2, 'de-DE': 3 }
const localeDir = resolve(fileURLToPath(import.meta.url), '../../../src/ui/locales')
const catalog = Object.assign({}, ...await Promise.all((await readdir(localeDir)).filter(name => name.endsWith('.json'))
  .map(async name => JSON.parse(await readFile(resolve(localeDir, name), 'utf8')))))
function say(language, key, values = {}) {
  const index = COLUMNS[language]
  if (index !== undefined && !catalog[key]) throw new Error(`console.mjs: no translation of ${key}`)
  const text = index === undefined ? key.split('|')[0] : catalog[key][index]
  return text.replace(/\{(\w+)\}/g, (_, name) => String(values[name]))
}
const keysText = language => Object.fromEntries(Object.entries({
  section: 'API keys & MCP', openMenu: 'Open menu', create: 'Create key', createTitle: 'Create an API key', createdTitle: 'Your new API key',
  lead: 'Whoever holds this key can reach the mailboxes it is given through this server’s API and MCP server, until the key expires or is revoked. Give it only to a tool you trust.',
  held: 'This server does not store what it fetches for the tool. So that the tool can pick up a dropped connection, the MCP server holds what it sent in memory only, never on disk, for at most five minutes.',
  days: '30 days', copyKey: 'Copy key', copyCommand: 'Copy the Claude Code command with this key', done: 'Done', revoke: 'Revoke', revokeTitle: 'Revoke this key?',
  revokeKey: 'Revoke key', revoked: 'Revoked', empty: 'No API keys yet', copyURL: 'Copy URL', none: 'None yet', manage: 'Mailboxes and sends…',
  save: 'Save access', noSends: 'This key sent nothing in the last 30 days.', account: 'Account', mine: 'API keys you created',
}).map(([name, key]) => [name, say(language, key)]).concat([
  ['placeholder', `<${say(language, 'your key')}>`],
  ['gone', say(language, '{name} was revoked. A tool using it can no longer reach these mailboxes.', { name: 'Claude Code' })],
]))

for (const { language, mobile, scheme, scope, chosen, served = true, sends = true } of only && only !== 'keys' ? [] : [
  { language: 'en-US', mobile: false, scheme: 'light', scope: 'read', chosen: true },
  { language: 'en-US', mobile: true, scheme: 'dark', scope: 'write', chosen: false },
  { language: 'pt-BR', mobile: false, scheme: 'dark', scope: 'send', chosen: true },
  { language: 'de-DE', mobile: true, scheme: 'light', scope: 'write', chosen: true },
  // A server with MCP over HTTP off (MAIL_MCP_HTTP=false): no MCP address, no command.
  { language: 'en-US', mobile: false, scheme: 'dark', scope: 'read', chosen: false, served: false },
  // A server whose keys do not send (MAIL_KEYS_MAY_SEND=false): no send scope offered.
  { language: 'en-US', mobile: true, scheme: 'light', scope: 'write', chosen: true, sends: false },
]) {
  const label = `${language}-${mobile ? 'mobile' : 'desktop'}-${scheme}-${scope}${served ? '' : '-no-mcp'}${sends ? '' : '-no-send'}`
  const text = keysText(language)
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: language, colorScheme: scheme, reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
  })
  // What the page copies is recorded here, not written to this machine's clipboard.
  await context.addInitScript(() => {
    window.__copied = []
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { window.__copied.push(String(value)) } } })
  })
  const daemon = fakeDaemon({ consented: true, personal: true, mcpHTTP: served, keysSend: sends })
  const errors = []
  await context.route('**/v1/**', route => daemon.handle(route))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
  }
  const openSection = async name => {
    if (mobile) await page.getByRole('button', { name: text.openMenu }).click()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  const copied = () => page.evaluate(() => window.__copied.at(-1) ?? '')
  /** Where /mcp is not served, nothing in the section names it. */
  const noMcp = async where => {
    assert.equal(await page.locator('.keys-section .connect-panel').count(), 0, `${where}: no MCP panel`)
    assert.ok(!(await page.locator('.keys-section').innerText()).includes('/mcp'), `${where}: no MCP address`)
  }
  const endpoint = `${origin}/mcp`
  const workspace = 'wsp_00000000000000a1'
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()
    await openSection(text.section)
    const section = page.locator('.keys-section')
    await section.locator('.empty-card').getByText(text.empty).waitFor()

    // --- how to connect, with a placeholder where the key goes ----------------------
    // Only once the server said it serves /mcp (GET /v1/me/mcp).
    const connect = section.locator('.connect-panel')
    await until(() => daemon.calls.mcp === 1, 'the console to ask whether /mcp is served')
    if (served) {
      await connect.waitFor()
      const [url, command] = await connect.locator('code').allTextContents()
      assert.equal(url, endpoint, 'the MCP URL is this origin’s /mcp')
      assert.equal(command, `claude mcp add --transport http mailie ${endpoint} --header "Authorization: Bearer ${text.placeholder}"`)
      await connect.getByRole('button', { name: text.copyURL, exact: true }).click()
      assert.equal(await copied(), endpoint)
    } else {
      await page.waitForTimeout(300)
      await noMcp('keys-empty')
    }
    await shot('keys-empty')

    // --- the dialog: nothing asked before Create key --------------------------------
    const history = await page.evaluate(() => history.length)
    await section.locator('.empty-card').getByRole('button', { name: text.create, exact: true }).click()
    const dialog = page.getByRole('dialog', { name: text.createTitle })
    await dialog.waitFor()
    assert.equal((await dialog.locator('.key-terms .consent-lead').textContent()).trim(), text.lead)
    assert.ok((await dialog.locator('.key-terms p').allTextContents()).map(line => line.trim()).includes(text.held), 'the terms say what the MCP server holds, and for how long')
    await noLegal(page, `${label} key terms`)
    assert.equal(await dialog.locator('input[value=send]').count(), sends ? 1 : 0, sends ? 'the send scope is offered where keys send' : 'no send scope where keys do not send')
    await dialog.locator('input[name=key-name]').fill('Claude Code')
    await dialog.locator(`input[name=scope][value=${scope}]`).check()
    if (chosen) {
      await dialog.locator(`input[name=${GMAIL_ACCOUNT}-read]`).check()
      if (scope !== 'read') await dialog.locator(`input[name=${GMAIL_ACCOUNT}-act]`).check()
      if (scope === 'send') await dialog.locator(`input[name=${GMAIL_ACCOUNT}-send]`).check()
    }
    await dialog.locator('label.lifetime', { hasText: text.days }).locator('input').check()
    await dialog.locator('.dialog-content').evaluate(content => content.scrollTo(0, 0))
    await shot('keys-create')
    await dialog.locator('.key-terms').scrollIntoViewIfNeeded()
    await shot('keys-create-terms')
    assert.deepEqual(daemon.calls.keys, [], 'nothing asked of the daemon before Create key')

    // --- created: shown once, copied on request --------------------------------------
    await dialog.locator('button[type=submit]').click()
    const shown = page.getByRole('dialog', { name: text.createdTitle })
    await shown.waitFor()
    const [call] = daemon.calls.keys
    const given = chosen ? { mailboxes: [{ account_id: GMAIL_ACCOUNT, read: true, act: scope !== 'read', send: scope === 'send' }] } : {}
    assert.deepEqual(call.body, { name: 'Claude Code', scope, ttl_days: 30, terms_version: KEY_TERMS_VERSION, ...given })
    const secret = call.key
    assert.equal((await shown.locator('.key-secret').textContent()).trim(), secret, 'the key is shown')
    assert.match(await focused(page), new RegExp(text.copyKey), 'the keyboard lands on Copy key')
    await shot('keys-created')
    // Escape by habit, twice (Chrome closes a dialog on a second Escape however
    // its cancel is prevented), and a tap beside the dialog: the key stays.
    await page.keyboard.press('Escape')
    await page.keyboard.press('Escape')
    if (mobile) await page.touchscreen.tap(4, 4)
    else await page.mouse.click(4, 4)
    assert.ok(await shown.isVisible(), 'Escape and a tap beside the dialog leave the key on screen')
    assert.equal((await shown.locator('.key-secret').textContent()).trim(), secret, 'the key is still shown')
    await shown.getByRole('button', { name: text.copyKey, exact: true }).click()
    assert.equal(await copied(), secret)
    if (served) {
      await shown.getByRole('button', { name: text.copyCommand, exact: true }).click()
      assert.equal(await copied(), `claude mcp add --transport http mailie ${endpoint} --header "Authorization: Bearer ${secret}"`)
    } else {
      assert.equal(await shown.getByRole('button', { name: text.copyCommand, exact: true }).count(), 0, 'no Claude Code command where /mcp is not served')
    }
    assert.ok(!(await page.locator('body').innerText()).includes(`Bearer ${secret}`), 'the command with the key is never drawn')
    await shown.getByRole('button', { name: text.done, exact: true }).click()
    await shown.waitFor({ state: 'detached' })

    // --- gone from the page, and never kept ------------------------------------------
    assert.ok(!(await page.content()).includes(secret.split('.')[1]), 'the key is gone from the page')
    const kept = await page.evaluate(async () => ({
      local: JSON.stringify({ ...localStorage }), session: JSON.stringify({ ...sessionStorage }), href: location.href, history: history.length,
      state: JSON.stringify(history.state), caches: 'caches' in window ? await caches.keys() : [],
      databases: indexedDB.databases ? (await indexedDB.databases()).map(item => item.name) : [],
    }))
    for (const where of ['local', 'session', 'href', 'state']) assert.ok(!kept[where].includes(secret.split('.')[1]) && !kept[where].includes(secret.split('.')[0]), `no key in ${where}`)
    assert.equal(kept.history, history, 'no history entry')
    assert.deepEqual(kept.caches, [], 'no Cache API storage')
    assert.deepEqual([...kept.databases].sort(), ['mailie-browser-account', 'mailie-browser-session'], 'IndexedDB holds only the session vault and the account key’s')

    // --- the list, and the key's sheet: a mailbox given, its sends ---------------------
    const prefix = secret.split('.')[0]
    const card = section.locator('.key-card', { hasText: 'Claude Code' })
    await card.waitFor()
    assert.ok((await card.innerText()).includes(chosen ? 'suporte@example.test' : text.none), 'the card names what the key holds')
    await shot('keys-list')
    // On a server whose keys do not send, a key has no sends to show.
    await card.getByRole('button', { name: sends ? text.manage : 'Mailboxes…', exact: true }).click()
    const sheet = page.getByRole('dialog', { name: 'Claude Code' })
    await sheet.waitFor()
    const row = sheet.locator('.key-row[data-account=acc_0000000000000003]')
    await row.locator('input[value=read]').check()
    await row.getByRole('button', { name: text.save, exact: true }).click()
    await until(() => daemon.calls.keys.length === 2, 'the mailbox to be given to the key')
    assert.deepEqual(daemon.calls.keys[1], { method: 'PUT', path: `/v1/workspaces/${workspace}/apikeys/${prefix}/accounts/acc_0000000000000003`, body: { read: true, act: false, send: false } })
    await row.getByRole('button', { name: text.save, exact: true }).waitFor({ state: 'detached' })
    if (scope === 'send') await sheet.getByText(text.noSends).waitFor()
    else assert.equal(await sheet.getByText(text.noSends).count(), 0, 'no sends listed for a key that cannot send')
    await shot('keys-sheet')
    await sheet.getByRole('button', { name: text.done, exact: true }).click()
    await sheet.waitFor({ state: 'detached' })

    // --- revoking after a confirmation ------------------------------------------------
    await card.getByRole('button', { name: text.revoke, exact: true }).click()
    const confirm = page.getByRole('dialog', { name: text.revokeTitle })
    await confirm.waitFor()
    assert.equal(daemon.calls.keys.length, 2, 'nothing revoked before the confirmation')
    await shot('keys-revoke')
    await confirm.getByRole('button', { name: text.revokeKey, exact: true }).click()
    await confirm.waitFor({ state: 'detached' })
    assert.deepEqual(daemon.calls.keys.slice(2), [{ method: 'DELETE', path: `/v1/workspaces/${workspace}/apikeys/${prefix}` }])
    await card.locator('.status-chip', { hasText: text.revoked }).waitFor()
    assert.equal(await card.getByRole('button', { name: text.revoke, exact: true }).count(), 0, 'a revoked key offers no revoke')
    await section.locator('.success', { hasText: text.gone }).waitFor()
    await shot('keys-revoked')
    if (!served) await noMcp('keys-revoked')

    // --- the keys she created, in her account -------------------------------------------
    await openSection(text.account)
    const mine = page.locator('.my-keys')
    await mine.getByText(text.mine).waitFor()
    await mine.locator('.my-key', { hasText: 'Claude Code' }).locator('.status-chip', { hasText: text.revoked }).waitFor()
    await mine.scrollIntoViewIfNeeded()
    await shot('keys-account')
    assert.equal(daemon.calls.mcp, 1, 'asked once whether /mcp is served')
    assert.deepEqual(daemon.calls.unoffered, [], 'no request to the mail or sending routes')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok keys ${label}`)
  } catch (error) {
    failures.push(`keys ${label}: ${error.message}`)
    console.log(`FAIL keys ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-keys-${label}.png`) }).catch(() => {})
  } finally {
    daemon.close()
    await context.close()
  }
}


// --- storage ------------------------------------------------------------------------
// What each mailbox takes up in the index, read when the section is first
// shown: a mailbox whose account needs authorizing again keeps what was
// indexed and says sync is paused; the totals add up; an owner is told the
// database's size; Refresh reads it again as the first sync of a new mailbox
// runs; turning sync off in Account reads it again without a Refresh, and
// every mailbox then says nothing is indexed. Only GET.

const STORAGE_TEXT = {
  'en-US': { section: 'Storage', openMenu: 'Open menu', byMailbox: 'By mailbox', indexed: 'Messages indexed', database: 'Database on disk', refresh: 'Refresh', notSynced: 'Not synced: nothing from this mailbox is indexed.', paused: 'Sync is paused until the account works again. What was indexed is kept.' },
  'pt-BR': { section: 'Armazenamento', openMenu: 'Abrir menu', byMailbox: 'Por caixa de email', indexed: 'Mensagens indexadas', database: 'Banco de dados em disco', refresh: 'Atualizar', notSynced: 'Não sincronizada: nada desta caixa de email está indexado.', paused: 'A sincronização está pausada até a conta voltar a funcionar. O que foi indexado é mantido.' },
  'de-DE': { section: 'Speicher', openMenu: 'Menü öffnen', byMailbox: 'Nach Postfach', indexed: 'Indexierte Nachrichten', database: 'Datenbank auf der Festplatte', refresh: 'Aktualisieren', notSynced: 'Nicht synchronisiert: Aus diesem Postfach ist nichts indexiert.', paused: 'Die Synchronisierung ist pausiert, bis das Konto wieder funktioniert. Bereits Indexiertes bleibt erhalten.' },
}

for (const { language, mobile, scheme } of only && only !== 'storage' ? [] : [
  { language: 'en-US', mobile: false, scheme: 'light' },
  { language: 'en-US', mobile: true, scheme: 'dark' },
  { language: 'pt-BR', mobile: false, scheme: 'dark' },
  { language: 'de-DE', mobile: true, scheme: 'light' },
]) {
  const label = `${language}-${mobile ? 'mobile' : 'desktop'}-${scheme}`
  const text = STORAGE_TEXT[language]
  const context = await browser.newContext({
    serviceWorkers: 'block', locale: language, colorScheme: scheme, reducedMotion: 'reduce',
    viewport: mobile ? { width: 390, height: 844 } : { width: 1360, height: 900 }, isMobile: mobile, hasTouch: mobile,
  })
  const daemon = fakeDaemon({ consented: true, progressMS: 400 })
  const errors = []
  const methods = []
  await context.route('**/v1/**', route => { if (new URL(route.request().url()).pathname === '/v1/me/storage') methods.push(route.request().method()); return daemon.handle(route) })
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'error' && /Content Security Policy|Refused to|TypeError|Uncaught/.test(message.text())) errors.push(message.text()) })
  const shot = async name => {
    await noHorizontalOverflow(page, `${label} ${name}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `${name}-${label}.png`) })
  }
  const openSection = async name => {
    if (mobile) await page.getByRole('button', { name: text.openMenu }).click()
    await page.locator('.console-nav:visible').getByRole('button', { name: new RegExp(`^${name}`) }).click()
  }
  const figures = async email => (await page.locator('.storage-list li').filter({ hasText: email }).locator('dd').allTextContents()).map(item => item.trim())
  try {
    await page.goto(origin + '/')
    await page.locator('input[name=username]').fill('ana@example.test')
    await page.locator('input[name=password]').fill(PASSWORD)
    await page.locator('form[name=mailie-login] button[type=submit]').click()
    await page.locator('.account-card').first().waitFor()
    assert.equal(daemon.calls.storage, 0, 'nothing asked before the section is shown')
    await openSection(text.section)
    const section = page.locator('.storage-section')
    await section.getByRole('heading', { name: text.byMailbox }).waitFor()
    await section.locator('.storage-list li').nth(2).waitFor()
    // By address, as the daemon lists them.
    const emails = (await section.locator('.storage-list li strong').allTextContents()).map(item => item.trim())
    assert.deepEqual(emails, ['ana.souza@example.test', 'contato@<b>loja</b>.example.test', 'suporte@example.test'], 'every mailbox, by address, as text')
    assert.match((await figures('suporte@example.test'))[0], /^1[.,]284$/)
    // The Microsoft mailbox needs authorizing: sync is paused, and what it indexed before is kept and counted.
    const microsoft = section.locator('.storage-list li').filter({ hasText: 'ana.souza@example.test' })
    await microsoft.getByText(text.paused).waitFor()
    assert.equal(await microsoft.getByText(text.notSynced).count(), 0, 'a mailbox whose index is kept never says nothing is indexed')
    assert.equal((await figures('ana.souza@example.test'))[0], '90')
    assert.equal(await section.getByText(text.notSynced).count(), 0, 'every mailbox here syncs')
    const overview = section.locator('.console-overview')
    await overview.getByText(text.indexed, { exact: true }).waitFor()
    assert.match(await overview.locator('article').nth(1).locator('strong').innerText(), /^1[.,]730$/, 'the total is the sum')
    await overview.getByText(text.database, { exact: true }).waitFor()
    await shot('storage')

    // A new mailbox: its first sync runs, and Refresh shows what it indexed so far.
    const added = { id: 'acc_0000000000000009', email: 'nova@example.test', provider: 'imap', auth_kind: 'password', state: 'active', save_sent_copy: true, created_at: now(), sync: { state: 'live', tier: 'condstore', folders_synced: 5, folders_total: 5, messages: 42, initial_progress: 100, last_synced_at: now() }, target: 42 }
    daemon.accountsByUser.get('usr_00000000000000a1').push(added)
    const before = daemon.calls.storage
    await section.getByRole('button', { name: text.refresh, exact: true }).click()
    await section.locator('.storage-list li').filter({ hasText: 'nova@example.test' }).waitFor()
    assert.equal(daemon.calls.storage, before + 1, 'Refresh reads it once')
    assert.match((await figures('nova@example.test'))[0], /^42$/)

    // Sync turned off in Account deletes the index: Storage reads it again on its own.
    const sync = SYNC_TEXT[language]
    const read = daemon.calls.storage
    await openSection(sync.myAccount)
    await page.getByRole('switch', { name: sync.mailSync }).click()
    const confirm = page.getByRole('dialog', { name: sync.offTitle })
    await confirm.getByRole('button', { name: sync.offDelete }).click()
    await confirm.waitFor({ state: 'hidden' })
    await page.locator('.account-settings .success').filter({ hasText: sync.offDone }).waitFor()
    await until(() => daemon.calls.storage > read, 'Storage to read again after sync was turned off')
    await openSection(text.section)
    await until(async () => (await section.getByText(text.notSynced).count()) === 4, 'every mailbox to say nothing is indexed')
    for (const email of emails.concat('nova@example.test')) assert.equal((await figures(email))[0], '0', `${email} has nothing indexed`)
    assert.equal(await overview.locator('article').nth(1).locator('strong').innerText(), '0', 'nor in total')
    assert.equal(await section.getByText(text.paused).count(), 0, 'nothing is paused once sync is off')
    await shot('storage-sync-off')
    assert.ok(methods.every(method => method === 'GET'), 'Storage only reads')
    assert.deepEqual(daemon.calls.unoffered, [], 'no request to the mail or sending routes')
    assert.deepEqual(errors, [], 'no page, script or CSP errors')
    console.log(`ok storage ${label}`)
  } catch (error) {
    failures.push(`storage ${label}: ${error.message}`)
    console.log(`FAIL storage ${label}: ${error.stack}`)
    if (screenshots) await page.screenshot({ path: resolve(screenshots, `failure-storage-${label}.png`) }).catch(() => {})
  } finally {
    daemon.close()
    await context.close()
  }
}

await browser.close()
if (failures.length) { console.error(failures.join('\n')); process.exit(1) }
