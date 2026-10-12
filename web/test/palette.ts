// The palette's tokens (src/ui/palette.css) and a small reader of the
// stylesheets that declare them, for test/palette.spec.ts and for an edition
// that declares the same properties its own way and holds its stylesheet to
// the same list (docs/console.md, "Editions").

/**
 * The colours, light and dark, the monospace font and the motion curves: the
 * custom properties the console's stylesheets read, which palette.css
 * declares for the light page and an edition may declare its own way.
 */
export const COLOUR_TOKENS = [
  '--bg', '--bg-panel', '--bg-raised', '--bg-hover', '--bg-active', '--bg-input',
  '--text', '--text-dim', '--text-faint', '--line',
  '--brand', '--accent', '--accent-dim', '--danger', '--warn', '--warn-text', '--ok', '--ok-text', '--on-accent', '--on-danger',
  '--console-accent', '--console-tint', '--console-border',
  '--radius', '--mono', '--ease', '--pop',
] as const

/** The shapes of a few surfaces, with the open console's values, which palette.css keeps as they were. */
export const GEOMETRY: Readonly<Record<string, string>> = {
  '--card-radius': '22px',
  '--card-shadow': '0 18px 70px #0000000d',
  '--dialog-radius': '20px',
  '--dialog-shadow': '0 24px 90px #0005',
  '--control-radius': '12px',
}

/** Every token the palette declares for the light page: the colours and the shapes. */
export const TOKENS: readonly string[] = [...COLOUR_TOKENS, ...Object.keys(GEOMETRY)]

/** A stylesheet without its comments. Only for CSS: a script's string may hold a slash and a star. */
export function withoutComments(css: string): string {
  return css.replace(/\/\*[^]*?\*\//g, '')
}

/** The custom properties a stylesheet declares (--name: value), wherever they are. */
export function declared(css: string): string[] {
  return [...withoutComments(css).matchAll(/(?:^|[{;\s])(--[\w-]+)\s*:/g)].map(match => match[1]!)
}

/** A rule of a stylesheet: its selector, after the at-rules it sits in, and its declarations in order, name and value. */
export interface Rule {
  selector: string
  declarations: [string, string][]
}

/**
 * The rules of a stylesheet with declarations of their own, in order. A
 * nested rule's selector is its at-rule's prelude and its own, joined by a
 * space ("@media (prefers-color-scheme: dark) :root:not([data-theme])").
 * Whitespace in selectors and values is collapsed. Enough for a palette's
 * plain rules: no braces or semicolons in strings.
 */
export function rules(css: string): Rule[] {
  const found: Rule[] = []
  const open: { selector: string; body: string }[] = []
  let text = ''
  const collapse = (value: string) => value.trim().replace(/\s+/g, ' ')
  for (const char of withoutComments(css)) {
    if (char === '{') {
      const cut = text.lastIndexOf(';') + 1
      if (open.length) open.at(-1)!.body += text.slice(0, cut)
      open.push({ selector: [open.at(-1)?.selector, collapse(text.slice(cut))].filter(Boolean).join(' '), body: '' })
      text = ''
    } else if (char === '}') {
      const rule = open.pop()
      if (!rule) throw new Error('palette: a closing brace with nothing open')
      const declarations = (rule.body + text).split(';').map(part => part.trim()).filter(Boolean).map((part): [string, string] => {
        const colon = part.indexOf(':')
        if (colon < 0) throw new Error(`palette: not a declaration in ${rule.selector}: ${part}`)
        return [part.slice(0, colon).trim(), collapse(part.slice(colon + 1))]
      })
      if (declarations.length) found.push({ selector: rule.selector, declarations })
      text = ''
    } else {
      text += char
    }
  }
  if (open.length || text.trim()) throw new Error('palette: a rule left open')
  return found
}
