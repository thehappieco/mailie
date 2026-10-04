import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { initializeTheme, readPreference, setSharedDomain, setTheme, theme, writePreference } from '../src/ui/preferences'

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
afterEach(() => { cleanup?.(); cleanup = undefined; setSharedDomain(''); vi.unstubAllGlobals() })

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
