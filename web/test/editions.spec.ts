// What makes src/ a core the editions build on: the core never reaches
// outside src/ (an edition imports it, never the other way round), and it
// names no hosted service, company, policy or hosted text revision anywhere
// (test/hosted.ts): it is public source, and a self-hosted server serves none
// of it (platform decision 0063). The core ships no edition and no console of
// its own: each edition brings its console and its texts, names its frame,
// and may draw its own brand and appearance control there (Edition.look);
// the specs run under test/edition.ts.
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, defineComponent, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import { configureEdition } from '../src/edition'
import { TestConsole, testEdition } from './edition'
import { accounts } from '../src/state/accounts'
import { session } from '../src/state/session'
import { workspaces } from '../src/state/workspaces'
import { locale } from '../src/ui/i18n'
import { hostPreferences, setPreferenceStore } from '../src/ui/preferences'
import { hostedName } from './hosted'
import { literalKeys, sourceFiles } from './i18nGuard'
import { ana } from './support'

const src = fileURLToPath(new URL('../src', import.meta.url))
const web = fileURLToPath(new URL('..', import.meta.url))

/** Every file under dir, whatever its kind: sources, catalogs, styles, icons. */
function everyFile(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true, recursive: true })
    .filter(entry => entry.isFile())
    .map(entry => join(entry.parentPath, entry.name))
}

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), context)
  return html + Object.values(context.teleports ?? {}).join('')
}

afterEach(() => {
  configureEdition(testEdition)
  locale.value = 'en'
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0 })
  Object.assign(accounts, { list: [], loaded: false })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  vi.unstubAllGlobals()
})

describe('the core and the editions', () => {
  it('never imports from outside src/, nor through an alias: another edition imports the core, never the other way round', () => {
    // A scoped name is an alias unless it is a package the console depends
    // on (package.json's dependencies: the kit, @thehappieco/kit), which
    // every edition resolves the same way from node_modules.
    const dependencies = Object.keys((JSON.parse(readFileSync(join(web, 'package.json'), 'utf8')) as { dependencies: Record<string, string> }).dependencies)
    const dependency = (specifier: string) => dependencies.some(name => specifier === name || specifier.startsWith(`${name}/`))
    const offenders: string[] = []
    for (const file of sourceFiles(src)) {
      for (const [, specifier] of readFileSync(file, 'utf8').matchAll(/(?:from|import)\s*\(?\s*'([^']+)'/g)) {
        const target = specifier!.startsWith('.') ? relative(src, resolve(dirname(file), specifier!)) : specifier!
        if (target.startsWith('..') || (target.startsWith('@') && !dependency(target))) offenders.push(`${relative(src, file)} → ${specifier}`)
      }
    }
    expect(offenders).toEqual([])
  })

  it('sets aside only the quoted package name of an import of the kit, never the company’s name anywhere else', () => {
    expect(hostedName(`import { seal } from '@thehappieco/kit/seal'`)).toBe('')
    expect(hostedName(`export { x } from '@thehappieco/kit'`)).toBe('')
    expect(hostedName(`const kit = '@thehappieco/kit'`)).toBe('happie')
    expect(hostedName(`import { seal } from '@thehappieco/kit/seal' // by The Happie Co`)).toBe('Happie')
    expect(hostedName(`import x from '@thehappieco/kitchen'`)).toBe('happie')
  })

  it('names no hosted service, company, policy or cloud text revision in any source, catalog, stylesheet or icon', () => {
    const files = [...everyFile(src), ...everyFile(join(web, 'public'))]
    expect(files.length).toBeGreaterThan(50)
    const offenders = files.flatMap(file => {
      const name = hostedName(readFileSync(file, 'utf8'))
      return name ? [`${relative(web, file)}: ${name}`] : []
    })
    expect(offenders).toEqual([])
  })

  it('words no shared sentence as a console’s: each edition names its own frame, a console or an app', () => {
    const catalogs = import.meta.glob<Record<string, string[]>>('../src/ui/locales/*.json', { eager: true, import: 'default' })
    const naming = Object.values(catalogs).flatMap(catalog => Object.entries(catalog))
      .filter(([key, translations]) => /console|consola|konsole/i.test([key, ...translations].join(' '))).map(([key]) => key)
    expect(naming).toEqual([])
    const users = sourceFiles(src).filter(file => literalKeys(readFileSync(file, 'utf8')).some(key => /console|consola|konsole/i.test(key)))
    expect(users.map(file => relative(src, file))).toEqual([])
  })

  it('refuses to render a screen before an edition is configured', async () => {
    vi.resetModules()
    const { edition } = await import('../src/edition')
    expect(() => edition()).toThrow(/configureEdition/)
  })
})

describe('an edition and the core’s frame', () => {
  it('keeps the theme and the language in the core’s own store, on this host alone, for an edition that sets none: only the edition seam installs another', () => {
    expect(testEdition.preferences).toBeUndefined()
    configureEdition(testEdition)
    expect(setPreferenceStore(null)).toBe(hostPreferences)
    const installing = sourceFiles(src).filter(file => readFileSync(file, 'utf8').includes('setPreferenceStore(')).map(file => relative(src, file)).sort()
    expect(installing).toEqual(['edition.ts', 'ui/preferences.ts'])
  })

  it('names its frame as the edition does: the sidebar, the lockup and the breadcrumb', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
    Object.assign(accounts, { list: [], loaded: true })
    const breadcrumb = (html: string) => (html.match(/<div class="console-breadcrumb"[^>]*>(.*?)<\/div>/)?.[1] ?? '').replace(/<[^>]+>/g, '').replace(/\s+/g, ' ')
    const html = await render(TestConsole)
    expect(html).toMatch(/<aside class="console-sidebar" aria-label="Console navigation"/)
    expect(html).toMatch(/<span class="lockup"[^>]*role="img" aria-label="Mailie · Console"/)
    expect(breadcrumb(html)).toBe('Console / Mailboxes')
    configureEdition({ ...testEdition, shell: { rootLabel: () => 'Mail app', navLabel: () => 'Mail app navigation' } })
    const app = await render(TestConsole)
    expect(app).toMatch(/<aside class="console-sidebar" aria-label="Mail app navigation"/)
    expect(app).toMatch(/<span class="lockup"[^>]*role="img" aria-label="Mailie · Mail app"/)
    expect(breadcrumb(app)).toBe('Mail app / Mailboxes')
  })
})

describe('an edition’s look in the frame', () => {
  /** An edition's brand, which draws where it is and what it is called, so a spec reads both. */
  const Brand = defineComponent({
    name: 'Brand',
    props: { place: { type: String, required: true }, label: { type: String, required: true } },
    setup: props => () => h('span', { class: 'edition-brand', role: 'img', 'aria-label': props.label, 'data-place': props.place }),
  })
  const Appearance = defineComponent({ name: 'Appearance', render: () => h('div', { class: 'edition-appearance' }, [h('button', { type: 'button' }, 'Theme')]) })
  /** The page as a browser would get it, without scoped-style attributes and Vue's comment anchors. */
  const plain = (html: string) => html.replace(/ data-v-[0-9a-f]+/g, '').replace(/<!--[^]*?-->/g, '')
  /** The sidebar's brand and the header, as the frame draws them. */
  const sidebarBrand = (html: string) => plain(html).match(/<div class="console-brand">([^]*?)<\/div>/)?.[1] ?? ''
  const header = (html: string) => plain(html).match(/<header class="console-header">([^]*?)<\/header>/)?.[1] ?? ''

  function signedIn() {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
    Object.assign(accounts, { list: [], loaded: true })
  }

  it('draws the edition’s brand atop the sidebar, told it is in the menu and named as the core names its lockup, and its appearance control in the header', async () => {
    signedIn()
    configureEdition({ ...testEdition, look: { brand: Brand, appearance: Appearance } })
    const html = await render(TestConsole)
    expect(sidebarBrand(html)).toBe('<span class="edition-brand" role="img" aria-label="Mailie · Console" data-place="menu"></span>')
    expect(header(html)).toMatch(/<\/h1><\/div><div class="edition-appearance"><button type="button">Theme<\/button><\/div><button class="mobile-profile"/)
    for (const core of ['class="lockup"', 'appearance-trigger', 'appearance-dialog']) expect(html).not.toContain(core)
    // The rest of the frame is the core's.
    expect(html).toMatch(/<aside class="console-sidebar" aria-label="Console navigation"/)
    expect(plain(html)).toContain('<h1>Mailboxes</h1>')
  })

  it('draws only what the edition sets, and the core’s in the other place', async () => {
    signedIn()
    configureEdition({ ...testEdition, look: { brand: Brand } })
    let html = await render(TestConsole)
    expect(sidebarBrand(html)).toBe('<span class="edition-brand" role="img" aria-label="Mailie · Console" data-place="menu"></span>')
    expect(header(html)).toMatch(/<\/h1><\/div><button type="button" class="appearance-trigger"[^>]*>[^]*?<\/button><button class="mobile-profile"/)
    expect(html).not.toContain('class="lockup"')
    expect(html).not.toContain('edition-appearance')
    configureEdition({ ...testEdition, look: { appearance: Appearance } })
    html = await render(TestConsole)
    expect(sidebarBrand(html)).toMatch(/^<span class="lockup" data-size="34" role="img" aria-label="Mailie · Console"/)
    expect(header(html)).toMatch(/<\/h1><\/div><div class="edition-appearance"><button type="button">Theme<\/button><\/div><button class="mobile-profile"/)
    expect(html).not.toContain('appearance-trigger')
    expect(html).not.toContain('edition-brand')
  })

  it('is the core’s when the edition sets none: the core’s lockup and appearance menu', async () => {
    signedIn()
    for (const edition of [testEdition, { ...testEdition, look: {} }]) {
      configureEdition(edition)
      const html = await render(TestConsole)
      expect(sidebarBrand(html)).toMatch(/^<span class="lockup" data-size="34" role="img" aria-label="Mailie · Console"/)
      expect(header(html)).toMatch(/<\/h1><\/div><button type="button" class="appearance-trigger"[^>]*>[^]*?<\/button><button class="mobile-profile"/)
      expect(html).toContain('appearance-dialog')
      expect(html).not.toMatch(/edition-brand|edition-appearance/)
    }
  })
})
