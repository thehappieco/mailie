// What makes src/ the open console and a core another edition builds on: the
// core never reaches outside src/ (another edition imports it, never the
// other way round), the open edition's texts belong to whoever runs the
// server (no company, no policy link), and its console has the server's
// sections and nothing to read or send mail with.
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import OpenConsole from '../src/open/OpenConsole.vue'
import ActionsText from '../src/open/ActionsText.vue'
import KeyTermsText from '../src/open/KeyTermsText.vue'
import SyncText from '../src/open/SyncText.vue'
import { accounts } from '../src/state/accounts'
import { session } from '../src/state/session'
import { workspaces } from '../src/state/workspaces'
import { languageOptions, locale } from '../src/ui/i18n'
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
  locale.value = 'en'
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0 })
  Object.assign(accounts, { list: [], loaded: false })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  vi.unstubAllGlobals()
})

describe('the core and the editions', () => {
  it('never imports from outside src/, nor through an alias: another edition imports the core, never the other way round', () => {
    const offenders: string[] = []
    for (const file of sourceFiles(src)) {
      for (const [, specifier] of readFileSync(file, 'utf8').matchAll(/(?:from|import)\s*\(?\s*'([^']+)'/g)) {
        const target = specifier!.startsWith('.') ? relative(src, resolve(dirname(file), specifier!)) : specifier!
        if (target.startsWith('..') || target.startsWith('@')) offenders.push(`${relative(src, file)} → ${specifier}`)
      }
    }
    expect(offenders).toEqual([])
  })

  it('names no hosted service, company, policy or cloud text revision in any source, catalog, icon or the page itself', () => {
    const files = [...everyFile(src), ...everyFile(join(web, 'public')), join(web, 'index.html')]
    expect(files.length).toBeGreaterThan(50)
    const offenders = files.flatMap(file => {
      const name = hostedName(readFileSync(file, 'utf8'))
      return name ? [`${relative(web, file)}: ${name}`] : []
    })
    expect(offenders).toEqual([])
  })

  it('words no shared sentence as a console’s: only the open edition calls its frame one, another calls its own an app', () => {
    const catalogs = import.meta.glob<Record<string, string[]>>('../src/ui/locales/*.json', { eager: true, import: 'default' })
    const naming = Object.values(catalogs).flatMap(catalog => Object.entries(catalog))
      .filter(([key, translations]) => /console|consola|konsole/i.test([key, ...translations].join(' '))).map(([key]) => key)
    expect(naming.sort()).toEqual(['Console', 'Console navigation'])
    const users = sourceFiles(src).filter(file => literalKeys(readFileSync(file, 'utf8')).some(key => naming.includes(key)))
    expect(users.map(file => relative(src, file))).toEqual(['open/edition.ts'])
  })

  it('refuses to render a screen before an edition is configured', async () => {
    vi.resetModules()
    const { edition } = await import('../src/edition')
    expect(() => edition()).toThrow(/configureEdition/)
  })
})

describe('the open edition', () => {
  it('words what a person agrees to as this server’s, in every language, linking no policy and naming no company', async () => {
    for (const { value } of languageOptions) {
      locale.value = value
      for (const html of [await render(SyncText), await render(ActionsText), await render(KeyTermsText, { write: true }), await render(KeyTermsText, { write: true, send: true, team: 'Support' })]) {
        expect(html).not.toMatch(/<a\b/)
        expect(html, value).not.toMatch(/Happie|thehappie|Mailie/)
      }
    }
  })

  it('shows the server’s sections, and nothing to read or send mail with', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
    Object.assign(accounts, { list: [], loaded: true })
    const nav = (html: string) => (html.match(/<nav class="console-nav"[^]*?<\/nav>/)?.[0] ?? '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()
    // API keys are a workspace's: offered once the person's workspaces say which is shown.
    expect(nav(await render(OpenConsole))).toBe('Mailboxes 0 Storage Account')
    Object.assign(workspaces, { list: [{ id: 'wsp_000000000000aaaa', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }], loaded: true, supported: true, currentID: 'wsp_000000000000aaaa' })
    Object.assign(accounts, { list: [], loaded: true, workspace: 'wsp_000000000000aaaa' })
    const html = await render(OpenConsole)
    expect(nav(html)).toBe('Mailboxes 0 Members API keys &amp; MCP Storage Account')
    expect(html).not.toMatch(/Compose|mail-compose|Mail sent|by The Happie Co/)
    expect(html).not.toContain('lockup-by')
  })

  it('calls its frame the server’s console, in the person’s language', async () => {
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
    Object.assign(accounts, { list: [], loaded: true })
    const breadcrumb = (html: string) => (html.match(/<div class="console-breadcrumb"[^>]*>(.*?)<\/div>/)?.[1] ?? '').replace(/<[^>]+>/g, '').replace(/\s+/g, ' ')
    const html = await render(OpenConsole)
    expect(html).toMatch(/<aside class="console-sidebar" aria-label="Console navigation"/)
    expect(html).toMatch(/<span class="lockup"[^>]*role="img" aria-label="Mailie · Console"/)
    expect(breadcrumb(html)).toBe('Console / Mailboxes')
    locale.value = 'pt'
    const pt = await render(OpenConsole)
    expect(pt).toMatch(/<aside class="console-sidebar" aria-label="Navegação do console"/)
    expect(breadcrumb(pt)).toBe('Console / Caixas de email')
  })
})
