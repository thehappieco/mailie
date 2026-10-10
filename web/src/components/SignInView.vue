<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { MIN_PASSWORD } from '../api/auth'
import { failure, type Failure, type Operation } from '../state/failure'
import { dropInvitation } from '../state/invitation'
import { pendingOAuthReturn } from '../state/oauthReturn'
import { dropReset } from '../state/resetLink'
import { session } from '../state/session'
import { openResetLink, recover, resetPassword, signIn, signUp } from '../state/account'
import { edition } from '../edition'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import type { Opening } from '../api/types'
import type { Invitation, ResetLink } from '../ui/signupLink'
import AppearanceMenu from './AppearanceMenu.vue'
import AppIcon from './AppIcon.vue'
import BrandLockup from './BrandLockup.vue'
import PasswordInput from './PasswordInput.vue'

// There is no public sign-up: an account starts from an invitation link, which
// carries the code and the address it is for. Without one, this is a sign-in
// form and says where invitations come from. Where teams are made here, an
// invitation may also be one to join a team, which someone who already has an
// account accepts once signed in. What creating an account agrees to, and the
// links at the card's foot, are the edition's (legal), if any. An edition
// whose people sign in another way puts its own sign-in in place of the
// password card (signIn), inside the same frame; no invitation signs anyone
// up there.
//
// The password never leaves this page (docs/key-scheme.md): every form here
// derives from it in the browser (state/account.ts), which takes a few
// seconds by design, and says so while it works, the sign-in form too: since
// the one-time upgrade of an account made before the key scheme left (it sent
// the old password once, in the release that brought the scheme only), no
// form sends a password, whatever the server answers. Besides signing in and
// up: recovering an account with its recovery code, and a reset link from
// the operator (#reset=…), which is also the way back for an account made
// before the key scheme that never enrolled.
const props = defineProps<{ invitation: Invitation | null; reset?: ResetLink | null }>()
const legal = edition().legal
const teams = edition().teams === true
const editionSignIn = edition().signIn

type Mode = 'sign-in' | 'sign-up' | 'recover' | 'reset'
const mode = ref<Mode>(props.reset ? 'reset' : props.invitation ? 'sign-up' : 'sign-in')
const busy = ref(false)
const problem = ref<Failure | null>(null)
const mismatch = ref(false)
const email = ref(props.reset?.email ?? props.invitation?.email ?? '')
const name = ref('')
const password = ref('')
const confirm = ref('')
const code = ref('')
/** A recovery that went through, whose sign-in after it did not: the new password works. */
const recovered = ref(false)
const returning = pendingOAuthReturn()
/** What the reset link answered when the page opened it: the target and the seal id the new password is bound to. */
let resetOpening: Opening | undefined

/** Modes that choose a new password, with a second field to repeat it. */
const choosing = computed(() => mode.value !== 'sign-in')
const title = computed(() => {
  switch (mode.value) {
    case 'sign-up': return t('Create your account')
    case 'recover': return t('Recover your account')
    case 'reset': return t('Choose a new password')
    default: return t('Sign in')
  }
})
const action = computed(() => {
  switch (mode.value) {
    case 'sign-up': return busy.value ? t('Creating your account…') : t('Create your account')
    case 'recover': return busy.value ? t('Recovering your account…') : t('Recover and sign in')
    case 'reset': return busy.value ? t('Saving…') : t('Save and sign in')
    default: return busy.value ? t('Signing in…') : t('Sign in')
  }
})
const operation = computed<Operation>(() => mode.value === 'sign-up' ? 'sign-up' : mode.value === 'recover' ? 'recover' : mode.value === 'reset' ? 'reset' : 'sign-in')
const problemText = computed(() => mismatch.value ? t('The passwords do not match.') : problem.value ? describe(problem.value) : '')

// A reset link is checked before anyone types a password: one that is not
// valid, or that would take the last reader of a team mailbox, says so first.
onMounted(async () => {
  if (mode.value !== 'reset' || !props.reset) return
  try { resetOpening = await openResetLink(props.reset.reset, props.reset.email) }
  catch (error) { if (mode.value === 'reset') problem.value = failure('reset', error) }
})

function switchTo(next: Mode) {
  if (busy.value) return
  if (mode.value === 'reset') dropReset()
  mode.value = next
  problem.value = null
  mismatch.value = false
  recovered.value = false
  password.value = ''
  confirm.value = ''
  code.value = ''
  email.value = next === 'sign-up' ? props.invitation?.email ?? '' : email.value
}

async function submit(event: SubmitEvent) {
  if (busy.value) return
  // Safari and Chrome can autofill without a Vue input event. Read the named
  // native fields at submit time, and let native validation handle emptiness.
  const values = new FormData(event.currentTarget as HTMLFormElement)
  if (values.has('username')) email.value = String(values.get('username') ?? '').trim()
  if (values.has('password')) password.value = String(values.get('password') ?? '')
  if (values.has('confirm-password')) confirm.value = String(values.get('confirm-password') ?? '')
  if (values.has('name')) name.value = String(values.get('name') ?? '').trim()
  if (values.has('recovery-code')) code.value = String(values.get('recovery-code') ?? '')
  problem.value = null
  mismatch.value = false
  recovered.value = false
  if (choosing.value && password.value !== confirm.value) { mismatch.value = true; return }
  busy.value = true
  const op = operation.value
  try {
    if (mode.value === 'sign-up' && props.invitation) {
      await signUp({ invite: props.invitation.invite, email: email.value, name: name.value, password: password.value })
      // Spent: the account it made is the one signed in now.
      dropInvitation()
    } else if (mode.value === 'reset' && props.reset) {
      await resetPassword({ reset: props.reset.reset, email: props.reset.email, password: password.value }, resetOpening)
      dropReset()
    } else if (mode.value === 'recover') {
      if (await recover({ email: email.value, code: code.value, password: password.value }) === 'sign-in-again') {
        mode.value = 'sign-in'
        recovered.value = true
      }
    } else {
      await signIn(email.value, password.value)
    }
  } catch (error) {
    problem.value = failure(op, error)
  } finally {
    busy.value = false
    password.value = ''
    confirm.value = ''
    code.value = ''
  }
}
</script>

<template>
  <div class="unlock">
    <div class="unlock-card">
      <div class="auth-top">
        <BrandLockup :size="44" animate="load" label="Mailie" />
        <AppearanceMenu />
      </div>
      <template v-if="editionSignIn">
        <p v-if="session.notice === 'expired'" class="note" role="status">{{ t('Your session ended. Sign in again.') }}</p>
        <p v-if="returning" class="note" role="status">{{ t('Sign in to finish connecting your email account.') }}</p>
        <component :is="editionSignIn" />
        <div v-if="legal?.signInFooter" class="auth-footer">
          <p class="auth-legal"><component :is="legal.signInFooter" /></p>
        </div>
      </template>
      <template v-else>
        <h1>{{ title }}</h1>
        <p v-if="mode === 'sign-up'" class="sub">{{ t('This invitation is for the address below. Choose your name and a password to finish.') }}</p>
        <p v-else-if="mode === 'recover'" class="sub">{{ t('Enter the recovery code you saved when you created your account, and choose a new password. You get a new recovery code too.') }}</p>
        <p v-else-if="mode === 'reset'" class="sub">{{ t('This link from the administrator of this server gives the address below a new password and a new recovery code. The old ones stop working.') }}</p>
        <p v-else class="sub">{{ t('Your email accounts, connected in one place. Sign in to continue.') }}</p>

        <p v-if="session.notice === 'expired' && mode === 'sign-in' && !recovered" class="note" role="status">{{ t('Your session ended. Sign in again.') }}</p>
        <p v-if="recovered && mode === 'sign-in'" class="note" role="status">{{ t('Your account is recovered. Sign in with your new password.') }}</p>
        <p v-if="returning" class="note" role="status">{{ t('Sign in to finish connecting your email account.') }}</p>
        <p v-if="teams && invitation && mode === 'sign-in'" class="note">{{ t('Once you are signed in, you can accept the invitation to join a team.') }}</p>
        <div v-if="problemText" class="alert" role="alert">{{ problemText }}</div>

        <form :name="mode === 'sign-in' ? 'mailie-login' : mode === 'sign-up' ? 'mailie-signup' : `mailie-${mode}`" method="post" autocomplete="on" @submit.prevent="submit">
          <div v-if="mode === 'sign-up'" class="field">
            <label for="name">{{ t('Your name') }}</label>
            <input id="name" v-model="name" name="name" required maxlength="120" autocomplete="name" :disabled="busy" />
          </div>
          <div class="field">
            <label for="email">{{ t('Email') }}</label>
            <input id="email" v-model="email" name="username" type="email" autocomplete="username" required autocapitalize="off"
              spellcheck="false" inputmode="email" :readonly="(mode === 'sign-up' && !!invitation?.email) || mode === 'reset'" :disabled="busy" />
            <p v-if="mode === 'sign-up' && invitation?.email" class="hint">{{ t('The invitation only works for this address.') }}</p>
          </div>
          <div v-if="mode === 'recover'" class="field">
            <label for="recovery-code">{{ t('Recovery code') }}</label>
            <input id="recovery-code" v-model="code" name="recovery-code" required autocomplete="off" autocapitalize="characters"
              spellcheck="false" maxlength="80" :disabled="busy" />
          </div>
          <div class="field">
            <label for="password">{{ choosing ? t('New password') : t('Password') }}</label>
            <PasswordInput id="password" v-model="password" name="password" required :disabled="busy"
              :minlength="choosing ? MIN_PASSWORD : undefined"
              :autocomplete="choosing ? 'new-password' : 'current-password'" />
            <p v-if="choosing" class="hint">{{ t('Use at least {count} characters.', { count: MIN_PASSWORD }) }}</p>
          </div>
          <div v-if="choosing" class="field">
            <label for="confirm">{{ t('Repeat the password') }}</label>
            <PasswordInput id="confirm" v-model="confirm" name="confirm-password" required :minlength="MIN_PASSWORD" autocomplete="new-password" :disabled="busy" />
          </div>
          <button class="primary" type="submit" :disabled="busy || (mode === 'reset' && problem?.op === 'reset' && problem.code === 'not_authorized')">
            <span v-if="busy" class="loading-spinner inline" aria-hidden="true" />
            {{ action }}
          </button>
          <p v-if="busy" class="hint working" role="status">{{ t('Your password is processed here, in this browser, and never sent. This takes a few seconds.') }}</p>
          <p v-if="mode === 'sign-up' && legal?.signUp" class="consent"><component :is="legal.signUp" /></p>
        </form>

        <div class="auth-footer">
          <button v-if="mode === 'sign-in'" class="linkish" type="button" @click="switchTo('recover')">{{ t('Forgot your password?') }}</button>
          <button v-if="mode === 'sign-up' || mode === 'recover' || mode === 'reset'" class="linkish" type="button" @click="switchTo('sign-in')">{{ mode === 'sign-up' ? t('I already have an account') : t('Back to sign in') }}</button>
          <button v-else-if="invitation" class="linkish" type="button" @click="switchTo('sign-up')">{{ t('Use my invitation') }}</button>
          <p v-else class="auth-footnote"><AppIcon name="info" :size="16" />{{ t('Accounts are created by invitation. Ask the administrator of this server for a link.') }}</p>
          <p v-if="legal?.signInFooter" class="auth-legal"><component :is="legal.signInFooter" /></p>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.auth-top { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; margin-bottom: 28px; }
.auth-top :deep(.appearance-trigger) { margin: -6px -8px 0 0; }
.unlock-card { border-radius: 22px; box-shadow: 0 18px 70px #0000000d; }
form .primary { min-height: 46px; border-radius: 12px; margin-top: 6px; }
.working { margin: 10px 0 0; text-align: center; }
.auth-footer { display: grid; justify-items: start; gap: 10px; margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line); }
.auth-footnote { display: flex; align-items: flex-start; gap: 8px; margin: 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; }
.auth-footnote .app-icon { margin-top: 1px; }
.consent { margin: 12px 0 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; text-align: center; }
.consent :deep(a) { text-underline-offset: 2px; }
.auth-legal { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 4px 0 0; color: var(--text-faint); font-size: 12px; }
.auth-legal :deep(a) { color: var(--text-dim); text-decoration: none; }
.auth-legal :deep(a:hover) { color: var(--text); text-decoration: underline; }
input:-webkit-autofill { -webkit-text-fill-color: var(--text); box-shadow: 0 0 0 1000px var(--bg-input) inset; caret-color: var(--text); }
@media (max-width: 600px) { .unlock-card { padding: 26px 22px; } input { font-size: 16px; } }
</style>
