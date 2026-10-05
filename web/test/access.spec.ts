// What the console offers and explains about workspaces before anyone tries
// (ui/access.ts): the server decides every change, and these rules only have
// to agree with it (docs/workspaces.md, "Who may do what" and "Protections").
import { describe, expect, it } from 'vitest'
import type { GrantFlags, Member, Workspace } from '../src/api/types'
import {
  administers, canLinkInto, changeableTeam, grantChange, grantPermissions, grantSummary, inviteRoles, memberPermissions, NO_ACCESS,
  linkWaitsForSync, seesInvitations, takeOverStanding, teamsCreatedHere, toggleFlag, workspaceName, FULL_ACCESS,
} from '../src/ui/access'

const ANA = 'usr_ana'
const BEA = 'usr_bea'
const CAROL = 'usr_carol'

function member(userID: string, fields: Partial<Member> = {}): Member {
  return { user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, links: 0, joined_at: 1_790_000_000, ...fields }
}

function team(role: string, fields: Partial<Workspace> = {}): Workspace {
  return { id: 'wsp_team', kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000, ...fields }
}

const flags = (fields: Partial<GrantFlags> = {}): GrantFlags => ({ ...NO_ACCESS, ...fields })

describe('who may give what on a mailbox', () => {
  it('lets an owner or an admin give manage, and never a read they do not hold, not even to themselves', () => {
    const rules = grantPermissions({ callerID: ANA, administers: true, mine: flags(), member: member(BEA), held: flags(), linkedBy: CAROL, managers: 1 })
    expect(rules.canAdd).toEqual({ read: false, act: false, send: false, manage: true })
    const self = grantPermissions({ callerID: ANA, administers: true, mine: flags(), member: member(ANA), held: flags(), linkedBy: CAROL, managers: 1 })
    expect(self.canAdd.read).toBe(false)
    expect(self.canAdd.manage).toBe(true)
  })

  it('lets someone who manages a mailbox pass on only the flags they hold', () => {
    const rules = grantPermissions({ callerID: ANA, administers: false, mine: flags({ read: true, manage: true }), member: member(BEA), held: flags(), linkedBy: CAROL, managers: 2 })
    expect(rules.canAdd).toEqual({ read: true, act: false, send: false, manage: true })
  })

  it('gives a member who neither administers nor manages nothing to give, and only their own flags to drop', () => {
    const other = grantPermissions({ callerID: ANA, administers: false, mine: flags({ read: true, send: true }), member: member(BEA), held: flags({ read: true }), linkedBy: CAROL, managers: 1 })
    expect(other.canAdd).toEqual({ read: false, act: false, send: false, manage: false })
    expect(other.canRemove.read).toBe(false)
    const own = grantPermissions({ callerID: ANA, administers: false, mine: flags({ read: true, send: true }), member: member(ANA), held: flags({ read: true, send: true }), linkedBy: CAROL, managers: 1 })
    expect(own.canRemove).toMatchObject({ read: true, send: true })
    expect(own.canAdd.read).toBe(false)
  })

  it('never changes the grant of the person a mailbox is linked by', () => {
    const rules = grantPermissions({ callerID: ANA, administers: true, mine: FULL_ACCESS, member: member(CAROL), held: FULL_ACCESS, linkedBy: CAROL, managers: 1 })
    expect(rules.lock).toBe('linker')
    expect(Object.values(rules.canAdd).some(Boolean)).toBe(false)
    expect(Object.values(rules.canRemove).some(Boolean)).toBe(false)
  })

  it('keeps manage with the only person who holds it', () => {
    const rules = grantPermissions({ callerID: ANA, administers: true, mine: FULL_ACCESS, member: member(BEA), held: flags({ read: true, manage: true }), linkedBy: CAROL, managers: 1 })
    expect(rules.lastManager).toBe(true)
    expect(rules.canRemove.manage).toBe(false)
    expect(rules.canRemove.read).toBe(true)
  })

  it('gives nothing to a member who is disabled, in the team or on the server', () => {
    for (const target of [member(BEA, { status: 'disabled' }), member(BEA, { person_disabled: true })]) {
      const rules = grantPermissions({ callerID: ANA, administers: true, mine: FULL_ACCESS, member: target, held: flags(), linkedBy: CAROL, managers: 1 })
      expect(rules.lock).toBe('inactive')
      expect(Object.values(rules.canAdd).some(Boolean)).toBe(false)
    }
  })

  it('ticks read with act, and unticks act with read: act never stands without read', () => {
    expect(toggleFlag(flags(), 'act', true)).toEqual(flags({ read: true, act: true }))
    expect(toggleFlag(flags({ read: true, act: true, send: true }), 'read', false)).toEqual(flags({ send: true }))
    expect(toggleFlag(flags({ read: true }), 'manage', true)).toEqual(flags({ read: true, manage: true }))
  })

  it('takes flags away with a revoke and gives them with the whole grant', () => {
    const before = flags({ read: true, act: true, send: true })
    expect(grantChange(before, before)).toEqual({ kind: 'none' })
    expect(grantChange(before, flags({ read: true }))).toEqual({ kind: 'revoke', flags: ['act', 'send'] })
    expect(grantChange(before, flags())).toEqual({ kind: 'revoke', flags: [] })
    expect(grantChange(before, flags({ read: true, act: true, manage: true }))).toEqual({ kind: 'set', flags: flags({ read: true, act: true, manage: true }) })
  })

  it('says a grant in a few words', () => {
    expect(grantSummary(FULL_ACCESS)).toBe('Full access')
    expect(grantSummary(NO_ACCESS)).toBe('No access')
    expect(grantSummary(flags({ read: true, send: true }))).toBe('Read, Send')
  })
})

describe('who may change a member of a team', () => {
  it('lets an owner change anyone’s role and status, and remove anyone', () => {
    const rules = memberPermissions(ANA, 'owner', member(BEA, { role: 'admin' }))
    expect(rules.roles).toEqual(['owner', 'admin', 'member'])
    expect(rules).toMatchObject({ canDisable: true, canRemove: true, canLeave: false, protection: '' })
  })

  it('lets an admin disable and remove members only, and make nobody an admin or an owner', () => {
    const plain = memberPermissions(ANA, 'admin', member(BEA))
    expect(plain.roles).toEqual(['member'])
    expect(plain).toMatchObject({ canDisable: true, canRemove: true })
    const admin = memberPermissions(ANA, 'admin', member(BEA, { role: 'admin' }))
    expect(admin).toMatchObject({ roles: ['admin'], canDisable: false, canRemove: false })
    expect(memberPermissions(ANA, 'admin', member(BEA, { status: 'disabled' })).canEnable).toBe(true)
  })

  it('lets a member only leave', () => {
    expect(memberPermissions(ANA, 'member', member(BEA))).toMatchObject({ roles: ['member'], canDisable: false, canRemove: false, canLeave: false })
    expect(memberPermissions(ANA, 'member', member(ANA)).canLeave).toBe(true)
  })

  it('keeps the last owner from being demoted, disabled, removed or leaving', () => {
    const other = memberPermissions(ANA, 'owner', member(BEA, { role: 'owner', last_owner: true }))
    expect(other).toMatchObject({ roles: ['owner'], canDisable: false, canRemove: false, protection: 'last-owner' })
    expect(memberPermissions(ANA, 'owner', member(ANA, { role: 'owner', last_owner: true })).canLeave).toBe(false)
  })

  it('keeps a person mailboxes there are linked by from being disabled, removed or leaving, but not from a new role', () => {
    const linker = memberPermissions(ANA, 'owner', member(BEA, { links: 2 }))
    expect(linker).toMatchObject({ canDisable: false, canRemove: false, protection: 'linker' })
    expect(linker.roles).toEqual(['owner', 'admin', 'member'])
    expect(memberPermissions(BEA, 'member', member(BEA, { links: 1 })).canLeave).toBe(false)
  })

  it('lets an owner invite any role and an admin members only', () => {
    expect(inviteRoles('owner')).toEqual(['owner', 'admin', 'member'])
    expect(inviteRoles('admin')).toEqual(['member'])
    expect(inviteRoles('member')).toEqual([])
  })

  it('shows a team’s invitations to its owners and admins, and to no member', () => {
    expect(seesInvitations('owner')).toBe(true)
    expect(seesInvitations('admin')).toBe(true)
    expect(seesInvitations('member')).toBe(false)
    expect(seesInvitations(undefined)).toBe(false)
  })
})

describe('workspaces', () => {
  it('links into the personal workspace, or a team the person owns or administers', () => {
    expect(canLinkInto({ id: 'wsp_me', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1 })).toBe(true)
    expect(canLinkInto(team('owner'))).toBe(true)
    expect(canLinkInto(team('admin'))).toBe(true)
    expect(canLinkInto(team('member'))).toBe(false)
    expect(administers(team('admin', { status: 'disabled' }))).toBe(false)
  })

  it('changes here only teams made here, and makes none where any workspace comes from elsewhere', () => {
    expect(changeableTeam(team('owner'))).toBe(true)
    expect(changeableTeam(team('owner', { source: 'platform' }))).toBe(false)
    expect(teamsCreatedHere([team('owner')])).toBe(true)
    expect(teamsCreatedHere([team('owner'), team('member', { id: 'wsp_other', source: 'platform' })])).toBe(false)
  })

  it('names a team by its name and the others in the person’s language', () => {
    expect(workspaceName(team('owner'))).toBe('Support')
    expect(workspaceName({ kind: 'personal', name: '' })).toBe('Personal')
  })

  it('offers a take-over only to whoever holds every flag, may link there and agreed to the current text of sync', () => {
    const base = { callerID: ANA, linkedBy: BEA, mine: FULL_ACCESS, workspace: team('admin'), sync: { consented: true, current: true } }
    expect(takeOverStanding(base)).toBe('available')
    expect(takeOverStanding({ ...base, linkedBy: ANA })).toBe('linker')
    expect(takeOverStanding({ ...base, mine: flags({ read: true, act: true, send: true }) })).toBe('needs-flags')
    expect(takeOverStanding({ ...base, workspace: team('member') })).toBe('needs-role')
    expect(takeOverStanding({ ...base, sync: { consented: false, current: false } })).toBe('needs-sync')
    // Agreed to an earlier text, which may not say who reads a team mailbox: it would sync under that.
    expect(takeOverStanding({ ...base, sync: { consented: true, current: false } })).toBe('needs-current-sync')
  })

  it('holds a link into a team, and only into a team, for someone who agreed to an earlier text of sync', () => {
    const earlier = { consented: true, current: false }
    expect(linkWaitsForSync(team('owner'), earlier)).toBe(true)
    expect(linkWaitsForSync({ ...team('owner'), kind: 'personal' }, earlier)).toBe(false)
    expect(linkWaitsForSync(team('owner'), { consented: true, current: true })).toBe(false)
    // Nothing syncs without an agreement, and the one they give is to the current text.
    expect(linkWaitsForSync(team('owner'), { consented: false, current: false })).toBe(false)
  })
})
