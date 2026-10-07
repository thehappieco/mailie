<script setup lang="ts">
// What creating an API key authorizes on this server, in plain words
// (docs/workspaces.md, "API keys"): who can use it; that it belongs to the
// workspace, whose owners and admins see it, choose its mailboxes and what it
// does in each, and revoke it, and that whoever runs the server can list and
// revoke every key too; that it reads only mailboxes given to it by someone
// who reads them; what a tool with it can do there (search, and read whole
// messages with their attachments, fetched from the mail server; with write,
// the actions, where it is given Act, whatever anyone chooses about actions
// in their account (ActionsText.vue); with send, sending, where it is
// given Send, every message confirmed by the tool's own request, under the
// mailbox's address alone, and recorded without its text or recipients);
// what it can never do; when it stops (it expires, it is revoked, or the
// person who creates it leaves the workspace or is disabled or deleted on
// this server); what the server keeps of what it sends (nothing stored; what
// the MCP server streamed, in memory for resuming, within internal/mcp's
// DefaultEventStoreAge); that the tool decides what it does with the data;
// that keys created under the earlier terms keep those, and never send; and
// how to stop it. Creating the key is the person's agreement to this text,
// recorded on the key as KEY_TERMS_VERSION (versions.ts): a new text comes
// with a new value there. The actions and sending lines are part of the same
// text, shown when the scope chosen includes them; the workspace's lines are
// worded for a team (team: its name) or for the person's own workspace.
import { t } from '../ui/i18n'

defineProps<{ write: boolean; send?: boolean; team?: string }>()
</script>

<template>
  <div class="consent-text">
    <p class="consent-lead">{{ t('Whoever holds this key can reach the mailboxes it is given through this server’s API and MCP server, until the key expires or is revoked. Give it only to a tool you trust.') }}</p>
    <p v-if="team">{{ t('The key belongs to {team}: its owners and admins see it, choose which of the team’s mailboxes it reaches and what it may do in each, and can revoke it. It reads only the mailboxes given to it by someone who reads them, and keeps what it is given until an owner or an admin takes it away.', { team }) }}</p>
    <p v-else>{{ t('The key belongs to your personal workspace: you see it, choose which of your mailboxes it reaches and what it may do in each, and can revoke it.') }}</p>
    <p>{{ t('With this key, a tool can:') }}</p>
    <ul>
      <li>{{ t('list the mailboxes the key reaches, and their folders') }}</li>
      <li>{{ t('where it is given Read: search the messages by the details in the index (sender, recipients, subject and dates), read any of them, text and attachments included, which this server fetches from the mail server when the tool asks, and hear when new mail arrives') }}</li>
      <li v-if="write || send">{{ t('where it is given Act: mark messages as read or unread, star them, archive them, and move them to another folder or to the trash; turning actions off in an account does not stop it, taking Act away from it does') }}</li>
      <li v-if="send">{{ t('where it is given Send: send email from the mailbox, under its address alone. Each message is sent only when the tool’s own request confirms it (confirm: true), and this server keeps a record of each send under the key for 30 days: how many recipients, never who they are, the subject or the text.') }}</li>
    </ul>
    <p v-if="send">{{ t('It cannot connect or remove mailboxes, create other keys, or change anyone’s access or account.') }}</p>
    <p v-else>{{ t('It cannot send email, connect or remove mailboxes, create other keys, or change anyone’s access or account.') }}</p>
    <p v-if="team">{{ t('It stops working when it expires or is revoked, and when you leave {team} or your account on this server is disabled or deleted.', { team }) }}</p>
    <p v-else>{{ t('It stops working when it expires or is revoked, and when your account on this server is disabled or deleted.') }}</p>
    <p>{{ t('This server does not store what it fetches for the tool. So that the tool can pick up a dropped connection, the MCP server holds what it sent in memory only, never on disk, for at most five minutes.') }}</p>
    <p>{{ t('What the tool does with the data is up to that tool and whoever provides it, under their own terms. An AI assistant such as Claude, for example, sends it to Anthropic to answer you.') }}</p>
    <p>{{ t('Keys created under this server’s earlier terms for API keys keep working under those terms, and never send email.') }}</p>
    <p v-if="team">{{ t('You, and any owner or admin of {team}, can revoke the key here whenever you want, and it stops working at once.', { team }) }}</p>
    <p v-else>{{ t('You can revoke the key here whenever you want, and it stops working at once.') }}</p>
    <p>{{ t('Whoever runs this server can also list every API key, with the mailboxes it holds, and revoke it.') }}</p>
  </div>
</template>

<style scoped>
.consent-text { display: grid; gap: 10px; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.consent-text p { margin: 0; }
.consent-text .consent-lead { color: var(--text); font-weight: 600; }
.consent-text ul { margin: -4px 0 0; padding-left: 20px; display: grid; gap: 4px; color: var(--text); }
</style>
