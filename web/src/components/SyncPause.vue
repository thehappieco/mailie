<script setup lang="ts">
// Pausing and resuming one mailbox's sync, in the account sheet's Sync
// section (docs/workspaces.md, "Pausing a mailbox's sync"). A pause stops
// indexing new mail and keeps what was indexed — searched, opened, acted on
// and sent from — until someone who may resumes it; a recovery pauses a
// mailbox too. Its person pauses and resumes a personal mailbox; a team's
// owners and admins a team's, by their role, and a team's resume agrees to
// the edition's sync text on the team's behalf (TeamSyncAgreement), as
// turning it on does. Pause is offered only where the edition offers it
// (edition().syncPause) and while the mailbox syncs; Resume always, but on a
// team mailbox nobody can read (TeamMailboxSync says why), or one only a
// suspended member would read once resumed, which waits for them. Who paused
// it is said to the team's owners and admins (the access directory), never
// to a member, who is told an owner or an admin did. The server decides; this
// says beforehand what it will do.
import { computed, nextTick, onMounted, ref } from 'vue'
import type { Account } from '../api/types'
import { edition } from '../edition'
import type { Failure } from '../state/failure'
import { consentTextOutdated, pauseSync, resumeSync } from '../state/sync'
import { directoryEntry, loadDirectory, loadMembers, personName, team } from '../state/team'
import { currentWorkspace } from '../state/workspaces'
import { actorName, administers, owns, workspaceName } from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import TeamSyncAgreement from './TeamSyncAgreement.vue'

const props = defineProps<{ account: Account }>()

const workspace = computed(currentWorkspace)
const inTeam = computed(() => workspace.value?.kind === 'team')
const teamName = computed(() => workspaceName(workspace.value))
const paused = computed(() => props.account.sync.paused === true)
/** Its person, for a personal mailbox; a team's owners and admins, by their role, for a team's. */
const mayPause = computed(() => inTeam.value ? administers(workspace.value) : owns(workspace.value))
/** The edition offers a person's pause; a paused mailbox's Resume is offered either way. */
const offered = edition().syncPause === true
/** Syncing, as the sheet says: active, and its agreement to sync stands. */
const syncing = computed(() => props.account.state === 'active' && props.account.sync.enabled)
const entry = computed(() => inTeam.value ? directoryEntry(props.account.id) : undefined)
/** Nobody can read it, nor be given Read on it: it is never resumed. */
const unreadable = computed(() => !!entry.value?.no_reader)
/** Nobody reads it now, but someone suspended would once resumed: it waits for them. */
const waiting = computed(() => !!entry.value && entry.value.readers === 0 && !entry.value.no_reader)
const date = computed(() => dayStamp(props.account.sync.paused_since))

/** When it was paused, and, to whoever manages the team's mailboxes, by whom. */
const line = computed(() => {
  if (!inTeam.value) return t('Sync paused on {date}.', { date: date.value })
  if (!mayPause.value) return t('Sync paused by an owner or an admin of {team} on {date}.', { team: teamName.value, date: date.value })
  const name = actorName(entry.value?.paused_by, personName)
  return name ? t('Sync paused by {name} on {date}.', { name, date: date.value }) : t('Sync paused on {date}.', { date: date.value })
})

const root = ref<HTMLElement | null>(null)
const action = ref<HTMLButtonElement | null>(null)
const dialog = ref<'' | 'pause' | 'resume'>('')
const busy = ref(false)
const problem = ref<Failure | null>(null)
const outdated = computed(consentTextOutdated)

function open(kind: 'pause' | 'resume') {
  if (busy.value) return
  problem.value = null
  dialog.value = kind
}
function close() { if (!busy.value) dialog.value = '' }
function reloadPage() { location.reload() }
/** The button that took the place of the one pressed, once drawn: what focus goes back to. */
async function refocus() {
  await nextTick()
  action.value?.focus({ preventScroll: true })
}

async function pause() {
  if (busy.value) return
  busy.value = true
  problem.value = await pauseSync(props.account)
  busy.value = false
  if (problem.value) return
  dialog.value = ''
  announce(t('Sync of {email} is paused. What Mailie indexed is kept.', { email: props.account.email }))
  void refocus()
}

async function resume() {
  if (busy.value || (inTeam.value && outdated.value)) return
  busy.value = true
  problem.value = await resumeSync(props.account)
  busy.value = false
  if (problem.value) return
  dialog.value = ''
  announce(inTeam.value
    ? t('Sync of {email} is resumed for {team}.', { email: props.account.email, team: teamName.value })
    : t('Sync of {email} is resumed.', { email: props.account.email }))
  void refocus()
}

onMounted(() => {
  if (!inTeam.value || !mayPause.value) return
  if (!team.directory.loaded && !team.directory.loading) void loadDirectory()
  if (!team.members.loaded && !team.members.loading) void loadMembers()
})
</script>

<template>
  <div ref="root" class="sync-pause" tabindex="-1">
    <template v-if="paused">
      <p class="dim sync-pause-line">{{ line }}</p>
      <template v-if="mayPause">
        <template v-if="!inTeam">
          <button ref="action" class="ghost small sync-pause-button" type="button" :disabled="busy" @click="resume"><AppIcon name="play" :size="16" />{{ busy ? t('Resuming…') : t('Resume sync') }}</button>
          <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
        </template>
        <p v-else-if="waiting" class="note sync-pause-note"><AppIcon name="eye-off" :size="15" /><span>{{ t('Nobody in {team} reads it now, but someone suspended on this server would once resumed: its sync is resumed once someone reads it.', { team: teamName }) }}</span></p>
        <button v-else-if="!unreadable" ref="action" class="ghost small sync-pause-button" type="button" aria-haspopup="dialog" :disabled="busy" @click="open('resume')"><AppIcon name="play" :size="16" />{{ t('Resume sync for {team}…', { team: teamName }) }}</button>
      </template>
      <p v-else-if="inTeam" class="dim">{{ t('The owners and admins of {team} resume it, for the team.', { team: teamName }) }}</p>
    </template>
    <button v-else-if="mayPause && offered && syncing" ref="action" class="ghost small sync-pause-button" type="button" aria-haspopup="dialog" :disabled="busy" @click="open('pause')"><AppIcon name="pause" :size="16" />{{ t('Pause sync…') }}</button>

    <ConsoleDialog v-if="dialog === 'pause'" :title="t('Pause sync of {email}?', { email: account.email })" :busy="busy" :return-focus="() => root" @close="close">
      <p class="dim sync-pause-what">{{ inTeam
        ? t('Mailie stops indexing new mail for this mailbox, for everyone in {team} who reads it, and keeps what it already indexed: they can still search it, open its messages, act on them and send from it. Nothing is deleted, and the others who read it are told. Resume sync to catch up.', { team: teamName })
        : t('Mailie stops indexing new mail for this mailbox and keeps what it already indexed: you can still search it, open its messages, act on them and send from it. Nothing is deleted. Resume sync to catch up.') }}</p>
      <p class="note sync-pause-waits">{{ t('While it is paused, what changes on the mail server — new mail, messages moved or deleted there — shows in Mailie only once sync is resumed, and a message there may not open until then.') }}</p>
      <p v-if="problem" class="alert sync-pause-problem" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
        <button class="primary" type="button" :disabled="busy" @click="pause">{{ busy ? t('Pausing…') : t('Pause sync') }}</button>
      </div>
    </ConsoleDialog>

    <ConsoleDialog v-if="dialog === 'resume'" :title="t('Resume sync for {team}?', { team: teamName })" :subtitle="account.email" :busy="busy" wide :return-focus="() => root" @close="close">
      <TeamSyncAgreement :team="teamName" :lead="t('You agree to this text on behalf of {team}: {email} then syncs again under the team’s agreement, for everyone in the team who reads it, from where it stopped.', { team: teamName, email: account.email })" />
      <p v-if="problem && !outdated" class="alert sync-pause-problem" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
        <button v-if="outdated" class="primary" type="button" @click="reloadPage">{{ t('Reload page') }}</button>
        <button v-else class="primary" type="button" :disabled="busy" @click="resume">{{ busy ? t('Resuming…') : t('Resume sync for {team}', { team: teamName }) }}</button>
      </div>
    </ConsoleDialog>
  </div>
</template>

<style scoped>
.sync-pause { display: grid; gap: 10px; justify-items: start; margin-top: 12px; }
.sync-pause:focus { outline: none; }
.sync-pause:empty { display: none; }
.sync-pause p { margin: 0; }
.sync-pause-button { display: inline-flex; align-items: center; gap: 6px; }
.sync-pause-note { display: flex; align-items: flex-start; gap: 7px; font-size: 13px; line-height: 1.5; }
.sync-pause-note .app-icon { flex: none; margin-top: 2px; }
.sync-pause-what, .sync-pause-waits { margin: 0 0 12px; }
.sync-pause-problem { margin: 14px 0 0; }
</style>
