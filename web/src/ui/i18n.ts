import { ref } from 'vue'
import { readPreference, writePreference } from './preferences'

// The source text is English and is the key. Catalogs map it to the other four
// languages, {"<English>": [pt, es, fr, de]}, split by feature. Whatserver2's
// console moved new features to English source text; a new product has no
// legacy keys to keep, so here every string starts that way. These are the
// core's catalogs; an edition adds its own (addCatalogs, src/edition.ts).
const catalogs = import.meta.glob<Catalog>('./locales/*.json', { eager: true, import: 'default' })

export type Catalog = Record<string, string[]>

export type Locale = 'pt' | 'en' | 'es' | 'fr' | 'de'
export type Translations = Record<string, [string, string, string, string]>
export const languageOptions: { value: Locale; label: string }[] = [
  { value: 'pt', label: 'Português' }, { value: 'en', label: 'English' },
  { value: 'es', label: 'Español' }, { value: 'fr', label: 'Français' },
  { value: 'de', label: 'Deutsch' },
]
export const locale = ref<Locale>('en')
const columns: Record<Exclude<Locale, 'en'>, number> = { pt: 0, es: 1, fr: 2, de: 3 }
const catalog: Catalog = Object.assign({}, ...Object.values(catalogs))
/** The languages the edition offers (src/edition.ts), or null for every one of the core's. */
let offered: ReadonlySet<Locale> | null = null

/**
 * Narrows the languages offered to an edition's list, or back to all of the
 * core's with null. The list names only the core's languages and always
 * English, the source text and the last fallback; any other list is refused
 * and changes nothing. Every catalog keeps its four translations either way
 * (the catalogs' guard). configureEdition calls this, usually before
 * initializeLocale; called after it, a language shown that is no longer
 * offered gives way as initializeLocale chooses (the saved one, the
 * browser's, then English), saving nothing.
 */
export function setOfferedLocales(list: readonly Locale[] | null): void {
  if (list) {
    const unknown = list.filter(value => !languageOptions.some(option => option.value === value))
    if (unknown.length) throw new Error(`i18n: not among the core's languages: ${unknown.join(', ')}`)
    if (!list.includes('en')) throw new Error("i18n: the languages offered must include 'en', the source text")
  }
  offered = list ? new Set(list) : null
  if (offered && !offered.has(locale.value)) initializeLocale()
}

/** The languages a person may choose: the offered ones, in the core's order. */
export function offeredLanguages(): { value: Locale; label: string }[] {
  return languageOptions.filter(option => offered?.has(option.value) ?? true)
}

/** Adds an edition's translations. A key the core already has keeps the core's: an edition only adds. */
export function addCatalogs(...more: Catalog[]): void {
  for (const added of more) {
    for (const [key, translations] of Object.entries(added)) {
      if (!Object.hasOwn(catalog, key)) catalog[key] = translations
    }
  }
}

/** The offered language a preference or a browser language names (pt-BR is pt), if any. */
export function supportedLocale(value: string | null | undefined): Locale | undefined {
  const base = value?.toLowerCase().split(/[-_]/)[0]
  return offeredLanguages().find((item) => item.value === base)?.value
}

/** The saved language, then the browser's, then English: the first of them that is offered. */
export function initializeLocale(): void {
  const saved = supportedLocale(readPreference('locale'))
  const languages = typeof navigator === 'undefined' ? [] : navigator.languages ?? [navigator.language]
  const detected = languages.map(supportedLocale).find(Boolean)
  applyLocale(saved ?? detected ?? 'en')
}

function applyLocale(value: Locale): void {
  locale.value = value
  if (typeof document !== 'undefined') document.documentElement.lang = value === 'pt' ? 'pt-BR' : value
}

/** Shows and saves a language the person chose. One that is not offered changes and saves nothing. */
export function setLocale(value: Locale): void {
  if (supportedLocale(value) !== value) return
  applyLocale(value)
  writePreference('locale', value)
}

/**
 * The English a source shows. When the same English means two things (the
 * Archive folder, and archiving a message), the source carries a context
 * after a "|" (`Archive|verb`): it picks the translation and is never shown.
 */
function english(source: string): string {
  const bar = source.indexOf('|')
  return bar < 0 ? source : source.slice(0, bar)
}

/** Catalog values are text only. Vue escapes them; never insert translations as HTML. */
export function t(source: string, values?: Record<string, string | number | undefined>): string {
  const translated = locale.value === 'en' ? english(source) : catalog[source]?.[columns[locale.value]] ?? english(source)
  return values ? translated.replace(/\{(\w+)\}/g, (match, key: string) =>
    values[key] === undefined ? match : String(values[key])) : translated
}

export function intlLocale(): string { return locale.value === 'pt' ? 'pt-BR' : locale.value }
