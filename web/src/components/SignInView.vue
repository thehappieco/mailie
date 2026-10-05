<script setup lang="ts">
import { computed, ref } from 'vue'
import { MIN_PASSWORD } from '../api/auth'
import { failure, type Failure } from '../state/failure'
import { dropInvitation } from '../state/invitation'
import { pendingOAuthReturn } from '../state/oauthReturn'
import { session, signIn, signUp } from '../state/session'
import { edition } from '../edition'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import type { Invitation } from '../ui/signupLink'
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
const props = defineProps<{ invitation: Invitation | null }>()
const legal = edition().legal
const teams = edition().teams === true
const editionSignIn = edition().signIn

type Mode = 'sign-in' | 'sign-up'
const mode = ref<Mode>(props.invitation ? 'sign-up' : 'sign-in')
const busy = ref(false)
const problem = ref<Failure | null>(null)
const mismatch = ref(false)
const email = ref(props.invitation?.email ?? '')
const name = ref('')
const password = ref('')
const confirm = ref('')
const returning = pendingOAuthReturn()

const title = computed(() => mode.value === 'sign-up' ? t('Create your account') : t('Sign in'))
const problemText = computed(() => mismatch.value ? t('The passwords do not match.') : problem.value ? describe(problem.value) : '')

function switchTo(next: Mode) {
  if (busy.value) return
  mode.value = next
  problem.value = null
  mismatch.value = false
  password.value = ''
  confirm.value = ''
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
  problem.value = null
  mismatch.value = false
  if (mode.value === 'sign-up' && password.value !== confirm.value) { mismatch.value = true; return }
  busy.value = true
  try {
    if (mode.value === 'sign-up' && props.invitation) {
      await signUp({ invite: props.invitation.invite, email: email.value, name: name.value, password: password.value })
      // Spent: the account it made is the one signed in now.
      dropInvitation()
    } else {
      await signIn(email.value, password.value)
    }
  } catch (error) {
    problem.value = failure(mode.value, error)
  } finally {
    busy.value = false
    password.value = ''
    confirm.value = ''
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
        <p v-else class="sub">{{ t('Your email accounts, connected in one place. Sign in to continue.') }}</p>

        <p v-if="session.notice === 'expired' && mode === 'sign-in'" class="note" role="status">{{ t('Your session ended. Sign in again.') }}</p>
        <p v-if="returning" class="note" role="status">{{ t('Sign in to finish connecting your email account.') }}</p>
        <p v-if="teams && invitation && mode === 'sign-in'" class="note">{{ t('Once you are signed in, you can accept the invitation to join a team.') }}</p>
        <div v-if="problemText" class="alert" role="alert">{{ problemText }}</div>

        <form :name="mode === 'sign-in' ? 'mailie-login' : 'mailie-signup'" method="post" autocomplete="on" @submit.prevent="submit">
          <div v-if="mode === 'sign-up'" class="field">
            <label for="name">{{ t('Your name') }}</label>
            <input id="name" v-model="name" name="name" required maxlength="120" autocomplete="name" :disabled="busy" />
          </div>
          <div class="field">
            <label for="email">{{ t('Email') }}</label>
            <input id="email" v-model="email" name="username" type="email" autocomplete="username" required autocapitalize="off"
              spellcheck="false" inputmode="email" :readonly="mode === 'sign-up' && !!invitation?.email" :disabled="busy" />
            <p v-if="mode === 'sign-up' && invitation?.email" class="hint">{{ t('The invitation only works for this address.') }}</p>
          </div>
          <div class="field">
            <label for="password">{{ t('Password') }}</label>
            <PasswordInput id="password" v-model="password" name="password" required :disabled="busy"
              :minlength="mode === 'sign-in' ? undefined : MIN_PASSWORD"
              :autocomplete="mode === 'sign-in' ? 'current-password' : 'new-password'" />
            <p v-if="mode === 'sign-up'" class="hint">{{ t('Use at least {count} characters.', { count: MIN_PASSWORD }) }}</p>
          </div>
          <div v-if="mode === 'sign-up'" class="field">
            <label for="confirm">{{ t('Repeat the password') }}</label>
            <PasswordInput id="confirm" v-model="confirm" name="confirm-password" required :minlength="MIN_PASSWORD" autocomplete="new-password" :disabled="busy" />
          </div>
          <button class="primary" type="submit" :disabled="busy">
            <span v-if="busy" class="loading-spinner inline" aria-hidden="true" />
            {{ busy ? (mode === 'sign-up' ? t('Creating your account…') : t('Signing in…')) : title }}
          </button>
          <p v-if="mode === 'sign-up' && legal?.signUp" class="consent"><component :is="legal.signUp" /></p>
        </form>

        <div class="auth-footer">
          <button v-if="mode === 'sign-up'" class="linkish" type="button" @click="switchTo('sign-in')">{{ t('I already have an account') }}</button>
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
.auth-footer { margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line); }
.auth-footnote { display: flex; align-items: flex-start; gap: 8px; margin: 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; }
.auth-footnote .app-icon { margin-top: 1px; }
.consent { margin: 12px 0 0; color: var(--text-dim); font-size: 12px; line-height: 1.5; text-align: center; }
.consent :deep(a) { text-underline-offset: 2px; }
.auth-legal { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 14px 0 0; color: var(--text-faint); font-size: 12px; }
.auth-legal :deep(a) { color: var(--text-dim); text-decoration: none; }
.auth-legal :deep(a:hover) { color: var(--text); text-decoration: underline; }
input:-webkit-autofill { -webkit-text-fill-color: var(--text); box-shadow: 0 0 0 1000px var(--bg-input) inset; caret-color: var(--text); }
@media (max-width: 600px) { .unlock-card { padding: 26px 22px; } input { font-size: 16px; } }
</style>
