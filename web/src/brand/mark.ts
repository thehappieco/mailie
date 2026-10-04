/**
 * The Mailie mark — PROVISIONAL.
 *
 * There is no Mailie kit in the company brand repository yet, so this drawing
 * is built here from the family's own rules and must be replaced, literal for
 * literal, when the official kit exists (web/README.md says the same).
 *
 * What is borrowed and what is new:
 * - The frame is the family frame, byte-for-byte: W = 12u, I = 3πu, R = πu
 *   (u = 7.5), identical in every kit.
 * - The envelope body is 6u wide and W/e tall — the Wappie bubble's box —
 *   centred on (45, 45), with corners of u/2 so it reads as paper, not speech.
 * - The flap is a "cut": stroked in the INTERIOR colour at t/2, the Wappie
 *   chevrons' width. It runs from the centres of the two top corner arcs down to
 *   the body's golden section (h/φ from the top), the ratio Authie's arms use.
 * - The colour is RGB = round(18π², 24π, 11e) = #B24B1E, a terracotta no
 *   sibling uses (navy, green, indigo, petrol, amethyst, rose), 5.36:1 on white.
 *
 * The reason the mark is inline SVG instead of an <img> is that each part
 * animates on its own, like the siblings'.
 */

export type MarkMode = 'outline' | 'tile'
export type PartKind = 'fill' | 'cut'

export interface Part {
  id: string
  /** fill = painted in the symbol colour; cut = stroked in the INTERIOR colour (the kits paint it that way). */
  kind: PartKind
  d: string
  width?: number
  linecap?: 'round'
  linejoin?: 'round'
  /** Geometric path length, used by draw(); only stroked parts have one. */
  length?: number
  /** Choreographed as a stroke draw-in (dash attributes while playing). */
  draw?: boolean
}

/** Rounded square, identical in every kit (W = 90, R = πu). */
export const FRAME_OUTER = 'M 23.561944902 0 H 66.438055098 A 23.561944902 23.561944902 0 0 1 90 23.561944902 V 66.438055098 A 23.561944902 23.561944902 0 0 1 66.438055098 90 H 23.561944902 A 23.561944902 23.561944902 0 0 1 0 66.438055098 V 23.561944902 A 23.561944902 23.561944902 0 0 1 23.561944902 0 Z'
/** Interior (always opaque), inset by the frame stroke with Ri = R - t. */
export const FRAME_INNER = 'M 23.561944902 9.657082647 H 66.438055098 A 13.904862255 13.904862255 0 0 1 80.342917353 23.561944902 V 66.438055098 A 13.904862255 13.904862255 0 0 1 66.438055098 80.342917353 H 23.561944902 A 13.904862255 13.904862255 0 0 1 9.657082647 66.438055098 V 23.561944902 A 13.904862255 13.904862255 0 0 1 23.561944902 9.657082647 Z'

/** The provisional brand hex; mark.css, the lockup and the favicons carry the same literal. */
export const MAILIE_COLOR = '#B24B1E'

export const MAILIE_PARTS: readonly Part[] = [
  { id: 'body', kind: 'fill',
    d: 'M 26.25 28.445425147 H 63.75 A 3.75 3.75 0 0 1 67.5 32.195425147 V 57.804574853 A 3.75 3.75 0 0 1 63.75 61.554574853 H 26.25 A 3.75 3.75 0 0 1 22.5 57.804574853 V 32.195425147 A 3.75 3.75 0 0 1 26.25 28.445425147 Z' },
  { id: 'flap', kind: 'cut', draw: true, width: 4.828541324, linecap: 'round', linejoin: 'round', length: 50.234364,
    d: 'M 26.25 32.195425147 L 45 48.908005004 L 63.75 32.195425147' },
]

const round = (value: number): string => String(Math.round(value * 1e6) / 1e6)

/**
 * Dash attributes for a stroke draw-in. D = 1.01·L (rounded up to 0.01) so the
 * dash always covers the whole path; c = width/2 + 0.5 pushes the pattern back
 * so the round cap of the not-yet-drawn dash sits outside the path start
 * (otherwise a dot shows at frame 0). The `bm-draw` keyframe takes dashoffset
 * from this value to 0. Identical to the siblings' draw().
 */
export function draw(length: number, width: number): { dasharray: string; dashoffset: string } {
  const span = Math.ceil(1.01 * length * 100) / 100
  const cap = width / 2 + 0.5
  return { dasharray: `${round(span)} ${round(span + 2 * cap)}`, dashoffset: round(span + cap) }
}
