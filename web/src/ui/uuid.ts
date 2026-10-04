/**
 * A random v4 UUID from getRandomValues. crypto.randomUUID exists only in a
 * secure context, and a console opened over plain http on a LAN address must
 * still work: it names a browser's login, and anything an edition must not
 * have the server do twice (an Idempotency-Key).
 */
export function uuid(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6]! & 0x0f) | 0x40
  bytes[8] = (bytes[8]! & 0x3f) | 0x80
  const hex = [...bytes].map(byte => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
