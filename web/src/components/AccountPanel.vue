<script setup lang="ts">
// The signed-in person: name, password and sessions (and their role on this
// server, where the edition's people have one), and below them whatever
// the edition puts in its account section (the default slot): the permission
// rows, and anything else of its own. A password change ends every session on
// the server, this one included, and hands back the one that replaces it; the
// other browsers find out on their next request. A person who signs in only
// another way (an edition's own sign-in) has no password, and is not offered
// to change one. One success line serves the whole section: the rows say what
// they did through accountNotice.
//
// The password never leaves the browser (docs/key-scheme.md): changing it is
// two steps behind one form (state/account.ts), and replacing the recovery
// code wraps the account key this browser keeps under a new one, after a
// step-up when the session's last proof of the password is more than ten
// minutes old. A person who signed up before the key scheme and has not
// signed in since has no account key yet: they are asked to sign in again,
// which upgrades their account.
import { computed, provide, ref, shallowRef, watch } from 'vue'
import { MIN_PASSWORD } from '../api/auth'
import { ApiError } from '../api/http'
import { enrolled, hasPassword } from '../api/types'
import { edition } from '../edition'
import { changePassword, replaceRecoveryCode } from '../state/account'
import { failure, type Failure } from '../state/failure'
import { freshStepUp, session, signOut, updateProfile } from '../state/session'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { dayStamp, initials } from '../ui/format'
import { t } from '../ui/i18n'
import { roleLabel } from '../ui/labels'
import { accountNotice } from './accountNotice'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import PasswordInput from './PasswordInput.vue'
import StepUpDialog from './StepUpDialog.vue'

const name = ref('')
const profileBusy = ref(false)
const profileProblem = ref<Failure | null>(null)
/** The section's success line: a name saved, a password changed, a permission switched. */
const done = shallowRef<(() => string) | null>(null)
provide(accountNotice, { show(words) { done.value = words }, clear() { done.value = null } })
const label = computed(() => edition().copy.accountSection())
/** The person's role on this server, where the edition's people have one. */
const serverRole = edition().serverRole === true

watch(() => session.user?.name, value => { name.value = value ?? '' }, { immediate: true })
const profileChanged = computed(() => name.value.trim() !== (session.user?.name ?? '') && !!name.value.trim())

async function saveProfile() {
  if (profileBusy.value || !profileChanged.value) return
  profileBusy.value = true; profileProblem.value = null; done.value = null
  try {
    await updateProfile(name.value.trim())
    done.value = () => t('Name saved.')
    announce(t('Name saved.'))
  }
  catch (error) { profileProblem.value = failure('profile', error) }
  finally { profileBusy.value = false }
}

/** Whether there is a password to change: not for a person who signs in only another way, nor one not upgraded yet. */
const withPassword = computed(() => hasPassword(session.user) && enrolled(session.user))
/** A password the server still checks itself: the person signs in again to upgrade, which gives them an account key. */
const notUpgraded = computed(() => hasPassword(session.user) && !enrolled(session.user))
const passwordOpen = ref(false)
const current = ref('')
const next = ref('')
const confirm = ref('')
const passwordBusy = ref(false)
const passwordProblem = ref<Failure | null>(null)
const mismatch = ref(false)

function openPassword() {
  passwordOpen.value = true
  current.value = ''; next.value = ''; confirm.value = ''
  passwordProblem.value = null; mismatch.value = false; done.value = null
}
function closePassword() {
  if (passwordBusy.value) return
  passwordOpen.value = false
  current.value = ''; next.value = ''; confirm.value = ''
}
async function rotatePassword(event: SubmitEvent) {
  if (passwordBusy.value) return
  // Password managers can fill without an input event; read the named fields.
  const fields = new FormData(event.currentTarget as HTMLFormElement)
  current.value = String(fields.get('password') ?? '')
  next.value = String(fields.get('new-password') ?? '')
  confirm.value = String(fields.get('confirm-password') ?? '')
  passwordProblem.value = null
  mismatch.value = next.value !== confirm.value
  if (mismatch.value) return
  passwordBusy.value = true
  try {
    await changePassword(current.value, next.value)
    passwordBusy.value = false
    closePassword()
    done.value = () => t('Password changed. Every other session of your account has ended.')
    announce(t('Password changed. Every other session of your account has ended.'))
  } catch (error) {
    passwordProblem.value = failure('password', error)
  } finally {
    passwordBusy.value = false
  }
}

/** Replacing the recovery code: after a step-up when the session's is older than ten minutes. */
const recoveryBusy = ref(false)
const recoveryProblem = ref<Failure | null>(null)
const steppingUp = ref(false)

/** steppedUp: the person just proved their password, so a refusal now is no stale step-up and is said as it is. */
async function replaceCode(steppedUp = false) {
  if (recoveryBusy.value) return
  recoveryProblem.value = null; done.value = null
  // Without the key here nothing would be asked for: say so at once (no_account_key).
  if (session.keyed && !freshStepUp()) { steppingUp.value = true; return }
  recoveryBusy.value = true
  try {
    await replaceRecoveryCode()
    done.value = () => t('Recovery code replaced. The old one no longer works.')
    announce(t('Recovery code replaced. The old one no longer works.'))
  } catch (error) {
    // The server's clock found the step-up older than this browser's did: ask again.
    if (error instanceof ApiError && error.code === 'not_authorized' && !steppedUp) { session.authenticatedAt = 0; steppingUp.value = true }
    else recoveryProblem.value = failure('recovery-code', error)
  } finally {
    recoveryBusy.value = false
  }
}
function afterStepUp() {
  steppingUp.value = false
  void replaceCode(true)
}

const everywhere = ref(false)
const leaving = ref(false)
const leaveProblem = ref<Failure | null>(null)
async function leave(all: boolean) {
  if (leaving.value) return
  leaving.value = true; leaveProblem.value = null
  try { await signOut({ everywhere: all }) }
  catch (error) { leaveProblem.value = failure('sign-out', error) }
  finally { leaving.value = false; everywhere.value = false }
}
</script>

<template>
  <section class="account-settings" :aria-label="label">
    <form class="account-profile" @submit.prevent="saveProfile">
      <span class="account-avatar" aria-hidden="true">{{ initials(session.user?.name || session.user?.email || '') }}</span>
      <div class="profile-fields">
        <label>{{ t('Your name') }}<input v-model="name" name="name" maxlength="120" autocomplete="name" required :disabled="profileBusy" /></label>
        <div class="profile-facts">
          <div><span>{{ t('Email') }}</span><strong>{{ session.user?.email }}</strong></div>
          <div v-if="serverRole && session.user"><span>{{ t('Role on this server') }}</span><strong>{{ roleLabel(session.user.role) }}</strong></div>
        </div>
        <button class="primary small" type="submit" :disabled="profileBusy || !profileChanged">{{ profileBusy ? t('Saving…') : t('Save name') }}</button>
      </div>
    </form>
    <p v-if="profileProblem" class="alert" role="alert">{{ describe(profileProblem) }}</p>
    <!-- Said by the live region (ui/announce.ts); a status created with its text is often not read. -->
    <p v-if="done" class="success"><AppIcon name="check" :size="18" />{{ done() }}</p>

    <div class="security-options">
      <button v-if="withPassword" class="security-summary" type="button" aria-haspopup="dialog" @click="openPassword">
        <span class="summary-icon"><AppIcon name="lock" :size="22" /></span>
        <span class="summary-text"><strong>{{ t('Password') }}</strong><small>{{ t('Change the password you sign in with') }}</small></span>
        <AppIcon name="chevron-right" :size="18" />
      </button>
      <button v-if="withPassword" class="security-summary" type="button" :disabled="recoveryBusy" @click="replaceCode()">
        <span class="summary-icon"><AppIcon name="key" :size="22" /></span>
        <span class="summary-text"><strong>{{ t('Recovery code') }}</strong><small>{{ recoveryBusy ? t('Making a new recovery code…') : t('Replace the code that lets you back in if you forget your password') }}</small></span>
        <AppIcon name="chevron-right" :size="18" />
      </button>
      <p v-if="recoveryProblem" class="alert" role="alert">{{ describe(recoveryProblem) }}</p>
      <div v-if="notUpgraded" class="security-summary upgrade">
        <span class="summary-icon"><AppIcon name="lock" :size="22" /></span>
        <span class="summary-text"><strong>{{ t('Password') }}</strong><small>{{ t('Sign in again to finish setting up your account: your password then stays in your browser, and you get a recovery code. Until then you cannot change either.') }}</small></span>
        <div class="session-actions"><button class="ghost small" type="button" :disabled="leaving" @click="leave(false)"><AppIcon name="logout" :size="16" />{{ t('Sign in again') }}</button></div>
      </div>
      <div class="security-summary sessions">
        <span class="summary-icon"><AppIcon name="shield" :size="22" /></span>
        <span class="summary-text">
          <strong>{{ t('Sessions') }}</strong>
          <small>{{ t('This session lasts until {date}. Signing out everywhere ends the sessions of your account on every browser.', { date: dayStamp(session.expiresAt) }) }}</small>
        </span>
        <div class="session-actions">
          <template v-if="everywhere">
            <button class="danger small" type="button" :disabled="leaving" @click="leave(true)">{{ leaving ? t('Signing out…') : t('Sign out everywhere now') }}</button>
            <button class="ghost small" type="button" :disabled="leaving" @click="everywhere = false">{{ t('Cancel') }}</button>
          </template>
          <template v-else>
            <button class="ghost small" type="button" :disabled="leaving" @click="leave(false)"><AppIcon name="logout" :size="16" />{{ t('Sign out') }}</button>
            <button class="ghost small" type="button" :disabled="leaving" @click="everywhere = true">{{ t('Sign out everywhere') }}</button>
          </template>
        </div>
      </div>
      <p v-if="leaveProblem" class="alert" role="alert">{{ describe(leaveProblem) }}</p>
      <slot />
    </div>

    <StepUpDialog v-if="steppingUp" @done="afterStepUp" @close="steppingUp = false" />
    <ConsoleDialog v-if="passwordOpen && withPassword" :title="t('Change password')" :busy="passwordBusy" @close="closePassword">
      <p v-if="passwordProblem || mismatch" class="alert" role="alert">{{ mismatch ? t('The new passwords do not match.') : passwordProblem ? describe(passwordProblem) : '' }}</p>
      <form class="form-stack" name="mailie-password-change" method="post" autocomplete="on" @submit.prevent="rotatePassword">
        <p class="dim">{{ t('Use at least {count} characters. Changing the password signs you out of every other browser.', { count: MIN_PASSWORD }) }}</p>
        <!-- For password managers: the account the new password belongs to. -->
        <input name="username" :value="session.user?.email" type="email" autocomplete="username" class="account-identifier" readonly tabindex="-1" aria-hidden="true" />
        <!-- The label ends before the field: wrapped around it, it would take the
             show/hide button's name into the field's own. -->
        <div class="password-field"><label for="current-password">{{ t('Current password') }}</label><PasswordInput id="current-password" v-model="current" name="password" required autocomplete="current-password" :disabled="passwordBusy" /></div>
        <div class="password-field"><label for="new-password">{{ t('New password') }}</label><PasswordInput id="new-password" v-model="next" name="new-password" required :minlength="MIN_PASSWORD" autocomplete="new-password" :disabled="passwordBusy" /></div>
        <div class="password-field"><label for="confirm-password">{{ t('Repeat the new password') }}</label><PasswordInput id="confirm-password" v-model="confirm" name="confirm-password" required :minlength="MIN_PASSWORD" autocomplete="new-password" :disabled="passwordBusy" /></div>
        <div class="dialog-actions"><button class="ghost" type="button" :disabled="passwordBusy" @click="closePassword">{{ t('Cancel') }}</button><button class="primary" type="submit" :disabled="passwordBusy">{{ passwordBusy ? t('Changing…') : t('Change password') }}</button></div>
      </form>
    </ConsoleDialog>
  </section>
</template>

<style scoped>
.account-settings { display: grid; gap: 14px; max-width: 850px; }
.account-profile { display: flex; align-items: flex-start; gap: 26px; padding: 24px; border: 1px solid var(--console-border); border-radius: 16px; background: var(--bg-raised); }
.account-avatar { flex: none; width: 92px; height: 92px; border-radius: 50%; display: grid; place-items: center; background: var(--accent-dim); color: var(--accent); font-size: 30px; font-weight: 600; }
.profile-fields { display: grid; gap: 18px; flex: 1; min-width: 0; }
.profile-fields label { display: grid; gap: 8px; font-size: 12px; color: var(--text-dim); }
.profile-fields input { min-height: 42px; border: 1px solid var(--console-border); border-radius: 8px; font-size: 14px; color: var(--text); }
.profile-facts { display: flex; flex-wrap: wrap; gap: 18px 36px; }
.profile-facts div { display: grid; gap: 7px; min-width: 0; }
.profile-facts span { font-size: 12px; color: var(--text-dim); }
.profile-facts strong { font-size: 14px; font-weight: 500; overflow-wrap: anywhere; }
.profile-fields > button { justify-self: start; }
.security-options { display: grid; gap: 12px; }
.security-summary { display: flex; align-items: center; gap: 14px; width: 100%; padding: 18px; text-align: left; border: 1px solid var(--console-border); border-radius: 14px; background: var(--bg-panel); color: var(--text-dim); }
button.security-summary:hover { background: var(--bg-hover); }
.summary-icon { display: grid; place-items: center; flex: none; width: 42px; height: 42px; border-radius: 12px; color: var(--accent); background: var(--accent-dim); }
.summary-text { flex: 1; min-width: 0; }
.summary-text strong { display: block; font-size: 14px; color: var(--text); }
.summary-text small { display: block; font-size: 12px; color: var(--text-dim); margin-top: 5px; line-height: 1.5; }
.sessions, .upgrade { flex-wrap: wrap; }
.session-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.session-actions button { display: inline-flex; align-items: center; gap: 6px; }
.success, .alert { margin: 0; }
.password-field { display: grid; gap: 8px; }
.account-identifier { position: absolute !important; width: 1px !important; height: 1px !important; min-height: 0 !important; opacity: 0; pointer-events: none; padding: 0 !important; border: 0 !important; }
@media (max-width: 760px) {
  .sessions .session-actions { width: 100%; padding-left: 56px; }
}
@media (max-width: 600px) {
  .account-profile { flex-direction: column; align-items: stretch; padding: 18px; gap: 20px; }
  .profile-fields input { font-size: 16px; }
  .account-avatar { align-self: center; width: 76px; height: 76px; font-size: 25px; }
  .profile-fields > button { width: 100%; }
  .security-summary { padding: 15px; }
  .sessions .session-actions { padding-left: 0; }
  .session-actions button { flex: 1; }
}
</style>
