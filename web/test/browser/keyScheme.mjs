// What the fake daemon (fakeDaemon.mjs) needs of the key scheme to seed a
// person who enrolled before the pass began (docs/key-scheme.md section 5):
// the auth key their password derives under their salt, and their account key
// wrapped under the password and a recovery code. The browser QA runs in plain
// Node, which cannot load src/crypto/mailie.ts, so Mailie's account profile is
// written out here from the kit's pieces; a byte that differs from the
// console's would make the pass's first sign-in fail, which is the check.
// Not a spec, and nothing here leaves the QA's own process.
import { derive, generateAccountKeys, newRecoveryCode, recoveryKey, recoveryProof, wrapPrivateKey } from '@thehappieco/kit/account'
import { encodeUTF8, fromBase64URL, toBase64URL } from '@thehappieco/kit/bytes'
import { canonicalJSON } from '@thehappieco/kit/jcs'
import { canonicalRecoveryCode, KDF_BOUNDS, preparePassword, SALT_LEN } from '@thehappieco/kit/profiles/platform/core'

/** The server's default parameters, every account's target (section 5.2). */
export const KDF = Object.freeze({ alg: 'argon2id', m: 65536, t: 3, p: 1 })

const mailieAccount = Object.freeze({
  authLabel: 'mailie/v1/password/auth',
  wrapLabel: 'mailie/v1/password/wrap',
  recoveryKeyLabel: 'mailie/v1/recovery/wrap',
  recoveryProofLabel: 'mailie/v1/recovery/auth',
  wrapHeader: [0x02],
  legacyV1: false,
  encoding: 'base64url',
  prepare: password => preparePassword(password, { isNew: false }),
  bounds: {
    min: { m: KDF_BOUNDS.m.floor, t: KDF_BOUNDS.t.floor, p: KDF_BOUNDS.p.floor },
    max: { m: KDF_BOUNDS.m.ceiling, t: KDF_BOUNDS.t.ceiling, p: KDF_BOUNDS.p.ceiling },
    maxCost: KDF_BOUNDS.mt,
    minSaltLen: SALT_LEN,
    maxSaltLen: SALT_LEN,
  },
  normaliseRecovery: canonicalRecoveryCode,
})

/** An account wrap's additional data (section 5.5). */
const wrapAAD = (kind, sealID, publicKey) => encodeUTF8(canonicalJSON(['mailie/account-wrap', 1, kind, sealID, toBase64URL(publicKey)]))

/** A salt the server would answer for a label: 16 bytes, base64url. */
export function saltOf(label) {
  const bytes = new Uint8Array(16)
  for (const [i, byte] of new TextEncoder().encode(label).entries()) bytes[i % 16] = (bytes[i % 16] * 31 + byte) & 0xff
  return toBase64URL(bytes)
}

/**
 * enrolment is what the server stores of a person who enrolled with this
 * password under this salt and seal id, as its browser would have sent it,
 * and the recovery code that browser would have shown.
 */
export async function enrolment(password, salt, sealID) {
  const { authKey, wrapKey } = await derive(mailieAccount, password, fromBase64URL(salt, 16), KDF)
  const { publicKey, privateKey } = await generateAccountKeys()
  const code = newRecoveryCode()
  const recovery = await recoveryKey(mailieAccount, code)
  const record = {
    authKey,
    publicKey: toBase64URL(publicKey),
    passwordWrap: toBase64URL(await wrapPrivateKey(mailieAccount, privateKey, wrapKey, wrapAAD('password', sealID, publicKey))),
    recoveryWrap: toBase64URL(await wrapPrivateKey(mailieAccount, privateKey, recovery, wrapAAD('recovery', sealID, publicKey))),
    recoveryProof: await recoveryProof(mailieAccount, code),
    recoveryCode: code,
  }
  privateKey.fill(0)
  return record
}
