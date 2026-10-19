<script setup lang="ts">
// What the page draws over its phases, whoever draws them (App.vue, or a
// host's root beside state/appLifecycle.ts): the invitation to join a team,
// accepted signed in where teams are made here; the recovery code a sign-up
// or a recovery shows once; the step-up a write of a mailbox key or a grant
// waits for (state/stepUp.ts); the line that says this browser would not
// remember the session; and the live region everything is said through.
import { ref, watch } from 'vue'
import { edition } from '../edition'
import { invitation } from '../state/invitation'
import { session } from '../state/session'
import { t } from '../ui/i18n'
import InvitationDialog from './InvitationDialog.vue'
import LiveRegion from './LiveRegion.vue'
import RecoveryCodeDialog from './RecoveryCodeDialog.vue'
import StepUpPrompt from './StepUpPrompt.vue'

// finishing: a provider's return is being finished (state/appLifecycle.ts); nothing is offered over it.
defineProps<{ finishing: boolean }>()

/** Teams are made here, so an invitation may be one to join a team, accepted signed in. */
const teams = edition().teams === true
const rememberNoticeClosed = ref(false)
watch(() => session.phase, () => { rememberNoticeClosed.value = false })
</script>

<template>
  <InvitationDialog v-if="teams && session.phase === 'ready' && !finishing && invitation.pending" />
  <!-- Over whatever is on screen, signed in or not: a recovery whose sign-in failed still shows its new code. -->
  <RecoveryCodeDialog v-if="session.phase !== 'restoring'" />
  <!-- Over whatever dialog asked for it: the step-up a write of a mailbox key or a grant waits for (state/stepUp.ts). -->
  <StepUpPrompt v-if="session.phase === 'ready'" />

  <div v-if="session.phase === 'ready' && session.notRemembered && !rememberNoticeClosed" class="alert session-notice" role="status">
    <span>{{ t('This browser did not let Mailie remember your session. You will need to sign in again after reloading the page.') }}</span>
    <button class="ghost small" type="button" @click="rememberNoticeClosed = true">{{ t('Close') }}</button>
  </div>

  <!-- Outside every v-if, so it exists before anything is said through it. -->
  <LiveRegion />
</template>

<style scoped>
.session-notice { position: fixed; z-index: 100; top: 12px; left: 50%; transform: translateX(-50%); width: max-content; max-width: min(600px, calc(100vw - 32px)); display: flex; align-items: center; gap: 12px; }
</style>
