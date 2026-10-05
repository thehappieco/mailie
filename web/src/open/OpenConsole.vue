<script setup lang="ts">
// The console a self-hosted server serves: where the person who runs it, and
// the people they invite, reach the server's settings for their own
// mailboxes. Mailboxes first, then the people of the team shown (or making a
// team, in the personal workspace), API keys and how to connect a tool over
// MCP, what the index holds for them, and their account. There is no mail to
// read or send here: a tool reads it, with a key.
//
// Members is here only while the workspace shown is one whose people are
// changed here: the personal workspace, where teams are made, or a team made
// on this server. Teams mirrored from elsewhere are changed there. Its line
// names the invitations only to whoever sees them: a team's owners and
// admins. API keys and the account are the person's own, the same in every
// workspace: the header names no workspace on them.
import { computed } from 'vue'
import AccountsPanel from '../components/AccountsPanel.vue'
import ConsoleShell, { type ConsoleSection } from '../components/ConsoleShell.vue'
import KeysPanel from '../components/KeysPanel.vue'
import StoragePanel from '../components/StoragePanel.vue'
import { accounts } from '../state/accounts'
import { currentWorkspace, workspaces } from '../state/workspaces'
import { changeableTeam, seesInvitations, teamsCreatedHere, workspaceName } from '../ui/access'
import { t } from '../ui/i18n'
import OpenAccount from './OpenAccount.vue'
import OpenMembers from './OpenMembers.vue'

const shown = computed(currentWorkspace)
const members = computed(() => workspaces.supported && (changeableTeam(shown.value) || (shown.value?.kind === 'personal' && teamsCreatedHere(workspaces.list))))

const sections = computed<ConsoleSection[]>(() => [
  {
    id: 'mailboxes', label: t('Mailboxes'), icon: 'mail', component: AccountsPanel,
    description: shown.value?.kind === 'team'
      ? t('The mailboxes of {team} you have access to: check that this server can sign in to each one and follow their sync. Access to a mailbox’s mail is given only by someone who has it.', { team: workspaceName(shown.value) })
      : t('Connect the mailboxes this server reaches for you, check that it can sign in to each one, and follow their sync.'),
    count: accounts.loaded ? accounts.list.length : undefined,
  },
  ...(members.value ? [{
    id: 'members', label: t('Members'), icon: 'users' as const, component: OpenMembers,
    description: shown.value?.kind !== 'team'
      ? t('Teams let people on this server share mailboxes: each person uses only the mailboxes they are given access to.')
      : seesInvitations(shown.value.role)
        ? t('Who is in {team}, their roles, and the invitations to join it.', { team: workspaceName(shown.value) })
        : t('Who is in {team} and their roles.', { team: workspaceName(shown.value) }),
  }] : []),
  {
    id: 'keys', label: t('API keys & MCP'), icon: 'key', component: KeysPanel, scope: 'person',
    description: t('Create keys for your own tools, such as an AI assistant, and connect them to this server over MCP. A key reaches only the mailboxes you choose, and you can revoke it at any time.'),
  },
  {
    id: 'storage', label: t('Storage'), icon: 'server', component: StoragePanel,
    description: t('What the mailboxes you can read take up in this server’s index.'),
  },
  {
    id: 'account', label: t('Account'), icon: 'user', component: OpenAccount, scope: 'person',
    description: t('Your name, your password, where you are signed in, and what you allow this server to do with your mailboxes.'),
  },
])
</script>

<template>
  <ConsoleShell :sections="sections" home="mailboxes" account="account" />
</template>
