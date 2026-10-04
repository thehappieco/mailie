// What API keys and the MCP server look like to a person: the words for a
// key's access, lifetime and standing, where the MCP server is, and the
// command that connects Claude Code to it.

import type { Account, KeyLifetime, PersonalKey } from '../api/types'
import { t } from './i18n'

/** The most keys a person may hold that are neither revoked nor expired; the server refuses one more. */
export const MAX_LIVE_KEYS = 20

export const DEFAULT_LIFETIME: KeyLifetime = 90

/**
 * Where the MCP server answers, as a tool is given it: the daemon serves the
 * console, the API and /mcp on one origin. In development the Vite proxy
 * forwards /mcp too.
 */
export function mcpEndpoint(origin: string): string {
  return `${new URL(origin).origin}/mcp`
}

/**
 * The command that connects Claude Code to this server. The key is the
 * placeholder everywhere it is shown; only a key just created, copied at the
 * person's request, ever goes in its place, and then only to the clipboard.
 */
export function claudeCommand(endpoint: string, key: string): string {
  return `claude mcp add --transport http mailie ${endpoint} --header "Authorization: Bearer ${key}"`
}

/** The placeholder a shown command carries where the key goes. */
export function keyPlaceholder(): string {
  return `<${t('your key')}>`
}

export function scopeLabel(scope: string): string {
  switch (scope) {
    case 'read': return t('Read')
    case 'write': return t('Read and act')
    // A scope a newer daemon issues: its identifier says more than a guess would.
    default: return scope
  }
}

export function lifetimeLabel(days: KeyLifetime): string {
  switch (days) {
    case 30: return t('30 days')
    case 90: return t('90 days')
    case 365: return t('1 year')
  }
}

export type KeyStanding = 'live' | 'expired' | 'revoked'

/** A key works until it is revoked or reaches its expiry, whichever comes first. */
export function keyStanding(key: Pick<PersonalKey, 'revoked_at' | 'expires_at'>, now = Date.now()): KeyStanding {
  if (key.revoked_at) return 'revoked'
  return key.expires_at * 1000 <= now ? 'expired' : 'live'
}

export function standingLabel(standing: KeyStanding): string {
  switch (standing) {
    case 'live': return t('Active|key')
    case 'expired': return t('Expired')
    case 'revoked': return t('Revoked')
  }
}

/**
 * The mailboxes a key reaches, in words: every one of the person's, or the
 * ones it names, by address. A mailbox removed since has no address left,
 * and leaves the list: a key made for chosen mailboxes that are all gone
 * names none, and reached only those, never all of them.
 */
export function keyMailboxes(key: Pick<PersonalKey, 'account_ids' | 'restricted'>, accounts: readonly Pick<Account, 'id' | 'email'>[]): string[] {
  if (!key.account_ids?.length) return [key.restricted ? t('Only mailboxes removed since') : t('All your mailboxes')]
  return key.account_ids.map(id => accounts.find(account => account.id === id)?.email ?? t('A removed mailbox'))
}
