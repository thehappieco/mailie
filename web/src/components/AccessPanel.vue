<script setup lang="ts">
// Who in a team can use one of its mailboxes, and changing it
// (docs/workspaces.md, "Grant rules"), for the team's owners and admins, the
// only people who change it. Each person's flags are ticked and then saved;
// what the caller may not change is shown as such beforehand, with why: Read
// passes only from someone who reads the mailbox now, so an owner or an admin
// who does not gives none, not even to themselves; Act goes only to someone
// who reads it, and Send to anyone; Manage is given to members, as owners
// and admins have it by their role; and the last person who can read the
// mailbox keeps Read. A mailbox nobody can read says so, and what is left to
// do about it. Below the people, the API keys holding something on it: what
// each holds and who created it, and taking the mailbox out of one; keys
// never count as readers, and what one holds is changed in the workspace's
// API keys. The server decides every change.
import { computed, onMounted, ref } from 'vue'
import type { GrantFlags, MailboxKey, Member } from '../api/types'
import { dropKeyMailbox } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { session } from '../state/session'
import { directoryEntry, loadDirectory, loadMembers, personName, saveGrant, team } from '../state/team'
import { currentWorkspace } from '../state/workspaces'
import {
  activeMember, administers, flagHint, flagLabel, flagNames, flagsOf, grantPermissions, grantSummary, holdsAny, lastReaderOf, sameFlags,
  toggleFlag, workspaceName, workspaceRoleLabel, type FlagName, type GrantPermissions,
} from '../ui/access'
import { announce } from '../ui/announce'
import { keyFlagsSummary, keyPerson, scopeLabel } from '../ui/apikeys'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ accountId: string; email: string }>()

interface Row { member: Member; held: GrantFlags; rules: GrantPermissions }

const me = computed(() => session.user?.id ?? '')
const workspace = computed(currentWorkspace)
const teamName = computed(() => workspaceName(workspace.value))
const admin = computed(() => administers(workspace.value))
const entry = computed(() => directoryEntry(props.accountId))
/** Gone from the team's directory since the sheet opened: the mailbox was removed. */
const gone = computed(() => team.directory.loaded && !entry.value)
const grantOf = (userID: string): GrantFlags => flagsOf(entry.value?.grants.find(grant => grant.user_id === userID))
const mine = computed(() => grantOf(me.value))
/** The person is the only one who can read the mailbox, as the server marks them, or as its count of readers says. */
function lastReader(member: Member, held: GrantFlags): boolean {
  if (lastReaderOf(member).includes(props.accountId)) return true
  return held.read && activeMember(member) && entry.value?.readers === 1
}

function rank(row: Row): number {
  if (row.member.user_id === me.value) return 0
  return holdsAny(row.held) ? 1 : 2
}

/** Everyone who can be given access, or holds some: the caller first, then those who hold some. */
const rows = computed<Row[]>(() => team.members.list
  .map(member => {
    const held = grantOf(member.user_id)
    const rules = grantPermissions({ administers: admin.value, mine: mine.value, member, held, lastReader: lastReader(member, held) })
    return { member, held, rules }
  })
  .filter(row => row.rules.lock !== 'inactive')
  .sort((a, b) => rank(a) - rank(b) || (a.member.name || a.member.email).localeCompare(b.member.name || b.member.email)))

/** What the caller ticked for each person, until it is saved or set back. */
const drafts = ref<Record<string, GrantFlags>>({})
const saving = ref('')
const problems = ref<Record<string, Failure>>({})

const shown = (row: Row): GrantFlags => drafts.value[row.member.user_id] ?? row.held
const changed = (row: Row): boolean => !sameFlags(shown(row), row.held)
/** Ticked as shown: an owner's or an admin's Manage is their role's, whatever their grant. */
const ticked = (row: Row, flag: FlagName): boolean => shown(row)[flag] || (flag === 'manage' && row.rules.byRole)

/** Whether the caller may tick or untick this flag for this person, from what is ticked now. */
function editable(row: Row, flag: FlagName): boolean {
  if (row.rules.lock || saving.value) return false
  if (flag === 'manage' && row.rules.byRole) return false
  const flags = shown(row)
  // Setting back what this draft changed is always possible.
  if (flags[flag] !== row.held[flag]) return true
  if (flags[flag]) return row.rules.canRemove[flag]
  if (!row.rules.canAdd[flag]) return false
  // Act goes to someone who reads: the read it needs must be there, or be the caller's to give.
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

/** Why a row's Read cannot go: said beside it, before anyone tries. */
function lockText(row: Row): string {
  if (!row.rules.lastReader || !row.held.read) return ''
  return row.member.user_id === me.value
    ? t('You are the only person who can read this mailbox: give someone else Read before you give yours up.')
    : t('The only person who can read this mailbox: give someone else Read before taking theirs.')
}

/** What the caller cannot give, said once for every row. */
const giving = computed(() => mine.value.read ? ''
  : t('You do not read this mailbox, so you cannot give Read on it, not even to yourself: only an owner or an admin who reads it can.'))

/** The live keys holding something on the mailbox, by name. */
const keys = computed<MailboxKey[]>(() => [...(entry.value?.keys ?? [])].sort((a, b) => a.name.localeCompare(b.name)))
/** The key whose removal from this mailbox is being asked about, and the one being removed. */
const taking = ref('')
const takingBusy = ref(false)
const keyProblems = ref<Record<string, Failure>>({})
function askTake(key: MailboxKey) {
  delete keyProblems.value[key.prefix]
  taking.value = key.prefix
}
async function take(key: MailboxKey) {
  if (takingBusy.value) return
  takingBusy.value = true
  const problem = await dropKeyMailbox(key.prefix, props.accountId)
  takingBusy.value = false
  if (problem) { keyProblems.value[key.prefix] = problem; return }
  taking.value = ''
  announce(t('{email} was taken out of {name}.', { email: props.email, name: key.name }))
}
const keyCreator = (key: MailboxKey) => keyPerson(key.created_by, me.value, personName)

function refresh() {
  void loadMembers()
  void loadDirectory()
}

onMounted(() => {
  if (!team.members.loaded && !team.members.loading) void loadMembers()
  if (!team.directory.loaded && !team.directory.loading) void loadDirectory()
})
</script>

<template>
  <div class="access-panel">
    <p class="hint">{{ t('Who in {team} can use this mailbox. Each person needs their own access: being an owner or an admin of the team gives Manage, never its mail.', { team: teamName }) }}</p>

    <div v-if="team.members.failure || team.directory.failure" class="alert with-action" role="alert">
      <span>{{ describe((team.members.failure ?? team.directory.failure)!) }}</span>
      <button class="ghost small" type="button" @click="refresh">{{ t('Try again') }}</button>
    </div>
    <p v-else-if="!team.members.loaded || !team.directory.loaded" class="dim" role="status">{{ t('Reading who has access…') }}</p>
    <p v-else-if="gone" class="note" role="status">{{ t('{email} is no longer a mailbox of {team}: it was removed.', { email, team: teamName }) }}</p>

    <template v-else>
      <p v-if="entry?.no_reader" class="note access-nobody" role="status"><AppIcon name="eye-off" :size="16" /><span>{{ t('Nobody in {team} can read this mailbox any more, so it does not sync. Read is given only by someone who reads it: remove the mailbox, or remove it and connect it again, which gives Read to whoever connects it.', { team: teamName }) }}</span></p>
      <p v-if="giving" class="hint">{{ giving }}</p>
      <ul class="access-rows" :aria-label="t('Access to {email}', { email })">
        <li v-for="row in rows" :key="row.member.user_id" class="access-row" :data-user="row.member.user_id">
          <div class="who">
            <strong>{{ row.member.user_id === me ? t('You') : row.member.name || row.member.email }}</strong>
            <small>{{ row.member.email }} · {{ workspaceRoleLabel(row.member.role) }}</small>
          </div>
          <fieldset class="flags" :disabled="!!row.rules.lock">
            <legend class="visually-hidden">{{ t('Access of {name}', { name: row.member.name || row.member.email }) }}</legend>
            <label v-for="flag in flagNames" :key="flag" class="flag" :title="flagHint(flag)">
              <input type="checkbox" :name="`${row.member.user_id}-${flag}`" :value="flag" :checked="ticked(row, flag)" :disabled="!editable(row, flag)" @change="toggle(row, flag, $event)" />
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
    </template>

    <dl class="flag-legend">
      <div v-for="flag in flagNames" :key="flag"><dt>{{ flagLabel(flag) }}</dt><dd>{{ flagHint(flag) }}</dd></div>
    </dl>

    <section v-if="entry && !gone" class="access-keys" :aria-labelledby="`keys-${accountId}`">
      <h4 :id="`keys-${accountId}`">{{ t('API keys') }}</h4>
      <p v-if="!keys.length" class="dim">{{ t('No API key holds anything on this mailbox.') }}</p>
      <ul v-else class="access-rows" :aria-label="t('API keys on {email}', { email })">
        <li v-for="key in keys" :key="key.prefix" class="access-row key-holder" :data-key="key.prefix">
          <div class="who">
            <strong>{{ key.name }} <span class="mono" translate="no">{{ key.prefix }}…</span></strong>
            <small>{{ keyFlagsSummary(key) }} · {{ scopeLabel(key.scope) }}<template v-if="key.created_by === me"> · {{ t('Created by you') }}</template><template v-else-if="keyCreator(key)"> · {{ t('Created by {name}', { name: keyCreator(key) }) }}</template><template v-if="key.carried_over"> · {{ t('Made before keys belonged to workspaces') }}</template></small>
          </div>
          <div v-if="taking === key.prefix" class="row-save">
            <span class="dim">{{ t('{name} loses everything it holds on this mailbox.', { name: key.name }) }}</span>
            <button class="ghost small" type="button" :disabled="takingBusy" @click="taking = ''">{{ t('Cancel') }}</button>
            <button class="danger small" type="button" :disabled="takingBusy" @click="take(key)">{{ takingBusy ? t('Saving…') : t('Take out of the key') }}</button>
          </div>
          <div v-else-if="admin" class="row-save"><button class="ghost small remove" type="button" @click="askTake(key)">{{ t('Take out of the key…') }}</button></div>
          <p v-if="keyProblems[key.prefix]" class="alert" role="alert">{{ describe(keyProblems[key.prefix]!) }}</p>
        </li>
      </ul>
      <p class="hint">{{ t('A key reads a mailbox only where an owner or an admin who reads it gave it Read, and keeps what it holds when they lose their own access. Keys never count as readers. What a key holds on each mailbox is changed in API keys & MCP.') }}</p>
    </section>
  </div>
</template>

<style scoped>
.access-panel { display: grid; gap: 12px; }
.access-panel p { margin: 0; }
.access-nobody { display: flex; align-items: flex-start; gap: 8px; font-size: 13px; line-height: 1.5; }
.access-nobody .app-icon { flex: none; margin-top: 2px; }
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
.access-keys { display: grid; gap: 10px; padding-top: 12px; border-top: 1px solid var(--line); }
.access-keys h4 { margin: 0; font-size: 13px; }
.access-keys .mono { font-weight: 400; font-size: 12px; color: var(--text-dim); }
.remove { color: var(--danger); }
@media (max-width: 600px) {
  .flag { flex: 1 1 calc(50% - 8px); justify-content: center; }
  .row-save button { flex: 1; }
}
</style>
