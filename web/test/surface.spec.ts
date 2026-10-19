// What a host's own frame relies on in the core's stylesheet: the panels'
// look is ui/surface.css's, under .console-surface, which ui/styles.css
// imports first, so every edition that imports styles.css has it; and the
// core's own frame, the page that never scrolls, is only under
// html.fixed-frame (ui/frame.ts), which only App.vue asks for, so another
// host's page scrolls as any page does.
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { rules, withoutComments } from './palette'

const read = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8')
/** A rule's selectors, one by one, without the at-rule it sits in. */
const selectors = (selector: string) => selector.replace(/^@media [^{]*?\) (?=[.#a-z])/, '').split(',').map(one => one.trim())

describe('the surface the panels sit on', () => {
  it('is imported first by the stylesheet, and every rule of it sits under console-surface', () => {
    expect(withoutComments(read('ui/styles.css')).trimStart().startsWith("@import './surface.css';")).toBe(true)
    const surface = rules(read('ui/surface.css'))
    expect(surface.length).toBeGreaterThan(5)
    for (const rule of surface) {
      for (const one of selectors(rule.selector)) expect(one, rule.selector).toMatch(/^\.console-surface[.\s]/)
    }
    // The room between what a section shows, wherever a host's content is.
    expect(surface.some(rule => rule.selector.includes('.console-content > :not(:first-child)'))).toBe(true)
  })

  it('is the console frame’s, which no longer styles the panels itself', () => {
    const shell = read('components/ConsoleShell.vue')
    expect(shell).toContain('class="console console-app console-surface"')
    const style = shell.slice(shell.indexOf('<style'))
    expect(style).not.toContain('console-panel')
    expect(style).not.toContain(':deep(input')
  })
})

describe('the page that never scrolls', () => {
  const sheet = rules(read('ui/styles.css'))
  const root = (one: string) => /^(html|body|#app)\b/.test(one) || /^html\.fixed-frame\b/.test(one)

  it('is the core’s own frame only: html, body and #app keep their height, overflow and place only under html.fixed-frame', () => {
    const anyPage = new Set(['margin', 'background', 'color', 'font', '-webkit-font-smoothing'])
    for (const rule of sheet) {
      const roots = selectors(rule.selector).filter(root)
      if (!roots.length) continue
      if (roots.every(one => one.startsWith('html.fixed-frame'))) continue
      // Outside the frame, a root rule says only what any page may: no height, no overflow, no position.
      expect(roots.some(one => one.startsWith('html.fixed-frame')), rule.selector).toBe(false)
      for (const [name] of rule.declarations) expect(anyPage.has(name), `${rule.selector} { ${name} }`).toBe(true)
    }
    const framed = Object.fromEntries(sheet.filter(rule => selectors(rule.selector).every(one => one.startsWith('html.fixed-frame'))).flatMap(rule => rule.declarations))
    expect(framed).toMatchObject({ overflow: 'hidden', position: 'fixed', height: 'var(--app-height, 100dvh)', 'overscroll-behavior': 'none' })
  })

  it('is asked for by the core’s own root, and by nothing else under src/', () => {
    const src = fileURLToPath(new URL('../src', import.meta.url))
    const asking = readdirSync(src, { recursive: true, withFileTypes: true })
      .filter(entry => entry.isFile() && /\.(ts|vue)$/.test(entry.name))
      .map(entry => relative(src, join(entry.parentPath, entry.name)))
      .filter(file => /(?<!function )useFixedFrame\(\)(?!\s*:)/.test(read(file)))
    expect(asking).toEqual(['App.vue'])
    expect(read('ui/frame.ts')).toContain("FIXED_FRAME_CLASS = 'fixed-frame'")
  })
})
