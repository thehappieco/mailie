import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { initializeTheme, readPreference, setSharedDomain, setTheme, showThemeForPage, theme, writePreference, type ThemePreference } from '../src/ui/preferences'

let cleanup: (() => void) | undefined
let cookies: Map<string, string>
let saved: Map<string, string>
let writes: string[]
let media: EventTarget & { matches: boolean }
let doc: EventTarget & { documentElement: { dataset: Record<string, string> }; visibilityState: string; cookie?: string }
let win: EventTarget
beforeEach(() => {
  cookies = new Map(); saved = new Map(); writes = []
  media = Object.assign(new EventTarget(), { matches: true })
  doc = Object.assign(new EventTarget(), { documentElement: { dataset: {} }, visibilityState: 'visible' })
  Object.defineProperty(doc, 'cookie', { configurable: true, get: () => [...cookies].map(([k, v]) => `${k}=${v}`).join('; '), set: (v: string) => { writes.push(v); const [k, value] = v.split(';')[0]!.split('='); cookies.set(k!, value!) } })
  win = Object.assign(new EventTarget(), { matchMedia: () => media })
  vi.stubGlobal('document', doc); vi.stubGlobal('window', win)
  vi.stubGlobal('location', { hostname: 'mail.example.org', protocol: 'https:' })
  vi.stubGlobal('localStorage', { getItem: (key: string) => saved.get(key) ?? null, setItem: (key: string, value: string) => { saved.set(key, value) } })
})
afterEach(() => { cleanup?.(); cleanup = undefined; showThemeForPage(null); setSharedDomain(''); vi.unstubAllGlobals() })

describe('appearance preferences', () => {
  it('follows the system at startup and when it changes', () => {
    cleanup = initializeTheme(); expect(theme.value).toBe('system'); expect(doc.documentElement.dataset.theme).toBe('dark')
    media.matches = false; media.dispatchEvent(new Event('change')); expect(doc.documentElement.dataset.theme).toBe('light')
  })
  it('keeps an explicit appearance despite a conflicting system setting', () => {
    cookies.set('mailie_theme', 'light'); cleanup = initializeTheme(); media.dispatchEvent(new Event('change'))
    expect(theme.value).toBe('light'); expect(doc.documentElement.dataset.theme).toBe('light')
    setTheme('dark'); media.matches = false; media.dispatchEvent(new Event('change')); expect(doc.documentElement.dataset.theme).toBe('dark')
  })
  it('keeps preferences to this host unless the edition names a domain to share them with', () => {
    // The open edition names none.
    writePreference('theme', 'dark'); expect(writes[0]).toBe('mailie_theme=dark; Path=/; Max-Age=31536000; SameSite=Lax; Secure'); expect(saved.get('mailie_theme')).toBe('dark')
    setSharedDomain('Product.Example')
    vi.stubGlobal('location', { hostname: 'console.product.example', protocol: 'https:' }); writePreference('locale', 'de'); expect(writes[1]).toContain('SameSite=Lax; Domain=product.example; Secure')
    vi.stubGlobal('location', { hostname: 'notproduct.example', protocol: 'https:' }); writePreference('locale', 'de'); expect(writes[2]).not.toContain('Domain=')
    vi.stubGlobal('location', { hostname: 'other.example', protocol: 'https:' }); writePreference('locale', 'de'); expect(writes[3]).not.toContain('Domain=')
  })
  it('works on localhost without a Secure or shared-domain cookie', () => {
    vi.stubGlobal('location', { hostname: 'localhost', protocol: 'http:' }); writePreference('locale', 'en')
    expect(writes[0]).not.toContain('Domain='); expect(writes[0]).not.toContain('Secure'); expect(readPreference('locale')).toBe('en')
  })
  it('prefers the shared cookie over stale local storage and refreshes when the tab is shown again', () => {
    saved.set('mailie_locale', 'pt'); cookies.set('mailie_locale', 'fr'); expect(readPreference('locale')).toBe('fr')
    cleanup = initializeTheme(); cookies.set('mailie_theme', 'light'); doc.dispatchEvent(new Event('visibilitychange')); expect(theme.value).toBe('light')
  })
  it('follows another tab that changes the preference', () => {
    cleanup = initializeTheme(); saved.set('mailie_theme', 'light'); win.dispatchEvent(Object.assign(new Event('storage'), { key: 'mailie_theme' })); expect(theme.value).toBe('light')
  })
  it('keeps working when every kind of storage is denied', () => {
    Object.defineProperty(doc, 'cookie', { get: () => { throw Error('denied') }, set: () => { throw Error('denied') } })
    vi.stubGlobal('localStorage', { getItem: () => { throw Error('denied') }, setItem: () => { throw Error('denied') } })
    cleanup = initializeTheme(); expect(() => setTheme('light')).not.toThrow(); expect(doc.documentElement.dataset.theme).toBe('light')
  })
  it('ignores malformed or unknown saved choices', () => {
    cookies.set('mailie_theme', '%XX'); cleanup = initializeTheme(); expect(theme.value).toBe('system')
    cookies.set('mailie_theme', 'sepia'); doc.dispatchEvent(new Event('visibilitychange')); expect(theme.value).toBe('system')
  })
  it('removes its listeners on cleanup', () => {
    cleanup = initializeTheme(); cleanup(); media.matches = false; media.dispatchEvent(new Event('change')); expect(doc.documentElement.dataset.theme).toBe('dark')
  })
})

describe('a theme shown for the page alone', () => {
  const restored = () => {
    win.dispatchEvent(Object.assign(new Event('storage'), { key: 'mailie_theme' }))
    doc.dispatchEvent(new Event('visibilitychange'))
  }

  it('is shown over the one saved and saves nothing, lasting through another tab’s change and the tab shown again, until the person chooses one', () => {
    cookies.set('mailie_theme', 'light'); cleanup = initializeTheme()
    showThemeForPage('dark')
    expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['dark', 'dark'])
    expect(writes).toEqual([]); expect(saved.size).toBe(0)
    // Another tab saves a theme, and the tab is shown again: the page keeps its own.
    saved.set('mailie_theme', 'light'); restored()
    expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['dark', 'dark'])
    // System follows the system's, as a saved system does.
    showThemeForPage('system'); expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['system', 'dark'])
    media.matches = false; media.dispatchEvent(new Event('change')); expect(doc.documentElement.dataset.theme).toBe('light')
    // A choice ends it, saved as ever, and what is restored afterwards is what is saved.
    setTheme('dark'); expect([theme.value, saved.get('mailie_theme'), cookies.get('mailie_theme')]).toEqual(['dark', 'dark', 'dark'])
    cookies.set('mailie_theme', 'light'); restored(); expect(theme.value).toBe('light')
  })

  it('gives way to the one saved when ended, and an end with none shown changes nothing', () => {
    cookies.set('mailie_theme', 'light'); cleanup = initializeTheme()
    showThemeForPage('dark'); showThemeForPage(null)
    expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['light', 'light'])
    // Nothing shown: an end leaves even a choice the browser would not keep as it is.
    Object.defineProperty(doc, 'cookie', { get: () => { throw Error('denied') }, set: () => { throw Error('denied') } })
    vi.stubGlobal('localStorage', { getItem: () => { throw Error('denied') }, setItem: () => { throw Error('denied') } })
    setTheme('dark'); showThemeForPage(null)
    expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['dark', 'dark'])
  })

  it('ignores an unknown theme', () => {
    cookies.set('mailie_theme', 'light'); cleanup = initializeTheme()
    for (const unknown of ['sepia', '', 'Dark', '__proto__']) showThemeForPage(unknown as ThemePreference)
    expect([theme.value, doc.documentElement.dataset.theme]).toEqual(['light', 'light'])
    // Still none shown: what is restored is what is saved.
    cookies.set('mailie_theme', 'dark'); restored(); expect(theme.value).toBe('dark')
  })
})
