<script setup lang="ts">
// Who in a team can use one of its mailboxes, and changing it
// (docs/workspaces.md, "Who may change a grant"), for an owner or an admin of
// the team and for whoever manages the mailbox. Each person's four flags are
// ticked and then saved; what the caller may not change is shown as such
// beforehand, with why: read, act and send pass only from someone who holds
// them (so owners and admins hand out no read they do not have), the person
// the mailbox is linked by keeps all four while it is, and a mailbox keeps
// someone who manages it. Someone who only uses the mailbox sees their own
// access, and may give it up. Below, taking the link over, where the caller
// may, after saying what it changes, and once they agreed to the current text
// of mail sync, which it would sync under. The server decides every change.
import { computed, onMounted, ref } from 'vue'
import type { Account, GrantFlags, Member } from '../api/types'
import type { Failure } from '../state/failure'
import { session } from '../state/session'
import { syncStanding } from '../state/sync'
import { directoryEntry, loadDirectory, loadMembers, personName, saveGrant, takeOverLink, team } from '../state/team'
import { currentWorkspace } from '../state/workspaces'
import {
  accessOf, administers, flagHint, flagLabel, flagNames, flagsOf, grantPermissions, grantSummary, holdsAny, NO_ACCESS, sameFlags,
  takeOverStanding, toggleFlag, workspaceName, workspaceRoleLabel, type FlagName, type GrantPermissions,
} from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { anyOf } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import SyncRenewal from './SyncRenewal.vue'

// account: the caller's own card of the mailbox, when they hold a grant on it.
const props = defineProps<{ accountId: string; email: string; account?: Account }>()

interface Row { member: Member; held: GrantFlags; rules: GrantPermissions }

const me = computed(() => session.user?.id ?? '')
const workspace = computed(currentWorkspace)
const teamName = computed(() => workspaceName(workspace.value))
const admin = computed(() => administers(workspace.value))
const entry = computed(() => directoryEntry(props.accountId))
/** Opened from the team's list, and gone from it since: the mailbox was removed. */
const gone = computed(() => !props.account && admin.value && team.directory.loaded && !entry.value)
const linkedBy = computed(() => entry.value?.linked_by ?? props.account?.linked_by)
const grantOf = (userID: string): GrantFlags => {
  if (entry.value) return flagsOf(entry.value.grants.find(grant => grant.user_id === userID))
  return userID === me.value && props.account ? accessOf(props.account) : { ...NO_ACCESS }
}
const mine = computed(() => grantOf(me.value))
/** The caller administers the team or manages the mailbox: they see and change everyone's access. */
const manages = computed(() => admin.value || mine.value.manage)
const managers = computed(() => entry.value?.grants.filter(grant => grant.manage).length ?? 0)
const linkerName = computed(() => personName(linkedBy.value) || t('Another member'))
const linkerLine = computed(() => {
  if (!linkedBy.value) return ''
  return linkedBy.value === me.value
    ? t('Linked by you: it syncs under your agreement to sync.')
    : t('Linked by {name}: it syncs under their agreement to sync.', { name: linkerName.value })
})

function rank(row: Row): number {
  if (row.member.user_id === linkedBy.value) return 0
  if (row.member.user_id === me.value) return 1
  return holdsAny(row.held) ? 2 : 3
}

/** Everyone who can be given access, or holds some: the person the mailbox is linked by first, then the caller. */
const rows = computed<Row[]>(() => {
  const members = manages.value ? team.members.list : team.members.list.filter(member => member.user_id === me.value)
  return members
    .map(member => {
      const held = grantOf(member.user_id)
      const rules = grantPermissions({ callerID: me.value, administers: admin.value, mine: mine.value, member, held, linkedBy: linkedBy.value, managers: managers.value })
      return { member, held, rules }
    })
    .filter(row => row.rules.lock !== 'inactive')
    .sort((a, b) => rank(a) - rank(b) || (a.member.name || a.member.email).localeCompare(b.member.name || b.member.email))
})

/** What the caller ticked for each person, until it is saved or set back. */
const drafts = ref<Record<string, GrantFlags>>({})
const saving = ref('')
const problems = ref<Record<string, Failure>>({})

const shown = (row: Row): GrantFlags => drafts.value[row.member.user_id] ?? row.held
const changed = (row: Row): boolean => !sameFlags(shown(row), row.held)

/** Whether the caller may tick or untick this flag for this person, from what is ticked now. */
function editable(row: Row, flag: FlagName): boolean {
  if (row.rules.lock || saving.value) return false
  const flags = shown(row)
  // Setting back what this draft changed is always possible.
  if (flags[flag] !== row.held[flag]) return true
  if (flags[flag]) return row.rules.canRemove[flag]
  if (!row.rules.canAdd[flag]) return false
  // Act comes with read: the read it needs must be there, or be the caller's to give.
  return flag !== 'act' || flags.read || row.rules.canAdd.read
}

function toggle(row: Row, flag: FlagName, event: Event) {
  const on = (event.target as HTMLInputElement).checked
  const next = toggleFlag(shown(row), flag, on)
  const id = row.member.user_id
  delete problems.value[id]
  if (sameFlags(next, row.held)) delete drafts.value[id]
  else drafts.value[id] = next
}

function setBack(row: Row) {
  delete drafts.value[row.member.user_id]
  delete problems.value[row.member.user_id]
}

async function save(row: Row) {
  const id = row.member.user_id
  const draft = drafts.value[id]
  if (!draft || saving.value) return
  saving.value = id
  const problem = await saveGrant(props.accountId, id, row.held, draft)
  saving.value = ''
  if (problem) { problems.value[id] = problem; return }
  delete drafts.value[id]
  announce(t('Access saved.'))
}

/** Why a row, or one of its flags, cannot change: said beside it, before anyone tries. */
function lockText(row: Row): string {
  if (row.rules.lock === 'linker') {
    return row.member.user_id === me.value
      ? t('You linked this mailbox: it syncs under your agreement, so your access stays complete while it is linked.')
      : t('Linked this mailbox: it syncs under their agreement, so their access stays complete while it is linked.')
  }
  if (row.rules.lastManager) return t('The only person who manages this mailbox: give Manage to someone else before taking it away.')
  return ''
}

/** What the caller cannot give, said once for every row. */
const giving = computed(() => {
  if (!manages.value) return ''
  const missing = flagNames.filter(flag => flag !== 'manage' && !mine.value[flag]).map(flagLabel)
  return missing.length ? t('You can give only what you hold on this mailbox yourself: not {flags}.', { flags: anyOf(missing) }) : ''
})

// Taking the link over.
const standing = computed(() => takeOverStanding({
  callerID: me.value, linkedBy: linkedBy.value, mine: mine.value, workspace: workspace.value, sync: syncStanding(),
}))
const taking = ref(false)
const takingOver = ref(false)
const takeProblem = ref<Failure | null>(null)
function askTakeOver() { takeProblem.value = null; taking.value = true }
function closeTakeOver() { if (!takingOver.value) taking.value = false }
async function takeOver() {
  if (takingOver.value) return
  takingOver.value = true
  takeProblem.value = await takeOverLink(props.accountId)
  takingOver.value = false
  if (takeProblem.value) return
  taking.value = false
  announce(t('You took over the link of {email}. It syncs under your agreement now.', { email: props.email }))
}
const standingText = computed(() => {
  switch (standing.value) {
    case 'needs-flags': return t('To take the link over you need Read, Act, Send and Manage on this mailbox.')
    case 'needs-role': return t('Only owners and admins of the team can take a link over, as only they link mailboxes into it.')
    case 'needs-sync': return t('Turn on mail sync first: the mailbox would sync under your agreement.')
    case 'needs-current-sync': return t('Agree to the current text of mail sync first: the mailbox would sync under your agreement, which was to an earlier text.')
    default: return ''
  }
})

function refresh() {
  void loadMembers()
  if (manages.value) void loadDirectory()
}

onMounted(() => {
  if (!team.members.loaded && !team.members.loading) void loadMembers()
  if (manages.value && !team.directory.loaded && !team.directory.loading) void loadDirectory()
})
</script>

<template>
  <div class="access-panel">
    <p v-if="manages" class="hint">{{ t('Who in {team} can use this mailbox. Each person needs their own access: being an owner or an admin of the team gives none.', { team: teamName }) }}</p>
    <p v-if="linkerLine" class="access-linker"><AppIcon name="shield" :size="16" /><span>{{ linkerLine }}</span></p>
    <p v-if="giving" class="hint">{{ giving }}</p>

    <div v-if="team.members.failure || team.directory.failure" class="alert with-action" role="alert">
      <span>{{ describe((team.members.failure ?? team.directory.failure)!) }}</span>
      <button class="ghost small" type="button" @click="refresh">{{ t('Try again') }}</button>
    </div>
    <p v-else-if="!team.members.loaded || (manages && !team.directory.loaded)" class="dim" role="status">{{ t('Reading who has access…') }}</p>
    <p v-else-if="gone" class="note" role="status">{{ t('{email} is no longer a mailbox of {team}: it was removed.', { email, team: teamName }) }}</p>

    <ul v-else class="access-rows" :aria-label="t('Access to {email}', { email })">
      <li v-for="row in rows" :key="row.member.user_id" class="access-row" :data-user="row.member.user_id">
        <div class="who">
          <strong>{{ row.member.user_id === me ? t('You') : row.member.name || row.member.email }}</strong>
          <small>{{ row.member.email }} · {{ workspaceRoleLabel(row.member.role) }}</small>
        </div>
        <fieldset class="flags" :disabled="!!row.rules.lock">
          <legend class="visually-hidden">{{ t('Access of {name}', { name: row.member.name || row.member.email }) }}</legend>
          <label v-for="flag in flagNames" :key="flag" class="flag" :title="flagHint(flag)">
            <input type="checkbox" :name="`${row.member.user_id}-${flag}`" :value="flag" :checked="shown(row)[flag]" :disabled="!editable(row, flag)" @change="toggle(row, flag, $event)" />
            <span>{{ flagLabel(flag) }}</span>
          </label>
        </fieldset>
        <p v-if="lockText(row)" class="hint lock"><AppIcon name="lock" :size="14" />{{ lockText(row) }}</p>
        <p v-if="problems[row.member.user_id]" class="alert" role="alert">{{ describe(problems[row.member.user_id]!) }}</p>
        <div v-if="changed(row)" class="row-save">
          <span class="dim">{{ t('Now: {flags}', { flags: grantSummary(row.held) }) }}</span>
          <button class="ghost small" type="button" :disabled="!!saving" @click="setBack(row)">{{ t('Cancel') }}</button>
          <button class="primary small" type="button" :disabled="!!saving" @click="save(row)">{{ saving === row.member.user_id ? t('Saving…') : t('Save access') }}</button>
        </div>
      </li>
    </ul>

    <dl class="flag-legend">
      <div v-for="flag in flagNames" :key="flag"><dt>{{ flagLabel(flag) }}</dt><dd>{{ flagHint(flag) }}</dd></div>
    </dl>

    <section v-if="manages && standing !== 'linker' && !gone" class="take-over" :aria-label="t('Take over the link')">
      <p class="dim">{{ t('It syncs under the agreement of {name}: turning their sync off deletes its index, and they cannot leave the team while it is linked. Taking the link over moves it, with its index, under your agreement.', { name: linkerName }) }}</p>
      <SyncRenewal v-if="standing === 'needs-current-sync'" :note="standingText" />
      <p v-else-if="standingText" class="hint">{{ standingText }}</p>
      <button class="ghost small" type="button" aria-haspopup="dialog" :disabled="standing !== 'available'" @click="askTakeOver">{{ t('Take over the link…') }}</button>
    </section>

    <ConsoleDialog v-if="taking" :title="t('Take over this link?')" :busy="takingOver" @close="closeTakeOver">
      <div class="form-stack take-over-confirm">
        <ul>
          <li>{{ t('{email} syncs under your agreement to sync from now on, instead of {name}’s. Its index is kept.', { email, name: linkerName }) }}</li>
          <li>{{ t('If you turn sync off, its index is deleted, for everyone in {team} who reads it.', { team: teamName }) }}</li>
          <li>{{ t('{name} keeps their access as an ordinary member: it can then be changed, and they can leave the team.', { name: linkerName }) }}</li>
        </ul>
        <p v-if="takeProblem" class="alert" role="alert">{{ describe(takeProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="takingOver" @click="closeTakeOver">{{ t('Cancel') }}</button>
          <button class="primary" type="button" :disabled="takingOver" @click="takeOver">{{ takingOver ? t('Taking over…') : t('Take over the link') }}</button>
        </div>
      </div>
    </ConsoleDialog>
  </div>
</template>

<style scoped>
.access-panel { display: grid; gap: 12px; }
.access-panel p { margin: 0; }
.access-linker { display: flex; align-items: flex-start; gap: 8px; font-size: 13px; line-height: 1.5; color: var(--text); }
.access-linker .app-icon { flex: none; margin-top: 2px; color: var(--accent); }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.access-rows { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
.access-row { display: grid; gap: 8px; padding: 12px 14px; border-top: 1px solid var(--line); }
.access-row:first-child { border-top: 0; }
.who { min-width: 0; }
.who strong { display: block; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
.who small { display: block; margin-top: 2px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; }
.flags { display: flex; flex-wrap: wrap; gap: 8px; margin: 0; padding: 0; border: 0; min-width: 0; }
.flag { display: inline-flex; align-items: center; gap: 7px; min-height: 36px; padding: 6px 12px; border: 1px solid var(--line); border-radius: 999px; background: var(--bg-raised); font-size: 12.5px; cursor: pointer; }
.flag:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); font-weight: 600; }
.flag:has(input:disabled) { cursor: default; opacity: .65; }
.flag input { width: auto; min-height: 0; margin: 0; accent-color: var(--accent); }
.lock { display: flex; align-items: flex-start; gap: 6px; }
.lock .app-icon { flex: none; margin-top: 2px; }
.row-save { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; }
.row-save .dim { margin-right: auto; font-size: 12px; }
.flag-legend { display: grid; gap: 6px; margin: 0; font-size: 12px; line-height: 1.45; }
.flag-legend div { display: grid; grid-template-columns: 70px minmax(0, 1fr); gap: 10px; }
.flag-legend dt { font-weight: 600; }
.flag-legend dd { margin: 0; color: var(--text-dim); }
.take-over { display: grid; gap: 8px; justify-items: start; padding-top: 12px; border-top: 1px solid var(--line); }
.take-over-confirm ul { margin: 0; padding-left: 20px; display: grid; gap: 8px; font-size: 13px; line-height: 1.55; }
@media (max-width: 600px) {
  .flag { flex: 1 1 calc(50% - 8px); justify-content: center; }
  .row-save button { flex: 1; }
}
</style>
