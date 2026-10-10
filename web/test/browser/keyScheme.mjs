// What the fake daemon (fakeDaemon.mjs) needs of the key scheme to seed a
// person who enrolled before the pass began (docs/key-scheme.md section 5):
// the auth key their password derives under their salt, and their account key
// wrapped under the password and a recovery code. The browser QA runs in plain
// Node, which cannot load src/crypto/mailie.ts, so it takes Mailie's profile
// where that module does, from the kit (@thehappieco/kit/profiles/mailie).
// Not a spec, and nothing here leaves the QA's own process.
import { derive, generateAccountKeys, newRecoveryCode, recoveryKey, recoveryProof } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL } from '@thehappieco/kit/bytes'
import { DEFAULT_KDF, mailieAccount, sealAccountWrap } from '@thehappieco/kit/profiles/mailie'

/** The server's default parameters, every account's target (section 5.2). */
export const KDF = DEFAULT_KDF

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
    passwordWrap: toBase64URL(await sealAccountWrap('password', wrapKey, privateKey, sealID)),
    recoveryWrap: toBase64URL(await sealAccountWrap('recovery', recovery, privateKey, sealID)),
    recoveryProof: await recoveryProof(mailieAccount, code),
    recoveryCode: code,
  }
  privateKey.fill(0)
  return record
}
