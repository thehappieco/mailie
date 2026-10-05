import { afterEach, describe, expect, it, vi } from 'vitest'
import { computed } from 'vue'
import { fileURLToPath } from 'node:url'
import { addCatalogs, initializeLocale, locale, setLocale, supportedLocale, t } from '../src/ui/i18n'
import { anyOf, count, since, stamp } from '../src/ui/format'
import { badTranslations, keysOf, managersGivingAccess, repeatedKeys, sourceFiles, usedKeys, type Catalogs } from './i18nGuard'

const catalogs: Catalogs = import.meta.glob<Record<string, string[]>>('../src/ui/locales/*.json', { eager: true, import: 'default' })

/** The core's sources and the open edition's: all of src/. Another edition has its own catalogs and runs the same guard. */
function coreSources(): string[] {
  return sourceFiles(fileURLToPath(new URL('../src', import.meta.url)))
}
afterEach(() => { locale.value = 'en'; vi.unstubAllGlobals() })

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
