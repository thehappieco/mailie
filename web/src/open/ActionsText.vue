<script setup lang="ts">
// What allowing actions means on this server, in plain words: who can ask
// for a change (someone allowed to act on the mailbox, under their own
// agreement: a team's mailboxes are not the person's; and a tool whose API key
// is given Act there, under the key terms, not under anyone's agreement
// here), which changes, where they are made and how far they can be undone,
// what they mean on Gmail, what the server never does, and how to stop it:
// turning actions off stops the person's own, and those of a key they created
// before keys belonged to workspaces, which act only while their creator
// allows actions; a key created since is stopped by taking Act away from it
// or revoking it. The same text wherever the question is asked, because it
// is what the person agrees to: revision ACTIONS_TEXT_VERSION (versions.ts),
// and a new text comes with a new value there.
import { t } from '../ui/i18n'
</script>

<template>
  <div class="consent-text">
    <p class="consent-lead">{{ t('This server changes a mailbox only when someone allowed to act on it asks: you, under this agreement, on the mailboxes where you may act; anyone else allowed to act there, under their own; or a tool with an API key given Act there, under the terms the key was created with.') }}</p>
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
    <p>{{ t('You can turn actions off here at any time, and yours stop at once, as do those of any API key you created before keys belonged to workspaces. A key created since acts under its own terms, not under this agreement: taking Act away from it, or revoking it, stops it.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
