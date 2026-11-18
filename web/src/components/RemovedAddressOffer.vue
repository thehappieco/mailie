<script setup lang="ts">
// An address a link was refused for, which the workspace shown holds removed
// and kept (docs/workspaces.md, "Removed mailboxes"): to an owner of the
// workspace, recovering it, with what it kept, or deleting it for good (its
// address typed again) and then connecting it anew. Its removed mailboxes are
// read when it is drawn, so the offer is made only for one that is there;
// for anyone else, and for an address that is not one of them, nothing is
// drawn (shown answers whether anything is), and the refusal's own words say
// that an owner can. The form that was refused (ConnectForm) draws it, and an
// edition's page for a link at the provider's failure does too.
import { computed, onMounted, ref, useId, watch } from 'vue'
import type { RemovedMailbox } from '../api/types'
import { loadRemoved, purgeRemoved, recoverRemoved, removed, removedByEmail } from '../state/accounts'
import { currentWorkspace } from '../state/workspaces'
import { owns } from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ email: string }>()
const emit = defineEmits<{ close: []; recovered: []; again: []; shown: [boolean] }>()

const match = ref<RemovedMailbox | null>(null)
const asked = ref(false)
const typed = ref('')
const typedID = useId()
const purged = ref(false)
const busy = computed(() => removed.busy !== '')
const armed = computed(() => !!match.value && typed.value.trim().toLowerCase() === match.value.email.toLowerCase())
const problem = computed(() => match.value && removed.problem?.id === match.value.id ? removed.problem.failure : null)

async function find() {
  match.value = null
  asked.value = false
  typed.value = ''
  purged.value = false
  const email = props.email
  if (!email || !owns(currentWorkspace())) { emit('shown', false); return }
  await loadRemoved()
  if (props.email !== email) return
  match.value = removedByEmail(email) ?? null
  emit('shown', !!match.value)
}
onMounted(find)
watch(() => props.email, find)

async function recover() {
  const found = match.value
  if (!found || busy.value) return
  if (await recoverRemoved(found.id)) emit('recovered')
}
async function purge() {
  const found = match.value
  if (!found || !armed.value || busy.value) return
  if (await purgeRemoved(found.id)) {
    asked.value = false
    purged.value = true
    announce(t('{email} was deleted for good. You can connect it again now.', { email: found.email }))
  }
}
</script>

<template>
  <div v-if="match" class="removed-address">
    <template v-if="purged">
      <div class="success"><AppIcon name="check" :size="18" /><span>{{ t('{email} was deleted for good. You can connect it again now.', { email: match.email }) }}</span></div>
      <div class="dialog-actions">
        <button class="ghost" type="button" @click="emit('close')">{{ t('Close') }}</button>
        <button class="primary" type="button" @click="emit('again')">{{ t('Connect it again') }}</button>
      </div>
    </template>
    <form v-else-if="asked" class="form-stack removed-offer" autocomplete="off" @submit.prevent="purge">
      <p class="dim">{{ t('Mailie deletes the mailbox and everything it kept for it: the saved sign-in, its index and its key. It cannot be recovered afterwards. Nothing is deleted at the provider, and the address can then be connected again.') }}</p>
      <label :for="typedID">{{ t('Type {address} to confirm', { address: match.email }) }}</label>
      <input :id="typedID" v-model="typed" name="purge-confirm" autocomplete="off" :spellcheck="false" autocapitalize="off" inputmode="email" :placeholder="match.email" :disabled="busy" />
      <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="asked = false">{{ t('Back') }}</button>
        <button class="danger" type="submit" :disabled="busy || !armed">{{ busy ? t('Deleting…') : t('Delete for good') }}</button>
      </div>
    </form>
    <template v-else>
      <p class="note removed-offer-text">{{ t('{email} was removed from this workspace on {removed}, and is kept until {kept}. Recover it to use it again, with what it kept, or delete it for good to connect it anew.', { email: match.email, removed: dayStamp(match.removed_at), kept: dayStamp(match.purge_after) }) }}</p>
      <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="emit('close')">{{ t('Close') }}</button>
        <button class="ghost" type="button" :disabled="busy" @click="asked = true">{{ t('Delete for good…') }}</button>
        <button class="primary" type="button" :disabled="busy" @click="recover">{{ busy ? t('Recovering…') : t('Recover') }}</button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.removed-address { display: grid; gap: 16px; }
.removed-offer-text { margin: 0; line-height: 1.55; }
.form-stack { display: grid; gap: 16px; }
.form-stack label { display: grid; gap: 8px; font-size: 13px; }
.removed-offer p { margin: 0; }
.removed-offer input { color: var(--text); background: var(--bg-input); border: 1px solid var(--line); border-radius: 10px; padding: 11px 12px; width: 100%; }
.dialog-actions { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; }
@media (max-width: 600px) { .removed-offer input { font-size: 16px; } }
</style>
