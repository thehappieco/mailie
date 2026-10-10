// Who may do what in a workspace, as the console explains it before anyone
// tries (docs/workspaces.md, "Who may do what" and "Protections"). These only
// decide what is offered and what is said about it: the server decides, in
// the transaction that would make the change, and refuses whatever these get
// wrong. Pure functions of what the server listed, so each rule is a test.

import type { Account, GrantFlags, MailboxConsent, Member, Workspace, WorkspaceRole } from '../api/types'
import { t } from './i18n'

export const flagNames = ['read', 'act', 'send', 'manage'] as const
export type FlagName = typeof flagNames[number]

export const FULL_ACCESS: GrantFlags = { read: true, act: true, send: true, manage: true }
export const NO_ACCESS: GrantFlags = { read: false, act: false, send: false, manage: false }

/** What the caller may do with a mailbox. A server older than workspaces does not say, and decides all the same. */
export function accessOf(account: Pick<Account, 'access'> | undefined): GrantFlags {
  return account?.access ?? FULL_ACCESS
}

/**
 * Whether the caller holds Read on a mailbox that has a key, without the key
 * itself (docs/key-scheme.md section 12.13): they read nothing of it until
 * someone who reads it hands the key to them, or, for their own personal
 * mailbox, they write it a new one.
 */
export function waitsForKey(account: Pick<Account, 'access'> | undefined): boolean {
  return account?.access?.waiting_key === true
}

export function flagsOf(value: Partial<GrantFlags> | undefined): GrantFlags {
  return { read: !!value?.read, act: !!value?.act, send: !!value?.send, manage: !!value?.manage }
}

export function holdsAll(flags: GrantFlags): boolean {
  return flagNames.every(flag => flags[flag])
}

export function holdsAny(flags: GrantFlags): boolean {
  return flagNames.some(flag => flags[flag])
}

export function sameFlags(a: GrantFlags, b: GrantFlags): boolean {
  return flagNames.every(flag => a[flag] === b[flag])
}

export function flagLabel(flag: FlagName): string {
  switch (flag) {
    case 'read': return t('Read')
    case 'act': return t('Act')
    // The access, not the button that sends: an edition may have that one.
    case 'send': return t('Send|access')
    case 'manage': return t('Manage')
  }
}

/** What a flag lets its holder do, in a sentence. */
export function flagHint(flag: FlagName): string {
  switch (flag) {
    case 'read': return t('Search and read its messages, and see its folders and what it takes up.')
    case 'act': return t('Mark, star, archive and move its messages, once they allow actions themselves. Needs Read.')
    case 'send': return t('Send from it, under their own name, once they allow sending themselves.')
    case 'manage': return t('See it and authorize it again, without reading it. Owners and admins of the team have it by their role.')
  }
}

/** A grant in a few words: the flags it has, or that it is all of them. */
export function grantSummary(flags: GrantFlags): string {
  if (holdsAll(flags)) return t('Full access')
  if (!holdsAny(flags)) return t('No access')
  return flagNames.filter(flag => flags[flag]).map(flagLabel).join(', ')
}

export function workspaceRoleLabel(role: string | undefined): string {
  switch (role) {
    case 'owner': return t('Owner')
    case 'admin': return t('Admin')
    case 'member': return t('Member')
    default: return role ?? ''
  }
}

/** A workspace's name: a team's own, or the console's word for the others. */
export function workspaceName(workspace: Pick<Workspace, 'kind' | 'name'> | undefined): string {
  switch (workspace?.kind) {
    case undefined: return ''
    case 'personal': return t('Personal')
    case 'operator': return t('Operator')
    default: return workspace?.name || t('Team')
  }
}

/** The caller administers the workspace's people and grants: an owner or an admin of a team, active in it. */
export function administers(workspace: Workspace | undefined): boolean {
  return workspace?.kind === 'team' && (workspace.role === 'owner' || workspace.role === 'admin') && workspace.status !== 'disabled'
}

/**
 * Whether the caller sees, creates and revokes the workspace's API keys: an
 * owner or an admin of a team, active in it, or the person of their personal
 * workspace. A member of a team sees none; nobody's is the operator's.
 */
export function administersKeys(workspace: Workspace | undefined): boolean {
  return (workspace?.kind === 'personal' && workspace.status !== 'disabled') || administers(workspace)
}

/** Whether the caller may link a mailbox into the workspace: their personal one, or a team they own or administer. */
export function canLinkInto(workspace: Workspace | undefined): boolean {
  return workspace?.kind === 'personal' || administers(workspace)
}

/**
 * Whether people and invitations of a workspace are changed here: a team
 * created here (source local). One mirrored from elsewhere is changed where
 * it comes from, and the server refuses every change to it.
 */
export function changeableTeam(workspace: Workspace | undefined): boolean {
  return workspace?.kind === 'team' && workspace.source === 'local'
}

/**
 * Whether teams are created here at all: none of the person's workspaces is
 * mirrored from elsewhere, which is how a server whose workspaces come from
 * elsewhere shows itself.
 */
export function teamsCreatedHere(list: readonly Workspace[]): boolean {
  return list.every(workspace => workspace.source === 'local')
}

/** The roles an invitation from this caller may give: an owner any, an admin member only. */
export function inviteRoles(role: string | undefined): WorkspaceRole[] {
  switch (role) {
    case 'owner': return ['owner', 'admin', 'member']
    case 'admin': return ['member']
    default: return []
  }
}

/** Whether the caller sees and makes a team's invitations: an owner or an admin of it. A member sees none. */
export function seesInvitations(role: string | undefined): boolean {
  return inviteRoles(role).length > 0
}

/** The team's mailboxes this member alone can read, as the server works them out. */
export function lastReaderOf(member: Pick<Member, 'last_reader_of'>): string[] {
  return member.last_reader_of ?? []
}

/** What protects a member from losing their place: the team's last active owner, or the last person who can read one of its mailboxes. */
export type MemberProtection = '' | 'last-owner' | 'last-reader'

export interface MemberPermissions {
  /** The roles the caller may give this member, theirs included; one means there is nothing to choose. */
  roles: WorkspaceRole[]
  canDisable: boolean
  canEnable: boolean
  /** Removing someone else. */
  canRemove: boolean
  /** The caller removing themselves: an owner, while another owner remains. */
  canLeave: boolean
  /** Why disabling, removing or leaving is refused for this member, whoever asks. */
  protection: MemberProtection
}

/**
 * What the caller may change about a member of a team: an owner anyone's
 * role and status, removes anyone, and leaves while another owner remains;
 * an admin disables, enables and removes members only, and makes nobody an
 * admin or an owner; a member nothing, not even leaving. Whoever asks, the
 * last active owner is neither demoted, disabled nor removed, and the last
 * person who can read one of the team's mailboxes is neither disabled nor
 * removed (a new role takes no read away).
 */
export function memberPermissions(callerID: string, callerRole: string | undefined, member: Member): MemberPermissions {
  const self = member.user_id === callerID
  const owner = callerRole === 'owner'
  const admin = callerRole === 'admin'
  const protection: MemberProtection = member.last_owner ? 'last-owner' : lastReaderOf(member).length ? 'last-reader' : ''
  const roles: WorkspaceRole[] = owner && !member.last_owner ? ['owner', 'admin', 'member'] : [member.role as WorkspaceRole]
  const changesStatus = !self && (owner || (admin && member.role === 'member'))
  return {
    roles,
    canDisable: changesStatus && member.status === 'active' && !protection,
    canEnable: changesStatus && member.status === 'disabled',
    canRemove: changesStatus && !protection,
    canLeave: self && owner && !protection,
    protection,
  }
}

/** A member who counts: active in the workspace, and on the server. */
export function activeMember(member: Pick<Member, 'status' | 'person_disabled'>): boolean {
  return member.status === 'active' && !member.person_disabled
}

/** An owner or an admin, who manages every mailbox of the team by that role. */
export function managesByRole(role: string | undefined): boolean {
  return role === 'owner' || role === 'admin'
}

/** Why nothing of a person's grant changes from here: they are not an active member, and hold nothing. */
export type GrantLock = '' | 'inactive'

export interface GrantPermissions {
  lock: GrantLock
  /** Each flag the caller may turn on, where it is off. Act also needs Read there after the change. */
  canAdd: Record<FlagName, boolean>
  /** Each flag the caller may turn off, where it is on. */
  canRemove: Record<FlagName, boolean>
  /** The person is the only one who can read the mailbox: Read stays theirs. */
  lastReader: boolean
  /** An owner or an admin, who manages it by their role: Manage is never theirs to be given or taken. */
  byRole: boolean
}

export interface GrantContext {
  /** The caller owns or administers the mailbox's workspace. */
  administers: boolean
  /** What the caller holds on the mailbox. */
  mine: GrantFlags
  /** The person the row is for, and what they hold on the mailbox. */
  member: Member
  held: GrantFlags
  /** The person is the only one who can read the mailbox. */
  lastReader: boolean
}

/**
 * What the caller may change of one person's grant on a team mailbox
 * (docs/workspaces.md, "Grant rules"): only an owner or an admin of the
 * team, for anyone in it, themselves included. Read passes only from one
 * who reads the mailbox now, so no role gives it; Act goes only to someone
 * who reads it after the change, and Send to anyone, without holding them;
 * Manage is given to members only, as owners and admins have it by their
 * role. Any flag may be taken away, but the last reader's Read. A member
 * changes nothing, not even their own.
 */
export function grantPermissions(context: GrantContext): GrantPermissions {
  const { administers: admin, mine, member, held } = context
  const active = activeMember(member)
  const byRole = managesByRole(member.role)
  const canAdd = {
    read: admin && active && mine.read,
    act: admin && active,
    send: admin && active,
    manage: admin && active && !byRole,
  }
  const canRemove = { read: admin && !context.lastReader, act: admin, send: admin, manage: admin }
  const lock: GrantLock = !active && !holdsAny(held) ? 'inactive' : ''
  return { lock, canAdd, canRemove, lastReader: context.lastReader, byRole }
}

/** How a grant goes from one set of flags to another: the request that does it, or none. */
export type GrantChange =
  | { kind: 'none' }
  /** Flags only taken away: a revoke, which needs nothing held (DELETE …/access/{user}?flags=). */
  | { kind: 'revoke'; flags: FlagName[] }
  /** Something added: the grant set to exactly these (PUT). */
  | { kind: 'set'; flags: GrantFlags }

export function grantChange(before: GrantFlags, after: GrantFlags): GrantChange {
  if (sameFlags(before, after)) return { kind: 'none' }
  const added = flagNames.some(flag => after[flag] && !before[flag])
  if (added) return { kind: 'set', flags: { ...after } }
  // Every flag going: none named takes every one.
  if (!holdsAny(after)) return { kind: 'revoke', flags: [] }
  return { kind: 'revoke', flags: flagNames.filter(flag => before[flag] && !after[flag]) }
}

/**
 * Flags as a person ticks them: act never stands without read, so ticking act
 * ticks read, and unticking read unticks act.
 */
export function toggleFlag(flags: GrantFlags, flag: FlagName, on: boolean): GrantFlags {
  const next = { ...flags, [flag]: on }
  if (flag === 'act' && on) next.read = true
  if (flag === 'read' && !on) next.act = false
  return next
}

/**
 * Who did something, as the server records it, in words: a person by the
 * name the team lists them by, a key, or the command line. '' when the
 * record names nobody, as it does once that person was deleted.
 */
export function actorName(actor: string | undefined, personName: (id: string) => string): string {
  if (!actor) return ''
  if (actor === 'cli') return t('the command line')
  if (actor.startsWith('key:')) return t('an API key')
  return personName(actor) || t('someone no longer in the team')
}

/**
 * Where a team mailbox's own agreement to sync stands, as its owners and
 * admins see it (MailboxAccess.sync): on, to the current text or an earlier
 * one; on under the agreement the upgrade carried over from whoever linked
 * it, still tied to them (migrated); stopped with its index kept, because
 * that person was disabled (kept); or off.
 */
export type TeamSyncStanding = 'on' | 'on-earlier' | 'migrated' | 'kept' | 'off'

export function teamSyncStanding(consent: MailboxConsent | undefined): TeamSyncStanding {
  if (!consent) return 'off'
  if (consent.enabled) return consent.migrated ? 'migrated' : consent.current ? 'on' : 'on-earlier'
  return consent.migrated ? 'kept' : 'off'
}
