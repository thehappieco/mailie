// What the console offers and explains about workspaces before anyone tries
// (ui/access.ts): the server decides every change, and these rules only have
// to agree with it (docs/workspaces.md, "Who may do what", "Grant rules" and
// "Protections").
import { describe, expect, it } from 'vitest'
import type { GrantFlags, MailboxConsent, Member, Workspace } from '../src/api/types'
import {
  actorName, administers, canLinkInto, changeableTeam, grantChange, grantPermissions, grantSummary, inviteRoles, managesByRole, memberPermissions,
  NO_ACCESS, seesInvitations, teamsCreatedHere, teamSyncStanding, toggleFlag, workspaceName, FULL_ACCESS, type GrantContext,
} from '../src/ui/access'

const ANA = 'usr_ana'
const BEA = 'usr_bea'
const MAILBOX = 'acc_shared'

function member(userID: string, fields: Partial<Member> = {}): Member {
  return { user_id: userID, email: `${userID}@example.test`, name: '', role: 'member', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1_790_000_000, ...fields }
}

function team(role: string, fields: Partial<Workspace> = {}): Workspace {
  return { id: 'wsp_team', kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000, ...fields }
}

const flags = (fields: Partial<GrantFlags> = {}): GrantFlags => ({ ...NO_ACCESS, ...fields })
/** An owner or an admin who reads the mailbox, changing a member's grant they hold nothing of. */
const context = (fields: Partial<GrantContext> = {}): GrantContext =>
  ({ administers: true, mine: flags({ read: true }), member: member(BEA), held: flags(), lastReader: false, ...fields })
const nothing = { read: false, act: false, send: false, manage: false }

describe('who may give what on a team mailbox', () => {
  it('lets an owner or an admin who reads it give Read, and turn on Act and Send without holding them', () => {
    const rules = grantPermissions(context())
    expect(rules.canAdd).toEqual({ read: true, act: true, send: true, manage: true })
  })

  it('lets an owner or an admin who does not read it give no Read, not even to themselves, but Act, Send and Manage', () => {
    const rules = grantPermissions(context({ mine: flags() }))
    expect(rules.canAdd).toEqual({ read: false, act: true, send: true, manage: true })
    const self = grantPermissions(context({ mine: flags(), member: member(ANA, { role: 'admin' }) }))
    expect(self.canAdd.read).toBe(false)
    expect(self.canAdd.send).toBe(true)
  })

  it('gives Manage to members only: owners and admins manage every mailbox of the team by their role', () => {
    for (const role of ['owner', 'admin']) {
      const rules = grantPermissions(context({ member: member(BEA, { role }) }))
      expect(rules.byRole, role).toBe(true)
      expect(rules.canAdd.manage, role).toBe(false)
    }
    expect(grantPermissions(context()).byRole).toBe(false)
    expect(managesByRole('owner') && managesByRole('admin') && !managesByRole('member')).toBe(true)
  })

  it('gives a member nothing to give or take, not even their own flags', () => {
    const own = grantPermissions(context({ administers: false, mine: flags({ read: true, send: true }), member: member(ANA), held: flags({ read: true, send: true }) }))
    expect(own.canAdd).toEqual(nothing)
    expect(own.canRemove).toEqual(nothing)
  })

  it('lets an owner or an admin take any flag away, their own included, but the last reader’s Read', () => {
    const rules = grantPermissions(context({ member: member(ANA, { role: 'owner' }), held: flags({ read: true, act: true, send: true }) }))
    expect(rules.canRemove).toEqual({ read: true, act: true, send: true, manage: true })
    const last = grantPermissions(context({ held: flags({ read: true, act: true }), lastReader: true }))
    expect(last.lastReader).toBe(true)
    expect(last.canRemove.read).toBe(false)
    expect(last.canRemove.act).toBe(true)
  })

  it('gives nothing to a member who is disabled, in the team or on the server', () => {
    for (const target of [member(BEA, { status: 'disabled' }), member(BEA, { person_disabled: true })]) {
      const rules = grantPermissions(context({ mine: FULL_ACCESS, member: target }))
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
    // An owner's card of a mailbox they hold no grant on: manage, by their role.
    expect(grantSummary(flags({ manage: true }))).toBe('Manage')
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

  it('lets only an owner leave, while another owner remains: neither a member nor an admin leaves by themselves', () => {
    expect(memberPermissions(ANA, 'member', member(BEA))).toMatchObject({ roles: ['member'], canDisable: false, canRemove: false, canLeave: false })
    expect(memberPermissions(ANA, 'member', member(ANA)).canLeave).toBe(false)
    expect(memberPermissions(ANA, 'admin', member(ANA, { role: 'admin' })).canLeave).toBe(false)
    expect(memberPermissions(ANA, 'owner', member(ANA, { role: 'owner' })).canLeave).toBe(true)
  })

  it('keeps the last owner from being demoted, disabled, removed or leaving', () => {
    const other = memberPermissions(ANA, 'owner', member(BEA, { role: 'owner', last_owner: true }))
    expect(other).toMatchObject({ roles: ['owner'], canDisable: false, canRemove: false, protection: 'last-owner' })
    expect(memberPermissions(ANA, 'owner', member(ANA, { role: 'owner', last_owner: true })).canLeave).toBe(false)
  })

  it('keeps the last reader of a mailbox from being disabled, removed or leaving, but not from a new role', () => {
    const reader = memberPermissions(ANA, 'owner', member(BEA, { last_reader_of: [MAILBOX] }))
    expect(reader).toMatchObject({ canDisable: false, canRemove: false, protection: 'last-reader' })
    expect(reader.roles).toEqual(['owner', 'admin', 'member'])
    expect(memberPermissions(ANA, 'owner', member(ANA, { role: 'owner', last_reader_of: [MAILBOX] })).canLeave).toBe(false)
    // A daemon older than the rule does not say: nobody is protected for it, and the server decides.
    expect(memberPermissions(ANA, 'owner', member(BEA, { last_reader_of: undefined })).protection).toBe('')
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

describe('a team mailbox’s agreement to sync', () => {
  const consent = (fields: Partial<MailboxConsent>): MailboxConsent => ({ enabled: false, current: false, ...fields })

  it('says whether it stands, and whether to the current text, an earlier one, or the one the upgrade carried over', () => {
    expect(teamSyncStanding(undefined)).toBe('off')
    expect(teamSyncStanding(consent({}))).toBe('off')
    expect(teamSyncStanding(consent({ enabled: true, enabled_at: 1, version: 'v3', current: true }))).toBe('on')
    expect(teamSyncStanding(consent({ enabled: true, enabled_at: 1, version: 'v2' }))).toBe('on-earlier')
    // Tied to whoever linked it, whatever its revision says, until the team gives it.
    expect(teamSyncStanding(consent({ enabled: true, enabled_at: 1, version: 'v3', current: true, migrated: true }))).toBe('migrated')
    expect(teamSyncStanding(consent({ enabled_by: BEA, migrated: true }))).toBe('kept')
  })

  it('names who gave it: a person of the team, a key, the command line, or nobody once that person was deleted', () => {
    const names = (id: string) => id === BEA ? 'Bea Lima' : ''
    expect(actorName(BEA, names)).toBe('Bea Lima')
    expect(actorName('usr_gone', names)).toBe('someone no longer in the team')
    expect(actorName('key:0a1b2c', names)).toBe('an API key')
    expect(actorName('cli', names)).toBe('the command line')
    expect(actorName(undefined, names)).toBe('')
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
})
