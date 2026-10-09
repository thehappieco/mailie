<script setup lang="ts">
// A recovery code, shown once (docs/key-scheme.md section 5.6): right after
// an account is created, recovered, reset or upgraded, and after the person
// replaced theirs. It is the one way back in without the password, so the
// dialog stays until the person says they saved it; nothing keeps it, here
// or on the server, once it closes.
import { computed, ref } from 'vue'
import { recoveryCode, recoveryCodeSaved } from '../state/account'
import { announce } from '../ui/announce'
import { copyText } from '../ui/clipboard'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const saved = ref(false)
const copied = ref(false)
const copyFailed = ref(false)
const reminded = ref(false)

const intro = computed(() => recoveryCode.reason === 'new-account'
  ? t('If you forget your password, this code is the only way back into your account. It is shown only now: write it down, or keep it in a password manager, somewhere safe.')
  : t('Your old recovery code no longer works. This is the new one, shown only now: write it down, or keep it in a password manager, somewhere safe.'))

async function copy() {
  const ok = await copyText(recoveryCode.code)
  copyFailed.value = !ok
  copied.value = ok
  if (ok) announce(t('Recovery code copied.'))
}

function close() {
  if (!saved.value) { reminded.value = true; return }
  recoveryCodeSaved()
  saved.value = false
  copied.value = false
  reminded.value = false
}
</script>

<template>
  <ConsoleDialog v-if="recoveryCode.code" :title="t('Save your recovery code')" persistent @close="close">
    <div class="form-stack recovery-code">
      <p class="dim">{{ intro }}</p>
      <code class="code-secret notranslate" translate="no" spellcheck="false">{{ recoveryCode.code }}</code>
      <div class="dialog-actions start">
        <button class="ghost small" type="button" @click="copy"><AppIcon :name="copied ? 'check' : 'copy'" :size="16" />{{ copied ? t('Copied') : t('Copy the code') }}</button>
      </div>
      <p v-if="copyFailed" class="alert" role="alert">{{ t('Your browser did not let Mailie copy. Select the code and copy it yourself.') }}</p>
      <label class="check"><input v-model="saved" type="checkbox" name="recovery-code-saved" />{{ t('I saved my recovery code somewhere safe.') }}</label>
      <p v-if="reminded && !saved" class="alert" role="alert">{{ t('Confirm that you saved the code first: it cannot be shown again.') }}</p>
      <div class="dialog-actions"><button class="primary" type="button" :disabled="!saved" @click="close">{{ t('Continue') }}</button></div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.recovery-code .dim, .recovery-code .alert { margin: 0; }
.code-secret { display: block; padding: 16px; border: 1px dashed var(--console-border); border-radius: 10px; background: var(--bg-input); color: var(--text); font-family: var(--mono); font-size: 17px; letter-spacing: .5px; line-height: 1.5; text-align: center; overflow-wrap: anywhere; user-select: all; }
.dialog-actions.start { justify-content: flex-start; margin-top: 0; }
.dialog-actions button { display: inline-flex; align-items: center; gap: 6px; }
.check { display: flex !important; align-items: flex-start; gap: 10px !important; font-size: 14px !important; line-height: 1.45; }
.check input { width: auto; min-height: 0; margin: 3px 0 0; accent-color: var(--accent); flex: none; }
</style>
