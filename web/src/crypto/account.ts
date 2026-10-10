// What this browser does in each of the key scheme's ceremonies
// (docs/key-scheme.md sections 5 and 12): derive an auth key and a wrap key
// from a password under the salt and parameters the server answers, make an
// account key and a recovery code, seal and open their wraps. No state and no
// request: state/account.ts asks the server, and calls these in between.
//
// The password, the wrap keys, the recovery code's keys and the account key
// never leave the page. What goes to the server is the auth key, the
// recovery proof, the public key and the wraps, all base64url. Every refusal
// is a CeremonyError with a code the console translates, decided before
// anything is derived or sent whenever the input alone decides it.

import { AccountError, derive, generateAccountKeys, newRecoveryCode, recoveryKey, recoveryProof } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { isPlatformError } from '@thehappieco/kit/profiles/platform/core'
import type { Enrolment } from '../api/auth'
import type { KDFWire, Target } from '../api/types'
import { CeremonyError } from './errors'
import { checkKDF, isSealID, mailieAccount, openAccountWrap, prepareNewPassword, sealAccountWrap, type WrapKind } from './mailie'

const KEY_LEN = 32
const WRAP_LEN = 61

/** A password the person chose now (at least 12 code points), or one they present (no minimum). */
export type PasswordUse = 'new' | 'presented'

/** What a password becomes under a target: the auth key to send, and the wrap key that stays here. */
export interface PasswordKeys {
  authKey: string
  wrapKey: CryptoKey
  /** The parameters derived under, as the server takes them back. */
  kdf: KDFWire
}

/** Turns what the kit or this profile refused into the console's code. */
function refusal(error: unknown): CeremonyError {
  if (error instanceof CeremonyError) return error
  if (isPlatformError(error, 'password_too_short')) return new CeremonyError('password_too_short')
  if (isPlatformError(error, 'password_too_long')) return new CeremonyError('password_too_long')
  if (isPlatformError(error, 'password_invalid')) return new CeremonyError('password_invalid')
  if (isPlatformError(error, 'recovery_code')) return new CeremonyError('recovery_code')
  if (error instanceof AccountError) {
    if (error.code === 'password') return new CeremonyError('password_rejected')
    if (error.code === 'kdf' && error.reason === 'kdf_failed') return new CeremonyError('derive_failed')
  }
  // Parameters out of bounds, a seal id or a key outside its spelling, a
  // wrap that does not open: what the server answered is not used.
  return new CeremonyError('security')
}

/** A value the server answered, decoded in its one spelling; anything else is a security error. */
function bytesOf(text: string, n: number): Bytes {
  try { return fromBase64URL(text, n) } catch { throw new CeremonyError('security') }
}

/** checkNewPassword refuses a new password the platform's preparation would not take, before anything is derived or sent. */
export function checkNewPassword(password: string): void {
  try { prepareNewPassword(password) } catch (error) { throw refusal(error) }
}

/**
 * deriveKeys derives a password under a target the server answered: the
 * parameters and the salt are checked against the platform's bounds first
 * (crypto/mailie.ts checkKDF), so a server cannot make this derivation a
 * cheap one. Argon2id runs in the kit's worker where there are Workers.
 */
export async function deriveKeys(password: string, target: Target, use: PasswordUse): Promise<PasswordKeys> {
  if (use === 'new') checkNewPassword(password)
  const salt = bytesOf(target.salt, 16)
  try {
    const kdf = checkKDF(target.kdf, salt)
    const { authKey, wrapKey } = await derive(mailieAccount, password, salt, kdf)
    return { authKey, wrapKey, kdf: { alg: kdf.alg, m: kdf.m, t: kdf.t, p: kdf.p } }
  } catch (error) {
    throw refusal(error)
  }
}

/** A new recovery code, and what the server keeps of it: the account key wrapped under it, and its proof. */
export interface NewRecovery {
  /** Shown once, in six groups of five; never sent. */
  code: string
  recoveryWrap: string
  recoveryProof: string
}

/** newRecovery makes a recovery code and wraps the account key under it (docs/key-scheme.md section 5.6). */
export async function newRecovery(accountKey: Uint8Array, sealID: string): Promise<NewRecovery> {
  try {
    const code = newRecoveryCode()
    const key = await recoveryKey(mailieAccount, code)
    const wrap = await sealAccountWrap('recovery', key, accountKey, sealID)
    return { code, recoveryWrap: toBase64URL(wrap), recoveryProof: await recoveryProof(mailieAccount, code) }
  } catch (error) {
    throw refusal(error)
  }
}

/** What an enrolment made: the body the server stores, the code to show once, and the account key for the vault. */
export interface Enrolled {
  enrolment: Enrolment
  recoveryCode: string
  /** The account key, for the vault; the caller zeroes it. */
  accountKey: Bytes
  publicKey: Bytes
}

/**
 * enrol makes a person's account key and recovery code, and wraps the key
 * under a new password and the code, bound to the seal id the server answered
 * (sign-up and a reset; docs/key-scheme.md sections 12.1 and 12.6). The
 * password is prepared as a new one and derived under the target the server
 * answered with the seal id, which is what it stores.
 */
export async function enrol(password: string, opened: Target & { seal_id: string }): Promise<Enrolled> {
  if (!isSealID(opened.seal_id)) throw new CeremonyError('security')
  const keys = await deriveKeys(password, opened, 'new')
  let pair: { publicKey: Bytes; privateKey: Bytes } | undefined
  try {
    pair = await generateAccountKeys()
    const passwordWrap = await sealAccountWrap('password', keys.wrapKey, pair.privateKey, opened.seal_id)
    const recovery = await newRecovery(pair.privateKey, opened.seal_id)
    return {
      enrolment: {
        auth_key: keys.authKey, kdf: keys.kdf, public_key: toBase64URL(pair.publicKey), password_wrap: toBase64URL(passwordWrap),
        recovery_wrap: recovery.recoveryWrap, recovery_proof: recovery.recoveryProof,
      },
      recoveryCode: recovery.code,
      accountKey: pair.privateKey,
      publicKey: pair.publicKey,
    }
  } catch (error) {
    pair?.privateKey.fill(0)
    throw refusal(error)
  }
}

/**
 * openWrap opens an account wrap the server answered for a secret it just
 * verified, with the wrap key derived from that secret, for the person and
 * public key it named. A wrap that does not open is a security error, never
 * a wrong password: the server accepted the secret (docs/key-scheme.md
 * section 12.2, step 4).
 */
export async function openWrap(kind: WrapKind, wrapKey: CryptoKey, wrap: string, sealID: string, publicKey: string): Promise<Bytes> {
  try {
    return await openAccountWrap(kind, wrapKey, bytesOf(wrap, WRAP_LEN), sealID, bytesOf(publicKey, KEY_LEN))
  } catch {
    throw new CeremonyError('security')
  }
}

/** The password under a target, and the account key wrapped under it: what a password change and a re-derivation store. */
export interface Rewrapped { auth_key: string; kdf: KDFWire; password_wrap: string }

/** rewrap derives a password under a target and wraps the same account key under it (docs/key-scheme.md sections 12.2, step 5, and 12.3). */
export async function rewrap(password: string, target: Target, use: PasswordUse, accountKey: Uint8Array, sealID: string): Promise<Rewrapped> {
  const keys = await deriveKeys(password, target, use)
  try {
    const wrap = await sealAccountWrap('password', keys.wrapKey, accountKey, sealID)
    return { auth_key: keys.authKey, kdf: keys.kdf, password_wrap: toBase64URL(wrap) }
  } catch (error) {
    throw refusal(error)
  }
}

/** A recovery code typed back: its wrap key, and the proof the server checks. */
export interface RecoveryKeys { key: CryptoKey; proof: string }

/** recoveryKeys reads a recovery code back in the platform's canonical form and derives its keys; anything else is recovery_code. */
export async function recoveryKeys(code: string): Promise<RecoveryKeys> {
  try {
    return { key: await recoveryKey(mailieAccount, code), proof: await recoveryProof(mailieAccount, code) }
  } catch (error) {
    throw refusal(error)
  }
}
