import { afterEach, describe, expect, it, vi } from 'vitest'
import { computed, createSSRApp, h } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import { fileURLToPath } from 'node:url'
import AppearanceMenu from '../src/components/AppearanceMenu.vue'
import { configureEdition, edition, type Edition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import { addCatalogs, initializeLocale, languageOptions, locale, offeredLanguages, setLocale, showLocaleForPage, supportedLocale, t, type Locale } from '../src/ui/i18n'
import { anyOf, count, since, stamp } from '../src/ui/format'
import { badTranslations, keysOf, managersGivingAccess, repeatedKeys, sourceFiles, usedKeys, type Catalogs } from './i18nGuard'
import { memoryStorage } from './support'

const catalogs: Catalogs = import.meta.glob<Record<string, string[]>>('../src/ui/locales/*.json', { eager: true, import: 'default' })

/** The core's sources and the open edition's: all of src/. Another edition has its own catalogs and runs the same guard. */
function coreSources(): string[] {
  return sourceFiles(fileURLToPath(new URL('../src', import.meta.url)))
}
afterEach(() => { configureEdition(openEdition); showLocaleForPage(null); locale.value = 'en'; vi.unstubAllGlobals() })

describe('application languages', () => {
  it('recognizes regional language preferences and rejects unsupported values', () => {
    expect(supportedLocale('pt-BR')).toBe('pt')
    expect(supportedLocale('es-MX')).toBe('es')
    expect(supportedLocale('zh')).toBeUndefined()
    expect(supportedLocale('__proto__')).toBeUndefined()
  })

  it('uses a saved preference, then the browser languages, then English', () => {
    vi.stubGlobal('navigator', { languages: ['fr-CA', 'en'] })
    vi.stubGlobal('document', { cookie: 'mailie_locale=de', documentElement: { lang: '' } })
    initializeLocale()
    expect(locale.value).toBe('de')
    expect(document.documentElement.lang).toBe('de')
    vi.stubGlobal('document', { cookie: '', documentElement: { lang: '' } })
    vi.stubGlobal('localStorage', { getItem() { throw new Error('disabled') }, setItem() { throw new Error('disabled') } })
    initializeLocale()
    expect(locale.value).toBe('fr')
    vi.stubGlobal('navigator', { languages: ['zh-CN', 'ja'] })
    initializeLocale()
    expect(locale.value).toBe('en')
    vi.stubGlobal('navigator', { languages: ['pt-BR'] })
    initializeLocale()
    expect(document.documentElement.lang).toBe('pt-BR')
    setLocale('es')
    expect(locale.value).toBe('es')
  })

  it('reactively changes labels and locale-specific dates and numbers', () => {
    const label = computed(() => t('Email accounts'))
    const number = computed(() => count(1234))
    expect(label.value).toBe('Email accounts')
    expect(number.value).toBe('1,234')
    locale.value = 'pt'
    expect(label.value).toBe('Contas de email')
    expect(number.value).toBe('1.234')
    locale.value = 'de'
    expect(label.value).toBe('E-Mail-Konten')
    expect(stamp(Date.UTC(2026, 8, 8, 12) / 1000)).toContain('2026')
    expect(since(Math.floor(Date.now() / 1000) - 180)).toContain('3')
  })

  it('joins alternatives with the page language’s own “or”', () => {
    expect(anyOf(['Read', 'Act', 'Send'])).toBe('Read, Act, or Send')
    expect(anyOf(['Act'])).toBe('Act')
    locale.value = 'pt'
    expect(anyOf(['Leitura', 'Ações', 'Envio'])).toBe('Leitura, Ações ou Envio')
    locale.value = 'de'
    expect(anyOf(['Lesen', 'Senden'])).toBe('Lesen oder Senden')
  })

  it('interpolates values as plain text and leaves unknown text alone', () => {
    locale.value = 'pt'
    const value = '<img src=x onerror=alert(1)>'
    expect(t('{email} was removed from Mailie.', { email: value })).toBe(`${value} foi removida do Mailie.`)
    expect(t('A string nobody translated')).toBe('A string nobody translated')
  })

  it('tells apart the same English with a context the screen never shows', () => {
    // A key is Active|key, an account Active: French says the two differently.
    expect(t('Active|key')).toBe('Active')
    expect(t('Active')).toBe('Active')
    locale.value = 'fr'
    expect(t('Active|key')).toBe('Active')
    expect(t('Active')).toBe('Actif')
    expect(t('Nobody translated this|context')).toBe('Nobody translated this')
  })

  it('provides all four translations and exactly preserves every interpolation placeholder', () => {
    expect(badTranslations(catalogs)).toEqual([])
  })

  it('defines every key in exactly one catalog', () => {
    expect(repeatedKeys(catalogs)).toEqual([])
  })

  it('translates every literal key the core uses, and keeps no unused one', () => {
    const used = usedKeys(coreSources())
    const all = keysOf(catalogs)
    expect([...used].filter(key => !all.has(key))).toEqual([])
    expect([...all].filter(key => !used.has(key))).toEqual([])
  })

  it('never says that managing a mailbox is enough to give access to it', () => {
    expect(managersGivingAccess({ old: { 'Whoever manages it can give you access.': [], 'Joining gives no access until someone who manages one gives it.': [] } })).toHaveLength(2)
    expect(managersGivingAccess(catalogs)).toEqual([])
  })

  it('lets an edition add translations without changing the core’s', () => {
    locale.value = 'pt'
    addCatalogs({ 'A sentence only an edition says': ['Uma frase que só uma edição diz', 'x', 'x', 'x'], 'Email accounts': ['Outra coisa', 'x', 'x', 'x'] })
    expect(t('A sentence only an edition says')).toBe('Uma frase que só uma edição diz')
    expect(t('Email accounts')).toBe('Contas de email')
  })
})

describe('the languages an edition offers', () => {
  const three: Edition = { ...openEdition, locales: ['pt', 'en', 'es'] }

  /** The language menu's options, as the appearance dialog lists them. */
  async function menuOptions(): Promise<string[]> {
    const context: SSRContext = {}
    const html = await renderToString(createSSRApp({ render: () => h(AppearanceMenu) }), context)
    return [...(html + Object.values(context.teleports ?? {}).join('')).matchAll(/<option value="(\w+)"/g)].map(match => match[1]!)
  }

  /** A page whose preferences are kept in memory: what setLocale writes, and nothing else, lands here. */
  function stubPreferences(cookie = '') {
    const storage = memoryStorage()
    const page = { cookie, documentElement: { lang: '' } }
    vi.stubGlobal('localStorage', storage)
    vi.stubGlobal('document', page)
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    return { storage, page }
  }

  it('are all of the core’s when the edition names none, in the core’s order', async () => {
    expect(offeredLanguages()).toEqual(languageOptions)
    expect(offeredLanguages().map(option => option.value)).toEqual(['pt', 'en', 'es', 'fr', 'de'])
    expect(await menuOptions()).toEqual(['pt', 'en', 'es', 'fr', 'de'])
  })

  it('are only the edition’s, in the core’s order, in the menu and in what a preference or a browser may name', async () => {
    configureEdition({ ...openEdition, locales: ['es', 'en', 'pt'] })
    expect(offeredLanguages().map(option => option.value)).toEqual(['pt', 'en', 'es'])
    expect(await menuOptions()).toEqual(['pt', 'en', 'es'])
    expect(supportedLocale('es-MX')).toBe('es')
    expect(supportedLocale('fr')).toBeUndefined()
    expect(supportedLocale('de-DE')).toBeUndefined()
    // The page still carries the French and German translations: an edition only narrows what is offered.
    locale.value = 'fr'
    expect(t('Email accounts')).toBe('Comptes e-mail')
    locale.value = 'de'
    expect(t('Email accounts')).toBe('E-Mail-Konten')
  })

  it('move a page already showing one the edition, set again, no longer offers to the next, saving nothing', () => {
    const { storage, page } = stubPreferences('mailie_locale=fr')
    vi.stubGlobal('navigator', { languages: ['de-DE', 'es'] })
    initializeLocale()
    expect(locale.value).toBe('fr')
    configureEdition(three)
    expect(locale.value).toBe('es')
    expect(page.documentElement.lang).toBe('es')
    expect(storage.length).toBe(0)
    expect(page.cookie).toBe('mailie_locale=fr')
    // One still offered stays, whichever edition comes next.
    setLocale('pt')
    for (const next of [three, { ...three, byline: 'again' }, openEdition]) {
      configureEdition(next)
      expect(locale.value).toBe('pt')
    }
  })

  it('pass over a saved language the edition does not offer for the browser’s, then English', () => {
    configureEdition(three)
    stubPreferences('mailie_locale=fr')
    vi.stubGlobal('navigator', { languages: ['de-DE', 'es'] })
    initializeLocale()
    expect(locale.value).toBe('es')
    expect(document.documentElement.lang).toBe('es')
    vi.stubGlobal('navigator', { languages: ['de-DE', 'fr-CA'] })
    initializeLocale()
    expect(locale.value).toBe('en')
    // An offered one saved is kept, as before.
    stubPreferences('mailie_locale=pt')
    initializeLocale()
    expect(locale.value).toBe('pt')
  })

  it('are the only ones a person may choose: another changes and saves nothing', () => {
    configureEdition(three)
    const { storage, page } = stubPreferences()
    locale.value = 'pt'
    for (const refused of ['de', 'fr', 'pt-BR', '__proto__'] as Locale[]) {
      setLocale(refused)
      expect(locale.value).toBe('pt')
      expect(storage.length).toBe(0)
      expect(page.cookie).toBe('')
      expect(page.documentElement.lang).toBe('')
    }
    setLocale('es')
    expect(locale.value).toBe('es')
    expect(storage.getItem('mailie_locale')).toBe('es')
    expect(page.cookie).toMatch(/^mailie_locale=es;/)
  })

  it('always include English, among the core’s languages: any other list is refused, and the edition before stays', () => {
    configureEdition(three)
    for (const locales of [['pt', 'es'], [], ['en', 'it'], ['en', 'pt-BR']] as Locale[][]) {
      expect(() => configureEdition({ ...openEdition, byline: 'refused', locales })).toThrow(locales.includes('en') ? /not among the core's languages/ : /must include 'en'/)
      expect(edition()).toBe(three)
      expect(offeredLanguages().map(option => option.value)).toEqual(['pt', 'en', 'es'])
    }
    configureEdition(openEdition)
    expect(offeredLanguages()).toHaveLength(5)
  })
})

describe('a language shown for the page alone', () => {
  const three: Edition = { ...openEdition, locales: ['pt', 'en', 'es'] }

  /** A page whose preferences are kept in memory, with a browser in German. */
  function stubPreferences(cookie = '') {
    const storage = memoryStorage()
    const page = { cookie, documentElement: { lang: '' } }
    vi.stubGlobal('localStorage', storage)
    vi.stubGlobal('document', page)
    vi.stubGlobal('location', new URL('https://mail.example.org/'))
    vi.stubGlobal('navigator', { languages: ['de-DE'] })
    return { storage, page }
  }

  it('is shown over the one saved and saves nothing, lasting through the page’s own choosing again, until the person chooses one', () => {
    const { storage, page } = stubPreferences('mailie_locale=fr')
    initializeLocale()
    showLocaleForPage('es')
    expect([locale.value, page.documentElement.lang]).toEqual(['es', 'es'])
    expect(t('Email accounts')).toBe('Cuentas de correo')
    expect(storage.length).toBe(0)
    expect(page.cookie).toBe('mailie_locale=fr')
    // The page choosing again, or its edition set again, keeps it.
    initializeLocale()
    configureEdition({ ...openEdition, byline: 'again' })
    expect(locale.value).toBe('es')
    // A choice ends it, and is saved as ever.
    setLocale('pt')
    expect([locale.value, storage.getItem('mailie_locale')]).toEqual(['pt', 'pt'])
    expect(page.documentElement.lang).toBe('pt-BR')
    initializeLocale()
    expect(locale.value).toBe('pt')
  })

  it('gives way to what the page would choose when ended, and an end with none shown changes nothing', () => {
    stubPreferences('mailie_locale=fr')
    initializeLocale()
    showLocaleForPage('pt')
    expect(locale.value).toBe('pt')
    showLocaleForPage(null)
    expect(locale.value).toBe('fr')
    // Nothing shown: an end leaves even a language set without saving as it is.
    locale.value = 'es'
    showLocaleForPage(null)
    expect(locale.value).toBe('es')
  })

  it('is only one the edition offers: another changes and saves nothing, and one it stops offering gives way', () => {
    configureEdition(three)
    const { storage, page } = stubPreferences('mailie_locale=pt')
    initializeLocale()
    for (const refused of ['de', 'fr', 'pt-BR', '__proto__'] as Locale[]) {
      showLocaleForPage(refused)
      expect(locale.value, refused).toBe('pt')
    }
    configureEdition(openEdition)
    showLocaleForPage('fr')
    expect(locale.value).toBe('fr')
    configureEdition(three)
    expect(locale.value).toBe('pt')
    // Gone for good: the next edition that offers French again shows the saved language.
    configureEdition(openEdition)
    initializeLocale()
    expect(locale.value).toBe('pt')
    expect(storage.length).toBe(0)
    expect(page.cookie).toBe('mailie_locale=pt')
  })
})
