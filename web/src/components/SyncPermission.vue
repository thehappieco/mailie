<script setup lang="ts">
// Mail sync in the account section, switched in both directions: on, after
// the edition's text of what sync stores (the same the accounts page shows),
// or, when that text changed while the page was open, a reload to read the
// new one; off, after saying that it deletes the index of the person's
// personal mailboxes. A team's mailboxes sync under the team's agreement,
// which this never touches: but for one the person connected before the
// server was updated, still tied to their agreement until an owner or an
// admin of its team gives it for the team, which the dialog says to whoever
// is in a team.
import { computed, inject, ref } from 'vue'
import { edition } from '../edition'
import { consent, consentTextOutdated, grantConsent, loadConsent, withdrawConsent } from '../state/sync'
import { workspaces } from '../state/workspaces'
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
/**
 * The person is in a team: what turning sync off reaches is then said apart
 * from the team's mailboxes, one of which they connected before the update
 * may still sync under their agreement.
 */
const inTeams = computed(() => workspaces.list.some(item => item.kind === 'team'))

function open() {
  if (!consent.loaded || consent.busy) return
  consent.problem = null
  notice.clear()
  dialog.value = consent.consented ? 'off' : 'on'
}
function close() { if (!consent.busy) dialog.value = '' }
function reloadPage() { location.reload() }
async function change() {
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
      <p v-if="!inTeams" class="dim">{{ t('Mailie stops syncing all your mailboxes and deletes everything it indexed for them: the details of every message, the folder list and its counts, and the log of changes. Your mailboxes stay connected, and nothing changes at your email provider.') }}</p>
      <p v-if="inTeams" class="dim">{{ t('Mailie stops syncing the mailboxes of your personal workspace and deletes everything it indexed for them: the details of every message, the folder list and its counts, and the log of changes. Your mailboxes stay connected, and nothing changes at your email provider.') }}</p>
      <p v-if="inTeams" class="note team-note">{{ t('Your teams’ mailboxes sync under each team’s agreement and keep syncing. The exception is a team mailbox you connected before this server was updated, until an owner or an admin of its team turns its sync on for the team: it still syncs under your agreement, so it stops too, and its index is deleted.') }}</p>
      <p class="dim">{{ t('If you turn sync on again later, it starts over from the last 90 days.') }}</p>
      <p v-if="consent.problem" class="alert" role="alert">{{ describe(consent.problem) }}</p>
      <div class="dialog-actions"><button class="ghost" type="button" :disabled="!!consent.busy" @click="close">{{ t('Cancel') }}</button><button class="danger" type="button" :disabled="!!consent.busy" @click="change">{{ consent.busy === 'withdraw' ? t('Deleting…') : t('Turn off and delete') }}</button></div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.sync-problem { margin: 14px 0 0; }
.sync-outdated { margin: 0 0 12px; }
.sync-confirm p { margin: 0; }
.team-note { margin: 0; }
</style>
