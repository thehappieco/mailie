<script setup lang="ts">
// The API keys the person created, in every workspace (GET /v1/me/apikeys),
// in their account section: they answer for each one, which stops when they
// leave its workspace or their account here is disabled or deleted, and they
// may revoke any of them from here, a key carried over from before keys
// belonged to workspaces too, whole. Shown once they have created one; the
// workspace's own keys, and changing what one holds, are in its keys
// section, for its owners and admins.
import { computed, inject, onMounted, ref } from 'vue'
import type { WorkspaceKey } from '../api/types'
import { myKeys, loadMyKeys, revokeMyKey } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { workspaces } from '../state/workspaces'
import { workspaceName } from '../ui/access'
import { announce } from '../ui/announce'
import { keyOriginNote, keyStanding, scopeLabel, standingLabel, type KeyStanding } from '../ui/apikeys'
import { describe } from '../ui/errors'
import { count, dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import { accountNotice, noAccountNotice } from './accountNotice'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const notice = inject(accountNotice, noAccountNotice)
const tone: Record<KeyStanding, string> = { live: 'live', expired: 'off', revoked: 'bad' }
const rank: Record<KeyStanding, number> = { live: 0, expired: 1, revoked: 2 }
const ordered = computed(() => myKeys.list
  .map(key => ({ key, standing: keyStanding(key) }))
  .sort((a, b) => rank[a.standing] - rank[b.standing] || b.key.created_at - a.key.created_at))

/** Where a key is: its workspace, by name; a key carried over from before, in several. */
function whereIs(key: WorkspaceKey): string {
  if (key.carried_over || !key.workspace_id) {
    const many = new Set(key.mailboxes.map(item => item.workspace_id)).size
    return t('Several workspaces ({count})', { count: count(Math.max(many, 1)) })
  }
  return workspaceName(workspaces.list.find(item => item.id === key.workspace_id)) || t('A workspace you are no longer in')
}

const revoking = ref<WorkspaceKey | null>(null)
const problem = ref<Failure | null>(null)
function ask(key: WorkspaceKey) { problem.value = null; notice.clear(); revoking.value = key }
function closeRevoke() { if (!myKeys.revoking) revoking.value = null }
async function confirmRevoke() {
  const key = revoking.value
  if (!key || myKeys.revoking) return
  problem.value = await revokeMyKey(key.prefix)
  if (problem.value) return
  revoking.value = null
  const words = () => t('{name} was revoked. A tool using it can no longer reach any mailbox.', { name: key.name })
  notice.show(words)
  announce(words())
}

onMounted(() => { if (!myKeys.loaded && !myKeys.loading) void loadMyKeys() })
</script>

<template>
  <section v-if="myKeys.failure || myKeys.list.length" class="my-keys" :aria-label="t('API keys you created')">
    <div class="my-keys-head">
      <span class="summary-icon"><AppIcon name="key" :size="22" /></span>
      <span class="summary-text">
        <strong>{{ t('API keys you created') }}</strong>
        <small>{{ t('You answer for these keys, in every workspace: each one stops when it expires or is revoked, and when you leave its workspace or your account on this server is disabled or deleted. You can revoke any of them here.') }}</small>
      </span>
    </div>
    <div v-if="myKeys.failure" class="alert with-action" role="alert"><span>{{ describe(myKeys.failure) }}</span><button class="ghost small" type="button" @click="loadMyKeys">{{ t('Try again') }}</button></div>
    <ul v-else class="my-key-list" :aria-busy="myKeys.loading">
      <li v-for="{ key, standing } in ordered" :key="key.prefix" class="my-key" :data-key="key.prefix">
        <div class="who">
          <strong>{{ key.name }} <span class="mono" translate="no">{{ key.prefix }}…</span></strong>
          <small>{{ whereIs(key) }} · {{ scopeLabel(key.scope) }} · {{ t('Mailboxes: {count}', { count: count(key.mailboxes.length) }) }} · {{ standing === 'revoked' ? t('Revoked on {date}', { date: dayStamp(key.revoked_at) }) : standing === 'expired' ? t('Expired on {date}', { date: dayStamp(key.expires_at) }) : t('Expires on {date}', { date: dayStamp(key.expires_at) }) }}</small>
          <small v-if="keyOriginNote(key, 'mine')" class="origin">{{ keyOriginNote(key, 'mine') }}</small>
        </div>
        <span class="status-chip" :class="tone[standing]"><i aria-hidden="true" />{{ standingLabel(standing) }}</span>
        <button v-if="standing === 'live'" class="ghost small revoke" type="button" aria-haspopup="dialog" :disabled="!!myKeys.revoking" @click="ask(key)">{{ t('Revoke…') }}</button>
      </li>
    </ul>

    <ConsoleDialog v-if="revoking" :title="t('Revoke this key?')" :busy="myKeys.revoking === revoking.prefix" @close="closeRevoke">
      <div class="form-stack">
        <p class="dim">{{ t('{name} stops working at once, in every mailbox it holds: a tool using it loses access at its next request. This cannot be undone.', { name: revoking.name }) }}</p>
        <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
        <div class="dialog-actions">
          <button class="ghost" type="button" :disabled="!!myKeys.revoking" @click="closeRevoke">{{ t('Cancel') }}</button>
          <button class="danger" type="button" :disabled="!!myKeys.revoking" @click="confirmRevoke">{{ myKeys.revoking ? t('Revoking…') : t('Revoke key') }}</button>
        </div>
      </div>
    </ConsoleDialog>
  </section>
</template>

<style scoped>
.my-keys { display: grid; gap: 12px; padding: 18px; border: 1px solid var(--console-border); border-radius: 14px; background: var(--bg-panel); }
.my-keys-head { display: flex; align-items: flex-start; gap: 14px; }
.summary-icon { display: grid; place-items: center; flex: none; width: 42px; height: 42px; border-radius: 12px; color: var(--accent); background: var(--accent-dim); }
.summary-text { flex: 1; min-width: 0; }
.summary-text strong { display: block; font-size: 14px; color: var(--text); }
.summary-text small { display: block; font-size: 12px; color: var(--text-dim); margin-top: 5px; line-height: 1.5; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.my-key-list { list-style: none; margin: 0; padding: 0; border: 1px solid var(--console-border); border-radius: 12px; overflow: hidden; }
.my-key { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; padding: 12px 14px; border-top: 1px solid var(--console-border); }
.my-key:first-child { border-top: 0; }
.who { flex: 1 1 260px; min-width: 0; }
.who strong { display: block; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
.who strong .mono { font-weight: 400; font-size: 12px; color: var(--text-dim); }
.who small { display: block; margin-top: 3px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; line-height: 1.45; }
.revoke { color: var(--danger); }
.form-stack p { margin: 0; }
@media (max-width: 600px) {
  .my-keys { padding: 15px; }
  .my-key .revoke { flex: 1; }
}
</style>
