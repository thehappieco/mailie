/**
 * Every request goes to the page's own origin.
 *
 * The console and the API are one origin by design: the daemon serves both
 * (and in development the Vite proxy recreates that), so the bearer token only
 * ever travels to 'self', the CSP can say connect-src 'self', and there is no
 * CORS to get wrong. There is deliberately no hostname anywhere in this client.
 */
export function endpoint(path: string): string {
  if (!path.startsWith('/v1/')) throw new Error('endpoint: not an API path')
  // Refuse anything that could change the origin or smuggle a query: callers
  // build paths from ids, and an id is never allowed to become a URL.
  if (/[?#\\]|\/\/|\/\.\.?(\/|$)/.test(path)) throw new Error('endpoint: unsafe API path')
  return new URL(path, location.origin).toString()
}

/** segment escapes one path segment built from an identifier. */
export function segment(value: string): string {
  if (!value || value === '.' || value === '..') throw new Error('endpoint: empty path segment')
  return encodeURIComponent(value)
}
