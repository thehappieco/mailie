<script setup lang="ts">
// Mail sync in the account section, switched in both directions: on, after
// the edition's text of what sync stores (the same the accounts page shows),
// or, when that text changed while the page was open, a reload to read the
// new one; off, after saying that it deletes the index: of every mailbox the
// person linked, in every workspace, so the team mailboxes among them are
// named first, since other people read their index too: the button waits
// until they are known, or could not be.
import { computed, inject, ref } from 'vue'
import { edition } from '../edition'
import { everyMailbox, loadEveryMailbox } from '../state/everyMailbox'
import { session } from '../state/session'
import { consent, consentTextOutdated, grantConsent, loadConsent, withdrawConsent } from '../state/sync'
import { workspaces } from '../state/workspaces'
import { workspaceName } from '../ui/access'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import { noticeText } from '../ui/notices'
import { accountNotice, noAccountNotice } from './accountNotice'
import ConsoleDialog from './ConsoleDialog.vue'
import PermissionRow from './PermissionRow.vue'

const text = edition().sync
const notice = inject(accountNotice, noAccountNotice)
const dialog = ref<'' | 'on' | 'off'>('')
const summary = computed(() => {
  if (!consent.loaded) return consent.failure ? '' : t('Checking…')
  return consent.consented
    ? t('On since {date}. Mailie keeps an index of your mail’s details. Turning sync off deletes it.', { date: dayStamp(consent.consentedAt) })
    : t('Off. Mailie stores nothing from your mail.')
})
const textOutdated = computed(consentTextOutdated)
/** The team mailboxes the person linked, which turning sync off deletes the index of for everyone who reads them. */
const teamMailboxes = computed(() => everyMailbox.list
  .filter(item => item.linked_by === session.user?.id)
  .map(item => ({ item, team: workspaces.list.find(workspace => workspace.id === item.workspace_id) }))
  .filter(({ team }) => team?.kind === 'team')
  .map(({ item, team }) => ({ id: item.id, email: item.email, team: workspaceName(team) })))
/** Which team mailboxes turning it off reaches is being read (again on every opening: links change): nothing is confirmed before they are named. */
const checking = computed(() => workspaces.supported && everyMailbox.loading)

function open() {
  if (!consent.loaded || consent.busy) return
  consent.problem = null
  notice.clear()
  dialog.value = consent.consented ? 'off' : 'on'
  // Which team mailboxes turning it off reaches, read now: links change.
  if (dialog.value === 'off' && workspaces.supported) void loadEveryMailbox()
}
function close() { if (!consent.busy) dialog.value = '' }
function reloadPage() { location.reload() }
async function change() {
  if (dialog.value === 'off' && checking.value) return
  const turningOn = dialog.value === 'on'
  const ok = turningOn ? await grantConsent() : await withdrawConsent()
  if (!ok) return
  dialog.value = ''
  // Said by the sync store already (state/accounts.ts notifySync).
  notice.show(() => noticeText({ kind: turningOn ? 'sync-on' : 'sync-off' }))
}
</script>

<template>
  <PermissionRow name="sync" icon="refresh" :icon-size="22" :label="t('Mail sync')" :summary="summary" :on="consent.consented" :disabled="!consent.loaded || !!consent.busy"
    :failure="consent.failure ? describe(consent.failure) : ''" @toggle="open" @retry="loadConsent" />

  <ConsoleDialog v-if="dialog === 'on'" :title="t('Turn on mail sync?')" :busy="consent.busy === 'grant'" wide @close="close">
    <p v-if="textOutdated" class="dim sync-outdated">{{ text.changedWhileOpen() }}</p>
    <component :is="text.component" v-else />
    <p v-if="consent.problem && !textOutdated" class="alert sync-problem" role="alert">{{ describe(consent.problem) }}</p>
    <div class="dialog-actions"><button class="ghost" type="button" :disabled="!!consent.busy" @click="close">{{ t('Not now') }}</button><button v-if="textOutdated" class="primary" type="button" @click="reloadPage">{{ t('Reload page') }}</button><button v-else class="primary" type="button" :disabled="!!consent.busy" @click="change">{{ consent.busy === 'grant' ? t('Turning on…') : t('Turn on sync') }}</button></div>
  </ConsoleDialog>

  <ConsoleDialog v-if="dialog === 'off'" :title="t('Turn off mail sync?')" :busy="consent.busy === 'withdraw'" @close="close">
    <div class="form-stack sync-confirm">
      <p class="dim">{{ t('Mailie stops syncing all your mailboxes and deletes everything it indexed for them: the details of every message, the folder list and its counts, and the log of changes. Your mailboxes stay connected, and nothing changes at your email provider.') }}</p>
      <div v-if="teamMailboxes.length" class="note team-warning">
        <p>{{ teamMailboxes.length === 1 ? t('This team mailbox was linked by you, so it syncs under your agreement. Its index is deleted too, for everyone in the team who reads it:') : t('These team mailboxes were linked by you, so they sync under your agreement. Their index is deleted too, for everyone in the team who reads them:') }}</p>
        <ul><li v-for="mailbox in teamMailboxes" :key="mailbox.id">{{ t('{email} in {team}', { email: mailbox.email, team: mailbox.team }) }}</li></ul>
        <p>{{ t('To keep a team’s index, have an owner or an admin of the team take the link over first.') }}</p>
      </div>
      <p v-else-if="checking" class="dim" role="status">{{ t('Checking which team mailboxes this reaches…') }}</p>
      <p v-else-if="workspaces.supported && everyMailbox.failure" class="note">{{ t('Mailie could not check which team mailboxes you linked. Any you did lose their index too, for everyone in the team who reads them.') }}</p>
      <p class="dim">{{ t('If you turn sync on again later, it starts over from the last 90 days.') }}</p>
      <p v-if="consent.problem" class="alert" role="alert">{{ describe(consent.problem) }}</p>
      <div class="dialog-actions"><button class="ghost" type="button" :disabled="!!consent.busy" @click="close">{{ t('Cancel') }}</button><button class="danger" type="button" :disabled="!!consent.busy || checking" @click="change">{{ consent.busy === 'withdraw' ? t('Deleting…') : t('Turn off and delete') }}</button></div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.sync-problem { margin: 14px 0 0; }
.sync-outdated { margin: 0 0 12px; }
.sync-confirm p { margin: 0; }
.team-warning { display: grid; gap: 8px; margin: 0; }
.team-warning ul { margin: 0; padding-left: 18px; display: grid; gap: 4px; color: var(--text); overflow-wrap: anywhere; }
</style>
