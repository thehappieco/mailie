// What the open console never names: the hosted service's addresses, the
// company that runs it, its privacy policy and terms (in the languages the
// console speaks), and the revisions of the cloud app's texts. A self-hosted
// server's console belongs to whoever runs it. test/editions.spec.ts reads
// every file under src/ and public/, and index.html, for these; the build
// (vite.config.ts) fails on an output file that has one.
export const HOSTED = /happie|api\.mailie|console\.mailie|2026-09-|privacy policy|terms of use|política de privacidad|politique de confidentialité|datenschutzerklärung|termos de uso|términos de uso|conditions d’utilisation|nutzungsbedingungen/i

/** The first hosted name in text, or ''. */
export function hostedName(text: string): string {
  return HOSTED.exec(text)?.[0] ?? ''
}
