// What API keys and the MCP server look like to a person: the words for a
// key's scope, lifetime, standing and what it holds on each mailbox; what an
// owner or an admin may give a key there, said before anyone tries
// (docs/workspaces.md, "API keys"); where the MCP server is, and the command
// that connects Claude Code to it. These only decide what is offered and how
// it is said: the server decides, in the transaction that would make the
// change, and refuses whatever these get wrong. Pure functions, so each rule
// is a test.

import type { KeyFlags, KeyLifetime, KeySend, WorkspaceKey } from '../api/types'
import { count } from './format'
import { t } from './i18n'

/** The most keys a workspace may hold that are neither revoked nor expired; the server refuses one more. */
export const MAX_LIVE_KEYS = 20

export const DEFAULT_LIFETIME: KeyLifetime = 90

export const keyFlagNames = ['read', 'act', 'send'] as const
export type KeyFlag = typeof keyFlagNames[number]

export const NO_KEY_ACCESS: KeyFlags = { read: false, act: false, send: false }

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
    case 'send': return t('Read, act and send')
    // A scope a newer daemon issues: its identifier says more than a guess would.
    default: return scope
  }
}

/** Whether a scope lets a key act: write, or send, which covers it. */
export function scopeActs(scope: string): boolean {
  return scope === 'write' || scope === 'send'
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
export function keyStanding(key: Pick<WorkspaceKey, 'revoked_at' | 'expires_at'>, now = Date.now()): KeyStanding {
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

export function keyFlagLabel(flag: KeyFlag): string {
  switch (flag) {
    case 'read': return t('Read')
    case 'act': return t('Act')
    // The access, not the button that sends.
    case 'send': return t('Send|access')
  }
}

/** What a key may do on a mailbox where it holds a flag, in a sentence. */
export function keyFlagHint(flag: KeyFlag): string {
  switch (flag) {
    case 'read': return t('Search and read its messages, and its folders. Given only by someone who reads the mailbox.')
    case 'act': return t('Mark, star, archive and move its messages. Needs Read, and a key that can act.')
    case 'send': return t('Send from it, under its address alone, each message confirmed by the tool. Needs a key that can send.')
  }
}

/** What a key holds on a mailbox, in a few words: its flags, or nothing. */
export function keyFlagsSummary(flags: KeyFlags): string {
  const held = keyFlagNames.filter(flag => flags[flag])
  return held.length ? held.map(keyFlagLabel).join(', ') : t('Nothing')
}

export function keyFlagsOf(value: Partial<KeyFlags> | undefined): KeyFlags {
  return { read: !!value?.read, act: !!value?.act, send: !!value?.send }
}

export function holdsAnyKeyFlag(flags: KeyFlags): boolean {
  return flags.read || flags.act || flags.send
}

export function sameKeyFlags(a: KeyFlags, b: KeyFlags): boolean {
  return keyFlagNames.every(flag => a[flag] === b[flag])
}

/**
 * Flags as a person ticks them: act never stands without read, so ticking act
 * ticks read, and unticking read unticks act.
 */
export function toggleKeyFlag(flags: KeyFlags, flag: KeyFlag, on: boolean): KeyFlags {
  const next = { ...flags, [flag]: on }
  if (flag === 'act' && on) next.read = true
  if (flag === 'read' && !on) next.act = false
  return next
}

/** What a key is, as the rules for what it may be given need it. */
export type KeyFacts = Pick<WorkspaceKey, 'scope' | 'sends' | 'carried_over' | 'origin'> & { live: boolean }

export interface KeyAccessContext {
  /** The key, or what one being created will be. */
  key: KeyFacts
  /** The caller reads the mailbox now: only then may they give a key Read on it. */
  viewerReads: boolean
  /** This server's keys may send (GET /v1/me/mcp keys_send). */
  keysSend: boolean
}

export interface KeyAccessRules {
  /** Each flag the caller may turn on, where it is off. Act also needs Read there after the change. */
  canAdd: Record<KeyFlag, boolean>
  /** Each flag the caller may turn off, where it is on. */
  canRemove: Record<KeyFlag, boolean>
}

/**
 * What an owner or an admin of a workspace may give a key on one of its
 * mailboxes (docs/workspaces.md, "API keys"): Read only where they read the
 * mailbox themselves; Act where the key reads, with a scope that acts; Send
 * with the send scope, on a server whose keys may send, under the current
 * key terms (sends). A key carried over from before keys belonged to
 * workspaces gains nothing, and one that no longer works changes no more.
 * Anything may be taken away from a key that works.
 */
export function keyAccessRules(context: KeyAccessContext): KeyAccessRules {
  const { key, viewerReads, keysSend } = context
  const open = key.live && !key.carried_over
  return {
    canAdd: {
      read: open && viewerReads,
      act: open && scopeActs(key.scope),
      send: open && key.sends && keysSend,
    },
    canRemove: { read: key.live, act: key.live, send: key.live },
  }
}

/**
 * Whether a flag may be ticked or unticked in a row, from what is ticked now
 * and what the key held: setting back a change is always possible; Act, the
 * Read it needs must be there, or be the caller's to give.
 */
export function keyFlagEditable(rules: KeyAccessRules, held: KeyFlags, shown: KeyFlags, flag: KeyFlag): boolean {
  if (shown[flag] !== held[flag]) return true
  if (shown[flag]) return rules.canRemove[flag]
  if (!rules.canAdd[flag]) return false
  return flag !== 'act' || shown.read || rules.canAdd.read
}

/**
 * A person a key's record names, in words: who created it or gave it a
 * mailbox, by the name the workspace lists them by, "you", or the upgrade
 * that moved a person's key into their workspace; '' once that person was
 * deleted, as the record then names nobody.
 */
export function keyPerson(id: string | undefined, me: string, personName: (id: string) => string): string {
  if (!id) return ''
  if (id === 'migration') return t('the upgrade to workspace keys')
  if (id === me) return t('You')
  return personName(id) || t('someone no longer in the team')
}

/**
 * What a key made before keys belonged to workspaces does differently, said
 * on it: one carried over with mailboxes in several workspaces gains none,
 * and is revoked with its last; one moved into its workspace kept what it
 * held. Both act only while their person allows actions, and never send, as
 * the terms they were made under said. '' for a key made as keys are now.
 *
 * Where it is said matters for a key carried over: in a workspace's keys,
 * revoking it takes only that workspace's mailboxes out of it; among the
 * keys the person created (`mine`, My account), revoking it ends it in
 * every workspace, and the list there does not count the others.
 */
export function keyOriginNote(key: Pick<WorkspaceKey, 'carried_over' | 'origin' | 'other_workspaces'>, where: 'workspace' | 'mine' = 'workspace'): string {
  if (key.carried_over && where === 'mine') {
    return t('Made before keys belonged to workspaces, with mailboxes in several of them, and kept as it was: it gains no mailbox, acts only while you allow actions, never sends, and is revoked with its last mailbox. Revoking it here ends it in every workspace.')
  }
  if (key.carried_over) {
    return t('Made before keys belonged to workspaces, and kept as it was: it also holds mailboxes in {count} other workspaces, gains no mailbox, acts only while the person who made it allows actions, never sends, and is revoked with its last mailbox. Revoking it here takes this workspace’s mailboxes out of it.', { count: count(key.other_workspaces ?? 0) })
  }
  if (key.origin === 'person-all') return t('Made before keys belonged to workspaces, for every mailbox of the person who made it: it was given the mailboxes they read then, and none connected since. It acts only while they allow actions, and never sends.')
  if (key.origin) return t('Made before keys belonged to workspaces, for the mailboxes the person who made it chose. It acts only while they allow actions, and never sends.')
  return ''
}

export function sendStateLabel(state: KeySend['state']): string {
  switch (state) {
    case 'sending': return t('Sending')
    case 'sent': return t('Sent')
    case 'failed': return t('Not sent')
    // Handed over before the connection dropped: it may have been delivered, and it is never sent again by itself.
    case 'unknown': return t('May have been delivered')
    default: return state
  }
}
