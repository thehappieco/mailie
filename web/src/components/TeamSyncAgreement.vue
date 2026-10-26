<script setup lang="ts">
// The team's agreement to the sync text, as both acts that give it show it
// (docs/workspaces.md, "Consents" and "Pausing a mailbox's sync"): turning a
// team mailbox's sync on (TeamMailboxSync) and resuming a paused one
// (SyncPause), each on the team's behalf, to the revision of the edition's
// text the page carries. Its lead says what agreeing does; the text is the
// edition's, drawn for the team. When the server already names another
// revision, the text changed while the page was open: it says so instead,
// and the dialog around it offers a reload in place of agreeing. The dialogs
// keep their own titles, buttons, busy states and failures.
import { computed, onMounted } from 'vue'
import { edition } from '../edition'
import { consent, consentTextOutdated, loadConsent } from '../state/sync'

const props = defineProps<{ team: string; lead: string }>()

const text = edition().sync
const outdated = computed(consentTextOutdated)

onMounted(() => { if (!consent.loaded && !consent.loading) void loadConsent() })
</script>

<template>
  <div class="team-sync-agreement">
    <p v-if="outdated" class="dim team-sync-outdated">{{ text.changedWhileOpen() }}</p>
    <template v-else>
      <p class="team-sync-lead">{{ props.lead }}</p>
      <component :is="text.component" :team="props.team" />
    </template>
  </div>
</template>

<style scoped>
.team-sync-lead { margin: 0 0 12px; font-size: 13px; line-height: 1.55; color: var(--text); font-weight: 600; }
.team-sync-outdated { margin: 0 0 12px; }
</style>
