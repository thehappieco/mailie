<script setup lang="ts">
// What turning sync on means on this server, in plain words: what is
// stored, where, from when, for how long, how it goes away, and who can read
// it. The same text wherever the question is asked, because it is what the
// person agrees to: revision SYNC_TEXT_VERSION (versions.ts), and a new text
// comes with a new value there.
//
// A mailbox syncs under the agreement of the person who connected it, in
// their personal workspace or in a team; in a team, the members given read
// access to it read its index too, and turning sync off deletes the index of
// every mailbox the person connected, team mailboxes included, unless
// another member took the link over first (docs/workspaces.md, "Consents").
import { t } from '../ui/i18n'
</script>

<template>
  <div class="consent-text">
    <p>{{ t('With sync on, this server keeps an index of the mail in the mailboxes you connect, in your personal workspace or in a team, in its own database, so it knows what each folder holds and notices new messages as they arrive. Nothing is indexed until you turn sync on.') }}</p>
    <p class="consent-lead">{{ t('For each message, the index holds:') }}</p>
    <ul>
      <li>{{ t('who sent it and who it was sent to, with their names') }}</li>
      <li>{{ t('its subject, dates and size') }}</li>
      <li>{{ t('the folder it is in, and flags such as read or starred') }}</li>
      <li>{{ t('the identifiers that tie a reply to its conversation') }}</li>
      <li>{{ t('the type, size and file name of each part, but not what the part contains') }}</li>
    </ul>
    <p>{{ t('Message bodies and attachments are never stored. When someone who may read the mailbox, or a tool with their key, asks for one, this server fetches it from the mail server and does not keep it.') }}</p>
    <p>{{ t('Sync starts with the last 90 days, newest first, then keeps up with new mail. Every folder is synced except All Mail, Starred and Important in Gmail. The server also keeps a log of recent changes: an entry is deleted once it is 7 days old, unless it is among the 10,000 most recent on this server.') }}</p>
    <p>{{ t('Who can read the index: in your personal workspace, only you. In a team, also the members given read access to the mailbox, which only someone who already has it can give, and the tools they give a key: they can search its index and have this server fetch its messages.') }}</p>
    <p>{{ t('The index is kept until you turn sync off, the mailbox is removed, or your account on this server is closed. Turning sync off deletes the index of every mailbox you connected, team mailboxes included, even while others read them. If another member takes over a team mailbox’s link first, its index is kept under their agreement instead of yours.') }}</p>
    <p>{{ t('Whoever runs this server can read its database, this index included.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
