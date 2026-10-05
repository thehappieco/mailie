import { afterEach, describe, expect, it, vi } from 'vitest'
import { configureEdition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import { draftRequest, emptyDraft, guessHosts, isICloudAddress, portForSecurity, signsInWithPassword } from '../src/ui/accountDraft'
import { describe as describeFailure, describeFolders } from '../src/ui/errors'
import { countdown, initials } from '../src/ui/format'
import { locale, type Locale } from '../src/ui/i18n'
import { needsAuthorization, providerChoice, providerIcon, providerName, stateDetail, stateTone, syncTierLabel } from '../src/ui/labels'
import { readFileSync, readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { REASON_MAILBOX_REFUSED, REASON_NOT_GRANTED, REASON_TOKEN_REJECTED, reasonText } from '../src/ui/reasons'
import { signupLink, takeInvitation } from '../src/ui/signupLink'
import { account, ORIGIN, syncing } from './support'

afterEach(() => { locale.value = 'en'; vi.unstubAllGlobals() })

const INVITE = 'SyntheticInviteCode_0123456789abcdefghijklmn'

describe('invitation links', () => {
  it('take the code and address from the fragment and hand back an address without them', () => {
    const found = signupLink(`${ORIGIN}/#invite=${INVITE}&email=new%40example.test&keep=1`)
    expect(found).toEqual({ invite: INVITE, email: 'new@example.test', cleanURL: `${ORIGIN}/#keep=1` })
  })
  it('ignore a code in the query, where it would reach server logs', () => {
    expect(signupLink(`${ORIGIN}/?invite=${INVITE}`).invite).toBe('')
  })
  it('refuse a malformed code', () => {
    expect(signupLink(`${ORIGIN}/#invite=%3Cscript%3E`).invite).toBe('')
  })
  it('are removed from the address bar the moment the page reads them', () => {
    const history = { state: null, replaceState: vi.fn() }
    vi.stubGlobal('history', history)
    vi.stubGlobal('location', new URL(`${ORIGIN}/#invite=${INVITE}&email=new%40example.test`))
    expect(takeInvitation()).toEqual({ invite: INVITE, email: 'new@example.test' })
    expect(history.replaceState).toHaveBeenCalledWith(null, '', `${ORIGIN}/`)
  })
})

describe('the add-account form', () => {
  it('sends only what the person filled, and nothing IMAP-shaped for an OAuth provider', () => {
    const draft = { ...emptyDraft(), email: ' ana@example.test ', password: 'secret', imapHost: 'imap.example.test' }
    expect(draftRequest('gmail', draft)).toEqual({ email: 'ana@example.test', provider: 'gmail' })
    expect(draftRequest('imap', { ...draft, smtpHost: 'smtp.example.test', displayName: ' Vendas ' })).toEqual({
      email: 'ana@example.test', provider: 'imap', display_name: 'Vendas', password: 'secret',
      imap_host: 'imap.example.test', imap_port: 993, smtp_host: 'smtp.example.test', smtp_port: 465, smtp_tls: 'implicit',
    })
  })
  it('sends an iCloud address with its password and nothing server-shaped, since the server fills in Apple’s servers', () => {
    const draft = { ...emptyDraft(), email: ' ana@icloud.com ', password: 'abcd-efgh-ijkl-mnop', displayName: ' Pessoal ', imapHost: 'imap.elsewhere.example', smtpHost: 'smtp.elsewhere.example' }
    expect(draftRequest('icloud', draft)).toEqual({ email: 'ana@icloud.com', provider: 'icloud', display_name: 'Pessoal', password: 'abcd-efgh-ijkl-mnop' })
  })
  it('sends the iCloud address a custom domain signs in with, since Apple refuses the custom-domain one', () => {
    const draft = { ...emptyDraft(), email: 'ana@lima.example', password: 'abcd-efgh-ijkl-mnop', imapHost: 'imap.elsewhere.example', loginUser: ' ana@icloud.com ' }
    expect(draftRequest('icloud', draft)).toEqual({ email: 'ana@lima.example', provider: 'icloud', password: 'abcd-efgh-ijkl-mnop', login_user: 'ana@icloud.com' })
    expect(draftRequest('icloud', { ...draft, loginUser: '  ' })).not.toHaveProperty('login_user')
  })
  it('tests a password only for IMAP and iCloud; the others sign in at the provider', () => {
    expect((['gmail', 'microsoft', 'icloud', 'imap'] as const).filter(signsInWithPassword)).toEqual(['icloud', 'imap'])
  })
  it('recognizes Apple’s own addresses, and nothing that merely looks like them', () => {
    for (const email of ['ana@icloud.com', ' Ana@Me.com ', 'ana@MAC.COM']) expect(isICloudAddress(email), email).toBe(true)
    for (const email of ['ana@icloud.com.br', 'ana@mail.icloud.com', 'icloud.com', 'me.com@example.com', 'ana@example.com', '']) expect(isICloudAddress(email), email).toBe(false)
  })
  it('suggests conventional server names only for a plausible domain', () => {
    expect(guessHosts('vendas@Loja.Example.com')).toEqual({ imap: 'imap.loja.example.com', smtp: 'smtp.loja.example.com' })
    expect(guessHosts('vendas@localhost')).toEqual({ imap: '', smtp: '' })
    expect(guessHosts('ana@icloud.com')).toEqual({ imap: '', smtp: '' })
    expect(guessHosts('no-at-sign')).toEqual({ imap: '', smtp: '' })
  })
  it('moves the SMTP port with its security unless the person chose their own', () => {
    expect(portForSecurity(465, 'implicit', 'starttls')).toBe(587)
    expect(portForSecurity(2525, 'implicit', 'starttls')).toBe(2525)
  })
})

describe('provider names', () => {
  it('call iCloud by its product name, in the form Apple uses in each language, with a generic icon', () => {
    expect(providerName('icloud')).toBe('iCloud')
    expect(providerChoice('icloud')).toBe('iCloud Mail')
    expect(providerIcon('icloud')).toBe('cloud')
    expect(providerIcon('imap')).toBe('server')
    locale.value = 'pt'
    expect(providerName('icloud')).toBe('iCloud')
    expect(providerChoice('icloud')).toBe('Mail do iCloud')
    locale.value = 'fr'
    expect(providerChoice('icloud')).toBe('Mail iCloud')
  })
})

describe('account states', () => {
  it('offer re-authorization only for OAuth accounts that need it', () => {
    expect(needsAuthorization(account({ state: 'needs_reauth' }))).toBe(true)
    expect(needsAuthorization(account({ state: 'active' }))).toBe(false)
    expect(needsAuthorization(account({ state: 'error', auth_kind: 'password', provider: 'imap' }))).toBe(false)
  })
  it('are described in the console’s words, never in the server’s', () => {
    const detail = stateDetail(account({ state: 'error', state_reason: 'invalid_grant: token for user@x revoked', last_error: 'upstream: 535 5.7.8' }))
    expect(detail).toBe('The last attempt to connect failed. Try the authorization again.')
    expect(detail).not.toContain('invalid_grant')
    expect(detail).not.toContain('535')
    expect(stateTone('pending_auth')).toBe('warn')
  })
  it('say what to do when the provider or the mail server refused the grant, on the card and in the sheet', () => {
    const refused = account({ email: 'ana@gmail.example', state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED })
    expect(stateDetail(refused)).toBe('The mail server refused this authorization. Try again and choose the account ana@gmail.example on Google’s sign-in screen.')
    expect(needsAuthorization(refused)).toBe(true)
    const notGranted = account({ state: 'error', state_reason: REASON_NOT_GRANTED })
    expect(stateDetail(notGranted)).toBe('Google did not give Mailie access to this mailbox. Try again and, on Google’s screen, allow access to Gmail (tick the box if one is shown).')
    expect(needsAuthorization(notGranted)).toBe(true)
    // A reason left on an account that works again says nothing about it.
    expect(stateDetail(account({ state: 'active', state_reason: REASON_NOT_GRANTED }))).toBe('Connected. Sync is off.')
  })
  it('tell someone who does not manage the mailbox that someone who does has to authorize it, never to do it themselves', () => {
    const uses = { read: true, act: true, send: true, manage: false }
    expect(stateDetail(account({ state: 'pending_auth', access: uses }))).toBe('Authorization was not finished. Someone who manages this mailbox has to finish it.')
    expect(stateDetail(account({ state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED, access: uses }))).toBe('The provider asks for this mailbox to be authorized again, by someone who manages it. Mail is not syncing.')
    expect(stateDetail(account({ state: 'error', access: uses }))).toBe('The last attempt to connect failed. Someone who manages this mailbox has to authorize it again.')
    // Whoever manages it is told what to do, and so is anyone on a server older than grants.
    expect(stateDetail(account({ state: 'pending_auth', access: { ...uses, manage: true } }))).toBe('Authorization was not finished. Finish it, or remove this account.')
    expect(stateDetail(account({ state: 'pending_auth' }))).toBe('Authorization was not finished. Finish it, or remove this account.')
    // A password account is not authorized again by anyone.
    expect(stateDetail(account({ state: 'error', auth_kind: 'password', provider: 'imap', access: uses }))).toBe('The last attempt to connect failed. Check the account at the provider.')
  })
  it('say whether sync is on, and never promise one the person has not turned on', () => {
    expect(stateDetail(account({ state: 'active' }))).toBe('Connected. Sync is off.')
    expect(stateDetail(account({ state: 'active', sync: syncing() }))).toBe('Connected. Sync is on.')
    expect(syncTierLabel(undefined)).toBe('')
    expect(syncTierLabel('uidpoll')).toBe('Periodic check')
  })
})

describe('the reasons the console explains itself', () => {
  const locales: Locale[] = ['en', 'pt', 'es', 'fr', 'de']
  const cases = [
    { provider: 'gmail', state_reason: REASON_NOT_GRANTED, en: 'Google did not give Mailie access to this mailbox. Try again and, on Google’s screen, allow access to Gmail (tick the box if one is shown).' },
    { provider: 'microsoft', state_reason: REASON_NOT_GRANTED, en: 'Microsoft did not give Mailie access to this mailbox. Try again and accept every permission on Microsoft’s screen. For a work or school account, an administrator may have to allow Mailie first.' },
    { provider: 'imap', state_reason: REASON_NOT_GRANTED, en: 'The provider did not give Mailie access to this mailbox. Try again and allow access to the mailbox on the provider’s screen.' },
    { provider: 'gmail', state_reason: REASON_MAILBOX_REFUSED, en: 'The mail server refused this authorization. Try again and choose the account ana@example.test on Google’s sign-in screen.' },
    { provider: 'microsoft', state_reason: REASON_MAILBOX_REFUSED, en: 'The mail server refused this authorization. Try again and choose the account ana@example.test on Microsoft’s sign-in screen. If you already did, check that IMAP is turned on for this mailbox.' },
    { provider: 'imap', state_reason: REASON_MAILBOX_REFUSED, en: 'The mail server refused this authorization. Try again and choose the account ana@example.test on the provider’s sign-in screen.' },
  ] as const

  it('are fixed strings the daemon writes, matched exactly', () => {
    // The daemon's own constants, so a reason renamed there fails here rather
    // than quietly falling back to the state's general text.
    const dir = fileURLToPath(new URL('../../internal/account/', import.meta.url))
    const source = readdirSync(dir).filter(name => name.endsWith('.go') && !name.endsWith('_test.go')).map(name => readFileSync(dir + name, 'utf8')).join('\n')
    for (const reason of [REASON_NOT_GRANTED, REASON_MAILBOX_REFUSED, REASON_TOKEN_REJECTED]) expect(source, reason).toContain(JSON.stringify(reason))
    for (const other of ['consent was declined', 'The provider did not grant access to the mailbox', `${REASON_NOT_GRANTED}.`, '', undefined]) {
      expect(reasonText(account({ state: 'error', state_reason: other })), String(other)).toBeUndefined()
    }
  })

  it('ask a working account the mail server stopped taking to be authorized again as the right account', () => {
    const rejected = account({ email: 'ana@gmail.example', state: 'needs_reauth', state_reason: REASON_TOKEN_REJECTED })
    expect(reasonText(rejected)).toBe(reasonText({ ...rejected, state_reason: REASON_MAILBOX_REFUSED }))
    expect(stateDetail(rejected)).toContain('choose the account ana@gmail.example on Google’s sign-in screen')
  })

  it('are said in each of the five languages, naming the provider’s screen and the address to choose', () => {
    for (const item of cases) {
      const failing = account({ provider: item.provider, email: 'ana@example.test', state: 'needs_reauth', state_reason: item.state_reason })
      const seen = new Set<string>()
      for (const language of locales) {
        locale.value = language
        const text = reasonText(failing)!
        expect(text, `${language} ${item.provider} ${item.state_reason}`).toBeTruthy()
        if (language === 'en') expect(text).toBe(item.en)
        if (item.state_reason === REASON_MAILBOX_REFUSED) expect(text, language).toContain('ana@example.test')
        if (item.provider === 'gmail') expect(text, language).toContain('Google')
        if (item.provider === 'microsoft') expect(text, language).toContain('Microsoft')
        seen.add(text)
      }
      // Translated, not the English falling through.
      expect(seen.size, `${item.provider} ${item.state_reason}`).toBe(locales.length)
    }
  })

  it('explain only an account that is failing', () => {
    for (const state of ['active', 'pending_auth', 'disabled'] as const) {
      expect(reasonText(account({ state, state_reason: REASON_MAILBOX_REFUSED })), state).toBeUndefined()
    }
    expect(reasonText(account({ state: 'error', state_reason: REASON_MAILBOX_REFUSED }))).toBeDefined()
  })

  it('are in Portuguese what the person reads in the console', () => {
    locale.value = 'pt'
    expect(reasonText(account({ state: 'error', state_reason: REASON_NOT_GRANTED }))).toBe('O Google não deu ao Mailie acesso a esta caixa de email. Tente novamente e, na tela do Google, permita o acesso ao Gmail (marque a caixa, se aparecer uma).')
    expect(reasonText(account({ email: 'ana@gmail.example', state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED }))).toBe('O servidor de email recusou esta autorização. Tente novamente e escolha a conta ana@gmail.example na tela de acesso do Google.')
  })
})

describe('failure text', () => {
  it('depends on what was being done, not only on the code', () => {
    expect(describeFailure({ op: 'password', code: 'not_authorized' })).toBe('The current password is incorrect.')
    expect(describeFailure({ op: 'sign-up', code: 'not_authorized' })).toContain('invitation')
    expect(describeFailure({ op: 'profile', code: 'not_authorized' })).toBe('Your account is not allowed to do this.')
    // A grant without read: the mailbox is seen, its folders are not.
    expect(describeFailure({ op: 'folders', code: 'not_authorized' })).toBe('You do not have read access to this mailbox. It comes only from someone who has it and can change who has access.')
    locale.value = 'pt'
    expect(describeFailure({ op: 'sign-in', code: 'unauthorized' })).toBe('O email ou a senha estão incorretos.')
  })
  it('words a refused sign-up invitation by whether people join teams here, signed in', () => {
    expect(describeFailure({ op: 'sign-up', code: 'not_authorized' })).toBe('This invitation cannot create an account for this address. It may have expired or been used, or it invites you to a team: sign in to the account you already have, and open the link again.')
    try {
      // Where workspaces come from elsewhere an invitation only ever creates an account: no step to send anyone to.
      configureEdition({ ...openEdition, teams: false })
      expect(describeFailure({ op: 'sign-up', code: 'not_authorized' })).toBe('This invitation cannot create an account for this address. It may have expired or been used: ask for a new one.')
    } finally {
      configureEdition(openEdition)
    }
  })
  it('says that the lists wait for the workspaces, whatever kept them from loading', () => {
    const waiting = 'Could not load your workspaces. Your mailboxes and what they take up are shown once they load: Mailie tries again by itself, or you can try now.'
    for (const code of ['unavailable', 'internal', 'rate_limited', 'invalid_response'] as const) expect(describeFailure({ op: 'load-workspaces', code })).toBe(waiting)
    expect(describeFailure({ op: 'load-workspaces', code: 'unauthorized' })).toBe('Your session ended. Sign in again.')
  })
  it('names the app-specific password when iCloud refuses the sign-in, and keeps the general texts for the rest', () => {
    expect(describeFailure({ op: 'test-login-icloud', code: 'bad_request' })).toBe('iCloud refused the sign-in. Check the address and that you used an app-specific password, not your Apple Account password.')
    expect(describeFailure({ op: 'test-login', code: 'bad_request' })).toContain('the server names')
    expect(describeFailure({ op: 'test-login-icloud', code: 'conflict' })).toBe('This address is already connected in this workspace.')
    expect(describeFailure({ op: 'test-login-icloud', code: 'not_authorized' })).toBe('Your account is not allowed to connect mailboxes here. In a team, only its owners and admins connect them.')
    expect(describeFailure({ op: 'test-login-icloud', code: 'unavailable' })).toBe('Could not reach the server. Check your connection and try again.')
    locale.value = 'pt'
    expect(describeFailure({ op: 'test-login-icloud', code: 'bad_request' })).toBe('O iCloud recusou o acesso. Confira o endereço e se você usou uma senha específica de app, e não a senha da sua Conta Apple.')
  })
  it('says the reason a failing account records instead of the operation’s own text', () => {
    const left = { provider: 'gmail', email: 'ana@gmail.example', state: 'error', state_reason: REASON_NOT_GRANTED } as const
    expect(describeFailure({ op: 'wait-auth', code: 'flow_failed', account: left })).toBe(reasonText(left))
    expect(describeFailure({ op: 'complete-auth', code: 'bad_request', account: left })).toContain('allow access to Gmail')
    // Any other reason keeps the operation's text, and never shows the server's.
    const other = { ...left, state_reason: 'invalid_grant for token=abc' }
    expect(describeFailure({ op: 'complete-auth', code: 'bad_request', account: other })).toBe('The provider did not grant access. You can try again from the account.')
    expect(describeFailure({ op: 'wait-auth', code: 'flow_failed', account: other })).toBe('The provider did not complete the authorization. Try again.')
  })
  it('names the cause of a refused folder listing from the account, since the server answers 409 for all of them', () => {
    const conflict = { op: 'folders', code: 'conflict' } as const
    const password = account({ provider: 'imap', auth_kind: 'password', state: 'active' })
    expect(describeFolders(conflict, password)).toContain('did not accept the saved password')
    expect(describeFolders(conflict, password)).not.toContain('authorized again')
    expect(describeFolders(conflict, account({ state: 'pending_auth' }))).toBe('Finish the authorization before listing folders.')
    expect(describeFolders(conflict, account({ state: 'needs_reauth' }))).toBe('This account needs to be authorized again before its folders can be listed.')
    expect(describeFolders(conflict, account({ state: 'active' }))).toContain('Check that IMAP is enabled')
    // The status above it says the reason; the listing says what it needs.
    expect(describeFolders(conflict, account({ state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED }))).toBe('This account needs to be authorized again before its folders can be listed.')
    expect(describeFolders(conflict, account({ state: 'error', state_reason: REASON_NOT_GRANTED }))).toBe('This account needs to be authorized again before its folders can be listed.')
    expect(describeFolders({ op: 'folders', code: 'unavailable' }, password)).toBe('Could not list the folders. The mail server may be slow or unreachable.')
  })
})

describe('small formats', () => {
  it('count down to a deadline without going negative', () => {
    expect(countdown(1000, 1000 * 1000 - 61_000)).toBe('1:01')
    expect(countdown(1000, 2000 * 1000)).toBe('0:00')
  })
  it('make initials from a name or an address', () => {
    expect(initials('Ana Souza')).toBe('AS')
    expect(initials('bruno.lima@example.test')).toBe('BL')
    expect(initials('')).toBe('?')
  })
})

describe('an ordinary address', () => {
  it('is left alone: only an invitation fragment is rewritten', () => {
    const history = { state: null, replaceState: vi.fn() }
    vi.stubGlobal('history', history)
    vi.stubGlobal('location', new URL(`${ORIGIN}/#console-content`))
    expect(takeInvitation()).toBeNull()
    expect(history.replaceState).not.toHaveBeenCalled()
  })
})
