<script setup lang="ts">
// The add dialog's form fields. Separate from the dialog so what they render
// can be checked on its own: the mailbox password must never reach the markup.
//
// iCloud asks for the address and an app-specific password, plus the iCloud
// address to sign in with for a custom domain: Apple's servers are fixed, and
// the server fills them in itself.
import { computed, ref, useId, watch } from 'vue'
import type { ProviderID, SMTPSecurity } from '../api/types'
import { guessHosts, ICLOUD_PASSWORD_HELP, isICloudAddress, portForSecurity, signsInWithPassword, type AccountDraft } from '../ui/accountDraft'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import PasswordInput from './PasswordInput.vue'

// offerIcloud: the server can connect iCloud, so an Apple address typed into
// the IMAP form is pointed there instead of at a guessed server that is wrong.
const props = defineProps<{ provider: ProviderID; busy?: boolean; offerIcloud?: boolean }>()
const emit = defineEmits<{ 'use-icloud': [] }>()
const draft = defineModel<AccountDraft>('draft', { required: true })
const id = useId()
const suggestICloud = computed(() => props.provider === 'imap' && props.offerIcloud && isICloudAddress(draft.value.email))

// Offer the conventional server names for the address until the person
// types their own; a name they typed is never overwritten.
const guessed = ref({ imap: '', smtp: '' })
watch(() => draft.value.email, email => {
  if (props.provider !== 'imap') return
  const next = guessHosts(email)
  if (draft.value.imapHost === guessed.value.imap) draft.value.imapHost = next.imap
  if (draft.value.smtpHost === guessed.value.smtp) draft.value.smtpHost = next.smtp
  guessed.value = next
})

function changeSecurity(event: Event) {
  const to = (event.target as HTMLSelectElement).value as SMTPSecurity
  draft.value.smtpPort = portForSecurity(draft.value.smtpPort, draft.value.smtpTLS, to)
  draft.value.smtpTLS = to
}
</script>

<template>
  <label :for="`${id}-email`">{{ t('Email address') }}
    <input :id="`${id}-email`" v-model="draft.email" type="email" name="mailbox" required maxlength="320" autocapitalize="off"
      spellcheck="false" inputmode="email" autocomplete="off" data-1p-ignore data-lpignore="true" :disabled="busy" :placeholder="provider === 'icloud' ? 'name@icloud.com' : 'name@example.com'" />
  </label>
  <div v-if="suggestICloud" class="suggestion">
    <p :id="`${id}-icloud-hint`">{{ t('This is an iCloud address: the iCloud Mail option already knows Apple’s servers.') }}</p>
    <button class="ghost small" type="button" :aria-describedby="`${id}-icloud-hint`" :disabled="busy" @click="emit('use-icloud')">{{ t('Use iCloud Mail') }}</button>
  </div>
  <div v-if="signsInWithPassword(provider)" class="with-hint">
    <!-- The label ends before the field: wrapped around it, it would take the
         show/hide button's name into the field's own. -->
    <div class="password-field">
      <label :for="`${id}-password`">{{ provider === 'icloud' ? t('App-specific password') : t('Password') }}</label>
      <!-- Not autocomplete="current-password": that would offer the Mailie
           password for somebody else's mailbox. -->
      <PasswordInput :id="`${id}-password`" v-model="draft.password" name="mailbox-password" required autocomplete="off" data-1p-ignore data-lpignore="true"
        :aria-describedby="provider === 'icloud' ? `${id}-password-hint ${id}-password-steps` : `${id}-password-hint`" :disabled="busy" />
    </div>
    <template v-if="provider === 'icloud'">
      <p :id="`${id}-password-hint`" class="hint">{{ t('iCloud needs an app-specific password here, not your Apple Account password. Two-factor authentication must be on for your Apple Account.') }}</p>
      <p :id="`${id}-password-steps`" class="hint">{{ t('Create one at account.apple.com → Sign-In and Security → App-Specific Passwords.') }}</p>
      <a class="help-link" :href="ICLOUD_PASSWORD_HELP" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">{{ t('How to create an app-specific password') }}<AppIcon name="external" :size="14" /></a>
    </template>
    <p v-else :id="`${id}-password-hint`" class="hint">{{ t('Many providers require an app password here instead of your usual password.') }}</p>
  </div>
  <!-- Apple refuses an iCloud+ custom-domain address as the sign-in, on IMAP
       and SMTP alike: those accounts sign in as their iCloud address. -->
  <div v-if="provider === 'icloud'" class="with-hint">
    <label :for="`${id}-icloud-login`">{{ t('iCloud address to sign in with (optional)') }}
      <input :id="`${id}-icloud-login`" v-model="draft.loginUser" type="email" maxlength="320" autocapitalize="off" spellcheck="false" inputmode="email" autocomplete="off"
        data-1p-ignore data-lpignore="true" :aria-describedby="`${id}-icloud-login-hint`" :disabled="busy" placeholder="name@icloud.com" />
    </label>
    <p :id="`${id}-icloud-login-hint`" class="hint">{{ t('Only for a custom domain on iCloud+: Apple does not accept that address as the sign-in, only your iCloud address.') }}</p>
  </div>
  <template v-if="provider === 'imap'">
    <fieldset class="server-group" :disabled="busy">
      <legend>{{ t('Incoming mail (IMAP)') }}</legend>
      <div class="server-row">
        <label :for="`${id}-imap-host`">{{ t('Server') }}<input :id="`${id}-imap-host`" v-model="draft.imapHost" required maxlength="253" autocapitalize="off" spellcheck="false" autocomplete="off" placeholder="imap.example.com" /></label>
        <label :for="`${id}-imap-port`">{{ t('Port') }}<input :id="`${id}-imap-port`" v-model.number="draft.imapPort" type="number" required min="1" max="65535" inputmode="numeric" /></label>
      </div>
      <p class="hint">{{ t('Always over TLS. Mailie never signs in to IMAP without encryption.') }}</p>
    </fieldset>
    <fieldset class="server-group" :disabled="busy">
      <legend>{{ t('Outgoing mail (SMTP)') }}</legend>
      <div class="server-row">
        <label :for="`${id}-smtp-host`">{{ t('Server') }}<input :id="`${id}-smtp-host`" v-model="draft.smtpHost" required maxlength="253" autocapitalize="off" spellcheck="false" autocomplete="off" placeholder="smtp.example.com" /></label>
        <label :for="`${id}-smtp-port`">{{ t('Port') }}<input :id="`${id}-smtp-port`" v-model.number="draft.smtpPort" type="number" required min="1" max="65535" inputmode="numeric" /></label>
      </div>
      <label :for="`${id}-smtp-tls`">{{ t('Security') }}
        <select :id="`${id}-smtp-tls`" :value="draft.smtpTLS" @change="changeSecurity">
          <option value="implicit">{{ t('SSL/TLS (usually port 465)') }}</option>
          <option value="starttls">{{ t('STARTTLS (usually port 587)') }}</option>
        </select>
      </label>
    </fieldset>
    <div class="with-hint">
      <label :for="`${id}-login`">{{ t('Login name (optional)') }}
        <input :id="`${id}-login`" v-model="draft.loginUser" maxlength="320" autocapitalize="off" spellcheck="false" autocomplete="off" :aria-describedby="`${id}-login-hint`" :disabled="busy" />
      </label>
      <p :id="`${id}-login-hint`" class="hint">{{ t('Only when the server expects something other than the email address.') }}</p>
    </div>
  </template>
  <div class="with-hint">
    <label :for="`${id}-name`">{{ t('Display name (optional)') }}
      <input :id="`${id}-name`" v-model="draft.displayName" maxlength="120" autocomplete="off" :aria-describedby="`${id}-name-hint`" :disabled="busy" :placeholder="t('For example: Support')" />
    </label>
    <p :id="`${id}-name-hint`" class="hint">{{ t('How this mailbox is named in Mailie. The provider is not changed.') }}</p>
  </div>
</template>

<style scoped>
.server-group { display: grid; gap: 12px; margin: 0; padding: 14px; border: 1px solid var(--line); border-radius: 12px; min-width: 0; }
.server-group legend { padding: 0 6px; font-size: 13px; font-weight: 600; }
.server-row { display: grid; grid-template-columns: minmax(0, 1fr) 96px; gap: 10px; }
.with-hint { display: grid; gap: 6px; }
.password-field { display: grid; gap: 8px; }
.hint { font-size: 12px; color: var(--text-dim); line-height: 1.5; margin: 0; }
/* Not a flex box: when the text wraps, the icon stays after its last word. */
.help-link { justify-self: start; font-size: 13px; font-weight: 600; line-height: 1.5; text-decoration: none; padding: 2px 0; }
.help-link:hover { text-decoration: underline; }
.help-link .app-icon { margin-left: 5px; vertical-align: -2px; }
.suggestion { display: grid; justify-items: start; gap: 8px; margin: -4px 0 0; padding: 10px 12px; border-left: 3px solid var(--accent); border-radius: 0 12px 12px 0; background: var(--bg-hover); }
.suggestion p { margin: 0; font-size: 12.5px; line-height: 1.5; color: var(--text-dim); }
@media (max-width: 420px) { .server-row { grid-template-columns: minmax(0, 1fr) 84px; } }
</style>
