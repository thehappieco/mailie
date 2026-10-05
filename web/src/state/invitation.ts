// The invitation the page was opened with (an <origin>/#invite=…&email=…
// link, ui/signupLink.ts), until it is spent, accepted or set aside. In
// memory only: its code never reaches storage, the address bar or history.
//
// Signed out, an invitation is used to create an account. On a server whose
// teams are made here (edition().teams), a person who already has an account
// signs in, or is signed in already, and accepts it instead: an invitation
// into a team joins them to it (POST /v1/auth/invites/accept). Joining gives
// access to no mailbox.

import { reactive, watch } from 'vue'
import * as api from '../api/workspaces'
import type { Workspace } from '../api/types'
import type { Invitation } from '../ui/signupLink'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'
import { loadWorkspaces, selectWorkspace, upsertWorkspace } from './workspaces'

interface InvitationState {
  pending: Invitation | null
  busy: boolean
  problem: Failure | null
  /** The team just joined, for the notice that says so. */
  joined: Workspace | null
}

export const invitation = reactive<InvitationState>({ pending: null, busy: false, problem: null, joined: null })

// The invitation outlives a sign-in, which is how a person with an account
// gets to accept it; what came of accepting it is the previous person's.
watch(identity, () => {
  invitation.busy = false
  invitation.problem = null
  invitation.joined = null
}, { flush: 'sync' })

/** Holds the invitation the page was opened with. */
export function holdInvitation(value: Invitation | null): void {
  invitation.pending = value
  invitation.problem = null
}

/** Lets go of it: spent by a sign-up, accepted, or set aside by the person. */
export function dropInvitation(): void {
  invitation.pending = null
  invitation.problem = null
}

/** Whether the invitation names the address of the person signed in: the only one it works for. */
export function invitationFits(email: string | undefined): boolean {
  const invited = invitation.pending?.email.trim().toLowerCase() ?? ''
  return !invited || invited === (email ?? '').trim().toLowerCase()
}

/**
 * Accepts the invitation as the person signed in, who joins the team it
 * names, which the console then shows. A refusal does not spend it.
 */
export async function acceptInvitation(): Promise<boolean> {
  const code = invitation.pending?.invite
  if (!code || invitation.busy) return false
  const person = identity()
  invitation.busy = true
  invitation.problem = null
  try {
    const team = await authorized(token => api.acceptInvite(token, code))
    if (identity() !== person) return false
    invitation.pending = null
    invitation.joined = team
    upsertWorkspace(team)
    selectWorkspace(team.id)
    // The list as the server holds it now, with the team in it.
    void loadWorkspaces()
    return true
  } catch (error) {
    if (identity() === person) invitation.problem = failure('accept-invite', error)
    return false
  } finally {
    if (identity() === person) invitation.busy = false
  }
}

/** Says nothing more about the team just joined. */
export function dismissJoined(): void {
  invitation.joined = null
}
