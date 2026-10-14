<script setup lang="ts">
// The removed mailboxes of the workspace shown, for its owners only: the
// person of their personal workspace, an owner of a team (docs/workspaces.md,
// "Removed mailboxes"). Each is kept 30 days from its removal, used by
// nothing, and then deleted for good; until then an owner recovers it — back
// with what it kept and the readers who still qualify, its sync held until
// someone who may turns it on — or deletes it for good sooner, which asks for
// its address typed, as removing it did, and sends its id to confirm. A team
// mailbox nobody who could read it is left to read is not recovered: it is
// deleted for good and connected again. Shown only while there is one, or
// while they could not be read.
import { computed, onMounted, ref, useId, watch } from 'vue'
import type { ProviderID, RemovedMailbox } from '../api/types'
import { purgeRemoved, recoverRemoved, loadRemoved, removed } from '../state/accounts'
import { session } from '../state/session'
import { personName } from '../state/team'
import { currentWorkspace, workspaces } from '../state/workspaces'
import { actorName, owns, workspaceName } from '../ui/access'
import { describe } from '../ui/errors'
import { count, dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import { providerIcon, providerName } from '../ui/labels'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const headingID = useId()
const typedID = useId()
const shown = computed(currentWorkspace)
const inTeam = computed(() => shown.value?.kind === 'team')
const teamName = computed(() => workspaceName(shown.value))
/** The removed mailbox whose deletion for good is being confirmed, and what was typed. */
const purging = ref<RemovedMailbox | null>(null)
const typed = ref('')
const armed = computed(() => !!purging.value && typed.value.trim().toLowerCase() === purging.value.email.toLowerCase())
const busy = computed(() => removed.busy !== '')
const purgeProblem = computed(() => purging.value && removed.problem?.id === purging.value.id ? removed.problem.failure : null)

/** Reads them for the workspace shown, once, and only for one the person owns: anyone else is never asked about them. */
function read() {
  if (!owns(currentWorkspace()) || removed.loading || (removed.loaded && removed.workspace === workspaces.currentID)) return
  void loadRemoved()
}
onMounted(read)
watch(() => workspaces.currentID, read)

/** Who removed it, and when: the person themselves, someone the team lists, a key or the command line. */
function removedLine(item: RemovedMailbox): string {
  const date = dayStamp(item.removed_at)
  if (item.removed_by && item.removed_by === session.user?.id) return t('Removed by you on {date}.', { date })
  const by = item.removed_by?.startsWith('usr_') ? personName(item.removed_by) : actorName(item.removed_by, personName)
  return by ? t('Removed by {name} on {date}.', { name: by, date }) : t('Removed on {date}.', { date })
}
/** A team mailbox nobody would read once recovered: the server refuses to recover it. */
const unreadable = (item: RemovedMailbox) => inTeam.value && item.readers === 0
const problemFor = (item: RemovedMailbox) => removed.problem?.id === item.id && !purging.value ? removed.problem.failure : null

function recover(item: RemovedMailbox) { void recoverRemoved(item.id) }
function askPurge(item: RemovedMailbox) {
  if (busy.value) return
  removed.problem = null
  typed.value = ''
  purging.value = item
}
function closePurge() { if (!busy.value) purging.value = null }
async function purge() {
  const item = purging.value
  if (!item || !armed.value || busy.value) return
  if (await purgeRemoved(item.id)) purging.value = null
}
</script>

<template>
  <section v-if="removed.list.length || removed.failure" class="removed-mailboxes" :aria-labelledby="headingID">
    <div class="removed-head">
      <h2 :id="headingID">{{ t('Removed mailboxes') }}</h2>
      <p class="dim">{{ inTeam
        ? t('Kept for 30 days after they were removed, then deleted for good. Meanwhile nothing of them syncs and nobody reads them, and any owner of {team} can recover them.', { team: teamName })
        : t('Kept for 30 days after you removed them, then deleted for good. Meanwhile nothing of them syncs, and you can recover them.') }}</p>
    </div>
    <div v-if="removed.failure" class="alert with-action" role="alert"><span>{{ describe(removed.failure) }}</span><button class="ghost small" type="button" @click="loadRemoved">{{ t('Try again') }}</button></div>
    <ul v-if="removed.list.length" class="removed-list" :aria-labelledby="headingID">
      <li v-for="item in removed.list" :key="item.id" class="removed-item">
        <div class="removed-identity">
          <span class="provider-tile"><AppIcon :name="providerIcon(item.provider as ProviderID)" :size="19" /></span>
          <span class="grow"><strong>{{ item.display_name || item.email }}</strong><small>{{ item.display_name ? item.email : providerName(item.provider as ProviderID) }}</small></span>
        </div>
        <p class="removed-facts">
          <span>{{ removedLine(item) }}</span>
          <span>{{ t('Kept until {date}.', { date: dayStamp(item.purge_after) }) }}</span>
          <span v-if="inTeam && item.readers > 0">{{ t('People in {team} who read it again once it is recovered: {count}.', { count: count(item.readers), team: teamName }) }}</span>
          <span v-if="!item.index_kept">{{ t('Its index was deleted when its sync was turned off: once recovered and turned on, it syncs from the start.') }}</span>
        </p>
        <p v-if="unreadable(item)" class="note removed-note"><AppIcon name="eye-off" :size="15" /><span>{{ t('Nobody in {team} who could read it is left, so it cannot be recovered. To use it again, delete it for good and connect it again.', { team: teamName }) }}</span></p>
        <p v-if="problemFor(item)" class="alert" role="alert">{{ describe(problemFor(item)!) }}</p>
        <div class="row-actions">
          <button v-if="!unreadable(item)" class="ghost" type="button" :disabled="busy" @click="recover(item)"><AppIcon name="undo" :size="16" />{{ removed.busy === item.id && !purging ? t('Recovering…') : t('Recover') }}</button>
          <button class="ghost danger-text" type="button" :disabled="busy" aria-haspopup="dialog" @click="askPurge(item)"><AppIcon name="trash" :size="16" />{{ t('Delete for good…') }}</button>
        </div>
      </li>
    </ul>

    <ConsoleDialog v-if="purging" :title="t('Delete {email} for good?', { email: purging.email })" :busy="busy" @close="closePurge">
      <form class="form-stack removed-confirm" autocomplete="off" @submit.prevent="purge">
        <p class="dim">{{ t('Mailie deletes the mailbox and everything it kept for it: the saved sign-in, its index and its key. It cannot be recovered afterwards. Nothing is deleted at the provider, and the address can then be connected again.') }}</p>
        <label :for="typedID">{{ t('Type {address} to confirm', { address: purging.email }) }}</label>
        <input :id="typedID" v-model="typed" name="purge-confirm" autocomplete="off" :spellcheck="false" autocapitalize="off" inputmode="email" :placeholder="purging.email" :disabled="busy" />
        <p v-if="purgeProblem" class="alert" role="alert">{{ describe(purgeProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="busy" @click="closePurge">{{ t('Cancel') }}</button>
          <button class="danger" type="submit" :disabled="busy || !armed">{{ busy ? t('Deleting…') : t('Delete for good') }}</button>
        </div>
      </form>
    </ConsoleDialog>
  </section>
</template>

<style scoped>
.removed-mailboxes { display: grid; gap: 12px; margin-top: 10px; }
.removed-head h2 { font-size: 15px; margin: 0; font-weight: 600; }
.removed-head p { margin: 6px 0 0; font-size: 13px; line-height: 1.55; }
.removed-list { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; }
.removed-item { display: grid; gap: 10px; padding: 16px; border: 1px dashed var(--console-border); border-radius: 12px; background: var(--bg-panel); min-width: 0; }
.removed-identity { display: flex; align-items: center; gap: 12px; min-width: 0; }
.removed-identity .provider-tile { width: 36px; height: 36px; border-radius: 10px; background: var(--bg-hover); color: var(--text-dim); }
.removed-identity strong { display: block; font-size: 14px; font-weight: 600; overflow-wrap: anywhere; }
.removed-identity small { display: block; margin-top: 3px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; }
.removed-facts { display: grid; gap: 3px; margin: 0; font-size: 12px; line-height: 1.5; color: var(--text-dim); }
.removed-note { display: flex; align-items: flex-start; gap: 7px; margin: 0; }
.removed-note .app-icon { flex: none; margin-top: 2px; }
.removed-item .alert { margin: 0; }
.row-actions button { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; padding: 8px 12px; }
.danger-text { color: var(--danger); }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.removed-confirm p { margin: 0; }
.removed-confirm input { color: var(--text); background: var(--bg-input); border: 1px solid var(--line); border-radius: 10px; padding: 11px 12px; width: 100%; }
@media (max-width: 600px) { .removed-confirm input { font-size: 16px; } }
</style>
