<script setup lang="ts">
// A team mailbox's sync, for the team's owners and admins, who give or
// withdraw its agreement on the team's behalf (docs/workspaces.md,
// "Consents"): who turned it on, when and to which text, and the switch. On
// shows the edition's sync text, the same a person agrees to for their own
// mailboxes, and sends its revision; when the server already names another,
// a reload is offered instead. Off asks for the mailbox's address typed, and
// says its index goes for everyone who reads it. An agreement to an earlier
// text, or one the upgrade carried over from whoever connected the mailbox
// (still tied to them until it is given again for the team), offers
// confirming it for the team in place of the switch. A mailbox nobody can
// read says it does not sync, and offers no way to turn it on, which the
// server refuses: nobody could be given Read on it again. Only turning it off
// is offered, while something of it is still on or kept. The server decides;
// this says beforehand what it will do.
import { computed, onMounted, ref, useId } from 'vue'
import type { Account } from '../api/types'
import { edition } from '../edition'
import type { Failure } from '../state/failure'
import { consent, consentTextOutdated, loadConsent } from '../state/sync'
import { directoryEntry, loadDirectory, loadMembers, personName, switchTeamSync, team } from '../state/team'
import { currentWorkspace } from '../state/workspaces'
import { actorName, teamSyncStanding, workspaceName } from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { count, dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import PermissionRow from './PermissionRow.vue'

const props = defineProps<{ account: Account }>()

const text = edition().sync
const teamName = computed(() => workspaceName(currentWorkspace()))
const entry = computed(() => directoryEntry(props.account.id))
const record = computed(() => entry.value?.sync)
const standing = computed(() => teamSyncStanding(record.value))
const giver = computed(() => actorName(record.value?.enabled_by, personName))
const since = computed(() => record.value?.enabled_at ? dayStamp(record.value.enabled_at) : '')
const outdated = computed(consentTextOutdated)
/** Nobody can read it, nor be given Read on it: it can be turned off, never on. */
const unreadable = computed(() => !!entry.value?.no_reader)

const summary = computed(() => {
  if (!entry.value) return team.directory.failure ? '' : t('Checking…')
  // A migrated agreement names whoever it is tied to; once they were deleted, nobody.
  const values = { team: teamName.value, date: since.value, name: giver.value || t('someone no longer in the team') }
  switch (standing.value) {
    case 'on': return giver.value
      ? t('On for {team} since {date}, turned on by {name}. Turning it off deletes its index for everyone who reads it.', values)
      : t('On for {team} since {date}. Turning it off deletes its index for everyone who reads it.', values)
    case 'on-earlier': return t('On for {team} since {date}, under an earlier text of sync. It keeps syncing: confirm it under the current text, or turn it off.', values)
    case 'migrated': return t('On since {date} under the agreement {name} gave for their own mailboxes before this server was updated. It is still tied to them: if they turn their sync off or their account is closed, it stops and its index is deleted. Confirm it for {team} to keep it syncing.', values)
    // Kept for as long as it stays stopped, and still tied to them: deleting them deletes it too.
    case 'kept': return unreadable.value
      ? t('Stopped since this server was updated, because {name}, who connected it, was disabled on this server then. Its index is kept while it stays stopped, until its sync is turned off, the mailbox is removed, or their account on this server is deleted.', values)
      : t('Stopped since this server was updated, because {name}, who connected it, was disabled on this server then. Its index is kept while it stays stopped: turning its sync on for {team} resumes from it, and turning it off, removing the mailbox or deleting their account on this server deletes it.', values)
    default: return t('Sync is off. Nothing from this mailbox is stored.')
  }
})
/**
 * An agreement that stands but should be given again: in place of the switch, giving it again or turning it off.
 * For a mailbox nobody can read, only turning off what is still on or kept.
 */
const paused = computed(() => {
  const again = standing.value === 'migrated' || standing.value === 'on-earlier'
  if (unreadable.value) return again || standing.value === 'kept' ? { off: t('Turn off…') } : null
  return again ? { agree: t('Confirm for {team}…', { team: teamName.value }), off: t('Turn off…') } : null
})

const dialog = ref<'' | 'on' | 'off'>('')
const busy = ref(false)
const problem = ref<Failure | null>(null)
const typed = ref('')
const typedID = useId()
const armed = computed(() => typed.value.trim().toLowerCase() === props.account.email.toLowerCase())
const readers = computed(() => entry.value?.readers ?? 0)

function open(kind: 'on' | 'off') {
  if (busy.value || !entry.value || (kind === 'on' && unreadable.value)) return
  problem.value = null
  typed.value = ''
  dialog.value = kind
}
function toggle() { open(record.value?.enabled ? 'off' : 'on') }
function close() { if (!busy.value) dialog.value = '' }
function reloadPage() { location.reload() }
async function change() {
  const on = dialog.value === 'on'
  if (busy.value || (on && outdated.value) || (!on && !armed.value)) return
  busy.value = true
  problem.value = await switchTeamSync(props.account.id, on)
  busy.value = false
  if (problem.value) {
    // Refused for a text the server no longer asks about: what it asks about now is read, and the dialog offers a reload.
    if (on && problem.value.code === 'bad_request') void loadConsent()
    return
  }
  dialog.value = ''
  announce(on ? t('Sync is on for {email}, for {team}.', { email: props.account.email, team: teamName.value })
    : t('Sync is off for {email}. Its index was deleted.', { email: props.account.email }))
}

onMounted(() => {
  if (!team.directory.loaded && !team.directory.loading) void loadDirectory()
  if (!team.members.loaded && !team.members.loading) void loadMembers()
  if (!consent.loaded && !consent.loading) void loadConsent()
})
</script>

<template>
  <div class="team-sync">
    <PermissionRow name="team-sync" icon="users" :label="t('Sync for {team}', { team: teamName })" :summary="summary"
      :on="!!record?.enabled" :disabled="!entry || busy || (unreadable && !record?.enabled)" :paused="paused" :busy="busy"
      :failure="team.directory.failure ? describe(team.directory.failure) : ''"
      @toggle="toggle" @agree="open('on')" @off="open('off')" @retry="loadDirectory" />
    <!-- The record of what was agreed to, beside who and when: the revision of the text. -->
    <p v-if="record?.version" class="hint team-sync-version">{{ t('Agreed to the sync text of revision {version}.', { version: record.version }) }}</p>
    <p v-if="unreadable" class="note team-sync-note"><AppIcon name="eye-off" :size="15" /><span>{{ t('Nobody in {team} can read this mailbox, so it does not sync, and its sync cannot be turned on: Read is given only by someone who reads it.', { team: teamName }) }}</span></p>

    <ConsoleDialog v-if="dialog === 'on'" :title="t('Turn on sync for {team}?', { team: teamName })" :subtitle="account.email" :busy="busy" wide @close="close">
      <p v-if="outdated" class="dim team-sync-outdated">{{ text.changedWhileOpen() }}</p>
      <template v-else>
        <p class="team-sync-lead">{{ t('You agree to this text on behalf of {team}: {email} then syncs under the team’s agreement, for everyone in the team who reads it.', { team: teamName, email: account.email }) }}</p>
        <component :is="text.component" />
      </template>
      <p v-if="problem && !outdated" class="alert team-sync-problem" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
        <button v-if="outdated" class="primary" type="button" @click="reloadPage">{{ t('Reload page') }}</button>
        <button v-else class="primary" type="button" :disabled="busy" @click="change">{{ busy ? t('Turning on…') : t('Turn on sync for {team}', { team: teamName }) }}</button>
      </div>
    </ConsoleDialog>

    <ConsoleDialog v-if="dialog === 'off'" :title="t('Turn off sync for {team}?', { team: teamName })" :subtitle="account.email" :busy="busy" @close="close">
      <form class="form-stack team-sync-confirm" autocomplete="off" @submit.prevent="change">
        <p class="dim">{{ t('Mailie stops syncing {email} and deletes everything it indexed for it: the details of every message, the folder list and its counts, and the log of changes. The mailbox stays connected, and nothing changes at its email provider.', { email: account.email }) }}</p>
        <p class="note">{{ t('The index is deleted for everyone in {team} who reads it: {count} now.', { team: teamName, count: count(readers) }) }}</p>
        <label :for="typedID">{{ t('Type {address} to confirm', { address: account.email }) }}</label>
        <input :id="typedID" v-model="typed" name="team-sync-confirm" autocomplete="off" :spellcheck="false" autocapitalize="off" inputmode="email" :placeholder="account.email" :disabled="busy" />
        <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
          <button class="danger" type="submit" :disabled="busy || !armed">{{ busy ? t('Deleting…') : t('Turn off and delete') }}</button>
        </div>
      </form>
    </ConsoleDialog>
  </div>
</template>

<style scoped>
.team-sync { display: grid; gap: 10px; margin-top: 12px; }
.team-sync-version { margin: 0; font-size: 12px; overflow-wrap: anywhere; }
.team-sync-note { display: flex; align-items: flex-start; gap: 7px; margin: 0; font-size: 13px; line-height: 1.5; }
.team-sync-note .app-icon { flex: none; margin-top: 2px; }
.team-sync-lead { margin: 0 0 12px; font-size: 13px; line-height: 1.55; color: var(--text); font-weight: 600; }
.team-sync-outdated { margin: 0 0 12px; }
.team-sync-problem { margin: 14px 0 0; }
.team-sync-confirm p { margin: 0; }
.team-sync-confirm input { color: var(--text); background: var(--bg-input); border: 1px solid var(--line); border-radius: 10px; padding: 11px 12px; width: 100%; }
@media (max-width: 600px) { .team-sync-confirm input { font-size: 16px; } }
</style>
