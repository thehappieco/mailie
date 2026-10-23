import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { configureEdition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import { initializeLocale, locale, setLocale } from '../src/ui/i18n'
import { hostPreferences, initializeTheme, readPreference, setPreferenceStore, setTheme, theme, writePreference, type PreferenceName, type PreferenceStore } from '../src/ui/preferences'

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
afterEach(() => { cleanup?.(); cleanup = undefined; configureEdition(openEdition); locale.value = 'en'; vi.unstubAllGlobals() })

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
  it('keeps preferences to this host alone, in a cookie and local storage, whatever its domain', () => {
    writePreference('theme', 'dark'); expect(writes[0]).toBe('mailie_theme=dark; Path=/; Max-Age=31536000; SameSite=Lax; Secure'); expect(saved.get('mailie_theme')).toBe('dark')
    for (const hostname of ['console.product.example', 'product.example', 'mail.example.org']) {
      vi.stubGlobal('location', { hostname, protocol: 'https:' }); writePreference('locale', 'de')
      expect(writes.at(-1), hostname).toBe('mailie_locale=de; Path=/; Max-Age=31536000; SameSite=Lax; Secure')
    }
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

describe('a store an edition installs', () => {
  /** A store that keeps its values in memory, as an edition's own might, the language as a BCP 47 tag. */
  function memoryStore(values: Partial<Record<PreferenceName, string>> = {}) {
    const kept = new Map(Object.entries(values))
    const written: [PreferenceName, string][] = []
    const store: PreferenceStore = {
      read: name => kept.get(name) ?? null,
      write: (name, value) => { written.push([name, value]); kept.set(name, name === 'locale' && value === 'pt' ? 'pt-BR' : value) },
    }
    return { store, kept, written }
  }

  it('is the only place the theme and the language are read from and written to, and the core’s own comes back without one', () => {
    cookies.set('mailie_theme', 'light'); cookies.set('mailie_locale', 'fr'); saved.set('mailie_theme', 'light')
    vi.stubGlobal('navigator', { languages: ['de-DE'] })
    const { store, kept, written } = memoryStore({ theme: 'dark', locale: 'pt-BR' })
    configureEdition({ ...openEdition, preferences: store })
    cleanup = initializeTheme(); initializeLocale()
    expect([theme.value, doc.documentElement.dataset.theme, locale.value]).toEqual(['dark', 'dark', 'pt'])
    setTheme('system'); setLocale('es'); setLocale('pt')
    expect(written).toEqual([['theme', 'system'], ['locale', 'es'], ['locale', 'pt']])
    expect(kept.get('locale')).toBe('pt-BR')
    // Nothing reached the core's own cookie or storage.
    expect(writes).toEqual([]); expect([cookies.get('mailie_theme'), saved.get('mailie_theme')]).toEqual(['light', 'light'])
    // Restoring the theme asks the store again.
    kept.set('theme', 'light'); doc.dispatchEvent(new Event('visibilitychange')); expect(theme.value).toBe('light')
    // The edition set again without one: the core's own, as ever.
    configureEdition(openEdition)
    expect(readPreference('theme')).toBe('light'); expect(readPreference('locale')).toBe('fr')
    writePreference('theme', 'dark'); expect(writes).toEqual(['mailie_theme=dark; Path=/; Max-Age=31536000; SameSite=Lax; Secure'])
    expect(setPreferenceStore(null)).toBe(hostPreferences)
  })

  it('is checked as the core’s own is: an unknown value is none, and a store that throws only forgets', () => {
    vi.stubGlobal('navigator', { languages: ['es-MX'] })
    configureEdition({ ...openEdition, preferences: memoryStore({ theme: 'sepia', locale: 'xx' }).store })
    cleanup = initializeTheme(); initializeLocale()
    expect([theme.value, locale.value]).toEqual(['system', 'es'])
    const refusing: PreferenceStore = { read: () => { throw Error('denied') }, write: () => { throw Error('denied') } }
    configureEdition({ ...openEdition, preferences: refusing })
    expect(() => { initializeLocale(); setTheme('dark'); setLocale('pt') }).not.toThrow()
    expect([theme.value, doc.documentElement.dataset.theme, locale.value]).toEqual(['dark', 'dark', 'pt'])
    expect(writes).toEqual([])
  })

  it('is in place before a language the edition no longer offers gives way, and an edition refused leaves the one before', () => {
    vi.stubGlobal('navigator', { languages: ['de-DE'] })
    locale.value = 'fr'
    configureEdition({ ...openEdition, locales: ['pt', 'en', 'es'], preferences: memoryStore({ locale: 'es' }).store })
    expect(locale.value).toBe('es')
    const { store } = memoryStore({ theme: 'dark' })
    expect(() => configureEdition({ ...openEdition, locales: ['pt'], preferences: store })).toThrow(/must include 'en'/)
    expect(readPreference('theme')).toBeNull()
    expect(readPreference('locale')).toBe('es')
  })
})
