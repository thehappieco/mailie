// /v1/workspaces and the routes that administer them: the caller's
// workspaces, a team's members and invitations, and who holds what on each of
// its mailboxes (docs/workspaces.md). Who may do what is decided by the
// server (internal/service); this file only asks.

import { segment } from './endpoint'
import { ApiError, checked, request } from './http'
import {
  isGrant, isMailboxAccessList, isMember, isMemberList, isTeamInvite, isTeamInviteList, isWorkspace, isWorkspaceList,
  type Grant, type GrantChange, type GrantFlags, type MailboxAccess, type Member, type MemberChange, type TeamInvite, type Workspace,
  type WorkspaceRole,
} from './types'

export async function listWorkspaces(token: string, signal?: AbortSignal): Promise<Workspace[]> {
  return checked(await request('/v1/workspaces', { token, signal }), isWorkspaceList)
}

/** Creates a team, whose owner the caller becomes. */
export async function createWorkspace(token: string, name: string): Promise<Workspace> {
  return checked(await request('/v1/workspaces', { token, body: { name } }), isWorkspace)
}

export async function renameWorkspace(token: string, id: string, name: string): Promise<Workspace> {
  return checked(await request(`/v1/workspaces/${segment(id)}`, { token, method: 'PATCH', body: { name } }), isWorkspace)
}

export async function listMembers(token: string, id: string, signal?: AbortSignal): Promise<Member[]> {
  return checked(await request(`/v1/workspaces/${segment(id)}/members`, { token, signal }), isMemberList)
}

export async function changeMember(token: string, id: string, userID: string, change: MemberChange): Promise<Member> {
  return checked(await request(`/v1/workspaces/${segment(id)}/members/${segment(userID)}`, { token, method: 'PATCH', body: change }), isMember)
}

/** Removes a member, or, for the caller's own id, leaves the team (an owner, while another owner remains). */
export async function removeMember(token: string, id: string, userID: string): Promise<void> {
  await request<void>(`/v1/workspaces/${segment(id)}/members/${segment(userID)}`, { token, method: 'DELETE' })
}

export async function listTeamInvites(token: string, id: string, signal?: AbortSignal): Promise<TeamInvite[]> {
  return checked(await request(`/v1/workspaces/${segment(id)}/invites`, { token, signal }), isTeamInviteList)
}

/**
 * Invites an address into a team. The answer carries the link to send, the
 * only time it exists outside the server; the caller shows it once.
 */
export async function createTeamInvite(token: string, id: string, email: string, role: WorkspaceRole): Promise<TeamInvite> {
  const invite = checked(await request(`/v1/workspaces/${segment(id)}/invites`, { token, body: { email, role } }), isTeamInvite)
  if (!invitationLink(invite.url)) throw new ApiError('invalid_response', 201)
  return invite
}

/**
 * The link an invitation travels as, when it is one a person may be handed:
 * http or https, without credentials. Anything else is not drawn or copied.
 */
export function invitationLink(value: string | undefined): string {
  if (!value) return ''
  try {
    const url = new URL(value)
    return (url.protocol === 'https:' || url.protocol === 'http:') && !url.username && !url.password ? url.toString() : ''
  } catch { return '' }
}

export async function revokeTeamInvite(token: string, id: string, inviteID: string): Promise<void> {
  await request<void>(`/v1/workspaces/${segment(id)}/invites/${segment(inviteID)}`, { token, method: 'DELETE' })
}

/** Joins the team an invitation names, with the role it gives, as the person signed in. */
export async function acceptInvite(token: string, invite: string): Promise<Workspace> {
  return checked(await request('/v1/auth/invites/accept', { token, body: { invite } }), isWorkspace)
}

export async function accessDirectory(token: string, id: string, signal?: AbortSignal): Promise<MailboxAccess[]> {
  return checked(await request(`/v1/workspaces/${segment(id)}/access`, { token, signal }), isMailboxAccessList)
}

/**
 * Sets exactly what a person holds on a mailbox: every flag is sent, so
 * leaving one out never takes it away. sealed, with Read given on a mailbox
 * that has a key to a person with an account key, is their grant at its
 * current epoch and the account public key it was sealed to, which the
 * server holds to theirs now (docs/key-scheme.md section 12.13).
 */
export async function setAccess(token: string, accountID: string, userID: string, flags: GrantFlags, sealed?: { grant: string; public_key: string }): Promise<Grant> {
  const body: GrantChange = { read: flags.read, act: flags.act, send: flags.send, manage: flags.manage }
  if (sealed) {
    body.grant = sealed.grant
    body.public_key = sealed.public_key
  }
  return checked(await request(`/v1/accounts/${segment(accountID)}/access/${segment(userID)}`, { token, method: 'PUT', body }), isGrant)
}

/** Takes these flags away from a person's grant on a mailbox; none named takes every one. */
export async function revokeAccess(token: string, accountID: string, userID: string, flags: (keyof GrantFlags)[] = []): Promise<void> {
  const query = flags.length ? { flags: flags.join(',') } : undefined
  await request<void>(`/v1/accounts/${segment(accountID)}/access/${segment(userID)}`, { token, method: 'DELETE', query })
}
