<script setup lang="ts">
// Actions on the person's messages in the account section, switched like
// sync: on, after the edition's text of what actions change; off, after
// saying that nothing is deleted and they stop. What stops is what the
// person asks for: a workspace's API key given Act acts under the key terms,
// not under this switch, so nothing here says the mailboxes stop changing. Allowed to an older text,
// they are paused (the server refuses them): the row says so and offers the
// new text to agree to, or to turn them off.
import { computed, inject, ref } from 'vue'
import { edition } from '../edition'
import { actionsConsent, actionsOutdated, actionsTextOutdated, allowActions, loadActionsConsent, stopActions } from '../state/actionsConsent'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import { accountNotice, noAccountNotice } from './accountNotice'
import ConsoleDialog from './ConsoleDialog.vue'
import PermissionRow from './PermissionRow.vue'

const { actions: text, copy: words } = edition()
const notice = inject(accountNotice, noAccountNotice)
const dialog = ref<'' | 'on' | 'off'>('')
/** Allowed to an older text than the server asks about: paused until the person agrees again. */
const renewing = computed(actionsOutdated)
const summary = computed(() => {
  if (!actionsConsent.loaded) return actionsConsent.failure ? '' : t('Checking…')
  if (renewing.value) return text.changedSince(dayStamp(actionsConsent.consentedAt))
  return actionsConsent.consented
    ? t('Allowed since {date}. Mailie changes your mailbox only when you ask.', { date: dayStamp(actionsConsent.consentedAt) })
    : t('Off. Mailie does not change your mailboxes when you ask.')
})
const paused = computed(() => renewing.value ? { agree: t('Review and agree'), off: t('Turn off actions') } : null)
const textChanged = computed(actionsTextOutdated)

/** The switch turns them on or off, each after a dialog; a paused consent picks which. */
function open(which?: 'on' | 'off') {
  if (!actionsConsent.loaded || actionsConsent.busy) return
  actionsConsent.problem = null
  notice.clear()
  dialog.value = which ?? (actionsConsent.consented ? 'off' : 'on')
}
function close() { if (!actionsConsent.busy) dialog.value = '' }
function reloadPage() { location.reload() }
async function change() {
  const turningOn = dialog.value === 'on'
  const ok = turningOn ? await allowActions() : await stopActions()
  if (!ok) return
  dialog.value = ''
  const done = turningOn
    ? () => t('Actions are on. Mailie changes your mailbox only when you ask.')
    : () => t('Actions are off. Mailie no longer changes your mailboxes when you ask.')
  notice.show(done)
  announce(done())
}
</script>

<template>
  <PermissionRow name="actions" icon="pencil" :label="t('Actions on my messages')" :summary="summary" :on="actionsConsent.consented"
    :disabled="!actionsConsent.loaded || !!actionsConsent.busy" :paused="paused" :busy="!!actionsConsent.busy"
    :failure="actionsConsent.failure ? describe(actionsConsent.failure) : ''" @toggle="open()" @agree="open('on')" @off="open('off')" @retry="loadActionsConsent" />

  <ConsoleDialog v-if="dialog === 'on'" :title="renewing ? t('Actions on your messages: the terms changed') : t('Allow actions on your messages?')" :busy="actionsConsent.busy === 'grant'" wide @close="close">
    <p v-if="textChanged" class="dim sync-outdated">{{ text.changedWhileOpen() }}</p>
    <component :is="text.component" v-else />
    <p v-if="actionsConsent.problem && !textChanged" class="alert sync-problem" role="alert">{{ describe(actionsConsent.problem) }}</p>
    <div class="dialog-actions"><button class="ghost" type="button" :disabled="!!actionsConsent.busy" @click="close">{{ t('Not now') }}</button><button v-if="textChanged" class="primary" type="button" @click="reloadPage">{{ t('Reload page') }}</button><button v-else class="primary" type="button" :disabled="!!actionsConsent.busy" @click="change">{{ actionsConsent.busy === 'grant' ? t('Saving…') : renewing ? t('I agree') : t('Allow actions') }}</button></div>
  </ConsoleDialog>

  <ConsoleDialog v-if="dialog === 'off'" :title="t('Turn off actions?')" :busy="actionsConsent.busy === 'withdraw'" @close="close">
    <div class="form-stack sync-confirm">
      <p class="dim">{{ words.actionsOff() }}</p>
      <p class="dim">{{ t('You can allow actions again at any time.') }}</p>
      <p v-if="actionsConsent.problem" class="alert" role="alert">{{ describe(actionsConsent.problem) }}</p>
      <div class="dialog-actions"><button class="ghost" type="button" :disabled="!!actionsConsent.busy" @click="close">{{ t('Cancel') }}</button><button class="primary" type="button" :disabled="!!actionsConsent.busy" @click="change">{{ actionsConsent.busy === 'withdraw' ? t('Turning off…') : t('Turn off actions') }}</button></div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.sync-problem { margin: 14px 0 0; }
.sync-outdated { margin: 0 0 12px; }
.sync-confirm p { margin: 0; }
</style>
