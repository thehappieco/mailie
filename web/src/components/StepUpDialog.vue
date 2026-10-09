<script setup lang="ts">
// The step-up (docs/key-scheme.md section 11): what gives access to a
// mailbox and writes its keys, in the next step of phase 3, needs the
// person's password proved within the last ten minutes, on this session;
// nothing asks for it yet. Replacing the recovery code and changing the
// password prove the password in their own request instead. The password is
// derived here as at sign-in and never sent; only this session's step-up
// time moves.
import { ref } from 'vue'
import { stepUp } from '../state/account'
import { failure, type Failure } from '../state/failure'
import { session } from '../state/session'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import ConsoleDialog from './ConsoleDialog.vue'
import PasswordInput from './PasswordInput.vue'

const emit = defineEmits<{ done: []; close: [] }>()
const password = ref('')
const busy = ref(false)
const problem = ref<Failure | null>(null)

async function submit(event: SubmitEvent) {
  if (busy.value) return
  password.value = String(new FormData(event.currentTarget as HTMLFormElement).get('password') ?? '')
  busy.value = true
  problem.value = null
  try {
    await stepUp(password.value)
    busy.value = false
    emit('done')
  } catch (error) {
    problem.value = failure('step-up', error)
  } finally {
    busy.value = false
    password.value = ''
  }
}
</script>

<template>
  <ConsoleDialog :title="t('Enter your password again')" :busy="busy" @close="emit('close')">
    <form class="form-stack" name="mailie-step-up" method="post" autocomplete="on" @submit.prevent="submit">
      <p class="dim">{{ t('For your safety, this needs your password from the last ten minutes. It is processed here and never sent.') }}</p>
      <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
      <!-- For password managers: the account the password belongs to. -->
      <input name="username" :value="session.user?.email" type="email" autocomplete="username" class="account-identifier" readonly tabindex="-1" aria-hidden="true" />
      <div class="password-field"><label for="step-up-password">{{ t('Password') }}</label><PasswordInput id="step-up-password" v-model="password" name="password" required autocomplete="current-password" :disabled="busy" /></div>
      <div class="dialog-actions"><button class="ghost" type="button" :disabled="busy" @click="emit('close')">{{ t('Cancel') }}</button><button class="primary" type="submit" :disabled="busy">{{ busy ? t('Checking…') : t('Continue') }}</button></div>
    </form>
  </ConsoleDialog>
</template>

<style scoped>
.password-field { display: grid; gap: 8px; }
.account-identifier { position: absolute !important; width: 1px !important; height: 1px !important; min-height: 0 !important; opacity: 0; pointer-events: none; padding: 0 !important; border: 0 !important; }
.dim, .alert { margin: 0; }
</style>
