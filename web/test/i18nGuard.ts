// The checks every project of the console runs over its translations: each
// literal t('…') in its sources is translated, in all four languages, with
// the same placeholders; each key lives in exactly one catalog; and none of
// the project's own catalogs keeps a key its sources no longer use. Not a
// spec itself: test/i18n.spec.ts runs it over the core and the open edition,
// and another edition's spec over its own sources, with the core's catalogs
// beneath its own.
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

export type Catalogs = Record<string, Record<string, string[]>>

/** The .ts and .vue files under dir, but its locales and the directories skipped. */
export function sourceFiles(dir: string, skip: string[] = []): string[] {
  const files: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) {
      if (entry.name !== 'locales' && !skip.includes(path)) files.push(...sourceFiles(path, skip))
    } else if (entry.isFile() && /\.(vue|ts)$/.test(entry.name)) files.push(path)
  }
  return files
}

/** Every key a source passes to t() as a literal. */
export function literalKeys(source: string): string[] {
  const keys: string[] = []
  for (const match of source.matchAll(/\bt\(\s*('(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*")/g)) {
    const literal = match[1]!
    keys.push(literal[0] === '"' ? JSON.parse(literal) : literal.slice(1, -1).replace(/\\'/g, "'").replace(/\\n/g, '\n').replace(/\\\\/g, '\\'))
  }
  return keys
}

/** The keys these files use. */
export function usedKeys(files: string[]): Set<string> {
  const used = new Set<string>()
  for (const file of files) for (const key of literalKeys(readFileSync(file, 'utf8'))) used.add(key)
  return used
}

const placeholders = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map(m => m[1]).sort()

/** Keys whose translations are not four, are blank, or lose or gain a placeholder. */
export function badTranslations(catalogs: Catalogs): string[] {
  const bad: string[] = []
  for (const catalog of Object.values(catalogs)) {
    for (const [source, translations] of Object.entries(catalog)) {
      const wrong = translations.length !== 4 || translations.some(value => !value.trim() || placeholders(value).join() !== placeholders(source).join())
      if (wrong) bad.push(source)
    }
  }
  return bad
}

/** Keys found in more than one catalog, with where. */
export function repeatedKeys(...sets: Catalogs[]): string[] {
  const seen = new Map<string, string>()
  const repeated: string[] = []
  for (const catalogs of sets) {
    for (const [file, catalog] of Object.entries(catalogs)) {
      for (const key of Object.keys(catalog)) {
        if (seen.has(key)) repeated.push(`${key} is in ${seen.get(key)} and ${file}`)
        else seen.set(key, file)
      }
    }
  }
  return repeated
}

/** Every key of these catalogs. */
export function keysOf(...sets: Catalogs[]): Set<string> {
  return new Set(sets.flatMap(catalogs => Object.values(catalogs).flatMap(catalog => Object.keys(catalog))))
}
