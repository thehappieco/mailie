// The console's half of the key scheme's ceremonies (docs/key-scheme.md
// section 12), against the in-memory server half of test/accountServer.ts,
// with the real derivation (Argon2id at the platform's floor), the real
// account wraps and the real browser vault: what is sent, what is never sent,
// and what this browser keeps.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { toBase64URL } from '@thehappieco/kit/bytes'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { deriveKeys, enrol, openWrap, recoveryKeys, type Enrolled } from '../src/crypto/account'
import { accountServer, DEFAULT_KDF, saltOf, targetOf, type Stored } from './accountServer'
import { freshModules, now, stubPage } from './support'

const PASSWORD = 'correct horse battery staple'
const ANA = 'ana@example.test'
const ANA_SEAL = 'b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4'

/** A fresh page: the session, the ceremonies, the vault and the errors of one module graph. */
async function load() {
  await freshModules()
  const [account, session, vault, errors, http] = await Promise.all([
    import('../src/state/account'), import('../src/state/session'), import('../src/state/accountVault'),
    import('../src/crypto/errors'), import('../src/api/http'),
  ])
  return { ...account, ...session, vault, CeremonyError: errors.CeremonyError, ApiError: http.ApiError }
}

/** Ana enrolled at her target, and at another salt (an address changed, a salt key replaced): made once, with the real derivation. */
let atTarget: Enrolled
let offTarget: Enrolled
beforeAll(async () => {
  atTarget = await enrol(PASSWORD, { salt: targetOf(ANA), kdf: DEFAULT_KDF, seal_id: ANA_SEAL }, 'new')
  offTarget = await enrol(PASSWORD, { salt: saltOf('an older salt'), kdf: DEFAULT_KDF, seal_id: ANA_SEAL }, 'new')
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
    expect(await s.vault.rememberedEnrolled(' NEW@example.test ')).toBe(true)
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

describe('the upgrade of an account made before the key scheme', { timeout: 30_000 }, () => {
  const legacy = (): Stored => ({ ...stored(atTarget, '', { legacyPassword: 'my old password' }), publicKey: undefined, authKey: '' })

  it('sends the old password once, enrols with the same password, and never sends it again for that address', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, legacy())
    await s.signIn(ANA, 'my old password')
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/upgrade/login', '/v1/auth/upgrade/enrol'])
    expect(server.calls[1]!.body).toEqual({ email: ANA, password: 'my old password' })
    const enrolment = server.calls[2]!.body
    expect(JSON.stringify(enrolment)).not.toContain('my old password')
    expect(server.people.get(ANA)!.publicKey).toBe(enrolment.public_key)
    expect(s.session.keyed).toBe(true)
    expect(s.recoveryCode.reason).toBe('new-account')

    // A server that answers upgrade again, for any spelling of the address, is not believed.
    server.people.set(ANA, legacy())
    const next = await load()
    for (const spelling of [ANA, '  ANA@example.TEST\t']) {
      await expect(next.signIn(spelling, 'my old password')).rejects.toMatchObject({ code: 'upgrade_refused' })
    }
    expect(server.paths().filter(path => path === '/v1/auth/upgrade/login')).toHaveLength(1)
  })

  it('never sends the password in clear for an address that signed in here with the key scheme', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    server.people.set(ANA, legacy())
    await expect(s.signIn(ANA, PASSWORD)).rejects.toMatchObject({ code: 'upgrade_refused' })
    expect(server.paths()).not.toContain('/v1/auth/upgrade/login')
  })

  it('remembers an address that enrolled in this page even when the browser refuses storage', async () => {
    // A browser that refuses site data: IndexedDB throws, and nothing this page learns outlives it.
    vi.stubGlobal('indexedDB', { open() { throw new Error('storage refused') } })
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    expect(s.session.notRemembered).toBe(true)
    await s.signOut()
    // Signing in again in the same page, to a server that now says upgrade.
    server.people.set(ANA, legacy())
    await expect(s.signIn(' ana@EXAMPLE.test', PASSWORD)).rejects.toMatchObject({ code: 'upgrade_refused' })
    expect(server.paths()).not.toContain('/v1/auth/upgrade/login')
    expect(sentPassword(server)).toBe(false)
  })

  it('asks for a new password when the old one cannot be used as it is', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, { ...legacy(), legacyPassword: 'old\u0007password' })
    const pending = await s.signIn(ANA, 'old\u0007password')
    expect(pending).not.toBeNull()
    expect(server.paths()).toEqual(['/v1/auth/challenge', '/v1/auth/upgrade/login'])
    await expect(s.finishUpgrade(pending!, 'too short')).rejects.toMatchObject({ code: 'password_too_short' })
    await s.finishUpgrade(pending!, 'a password with no control characters')
    expect(server.paths().at(-1)).toBe('/v1/auth/upgrade/enrol')
    expect(s.session.phase).toBe('ready')
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
    const person = server.people.get(ANA)!
    expect(person.publicKey).toBe(atTarget.enrolment.public_key)
    expect(s.session.phase).toBe('ready')
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, person.publicKey!), atTarget.accountKey)).toBe(true)
    expect(s.recoveryCode.reason).toBe('recovered')
    expect(s.recoveryCode.code).not.toBe(atTarget.recoveryCode)
    expect((await recoveryKeys(s.recoveryCode.code)).proof).toBe(person.recoveryProof)
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
  it('changes the password in two steps over the same account key, and goes on with the session the server answers', async () => {
    const s = await load()
    const server = accountServer()
    server.people.set(ANA, stored(atTarget, targetOf(ANA)))
    await s.signIn(ANA, PASSWORD)
    const before = s.session.authenticatedAt
    await s.changePassword(PASSWORD, 'the next password of mine')
    expect(server.paths().slice(2)).toEqual(['/v1/auth/challenge', '/v1/auth/password/begin', '/v1/auth/password/finish'])
    expect(sentPassword(server) || sentPassword(server, 'the next password of mine')).toBe(false)
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
    const record = await new Promise<Record<string, unknown>>((resolve, reject) => {
      const request = indexedDB.open('mailie-browser-account', 1)
      request.onerror = () => reject(request.error)
      request.onsuccess = () => {
        const get = request.result.transaction('vault').objectStore('vault').get('current')
        get.onsuccess = () => { request.result.close(); resolve(get.result as Record<string, unknown>) }
      }
    })
    const envelope = record.envelope as { key: CryptoKey; ciphertext: ArrayBuffer }
    expect(envelope.key.extractable).toBe(false)
    const text = new Uint8Array(envelope.ciphertext)
    expect(same(text.subarray(0, 32), pair.privateKey)).toBe(false)
    expect(same(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText), pair.privateKey)).toBe(true)
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

  it('wipes a record of anyone else instead of opening it', async () => {
    const s = await load()
    const pair = await kept(s)
    const other = await generateAccountKeys()
    expect(await s.vault.accountKeyOf(ANA_SEAL, toBase64URL(other.publicKey))).toBeNull()
    expect(await s.vault.accountKeyOf(ANA_SEAL, pair.publicText)).toBeNull()
  })
})
