// The pieces the contract checks are built from: types.ts holds the replies
// the core reads to them, and an edition that reads routes of its own builds
// its checks from the same pieces, so a reply is held to one set of rules.
//
// Each piece is a type guard over an unknown value, bounded: a reply that is
// far larger than anything the contract allows is refused rather than drawn.

export type Fields = Record<string, unknown>
export const record = (v: unknown): v is Fields => typeof v === 'object' && v !== null && !Array.isArray(v)
export const text = (v: unknown, max = 4096): v is string => typeof v === 'string' && v.length <= max
export const filled = (v: unknown, max = 4096): v is string => text(v, max) && v.length > 0
export const seconds = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0
export const counter = seconds
/** A local row id: a positive integer JavaScript holds exactly. */
export const rowID = (v: unknown): v is number => counter(v) && v > 0
/** Anything a header or a body can hold; large, but bounded, so a runaway reply is refused rather than drawn. */
export const LONG_TEXT = 16 * 1024 * 1024
/**
 * What a message's sender wrote into a header: a display name, an address, a
 * subject, a Message-ID, a part's type, disposition, Content-ID or file name.
 * A folded header can make any of these as long as the sender likes, so none
 * is held to a protocol's length here, only to LONG_TEXT: one odd message must
 * not make a reply unreadable. Where one is shown, it is cut to what the
 * screen can use.
 */
export const headerText = (v: unknown): v is string => text(v, LONG_TEXT)
export const flag = (v: unknown): v is boolean => typeof v === 'boolean'
export const optional = (v: unknown, check: (v: unknown) => boolean): boolean => v === undefined || check(v)
export const oneOf = <T extends string>(options: readonly T[]) => (v: unknown): v is T => typeof v === 'string' && (options as readonly string[]).includes(v)
/** In strict mode every key must be one the contract names. */
export function known(v: Fields, keys: readonly string[], strict: boolean): boolean {
  return !strict || Object.keys(v).every(key => keys.includes(key))
}
