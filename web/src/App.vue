<script setup lang="ts">
// The whole application is a phase, not a route: restoring a remembered
// session, signed out, or ready. The only URL the page cares about is the
// provider's return (/oauth/return), and it is taken out of the address bar
// before anything renders. Ready shows the edition's console (src/edition.ts).
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { edition } from './edition'
import { t } from './ui/i18n'
import { describe } from './ui/errors'
import { forgetRemembered, recheckRemembered, restore, session, showSignIn } from './state/session'
import { captureOAuthReturn, pendingOAuthReturn, sweepOAuthStorage } from './state/oauthReturn'
import { finishOAuthReturn } from './state/accounts'
import { takeInvitation } from './ui/signupLink'
import SignInView from './components/SignInView.vue'
import OAuthReturnView from './components/OAuthReturnView.vue'
import LiveRegion from './components/LiveRegion.vue'

const consoleView = edition().console

// Both run before anything renders, so neither the provider's code nor an
// invitation code stays in the address bar for a moment longer than needed.
captureOAuthReturn()
const invitation = ref(takeInvitation())

// A provider start or return this tab never used is removed when its ten
// minutes are up, not merely ignored: it names a mailbox, or carries a code.
let oauthSweep: ReturnType<typeof setTimeout> | undefined
function sweepOAuth() {
  clearTimeout(oauthSweep)
  const next = sweepOAuthStorage()
  if (next !== null) oauthSweep = setTimeout(sweepOAuth, next + 50)
}
sweepOAuth()

const finishing = ref(false)
const rememberNoticeClosed = ref(false)
const mobileQuery = window.matchMedia('(max-width: 760px), (max-height: 500px) and (pointer: coarse)')

function updateViewport() {
  const viewport = window.visualViewport
  // Respect pinch zoom; the keyboard changes height without changing scale.
  if (viewport && viewport.scale !== 1) return
  document.documentElement.style.setProperty('--app-height', `${viewport?.height ?? window.innerHeight}px`)
  document.documentElement.style.setProperty('--app-top', `${viewport?.offsetTop ?? 0}px`)
}

// A session becomes ready after a restore or a sign-in; either way, a provider
// return captured on load is finished then, with that session's token.
watch(() => session.phase, async phase => {
  if (phase !== 'ready' || finishing.value || !pendingOAuthReturn()) return
  finishing.value = true
  try { await finishOAuthReturn() } finally { finishing.value = false }
})

watch(() => session.phase, phase => {
  rememberNoticeClosed.value = false
  // An invitation is spent by signing up; a later sign-out shows the plain sign-in form.
  if (phase === 'ready') invitation.value = null
})

/** A page back from the back/forward cache may be signed in with a login another tab has since cleared. */
function returnedToPage(event: PageTransitionEvent) {
  // Timers do not run while a page sits in the back/forward cache.
  sweepOAuth()
  if (event.persisted) void recheckRemembered()
}
function focused() { sweepOAuth(); void recheckRemembered() }

onMounted(() => {
  updateViewport()
  window.addEventListener('resize', updateViewport)
  mobileQuery.addEventListener('change', updateViewport)
  window.visualViewport?.addEventListener('resize', updateViewport)
  window.visualViewport?.addEventListener('scroll', updateViewport)
  window.addEventListener('pageshow', returnedToPage)
  window.addEventListener('focus', focused)
  if (invitation.value) showSignIn()
  else void restore()
})

onBeforeUnmount(() => {
  clearTimeout(oauthSweep)
  window.removeEventListener('resize', updateViewport)
  mobileQuery.removeEventListener('change', updateViewport)
  window.visualViewport?.removeEventListener('resize', updateViewport)
  window.visualViewport?.removeEventListener('scroll', updateViewport)
  window.removeEventListener('pageshow', returnedToPage)
  window.removeEventListener('focus', focused)
  document.documentElement.style.removeProperty('--app-height')
  document.documentElement.style.removeProperty('--app-top')
})
</script>

<template>
  <div v-if="session.phase === 'restoring'" class="empty app-loading" role="status" aria-live="polite">
    <div>
      <template v-if="!session.restoreFailed"><div class="loading-spinner" aria-hidden="true" /><div class="big">{{ t('Restoring your session…') }}</div></template>
      <template v-else>
        <p class="alert" role="alert">{{ describe({ op: 'restore', code: 'unavailable' }) }}</p>
        <div class="restore-actions"><button class="primary" type="button" @click="restore">{{ t('Try again') }}</button><button class="ghost" type="button" @click="forgetRemembered">{{ t('Sign in again') }}</button></div>
      </template>
    </div>
  </div>

  <SignInView v-else-if="session.phase === 'signed-out'" :invitation="invitation" />
  <OAuthReturnView v-else-if="finishing" />
  <component :is="consoleView" v-else />

  <div v-if="session.phase === 'ready' && session.notRemembered && !rememberNoticeClosed" class="alert session-notice" role="status">
    <span>{{ t('This browser did not let Mailie remember your session. You will need to sign in again after reloading the page.') }}</span>
    <button class="ghost small" type="button" @click="rememberNoticeClosed = true">{{ t('Close') }}</button>
  </div>

  <!-- Outside every v-if, so it exists before anything is said through it. -->
  <LiveRegion />
</template>

<style scoped>
.session-notice { position: fixed; z-index: 100; top: 12px; left: 50%; transform: translateX(-50%); width: max-content; max-width: min(600px, calc(100vw - 32px)); display: flex; align-items: center; gap: 12px; }
.restore-actions { display: flex; flex-wrap: wrap; justify-content: center; gap: 10px; }
.app-loading .alert { max-width: 420px; }
</style>
