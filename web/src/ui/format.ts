// Rendering helpers. Every timestamp the API sends is unix seconds.

import { intlLocale, t } from './i18n'

const formatters = new Map<string, Intl.DateTimeFormat>()
/** dateFormat is a date formatter in the page's language, made once for each set of options. */
export function dateFormat(options: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const key = intlLocale() + JSON.stringify(options)
  if (!formatters.has(key)) formatters.set(key, new Intl.DateTimeFormat(intlLocale(), options))
  return formatters.get(key)!
}
const full = () => dateFormat({ dateStyle: 'medium', timeStyle: 'short' })
const day = () => dateFormat({ dateStyle: 'long' })

function date(seconds: number | undefined): Date | undefined {
  return seconds && Number.isFinite(seconds) && seconds > 0 ? new Date(seconds * 1000) : undefined
}

/** stamp is a date and time to read, not glance at: the account sheet's facts. */
export function stamp(seconds: number | undefined): string {
  const at = date(seconds)
  return at ? full().format(at) : '—'
}

/** dayStamp is a date without a time, for things whose hour does not matter. */
export function dayStamp(seconds: number | undefined): string {
  const at = date(seconds)
  return at ? day().format(at) : '—'
}

/**
 * since says how long ago something was, in words.
 *
 * Coarse on purpose: "synced 3 min ago" is what somebody wants from a card,
 * and a timestamp to the second would have to be read rather than glanced at.
 */
export function since(seconds: number | undefined, now = Date.now()): string {
  const at = date(seconds)
  if (!at) return ''
  const elapsed = Math.max(0, Math.round((now - at.getTime()) / 1000))
  if (elapsed < 60) return t('just now')
  const relative = new Intl.RelativeTimeFormat(intlLocale(), { style: 'short', numeric: 'always' })
  const minutes = Math.round(elapsed / 60)
  if (minutes < 60) return relative.format(-minutes, 'minute')
  const hours = Math.round(minutes / 60)
  if (hours < 24) return relative.format(-hours, 'hour')
  const days = Math.round(hours / 24)
  if (days < 30) return relative.format(-days, 'day')
  return day().format(at)
}

/** ahead says how far away a moment is, in words: "in 2 min". Coarse, like since. */
export function ahead(seconds: number | undefined, now = Date.now()): string {
  const at = date(seconds)
  if (!at) return ''
  const left = Math.max(0, Math.round((at.getTime() - now) / 1000))
  const relative = new Intl.RelativeTimeFormat(intlLocale(), { style: 'short', numeric: 'always' })
  if (left < 60) return relative.format(Math.max(1, left), 'second')
  const minutes = Math.round(left / 60)
  if (minutes < 60) return relative.format(minutes, 'minute')
  return relative.format(Math.round(minutes / 60), 'hour')
}

/** countdown is m:ss until a unix-seconds deadline, never negative. */
export function countdown(deadline: number, now = Date.now()): string {
  const left = Math.max(0, Math.ceil(deadline - now / 1000))
  const m = Math.floor(left / 60)
  const s = left % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

const decimal = () => new Intl.NumberFormat(intlLocale())

export function count(n: number | undefined): string {
  return decimal().format(n ?? 0)
}

/** initials is what an avatar shows when there is no picture: two letters at most. */
export function initials(name: string): string {
  const words = name.replace(/@.*$/, '').split(/[\s._-]+/).filter(Boolean)
  const letters = words.length > 1 ? [words[0]!, words[words.length - 1]!] : words.slice(0, 1)
  const result = letters.map(word => [...word][0] ?? '').join('').toLocaleUpperCase(intlLocale())
  return result || '?'
}

const units = ['byte', 'kilobyte', 'megabyte', 'gigabyte'] as const

/** fileSize is a size in bytes as a person reads one: 820 bytes, 12 kB, 3.4 MB. */
export function fileSize(bytes: number | undefined): string {
  let value = Math.max(0, bytes ?? 0)
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) { value /= 1000; unit++ }
  return new Intl.NumberFormat(intlLocale(), {
    style: 'unit', unit: units[unit], unitDisplay: unit === 0 ? 'long' : 'short',
    maximumFractionDigits: unit === 0 || value >= 10 ? 0 : 1,
  }).format(value)
}
