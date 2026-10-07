<script setup lang="ts">
// One permission in the account section: what it is, where it stands, and a
// switch that turns it on or off, each after a dialog its owner shows. A
// consent given to an older text than the server asks about is paused, not
// on: in place of the switch, one button to read and agree to the new text
// and one to turn it off. When where it stands could not be read, an alert
// below offers to read it again.
import { t } from '../ui/i18n'
import AppIcon, { type IconName } from './AppIcon.vue'

const props = withDefaults(defineProps<{
  /** Names the row: its label is #<name>-switch-label, one per page. */
  name: string
  icon: IconName
  iconSize?: number
  label: string
  summary: string
  on: boolean
  /** The switch waits: the answer is not read yet, or a change is being made. */
  disabled: boolean
  /** Paused: the buttons' labels; with no agree, only turning it off is offered. */
  paused?: { agree?: string; off: string } | null
  busy?: boolean
  /** Why where it stands could not be read. */
  failure?: string
}>(), { iconSize: 21, paused: null, busy: false, failure: '' })
const emit = defineEmits<{ toggle: []; agree: []; off: []; retry: [] }>()
const labelID = `${props.name}-switch-label`
</script>

<template>
  <div class="security-summary sync-row">
    <span class="summary-icon"><AppIcon :name="icon" :size="iconSize" /></span>
    <span class="summary-text"><strong :id="labelID">{{ label }}</strong><small>{{ summary }}</small></span>
    <div v-if="paused" class="session-actions">
      <button v-if="paused.agree" class="primary small" type="button" aria-haspopup="dialog" :disabled="busy" @click="emit('agree')">{{ paused.agree }}</button>
      <button class="ghost small" type="button" aria-haspopup="dialog" :disabled="busy" @click="emit('off')">{{ paused.off }}</button>
    </div>
    <button v-else class="sync-switch" type="button" role="switch" :aria-checked="on" :aria-labelledby="labelID" aria-haspopup="dialog"
      :disabled="disabled" @click="emit('toggle')">
      <span class="switch-track" aria-hidden="true"><span class="switch-thumb" /></span><span class="switch-state">{{ on ? t('On') : t('Off') }}</span>
    </button>
  </div>
  <div v-if="failure" class="alert with-action" role="alert"><span>{{ failure }}</span><button class="ghost small" type="button" @click="emit('retry')">{{ t('Try again') }}</button></div>
</template>

<style scoped>
.security-summary { display: flex; align-items: center; gap: 14px; width: 100%; padding: 18px; text-align: left; border: 1px solid var(--console-border); border-radius: 14px; background: var(--bg-panel); color: var(--text-dim); flex-wrap: wrap; }
.summary-icon { display: grid; place-items: center; flex: none; width: 42px; height: 42px; border-radius: 12px; color: var(--accent); background: var(--accent-dim); }
.summary-text { flex: 1; min-width: 0; }
.summary-text strong { display: block; font-size: 14px; color: var(--text); }
.summary-text small { display: block; font-size: 12px; color: var(--text-dim); margin-top: 5px; line-height: 1.5; }
.sync-switch { display: inline-flex; align-items: center; gap: 10px; padding: 6px 4px; border-radius: 999px; color: var(--text-dim); font-size: 13px; font-weight: 600; }
.sync-switch:focus-visible { outline: 2px solid var(--console-accent); outline-offset: 2px; }
.switch-track { position: relative; width: 40px; height: 24px; border-radius: 999px; background: var(--bg-active); border: 1px solid var(--console-border); transition: background .15s var(--ease); flex: none; }
.switch-thumb { position: absolute; top: 2px; left: 2px; width: 18px; height: 18px; border-radius: 50%; background: var(--bg-panel); box-shadow: 0 1px 3px #0003; transition: transform .15s var(--ease); }
.sync-switch[aria-checked='true'] { color: var(--text); }
.sync-switch[aria-checked='true'] .switch-track { background: var(--accent); border-color: var(--accent); }
.sync-switch[aria-checked='true'] .switch-thumb { transform: translateX(16px); background: var(--on-accent); }
.sync-switch:disabled { opacity: .6; }
.session-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.session-actions button { display: inline-flex; align-items: center; gap: 6px; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
@media (max-width: 760px) {
  .sync-row .session-actions { width: 100%; padding-left: 56px; }
}
@media (prefers-reduced-motion: reduce) { .switch-track, .switch-thumb { transition: none; } }
@media (max-width: 600px) {
  .security-summary { padding: 15px; }
  .sync-row .session-actions { padding-left: 0; }
  .session-actions button { flex: 1; }
}
</style>
