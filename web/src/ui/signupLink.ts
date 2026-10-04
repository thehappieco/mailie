export interface Invitation { invite: string; email: string }

/**
 * An invitation arrives as <origin>/#invite=<code>&email=<address>. Secrets
 * ride in the fragment so they are absent from HTTP and referrer logs; the
 * page takes them out and hands back the address without them, for
 * history.replaceState, so a reload, a bookmark or a screenshot of the address
 * bar carries no code.
 */
export function signupLink(current: string): Invitation & { cleanURL: string } {
  const url = new URL(current)
  const fragment = new URLSearchParams(url.hash.slice(1))
  const invite = fragment.get('invite') ?? ''
  const email = fragment.get('email') ?? ''
  for (const key of ['invite', 'email']) fragment.delete(key)
  url.hash = fragment.toString()
  return { invite: /^[A-Za-z0-9_-]{16,256}$/.test(invite) ? invite : '', email: email.length <= 320 ? email.trim() : '', cleanURL: url.toString() }
}

/** Reads the invitation from the address, if there is one, and removes it from the address at once. */
export function takeInvitation(): Invitation | null {
  if (typeof location === 'undefined') return null
  const fragment = new URLSearchParams(location.hash.slice(1))
  if (!fragment.has('invite') && !fragment.has('email')) return null
  const linked = signupLink(location.href)
  if (linked.cleanURL !== location.href) {
    try { history.replaceState(history.state, '', linked.cleanURL) } catch { /* Kept in memory either way. */ }
  }
  return linked.invite ? { invite: linked.invite, email: linked.email } : null
}
