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
// on this server, for its owners and admins (a member of a team sees no
// Members, and its mailboxes say who manages them). Teams mirrored from
// elsewhere are changed there. API keys are the workspace's: here for the
// owners and admins of the team shown, or the person of their personal
// workspace (a member of a team sees none, and its mailboxes say who manages
// them). The account is the person's own, the same in every workspace: the
// header names no workspace on it.
import { computed } from 'vue'
import AccountsPanel from '../components/AccountsPanel.vue'
import ConsoleShell, { type ConsoleSection } from '../components/ConsoleShell.vue'
import KeysPanel from '../components/KeysPanel.vue'
import StoragePanel from '../components/StoragePanel.vue'
import { accounts } from '../state/accounts'
import { currentWorkspace, workspaces } from '../state/workspaces'
import { administers, administersKeys, changeableTeam, teamsCreatedHere, workspaceName } from '../ui/access'
import { t } from '../ui/i18n'
import OpenAccount from './OpenAccount.vue'
import OpenMembers from './OpenMembers.vue'

const shown = computed(currentWorkspace)
const members = computed(() => workspaces.supported
  && ((changeableTeam(shown.value) && administers(shown.value)) || (shown.value?.kind === 'personal' && teamsCreatedHere(workspaces.list))))
const keys = computed(() => workspaces.supported && administersKeys(shown.value))

const sections = computed<ConsoleSection[]>(() => [
  {
    id: 'mailboxes', label: t('Mailboxes'), icon: 'mail', component: AccountsPanel,
    description: shown.value?.kind !== 'team'
      ? t('Connect the mailboxes this server reaches for you, check that it can sign in to each one, and follow their sync.')
      : administers(shown.value)
        ? t('Every mailbox of {team}: you manage them by your role, and read those you are given Read on. Check that this server can sign in to each one, follow their sync, and choose who can use them.', { team: workspaceName(shown.value) })
        : t('The mailboxes of {team} you have access to: check that this server can sign in to each one and follow their sync.', { team: workspaceName(shown.value) }),
    count: accounts.loaded ? accounts.list.length : undefined,
  },
  ...(members.value ? [{
    id: 'members', label: t('Members'), icon: 'users' as const, component: OpenMembers,
    description: shown.value?.kind !== 'team'
      ? t('Teams let people on this server share mailboxes: each person uses only the mailboxes they are given access to.')
      : t('Who is in {team}, their roles, and the invitations to join it.', { team: workspaceName(shown.value) }),
  }] : []),
  ...(keys.value ? [{
    id: 'keys', label: t('API keys & MCP'), icon: 'key' as const, component: KeysPanel,
    description: shown.value?.kind !== 'team'
      ? t('Create keys for your own tools, such as an AI assistant, and connect them to this server over MCP. A key reaches only the mailboxes you give it, and you can revoke it at any time.')
      : t('The keys of {team} for its tools, such as an AI assistant, and how to connect them to this server over MCP. A key reaches only the mailboxes it is given, and every owner and admin of the team sees it and can revoke it.', { team: workspaceName(shown.value) }),
  }] : []),
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
