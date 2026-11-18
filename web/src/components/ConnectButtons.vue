<script setup lang="ts">
// Connecting a mailbox to the workspace shown, one button per provider the
// server offers (docs/console.md, "Connecting a mailbox"): Google's and
// Microsoft's go straight to the provider's account chooser through a link
// at the provider (state/accounts.ts connectAtProvider), after a step-up
// when the last sign-in is too old, with no address, no name and no
// workspace to choose: the mailbox goes into the workspace shown, always
// named, and its nickname, and a team's agreement to sync it, come once it
// is connected (NewMailbox). Microsoft keeps its address form one small link
// away (a shared mailbox, an organization whose administrator must approve
// Mailie); iCloud and another server open their form (form). A server with
// only the installed client's flow, and a browser that keeps nothing for
// the trip, ask for the address instead, as the form does.
//
// Drawn once, where the person may link into the workspace shown, and only
// once the workspaces are read: before that the page does not know which
// one it shows. The providers are read for the person; one that does not
// answer within PROVIDERS_WAIT_MS says so, and a late answer still lands.
// The edition's note about the access a mailbox grants is said beside the
// buttons, before any is granted.
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import type { ProviderID } from '../api/types'
import { edition } from '../edition'
import { accounts, connect, connectAtProvider, linksAtProvider, loadProviders, resetConnect } from '../state/accounts'
import type { Failure } from '../state/failure'
import { currentWorkspace, loadWorkspaces, workspaces } from '../state/workspaces'
import { workspaceName } from '../ui/access'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import { providerIcon } from '../ui/labels'
import AppIcon from './AppIcon.vue'

// quiet: drawn inside a card that says what it is for already (the first mailbox's), so its heading only names the group.
const props = defineProps<{ quiet?: boolean }>()
const emit = defineEmits<{ form: [provider: ProviderID, byAddress: boolean] }>()

/** How long the providers' read is waited for before the page says it could not read them; a later answer still lands. */
const PROVIDERS_WAIT_MS = 15_000

const accessNote = edition().legal?.accountAccess
const ids = { heading: useId(), status: useId(), alert: useId(), address: useId() }
const lineID = (id: ProviderID) => `${ids.heading}-${id}`
const shown = computed(currentWorkspace)
const inTeam = computed(() => shown.value?.kind === 'team')
const teamName = computed(() => workspaceName(shown.value))

const order: ProviderID[] = ['gmail', 'microsoft', 'icloud', 'imap']
/** Whether the server connects this provider for the person: a password for iCloud and IMAP, an OAuth flow for the others. */
function offered(id: ProviderID): boolean {
  const found = accounts.providers.find(item => item.id === id)
  if (!found) return false
  if (id === 'icloud' || id === 'imap') return found.password
  return found.oauth && found.flows.some(flow => flow === 'web' || flow === 'loopback' || flow === 'device')
}
const choices = computed(() => order.filter(offered))

function label(id: ProviderID): string {
  switch (id) {
    case 'gmail': return t('Add a Google account')
    case 'microsoft': return t('Add a Microsoft account')
    case 'icloud': return t('Add an iCloud account')
    case 'imap': return t('Other server (IMAP)')
  }
}
function line(id: ProviderID): string {
  switch (id) {
    case 'gmail': return t('Gmail or Google Workspace')
    case 'microsoft': return t('Microsoft 365 or Outlook')
    case 'icloud': return t('icloud.com, me.com, mac.com or an iCloud+ domain, with an app-specific password')
    case 'imap': return t('Any provider with IMAP and SMTP, signed in with a password.')
  }
}

/** The providers did not answer in time: said, with a way to ask again, until an answer lands. */
const waited = ref(false)
let timer: ReturnType<typeof setTimeout> | undefined
function waitForProviders() {
  clearTimeout(timer)
  waited.value = false
  if (!accounts.providersLoaded) timer = setTimeout(() => { waited.value = true }, PROVIDERS_WAIT_MS)
}
const providersFailure = computed<Failure | null>(() => {
  if (accounts.providersLoaded) return null
  return accounts.providersFailure ?? (waited.value ? { op: 'load-providers', code: 'unavailable' } : null)
})
function askProviders() {
  waitForProviders()
  void loadProviders()
}
onMounted(() => {
  if (!accounts.providersLoaded) askProviders()
})
onBeforeUnmount(() => clearTimeout(timer))

/** The provider pressed here, whose start this block says how it goes. */
const pressed = ref<ProviderID | ''>('')
const mine = computed(() => pressed.value !== '' && connect.provider === pressed.value)
const busy = computed(() => connect.phase === 'starting' || connect.phase === 'redirecting')
const status = computed(() => {
  if (!mine.value) return ''
  if (connect.phase === 'redirecting') return connect.provider === 'gmail' ? t('Opening Google…') : t('Opening Microsoft…')
  if (connect.phase === 'starting') return t('Preparing the sign-in…')
  return ''
})
const problem = computed(() => mine.value && connect.phase === 'failed' && connect.failure ? describe(connect.failure) : '')

async function press(id: ProviderID) {
  if (busy.value) return
  if (!linksAtProvider(id)) { emit('form', id, false); return }
  pressed.value = id
  await connectAtProvider({ provider: id, ...(workspaces.supported ? { workspaceID: workspaces.currentID } : {}) })
  // The step-up was closed: nothing was sent, and the buttons are back.
  if (connect.phase === 'idle') pressed.value = ''
}
function byAddress() {
  if (busy.value) return
  emit('form', 'microsoft', true)
}
function dismiss() {
  pressed.value = ''
  resetConnect()
}
</script>

<template>
  <section class="connect-buttons" :class="{ quiet: props.quiet }" role="group" :aria-labelledby="ids.heading">
    <h3 :id="ids.heading" :class="{ 'visually-hidden': props.quiet }">{{ t('Connect an email account') }}</h3>
    <div v-if="workspaces.failure && !workspaces.loaded" class="alert with-action" role="alert">
      <span>{{ describe(workspaces.failure) }}</span>
      <button class="ghost small" type="button" @click="loadWorkspaces">{{ t('Try again') }}</button>
    </div>
    <p v-else-if="!workspaces.loaded" class="dim" role="status">{{ t('Loading…|connect') }}</p>
    <div v-else-if="providersFailure" class="alert with-action" role="alert">
      <span>{{ describe(providersFailure) }}</span>
      <button class="ghost small" type="button" @click="askProviders">{{ t('Try again') }}</button>
    </div>
    <p v-else-if="!accounts.providersLoaded" class="dim" role="status">{{ t('Loading…|connect') }}</p>
    <p v-else-if="!choices.length" class="dim">{{ t('This server has no way to connect a mailbox yet.') }}</p>
    <template v-else>
      <p v-if="inTeam" class="hint team-line">{{ t('A mailbox you connect here belongs to {team}. It is connected with sync off; you can turn on sync for {team} once it is connected.', { team: teamName }) }}</p>
      <ul class="choices-list">
        <li v-for="id in choices" :key="id" :data-provider="id">
          <button class="provider-button" type="button" :disabled="busy" :aria-describedby="lineID(id)" @click="press(id)">
            <span class="provider-tile"><AppIcon :name="providerIcon(id)" :size="20" /></span>
            <span class="button-text"><strong>{{ label(id) }}</strong><small :id="lineID(id)">{{ line(id) }}</small></span>
            <AppIcon name="chevron-right" :size="18" />
          </button>
          <div v-if="id === 'microsoft' && linksAtProvider('microsoft')" class="address-switch">
            <button class="linkish" type="button" :disabled="busy" :aria-describedby="ids.address" @click="byAddress">{{ t('Enter the mailbox’s address instead') }}</button>
            <p :id="ids.address" class="hint">{{ t('For a shared mailbox, or when Microsoft says your organization’s administrator must approve Mailie.') }}</p>
          </div>
        </li>
      </ul>
      <!-- Said before any access is granted: ahead of the provider's consent screen, or of the password test. -->
      <p v-if="accessNote" class="access-note"><AppIcon name="shield" :size="15" /><span><component :is="accessNote" /></span></p>
    </template>
    <!-- One status line and one alert for the block, there from the start so a screen reader says what lands in them. -->
    <div class="connect-live">
      <p :id="ids.status" class="progress" role="status"><template v-if="status"><span class="loading-spinner inline" aria-hidden="true" />{{ status }}</template></p>
      <div :id="ids.alert" role="alert">
        <div v-if="problem" class="alert with-action">
          <span>{{ problem }}</span>
          <button class="ghost small" type="button" @click="dismiss">{{ t('Dismiss') }}</button>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.connect-buttons { display: grid; gap: 12px; padding: 18px; border: 1px solid var(--console-border); border-radius: 12px; background: var(--bg-raised); min-width: 0; }
.connect-buttons.quiet { padding: 0; border: 0; background: none; text-align: left; }
.connect-live { display: grid; gap: 8px; }
.connect-live:has(> .progress:empty):has(> [role=alert]:empty) { margin-top: -12px; }
.connect-buttons h3 { margin: 0; font-size: 14px; font-weight: 600; }
.connect-buttons p { margin: 0; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.team-line { font-size: 12.5px; }
.choices-list { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; grid-template-columns: repeat(2, minmax(0, 1fr)); }
@media (max-width: 760px) { .choices-list { grid-template-columns: minmax(0, 1fr); } }
.choices-list li { display: grid; gap: 4px; align-content: start; min-width: 0; }
.provider-button { display: flex; align-items: center; gap: 12px; width: 100%; min-height: 64px; padding: 12px 14px; text-align: left; border: 1px solid var(--line); border-radius: 12px; background: var(--bg-panel); color: var(--text-dim); }
.provider-button:hover:not(:disabled) { background: var(--bg-hover); }
.provider-button:disabled { opacity: .6; }
.button-text { flex: 1; min-width: 0; }
.button-text strong { display: block; font-size: 14px; color: var(--text); }
.button-text small { display: block; margin-top: 3px; font-size: 12px; line-height: 1.45; color: var(--text-dim); }
.address-switch { display: grid; justify-items: start; padding-left: 4px; }
.address-switch .linkish { min-height: 32px; text-align: left; }
.address-switch .hint { margin: 0; }
.access-note { display: flex; align-items: flex-start; gap: 6px; color: var(--text-dim); font-size: 12px; line-height: 1.5; }
.access-note .app-icon { flex: none; margin-top: 1px; color: var(--accent); }
.access-note :deep(a) { text-underline-offset: 2px; }
.progress { display: flex; align-items: center; gap: 8px; color: var(--text-dim); font-size: 13px; }
</style>
