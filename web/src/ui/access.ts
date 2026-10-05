// Who may do what in a workspace, as the console explains it before anyone
// tries (docs/workspaces.md, "Who may do what" and "Protections"). These only
// decide what is offered and what is said about it: the server decides, in
// the transaction that would make the change, and refuses whatever these get
// wrong. Pure functions of what the server listed, so each rule is a test.

import type { Account, GrantFlags, Member, Workspace, WorkspaceRole } from '../api/types'
import { t } from './i18n'

export const flagNames = ['read', 'act', 'send', 'manage'] as const
export type FlagName = typeof flagNames[number]

export const FULL_ACCESS: GrantFlags = { read: true, act: true, send: true, manage: true }
export const NO_ACCESS: GrantFlags = { read: false, act: false, send: false, manage: false }

/** What the caller may do with a mailbox. A server older than workspaces does not say, and decides all the same. */
export function accessOf(account: Pick<Account, 'access'> | undefined): GrantFlags {
  return account?.access ?? FULL_ACCESS
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
    case 'manage': return t('Authorize it again, remove it, and choose who has access to it.')
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

/** What protects a member from losing their place: the team's last active owner, or a person a mailbox there is linked by. */
export type MemberProtection = '' | 'last-owner' | 'linker'

export interface MemberPermissions {
  /** The roles the caller may give this member, theirs included; one means there is nothing to choose. */
  roles: WorkspaceRole[]
  canDisable: boolean
  canEnable: boolean
  /** Removing someone else. */
  canRemove: boolean
  /** The caller removing themselves. */
  canLeave: boolean
  /** Why disabling, removing or leaving is refused for this member, whoever asks. */
  protection: MemberProtection
}

/**
 * What the caller may change about a member of a team: an owner anyone's
 * role and status, and removes anyone; an admin disables, enables and
 * removes members only, and makes nobody an admin or an owner; a member only
 * leaves. Whoever asks, the last active owner is neither demoted, disabled
 * nor removed, and nobody linked mailboxes still linked there is disabled or
 * removed.
 */
export function memberPermissions(callerID: string, callerRole: string | undefined, member: Member): MemberPermissions {
  const self = member.user_id === callerID
  const owner = callerRole === 'owner'
  const admin = callerRole === 'admin'
  const protection: MemberProtection = member.last_owner ? 'last-owner' : member.links > 0 ? 'linker' : ''
  const roles: WorkspaceRole[] = owner && !member.last_owner ? ['owner', 'admin', 'member'] : [member.role as WorkspaceRole]
  const changesStatus = !self && (owner || (admin && member.role === 'member'))
  return {
    roles,
    canDisable: changesStatus && member.status === 'active' && !protection,
    canEnable: changesStatus && member.status === 'disabled',
    canRemove: changesStatus && !protection,
    canLeave: self && !protection,
    protection,
  }
}

/** A member who counts: active in the workspace, and on the server. */
export function activeMember(member: Pick<Member, 'status' | 'person_disabled'>): boolean {
  return member.status === 'active' && !member.person_disabled
}

/** Why nothing of a person's grant changes from here: they linked the mailbox, or they are not an active member. */
export type GrantLock = '' | 'linker' | 'inactive'

export interface GrantPermissions {
  lock: GrantLock
  /** Each flag the caller may turn on, where it is off. */
  canAdd: Record<FlagName, boolean>
  /** Each flag the caller may turn off, where it is on. */
  canRemove: Record<FlagName, boolean>
  /** The person is the only one who manages the mailbox: manage stays theirs. */
  lastManager: boolean
}

export interface GrantContext {
  callerID: string
  /** The caller owns or administers the mailbox's workspace. */
  administers: boolean
  /** What the caller holds on the mailbox. */
  mine: GrantFlags
  /** The person the row is for, and what they hold on the mailbox. */
  member: Member
  held: GrantFlags
  /** The person the mailbox is linked by, whose grant nobody changes while it is. */
  linkedBy?: string
  /** How many people hold manage on the mailbox. */
  managers: number
}

/**
 * What the caller may change of one person's grant on a mailbox
 * (docs/workspaces.md, "Who may change a grant"): an owner or an admin of the
 * workspace, or someone who manages the mailbox, may change anyone's; read,
 * act and send are passed on only by someone who holds them, so owners and
 * admins give no read they do not have, not even to themselves; manage, by an
 * owner, an admin or a manager. Anyone may drop their own flags. The person a
 * mailbox is linked by keeps all four while it is, and a mailbox keeps
 * someone who manages it.
 */
export function grantPermissions(context: GrantContext): GrantPermissions {
  const { callerID, mine, member, held } = context
  const none = { read: false, act: false, send: false, manage: false }
  const lastManager = held.manage && context.managers <= 1
  if (context.linkedBy && member.user_id === context.linkedBy) return { lock: 'linker', canAdd: none, canRemove: none, lastManager }
  const manages = context.administers || mine.manage
  const self = member.user_id === callerID
  const active = activeMember(member)
  const canAdd = {
    read: manages && active && mine.read,
    act: manages && active && mine.act,
    send: manages && active && mine.send,
    manage: manages && active,
  }
  const takesAway = manages || self
  const canRemove = { read: takesAway, act: takesAway, send: takesAway, manage: takesAway && !lastManager }
  const lock: GrantLock = !active && !holdsAny(held) ? 'inactive' : ''
  return { lock, canAdd, canRemove, lastManager }
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

/** Where taking a link over stands for the caller. */
export type TakeOverStanding = 'linker' | 'available' | 'needs-flags' | 'needs-role' | 'needs-sync' | 'needs-current-sync'

/** Where the caller stands on sync: agreed, and to the text the server describes it with now. */
export interface SyncStanding { consented: boolean; current: boolean }

/**
 * Whether the caller may take a mailbox's link over (POST …/take-over): they
 * hold all four flags on it, may link into its workspace (an owner or an admin
 * of a team), and have agreed to sync, which it would then sync under, in the
 * text the server describes sync with now: an earlier one may not say who
 * reads a team mailbox's index.
 */
export function takeOverStanding(input: { callerID: string; linkedBy?: string; mine: GrantFlags; workspace: Workspace | undefined; sync: SyncStanding }): TakeOverStanding {
  if (!input.linkedBy || input.linkedBy === input.callerID) return 'linker'
  if (!holdsAll(input.mine)) return 'needs-flags'
  if (!canLinkInto(input.workspace)) return 'needs-role'
  if (!input.sync.consented) return 'needs-sync'
  if (!input.sync.current) return 'needs-current-sync'
  return 'available'
}

/**
 * Whether linking a mailbox into this workspace waits for the caller to agree
 * to the current text of sync: a team mailbox syncs under the agreement of
 * whoever links it, at once when they agreed before, and an earlier text may
 * not say who reads a team mailbox's index. Agreeing to none is no reason to
 * wait: nothing syncs until they agree, and that is to the current text.
 */
export function linkWaitsForSync(workspace: Workspace | undefined, sync: SyncStanding): boolean {
  return workspace?.kind === 'team' && sync.consented && !sync.current
}
