<script setup lang="ts">
// The whole application is a phase, not a route: restoring a remembered
// session, signed out, or ready. The only URL the page cares about is the
// provider's return (/oauth/return), and it is taken out of the address bar
// before anything renders. Ready shows the edition's console (src/edition.ts).
// What runs around the phases is state/appLifecycle.ts's, and what is drawn
// over them components/AppOverlays.vue's, so a host that draws the phases in a
// frame of its own runs and draws the same. This root is the core's own
// frame: it owns the page's scrolling (ui/frame.ts).
import { edition } from './edition'
import { useAppLifecycle } from './state/appLifecycle'
import { invitation } from './state/invitation'
import { resetLink } from './state/resetLink'
import { session } from './state/session'
import { useFixedFrame } from './ui/frame'
import AppOverlays from './components/AppOverlays.vue'
import OAuthReturnView from './components/OAuthReturnView.vue'
import RestoringView from './components/RestoringView.vue'
import SignInView from './components/SignInView.vue'

const consoleView = edition().console
const { finishing } = useAppLifecycle()
useFixedFrame()
</script>

<template>
  <RestoringView v-if="session.phase === 'restoring'" />
  <SignInView v-else-if="session.phase === 'signed-out'" :invitation="invitation.pending" :reset="resetLink.pending" />
  <OAuthReturnView v-else-if="finishing" />
  <component :is="consoleView" v-else />
  <AppOverlays :finishing="finishing" />
</template>
