// Copying text for the person, when they press a Copy button.
//
// Only the asynchronous Clipboard API, which a browser allows from a click in
// a secure context: no hidden textarea and no execCommand, which would put the
// text into the page's own document. Where it is missing (a console opened
// over plain http on a LAN address) or refused, the caller says so and the
// person selects the text themselves.

export async function copyText(text: string): Promise<boolean> {
  try {
    if (typeof navigator === 'undefined' || !navigator.clipboard?.writeText) return false
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
