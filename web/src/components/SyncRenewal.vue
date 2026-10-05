<script setup lang="ts">
// Agreeing to the current text of mail sync, from where something waits on
// it: taking a team mailbox's link over, or connecting a mailbox to a team,
// for someone who agreed to an earlier text, which may not say who reads a
// team mailbox's index (ui/access.ts). The note says why; the text is shown
// whole before the person agrees, and when the server names another revision
// than the one this page carries, a reload is offered instead.
import { computed, ref } from 'vue'
import { edition } from '../edition'
import { consent, consentTextOutdated, grantConsent } from '../state/sync'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

defineProps<{ note: string }>()

const text = edition().sync
const reading = ref(false)
const outdated = computed(consentTextOutdated)

function open() { consent.problem = null; reading.value = true }
function close() { if (!consent.busy) reading.value = false }
function reload() { location.reload() }
async function agree() { if (await grantConsent()) reading.value = false }
</script>

<template>
  <div class="note sync-renewal">
    <p><AppIcon name="info" :size="15" /><span>{{ note }}</span></p>
    <button class="ghost small" type="button" aria-haspopup="dialog" :disabled="!!consent.busy" @click="open">{{ t('Read the current text…') }}</button>
  </div>
  <ConsoleDialog v-if="reading" :title="t('Mail sync: the terms changed')" :busy="consent.busy === 'grant'" wide @close="close">
    <p v-if="outdated" class="dim renewal-outdated">{{ text.changedWhileOpen() }}</p>
    <component :is="text.component" v-else />
    <p v-if="consent.problem && !outdated" class="alert renewal-problem" role="alert">{{ describe(consent.problem) }}</p>
    <div class="dialog-actions">
      <button class="ghost" type="button" :disabled="!!consent.busy" @click="close">{{ t('Cancel') }}</button>
      <button v-if="outdated" class="primary" type="button" @click="reload">{{ t('Reload page') }}</button>
      <button v-else class="primary" type="button" :disabled="!!consent.busy" @click="agree">{{ consent.busy === 'grant' ? t('Saving…') : t('I agree') }}</button>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.sync-renewal { display: grid; gap: 10px; justify-items: start; margin: 0; }
.sync-renewal p { display: flex; align-items: flex-start; gap: 7px; margin: 0; font-size: 13px; line-height: 1.5; }
.sync-renewal .app-icon { flex: none; margin-top: 2px; }
.renewal-outdated { margin: 0 0 12px; }
.renewal-problem { margin: 14px 0 0; }
</style>
