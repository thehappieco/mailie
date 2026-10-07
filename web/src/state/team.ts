// The people of the team the console shows, who holds what on its mailboxes
// and each one's agreement to sync, and its pending invitations
// (docs/workspaces.md): read when a screen of the team's owners and admins
// needs them (a member is shown none of it, and the server refuses it to
// them), and changed through the server, which decides every change in the
// transaction that makes it. A personal workspace has none of this: its
// person is its only member.
//
// The server works out each member's protections (last_owner,
// last_reader_of) and each mailbox's readers across the whole team, so after
// any change of people or access, and whenever the team's mailboxes in the
// person's own list come or go (one linked, removed), what was read is read
// again whole rather than patched. The caller's own role is taken from their
// row as it is listed, and a refusal for want of a role reads their
// workspaces again: what the console offers follows the role they have now.
//
// Everything resets when the person changes and when the console shows
// another workspace; a slow answer for one never lands in another's, nor an
// older answer over a newer one.

import { reactive, watch } from 'vue'
import { setMailboxSync } from '../api/sync'
import * as api from '../api/workspaces'
import { ApiError } from '../api/http'
import type { GrantFlags, MailboxAccess, Member, MemberChange, TeamInvite, WorkspaceRole } from '../api/types'
import { edition } from '../edition'
import { grantChange } from '../ui/access'
import { accounts, forgetFolders, loadAccounts, mergeSync, refreshAccount } from './accounts'
import { failure, type Failure, type Operation } from './failure'
import { authorized, identity, session } from './session'
import { adoptOwnPlace, loadWorkspaces, workspaces } from './workspaces'

interface Listing<T> { list: T[]; loaded: boolean; loading: boolean; failure: Failure | null }

interface TeamState {
  /** The workspace these were read for. */
  workspace: string
  members: Listing<Member>
  /** Who holds what on each mailbox of the team, and its agreement to sync. */
  directory: Listing<MailboxAccess>
  invites: Listing<TeamInvite>
}

const listing = <T>(): Listing<T> => ({ list: [], loaded: false, loading: false, failure: null })
const fresh = (): TeamState => ({ workspace: '', members: listing(), directory: listing(), invites: listing() })

export const team = reactive<TeamState>(fresh())

let generation = 0
/** Per listing, the latest read started: an older one that lands later is dropped. */
const latest: Partial<Record<Operation, number>> = {}
let reads = 0
function reset(): void {
  generation++
  Object.assign(team, fresh())
}
watch(identity, reset, { flush: 'sync' })
watch(() => workspaces.currentID, id => { if (team.workspace !== id) reset() }, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

/** The team shown, when it is one: these are read only for a team. */
function teamID(): string {
  const shown = workspaces.list.find(item => item.id === workspaces.currentID)
  return shown?.kind === 'team' ? shown.id : ''
}

/**
 * A failure of a team or access route. A refusal for want of a role means the
 * person's role changed elsewhere: their workspaces are read again (while the
 * same person and team are shown), and what the console offers follows the
 * role they have now.
 */
function refused(op: Operation, error: unknown, ok: () => boolean): Failure {
  const found = failure(op, error)
  if (found.code === 'not_authorized' && ok()) void loadWorkspaces()
  return found
}

async function read<T>(into: Listing<T>, op: Operation, call: (token: string, id: string) => Promise<T[]>, adopt?: (list: T[], id: string) => void): Promise<void> {
  const id = teamID()
  if (!id) return
  const ok = current()
  const mine = ++reads
  latest[op] = mine
  const newest = () => ok() && latest[op] === mine
  team.workspace = id
  into.loading = true
  into.failure = null
  try {
    const list = await authorized(token => call(token, id))
    if (!newest()) return
    into.list = list
    into.loaded = true
    adopt?.(list, id)
  } catch (error) {
    if (!newest()) return
    into.failure = refused(op, error, newest)
    // No longer a member: the person's workspaces say which they are in now.
    if (into.failure.code === 'not_found') void loadWorkspaces()
  } finally {
    if (newest()) into.loading = false
  }
}

/** The caller's own row says their role there now: what the console offers follows it. */
function adoptOwnRow(list: Member[], id: string): void {
  const own = list.find(member => member.user_id === session.user?.id)
  if (own) adoptOwnPlace(id, own.role, own.status)
}

export function loadMembers(): Promise<void> {
  return read(team.members, 'load-members', api.listMembers, adoptOwnRow)
}

export function loadDirectory(): Promise<void> {
  return read(team.directory, 'load-access', api.accessDirectory)
}

export function loadInvites(): Promise<void> {
  return read(team.invites, 'load-invites', api.listTeamInvites)
}

/**
 * Reads again what was read of the team shown: its people, whose protections
 * (the last owner, the last reader of a mailbox) the server works out across
 * the team, and who holds what on its mailboxes. What was never read is left
 * for whoever asks.
 */
export function refreshTeam(): void {
  if (!teamID() || team.workspace !== teamID()) return
  if (team.members.loaded || team.members.loading) void loadMembers()
  if (team.directory.loaded || team.directory.loading) void loadDirectory()
}

// The team's mailboxes in the person's own list came or went: one linked
// (whoever linked it reads it, and is its last reader), or removed (it
// leaves the directory, and its last reader's protection goes). Not the
// first read of the list, nor another workspace's.
watch(() => accounts.loaded && team.workspace && accounts.workspace === team.workspace ? accounts.list.map(item => item.id).sort().join(' ') : null, (now, before) => {
  if (now !== null && before !== null && now !== before) refreshTeam()
})

/** A member of the team shown, by id, as last listed. */
export function memberOf(userID: string | undefined): Member | undefined {
  return userID ? team.members.list.find(member => member.user_id === userID) : undefined
}

/** A person's name as the team lists them: their name, or their address. */
export function personName(userID: string | undefined): string {
  if (userID && userID === session.user?.id) return session.user.name || session.user.email
  const member = memberOf(userID)
  return member ? member.name || member.email : ''
}

/** Who holds what on a mailbox of the team shown, as last listed. */
export function directoryEntry(accountID: string): MailboxAccess | undefined {
  return team.directory.list.find(entry => entry.account_id === accountID)
}

/**
 * Sets what a person holds on a mailbox, from what they hold now to what the
 * caller ticked: a revoke when flags only go, the grant set exactly when one
 * comes. The directory is read again either way, and the members (whose last
 * reader may have changed); and the caller's own card, when the grant was
 * theirs.
 */
export async function saveGrant(accountID: string, userID: string, before: GrantFlags, after: GrantFlags): Promise<Failure | null> {
  const change = grantChange(before, after)
  if (change.kind === 'none') return null
  const ok = current()
  try {
    if (change.kind === 'set') await authorized(token => api.setAccess(token, accountID, userID, change.flags))
    else await authorized(token => api.revokeAccess(token, accountID, userID, change.flags))
  } catch (error) {
    const found = refused('change-access', error, ok)
    // Changed elsewhere meanwhile, or gone: what is shown is read again.
    if (ok() && (found.code === 'conflict' || found.code === 'not_found')) { void loadDirectory(); void loadMembers() }
    return found
  }
  if (!ok()) return null
  // What the row shows next is what the server now holds.
  void loadMembers()
  await Promise.all([loadDirectory(), userID === session.user?.id ? followOwnAccess(accountID) : undefined])
  return null
}

/** The caller's own grant on a mailbox changed: its card says what they hold now, or goes. */
async function followOwnAccess(accountID: string): Promise<void> {
  if (accounts.list.some(item => item.id === accountID)) await refreshAccount(accountID)
  else if (accounts.loaded) await loadAccounts()
}

/**
 * Turns a team mailbox's sync on or off for the team, as an owner or an
 * admin of it: on, to the text this console showed (the edition's sync
 * revision, never the one the server names), which also gives again an
 * agreement to an earlier text, or one the upgrade carried over; off,
 * deleting its index for everyone who reads it. The card takes the sync the
 * answer says, folders read of the mailbox are read again when next wanted,
 * and the directory, which records who gave the agreement, is read again.
 */
export async function switchTeamSync(accountID: string, on: boolean): Promise<Failure | null> {
  const ok = current()
  const body = on ? { enabled: true, version: edition().sync.version } : { enabled: false }
  try {
    const status = await authorized(token => setMailboxSync(token, accountID, body))
    if (!ok()) return null
    mergeSync(accountID, status)
    forgetFolders(accountID)
  } catch (error) {
    const found = refused(on ? 'team-sync-on' : 'team-sync-off', error, ok)
    if (ok() && found.code === 'not_found') void loadDirectory()
    return found
  }
  await loadDirectory()
  return null
}

/**
 * Changes a member's role or status. Disabling takes their grants in the team
 * with it; enabling gives none back; any change ends the invitations they
 * made there. The row changes at once, and the list is read again: another
 * member's protections may have changed with it (the caller is the last
 * owner once another owner is demoted, or the last reader of a mailbox once
 * another reader is disabled).
 */
export async function changeMember(userID: string, change: MemberChange): Promise<Failure | null> {
  const id = teamID()
  if (!id) return null
  const ok = current()
  try {
    const member = await authorized(token => api.changeMember(token, id, userID, change))
    if (!ok()) return null
    const index = team.members.list.findIndex(item => item.user_id === userID)
    if (index >= 0) team.members.list.splice(index, 1, member)
    void loadMembers()
    // Disabling takes their grants; a promotion, the Manage they held, which the role gives now.
    if (team.directory.loaded) void loadDirectory()
    // Any change ended the invitations they made here: the pending list shows them no more.
    if (team.invites.loaded) void loadInvites()
    // The caller's own role: what they may do here changed.
    if (userID === session.user?.id) void loadWorkspaces()
    return null
  } catch (error) {
    if (ok() && error instanceof ApiError && (error.code === 'not_found' || error.code === 'conflict')) void loadMembers()
    return refused('change-member', error, ok)
  }
}

/**
 * Removes a member: their grants in the team, its invitations waiting for
 * them and those they made go too. The list is read again, as the
 * protections of those who stay may have changed.
 */
export async function removeMember(userID: string): Promise<Failure | null> {
  const id = teamID()
  if (!id) return null
  const ok = current()
  try {
    await authorized(token => api.removeMember(token, id, userID))
  } catch (error) {
    if (!(error instanceof ApiError && error.code === 'not_found')) {
      if (ok() && error instanceof ApiError && error.code === 'conflict') void loadMembers()
      return refused('remove-member', error, ok)
    }
  }
  if (!ok()) return null
  team.members.list = team.members.list.filter(item => item.user_id !== userID)
  void loadMembers()
  if (team.directory.loaded) void loadDirectory()
  if (team.invites.loaded) void loadInvites()
  return null
}

/**
 * The caller leaves the team shown, as an owner while another owner remains:
 * the console then shows another of their workspaces.
 */
export async function leaveTeam(): Promise<Failure | null> {
  const id = teamID()
  const me = session.user?.id
  if (!id || !me) return null
  const ok = current()
  try {
    await authorized(token => api.removeMember(token, id, me))
  } catch (error) {
    if (!(error instanceof ApiError && error.code === 'not_found')) {
      // Refused by a protection the list did not show yet: it is read again.
      if (ok() && error instanceof ApiError && error.code === 'conflict') void loadMembers()
      return refused('leave-team', error, ok)
    }
  }
  await loadWorkspaces()
  return null
}

/** An invitation made, with the link to send, for the caller alone to show once; or why not. */
export type InviteCreation = { invite: TeamInvite } | { failure: Failure }

/**
 * Invites an address into the team shown, with a role in it. The answer's
 * link is handed to the caller and kept nowhere: the list is read again, and
 * lists invitations without their links.
 */
export async function inviteMember(email: string, role: WorkspaceRole): Promise<InviteCreation> {
  const id = teamID()
  if (!id) return { failure: { op: 'create-invite', code: 'not_found' } }
  const ok = current()
  try {
    const invite = await authorized(token => api.createTeamInvite(token, id, email, role))
    if (!ok()) return { failure: { op: 'create-invite', code: 'aborted' } }
    void loadInvites()
    return { invite }
  } catch (error) {
    const found = refused('create-invite', error, ok)
    // No answer, or one this page cannot read, may have made it all the same.
    if (ok() && (found.code === 'unavailable' || found.code === 'internal' || found.code === 'invalid_response')) void loadInvites()
    return { failure: found }
  }
}

/** Revokes a pending invitation: its link stops working. One already gone is what was asked for. */
export async function revokeInvite(inviteID: string): Promise<Failure | null> {
  const id = teamID()
  if (!id) return null
  const ok = current()
  try {
    await authorized(token => api.revokeTeamInvite(token, id, inviteID))
  } catch (error) {
    if (!(error instanceof ApiError && error.code === 'not_found')) return refused('revoke-invite', error, ok)
  }
  if (ok()) team.invites.list = team.invites.list.filter(item => item.id !== inviteID)
  return null
}
