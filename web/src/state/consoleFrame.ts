// What a frame keeps going while the console is on screen, whoever draws the
// frame: the core's own (components/ConsoleShell.vue), or a host's around
// the core's panels. It reads the person's workspaces (again by itself, after
// a first read that failed: every list waits for them), the mailboxes of the
// one shown, the providers, and where the person stands on sync and on
// actions, and keeps the event stream open; and it says what changed about
// the person's workspaces: one they are no longer in, or one they just
// joined. Sections, navigation and what is drawn stay the frame's.

import { computed, onBeforeUnmount, onMounted, watch, type ComputedRef } from 'vue'
import { accounts, loadAccounts, loadProviders } from './accounts'
import { actionsConsent, loadActionsConsent } from './actionsConsent'
import { dismissJoined, invitation } from './invitation'
import { startLive, stopLive, wakeLive } from './live'
import { consent, loadConsent } from './sync'
import { currentWorkspace, dismissLost, loadWorkspaces, retryWorkspaces, workspaces } from './workspaces'
import { workspaceName, workspaceRoleLabel } from '../ui/access'
import { announce } from '../ui/announce'
import { t } from '../ui/i18n'

export interface ConsoleFrame {
  /**
   * What changed about the person's workspaces, in a sentence: the one shown
   * went away from under them (and which one is shown now), or they joined a
   * team. Empty when there is nothing to say. Said by the live region
   * (ui/announce.ts) as it appears; the frame draws it above the section.
   */
  workspaceNotice: ComputedRef<string>
  /** Says nothing more about it. */
  dismissWorkspaceNotice(): void
}

/**
 * Keeps the console going while the calling component is mounted, and
 * answers the line about the person's workspaces. Call it from the setup of
 * the component that draws the frame, once the person is signed in.
 */
export function useConsoleFrame(): ConsoleFrame {
  const workspaceNotice = computed(() => {
    if (workspaces.lost) return t('You are no longer a member of {name}. This page now shows {current}.', { name: workspaceName(workspaces.lost), current: workspaceName(currentWorkspace()) })
    if (invitation.joined) return t('You joined {name} as {role}. Its mailboxes appear here as you are given access to them.', { name: workspaceName(invitation.joined), role: workspaceRoleLabel(invitation.joined.role) })
    return ''
  })
  watch(workspaceNotice, words => { if (words) announce(words) })

  // A network that comes back cuts the event stream's reconnection wait short.
  function online() { wakeLive() }

  let stopRetrying: (() => void) | null = null
  let stopStreamWatch: (() => void) | null = null
  onMounted(() => {
    stopRetrying = retryWorkspaces()
    if (!workspaces.loaded) void loadWorkspaces()
    if (!accounts.loaded) void loadAccounts()
    if (!accounts.providersLoaded) void loadProviders()
    if (!consent.loaded) void loadConsent()
    if (!actionsConsent.loaded) void loadActionsConsent()
    // The stream narrows to the workspace shown, so it opens once that is known.
    stopStreamWatch = watch(() => workspaces.loaded, loaded => { if (loaded) startLive() }, { immediate: true })
    window.addEventListener('online', online)
  })
  onBeforeUnmount(() => {
    stopRetrying?.()
    stopStreamWatch?.()
    window.removeEventListener('online', online)
    stopLive()
  })

  return { workspaceNotice, dismissWorkspaceNotice() { dismissLost(); dismissJoined() } }
}
