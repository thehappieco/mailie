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
  // A reset link carries an address too: it is takeReset's.
  if (fragment.has('reset') || (!fragment.has('invite') && !fragment.has('email'))) return null
  const linked = signupLink(location.href)
  if (linked.cleanURL !== location.href) {
    try { history.replaceState(history.state, '', linked.cleanURL) } catch { /* Kept in memory either way. */ }
  }
  return linked.invite ? { invite: linked.invite, email: linked.email } : null
}

/**
 * A reset invitation, the operator's link for a person who lost both their
 * password and their recovery code (docs/key-scheme.md section 12.6):
 * <origin>/#reset=<code>&email=<address>. Its code rides in the fragment and
 * leaves the address bar at once, as an invitation's does.
 */
export interface ResetLink { reset: string; email: string }

export function resetLink(current: string): ResetLink & { cleanURL: string } {
  const url = new URL(current)
  const fragment = new URLSearchParams(url.hash.slice(1))
  const reset = fragment.get('reset') ?? ''
  const email = fragment.get('email') ?? ''
  for (const key of ['reset', 'email']) fragment.delete(key)
  url.hash = fragment.toString()
  return { reset: /^[A-Za-z0-9_-]{16,256}$/.test(reset) ? reset : '', email: email.length <= 320 ? email.trim() : '', cleanURL: url.toString() }
}

/** Reads the reset link from the address, if there is one, and removes it from the address at once. */
export function takeReset(): ResetLink | null {
  if (typeof location === 'undefined') return null
  if (!new URLSearchParams(location.hash.slice(1)).has('reset')) return null
  const linked = resetLink(location.href)
  if (linked.cleanURL !== location.href) {
    try { history.replaceState(history.state, '', linked.cleanURL) } catch { /* Kept in memory either way. */ }
  }
  return linked.reset && linked.email ? { reset: linked.reset, email: linked.email } : null
}
