<script setup lang="ts">
// Connecting a mailbox by its address, or finishing an existing one's
// authorization, as a dialog around the form (ConnectForm): opened from a
// provider's button (ConnectButtons) straight on that provider's form, with
// no step to choose it, and from a card's "Finish authorization" (resume).
// The mailbox goes into the workspace shown; the dialog's title names the
// provider, its subtitle what happens next. Closing an unfinished attempt
// removes the account it created; Done, or Skip, on the new mailbox closes
// it.
import { computed, ref } from 'vue'
import type { Account, ProviderID } from '../api/types'
import { cancelConnect, connect, resetConnect } from '../state/accounts'
import { t } from '../ui/i18n'
import { providerChoice, providerName } from '../ui/labels'
import ConnectForm from './ConnectForm.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const props = defineProps<{ provider?: ProviderID; byAddress?: boolean; resume?: Account | null; returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ close: [] }>()

/** The form's provider: the one pressed, until the IMAP form points an Apple address to iCloud. */
const shown = ref<ProviderID>(props.resume?.provider ?? props.provider ?? 'imap')
const blocked = computed(() => connect.phase === 'starting' || connect.phase === 'redirecting')

const title = computed(() => {
  if (props.resume) return t('Finish authorization')
  if (connect.phase === 'done') return t('Account connected')
  return providerChoice(shown.value)
})

const subtitle = computed(() => {
  if (props.resume) return props.resume.email
  if (connect.phase === 'done') return ''
  if (shown.value === 'imap') return t('Mailie signs in to the IMAP server with these details before it saves anything. The SMTP server is not tested.')
  if (shown.value === 'icloud') return t('Mailie signs in to iCloud with this address and password before it saves anything.')
  return t('You will sign in at {provider} to let Mailie read and send mail for this address.', { provider: providerName(shown.value) })
})

/** The dialog's own close: an unfinished attempt goes with the account it created, as the form's Cancel does. */
function close() {
  if (blocked.value) return
  if (connect.phase === 'done') resetConnect()
  else cancelConnect()
  emit('close')
}
</script>

<template>
  <ConsoleDialog :title="title" :subtitle="subtitle" :busy="blocked" :return-focus="returnFocus" @close="close">
    <ConnectForm v-model:provider="shown" :by-address="byAddress" :resume="resume ?? null" @close="emit('close')" />
  </ConsoleDialog>
</template>
