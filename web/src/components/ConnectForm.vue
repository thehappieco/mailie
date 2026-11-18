<script setup lang="ts">
// A mailbox connected by its address, as a form with phases (the sibling's
// pairing flow): the address (and the password for IMAP and iCloud, and the
// servers for IMAP), then either leave for the provider, wait for the
// provider, or wait for the server's test sign-in; once it is connected, its
// nickname and a team's agreement to sync it (NewMailbox). Closing an
// unfinished attempt removes the account it created, so nothing
// half-connected is left behind. It goes into the workspace shown, always
// named: a person connects into another workspace by showing it first.
//
// Google's and Microsoft's are linked at the provider from their buttons
// (ConnectButtons); this form serves them where a link at the provider
// cannot be made here (a server with only the installed client, a browser
// that keeps nothing for the trip), and Microsoft's address form, which the
// person asks for (byAddress: a shared mailbox, or an organization whose
// administrator approved only the mail permissions), with the way back to
// the account chooser. resume finishes an existing mailbox's authorization.
//
// A new link carries the mailbox's first key (docs/key-scheme.md section
// 12.11, state/accounts.ts), which needs the person's password from the last
// ten minutes, with two of them to spare: it is asked before anything moves
// (state/stepUp.ts), and closing it leaves the form as it was. Finishing an
// authorization sends no key: the mailbox has its own.
//
// An address removed from the workspace is kept there 30 days, and linking it
// again is refused (conflict) as for one connected already: to an owner of
// the workspace, the refusal offers recovering it, or deleting it for good
// (RemovedAddressOffer).
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { Account, ProviderID } from '../api/types'
import { edition } from '../edition'
import {
  accounts, cancelConnect, connect, connectAtProvider, connectOAuthAccount, connectPasswordAccount, linksAtProvider, resetConnect,
  resumeAuthorization, retryConnect,
} from '../state/accounts'
import { workspaces } from '../state/workspaces'
import { draftRequest, emptyDraft, signsInWithPassword, type AccountDraft } from '../ui/accountDraft'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { countdown } from '../ui/format'
import { t } from '../ui/i18n'
import { providerName } from '../ui/labels'
import AccountFields from './AccountFields.vue'
import AppIcon from './AppIcon.vue'
import NewMailbox from './NewMailbox.vue'
import RemovedAddressOffer from './RemovedAddressOffer.vue'

const props = defineProps<{ byAddress?: boolean; resume?: Account | null }>()
const emit = defineEmits<{ close: [] }>()
/** What the edition says about the access a mailbox grants, before it is granted; nothing when it has nothing to say. */
const accessNote = edition().legal?.accountAccess

/** The provider of the form: iCloud's once an Apple address typed into the IMAP form is pointed there, which the page around it follows. */
const provider = defineModel<ProviderID>('provider', { required: true })
const draft = ref<AccountDraft>(emptyDraft())
const now = ref(Date.now())
const body = ref<HTMLElement | null>(null)
let ticker: ReturnType<typeof setInterval> | undefined

const blocked = computed(() => connect.phase === 'starting' || connect.phase === 'redirecting')
const name = computed(() => providerName(connect.provider || provider.value))
/** The server tests a password before it saves anything; OAuth leaves for the provider instead. */
const password = computed(() => signsInWithPassword(provider.value))
/** The provider names the address: no field for it, and the link starts at the provider (Microsoft's way back from its address form). */
const atProvider = ref(false)
/** Microsoft links at the provider here, so its address form has the way back to the account chooser. */
const offersChooser = computed(() => !props.resume && provider.value === 'microsoft' && linksAtProvider('microsoft'))
/** The mailbox this attempt connected, as listed. */
const connected = computed(() => accounts.list.find(item => item.id === connect.accountID))
/** Whether the server offers iCloud, so an Apple address typed into the IMAP form is pointed there. */
const icloudOffered = computed(() => !accounts.providersLoaded || !!accounts.providers.find(item => item.id === 'icloud')?.password)

function opening(): string {
  return (connect.provider || provider.value) === 'microsoft' ? t('Opening Microsoft…') : t('Opening Google…')
}
function continueLabel(): string {
  return provider.value === 'microsoft' ? t('Continue to Microsoft') : t('Continue to Google')
}

/** From the IMAP form, for an address Apple hosts: the same address and password, with Apple's servers. */
function useICloud() {
  if (blocked.value || !icloudOffered.value) return
  provider.value = 'icloud'
}

/** The workspace shown, which the mailbox goes into; none on a server without workspaces. */
function into(): string | undefined {
  return workspaces.supported && workspaces.currentID ? workspaces.currentID : undefined
}

async function submit() {
  if (blocked.value) return
  const workspaceID = into()
  if (password.value) {
    const request = draftRequest(provider.value, draft.value)
    if (workspaceID) request.workspace_id = workspaceID
    const ok = await connectPasswordAccount(request)
    // The password is not kept once it has done its job.
    if (ok) draft.value.password = ''
    return
  }
  if (atProvider.value && (provider.value === 'gmail' || provider.value === 'microsoft')) {
    await connectAtProvider({ provider: provider.value, ...(workspaceID ? { workspaceID } : {}) })
    return
  }
  await connectOAuthAccount({ provider: provider.value, email: draft.value.email.trim(), ...(workspaceID ? { workspaceID } : {}) })
}

/** Microsoft's way back from its address form: the account chooser, as its button does. */
async function useChooser() {
  if (blocked.value || provider.value !== 'microsoft') return
  atProvider.value = true
  await submit()
  if (connect.phase === 'idle') atProvider.value = false
}

async function retry() {
  if (await retryConnect()) return
  // Nothing left to authorize: a resume would otherwise sit on "Preparing the sign-in…" for good.
  if (props.resume) { emit('close'); return }
  // Back to the form as it was filled, the address one, whatever was tried from it.
  atProvider.value = false
}

function close() {
  if (blocked.value) return
  if (connect.phase === 'done') resetConnect()
  else cancelConnect()
  emit('close')
}

/** The refusal names an address the workspace holds removed and kept, which its owner is offered back. */
const offersRemoved = computed(() => {
  const found = connect.failure
  if (connect.phase !== 'failed' || found?.code !== 'conflict') return false
  return found.op === 'add-account' || found.op === 'test-login' || found.op === 'test-login-icloud'
})
const removedShown = ref(false)
watch(() => connect.failure, () => { removedShown.value = false })
function recovered() {
  resetConnect()
  emit('close')
}
async function again() {
  if (await retryConnect()) return
  atProvider.value = false
  removedShown.value = false
}

watch(() => connect.phase, phase => {
  clearInterval(ticker)
  if (phase === 'waiting') ticker = setInterval(() => { now.value = Date.now() }, 1000)
  // Said through the live region: these paragraphs are created with their
  // text, which screen readers often skip. A failure is a role="alert"; the
  // new mailbox's heading takes the focus.
  switch (phase) {
    case 'starting': announce(password.value ? t('Testing the connection… This can take up to half a minute.') : t('Preparing the sign-in…')); break
    case 'redirecting': announce(opening()); break
    case 'waiting': announce(t('Waiting for authorization…')); break
  }
})

/**
 * Every phase (and a switch from the IMAP form to iCloud) replaces the control
 * that had focus, which would leave focus on <body> behind a modal. Move it to
 * what the new phase is for. Not on the first render: the dialog, or the
 * page, places the initial focus.
 */
watch([provider, () => connect.phase, removedShown], async () => {
  await nextTick()
  const root = body.value
  if (!root) return
  let target: HTMLElement | null = null
  switch (connect.phase) {
    case 'idle': target = root.querySelector<HTMLElement>('input:not([disabled])'); break
    case 'waiting': target = root.querySelector<HTMLElement>('a.primary'); break
    case 'failed': target = root.querySelector<HTMLElement>('.dialog-actions .primary') ?? root.querySelector<HTMLElement>('.dialog-actions button'); break
    // starting, redirecting: nothing to act on, and the live region says what is happening; done: the new mailbox's heading takes it.
  }
  target?.focus({ preventScroll: true })
})

onMounted(() => {
  resetConnect()
  if (props.resume) void resumeAuthorization(props.resume)
})

onBeforeUnmount(() => {
  clearInterval(ticker)
  // Unmounted while waiting (the person signed out, the panel went away): stop polling and clean up.
  if (connect.phase === 'waiting') cancelConnect()
})
</script>

<template>
  <div ref="body" class="connect-form">
    <!-- The address, and the servers for IMAP. -->
    <form v-if="!resume && (connect.phase === 'idle' || connect.phase === 'starting') && !atProvider" class="form-stack" @submit.prevent="submit">
      <AccountFields v-model:draft="draft" :provider="provider" :busy="blocked" :offer-icloud="icloudOffered" @use-icloud="useICloud" />
      <!-- Microsoft's address form keeps the way back to the account chooser. -->
      <div v-if="offersChooser" class="address-switch">
        <button class="linkish" type="button" :disabled="blocked" @click="useChooser">{{ t('Use the account I choose at Microsoft') }}</button>
      </div>
      <!-- Said before any access is granted: ahead of the provider's consent screen, or of the password test. -->
      <p v-if="accessNote" class="access-note"><AppIcon name="shield" :size="15" /><span><component :is="accessNote" /></span></p>
      <p v-if="connect.phase === 'starting'" class="progress">
        <span class="loading-spinner inline" aria-hidden="true" />
        {{ password ? t('Testing the connection… This can take up to half a minute.') : t('Preparing the sign-in…') }}
      </p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="blocked" @click="close">{{ t('Cancel') }}</button>
        <button class="primary" type="submit" :disabled="blocked">
          {{ connect.phase === 'starting' ? (password ? t('Testing the connection…') : t('Starting…')) : password ? t('Connect') : continueLabel() }}
        </button>
      </div>
    </form>

    <p v-else-if="connect.phase === 'starting' || connect.phase === 'idle'" class="progress">
      <span class="loading-spinner inline" aria-hidden="true" />{{ t('Preparing the sign-in…') }}
    </p>

    <!-- Leaving for the provider (web flow). -->
    <p v-else-if="connect.phase === 'redirecting'" class="progress">
      <span class="loading-spinner inline" aria-hidden="true" />{{ opening() }}
    </p>

    <!-- The person opens the provider themselves; this page waits. -->
    <div v-else-if="connect.phase === 'waiting'" class="waiting">
      <template v-if="connect.flow === 'device'">
        <p>{{ t('Open the page below, enter this code, and sign in as {email}.', { email: connect.email }) }}</p>
        <div class="user-code">{{ connect.userCode }}</div>
        <a class="primary" :href="connect.verificationURI" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">{{ t('Open {provider} sign-in', { provider: name }) }}<AppIcon name="external" :size="17" /></a>
      </template>
      <template v-else>
        <p>{{ t('Open the sign-in page, choose {email} and allow access. This window notices by itself when you are done.', { email: connect.email }) }}</p>
        <a class="primary" :href="connect.authURL" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">{{ t('Open {provider} sign-in', { provider: name }) }}<AppIcon name="external" :size="17" /></a>
        <p class="hint local-only"><AppIcon name="info" :size="15" />{{ t('This link only works when the Mailie server runs on this computer.') }}</p>
      </template>
      <!-- Not a live region: the countdown would be read every second. -->
      <p class="progress">
        <span class="loading-spinner inline" aria-hidden="true" />{{ t('Waiting for authorization…') }}
        <span v-if="connect.expiresAt" class="countdown">{{ t('Expires in {time}', { time: countdown(connect.expiresAt, now) }) }}</span>
      </p>
      <div class="dialog-actions"><button class="ghost" type="button" @click="close">{{ t('Cancel') }}</button></div>
    </div>

    <!-- Connected: its nickname, and a team's agreement to sync it. -->
    <NewMailbox v-else-if="connect.phase === 'done' && connected" :account="connected" @done="close" />
    <div v-else-if="connect.phase === 'done'" class="outcome">
      <div class="success"><AppIcon name="check" :size="18" /><span>{{ t('{email} is connected.', { email: connect.email }) }}</span></div>
      <div class="dialog-actions"><button class="primary" type="button" @click="close">{{ t('Done') }}</button></div>
    </div>

    <!-- Refused: maybe for an address removed from this workspace and kept, which its owner may recover. -->
    <div v-else-if="connect.phase === 'failed' && connect.failure" class="outcome">
      <RemovedAddressOffer v-if="offersRemoved" v-show="removedShown" :email="connect.email" @shown="removedShown = $event" @close="close" @recovered="recovered" @again="again" />
      <template v-if="!removedShown">
        <p class="alert" role="alert">{{ describe(connect.failure) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" @click="close">{{ t('Close') }}</button>
          <!-- A removed account cannot be tried again; closing is all there is. -->
          <button v-if="connect.failure.code !== 'not_found'" class="primary" type="button" @click="retry">{{ t('Try again') }}</button>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.connect-form { min-width: 0; }
.form-stack { display: grid; gap: 16px; }
.form-stack :deep(label) { display: grid; gap: 8px; font-size: 13px; }
.dialog-actions { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; margin-top: 6px; }
.progress { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin: 0; color: var(--text-dim); font-size: 13px; }
.countdown { margin-left: auto; font-variant-numeric: tabular-nums; font-size: 12px; }
.waiting, .outcome { display: grid; gap: 16px; }
.waiting > p { margin: 0; line-height: 1.55; font-size: 14px; }
.waiting .primary { justify-self: start; text-decoration: none; }
.local-only { display: flex; align-items: flex-start; gap: 6px; margin: -6px 0 0; }
.local-only .app-icon { margin-top: 1px; }
.access-note { display: flex; align-items: flex-start; gap: 6px; margin: 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; }
.access-note .app-icon { flex: none; margin-top: 1px; color: var(--accent); }
.access-note :deep(a) { text-underline-offset: 2px; }
/* A small secondary link, the sign-in card's kind (.linkish). */
.address-switch { display: grid; justify-items: start; margin-top: -8px; }
.address-switch .linkish { min-height: 32px; text-align: left; }
.user-code { justify-self: start; font-family: var(--mono); font-size: 28px; letter-spacing: .14em; padding: 10px 18px; border: 1px dashed var(--accent); border-radius: 12px; background: var(--accent-dim); color: var(--text); }
.outcome .alert { margin: 0; }
.success strong { font-weight: 650; }
@media (max-width: 760px) { .waiting .primary { justify-self: stretch; } }
</style>
