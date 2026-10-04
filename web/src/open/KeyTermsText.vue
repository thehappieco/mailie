<script setup lang="ts">
// What creating an API key authorizes on this server, in plain words: who
// can use it, what a tool with it can do in the chosen mailboxes (search, and
// read whole messages with their attachments, fetched from the mail server;
// with write, also the actions, only while they are allowed), what it can
// never do, what the server keeps of what it sends (nothing stored; what the
// MCP server streamed, in memory for resuming, within internal/mcp's
// DefaultEventStoreAge), that the tool decides what it does with the data,
// and how to stop it. Creating the key is the person's agreement to this
// text, recorded on the key as KEY_TERMS_VERSION (versions.ts): a new text
// comes with a new value there. The actions line is part of the same text,
// shown when the access chosen includes them.
import { t } from '../ui/i18n'

defineProps<{ write: boolean }>()
</script>

<template>
  <div class="consent-text">
    <p class="consent-lead">{{ t('Whoever holds this key can reach the mailboxes it names through this server’s API and MCP server, until the key expires or you revoke it. Give it only to a tool you trust.') }}</p>
    <p>{{ t('With this key, a tool can:') }}</p>
    <ul>
      <li>{{ t('list the mailboxes the key reaches, and their folders') }}</li>
      <li>{{ t('search their messages by the details in the index: sender, recipients, subject and dates') }}</li>
      <li>{{ t('read any of those messages, text and attachments included, which this server fetches from the mail server when the tool asks') }}</li>
      <li>{{ t('wait for new mail, and hear when it arrives') }}</li>
      <li v-if="write">{{ t('mark messages as read or unread, star them, archive them, and move them to another folder or to the trash, for as long as you allow actions on your messages') }}</li>
    </ul>
    <p>{{ t('It cannot send email, connect or remove mailboxes, create other keys, or change your account.') }}</p>
    <p>{{ t('This server does not store what it fetches for the tool. So that the tool can pick up a dropped connection, the MCP server holds what it sent in memory only, never on disk, for at most five minutes.') }}</p>
    <p>{{ t('What the tool does with the data is up to that tool and whoever provides it, under their own terms. An AI assistant such as Claude, for example, sends it to Anthropic to answer you.') }}</p>
    <p>{{ t('You can revoke the key here whenever you want, and it stops working at once.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
