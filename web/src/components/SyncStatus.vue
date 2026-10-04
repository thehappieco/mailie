<script setup lang="ts">
// Where one account's sync stands: a label with a coloured dot, the first
// sync's progress, and a sentence. Only for an account that syncs; the card
// and the sheet show the account state's own words otherwise.
import { computed } from 'vue'
import type { Account } from '../api/types'
import { t } from '../ui/i18n'
import { syncView } from '../ui/sync'

const props = defineProps<{ account: Account; now?: number }>()
const view = computed(() => syncView(props.account, props.now))
</script>

<template>
  <div v-if="view" class="sync-status" :class="view.tone">
    <div class="sync-label"><i aria-hidden="true" /><strong>{{ view.label }}</strong><span v-if="view.progress !== undefined" class="sync-percent">{{ view.progress }}%</span></div>
    <!-- A native progress bar: its value is an attribute, so the strict CSP needs no inline style. -->
    <progress v-if="view.progress !== undefined" class="sync-progress" max="100" :value="view.progress" :aria-label="t('First sync progress')" />
    <p v-if="view.detail" class="sync-detail">{{ view.detail }}</p>
  </div>
</template>

<style scoped>
.sync-status { display: grid; gap: 8px; min-width: 0; }
.sync-label { display: flex; align-items: center; gap: 8px; font-size: 13px; }
.sync-label i { width: 8px; height: 8px; border-radius: 50%; background: var(--text-faint); flex-shrink: 0; }
.sync-label strong { font-weight: 600; }
.sync-percent { margin-left: auto; font-variant-numeric: tabular-nums; color: var(--text-dim); font-size: 12px; }
.live .sync-label i { background: var(--ok); }
.busy .sync-label i { background: var(--accent); }
.warn .sync-label i { background: var(--warn); }
.warn .sync-label strong { color: var(--warn-text); }
.bad .sync-label i { background: var(--danger); }
.bad .sync-label strong { color: var(--danger); }
.sync-detail { margin: 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; }
.sync-progress { appearance: none; -webkit-appearance: none; display: block; width: 100%; height: 6px; border: 0; border-radius: 999px; overflow: hidden; background: var(--bg-active); color: var(--accent); }
.sync-progress::-webkit-progress-bar { background: var(--bg-active); border-radius: 999px; }
.sync-progress::-webkit-progress-value { background: var(--accent); border-radius: 999px; transition: width .4s var(--ease); }
.sync-progress::-moz-progress-bar { background: var(--accent); border-radius: 999px; }
@media (prefers-reduced-motion: reduce) { .sync-progress::-webkit-progress-value { transition: none; } }
</style>
