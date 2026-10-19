// The palette (src/ui/palette.css) holds the console's tokens, and nothing
// else declares them: an edition's entry imports it before the stylesheet,
// or declares the same properties its own way (docs/console.md, "Editions").
// The tokens are the list test/palette.ts keeps, the light page's once and
// the dark page's twice the same, with the open console's shapes. And every
// custom property a source reads must be declared by a stylesheet under src/,
// or the page would draw it with nothing.
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { COLOUR_TOKENS, declared, GEOMETRY, rules, TOKENS, withoutComments } from './palette'

const src = fileURLToPath(new URL('../src', import.meta.url))
const read = (file: string) => readFileSync(file, 'utf8')

/**
 * What the page writes itself, at run time, with no stylesheet declaring it,
 * and why. A source that reads one names a fallback beside it. Nothing else
 * under src/ sets a custom property from a script or a style binding.
 */
const WRITTEN_AT_RUN_TIME: Record<string, string> = {
  '--app-height': 'state/appLifecycle.ts writes the visual viewport’s height on the document, for the core’s own frame and the full-screen dialog on a phone',
  '--app-top': 'state/appLifecycle.ts writes the visual viewport’s offset on the document beside it',
}

const STYLE = /<style\b[^>]*>([^]*?)<\/style>/g

/**
 * Every stylesheet, component and script under src/, split in two: its CSS
 * (a .css file, a .vue file's <style> blocks), without comments, and the rest
 * (a script, a .vue file's template and script), as written: a string there
 * may hold a slash and a star that opens no comment.
 */
const sources = readdirSync(src, { withFileTypes: true, recursive: true })
  .filter(entry => entry.isFile() && /\.(css|vue|ts)$/.test(entry.name))
  .map(entry => {
    const path = join(entry.parentPath, entry.name)
    const file = relative(src, path)
    const text = read(path)
    if (file.endsWith('.css')) return { file, css: [withoutComments(text)], code: '' }
    if (file.endsWith('.vue')) return { file, css: [...text.matchAll(STYLE)].map(([, css]) => withoutComments(css!)), code: text.replace(STYLE, '') }
    return { file, css: [], code: text }
  })

/** The stylesheets under src/, each with the file it is in. */
const stylesheets = sources.flatMap(({ file, css }) => css.map(text => ({ file, css: text })))

/** Where each custom property is read, var(--name) in a stylesheet, a template or a script, as the text around it, by file. */
function reads(): Map<string, { file: string; read: string }[]> {
  const found = new Map<string, { file: string; read: string }[]>()
  for (const { file, css, code } of sources) {
    for (const text of [...css, code]) {
      for (const match of text.matchAll(/var\(\s*(--[\w-]+)[^)]*/g)) {
        found.set(match[1]!, [...found.get(match[1]!) ?? [], { file, read: match[0] }])
      }
    }
  }
  return found
}

/** The custom properties a template or a script sets: setProperty, a key of a style object, a style attribute. */
function written(): string[] {
  return sources.flatMap(({ code }) => [
    ...[...code.matchAll(/setProperty\(\s*['"`](--[\w-]+)/g)].map(match => match[1]!),
    ...[...code.matchAll(/['"`](--[\w-]+)['"`]\s*:/g)].map(match => match[1]!),
    ...[...code.matchAll(/\bstyle="([^"]*)"/g)].flatMap(([, style]) => declared(style!)),
  ])
}

const palette = rules(read(join(src, 'ui', 'palette.css')))
const LIGHT = ':root'
const DARK = ":root[data-theme='dark']"
const SYSTEM_DARK = '@media (prefers-color-scheme: dark) :root:not([data-theme])'
/** A rule's declarations, name to value. */
const valuesOf = (selector: string) => Object.fromEntries(palette.filter(rule => rule.selector === selector).flatMap(rule => rule.declarations))
const tokensOf = (selector: string) => palette.filter(rule => rule.selector === selector).flatMap(rule => rule.declarations).map(([name]) => name).filter(name => name.startsWith('--'))

describe('the palette', () => {
  it('leaves no custom property a source reads undeclared under src/ (palette.css’s, or the component’s own), but the two the page’s life writes at run time, read with a fallback', () => {
    const declarations = new Set(stylesheets.flatMap(sheet => declared(sheet.css)))
    const found = reads()
    expect(found.size).toBeGreaterThan(30)
    const undeclared = [...found].filter(([name]) => !declarations.has(name) && !(name in WRITTEN_AT_RUN_TIME))
      .map(([name, places]) => `${name} (${[...new Set(places.map(place => place.file))].join(', ')})`)
    expect(undeclared).toEqual([])
    // The exceptions are exactly what the page writes, it still writes them,
    // and every read of one names a fallback for the page before it does.
    expect([...new Set(written())].sort()).toEqual(Object.keys(WRITTEN_AT_RUN_TIME).sort())
    const app = read(join(src, 'state', 'appLifecycle.ts'))
    for (const name of Object.keys(WRITTEN_AT_RUN_TIME)) {
      expect(found.has(name), name).toBe(true)
      expect(declarations.has(name), name).toBe(false)
      expect(app).toContain(`setProperty('${name}'`)
      for (const place of found.get(name)!) expect(place.read, place.file).toMatch(new RegExp(`^var\\(\\s*${name}\\s*,\\s*\\S`))
    }
  })

  it('declares the light page’s tokens once, exactly the list, and the dark page’s twice the same, only colours among them', () => {
    expect(palette.map(rule => rule.selector)).toEqual([LIGHT, DARK, SYSTEM_DARK, LIGHT])
    const light = tokensOf(LIGHT)
    expect([...light].sort()).toEqual([...TOKENS].sort())
    expect(light).toHaveLength(32)
    expect(valuesOf(LIGHT)['color-scheme']).toBe('light')
    // Dark, for the explicit choice and for a system that is dark before the
    // script sets one: the same declarations, so neither page differs.
    const dark = palette.find(rule => rule.selector === DARK)!.declarations
    const system = palette.find(rule => rule.selector === SYSTEM_DARK)!.declarations
    expect(Object.fromEntries(system)).toEqual(Object.fromEntries(dark))
    expect(system).toHaveLength(dark.length)
    expect(valuesOf(DARK)['color-scheme']).toBe('dark')
    const darkTokens = tokensOf(DARK)
    expect(new Set(darkTokens).size).toBe(darkTokens.length)
    expect(darkTokens.length).toBeGreaterThan(15)
    expect(darkTokens.filter(name => !(COLOUR_TOKENS as readonly string[]).includes(name))).toEqual([])
  })

  it('keeps the open console’s shapes: the card’s, the dialogs’ and the card’s button’s', () => {
    const values = valuesOf(LIGHT)
    for (const [name, value] of Object.entries(GEOMETRY)) expect(values[name], name).toBe(value)
  })

  it('is palette.css’s alone: no other stylesheet under src/ declares a token, styles.css no custom property at all, and the open console imports the palette before it', () => {
    const tokens = new Set(TOKENS)
    const elsewhere = stylesheets.filter(sheet => sheet.file !== join('ui', 'palette.css'))
      .flatMap(sheet => declared(sheet.css).filter(name => tokens.has(name)).map(name => `${name} (${sheet.file})`))
    expect(elsewhere).toEqual([])
    expect(stylesheets.length).toBeGreaterThan(10)
    expect(declared(read(join(src, 'ui', 'styles.css')))).toEqual([])
    const main = read(join(src, 'main.ts'))
    expect(main).toContain("import './ui/palette.css'")
    expect(main.indexOf("import './ui/palette.css'")).toBeLessThan(main.indexOf("import './ui/styles.css'"))
  })
})
