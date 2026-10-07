<script setup lang="ts">
// The open console's Members section: on a self-hosted server, people make
// their own teams (the local workspace source). In a team shown, for its
// owners and admins, the only people who see it: its people and their roles,
// changing a role, disabling or removing someone, an owner leaving while
// another owner remains, renaming it, and invitations. A member of the team
// is shown none of it (the console offers them no Members section; the
// server refuses them the list). In the personal workspace: creating a team,
// and the teams the person is in.
//
// What a person's role allows is offered, and what the team's protections
// refuse is said beside it beforehand (ui/access.ts memberPermissions): the
// last active owner keeps the team, and the last person who can read one of
// its mailboxes stays while they are. The server decides every change, in
// the transaction that makes it, and says no to whatever this gets wrong.
import { computed, onMounted, ref, useId, watch } from 'vue'
import type { Member, TeamInvite, WorkspaceRole } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import ConsoleDialog from '../components/ConsoleDialog.vue'
import { accounts } from '../state/accounts'
import type { Failure } from '../state/failure'
import { session } from '../state/session'
import { changeMember, directoryEntry, leaveTeam, loadDirectory, loadInvites, loadMembers, removeMember, revokeInvite, team } from '../state/team'
import { createTeam, currentWorkspace, loadWorkspaces, renameTeam, selectWorkspace, workspaces } from '../state/workspaces'
import {
  administers, changeableTeam, lastReaderOf, memberPermissions, seesInvitations, teamsCreatedHere, workspaceName, workspaceRoleLabel,
  type MemberPermissions,
} from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { count, dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import TeamInviteDialog from './TeamInviteDialog.vue'

const shown = computed(currentWorkspace)
/** A team made here, shown to one of its owners or admins: what this section administers. */
const inTeam = computed(() => changeableTeam(shown.value) && administers(shown.value))
/** A team made here, shown to a member of it: nothing here is theirs to see. */
const memberOnly = computed(() => changeableTeam(shown.value) && !administers(shown.value))
const teamName = computed(() => workspaceName(shown.value))
const myRole = computed(() => shown.value?.role)
const admin = computed(() => administers(shown.value))
const me = computed(() => session.user?.id ?? '')
const teams = computed(() => workspaces.list.filter(item => item.kind === 'team'))
const creatable = computed(() => teamsCreatedHere(workspaces.list))
const invites = computed(() => seesInvitations(myRole.value))

const roleRank: Record<string, number> = { owner: 0, admin: 1, member: 2 }
/** Owners, then admins, then members; the caller first among equals, then by name. */
const members = computed(() => [...team.members.list].sort((a, b) =>
  (roleRank[a.role] ?? 3) - (roleRank[b.role] ?? 3) || Number(b.user_id === me.value) - Number(a.user_id === me.value)
  || (a.name || a.email).localeCompare(b.name || b.email)))
const rules = (member: Member): MemberPermissions => memberPermissions(me.value, myRole.value, member)

/** The section's one success line. */
const done = ref('')
function say(words: string) { done.value = words; announce(words) }

// Reading, for owners and admins: the members, the invitations, and the
// directory that names the mailboxes someone is the last reader of.
function read() {
  if (!inTeam.value) return
  void loadMembers()
  void loadDirectory()
  if (invites.value) void loadInvites()
}
onMounted(() => {
  if (!inTeam.value) return
  if (!team.members.loaded && !team.members.loading) void loadMembers()
  if (!team.directory.loaded && !team.directory.loading) void loadDirectory()
  if (invites.value && !team.invites.loaded && !team.invites.loading) void loadInvites()
})
watch(() => [workspaces.currentID, myRole.value], () => { done.value = ''; read() })
/** Refresh reads the person's workspaces too: their own role may have changed, or they joined a team elsewhere. */
function refresh() {
  void loadWorkspaces()
  read()
}

// A role, changed at once from its list; set back when the server refuses.
const changing = ref('')
const rowProblems = ref<Record<string, Failure>>({})
async function setRole(member: Member, event: Event) {
  const select = event.target as HTMLSelectElement
  const role = select.value as WorkspaceRole
  if (role === member.role || changing.value) return
  changing.value = member.user_id
  delete rowProblems.value[member.user_id]
  const problem = await changeMember(member.user_id, { role })
  changing.value = ''
  if (problem) {
    rowProblems.value[member.user_id] = problem
    select.value = member.role
    return
  }
  say(t('{name} is now {role}.', { name: member.name || member.email, role: workspaceRoleLabel(role) }))
}
async function enable(member: Member) {
  if (changing.value) return
  changing.value = member.user_id
  delete rowProblems.value[member.user_id]
  const problem = await changeMember(member.user_id, { status: 'active' })
  changing.value = ''
  if (problem) { rowProblems.value[member.user_id] = problem; return }
  say(t('{name} is enabled again. Their access to mailboxes is not given back: it is given again mailbox by mailbox.', { name: member.name || member.email }))
}

// Disabling, removing and leaving: each asked first, with what it takes away.
type Confirm = { kind: 'disable' | 'remove' | 'leave'; member: Member }
const confirming = ref<Confirm | null>(null)
const confirmBusy = ref(false)
const confirmProblem = ref<Failure | null>(null)
function ask(kind: Confirm['kind'], member: Member) { confirmProblem.value = null; done.value = ''; confirming.value = { kind, member } }
function closeConfirm() { if (!confirmBusy.value) confirming.value = null }
const confirmTitle = computed(() => {
  switch (confirming.value?.kind) {
    case 'disable': return t('Disable this member?')
    case 'remove': return t('Remove this member?')
    case 'leave': return t('Leave {team}?', { team: teamName.value })
    default: return ''
  }
})
const confirmText = computed(() => {
  const target = confirming.value
  if (!target) return ''
  const name = target.member.name || target.member.email
  switch (target.kind) {
    case 'disable': return t('{name} stays listed in {team} but loses their access to every mailbox of it now, and the invitations they made for it stop working. Enabling them again gives none of it back.', { name, team: teamName.value })
    case 'remove': return t('{name} leaves {team}: their access to its mailboxes goes, invitations to it still waiting for them are deleted, and those they made stop working. To come back they need a new invitation.', { name, team: teamName.value })
    case 'leave': return t('Your access to the mailboxes of {team} goes now, and the invitations you made for it stop working. To come back you need a new invitation.', { team: teamName.value })
  }
  return ''
})
async function confirm() {
  const target = confirming.value
  if (!target || confirmBusy.value) return
  confirmBusy.value = true
  confirmProblem.value = null
  const name = target.member.name || target.member.email
  const problem = target.kind === 'disable' ? await changeMember(target.member.user_id, { status: 'disabled' })
    : target.kind === 'remove' ? await removeMember(target.member.user_id)
      : await leaveTeam()
  confirmBusy.value = false
  if (problem) { confirmProblem.value = problem; return }
  confirming.value = null
  if (target.kind === 'disable') say(t('{name} is disabled in {team}.', { name, team: teamName.value }))
  else if (target.kind === 'remove') say(t('{name} was removed from {team}.', { name, team: teamName.value }))
}

/** The addresses of the team's mailboxes a member alone can read, as far as this page knows them. */
function onlyReads(member: Member): string {
  return lastReaderOf(member)
    .map(id => accounts.list.find(item => item.id === id)?.email ?? directoryEntry(id)?.email ?? '')
    .filter(Boolean).join(', ')
}

/** Why a member's place cannot be disabled or removed, said beside them. */
function protectionText(member: Member, permissions: MemberPermissions): string {
  const self = member.user_id === me.value
  if (permissions.protection === 'last-owner') {
    return self ? t('You are the team’s only owner: make another member an owner before you leave or step down.')
      : t('The team’s only owner: another member must be made an owner first.')
  }
  if (permissions.protection === 'last-reader') {
    const mailboxes = onlyReads(member) || t('{count} of its mailboxes', { count: count(lastReaderOf(member).length) })
    return self ? t('You are the only person who can read {mailboxes}: give someone else Read before you leave.', { mailboxes })
      : t('The only person who can read {mailboxes}: give someone else Read before disabling or removing them.', { mailboxes })
  }
  return ''
}

// Creating and renaming a team.
const naming = ref<'' | 'create' | 'rename'>('')
const name = ref('')
const nameBusy = ref(false)
const nameProblem = ref<Failure | null>(null)
const nameID = useId()
function openNaming(kind: 'create' | 'rename') {
  naming.value = kind
  name.value = kind === 'rename' ? shown.value?.name ?? '' : ''
  nameProblem.value = null
  done.value = ''
}
function closeNaming() { if (!nameBusy.value) naming.value = '' }
async function saveName() {
  const value = name.value.trim()
  if (!value || nameBusy.value) return
  nameBusy.value = true
  nameProblem.value = null
  const kind = naming.value
  const id = shown.value?.id ?? ''
  const problem = kind === 'create' ? await createTeam(value) : await renameTeam(id, value)
  nameBusy.value = false
  if (problem) { nameProblem.value = problem; return }
  naming.value = ''
  say(kind === 'create' ? t('{team} is created, with you as its owner. Invite people to it from here.', { team: value }) : t('The team is now called {team}.', { team: value }))
}

// Invitations.
const inviting = ref(false)
const revoking = ref<TeamInvite | null>(null)
const revokeBusy = ref(false)
const revokeProblem = ref<Failure | null>(null)
function askRevoke(invite: TeamInvite) { revokeProblem.value = null; done.value = ''; revoking.value = invite }
function closeRevoke() { if (!revokeBusy.value) revoking.value = null }
async function confirmRevoke() {
  const invite = revoking.value
  if (!invite || revokeBusy.value) return
  revokeBusy.value = true
  revokeProblem.value = await revokeInvite(invite.id)
  revokeBusy.value = false
  if (revokeProblem.value) return
  revoking.value = null
  say(t('The invitation for {email} was revoked: its link no longer works.', { email: invite.email }))
}

const heading = ref<HTMLElement | null>(null)
const headingTarget = () => heading.value
</script>

<template>
  <div class="console-section members-section">
    <!-- Said by the live region (ui/announce.ts) as it appears. -->
    <p v-if="done" class="success"><AppIcon name="check" :size="18" /><span>{{ done }}</span></p>

    <template v-if="inTeam">
      <section class="team-card" :aria-labelledby="`${nameID}-team`">
        <span class="provider-tile"><AppIcon name="users" :size="21" /></span>
        <div class="grow">
          <h2 :id="`${nameID}-team`" ref="heading" tabindex="-1">{{ teamName }}</h2>
          <p class="dim">{{ t('Your role: {role}', { role: workspaceRoleLabel(myRole) }) }}</p>
        </div>
        <div class="team-actions">
          <button v-if="admin" class="ghost small" type="button" aria-haspopup="dialog" @click="openNaming('rename')"><AppIcon name="pencil" :size="15" />{{ t('Rename…') }}</button>
          <button v-if="invites" class="primary small" type="button" aria-haspopup="dialog" @click="inviting = true; done = ''"><AppIcon name="plus" :size="16" />{{ t('Invite someone') }}</button>
        </div>
      </section>

      <div class="section-title">
        <h2>{{ t('Members') }}</h2>
        <div class="section-actions"><button class="ghost small" type="button" :disabled="team.members.loading" @click="refresh"><AppIcon name="refresh" :size="16" />{{ t('Refresh') }}</button></div>
      </div>
      <div v-if="team.members.failure" class="alert with-action" role="alert"><span>{{ describe(team.members.failure) }}</span><button class="ghost small" type="button" @click="loadMembers">{{ t('Try again') }}</button></div>
      <p v-else-if="!team.members.loaded" class="dim" role="status">{{ t('Loading the members…') }}</p>
      <ul v-else class="member-list" :aria-label="t('Members')" :aria-busy="team.members.loading">
        <li v-for="member in members" :key="member.user_id" class="member-row" :data-user="member.user_id">
          <div class="who">
            <strong>{{ member.name || member.email }}</strong>
            <small>{{ member.email }}</small>
            <span class="badges">
              <span v-if="member.user_id === me" class="pill">{{ t('You') }}</span>
              <span v-if="member.last_owner" class="pill">{{ t('Only owner') }}</span>
              <span v-if="lastReaderOf(member).length" class="pill">{{ t('Only reader: {count}', { count: count(lastReaderOf(member).length) }) }}</span>
              <span v-if="member.status === 'disabled'" class="pill bad">{{ t('Disabled') }}</span>
              <span v-if="member.person_disabled" class="pill bad">{{ t('Disabled on this server') }}</span>
            </span>
          </div>
          <div class="member-controls">
            <label v-if="rules(member).roles.length > 1" class="role-select">
              <span class="visually-hidden">{{ t('Role of {name}', { name: member.name || member.email }) }}</span>
              <select :name="`role-${member.user_id}`" :value="member.role" :disabled="!!changing" @change="setRole(member, $event)">
                <option v-for="role in rules(member).roles" :key="role" :value="role" :selected="role === member.role">{{ workspaceRoleLabel(role) }}</option>
              </select>
            </label>
            <span v-else class="role-label">{{ workspaceRoleLabel(member.role) }}</span>
            <button v-if="rules(member).canDisable" class="ghost small" type="button" aria-haspopup="dialog" :disabled="!!changing" @click="ask('disable', member)">{{ t('Disable…') }}</button>
            <button v-if="rules(member).canEnable" class="ghost small" type="button" :disabled="!!changing" @click="enable(member)">{{ changing === member.user_id ? t('Enabling…') : t('Enable') }}</button>
            <button v-if="rules(member).canRemove" class="ghost small remove" type="button" aria-haspopup="dialog" :disabled="!!changing" @click="ask('remove', member)">{{ t('Remove…') }}</button>
            <button v-if="rules(member).canLeave" class="ghost small remove" type="button" aria-haspopup="dialog" :disabled="!!changing" @click="ask('leave', member)">{{ t('Leave the team…') }}</button>
          </div>
          <p v-if="protectionText(member, rules(member))" class="hint lock"><AppIcon name="lock" :size="14" />{{ protectionText(member, rules(member)) }}</p>
          <p v-if="rowProblems[member.user_id]" class="alert" role="alert">{{ describe(rowProblems[member.user_id]!) }}</p>
        </li>
      </ul>

      <template v-if="invites">
        <div class="section-title"><h2>{{ t('Pending invitations') }}</h2></div>
        <div v-if="team.invites.failure" class="alert with-action" role="alert"><span>{{ describe(team.invites.failure) }}</span><button class="ghost small" type="button" @click="loadInvites">{{ t('Try again') }}</button></div>
        <p v-else-if="!team.invites.loaded" class="dim" role="status">{{ t('Loading the invitations…') }}</p>
        <p v-else-if="!team.invites.list.length" class="dim">{{ t('No invitation is waiting. A link you make is shown once, when you make it.') }}</p>
        <ul v-else class="member-list" :aria-label="t('Pending invitations')">
          <li v-for="invite in team.invites.list" :key="invite.id" class="member-row">
            <div class="who"><strong>{{ invite.email }}</strong><small>{{ t('{role} · expires on {date}', { role: workspaceRoleLabel(invite.role), date: dayStamp(invite.expires_at) }) }}</small></div>
            <div class="member-controls"><button class="ghost small remove" type="button" aria-haspopup="dialog" @click="askRevoke(invite)">{{ t('Revoke…') }}</button></div>
          </li>
        </ul>
        <p v-if="myRole === 'admin'" class="hint">{{ t('As an admin, you see and make invitations for members only.') }}</p>
      </template>

      <p class="hint">{{ t('Being in the team gives nobody access to its mailboxes: owners and admins give it mailbox by mailbox, from each one’s card in Mailboxes, and give Read only on a mailbox they read themselves. They manage every mailbox of the team by their role, and read none by it. Changing someone’s role or status, or removing them, ends the invitations they made.') }}</p>
      <div v-if="creatable" class="team-more"><button class="ghost small" type="button" aria-haspopup="dialog" @click="openNaming('create')"><AppIcon name="plus" :size="16" />{{ t('Create another team…') }}</button></div>
    </template>

    <p v-else-if="memberOnly" class="note">{{ t('The people of {team}, and who can use each of its mailboxes, are managed by its owners and admins.', { team: teamName }) }}</p>

    <template v-else>
      <section class="team-card personal">
        <span class="provider-tile"><AppIcon name="user" :size="21" /></span>
        <div class="grow">
          <h2 ref="heading" tabindex="-1">{{ t('Your personal workspace') }}</h2>
          <p class="dim">{{ t('Only you are in it, and the mailboxes you connect here are yours alone. To share mailboxes with other people on this server, create a team: you become its owner and invite them.') }}</p>
        </div>
        <div v-if="creatable" class="team-actions"><button class="primary small" type="button" aria-haspopup="dialog" @click="openNaming('create')"><AppIcon name="plus" :size="16" />{{ t('Create a team…') }}</button></div>
      </section>
      <template v-if="teams.length">
        <div class="section-title"><h2>{{ t('Your teams') }}</h2></div>
        <ul class="member-list" :aria-label="t('Your teams')">
          <li v-for="item in teams" :key="item.id" class="member-row">
            <div class="who"><strong>{{ workspaceName(item) }}</strong><small>{{ workspaceRoleLabel(item.role) }}</small></div>
            <div class="member-controls"><button class="ghost small" type="button" @click="selectWorkspace(item.id)">{{ t('Show') }}</button></div>
          </li>
        </ul>
      </template>
    </template>

    <ConsoleDialog v-if="naming" :title="naming === 'create' ? t('Create a team') : t('Rename the team')" :busy="nameBusy" :return-focus="headingTarget" @close="closeNaming">
      <form class="form-stack" name="mailie-team-name" autocomplete="off" @submit.prevent="saveName">
        <p v-if="naming === 'create'" class="dim">{{ t('You become the team’s owner. Its people, and who has access to its mailboxes, are chosen here.') }}</p>
        <label :for="nameID">{{ t('Name') }}<input :id="nameID" v-model="name" name="team-name" maxlength="80" required autocomplete="off" :disabled="nameBusy" :placeholder="t('For example: Support')" /></label>
        <p v-if="nameProblem" class="alert" role="alert">{{ describe(nameProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="nameBusy" @click="closeNaming">{{ t('Cancel') }}</button>
          <button class="primary" type="submit" :disabled="nameBusy || !name.trim()">{{ nameBusy ? t('Saving…') : naming === 'create' ? t('Create the team') : t('Save name') }}</button>
        </div>
      </form>
    </ConsoleDialog>

    <ConsoleDialog v-if="confirming" :title="confirmTitle" :busy="confirmBusy" :return-focus="headingTarget" @close="closeConfirm">
      <div class="form-stack">
        <p class="dim">{{ confirmText }}</p>
        <p v-if="confirmProblem" class="alert" role="alert">{{ describe(confirmProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="confirmBusy" @click="closeConfirm">{{ t('Cancel') }}</button>
          <button class="danger" type="button" :disabled="confirmBusy" @click="confirm">{{ confirmBusy ? t('Saving…') : confirming.kind === 'disable' ? t('Disable') : confirming.kind === 'remove' ? t('Remove') : t('Leave the team') }}</button>
        </div>
      </div>
    </ConsoleDialog>

    <ConsoleDialog v-if="revoking" :title="t('Revoke this invitation?')" :busy="revokeBusy" :return-focus="headingTarget" @close="closeRevoke">
      <div class="form-stack">
        <p class="dim">{{ t('The link sent to {email} stops working at once.', { email: revoking.email }) }}</p>
        <p v-if="revokeProblem" class="alert" role="alert">{{ describe(revokeProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="revokeBusy" @click="closeRevoke">{{ t('Cancel') }}</button>
          <button class="danger" type="button" :disabled="revokeBusy" @click="confirmRevoke">{{ revokeBusy ? t('Revoking…') : t('Revoke invitation') }}</button>
        </div>
      </div>
    </ConsoleDialog>

    <TeamInviteDialog v-if="inviting" :return-focus="headingTarget" @close="inviting = false" />
  </div>
</template>

<style scoped>
.console-section { display: grid; gap: 18px; }
.success { margin: 0; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.team-card { display: flex; align-items: flex-start; gap: 14px; flex-wrap: wrap; padding: 20px; border: 1px solid var(--console-border); border-radius: 12px; background: var(--bg-raised); }
/* The name keeps a readable width; the buttons wrap below it rather than squeeze it. */
.team-card .grow { flex: 1 1 220px; min-width: 0; }
.team-card h2 { margin: 0; font-size: 17px; font-weight: 650; overflow-wrap: anywhere; }
.team-card h2:focus { outline: none; }
.team-card p { margin: 6px 0 0; line-height: 1.55; }
.team-card.personal .provider-tile { background: var(--bg-hover); color: var(--text-dim); }
.team-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.team-actions button, .team-more button, .section-actions button { display: inline-flex; align-items: center; gap: 6px; }
.section-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 6px; flex-wrap: wrap; }
.section-title h2 { font-size: 15px; margin: 0; font-weight: 600; }
.section-actions { display: flex; gap: 8px; }
.member-list { list-style: none; margin: 0; padding: 0; border: 1px solid var(--console-border); border-radius: 12px; overflow: hidden; background: var(--bg-panel); }
.member-row { display: flex; flex-wrap: wrap; align-items: center; gap: 10px 16px; padding: 14px 18px; border-top: 1px solid var(--console-border); }
.member-row:first-child { border-top: 0; }
.who { flex: 1 1 240px; min-width: 0; }
.who strong { display: block; font-size: 14px; font-weight: 600; overflow-wrap: anywhere; }
.who small { display: block; margin-top: 3px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; }
.badges { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; }
.badges:empty { display: none; }
.pill.bad { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 45%, transparent); }
.member-controls { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.role-select select { min-height: 36px; padding: 6px 10px; border: 1px solid var(--console-border); border-radius: 8px; background: var(--bg-input); color: var(--text); font: inherit; font-size: 13px; }
.role-label { font-size: 13px; color: var(--text-dim); }
.remove { color: var(--danger); }
.member-row .hint, .member-row .alert { flex-basis: 100%; margin: 0; }
.lock { display: flex; align-items: flex-start; gap: 6px; }
.lock .app-icon { flex: none; margin-top: 2px; }
.members-section > .hint, .members-section > .dim { margin: 0; }
.form-stack p { margin: 0; }
@media (max-width: 760px) {
  .member-row { padding: 14px; }
  .member-controls { width: 100%; }
  .member-controls button { flex: 1; }
  .role-select select { font-size: 16px; min-height: 44px; }
}
</style>
