<script setup lang="ts">
// What turning sync on means on this server, in plain words: what is
// stored, where, from when, for how long, how it goes away, and who can read
// it. The same text wherever the question is asked, because it is what is
// agreed to: revision SYNC_TEXT_VERSION (versions.ts), and a new text comes
// with a new value there.
//
// A person agrees to it for the mailboxes of their personal workspace; an
// owner or an admin of a team agrees to it on the team's behalf, for one of
// the team's mailboxes, when connecting it or later (docs/workspaces.md,
// "Consents"). Turning a person's sync off deletes the index of their
// personal mailboxes; turning a team mailbox's off, its index for everyone
// who reads it. Owners and admins see each team mailbox's card, never its
// mail. Closing a person's account removes their personal mailboxes and the
// teams they were the only member of; and a team mailbox the upgrade left
// syncing under its linker's own agreement stops with it until the team
// agrees (migration 0011): the text says both, since it is what is agreed to.
import { t } from '../ui/i18n'
</script>

<template>
  <div class="consent-text">
    <p>{{ t('With sync on, this server keeps an index of a mailbox’s mail in its own database, so it knows what each folder holds and notices new messages as they arrive. Nothing is indexed until sync is turned on.') }}</p>
    <p>{{ t('The mailboxes of your personal workspace sync under your agreement, which you give here. A team’s mailbox syncs under the team’s agreement: the owner or admin of the team who connects it, or turns its sync on later, agrees to this same text on the team’s behalf.') }}</p>
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
    <p>{{ t('Who can read the index: of a mailbox of your personal workspace, only you. Of a team’s mailbox, the members given read access to it, which only an owner or an admin of the team who reads it can give, and the tools they give a key: they can search its index and have this server fetch its messages. Owners and admins read nothing by their role: they see each mailbox’s address, its state and how far its sync has got, but not its mail.') }}</p>
    <p>{{ t('The index is kept until sync is turned off, the mailbox is removed, or, for a mailbox of your personal workspace, your account on this server is closed. Turning your sync off deletes the index of your personal workspace’s mailboxes only. Any owner or admin of a team can turn a team mailbox’s sync off, which deletes its index for everyone who reads it, and a team mailbox nobody can read any more stops syncing. Closing your account on this server does not remove the mailboxes of a team other people are in, nor their index; a team you are the only member of is deleted with your account, with its mailboxes and their index.') }}</p>
    <p>{{ t('One exception comes from before teams agreed to sync on their own: a team mailbox that still syncs under the agreement you gave for your own mailboxes stays tied to it until an owner or an admin of the team agrees for the team. Until then, turning your sync off, or closing your account, stops it and deletes its index.') }}</p>
    <p>{{ t('Whoever runs this server can read its database, this index included.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
