// Workspaces and teams as a person presses them, on a page (test/dom.ts):
// choosing the workspace reads its mailboxes, and an invitation opened
// signed in joins the team it names, or offers signing out for one of
// another address.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { click, find, fire, flush, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Workspace } from '../src/api/types'
import InvitationDialog from '../src/components/InvitationDialog.vue'
import WorkspaceSwitcher from '../src/components/WorkspaceSwitcher.vue'
import { accounts, loadAccounts } from '../src/state/accounts'
import { holdInvitation, invitation } from '../src/state/invitation'
import { session, adoptSession, signOut } from '../src/state/session'
import { loadWorkspaces, selectWorkspace, workspaces } from '../src/state/workspaces'
import { failure, json, reply, serve, stubPage, type Route } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const CODE = 'SUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUlJSUk'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })

interface Seen { path: string; method: string; query: Record<string, string>; body: unknown }
let seen: Seen[] = []
let mounted: Mounted | null = null

/** Signed in as Ana, with these workspaces and the team shown; route answers the rest. */
async function signedIn(role: string, route: Route, list: () => Workspace[] = () => [personal, support(role)]) {
  serve(request => {
    const url = new URL(request.url)
    seen.push({ path: request.path, method: request.method, query: Object.fromEntries(url.searchParams), body: request.body })
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (request.path === '/v1/workspaces' && request.method === 'GET') return json(list())
    return route(request)
  })
  await adoptSession(reply())
  await loadWorkspaces()
  selectWorkspace(TEAM)
}
const sent = (method: string, path: string) => seen.filter(request => request.method === method && request.path === path)

beforeEach(() => {
  stubPage()
  seen = []
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('choosing the workspace', () => {
  it('reads the mailboxes of the one chosen, and only them', async () => {
    await signedIn('member', ({ path, method }) => path === '/v1/accounts' && method === 'GET' ? json([]) : failure('not_found', 404))
    selectWorkspace(PERSONAL)
    await loadAccounts()
    mounted = mount(WorkspaceSwitcher)
    await flush()
    const select = find('select[name=workspace]')!
    expect(select.querySelectorAll('option').map(option => words(option))).toEqual(['Personal', 'Support · Member'])
    select.value = TEAM
    await fire(select, 'change')
    await vi.waitFor(() => expect(accounts.workspace).toBe(TEAM))
    expect(sent('GET', '/v1/accounts').map(request => request.query.workspace)).toEqual([PERSONAL, TEAM])
  })
})

describe('an invitation opened signed in', () => {
  it('joins the team it names, which the console then shows', async () => {
    let list = [personal]
    await signedIn('member', ({ path, method }) => {
      if (path !== '/v1/auth/invites/accept' || method !== 'POST') return failure('not_found', 404)
      list = [personal, support('member')]
      return json(support('member'))
    }, () => list)
    selectWorkspace(PERSONAL)
    holdInvitation({ invite: CODE, email: 'ana@example.test' })
    mounted = mount(InvitationDialog)
    await flush()
    expect(words(find('dialog')!)).toContain('Joining gives you access to no mailbox')
    await click(find('dialog button', 'Join the team'))
    expect(sent('POST', '/v1/auth/invites/accept').map(request => request.body)).toEqual([{ invite: CODE }])
    expect(find('dialog')).toBeNull()
    expect(workspaces.currentID).toBe(TEAM)
    expect(invitation.joined?.name).toBe('Support')
  })

  it('offers signing out for an invitation of another address, and keeps it to use then', async () => {
    await signedIn('member', () => failure('not_found', 404), () => [personal])
    holdInvitation({ invite: CODE, email: 'bea@example.test' })
    mounted = mount(InvitationDialog)
    await flush()
    expect(find('dialog button', 'Join the team')).toBeNull()
    await click(find('dialog button', 'Sign out'))
    expect(session.phase).toBe('signed-out')
    expect(invitation.pending?.email).toBe('bea@example.test')
    expect(sent('POST', '/v1/auth/invites/accept')).toEqual([])
  })
})
