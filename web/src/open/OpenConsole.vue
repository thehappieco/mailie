<script setup lang="ts">
// The console a self-hosted server serves: where the person who runs it, and
// the people they invite, reach the server's settings for their own
// mailboxes. Mailboxes first, then API keys and how to connect a tool over
// MCP, what the index holds for them, and their account. There is no mail to
// read or send here: a tool reads it, with a key.
import { computed } from 'vue'
import AccountsPanel from '../components/AccountsPanel.vue'
import ConsoleShell, { type ConsoleSection } from '../components/ConsoleShell.vue'
import KeysPanel from '../components/KeysPanel.vue'
import StoragePanel from '../components/StoragePanel.vue'
import { accounts } from '../state/accounts'
import { t } from '../ui/i18n'
import OpenAccount from './OpenAccount.vue'

const sections = computed<ConsoleSection[]>(() => [
  {
    id: 'mailboxes', label: t('Mailboxes'), icon: 'mail', component: AccountsPanel,
    description: t('Connect the mailboxes this server reaches for you, check that it can sign in to each one, and follow their sync.'),
    count: accounts.loaded ? accounts.list.length : undefined,
  },
  {
    id: 'keys', label: t('API keys & MCP'), icon: 'key', component: KeysPanel,
    description: t('Create keys for your own tools, such as an AI assistant, and connect them to this server over MCP. A key reaches only the mailboxes you choose, and you can revoke it at any time.'),
  },
  {
    id: 'storage', label: t('Storage'), icon: 'server', component: StoragePanel,
    description: t('What your mailboxes take up in this server’s index.'),
  },
  {
    id: 'account', label: t('Account'), icon: 'user', component: OpenAccount,
    description: t('Your name, your password, where you are signed in, and what you allow this server to do with your mailboxes.'),
  },
])
</script>

<template>
  <ConsoleShell :sections="sections" home="mailboxes" account="account" />
</template>
