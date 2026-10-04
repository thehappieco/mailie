<script setup lang="ts">
// The person's own API keys, the ones revoked or expired too (they answer
// "what could have reached my mail"), creating one, revoking one after saying
// what that does, and, where the edition offers the MCP server at this
// origin and the server says it answers there (state/mcp.ts), how to connect
// an AI assistant with a key. A key's secret is never here: only the
// creation dialog ever holds it.
import { computed, onMounted, ref } from 'vue'
import type { PersonalKey } from '../api/types'
import { accounts } from '../state/accounts'
import { apiKeys, atKeyLimit, loadKeys, revokeKey } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { loadMcpAccess, mcpOffered } from '../state/mcp'
import { announce } from '../ui/announce'
import { MAX_LIVE_KEYS, keyMailboxes, keyStanding, scopeLabel, standingLabel, type KeyStanding } from '../ui/apikeys'
import { describe } from '../ui/errors'
import { count, dayStamp, since } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'
import CreateKeyDialog from './CreateKeyDialog.vue'
import McpConnect from './McpConnect.vue'

const showMcp = computed(mcpOffered)
const creating = ref(false)
/** The key the revoke dialog asks about. */
const revoking = ref<PersonalKey | null>(null)
const revokeProblem = ref<Failure | null>(null)
/** The name of the key last revoked, for the notice. */
const revoked = ref('')
const heading = ref<HTMLElement | null>(null)
const headingTarget = () => heading.value

const tone: Record<KeyStanding, string> = { live: 'live', expired: 'off', revoked: 'bad' }
const rank: Record<KeyStanding, number> = { live: 0, expired: 1, revoked: 2 }
/** Working keys first, then the ones that no longer work; newest first within each. */
const ordered = computed(() => apiKeys.list
  .map(key => ({ key, standing: keyStanding(key) }))
  .sort((a, b) => rank[a.standing] - rank[b.standing] || b.key.created_at - a.key.created_at))
const limited = computed(() => atKeyLimit())

function create() { revoked.value = ''; creating.value = true }
function askRevoke(key: PersonalKey) { revokeProblem.value = null; revoked.value = ''; revoking.value = key }
function closeRevoke() { if (!apiKeys.revoking) revoking.value = null }
async function confirmRevoke() {
  const key = revoking.value
  if (!key || apiKeys.revoking) return
  revokeProblem.value = await revokeKey(key.prefix)
  if (revokeProblem.value) return
  revoking.value = null
  revoked.value = key.name
  announce(t('{name} was revoked. A tool using it can no longer reach your mail.', { name: key.name }))
}

/** The keys, and whether /mcp is served if the server could not say yet. */
function refresh() {
  void loadKeys()
  void loadMcpAccess()
}

onMounted(() => {
  if (!apiKeys.loaded && !apiKeys.loading) void loadKeys()
  void loadMcpAccess()
})
</script>

<template>
  <div class="console-section keys-section">
    <!-- Said by the live region (ui/announce.ts); a status created with its text is often not read. -->
    <p v-if="revoked" class="success"><AppIcon name="check" :size="18" /><span>{{ t('{name} was revoked. A tool using it can no longer reach your mail.', { name: revoked }) }}</span></p>

    <div class="section-title">
      <h2 ref="heading" tabindex="-1">{{ t('Your API keys') }}</h2>
      <div class="section-actions">
        <button class="ghost small" type="button" :disabled="apiKeys.loading" @click="refresh"><AppIcon name="refresh" :size="16" />{{ t('Refresh') }}</button>
        <button v-if="!apiKeys.loaded || apiKeys.list.length" class="primary small" type="button" aria-haspopup="dialog" :disabled="limited" @click="create"><AppIcon name="plus" :size="16" />{{ t('Create key') }}</button>
      </div>
    </div>
    <p v-if="limited" class="dim limit-note">{{ t('You have {count} active keys, the most you can have. Revoke one to create another.', { count: count(MAX_LIVE_KEYS) }) }}</p>

    <div v-if="apiKeys.failure" class="alert with-action" role="alert"><span>{{ describe(apiKeys.failure) }}</span><button class="ghost small" type="button" @click="loadKeys">{{ t('Try again') }}</button></div>
    <p v-if="apiKeys.loading && !apiKeys.loaded" class="dim" role="status">{{ t('Loading your API keys…') }}</p>

    <section class="cards" :aria-label="t('Your API keys')" :aria-busy="apiKeys.loading">
      <article v-for="{ key, standing } in ordered" :key="key.prefix" class="account-card key-card" :class="{ ended: standing !== 'live' }" :data-key="key.prefix">
        <div class="head">
          <span class="provider-tile"><AppIcon name="key" :size="21" /></span>
          <span class="status-chip" :class="tone[standing]"><i aria-hidden="true" />{{ standingLabel(standing) }}</span>
        </div>
        <div class="identity"><div class="name">{{ key.name }}</div><div class="sub mono" translate="no">{{ key.prefix }}…</div></div>
        <dl class="counts">
          <div><dt>{{ t('Access') }}</dt><dd>{{ scopeLabel(key.scope) }}</dd></div>
          <div><dt>{{ t('Mailboxes') }}</dt><dd>{{ keyMailboxes(key, accounts.list).join(', ') }}</dd></div>
          <div><dt>{{ t('Created') }}</dt><dd>{{ dayStamp(key.created_at) }}</dd></div>
          <div v-if="standing === 'revoked'"><dt>{{ t('Revoked on') }}</dt><dd>{{ dayStamp(key.revoked_at) }}</dd></div>
          <div v-else><dt>{{ standing === 'expired' ? t('Expired on') : t('Expires') }}</dt><dd>{{ dayStamp(key.expires_at) }}</dd></div>
          <div><dt>{{ t('Last used') }}</dt><dd>{{ key.last_used_at ? since(key.last_used_at) : t('Never') }}</dd></div>
        </dl>
        <div v-if="standing === 'live'" class="row-actions">
          <button class="ghost revoke" type="button" aria-haspopup="dialog" @click="askRevoke(key)"><AppIcon name="close" :size="15" />{{ t('Revoke') }}</button>
        </div>
      </article>
      <div v-if="apiKeys.loaded && !apiKeys.list.length" class="empty-card">
        <span class="empty-icon"><AppIcon name="key" :size="30" /></span>
        <h3>{{ t('No API keys yet') }}</h3>
        <p>{{ t('A key lets a tool, such as an AI assistant, search and read the mailboxes you choose. You decide what it may do and for how long, and you can revoke it at any time.') }}</p>
        <button class="primary" type="button" aria-haspopup="dialog" @click="create"><AppIcon name="plus" :size="18" />{{ t('Create key') }}</button>
      </div>
    </section>

    <McpConnect v-if="showMcp" />

    <CreateKeyDialog v-if="creating" :return-focus="headingTarget" @close="creating = false" />

    <ConsoleDialog v-if="revoking" :title="t('Revoke this key?')" :busy="apiKeys.revoking === revoking.prefix" :return-focus="headingTarget" @close="closeRevoke">
      <div class="form-stack">
        <p class="dim">{{ t('{name} stops working at once: a tool using it loses access to your mail at its next request. This cannot be undone.', { name: revoking.name }) }}</p>
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
.section-title h2 { font-size: 15px; margin: 0; font-weight: 600; }
/* A place to return focus to, not a control: no ring. */
.section-title h2:focus { outline: none; }
.section-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.section-actions button { display: inline-flex; align-items: center; gap: 6px; }
.limit-note { margin: -8px 0 0; font-size: 12px; }
.key-card.ended { background: var(--bg-panel); }
.key-card.ended .provider-tile { background: var(--bg-hover); color: var(--text-dim); }
.key-card .sub { font-size: 12px; }
.row-actions { margin-top: auto; }
.row-actions button { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; padding: 8px 12px; }
.revoke { color: var(--danger); }
.empty-icon { width: 56px; height: 56px; border-radius: 16px; display: grid; place-items: center; background: var(--accent-dim); color: var(--accent); }
.empty-card .primary { display: inline-flex; align-items: center; gap: 8px; margin-top: 4px; }
@media (max-width: 760px) {
  .section-actions { width: 100%; }
  .section-actions .primary { flex: 1; justify-content: center; }
  .account-card { padding: 17px; }
}
</style>
