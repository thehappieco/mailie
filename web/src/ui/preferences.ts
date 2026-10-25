import { ref } from 'vue'

export type ThemePreference = 'light' | 'dark' | 'system'
/** The two preferences of the page's look: its theme and its language. */
export type PreferenceName = 'theme' | 'locale'

/**
 * Where the theme and the language a person chose are kept (Edition.preferences,
 * src/edition.ts): read before the first frame and whenever the page restores
 * them (initializeTheme, initializeLocale), written at every choice (setTheme,
 * setLocale). The theme is 'light', 'dark' or 'system'; the language is one of
 * the core's (ui/i18n.ts: 'pt', 'en', 'es', 'fr' or 'de'), and a store may answer
 * any tag supportedLocale reads ('pt-BR' is 'pt'). The core checks whatever a
 * store answers, as it checks its own: a value it does not know is none. A store
 * that cannot read answers null, and one that cannot write keeps nothing; one
 * that throws is treated the same.
 */
export interface PreferenceStore {
  read(name: PreferenceName): string | null
  write(name: PreferenceName, value: string): void
}

const prefix = 'mailie_'

/**
 * The core's own store, every edition's unless it installs another: a cookie of
 * this host alone (mailie_theme, mailie_locale), read first, and this origin's
 * local storage, for a browser that refuses cookies. Non-sensitive, and never
 * shared with another host.
 */
export const hostPreferences: PreferenceStore = {
  read(name) {
    try {
      const entry = document.cookie.split(';').map(part => part.trim()).find(part => part.startsWith(`${prefix}${name}=`))
      if (entry) return decodeURIComponent(entry.slice(entry.indexOf('=') + 1))
    } catch { /* Disabled cookies still allow a local preference. */ }
    try { return localStorage.getItem(`${prefix}${name}`) } catch { return null }
  },
  write(name, value) {
    try { localStorage.setItem(`${prefix}${name}`, value) } catch { /* Private browsing can deny storage. */ }
    try {
      document.cookie = `${prefix}${name}=${encodeURIComponent(value)}; Path=/; Max-Age=31536000; SameSite=Lax${location.protocol === 'https:' ? '; Secure' : ''}`
    } catch { /* A preference is still usable for the current page. */ }
  },
}

let store: PreferenceStore = hostPreferences

/**
 * Makes value the only place the theme and the language are read from and
 * written to, or the core's own again with null, and answers the one it
 * replaces. configureEdition calls this with the edition's
 * (Edition.preferences), before the page chooses either.
 */
export function setPreferenceStore(value: PreferenceStore | null): PreferenceStore {
  const before = store
  store = value ?? hostPreferences
  return before
}

/** The value the store keeps for name, or null. */
export function readPreference(name: PreferenceName): string | null {
  try { return store.read(name) } catch { return null }
}

/** Keeps value for name in the store; one it cannot keep lasts this page. */
export function writePreference(name: PreferenceName, value: string): void {
  try { store.write(name, value) } catch { /* The page still shows it. */ }
}

/**
 * Preferences of one person on this server alone: kept in this host's local
 * storage, never in a cookie, so neither another host of the edition's domain
 * nor any request carries them, under a key that names the person by their
 * opaque user id (mailie_workspace:usr_…), so two people who share a browser
 * never open on each other's choice. Non-sensitive, like the others: the last
 * workspace shown, an identifier the server checks on every read. Without a
 * person (nobody signed in) nothing is read or kept.
 */
type PersonPreferenceName = 'workspace'

const personKey = (name: PersonPreferenceName, person: string) => `${prefix}${name}:${person}`

export function readPersonPreference(name: PersonPreferenceName, person: string): string | null {
  if (!person) return null
  try { return localStorage.getItem(personKey(name, person)) } catch { return null }
}

export function writePersonPreference(name: PersonPreferenceName, person: string, value: string): void {
  if (!person) return
  try {
    if (value) localStorage.setItem(personKey(name, person), value)
    else localStorage.removeItem(personKey(name, person))
  } catch { /* Private browsing can deny storage; the page still keeps it in memory. */ }
}

export const theme = ref<ThemePreference>('system')
let media: MediaQueryList | null = null
let stopListening: (() => void) | null = null

function isTheme(value: string | null): value is ThemePreference {
  return value === 'light' || value === 'dark' || value === 'system'
}

function applyTheme(): void {
  if (typeof document === 'undefined') return
  document.documentElement.dataset.theme = theme.value === 'system' ? (media?.matches ? 'dark' : 'light') : theme.value
}

export function setTheme(value: ThemePreference): void {
  if (!isTheme(value)) return
  theme.value = value
  writePreference('theme', value)
  applyTheme()
}

/**
 * How the page shows a theme it reads again from the store while it runs
 * (the tab shown again, another tab's choice): it calls apply, which shows
 * next, exactly once, when and how it chooses (an edition may fade the page
 * into it). An edition's entry gives one to initializeTheme; without one the
 * theme read again shows at once. It is never asked for the theme read
 * before the first frame, nor when the system's scheme changes under a theme
 * that follows it.
 */
export type ThemeReread = (next: ThemePreference, apply: () => void) => void

/**
 * Call before mounting the application, so the first frame has the chosen
 * palette: it shows the store's theme at once, then reads it again whenever
 * the tab is shown again (and, for the core's own store, when another tab
 * saves one), showing that through reread when given.
 */
export function initializeTheme(options: { reread?: ThemeReread } = {}): () => void {
  stopListening?.()
  media = typeof window !== 'undefined' && typeof window.matchMedia === 'function'
    ? window.matchMedia('(prefers-color-scheme: dark)') : null
  const stored = (): ThemePreference => {
    const saved = readPreference('theme')
    return isTheme(saved) ? saved : 'system'
  }
  const show = (next: ThemePreference) => {
    theme.value = next
    applyTheme()
  }
  const restore = () => {
    const next = stored()
    if (options.reread) options.reread(next, () => show(next))
    else show(next)
  }
  const onStorage = (event: StorageEvent) => {
    if (event.key === `${prefix}theme` || event.key === null) restore()
  }
  const onVisibility = () => {
    if (document.visibilityState === 'visible') restore()
  }
  show(stored())
  media?.addEventListener('change', applyTheme)
  window.addEventListener('storage', onStorage)
  document.addEventListener('visibilitychange', onVisibility)
  const currentMedia = media
  const cleanup = () => {
    currentMedia?.removeEventListener('change', applyTheme)
    window.removeEventListener('storage', onStorage)
    document.removeEventListener('visibilitychange', onVisibility)
    if (stopListening === cleanup) stopListening = null
  }
  stopListening = cleanup
  return cleanup
}
