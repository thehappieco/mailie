<script setup lang="ts">
// What allowing actions means on this server, in plain words: who can ask
// for a change, which changes, where they are made and how far they can be
// undone, what they mean on Gmail, what the server never does, and how to
// stop it. The same text wherever the question is asked, because it is what
// the person agrees to: revision ACTIONS_TEXT_VERSION (versions.ts), and a
// new text comes with a new value there.
import { t } from '../ui/i18n'
</script>

<template>
  <div class="consent-text">
    <p class="consent-lead">{{ t('This server changes your mailboxes only when it is asked to: by you, or by a tool you gave an API key that can act.') }}</p>
    <p>{{ t('It can make these changes, and no others:') }}</p>
    <ul>
      <li>{{ t('marking messages as read or unread') }}</li>
      <li>{{ t('starring messages, or removing the star') }}</li>
      <li>{{ t('archiving messages') }}</li>
      <li>{{ t('moving messages to another folder') }}</li>
      <li>{{ t('moving messages to the trash') }}</li>
    </ul>
    <p>{{ t('Each change is made on your mail server, so your other mail apps show it too. It can be undone as far as your provider allows: a message can be moved back, but a provider may empty Spam and Trash on its own.') }}</p>
    <p>{{ t('On Gmail, archiving takes a message out of the Inbox. A message with no other label then also leaves this server’s index, which does not sync All Mail; it stays in Gmail.') }}</p>
    <p>{{ t('This server never deletes a message permanently and never changes anything on its own.') }}</p>
    <p>{{ t('You can turn actions off here at any time, and they stop at once.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
