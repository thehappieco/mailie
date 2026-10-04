<script setup lang="ts">
// Connecting a mailbox, as a dialog with phases (the sibling's pairing flow):
// choose the provider, fill in the address (and the password for IMAP and
// iCloud, and the servers for IMAP), then either leave for the provider, wait
// for the provider, or wait for the server's test sign-in. Closing an unfinished attempt removes the account it
// created, so nothing half-connected is left behind.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { Account, ProviderID } from '../api/types'
import { edition } from '../edition'
import { accounts, cancelConnect, connect, connectOAuthAccount, connectPasswordAccount, resetConnect, resumeAuthorization, retryConnect } from '../state/accounts'
import { draftRequest, emptyDraft, signsInWithPassword, type AccountDraft } from '../ui/accountDraft'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { countdown } from '../ui/format'
import { t } from '../ui/i18n'
import { providerChoice, providerIcon, providerName } from '../ui/labels'
import { noticeText } from '../ui/notices'
import AccountFields from './AccountFields.vue'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const props = defineProps<{ resume: Account | null; returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ close: [] }>()
/** What the edition says about the access a mailbox grants, before it is granted; nothing when it has nothing to say. */
const accessNote = edition().legal?.accountAccess

type Step = 'choose' | 'details'
const step = ref<Step>(props.resume ? 'details' : 'choose')
const provider = ref<ProviderID | ''>(props.resume?.provider ?? '')
const draft = ref<AccountDraft>(emptyDraft())
const now = ref(Date.now())
const body = ref<HTMLElement | null>(null)
let ticker: ReturnType<typeof setInterval> | undefined

const options: ProviderID[] = ['gmail', 'microsoft', 'icloud', 'imap']
const blocked = computed(() => connect.phase === 'starting' || connect.phase === 'redirecting')
const name = computed(() => providerName(connect.provider || provider.value))
/** The server tests a password before it saves anything; OAuth leaves for the provider instead. */
const password = computed(() => signsInWithPassword(provider.value))
/** Whether the new mailbox starts syncing at once: its owner has turned sync on. */
const syncing = computed(() => accounts.list.find(item => item.id === connect.accountID)?.sync.enabled ?? false)
const connected = computed(() => noticeText({ kind: 'connected', email: connect.email, syncing: syncing.value }))

/**
 * Whether the server can connect a provider for this person. Until the list
 * has loaded (or if it could not be), every option stays open: the server is
 * the authority and will say no itself.
 */
function available(id: ProviderID): boolean {
  if (!accounts.providersLoaded) return true
  const found = accounts.providers.find(item => item.id === id)
  if (!found) return false
  if (signsInWithPassword(id)) return found.password
  return found.oauth && found.flows.some(flow => flow === 'web' || flow === 'loopback' || flow === 'device')
}

const title = computed(() => {
  if (props.resume) return t('Finish authorization')
  if (step.value === 'choose' || !provider.value) return t('Connect an email account')
  return providerChoice(provider.value)
})

const subtitle = computed(() => {
  if (props.resume) return props.resume.email
  if (step.value === 'choose') return t('Choose where the mailbox is hosted.')
  if (provider.value === 'imap') return t('Mailie signs in to the IMAP server with these details before it saves anything. The SMTP server is not tested.')
  if (provider.value === 'icloud') return t('Mailie signs in to iCloud with this address and password before it saves anything.')
  return t('You will sign in at {provider} to let Mailie read and send mail for this address.', { provider: name.value })
})

function choose(id: ProviderID) {
  if (!available(id)) return
  provider.value = id
  step.value = 'details'
}

/** From the IMAP form, for an address Apple hosts: the same address and password, with Apple's servers. */
function useICloud() {
  if (blocked.value || !available('icloud')) return
  provider.value = 'icloud'
}

function back() {
  if (blocked.value) return
  resetConnect()
  if (props.resume) { emit('close'); return }
  step.value = 'choose'
}

async function submit() {
  if (blocked.value || !provider.value) return
  if (password.value) {
    const ok = await connectPasswordAccount(draftRequest(provider.value, draft.value))
    // The password is not kept once it has done its job.
    if (ok) draft.value.password = ''
    return
  }
  await connectOAuthAccount({ provider: provider.value, email: draft.value.email.trim(), displayName: draft.value.displayName.trim() })
}

async function retry() {
  if (await retryConnect()) return
  // Nothing left to authorize: a resume dialog would otherwise sit on
  // "Preparing the sign-in…" for good.
  if (props.resume) emit('close')
  else step.value = 'details'
}

function close() {
  if (blocked.value) return
  if (connect.phase === 'done') resetConnect()
  else cancelConnect()
  emit('close')
}

watch(() => connect.phase, phase => {
  clearInterval(ticker)
  if (phase === 'waiting') ticker = setInterval(() => { now.value = Date.now() }, 1000)
  // Said through the live region: these paragraphs are created with their
  // text, which screen readers often skip. A failure is a role="alert".
  switch (phase) {
    case 'starting': announce(password.value ? t('Testing the connection… This can take up to half a minute.') : t('Preparing the sign-in…')); break
    case 'redirecting': announce(t('Opening {provider}…', { provider: name.value })); break
    case 'waiting': announce(t('Waiting for authorization…')); break
    case 'done': announce(connected.value); break
  }
})

/**
 * Every step (and a switch from the IMAP form to iCloud) replaces the control
 * that had focus, which would leave focus on <body> behind the modal. Move it to what the new step is for. Not on the
 * first render: showModal() places the initial focus.
 */
watch([step, provider, () => connect.phase], async () => {
  await nextTick()
  const root = body.value
  if (!root) return
  let target: HTMLElement | null = null
  switch (connect.phase) {
    case 'idle': target = root.querySelector<HTMLElement>('input:not([disabled]), .provider-option:not([disabled])'); break
    case 'waiting': target = root.querySelector<HTMLElement>('a.primary'); break
    case 'done':
    case 'failed': target = root.querySelector<HTMLElement>('.dialog-actions .primary') ?? root.querySelector<HTMLElement>('.dialog-actions button'); break
    // starting, redirecting: nothing to act on, and the live region says what is happening.
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
  <ConsoleDialog :title="title" :subtitle="subtitle" :busy="blocked" :return-focus="returnFocus" @close="close">
    <div ref="body">
      <!-- 1. Where is the mailbox? -->
      <div v-if="step === 'choose' && connect.phase === 'idle'" class="provider-options">
        <p v-if="accounts.providersFailure" class="hint">{{ describe(accounts.providersFailure) }}</p>
        <button v-for="id in options" :key="id" type="button" class="provider-option" :disabled="!available(id)" @click="choose(id)">
          <span class="provider-tile"><AppIcon :name="providerIcon(id)" :size="21" /></span>
          <span class="option-text">
            <strong>{{ providerChoice(id) }}</strong>
            <small v-if="!available(id)">{{ t('Not configured on this server') }}</small>
            <small v-else-if="id === 'icloud'">{{ t('Addresses at icloud.com, me.com or mac.com, and custom domains on iCloud+. Signed in with an app-specific password.') }}</small>
            <small v-else-if="id === 'imap'">{{ t('Any provider with IMAP and SMTP, signed in with a password.') }}</small>
            <small v-else>{{ t('You sign in at the provider. Mailie never sees your password.') }}</small>
          </span>
          <AppIcon name="chevron-right" :size="18" />
        </button>
      </div>

      <!-- 2. The address, and the servers for IMAP. -->
      <form v-else-if="!resume && (connect.phase === 'idle' || connect.phase === 'starting') && provider" class="form-stack" @submit.prevent="submit">
        <AccountFields v-model:draft="draft" :provider="provider" :busy="blocked" :offer-icloud="available('icloud')" @use-icloud="useICloud" />
        <!-- Said before any access is granted: ahead of the provider's consent screen, or of the password test. -->
        <p v-if="accessNote" class="access-note"><AppIcon name="shield" :size="15" /><span><component :is="accessNote" /></span></p>
        <p v-if="connect.phase === 'starting'" class="progress">
          <span class="loading-spinner inline" aria-hidden="true" />
          {{ password ? t('Testing the connection… This can take up to half a minute.') : t('Preparing the sign-in…') }}
        </p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="blocked" @click="back">{{ t('Back') }}</button>
          <button class="primary" type="submit" :disabled="blocked">
            {{ connect.phase === 'starting' ? (password ? t('Testing the connection…') : t('Starting…')) : password ? t('Connect') : t('Continue to {provider}', { provider: name }) }}
          </button>
        </div>
      </form>

      <p v-else-if="connect.phase === 'starting' || connect.phase === 'idle'" class="progress">
        <span class="loading-spinner inline" aria-hidden="true" />{{ t('Preparing the sign-in…') }}
      </p>

      <!-- 3a. Leaving for the provider (web flow). -->
      <p v-else-if="connect.phase === 'redirecting'" class="progress">
        <span class="loading-spinner inline" aria-hidden="true" />{{ t('Opening {provider}…', { provider: name }) }}
      </p>

      <!-- 3b. The person opens the provider themselves; this page waits. -->
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

      <!-- 4. Done. -->
      <div v-else-if="connect.phase === 'done'" class="outcome">
        <div class="success"><AppIcon name="check" :size="18" /><span><strong>{{ t('Account connected') }}</strong><br />{{ connected }}</span></div>
        <div class="dialog-actions"><button class="primary" type="button" @click="close">{{ t('Done') }}</button></div>
      </div>

      <!-- 5. Failed. -->
      <div v-else-if="connect.phase === 'failed' && connect.failure" class="outcome">
        <p class="alert" role="alert">{{ describe(connect.failure) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" @click="close">{{ t('Close') }}</button>
          <!-- A removed account cannot be tried again; closing is all there is. -->
          <button v-if="connect.failure.code !== 'not_found'" class="primary" type="button" @click="retry">{{ t('Try again') }}</button>
        </div>
      </div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.provider-options { display: grid; gap: 10px; }
.provider-option { display: flex; align-items: center; gap: 14px; width: 100%; padding: 14px; text-align: left; border: 1px solid var(--line); border-radius: 14px; background: var(--bg-panel); color: var(--text-dim); }
.provider-option:hover:not(:disabled) { background: var(--bg-hover); }
.provider-option:disabled { opacity: .6; }
.provider-option:disabled .provider-tile { background: var(--bg-active); color: var(--text-dim); }
.option-text { flex: 1; min-width: 0; }
.option-text strong { display: block; font-size: 14px; color: var(--text); }
.option-text small { display: block; font-size: 12px; color: var(--text-dim); margin-top: 4px; line-height: 1.45; }
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
.user-code { justify-self: start; font-family: var(--mono); font-size: 28px; letter-spacing: .14em; padding: 10px 18px; border: 1px dashed var(--accent); border-radius: 12px; background: var(--accent-dim); color: var(--text); }
.outcome .alert { margin: 0; }
.success strong { font-weight: 650; }
@media (max-width: 760px) { .waiting .primary { justify-self: stretch; } }
</style>
