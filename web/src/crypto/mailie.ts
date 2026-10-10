// Mailie's profile of the kit's key scheme, for the browser: the labels,
// headers, magic, kinds and additional data with which a person's account key
// is wrapped, a mailbox's private key is granted to a person, and the account
// key is kept in this browser. ../../../docs/key-scheme.md is the
// specification and key-scheme-threat-model.md beside it what it defends
// against. The Go side is internal/keyscheme, which writes the vectors
// web/test/keyscheme.spec.ts opens with this module.
//
// The profile is the kit's own, its profiles/mailie module, which the kit's
// 0.7.0 took from this specification, frozen by its vectors; this
// module re-exports it whole and keeps only what the kit leaves to the
// console: drawing a mailbox's namespace, the address as the server stores
// it, and the one way the console reads the raw account key back out of the
// vault. Every refusal of the profile's own is the kit's MailieError
// (binding, shape or vault), which the console catches by class and code;
// the kit's other errors (AccountError, SealError, PlatformWrapError, the
// platform profile's PlatformError) pass through as they are. The account's
// ceremonies (sign-up, sign-in, recovery, the upgrade) use it through
// crypto/account.ts; mailbox keys and their grants, through
// crypto/mailbox.ts.

import { MailieError, openBrowserVault, browserVaultAAD, type BrowserKeyEnvelope, type Bytes } from '@thehappieco/kit/profiles/mailie'

export * from '@thehappieco/kit/profiles/mailie'

const KEY_LEN = 32

/** newNamespace draws a mailbox's namespace: a random UUIDv4. */
export function newNamespace(): string {
  return crypto.randomUUID()
}

// The white space Go's strings.TrimSpace removes (unicode.IsSpace), by code
// point. Not ECMAScript's trim, which also removes U+FEFF and keeps U+0085.
const GO_WHITE_SPACE: ReadonlySet<number> = new Set([
  0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680,
  0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
  0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
])

/**
 * lowerCodePoint is Unicode's simple lowercase mapping of one code point, as
 * Go's unicode.ToLower: ECMAScript's toLowerCase on the code point alone,
 * which applies no context (a capital sigma is always σ, never the final ς
 * toLowerCase gives a whole word), except U+0130, whose full mapping adds a
 * combining dot that the simple one does not.
 */
function lowerCodePoint(cp: number): string {
  if (cp === 0x130) return 'i'
  const one = String.fromCodePoint(cp)
  const lower = one.toLowerCase()
  return Array.from(lower).length === 1 ? lower : one
}

/**
 * normaliseAddress is an address as the server stores it (docs/key-scheme.md
 * section 2; Go keyscheme.NormaliseAddress, held to the same vectors): white
 * space trimmed at both ends as Go's strings.TrimSpace does, and every letter
 * lowered by its simple mapping, as Go's strings.ToLower does. The browser
 * keys its memory of the addresses that enrolled with it, so every spelling
 * the server takes for one account is one record. It validates nothing.
 */
export function normaliseAddress(address: string): string {
  const cps = Array.from(address, c => c.codePointAt(0) ?? 0)
  let start = 0
  let end = cps.length
  while (start < end && GO_WHITE_SPACE.has(cps[start] ?? 0)) start++
  while (end > start && GO_WHITE_SPACE.has(cps[end - 1] ?? 0)) end--
  let out = ''
  for (let i = start; i < end; i++) out += lowerCodePoint(cps[i] ?? 0)
  return out
}

/**
 * openBrowserVaultKey is openBrowserVault for the one caller that wraps the
 * account key again (a new recovery code, docs/key-scheme.md section 12.5):
 * the same checks, the kit's opening first (the AAD, and the public half
 * against the one recorded), then the raw 32 bytes the envelope holds, which
 * the caller zeroes once the new wrap is sealed. It never leaves the page. A
 * record that does not open is MailieError vault, as openBrowserVault's is.
 */
export async function openBrowserVaultKey(envelope: BrowserKeyEnvelope, sealID: string): Promise<Bytes> {
  await openBrowserVault(envelope, sealID)
  try {
    const raw = new Uint8Array(await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: envelope.nonce, additionalData: browserVaultAAD(sealID, envelope.publicRaw) }, envelope.key, envelope.ciphertext,
    )) as Bytes
    if (raw.length !== KEY_LEN) {
      raw.fill(0)
      throw new Error('not an account key')
    }
    return raw
  } catch {
    throw new MailieError('the vault record does not open for this person', 'vault')
  }
}
