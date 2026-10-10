// The console's half of the key scheme's ceremonies (docs/key-scheme.md
// section 12), against the in-memory server half of test/accountServer.ts,
// with the real derivation (Argon2id at the platform's floor), the real
// account wraps and the real browser vault: what is sent, what is never sent,
// and what this browser keeps.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { toBase64URL } from '@thehappieco/kit/bytes'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { IDBObjectStore } from 'fake-indexeddb'
import { deriveKeys, enrol, openWrap, recoveryKeys, type Enrolled } from '../src/crypto/account'
import { sealBrowserVault } from '../src/crypto/mailie'
import { accountServer, DEFAULT_KDF, saltOf, targetOf, type Stored } from './accountServer'
import { failure, freshModules, json, now, reply, serve, settle, stubPage } from './support'

const PASSWORD = 'correct horse battery staple'
const ANA = 'ana@example.test'
const ANA_SEAL = 'b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4'
const BEA = 'bea@example.test'
const BEA_ID = 'usr_00000000000000b2'
const BEA_SEAL = '0f4c2a1e-7d3b-4e8a-9c61-52b7e0d9a3f4'

/** A fresh page: the session, the ceremonies, the vault and the errors of one module graph. */
async function load() {
  await freshModules()
  const [account, session, vault, sessionVault, errors, http] = await Promise.all([
    import('../src/state/account'), import('../src/state/session'), import('../src/state/accountVault'),
    import('../src/state/sessionVault'), import('../src/crypto/errors'), import('../src/api/http'),
  ])
  return { ...account, ...session, vault, sessionVault, CeremonyError: errors.CeremonyError, ApiError: http.ApiError }
}

/** Ana enrolled at her target, and at another salt (an address changed, a salt key replaced): made once, with the real derivation. */
let atTarget: Enrolled
let offTarget: Enrolled
beforeAll(async () => {
  atTarget = await enrol(PASSWORD, { salt: targetOf(ANA), kdf: DEFAULT_KDF, seal_id: ANA_SEAL })
  offTarget = await enrol(PASSWORD, { salt: saltOf('an older salt'), kdf: DEFAULT_KDF, seal_id: ANA_SEAL })
}, 60_000)

function stored(made: Enrolled, salt: string, fields: Partial<Stored> = {}): Stored {
  const e = made.enrolment
  return {
    id: 'usr_00000000000000a1', email: ANA, name: 'Ana', sealID: ANA_SEAL, publicKey: e.public_key, salt, kdf: DEFAULT_KDF,
    authKey: e.auth_key, passwordWrap: e.password_wrap, recoveryWrap: e.recovery_wrap, recoveryProof: e.recovery_proof, ...fields,
  }
}

const same = (a: Uint8Array | null, b: Uint8Array) => a !== null && a.length === b.length && a.every((byte, i) => byte === b[i])
/** Whether any request carried the password, under any field. */
const sentPassword = (server: ReturnType<typeof accountServer>, password = PASSWORD) => server.calls.some(call => JSON.stringify(call.body).includes(password))
/** Ana as a person who signed up before the key scheme and never enrolled: no account key, nothing that signs her in. */
const notEnrolledAna = (): Stored => ({ ...stored(atTarget, ''), publicKey: undefined, authKey: '', passwordWrap: '', recoveryWrap: '', recoveryProof: '' })

/** Opens this browser's vault database at whatever version it has, for a spec to look inside. */
function openVaultDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open('mailie-browser-account')
    request.onerror = () => reject(request.error)
    request.onsuccess = () => resolve(request.result)
  })
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('creating an account by invitation', { timeout: 30_000 }, () => {
  it('binds the wraps to the seal id the invitation answered, never sends the password, and shows the recovery code once', async () => {
    const s = await load()
    const server = accountServer()
    const invite = server.invite('new@example.test')
    await s.signUp({ invite, email: 'new@example.test', name: 'New', password: 'a new password of mine' })
    expect(server.paths()).toEqual(['/v1/auth/signup/open', '/v1/auth/signup'])
    const body = server.calls[1]!.body
    const person = server.people.get('new@example.test')!
    expect(body.seal_id).toBe(person.sealID)
    expect(body.kdf).toEqual(DEFAULT_KDF)
    expect(Object.keys(body).sort()).toEqual(['auth_key', 'email', 'invite', 'kdf', 'name', 'password_wrap', 'public_key', 'recovery_proof', 'recovery_wrap', 'seal_id'])
    expect(sentPassword(server, 'a new password of mine')).toBe(false)
    expect(s.session.phase).toBe('ready')
    expect(s.session.keyed).toBe(true)
    // The code opens the recovery wrap the server keeps, to the key this browser keeps.
    expect(s.recoveryCode.code).toMatch(/^[0-9A-Z]{5}(-[0-9A-Z]{5}){5}$/)
    const recovery = await recoveryKeys(s.recoveryCode.code)
    expect(recovery.proof).toBe(person.recoveryProof)
    const opened = await openWrap('recovery', recovery.key, person.recoveryWrap, person.sealID, person.publicKey!)
    expect(same(await s.vault.accountKeyOf(person.sealID, person.publicKey!), opened)).toBe(true)
    s.recoveryCodeSaved()
    expect(s.recoveryCode.code).toBe('')

    // The next page signs in with the same password and opens the same key.
    const next = await load()
    await next.signOut()
    await next.signIn('new@example.test', 'a new password of mine')
    expect(next.session.user?.seal_id).toBe(person.sealID)
    expect(same(await next.vault.accountKeyOf(person.sealID, person.publicKey!), opened)).toBe(true)
  })

  it('refuses a new password under twelve code points before asking the server anything', async () => {
    const s = await load()
    const server = accountServer()
    await expect(s.signUp({ invite: server.invite('new@example.test'), email: 'new@example.test', name: 'New', password: 'elevenchars' }))
      .rejects.toMatchObject({ code: 'password_too_short' })
    expect(server.calls).toHaveLength(0)
  })
})

describe('signing in', { timeout: 30_000 }, () => {
  it('opens the password wrap and keeps the account key for this person only', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(' Ana@Example.test', PASSWORD)
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/login'])
    expect(sentPassword(server)).toBe(false)
    expect(s.session.phase).toBe('ready')
    expect(s.session.keyed).toBe(true)
    expect(s.session.authenticatedAt).toBeGreaterThan(0)
    expect(s.freshStepUp()).toBe(true)
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key), atTarget.accountKey)).toBe(true)
    // Asked for anyone else, the record is wiped, never opened.
    expect(await s.vault.accountKeyOf(crypto.randomUUID(), atTarget.enrolment.public_key)).toBeNull()
    expect(await s.vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key)).toBeNull()
  })

  it('treats a wrap that does not open after the auth key was accepted as a security error, and keeps nothing', async () => {
    const s = await load()
    const server = accountServer()
    // The right auth key, and somebody else's wrap.
    server.people.set(ANA, stored(atTarget, targetOf(ANA), { passwordWrap: offTarget.enrolment.password_wrap }))
    await expect(s.signIn(ANA, PASSWORD)).rejects.toMatchObject({ code: 'security' })
    expect(s.session.phase).not.toBe('ready')
    expect(await s.vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key)).toBeNull()
    // The session the server opened is ended.
    await vi.waitFor(() => expect(server.paths()).toContain('/v1/auth/logout'))
  })

  it('refuses parameters or a salt outside the platform’s bounds before deriving anything', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA), { kdf: { ...DEFAULT_KDF, m: 32_768 } }))
    await expect(s.signIn(ANA, PASSWORD)).rejects.toMatchObject({ code: 'security' })
    expect(server.paths()).toEqual(['/v1/auth/challenge'])
  })

  it('derives the same password again under the target a sign-in names, and ends no session', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(offTarget, saltOf('an older salt')))
    await s.signIn(ANA, PASSWORD)
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/login', '/v1/auth/password/finish'])
    const finish = server.calls[2]!.body
    expect(finish.kdf).toEqual(DEFAULT_KDF)
    expect(finish.auth_key).not.toBe(offTarget.enrolment.auth_key)
    // The ticket finishes only with the auth key that signed in, which the answer carrying it never held.
    expect(finish.current_auth_key).toBe(offTarget.enrolment.auth_key)
    expect(server.people.get(ANA)!.salt).toBe(targetOf(ANA))
    expect(s.session.phase).toBe('ready')
    expect([...server.sessions.values()].every(session => !session.ended)).toBe(true)
    // At the target now: the next sign-in names none, and opens the same key.
    const next = await load()
    await next.signIn(ANA, PASSWORD)
    expect(server.paths().slice(3)).toEqual(['/v1/auth/challenge', '/v1/auth/login'])
    expect(same(await next.vault.accountKeyOf(ANA_SEAL, offTarget.enrolment.public_key), offTarget.accountKey)).toBe(true)
  })
})

describe('an account made before the key scheme that never enrolled', { timeout: 30_000 }, () => {
  it('is refused as a wrong password is, and its password is never sent', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, notEnrolledAna())
    await expect(s.signIn(ANA, 'my old password')).rejects.toMatchObject({ code: 'unauthorized' })
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/login'])
    expect(Object.keys(server.calls[1]!.body).sort()).toEqual(['auth_key', 'email'])
    expect(sentPassword(server, 'my old password')).toBe(false)
    expect(s.session.phase).not.toBe('ready')
  })

  it('sends the auth key alone even to a server that still answers upgrade, as the release before did', async () => {
    for (const ana of ['never enrolled', 'enrolled'] as const) {
      stubPage()
      const s = await load()
      const server = accountServer()
      server.people.set(ANA, ana === 'enrolled' ? stored(atTarget, targetOf(ANA)) : notEnrolledAna())
      server.refuseNext.set('/v1/auth/challenge', () => json({ salt: targetOf(ANA), kdf: DEFAULT_KDF, upgrade: true }))
      const signedIn = await s.signIn(ANA, PASSWORD).then(() => true, (error: unknown) => error)
      if (ana === 'enrolled') expect(signedIn, ana).toBe(true)
      else expect(signedIn, ana).toMatchObject({ code: 'unauthorized' })
      expect(server.paths(), ana).toEqual(['/v1/auth/challenge', '/v1/auth/login'])
      expect(sentPassword(server), ana).toBe(false)
    }
  })

  it('enrols with a reset link from the administrator, under the seal id it always had', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, notEnrolledAna())
    await s.resetPassword({ reset: server.reset(ANA), email: ANA, password: 'a password after the reset' })
    expect(server.paths()).toEqual(['/v1/auth/reset/open', '/v1/auth/reset'])
    expect(sentPassword(server, 'a password after the reset')).toBe(false)
    const person = server.people.get(ANA)!
    expect(person.publicKey).toBe(server.calls[1]!.body.public_key)
    expect(s.session.user?.seal_id).toBe(ANA_SEAL)
    expect(s.session.keyed).toBe(true)
    expect(s.recoveryCode.reason).toBe('new-account')

    // From then on the new password signs in, from any page.
    const next = await load()
    await next.signOut()
    await next.signIn(ANA, 'a password after the reset')
    expect(next.session.phase).toBe('ready')
  })
})

describe('recovering an account', { timeout: 30_000 }, () => {
  it('keeps the account key, signs in with the new password, and shows a new recovery code', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.recover({ email: ANA, code: atTarget.recoveryCode.toLowerCase(), password: 'my brand new password' })
    expect(server.paths()).toEqual(['/v1/auth/recover/open', '/v1/auth/recover/finish', '/v1/auth/login'])
    expect(sentPassword(server, 'my brand new password') || sentPassword(server, atTarget.recoveryCode)).toBe(false)
    // The ticket finishes only with the proof that opened the recovery, which the answer carrying it never held.
    const finish = server.calls[1]!.body
    expect(Object.keys(finish).sort()).toEqual(['auth_key', 'current_recovery_proof', 'kdf', 'password_wrap', 'recovery_proof', 'recovery_wrap', 'ticket'])
    expect(finish.current_recovery_proof).toBe(atTarget.enrolment.recovery_proof)
    expect(finish.recovery_proof).not.toBe(atTarget.enrolment.recovery_proof)
    const person = server.people.get(ANA)!
    expect(person.publicKey).toBe(atTarget.enrolment.public_key)
    expect(s.session.phase).toBe('ready')
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, person.publicKey!), atTarget.accountKey)).toBe(true)
    expect(s.recoveryCode.reason).toBe('recovered')
    expect(s.recoveryCode.code).not.toBe(atTarget.recoveryCode)
    expect((await recoveryKeys(s.recoveryCode.code)).proof).toBe(person.recoveryProof)
  })

  it('says the recovery is done when the sign-in after it fails', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    // The account's sign-in budget spent by the time the new password signs in.
    server.refuseNext.set('/v1/auth/login', () => failure('rate_limited', 429))
    await expect(s.recover({ email: ANA, code: atTarget.recoveryCode, password: 'my brand new password' })).resolves.toBe('sign-in-again')
    expect(server.paths()).toEqual(['/v1/auth/recover/open', '/v1/auth/recover/finish', '/v1/auth/login'])
    expect(s.session.phase).not.toBe('ready')
    expect(s.recoveryCode.reason).toBe('recovered')
    expect((await recoveryKeys(s.recoveryCode.code)).proof).toBe(server.people.get(ANA)!.recoveryProof)

    // The new password signs in, as the page says.
    await s.signIn(ANA, 'my brand new password')
    expect(s.session.phase).toBe('ready')
    expect(sentPassword(server, 'my brand new password')).toBe(false)
  })

  it('refuses something that is not a recovery code before asking the server', async () => {
    const s = await load()
    const server = accountServer()
    await expect(s.recover({ email: ANA, code: 'UUUUU-UUUUU-UUUUU-UUUUU-UUUUU-UUUUU', password: 'my brand new password' }))
      .rejects.toMatchObject({ code: 'recovery_code' })
    expect(server.calls).toHaveLength(0)
  })
})

describe('a reset link', { timeout: 30_000 }, () => {
  it('derives the new password under the target the link answers, never the challenge’s salt', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(offTarget, saltOf('an older salt')))
    const code = server.reset(ANA)
    await s.resetPassword({ reset: code, email: ANA, password: 'a password after the reset' })
    expect(server.paths()).toEqual(['/v1/auth/reset/open', '/v1/auth/reset'])
    const body = server.calls[1]!.body
    const derived = await deriveKeys('a password after the reset', { salt: targetOf(ANA), kdf: DEFAULT_KDF }, 'new')
    expect(body.auth_key).toBe(derived.authKey)
    expect(body.public_key).not.toBe(offTarget.enrolment.public_key)
    expect(s.session.keyed).toBe(true)
    expect(s.recoveryCode.reason).toBe('new-account')
  })
})

describe('a signed-in person’s password and recovery code', { timeout: 40_000 }, () => {
  it('does not keep the new account key when the session ended while the server finished the change', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    // The server answers the finish, and before the page takes the new session, this one ends (another tab signed out).
    const fetchSpy = vi.mocked(globalThis.fetch)
    const inner = fetchSpy.getMockImplementation()!
    fetchSpy.mockImplementation(async (input, init) => {
      const response = await inner(input, init)
      if (new URL(String(input)).pathname === '/v1/auth/password/finish') await s.signOut()
      return response
    })
    await s.changePassword(PASSWORD, 'the next password of mine')
    expect(s.session.phase).toBe('signed-out')
    expect(await s.vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key)).toBeNull()
    expect(await (await load()).vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key)).toBeNull()
  })

  it('changes the password in two steps over the same account key, and goes on with the session the server answers', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    const before = s.session.authenticatedAt
    await s.changePassword(PASSWORD, 'the next password of mine')
    expect(server.paths().slice(2)).toEqual(['/v1/auth/challenge', '/v1/auth/password/begin', '/v1/auth/password/finish'])
    expect(sentPassword(server) || sentPassword(server, 'the next password of mine')).toBe(false)
    // The ticket finishes only with the current auth key that began the change.
    expect(server.calls.at(-1)!.body.current_auth_key).toBe(atTarget.enrolment.auth_key)
    const person = server.people.get(ANA)!
    expect(person.publicKey).toBe(atTarget.enrolment.public_key)
    expect(person.recoveryProof).toBe(atTarget.enrolment.recovery_proof)
    expect(s.session.phase).toBe('ready')
    expect(s.session.authenticatedAt).toBe(before)
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, person.publicKey!), atTarget.accountKey)).toBe(true)
  })

  it('refuses a wrong current password without changing anything', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    await expect(s.changePassword('not my password at all', 'the next password of mine')).rejects.toMatchObject({ code: 'not_authorized' })
    expect(server.paths().at(-1)).toBe('/v1/auth/password/begin')
    expect(s.session.phase).toBe('ready')
  })

  it('replaces the recovery code over the key this browser keeps, with the current password proved in the same request', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    // However recent the sign-in, the session alone is not enough: the password is asked for every time.
    await s.signIn(ANA, PASSWORD)
    const before = { ...server.people.get(ANA)! }
    await expect(s.replaceRecoveryCode('not my password at all')).rejects.toMatchObject({ code: 'not_authorized' })
    expect(server.people.get(ANA)!.recoveryProof).toBe(before.recoveryProof)
    expect(s.recoveryCode.code).toBe('')
    await s.replaceRecoveryCode(PASSWORD)
    expect(server.paths().slice(2)).toEqual(['/v1/auth/challenge', '/v1/auth/recovery', '/v1/auth/challenge', '/v1/auth/recovery'])
    const body = server.calls.at(-1)!.body
    expect(Object.keys(body).sort()).toEqual(['current_auth_key', 'recovery_proof', 'recovery_wrap'])
    expect(body.current_auth_key).toBe(atTarget.enrolment.auth_key)
    expect(sentPassword(server)).toBe(false)
    const person = server.people.get(ANA)!
    expect(s.recoveryCode.reason).toBe('replaced')
    expect(sentPassword(server, s.recoveryCode.code)).toBe(false)
    const keys = await recoveryKeys(s.recoveryCode.code)
    expect(keys.proof).toBe(person.recoveryProof)
    expect(same(await openWrap('recovery', keys.key, person.recoveryWrap, ANA_SEAL, person.publicKey!), atTarget.accountKey)).toBe(true)
    // The password and the account key did not change.
    expect(person.authKey).toBe(before.authKey)
    expect(person.publicKey).toBe(before.publicKey)
  })

  it('shows the new code to its person even when their session ended while the server replaced it', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    // Signed out here, or in another tab, before the answer arrives: the server replaced the code all the same, and this is its only copy.
    server.inFlight.set('/v1/auth/recovery', () => s.signOut())
    await s.replaceRecoveryCode(PASSWORD)
    expect(s.session.phase).toBe('signed-out')
    expect(s.recoveryCode.reason).toBe('replaced')
    expect((await recoveryKeys(s.recoveryCode.code)).proof).toBe(server.people.get(ANA)!.recoveryProof)
  })

  it('never shows it over another person signed in meanwhile, on this page or in the login this browser remembers', async () => {
    const bea = await enrol(PASSWORD, { salt: targetOf(BEA), kdf: DEFAULT_KDF, seal_id: BEA_SEAL })
    for (const meanwhile of ['signs in here', 'signs in in another tab'] as const) {
      const s = await load()
      const server = accountServer()
      server.people.set(ANA, stored(atTarget, targetOf(ANA)))
      server.people.set(BEA, stored(bea, targetOf(BEA), { id: BEA_ID, email: BEA, name: 'Bea', sealID: BEA_SEAL }))
      await s.signIn(ANA, PASSWORD)
      server.inFlight.set('/v1/auth/recovery', async () => {
        await s.signOut()
        if (meanwhile === 'signs in here') await s.signIn(BEA, PASSWORD)
        else await s.sessionVault.saveLocalSession({ id: crypto.randomUUID(), token: 'bea-in-another-tab', expiresAt: now() + 3600, userID: BEA_ID })
      })
      await s.replaceRecoveryCode(PASSWORD)
      expect(s.recoveryCode, meanwhile).toEqual({ code: '', reason: '' })
      expect(s.identity(), meanwhile).toBe(meanwhile === 'signs in here' ? BEA_ID : '')
      // Ana's code was replaced on the server: she replaces it again to see one.
      expect(server.people.get(ANA)!.recoveryProof, meanwhile).not.toBe(atTarget.enrolment.recovery_proof)
    }
  })

  it('says so when this browser does not hold the account key to wrap again, before deriving or sending anything', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    await s.vault.wipeAccountKey()
    const asked = server.calls.length
    await expect(s.replaceRecoveryCode(PASSWORD)).rejects.toMatchObject({ code: 'no_account_key' })
    expect(s.session.keyed).toBe(false)
    expect(server.calls).toHaveLength(asked)
  })
})

describe('the step-up', { timeout: 30_000 }, () => {
  it('proves the session’s own person with the password, and refreshes the step-up time', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    server.clock.authenticatedAt = () => now() - 11 * 60
    await s.signIn(ANA, PASSWORD)
    expect(s.freshStepUp()).toBe(false)
    await expect(s.stepUp('not my password at all')).rejects.toMatchObject({ code: 'not_authorized' })
    expect(s.freshStepUp()).toBe(false)
    await s.stepUp(PASSWORD)
    expect(s.freshStepUp()).toBe(true)
    expect(sentPassword(server)).toBe(false)
  })

  it('judges the step-up by the server’s clock, never this browser’s', async () => {
    // The browser's clock fifteen minutes ahead of the server's: a step-up made now is fresh.
    const ahead = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    server.clock.skew = 15 * 60
    await ahead.signIn(ANA, PASSWORD)
    expect(ahead.freshStepUp()).toBe(true)
    await ahead.stepUp(PASSWORD)
    expect(ahead.freshStepUp()).toBe(true)

    // Fifteen minutes behind it: one eleven minutes old by the server's clock is not.
    const behind = await load()
    server.clock.skew = -15 * 60
    server.clock.authenticatedAt = () => now() + 15 * 60 - 11 * 60
    await behind.signOut()
    await behind.signIn(ANA, PASSWORD)
    expect(behind.freshStepUp()).toBe(false)
  })
})

describe('the browser vault', () => {
  async function kept(s: Awaited<ReturnType<typeof load>>) {
    const pair = await generateAccountKeys()
    await s.vault.keepAccountKey(pair.privateKey, pair.publicKey, ANA_SEAL)
    return { ...pair, publicText: toBase64URL(pair.publicKey) }
  }

  it('keeps the account key only as ciphertext under a key the page cannot export', async () => {
    const s = await load()
    const pair = await kept(s)
    const db = await openVaultDatabase()
    const record = await new Promise<Record<string, unknown>>(resolve => {
      const get = db.transaction('vault').objectStore('vault').get('current')
      get.onsuccess = () => { db.close(); resolve(get.result as Record<string, unknown>) }
    })
    const envelope = record.envelope as { key: CryptoKey; ciphertext: ArrayBuffer }
    expect(envelope.key.extractable).toBe(false)
    const text = new Uint8Array(envelope.ciphertext)
    expect(same(text.subarray(0, 32), pair.privateKey)).toBe(false)
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText), pair.privateKey)).toBe(true)
  })

  it('drops the addresses an older console remembered, with their store, and keeps the account key it kept', async () => {
    // The database as the release that brought the key scheme left it: version 1, the vault and the enrolled addresses.
    const pair = await generateAccountKeys()
    const record = {
      version: 1, origin: location.origin, sealID: ANA_SEAL, publicKey: toBase64URL(pair.publicKey),
      envelope: await sealBrowserVault(pair.privateKey, pair.publicKey, ANA_SEAL),
    }
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.open('mailie-browser-account', 1)
      request.onerror = () => reject(request.error)
      request.onupgradeneeded = () => {
        request.result.createObjectStore('vault')
        request.result.createObjectStore('enrolled')
      }
      request.onsuccess = () => {
        const tx = request.result.transaction(['vault', 'enrolled'], 'readwrite')
        tx.objectStore('vault').put(record, 'current')
        tx.objectStore('enrolled').put(Date.now(), `${location.origin}|${ANA}`)
        tx.oncomplete = () => { request.result.close(); resolve() }
        tx.onerror = () => reject(tx.error)
      }
    })
    const s = await load()
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, record.publicKey), pair.privateKey)).toBe(true)
    const db = await openVaultDatabase()
    expect(db.version).toBe(2)
    expect([...db.objectStoreNames]).toEqual(['vault'])
    db.close()
  })

  it('wipes the account key at sign-out', async () => {
    const s = await load()
    const pair = await kept(s)
    await s.signOut()
    expect(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText)).toBeNull()
  })

  it('wipes the account key when the page finds no valid session', async () => {
    const s = await load()
    const pair = await kept(s)
    accountServer()
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText)).toBeNull()
  })

  it('keeps the account key across a reload of the same person’s session', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    const next = await load()
    await next.restore()
    expect(next.session.phase).toBe('ready')
    expect(next.session.keyed).toBe(true)
    expect(next.session.authenticatedAt).toBeGreaterThan(0)
  }, 30_000)

  it('keeps the key this page holds when the browser opens its storage but refuses the write', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    // An older record of someone else in the slot, then every write refused: the storage is full.
    await kept(s)
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(() => { throw new DOMException('the storage is full', 'QuotaExceededError') })
    await s.signIn(ANA, PASSWORD)
    expect(s.session.keyed).toBe(true)
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, atTarget.enrolment.public_key), atTarget.accountKey)).toBe(true)
    // What needs the key works from this page.
    await s.replaceRecoveryCode(PASSWORD)
    expect(s.recoveryCode.reason).toBe('replaced')
  }, 30_000)

  /** A page of an edition that keeps the account key past a session that merely expires: the hosted service's rule. */
  async function loadKeepingPastExpiry() {
    const s = await load()
    const { configureEdition, edition } = await import('../src/edition')
    configureEdition({ ...edition(), accountKey: { outlivesExpiry: true } })
    return s
  }

  /** Whether the next page load finds the account key in this browser's storage, not in a page's own copy. */
  async function storedAfterReload(pair: { privateKey: Uint8Array; publicText: string }): Promise<boolean> {
    const next = await load()
    return same(await next.vault.accountKeyOf(ANA_SEAL, pair.publicText), pair.privateKey)
  }

  /** The daemon refusing every token, its clock serverAheadS seconds ahead of the real one (its Date). */
  function refusingServer(serverAheadS: number, real = Date.now()) {
    return serve(() => failure('unauthorized', 401, { Date: new Date(real + serverAheadS * 1000).toUTCString() }))
  }

  /** A session this browser remembers, for another minute by its own clock, holding the key kept beside it, as a sign-in leaves them. */
  async function remembered(s: Awaited<ReturnType<typeof load>>, expiresAt = now() + 60) {
    const id = crypto.randomUUID()
    await s.vault.sessionBegan(id)
    await s.sessionVault.saveLocalSession({ id, token: 'tok_remembered_00000000000000000000000000000', expiresAt, userID: 'usr_00000000000000a1' })
  }

  it('keeps the account key past a session the server refused once its clock had passed the expiry, where the edition keeps it so', async () => {
    const s = await loadKeepingPastExpiry()
    const pair = await kept(s)
    await remembered(s)
    refusingServer(120)
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(await storedAfterReload(pair)).toBe(true)
  })

  it('wipes the account key of a session the server refused before its expiry, even where the edition keeps it past one', async () => {
    const s = await loadKeepingPastExpiry()
    const pair = await kept(s)
    await remembered(s)
    refusingServer(0)
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(await storedAfterReload(pair)).toBe(false)
  })

  it('wipes the account key of a session that expired on a self-hosted server', async () => {
    const s = await load()
    const pair = await kept(s)
    await remembered(s)
    refusingServer(120)
    await s.restore()
    expect(await storedAfterReload(pair)).toBe(false)
  })

  it('asks the server about a session this browser’s clock calls expired, and keeps the key only if the server’s clock agrees', async () => {
    const real = Date.now()
    for (const serverAgrees of [true, false]) {
      const s = await loadKeepingPastExpiry()
      const pair = await kept(s)
      await remembered(s)
      // This browser's clock is two minutes ahead: the record is past its expiry here.
      vi.spyOn(Date, 'now').mockReturnValue(real + 120_000)
      const server = refusingServer(serverAgrees ? 120 : 0, real)
      await s.restore()
      vi.mocked(Date.now).mockRestore()
      expect(server.mock.calls.map(([url]) => new URL(String(url)).pathname)).toEqual(['/v1/auth/me'])
      expect(s.session.phase).toBe('signed-out')
      expect(await storedAfterReload(pair)).toBe(serverAgrees)
      vi.restoreAllMocks()
    }
  })

  it('restores a session this browser’s clock calls expired when the server still takes it, where the edition keeps the key past an expiry', async () => {
    const s = await loadKeepingPastExpiry()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    const real = Date.now()
    vi.spyOn(Date, 'now').mockReturnValue(real + 15 * 86_400_000)
    const next = await loadKeepingPastExpiry()
    await next.restore()
    expect(next.session.phase).toBe('ready')
    expect(next.session.keyed).toBe(true)
  }, 30_000)

  it('keeps the key with no session record only after an expiry a page established, and wipes it after anything else', async () => {
    // A page established the expiry: the next one, which finds no session record, keeps the key.
    let s = await loadKeepingPastExpiry()
    let pair = await kept(s)
    await remembered(s)
    refusingServer(120)
    await s.restore()
    vi.restoreAllMocks()
    let next = await loadKeepingPastExpiry()
    await next.restore()
    expect(next.session.phase).toBe('signed-out')
    expect(await storedAfterReload(pair)).toBe(true)

    // A key kept beside a session whose record is gone, as when the browser refused to store it: wiped.
    stubPage()
    s = await loadKeepingPastExpiry()
    pair = await kept(s)
    next = await loadKeepingPastExpiry()
    await next.restore()
    expect(await storedAfterReload(pair)).toBe(false)
  })

  it('clears the mark of an expiry outlived as soon as a session begins with the key, by a sign-in or a restore', async () => {
    const markedKey = async () => {
      const s = await loadKeepingPastExpiry()
      const pair = await kept(s)
      await s.vault.sessionBegan('a-login-that-expired')
      await s.vault.settleRecord('a-login-that-expired', true)
      expect(await (await load()).vault.outlivedExpiry()).toBe(true)
      return { s, pair }
    }
    // A sign-in.
    let { s } = await markedKey()
    serve(() => new Response(null, { status: 204 }))
    await s.adoptSession(reply())
    expect(await s.vault.outlivedExpiry()).toBe(false)
    expect(await (await load()).vault.outlivedExpiry()).toBe(false)
    vi.restoreAllMocks()

    // A session this browser remembers, which the server still takes.
    stubPage()
    const marked = await markedKey()
    s = marked.s
    // The session's record only: the restore binds the marked key to it, and clears the mark.
    await s.sessionVault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_remembered_00000000000000000000000000000', expiresAt: now() + 3600, userID: 'usr_00000000000000a1' })
    const person = { ...reply().user, seal_id: ANA_SEAL, public_key: marked.pair.publicText }
    serve(() => json({ user: person, session: { id: 'ses_remembered', created_at: now() - 60, expires_at: now() + 3600, authenticated_at: now() } }))
    const next = await loadKeepingPastExpiry()
    await next.restore()
    expect(next.session.phase).toBe('ready')
    expect(next.session.keyed).toBe(true)
    expect(await (await load()).vault.outlivedExpiry()).toBe(false)
  })

  it('leaves alone the key a session begun since holds, in this page or another tab, when an ended session is settled late', async () => {
    // The ended session's re-ask is answered before or past its expiry; the session since begins in page B
    // with a key of its own, or in page A itself, holding the record already there (an edition's sign-in, a restore).
    for (const [where, serverPastExpiry] of [['other', false], ['other', true], ['same', false], ['same', true]] as const) {
      stubPage()
      const real = Date.now()
      // Page A's session is refused by the event stream: no Date, so A asks the server again, and that answer is slow.
      let answer: (response: Response) => void = () => {}
      const slow = new Promise<Response>(resolve => { answer = resolve })
      serve(({ token }) => token === 'tok_first_0000000000000000000000000000000000' ? slow : new Response(null, { status: 204 }))
      const a = await loadKeepingPastExpiry()
      const first = await kept(a)
      await a.adoptSession(reply())
      const ended = (await a.sessionVault.loadLocalSession())!.id
      await expect(a.authorized(() => Promise.reject(new a.ApiError('unauthorized')))).rejects.toMatchObject({ code: 'unauthorized' })

      let live: { privateKey: Uint8Array; publicText: string }
      if (where === 'other') {
        const b = await loadKeepingPastExpiry()
        live = await kept(b)
        await b.adoptSession(reply('tok_second_000000000000000000000000000000000'))
      } else {
        live = first
        await a.adoptSession(reply('tok_second_000000000000000000000000000000000'))
      }

      answer(failure('unauthorized', 401, { Date: new Date(real + (serverPastExpiry ? 15 * 86_400_000 : 0)).toUTCString() }))
      // A clears its ended session's record only after settling the key.
      await vi.waitFor(async () => expect(await a.sessionVault.localSessionWasCleared(ended)).toBe(true))
      expect(await storedAfterReload(live)).toBe(true)
      expect(await (await load()).vault.outlivedExpiry()).toBe(false)
      vi.restoreAllMocks()
    }
  })

  it('settles a key kept before records named their session with the first session that ends', async () => {
    const s = await loadKeepingPastExpiry()
    const pair = await kept(s)
    // The record as a console from before holders wrote it.
    const db = await openVaultDatabase()
    await new Promise<void>(resolve => {
      const tx = db.transaction('vault', 'readwrite')
      const store = tx.objectStore('vault')
      const read = store.get('current')
      read.onsuccess = () => { const { holder: _, ...older } = read.result as Record<string, unknown>; store.put(older, 'current') }
      tx.oncomplete = () => { db.close(); resolve() }
    })
    await s.sessionVault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_remembered_00000000000000000000000000000', expiresAt: now() + 60, userID: 'usr_00000000000000a1' })
    refusingServer(0)
    const next = await loadKeepingPastExpiry()
    await next.restore()
    expect(await storedAfterReload(pair)).toBe(false)
  })

  it('gives a slow restore up for the session another tab began meanwhile, and leaves that session its key', async () => {
    // Page B restores L1 and its answer is slow; meanwhile page A signs in again (L2) with a key of its own.
    const before = await loadKeepingPastExpiry()
    await kept(before)
    await remembered(before, now() + 3600)
    let answer: (response: Response) => void = () => {}
    const slow = new Promise<Response>(resolve => { answer = resolve })
    let second = { privateKey: new Uint8Array(), publicText: '' }
    const me = () => json({ user: { ...reply().user, seal_id: ANA_SEAL, public_key: second.publicText }, session: { id: 'ses_x', created_at: now() - 60, expires_at: now() + 3600, authenticated_at: now() } })
    serve(({ token }) => token === 'tok_remembered_00000000000000000000000000000' ? slow : me())
    const b = await loadKeepingPastExpiry()
    const restoring = b.restore()
    await settle(10)
    const a = await loadKeepingPastExpiry()
    second = await kept(a)
    await a.adoptSession(reply('tok_second_000000000000000000000000000000000'))
    answer(me())
    await restoring
    // B holds L2, the session the browser remembers now, never L1.
    expect(b.session.phase).toBe('ready')
    expect(await b.authorized(async token => token)).toBe('tok_second_000000000000000000000000000000000')
    expect(b.session.keyed).toBe(true)
    // L2 is refused before its expiry: its key goes, from storage too.
    vi.restoreAllMocks()
    refusingServer(0)
    const { me: askMe } = await import('../src/api/auth')
    await expect(a.authorized(t => askMe(t))).rejects.toMatchObject({ code: 'unauthorized' })
    await vi.waitFor(async () => expect(await storedAfterReload(second)).toBe(false))
  })

  it('never lets a restored session take a key a sign-in is about to begin its session with', async () => {
    const s = await loadKeepingPastExpiry()
    // A ceremony kept a key (pending) and has not begun its session; the browser still remembers L1.
    const pending = await kept(s)
    await s.sessionVault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_remembered_00000000000000000000000000000', expiresAt: now() + 3600, userID: 'usr_00000000000000a1' })
    const person = { ...reply().user, seal_id: ANA_SEAL, public_key: pending.publicText }
    serve(({ path }) => path === '/v1/auth/me' && restoredOnce++ === 0
      ? json({ user: person, session: { id: 'ses_old', created_at: now() - 60, expires_at: now() + 3600, authenticated_at: now() } })
      : failure('unauthorized', 401, { Date: new Date().toUTCString() }))
    let restoredOnce = 0
    const next = await loadKeepingPastExpiry()
    await next.restore()
    expect(next.session.phase).toBe('ready')
    // L1 is refused before its expiry: the pending key is not its to take with it.
    const { me } = await import('../src/api/auth')
    await expect(next.authorized(t => me(t))).rejects.toMatchObject({ code: 'unauthorized' })
    await settle(20)
    expect(await storedAfterReload(pending)).toBe(true)
  })

  it('judges the event stream’s refusal as it opens by that answer’s Date, asking nothing more', async () => {
    for (const serverPastExpiry of [false, true]) {
      stubPage()
      const s = await loadKeepingPastExpiry()
      const pair = await kept(s)
      await s.adoptSession(reply())
      const server = refusingServer(serverPastExpiry ? 15 * 86_400 : 0)
      const { readEventStream } = await import('../src/api/events')
      await expect(s.authorized(token => readEventStream({ token, signal: new AbortController().signal, onMessage: () => {} })))
        .rejects.toMatchObject({ code: 'unauthorized' })
      await vi.waitFor(async () => expect(await storedAfterReload(pair)).toBe(serverPastExpiry))
      expect(server.mock.calls.map(([url]) => new URL(String(url)).pathname)).toEqual(['/v1/events'])
      vi.restoreAllMocks()
    }
  })

  it('settles the key of a session with this page’s own copy only, when the browser refuses to store it', async () => {
    const s = await loadKeepingPastExpiry()
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(() => { throw new DOMException('the storage is full', 'QuotaExceededError') })
    const pair = await kept(s)
    await s.adoptSession(reply())
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText), pair.privateKey)).toBe(true)
    const { me } = await import('../src/api/auth')
    refusingServer(0)
    await expect(s.authorized(t => me(t))).rejects.toMatchObject({ code: 'unauthorized' })
    await vi.waitFor(async () => expect(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText)).toBeNull())
  })

  /** Records every write to browser storage, as store:operation:key, in order. */
  function recordWrites(): string[] {
    const writes: string[] = []
    const put = IDBObjectStore.prototype.put
    const del = IDBObjectStore.prototype.delete
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, ...args: Parameters<IDBObjectStore['put']>) {
      writes.push(`${this.name}:put:${String(args[1])}`)
      return put.apply(this, args)
    })
    vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (this: IDBObjectStore, ...args: Parameters<IDBObjectStore['delete']>) {
      writes.push(`${this.name}:delete:${String(args[0])}`)
      return del.apply(this, args)
    })
    return writes
  }

  /** Whether the vault was settled (marked when kept, wiped otherwise) before the session's record was cleared. */
  function settledFirst(writes: string[], keptPastExpiry: boolean): boolean {
    const vault = writes.findIndex(w => w === (keptPastExpiry ? 'vault:put:current' : 'vault:delete:current'))
    const cleared = writes.findIndex(w => w.startsWith('session:put:revoked:'))
    return vault >= 0 && cleared >= 0 && vault < cleared
  }

  it('settles the key before it clears the session’s record, so a page stopped in between leaves the record to be checked again', async () => {
    for (const serverPastExpiry of [false, true]) {
      // In the middle of use, refused through a request, whose answer's Date judges it: nothing is asked again.
      stubPage()
      const s = await loadKeepingPastExpiry()
      await kept(s)
      await s.adoptSession(reply())
      let writes = recordWrites()
      const server = refusingServer(serverPastExpiry ? 15 * 86_400 : 0)
      const { me } = await import('../src/api/auth')
      await expect(s.authorized(t => me(t))).rejects.toMatchObject({ code: 'unauthorized' })
      await vi.waitFor(() => expect(writes.some(w => w.startsWith('session:put:revoked:'))).toBe(true))
      expect(server).toHaveBeenCalledTimes(1)
      expect(settledFirst(writes, serverPastExpiry)).toBe(true)
      vi.restoreAllMocks()

      // A remembered session the server refuses when the page loads.
      stubPage()
      const before = await loadKeepingPastExpiry()
      await kept(before)
      await remembered(before)
      refusingServer(serverPastExpiry ? 120 : 0)
      const next = await loadKeepingPastExpiry()
      writes = recordWrites()
      await next.restore()
      expect(settledFirst(writes, serverPastExpiry)).toBe(true)
      vi.restoreAllMocks()
    }
  })

  it('wipes the account key at once on a self-hosted server, at a refusal in the middle of use and at the expiry, asking nothing more', async () => {
    const s = await load()
    const pair = await kept(s)
    await s.adoptSession(reply())
    const server = refusingServer(15 * 86_400)
    await expect(s.authorized(() => Promise.reject(new s.ApiError('unauthorized')))).rejects.toMatchObject({ code: 'unauthorized' })
    await vi.waitFor(async () => expect(await storedAfterReload(pair)).toBe(false))
    expect(server).not.toHaveBeenCalled()
    vi.restoreAllMocks()

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    try {
      stubPage()
      const t = await load()
      const other = await kept(t)
      serve(() => new Response(null, { status: 204 }))
      await t.adoptSession({ ...reply(), expires_at: now() + 60 })
      await vi.advanceTimersByTimeAsync(61_000)
      expect(t.session.phase).toBe('signed-out')
      await vi.waitFor(async () => expect(await storedAfterReload(other)).toBe(false))
    } finally {
      vi.useRealTimers()
    }
  })

  it('wipes the account key of a remembered session given up on unchecked, even where the edition keeps it past an expiry', async () => {
    for (const keeps of [true, false]) {
      const s = keeps ? await loadKeepingPastExpiry() : await load()
      const pair = await kept(s)
      await remembered(s)
      serve(() => { throw new TypeError('offline') })
      await s.restore()
      expect(s.session.restoreFailed).toBe(true)
      await s.forgetRemembered()
      expect(s.session.phase).toBe('signed-out')
      expect(await storedAfterReload(pair)).toBe(false)
      vi.restoreAllMocks()
    }
  })

  it('judges a refusal in the middle of use by the server’s clock, asked again: before the expiry it wipes the key, after it keeps it', async () => {
    // The refusal carries no Date, as the event stream's error event does; the page asks the server once more.
    const refused = () => Promise.reject(new s.ApiError('unauthorized'))
    const real = Date.now()
    let s = await loadKeepingPastExpiry()
    let pair = await kept(s)
    await s.adoptSession(reply())
    let server = refusingServer(0, real)
    await expect(s.authorized(refused)).rejects.toMatchObject({ code: 'unauthorized' })
    await vi.waitFor(async () => expect(await storedAfterReload(pair)).toBe(false))
    expect(s.session.notice).toBe('expired')
    expect(server.mock.calls.map(([url]) => new URL(String(url)).pathname)).toContain('/v1/auth/me')
    vi.restoreAllMocks()

    // This browser's clock is fifteen days ahead of the server's, so the session looks expired here:
    // the server's answer, before its expiry, decides.
    stubPage()
    s = await loadKeepingPastExpiry()
    pair = await kept(s)
    await s.adoptSession(reply())
    vi.spyOn(Date, 'now').mockReturnValue(real + 15 * 86_400_000)
    server = refusingServer(0, real)
    await expect(s.authorized(refused)).rejects.toMatchObject({ code: 'unauthorized' })
    await settle(20)
    vi.mocked(Date.now).mockRestore()
    await vi.waitFor(async () => expect(await storedAfterReload(pair)).toBe(false))
    vi.restoreAllMocks()

    // The server's clock is past the expiry: the key stays.
    stubPage()
    s = await loadKeepingPastExpiry()
    pair = await kept(s)
    await s.adoptSession(reply())
    server = refusingServer(15 * 86_400, real)
    await expect(s.authorized(refused)).rejects.toMatchObject({ code: 'unauthorized' })
    await vi.waitFor(async () => expect(await (await load()).vault.outlivedExpiry()).toBe(true))
    expect(await storedAfterReload(pair)).toBe(true)
  })

  it('ends the session at its expiry by the server’s clock, and keeps the key past it, where the edition keeps it so; sign-out wipes it', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    try {
      const s = await loadKeepingPastExpiry()
      const pair = await kept(s)
      const { noteServerDate, serverNow } = await import('../src/state/connection')
      // The server's clock is an hour ahead of this browser's.
      noteServerDate(new Date(Date.now() + 3_600_000).toUTCString())
      serve(() => new Response(null, { status: 204 }))
      await s.adoptSession({ ...reply(), expires_at: Math.floor(serverNow() / 1000) + 60 })
      // A later answer moves the clock back: the timer already set still ends the session as an expiry.
      noteServerDate(new Date(Date.now()).toUTCString())
      await vi.advanceTimersByTimeAsync(59_000)
      expect(s.session.phase).toBe('ready')
      await vi.advanceTimersByTimeAsync(2_000)
      expect(s.session.phase).toBe('signed-out')
      expect(s.session.notice).toBe('expired')
      await vi.waitFor(async () => expect(await s.vault.outlivedExpiry()).toBe(true))
      expect(await storedAfterReload(pair)).toBe(true)

      const again = await loadKeepingPastExpiry()
      await again.adoptSession(reply())
      await again.signOut()
      expect(await storedAfterReload(pair)).toBe(false)
    } finally {
      vi.useRealTimers()
    }
  })

  it('wipes a record of anyone else instead of opening it', async () => {
    const s = await load()
    const pair = await kept(s)
    const other = await generateAccountKeys()
    expect(await s.vault.accountKeyOf(ANA_SEAL, toBase64URL(other.publicKey))).toBeNull()
    expect(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText)).toBeNull()
  })
})
