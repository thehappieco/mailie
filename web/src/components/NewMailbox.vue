<script setup lang="ts">
// A mailbox this tab just connected (docs/console.md, "Naming a mailbox"):
// it says so, and asks what comes after the link, never before it — a
// nickname, shown in place of the address wherever the mailbox is listed,
// and, for a team's owners and admins, the team's agreement to sync it, which
// a link no longer gives (it connects a team mailbox with sync off). Both are
// optional: Skip sends nothing, Done sends what was filled, each once; a
// refusal is said under its own part and Done stays, what succeeded already
// is not sent again. The agreement is the one the sheet gives (state/team.ts
// switchTeamSync), to the revision of the edition's text this page shows,
// drawn for the team; a server that already asks about another text is
// offered a reload instead of the box. The heading takes the focus when the
// card is drawn, so a page that comes back from the provider lands on it.
import { computed, nextTick, onMounted, ref, useId } from 'vue'
import { MAILBOX_NAME_MAX, type Account } from '../api/types'
import { edition } from '../edition'
import { renameAccount } from '../state/accounts'
import type { Failure } from '../state/failure'
import { consent, consentTextOutdated, loadConsent } from '../state/sync'
import { switchTeamSync } from '../state/team'
import { workspaces } from '../state/workspaces'
import { administers, workspaceName } from '../ui/access'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ done: [] }>()

const ids = { name: useId(), nameHint: useId(), nameProblem: useId(), sync: useId(), syncHint: useId() }
const heading = ref<HTMLElement | null>(null)
const syncText = edition().sync
const outdated = computed(consentTextOutdated)

/** The team it belongs to, when it is a team's and the person administers it: they give its agreement to sync. */
const team = computed(() => {
  const found = workspaces.list.find(item => item.id === props.account.workspace_id)
  return found?.kind === 'team' && administers(found) ? found : undefined
})
const teamName = computed(() => workspaceName(team.value))
/** Offered while the team's sync is off and nothing pauses it: turning it on is the team's agreement. */
const offersSync = computed(() => !!team.value && !props.account.sync.enabled && !props.account.sync.paused)
const syncing = computed(() => props.account.state === 'active' && props.account.sync.enabled)

const name = ref('')
const agree = ref(false)
const busy = ref(false)
/** What was sent already and kept: the name the server took, and the team's agreement. */
const sentName = ref<string | null>(null)
const agreed = ref(false)
const nameProblem = ref<Failure | null>(null)
const syncProblem = ref<Failure | null>(null)

async function done() {
  if (busy.value) return
  busy.value = true
  nameProblem.value = null
  syncProblem.value = null
  const wanted = name.value.trim()
  if (wanted && wanted !== sentName.value) {
    const problem = await renameAccount(props.account.id, wanted)
    if (problem) nameProblem.value = problem
    else sentName.value = wanted
  }
  if (offersSync.value && agree.value && !agreed.value && !outdated.value) {
    const problem = await switchTeamSync(props.account.id, true)
    if (problem) {
      syncProblem.value = problem
      // Refused for a text the server no longer asks about: what it asks about now is read, and a reload is offered.
      if (problem.code === 'bad_request') void loadConsent()
    } else agreed.value = true
  }
  busy.value = false
  if (!nameProblem.value && !syncProblem.value) emit('done')
}
function skip() {
  if (!busy.value) emit('done')
}
function reloadPage() { location.reload() }

onMounted(async () => {
  if (offersSync.value && !consent.loaded && !consent.loading) void loadConsent()
  await nextTick()
  heading.value?.focus({ preventScroll: false })
})
</script>

<template>
  <section class="new-mailbox" :aria-labelledby="`${ids.name}-heading`">
    <div class="success">
      <AppIcon name="check" :size="18" />
      <span>
        <strong :id="`${ids.name}-heading`" ref="heading" class="new-mailbox-heading" tabindex="-1">{{ t('{email} is connected.', { email: account.email }) }}</strong>
        <br />{{ syncing ? t('Mailie is starting to sync it: the last 90 days first, then new mail as it arrives.') : t('You can list its folders now. Sync is off until you turn it on.') }}
      </span>
    </div>
    <form class="form-stack" autocomplete="off" @submit.prevent="done">
      <div class="with-hint">
        <label :for="ids.name">{{ t('Nickname (optional)') }}</label>
        <input :id="ids.name" v-model="name" name="nickname" :maxlength="MAILBOX_NAME_MAX" autocomplete="off" :placeholder="t('For example: Support')"
          :aria-describedby="nameProblem ? `${ids.nameHint} ${ids.nameProblem}` : ids.nameHint" :aria-invalid="nameProblem ? 'true' : undefined" :disabled="busy" />
        <p :id="ids.nameHint" class="hint">{{ t('Shown in place of the address wherever this mailbox is listed.') }}</p>
        <p v-if="nameProblem" :id="ids.nameProblem" class="alert" role="alert">{{ describe(nameProblem) }}</p>
      </div>
      <fieldset v-if="offersSync && !agreed" class="team-sync" :disabled="busy">
        <legend>{{ t('Mail sync for {team}', { team: teamName }) }}</legend>
        <template v-if="outdated">
          <p class="dim">{{ syncText.changedWhileOpen() }}</p>
          <button class="ghost small" type="button" @click="reloadPage">{{ t('Reload page') }}</button>
        </template>
        <template v-else>
          <div class="team-sync-text" tabindex="0" role="region" :aria-label="t('Mail sync for {team}', { team: teamName })"><component :is="syncText.component" :team="teamName" /></div>
          <label :for="ids.sync" class="team-sync-agree">
            <input :id="ids.sync" v-model="agree" type="checkbox" name="team-sync" :aria-describedby="ids.syncHint" />
            <span>{{ t('Turn on sync for {team}', { team: teamName }) }}</span>
          </label>
          <small :id="ids.syncHint" class="hint">{{ t('Ticked, you agree to the text above on behalf of {team}, and the mailbox syncs under the team’s agreement. Left unticked, it stays off; an owner or an admin can turn it on later from its details.', { team: teamName }) }}</small>
        </template>
        <p v-if="syncProblem && !outdated" class="alert" role="alert">{{ describe(syncProblem) }}</p>
      </fieldset>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="skip">{{ t('Skip') }}</button>
        <button class="primary" type="submit" :disabled="busy">{{ busy ? t('Saving…') : t('Done') }}</button>
      </div>
    </form>
  </section>
</template>

<style scoped>
.new-mailbox { display: grid; gap: 16px; min-width: 0; }
.new-mailbox-heading { font-weight: 650; overflow-wrap: anywhere; }
.new-mailbox-heading:focus { outline: none; }
.success span { overflow-wrap: anywhere; line-height: 1.5; }
.form-stack { display: grid; gap: 16px; }
.with-hint { display: grid; gap: 6px; }
.with-hint label { font-size: 13px; }
.with-hint .hint, .with-hint .alert { margin: 0; }
.team-sync { display: grid; gap: 10px; margin: 0; padding: 0; border: 0; min-width: 0; }
.team-sync legend { padding: 0; margin-bottom: 8px; font-size: 13px; }
.team-sync p { margin: 0; }
.team-sync .ghost { justify-self: start; }
.team-sync-text { max-height: 220px; overflow-y: auto; overscroll-behavior: contain; padding: 12px 14px; border: 1px solid var(--line); border-radius: 10px; background: var(--bg-raised); }
.team-sync-text:focus-visible { outline: 2px solid var(--console-accent); outline-offset: 2px; }
.team-sync-agree { display: flex; align-items: flex-start; gap: 10px; font-size: 13px; font-weight: 600; cursor: pointer; }
.team-sync-agree input { width: auto; min-height: 0; margin: 2px 0 0; accent-color: var(--accent); flex: none; }
.team-sync .hint { font-size: 12px; line-height: 1.45; }
.dialog-actions { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; }
@media (max-width: 600px) { .dialog-actions button { flex: 1; } .with-hint input { font-size: 16px; } }
</style>
