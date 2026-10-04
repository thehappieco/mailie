<script setup lang="ts">
// The question asked before anything from a person's mail is stored, with the
// edition's text of what sync keeps. Shown on the accounts page until they
// answer; "Not now" hides it for this visit only. When the server names
// another revision than the text this page carries, the card offers a reload
// instead of the agree button: agreeing here would be to a text not shown.
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { edition } from '../edition'
import { consent, consentTextOutdated, dismissConsent, grantConsent } from '../state/sync'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const text = edition().sync

const heading = ref<HTMLElement | null>(null)
/** Agreed before, to an older text: sync stays on, and the new text asks again. */
const renewing = computed(() => consent.consented && consent.version !== consent.currentVersion)
const outdated = computed(consentTextOutdated)

// A sheet's "Turn on sync…" brings the card back and sends the reader here:
// after the render that closes the sheet, whose own focus return comes first.
function takeFocus() {
  if (!consent.focusPending) return
  void nextTick(() => {
    consent.focusPending = false
    heading.value?.focus()
  })
}
watch(() => consent.focusPending, takeFocus)
onMounted(takeFocus)

async function agree() { await grantConsent() }
function reload() { location.reload() }
</script>

<template>
  <section class="consent-card" aria-labelledby="sync-consent-title">
    <div class="consent-head">
      <span class="consent-icon"><AppIcon name="refresh" :size="21" /></span>
      <div class="grow">
        <h2 id="sync-consent-title" ref="heading" tabindex="-1">{{ renewing ? t('Mail sync: the terms changed') : t('Turn on mail sync?') }}</h2>
        <p v-if="outdated" class="dim">{{ text.changedWhileOpen() }}</p>
        <p v-else-if="renewing" class="dim">{{ text.changedSince(dayStamp(consent.consentedAt)) }}</p>
      </div>
    </div>
    <component :is="text.component" v-if="!outdated" />
    <p v-if="consent.problem && !outdated" class="alert" role="alert">{{ describe(consent.problem) }}</p>
    <div class="consent-actions">
      <button class="ghost" type="button" :disabled="!!consent.busy" @click="dismissConsent">{{ t('Not now') }}</button>
      <button v-if="outdated" class="primary" type="button" @click="reload">{{ t('Reload page') }}</button>
      <button v-else class="primary" type="button" :disabled="!!consent.busy" @click="agree">
        {{ renewing ? (consent.busy === 'grant' ? t('Saving…') : t('I agree')) : consent.busy === 'grant' ? t('Turning on…') : t('Turn on sync') }}
      </button>
    </div>
  </section>
</template>

<style scoped>
.consent-card { display: grid; gap: 14px; padding: 22px; background: var(--bg-raised); border: 1px solid var(--console-border); border-left: 3px solid var(--accent); border-radius: 12px; min-width: 0; }
.consent-head { display: flex; align-items: flex-start; gap: 14px; }
.consent-head h2 { margin: 2px 0 0; font-size: 16px; font-weight: 650; }
.consent-head h2:focus { outline: none; }
.consent-head p { margin: 6px 0 0; }
.consent-icon { display: grid; place-items: center; flex: none; width: 40px; height: 40px; border-radius: 12px; background: var(--accent-dim); color: var(--accent); }
.consent-card .alert { margin: 0; }
.consent-actions { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; }
@media (max-width: 600px) {
  .consent-card { padding: 17px; }
  .consent-actions button { flex: 1; }
}
</style>
