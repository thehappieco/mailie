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

/** Adds an edition's translations. A key the core already has keeps the core's: an edition only adds. */
export function addCatalogs(...more: Catalog[]): void {
  for (const added of more) {
    for (const [key, translations] of Object.entries(added)) {
      if (!Object.hasOwn(catalog, key)) catalog[key] = translations
    }
  }
}

export function supportedLocale(value: string | null | undefined): Locale | undefined {
  const base = value?.toLowerCase().split(/[-_]/)[0]
  return languageOptions.find((item) => item.value === base)?.value
}

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

export function setLocale(value: Locale): void {
  if (!supportedLocale(value)) return
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
