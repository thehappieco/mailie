<script setup lang="ts">
// The person's sessions, as a row of the account section (AccountPanel) or of
// any page a host draws the person's own settings on: how long this one
// lasts, signing out here, and signing out everywhere, which ends every
// session of the account on every browser and asks once more before it does.
// Signing out goes through the core's sign-out (state/session.ts), which
// calls the edition's signedOut once it is done.
import { computed, ref } from 'vue'
import { failure, type Failure } from '../state/failure'
import { session, signOut } from '../state/session'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

// Each one unset is the core's words: "Sessions", how long this session lasts and what signing out everywhere ends, and "Sign out".
const props = defineProps<{ title?: string; description?: string; signOutLabel?: string }>()

const heading = computed(() => props.title || t('Sessions'))
const detail = computed(() => props.description || t('This session lasts until {date}. Signing out everywhere ends the sessions of your account on every browser.', { date: dayStamp(session.expiresAt) }))
const leaveLabel = computed(() => props.signOutLabel || t('Sign out'))

const everywhere = ref(false)
const leaving = ref(false)
const leaveProblem = ref<Failure | null>(null)
async function leave(all: boolean) {
  if (leaving.value) return
  leaving.value = true; leaveProblem.value = null
  try { await signOut({ everywhere: all }) }
  catch (error) { leaveProblem.value = failure('sign-out', error) }
  finally { leaving.value = false; everywhere.value = false }
}
</script>

<template>
  <div class="sessions-row">
    <div class="security-summary sessions">
      <span class="summary-icon"><AppIcon name="shield" :size="22" /></span>
      <span class="summary-text">
        <strong>{{ heading }}</strong>
        <small>{{ detail }}</small>
      </span>
      <div class="session-actions">
        <template v-if="everywhere">
          <button class="danger small" type="button" :disabled="leaving" @click="leave(true)">{{ leaving ? t('Signing out…') : t('Sign out everywhere now') }}</button>
          <button class="ghost small" type="button" :disabled="leaving" @click="everywhere = false">{{ t('Cancel') }}</button>
        </template>
        <template v-else>
          <button class="ghost small" type="button" :disabled="leaving" @click="leave(false)"><AppIcon name="logout" :size="16" />{{ leaveLabel }}</button>
          <button class="ghost small" type="button" :disabled="leaving" @click="everywhere = true">{{ t('Sign out everywhere') }}</button>
        </template>
      </div>
    </div>
    <p v-if="leaveProblem" class="alert" role="alert">{{ describe(leaveProblem) }}</p>
  </div>
</template>

<style scoped>
.sessions-row { display: grid; gap: 12px; }
.security-summary { display: flex; flex-wrap: wrap; align-items: center; gap: 14px; width: 100%; padding: 18px; text-align: left; border: 1px solid var(--console-border); border-radius: 14px; background: var(--bg-panel); color: var(--text-dim); }
.summary-icon { display: grid; place-items: center; flex: none; width: 42px; height: 42px; border-radius: 12px; color: var(--accent); background: var(--accent-dim); }
.summary-text { flex: 1; min-width: 0; }
.summary-text strong { display: block; font-size: 14px; color: var(--text); }
.summary-text small { display: block; font-size: 12px; color: var(--text-dim); margin-top: 5px; line-height: 1.5; }
.session-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.session-actions button { display: inline-flex; align-items: center; gap: 6px; }
.alert { margin: 0; }
@media (max-width: 760px) {
  .session-actions { width: 100%; padding-left: 56px; }
}
@media (max-width: 600px) {
  .security-summary { padding: 15px; }
  .session-actions { padding-left: 0; }
  .session-actions button { flex: 1; }
}
</style>
