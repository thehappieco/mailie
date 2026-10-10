// The sign-in card as a person uses it, on a page (test/dom.ts), against the
// in-memory server half of test/accountServer.ts: what it says while it
// works, that the password stays here, which holds now that no sign-in
// sends one, and what it says when a recovery went through but the sign-in
// after it did not.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, fill, find, flush, submit, words, type FakeElement } from './dom'
import { mount, type Mounted } from './mount'
import SignInView from '../src/components/SignInView.vue'
import { enrol } from '../src/crypto/account'
import { recoveryCode } from '../src/state/account'
import { accountServer, DEFAULT_KDF, targetOf, type Stored } from './accountServer'
import { failure, json, stubPage } from './support'

const NEVER_SENT = 'Your password is processed here, in this browser, and never sent.'

/** The browser's FormData over test/dom.ts's form: the named fields' values, as SignInView reads them at submit. */
class FormOf {
  private readonly fields = new Map<string, string>()
  constructor(form: FakeElement) {
    for (const field of form.querySelectorAll('input')) {
      const name = field.getAttribute('name')
      if (name) this.fields.set(name, field.value)
    }
  }
  has(name: string): boolean { return this.fields.has(name) }
  get(name: string): string | null { return this.fields.get(name) ?? null }
}

let mounted: Mounted | null = null
beforeEach(() => {
  stubPage()
  vi.stubGlobal('FormData', FormOf)
})
afterEach(() => {
  mounted?.unmount()
  mounted = null
  Object.assign(recoveryCode, { code: '', reason: '' })
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the sign-in card', { timeout: 30_000 }, () => {
  it('says the password is never sent while a sign-in works, and sends only its auth key, whatever the challenge answers', async () => {
    const server = accountServer()
    const old: Stored = {
      id: 'usr_00000000000000c3', email: 'old@example.test', name: 'Old', sealID: '6b0d2f4e-9a1c-4e7b-8d35-0c2a7f9e1b64', salt: '',
      kdf: DEFAULT_KDF, authKey: '', passwordWrap: '', recoveryWrap: '', recoveryProof: '',
    }
    server.people.set(old.email, old)
    // A person from before the key scheme who never enrolled, and a challenge as the release that brought the scheme answered it.
    server.refuseNext.set('/v1/auth/challenge', () => json({ salt: targetOf(old.email), kdf: DEFAULT_KDF, upgrade: true }))
    // The sign-in's answer held while the page waits for it.
    let release: () => void = () => {}
    server.inFlight.set('/v1/auth/login', () => new Promise<void>(resolve => { release = resolve }))
    mounted = mount(SignInView, { invitation: null })
    await flush()
    await fill(find('input[name=username]'), old.email)
    await fill(find('input[name=password]'), 'my old password')
    await submit(find('form'))
    await vi.waitFor(() => expect(server.paths()).toContain('/v1/auth/login'))
    await flush()
    expect(words()).toContain(NEVER_SENT)
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/login'])
    expect(server.calls.some(call => JSON.stringify(call.body).includes('my old password'))).toBe(false)
    release()
    await vi.waitFor(() => expect(find('[role=alert]')).not.toBeNull())
    expect(words(find('[role=alert]')!)).toBe('The email or password is incorrect.')
  })

  it('says a recovery is done, and to sign in with the new password, when the sign-in after it fails', async () => {
    const made = await enrol('correct horse battery staple', { salt: targetOf('ana@example.test'), kdf: DEFAULT_KDF, seal_id: 'b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4' })
    const e = made.enrolment
    const server = accountServer()
    server.people.set('ana@example.test', {
      id: 'usr_00000000000000a1', email: 'ana@example.test', name: 'Ana', sealID: 'b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4', publicKey: e.public_key,
      salt: targetOf('ana@example.test'), kdf: DEFAULT_KDF, authKey: e.auth_key, passwordWrap: e.password_wrap, recoveryWrap: e.recovery_wrap,
      recoveryProof: e.recovery_proof,
    })
    let proceed: () => void = () => {}
    server.inFlight.set('/v1/auth/recover/open', () => new Promise<void>(resolve => { proceed = resolve }))
    server.refuseNext.set('/v1/auth/login', () => failure('rate_limited', 429))
    mounted = mount(SignInView, { invitation: null })
    await flush()
    await click(find('button', 'Forgot your password?'))
    await fill(find('input[name=username]'), 'ana@example.test')
    await fill(find('input[name=recovery-code]'), made.recoveryCode)
    await fill(find('input[name=password]'), 'my brand new password')
    await fill(find('input[name=confirm-password]'), 'my brand new password')
    await submit(find('form'))
    // Choosing a new password, the claim holds: nothing of it is sent.
    await vi.waitFor(() => expect(server.paths()).toContain('/v1/auth/recover/open'))
    await flush()
    expect(words()).toContain(NEVER_SENT)
    proceed()

    await vi.waitFor(() => expect(words()).toContain('Your account is recovered. Sign in with your new password.'), { timeout: 20_000 })
    expect(server.paths()).toEqual(['/v1/auth/recover/open', '/v1/auth/recover/finish', '/v1/auth/login'])
    expect(words(find('h1')!)).toBe('Sign in')
    expect(find('[role=alert]')).toBeNull()
    expect(recoveryCode.reason).toBe('recovered')
  })
})
