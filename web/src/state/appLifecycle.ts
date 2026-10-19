// What the page does around its phases, whoever draws them: the core's own
// root (App.vue), or a host's root that draws the same phases inside a frame
// of its own (docs/console.md, "Editions"). The phases are the session's
// (state/session.ts): restoring a remembered session, signed out, or ready;
// and, once ready, finishing a provider's return the page captured
// (finishing: components/OAuthReturnView.vue is drawn meanwhile).
//
// Before anything renders, neither the provider's code, an invitation code
// nor a reset code stays in the address bar a moment longer than needed:
// they are captured and taken out of it (captureOAuthReturn may have run
// earlier already, from an edition's entry: it is the same either way). A
// provider start or return this tab never used is removed when its ten
// minutes are up. Right after a sign-in in this page (never a restored
// session), the first key of each mailbox the person reads without one,
// while the sign-in still counts as a step-up (docs/key-scheme.md section
// 12.14). The visual viewport's height and offset are written on the
// document (--app-height and --app-top), for the core's own frame and a
// full-screen dialog on a phone. Once mounted, a remembered session is
// restored, unless a reset link, or an invitation that only ever starts an
// account here, asks for the signed-out screen; and when the tab comes back
// (focus, the back/forward cache), the remembered session is checked again.

import { onBeforeUnmount, onMounted, ref, watch, type Ref } from 'vue'
import { edition } from '../edition'
import { takeInvitation, takeReset } from '../ui/signupLink'
import { finishOAuthReturn } from './accounts'
import { dropInvitation, holdInvitation, invitation } from './invitation'
import { firstKeysAfterSignIn } from './mailboxKeys'
import { captureOAuthReturn, pendingOAuthReturn, sweepOAuthStorage } from './oauthReturn'
import { holdReset, resetLink } from './resetLink'
import { recheckRemembered, restore, session, showSignIn } from './session'

export interface AppLifecycle {
  /** A provider's return is being finished with the session's token: draw components/OAuthReturnView.vue in place of the console. */
  finishing: Ref<boolean>
  /** Teams are made here (the edition's teams), so an invitation may be one to join a team, accepted signed in. */
  teams: boolean
}

/**
 * Runs the page's life around its phases while the calling component is
 * mounted. Call it once, from the setup of the page's root, before anything
 * renders; draw the phases from session.phase and finishing, and
 * components/AppOverlays.vue beside them.
 */
export function useAppLifecycle(): AppLifecycle {
  const teams = edition().teams === true

  // A reset link first: it carries an address too.
  captureOAuthReturn()
  holdReset(takeReset())
  holdInvitation(takeInvitation())

  // A provider start or return this tab never used is removed when its ten
  // minutes are up, not merely ignored: it names a mailbox, or carries a code.
  let oauthSweep: ReturnType<typeof setTimeout> | undefined
  function sweepOAuth() {
    clearTimeout(oauthSweep)
    const next = sweepOAuthStorage()
    if (next !== null) oauthSweep = setTimeout(sweepOAuth, next + 50)
  }
  sweepOAuth()

  const stopFirstKeys = firstKeysAfterSignIn()

  const finishing = ref(false)
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

  // An invitation is spent by signing up; a later sign-out shows the plain
  // sign-in form. Where teams are made here, a sign-in keeps it, to be
  // accepted in the console (the sign-up drops it, having spent it).
  watch(() => session.phase, phase => { if (phase === 'ready' && !teams) dropInvitation() })

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
    // An invitation link starts a new account, even in a browser that
    // remembers another one; where teams are made here, it may be one to join
    // a team, so a remembered session is restored and asked to accept it. A
    // reset link always starts signed out: using it ends every session anyway.
    if (resetLink.pending || (invitation.pending && !teams)) showSignIn()
    else void restore()
  })

  onBeforeUnmount(() => {
    stopFirstKeys()
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

  return { finishing, teams }
}
