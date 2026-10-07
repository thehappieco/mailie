<script setup lang="ts">
// The API keys of the workspace shown (docs/workspaces.md, "API keys"), for
// its owners and admins (in a personal workspace, its person): every key, the
// ones revoked or expired too (they answer "what could have reached this
// mailbox"), live ones first; who created each, what it holds on each
// mailbox, and the notes a key carried over from before keys belonged to
// workspaces needs; creating one, opening one to change what it holds and see
// its sends (KeySheet), and revoking one after saying what that does. And,
// where the edition offers the MCP server at this origin and the server says
// it answers there (state/mcp.ts), how to connect an AI assistant with a key.
// A key's secret is never here: only the creation dialog ever holds it.
import { computed, onMounted, ref, watch } from 'vue'
import type { WorkspaceKey } from '../api/types'
import { accounts } from '../state/accounts'
import { apiKeys, atKeyLimit, loadKeys, revokeKey } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { keysMaySend, loadMcpAccess, mcpOffered } from '../state/mcp'
import { session } from '../state/session'
import { loadMembers, personName, team } from '../state/team'
import { currentWorkspace, workspaces } from '../state/workspaces'
import { workspaceName } from '../ui/access'
import { announce } from '../ui/announce'
import { MAX_LIVE_KEYS, keyFlagsSummary, keyOriginNote, keyPerson, keyStanding, scopeLabel, standingLabel, type KeyStanding } from '../ui/apikeys'
import { describe } from '../ui/errors'
import { count, dayStamp, since } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import CreateKeyDialog from './CreateKeyDialog.vue'
import KeySheet from './KeySheet.vue'
import McpConnect from './McpConnect.vue'

const showMcp = computed(mcpOffered)
const creating = ref(false)
/** The prefix of the key whose sheet is open. */
const opened = ref('')
const openedKey = computed(() => apiKeys.list.find(key => key.prefix === opened.value) ?? null)
/** The key the revoke dialog asks about. */
const revoking = ref<WorkspaceKey | null>(null)
const revokeProblem = ref<Failure | null>(null)
/** What was last done, for the notice. */
const done = ref('')
const heading = ref<HTMLElement | null>(null)
const headingTarget = () => heading.value

const workspace = computed(currentWorkspace)
const inTeam = computed(() => workspace.value?.kind === 'team')
const teamName = computed(() => workspaceName(workspace.value))
const me = computed(() => session.user?.id ?? '')
const tone: Record<KeyStanding, string> = { live: 'live', expired: 'off', revoked: 'bad' }
const rank: Record<KeyStanding, number> = { live: 0, expired: 1, revoked: 2 }
/** Working keys first, then the ones that no longer work; newest first within each. */
const ordered = computed(() => apiKeys.list
  .map(key => ({ key, standing: keyStanding(key) }))
  .sort((a, b) => rank[a.standing] - rank[b.standing] || b.key.created_at - a.key.created_at))
const limited = computed(() => atKeyLimit())

/** A mailbox of the workspace, by address; one this page does not list any more, as removed. */
const emailOf = (accountID: string) => accounts.list.find(item => item.id === accountID)?.email ?? t('A removed mailbox')
/** What a key holds, mailbox by mailbox, in words. */
function holds(key: WorkspaceKey): string {
  if (!key.mailboxes.length) return t('None yet')
  return key.mailboxes.map(item => `${emailOf(item.account_id)} (${keyFlagsSummary(item)})`).join(', ')
}
const creator = (key: WorkspaceKey) => keyPerson(key.created_by, me.value, personName)

function create() { done.value = ''; creating.value = true }
function open(key: WorkspaceKey) { done.value = ''; opened.value = key.prefix }
function askRevoke(key: WorkspaceKey) { revokeProblem.value = null; done.value = ''; revoking.value = key }
function closeRevoke() { if (!apiKeys.revoking) revoking.value = null }
const revokeText = computed(() => {
  const key = revoking.value
  if (!key) return ''
  return key.carried_over
    ? t('{name} loses every mailbox of this workspace at once, and is revoked if it holds none elsewhere. This cannot be undone.', { name: key.name })
    : t('{name} stops working at once: a tool using it loses access at its next request. This cannot be undone.', { name: key.name })
})
async function confirmRevoke() {
  const key = revoking.value
  if (!key || apiKeys.revoking) return
  revokeProblem.value = await revokeKey(key.prefix)
  if (revokeProblem.value) return
  revoking.value = null
  // A key carried over from before loses this workspace's mailboxes, and
  // may still work in another workspace.
  done.value = key.carried_over
    ? t('{name} no longer holds this workspace’s mailboxes, and was revoked if it held none elsewhere.', { name: key.name })
    : t('{name} was revoked. A tool using it can no longer reach these mailboxes.', { name: key.name })
  announce(done.value)
}

/** The keys, the members who created them, and whether /mcp is served if the server could not say yet. */
function refresh() {
  void loadKeys()
  if (inTeam.value) void loadMembers()
  void loadMcpAccess()
}
function readTeam() {
  if (inTeam.value && !team.members.loaded && !team.members.loading) void loadMembers()
}

onMounted(() => {
  if (!apiKeys.loaded && !apiKeys.loading) void loadKeys()
  readTeam()
  void loadMcpAccess()
})
// Another workspace shown: its keys, read afresh.
watch(() => workspaces.currentID, () => {
  done.value = ''
  opened.value = ''
  revoking.value = null
  if (!apiKeys.loaded && !apiKeys.loading) void loadKeys()
  readTeam()
})
</script>

<template>
  <div class="console-section keys-section">
    <!-- Said by the live region (ui/announce.ts); a status created with its text is often not read. -->
    <p v-if="done" class="success"><AppIcon name="check" :size="18" /><span>{{ done }}</span></p>

    <div class="section-title">
      <h2 ref="heading" tabindex="-1">{{ inTeam ? t('API keys of {team}', { team: teamName }) : t('Your API keys') }}</h2>
      <div class="section-actions">
        <button class="ghost small" type="button" :disabled="apiKeys.loading" @click="refresh"><AppIcon name="refresh" :size="16" />{{ t('Refresh') }}</button>
        <button v-if="!apiKeys.loaded || apiKeys.list.length" class="primary small" type="button" aria-haspopup="dialog" :disabled="limited" @click="create"><AppIcon name="plus" :size="16" />{{ t('Create key') }}</button>
      </div>
    </div>
    <p v-if="limited" class="dim limit-note">{{ t('This workspace has {count} active keys, the most it can have. Revoke one to create another.', { count: count(MAX_LIVE_KEYS) }) }}</p>

    <div v-if="apiKeys.failure" class="alert with-action" role="alert"><span>{{ describe(apiKeys.failure) }}</span><button class="ghost small" type="button" @click="loadKeys">{{ t('Try again') }}</button></div>
    <p v-if="apiKeys.loading && !apiKeys.loaded" class="dim" role="status">{{ t('Loading the API keys…') }}</p>

    <section class="cards" :aria-label="inTeam ? t('API keys of {team}', { team: teamName }) : t('Your API keys')" :aria-busy="apiKeys.loading">
      <article v-for="{ key, standing } in ordered" :key="key.prefix" class="account-card key-card" :class="{ ended: standing !== 'live' }" :data-key="key.prefix">
        <div class="head">
          <span class="provider-tile"><AppIcon name="key" :size="21" /></span>
          <span class="status-chip" :class="tone[standing]"><i aria-hidden="true" />{{ standingLabel(standing) }}</span>
        </div>
        <div class="identity"><div class="name">{{ key.name }}</div><div class="sub mono" translate="no">{{ key.prefix }}…</div></div>
        <dl class="counts">
          <div><dt>{{ t('Access') }}</dt><dd>{{ scopeLabel(key.scope) }}</dd></div>
          <div class="wide"><dt>{{ t('Mailboxes') }}</dt><dd>{{ holds(key) }}</dd></div>
          <div v-if="inTeam && creator(key)"><dt>{{ t('Created by') }}</dt><dd>{{ creator(key) }}</dd></div>
          <div><dt>{{ t('Created') }}</dt><dd>{{ dayStamp(key.created_at) }}</dd></div>
          <div v-if="standing === 'revoked'"><dt>{{ t('Revoked on') }}</dt><dd>{{ dayStamp(key.revoked_at) }}</dd></div>
          <div v-else><dt>{{ standing === 'expired' ? t('Expired on') : t('Expires') }}</dt><dd>{{ dayStamp(key.expires_at) }}</dd></div>
          <div><dt>{{ t('Last used') }}</dt><dd>{{ key.last_used_at ? since(key.last_used_at) : t('Never') }}</dd></div>
        </dl>
        <p v-if="keyOriginNote(key)" class="hint origin-note">{{ keyOriginNote(key) }}</p>
        <div class="row-actions">
          <button class="ghost" type="button" aria-haspopup="dialog" @click="open(key)"><AppIcon :name="standing === 'live' ? 'pencil' : 'eye'" :size="15" />{{ standing === 'live' ? (keysMaySend() ? t('Mailboxes and sends…') : t('Mailboxes…')) : t('Details…') }}</button>
          <button v-if="standing === 'live'" class="ghost revoke" type="button" aria-haspopup="dialog" @click="askRevoke(key)"><AppIcon name="close" :size="15" />{{ t('Revoke') }}</button>
        </div>
      </article>
      <div v-if="apiKeys.loaded && !apiKeys.list.length" class="empty-card">
        <span class="empty-icon"><AppIcon name="key" :size="30" /></span>
        <h3>{{ t('No API keys yet') }}</h3>
        <p>{{ inTeam
          ? t('A key lets a tool, such as an AI assistant, use the mailboxes of {team} it is given: Read only from someone who reads them. Every owner and admin of the team sees it and can revoke it.', { team: teamName })
          : t('A key lets a tool, such as an AI assistant, use the mailboxes you give it. You decide what it may do on each one and for how long, and you can revoke it at any time.') }}</p>
        <button class="primary" type="button" aria-haspopup="dialog" @click="create"><AppIcon name="plus" :size="18" />{{ t('Create key') }}</button>
      </div>
    </section>

    <p v-if="inTeam" class="hint keys-rule">{{ t('Every owner and admin of {team} sees these keys and can revoke them. What a key holds stays when whoever gave it, or created it, loses their own access; a key stops when the person who created it leaves the team or is disabled on this server. Keys never count as readers of a mailbox.', { team: teamName }) }}</p>

    <McpConnect v-if="showMcp" />

    <CreateKeyDialog v-if="creating" :return-focus="headingTarget" @close="creating = false" />
    <KeySheet v-if="openedKey" :key-prefix="openedKey.prefix" :return-focus="headingTarget" @close="opened = ''" @revoke="askRevoke(openedKey!); opened = ''" />

    <ConsoleDialog v-if="revoking" :title="t('Revoke this key?')" :busy="apiKeys.revoking === revoking.prefix" :return-focus="headingTarget" @close="closeRevoke">
      <div class="form-stack">
        <p class="dim">{{ revokeText }}</p>
        <p v-if="revokeProblem" class="alert" role="alert">{{ describe(revokeProblem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="!!apiKeys.revoking" @click="closeRevoke">{{ t('Cancel') }}</button>
          <button class="danger" type="button" :disabled="!!apiKeys.revoking" @click="confirmRevoke">{{ apiKeys.revoking ? t('Revoking…') : t('Revoke key') }}</button>
        </div>
      </div>
    </ConsoleDialog>
  </div>
</template>

<style scoped>
.console-section { display: grid; gap: 18px; }
.success { margin: 0; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.section-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.section-title h2 { font-size: 15px; margin: 0; font-weight: 600; overflow-wrap: anywhere; }
/* A place to return focus to, not a control: no ring. */
.section-title h2:focus { outline: none; }
.section-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.section-actions button { display: inline-flex; align-items: center; gap: 6px; }
.limit-note { margin: -8px 0 0; font-size: 12px; }
.key-card.ended { background: var(--bg-panel); }
.key-card.ended .provider-tile { background: var(--bg-hover); color: var(--text-dim); }
.key-card .sub { font-size: 12px; }
.key-card .counts .wide { grid-column: 1 / -1; }
.origin-note { margin: 0; font-size: 12px; }
.row-actions { margin-top: auto; display: flex; flex-wrap: wrap; gap: 8px; }
.row-actions button { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; padding: 8px 12px; }
.revoke { color: var(--danger); }
.keys-rule { margin: 0; }
.empty-icon { width: 56px; height: 56px; border-radius: 16px; display: grid; place-items: center; background: var(--accent-dim); color: var(--accent); }
.empty-card .primary { display: inline-flex; align-items: center; gap: 8px; margin-top: 4px; }
@media (max-width: 760px) {
  .section-actions { width: 100%; }
  .section-actions .primary { flex: 1; justify-content: center; }
  .account-card { padding: 17px; }
}
</style>
