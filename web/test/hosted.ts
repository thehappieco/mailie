// What the open console never names: the hosted service's addresses, the
// company that runs it, its privacy policy and terms (in the languages the
// console speaks), and the revisions of the cloud app's texts. A self-hosted
// server's console belongs to whoever runs it. test/editions.spec.ts reads
// every file under src/ and public/, and index.html, for these; the build
// (vite.config.ts) fails on an output file that has one.
export const HOSTED = /happie|api\.mailie|console\.mailie|2026-09-|privacy policy|terms of use|política de privacidad|politique de confidentialité|datenschutzerklärung|termos de uso|términos de uso|conditions d’utilisation|nutzungsbedingungen/i

// The one place a source may spell the company's name: an import of the
// open-source kit the key scheme is built on (src/crypto/mailie.ts), by its
// package name, which is code and never reaches a screen. Only the quoted
// specifier of an import is set aside; the name anywhere else is still found.
const KIT_IMPORT = /((?:from|import)\s*\(?\s*)'@thehappieco\/kit(?:\/[A-Za-z0-9/._-]*)?'/g

/** The first hosted name in text, or ''. */
export function hostedName(text: string): string {
  return HOSTED.exec(text.replace(KIT_IMPORT, "$1''"))?.[0] ?? ''
}
