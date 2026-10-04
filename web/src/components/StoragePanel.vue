<script setup lang="ts">
// What the person's mailboxes take up in this server's index: per mailbox,
// the messages indexed and their size on the mail server, the totals, and
// for an owner the size of the database on disk, which holds everyone's.
// Read when the section is first shown, on Refresh, when a mailbox is
// connected or removed, and when sync is turned on or off: the section stays
// mounted once shown, and turning sync off deletes the index.
import { computed, onMounted, watch } from 'vue'
import { accounts } from '../state/accounts'
import { loadStorage, storage } from '../state/storage'
import { consent } from '../state/sync'
import { describe } from '../ui/errors'
import { count, fileSize } from '../ui/format'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const usage = computed(() => storage.usage)

/**
 * What a mailbox's figures mean when it is not syncing now, as the accounts
 * list says. Sync off: nothing is indexed, and its zeros say so. Sync on for
 * an account that does not work (waiting for authorization, or refused): the
 * server stops syncing it and keeps its index, so its figures are what was
 * indexed before.
 */
function note(id: string): string {
  const account = accounts.list.find(item => item.id === id)
  if (!account) return ''
  if (!account.sync.enabled) return t('Not synced: nothing from this mailbox is indexed.')
  if (account.state !== 'active') return t('Sync is paused until the account works again. What was indexed is kept.')
  return ''
}

onMounted(() => { if (!storage.loaded && !storage.loading) void loadStorage() })
// Mailboxes connected or removed, and sync turned on or off (by the person,
// or for one mailbox by the server's operator): the figures were read before.
watch(() => [consent.consented, ...accounts.list.map(item => `${item.id}:${item.sync.enabled}`)].join(' '), (now, before) => {
  if (storage.loaded && now !== before) void loadStorage()
})
</script>

<template>
  <div class="console-section storage-section">
    <div class="console-overview" :class="{ owner: usage?.database_bytes !== undefined }">
      <article><span>{{ t('Mailboxes') }}</span><strong>{{ usage ? count(usage.mailboxes.length) : '—' }}</strong><small>{{ t('yours on this server') }}</small></article>
      <article><span>{{ t('Messages indexed') }}</span><strong>{{ usage ? count(usage.total.messages) : '—' }}</strong><small>{{ t('a copy in each folder') }}</small></article>
      <article><span>{{ t('Size on the mail servers') }}</span><strong>{{ usage ? fileSize(usage.total.bytes) : '—' }}</strong><small>{{ t('of the messages indexed') }}</small></article>
      <article v-if="usage?.database_bytes !== undefined"><span>{{ t('Database on disk') }}</span><strong>{{ fileSize(usage.database_bytes) }}</strong><small>{{ t('everyone’s, on this server') }}</small></article>
    </div>

    <div class="section-title">
      <h2>{{ t('By mailbox') }}</h2>
      <div class="section-actions">
        <button class="ghost small" type="button" :disabled="storage.loading" @click="loadStorage"><AppIcon name="refresh" :size="16" />{{ t('Refresh') }}</button>
      </div>
    </div>

    <div v-if="storage.failure" class="alert with-action" role="alert"><span>{{ describe(storage.failure) }}</span><button class="ghost small" type="button" @click="loadStorage">{{ t('Try again') }}</button></div>
    <p v-if="storage.loading && !storage.loaded" class="dim" role="status">{{ t('Reading what your mailboxes take up…') }}</p>

    <ul v-if="usage && usage.mailboxes.length" class="storage-list" :aria-label="t('By mailbox')" :aria-busy="storage.loading">
      <li v-for="mailbox in usage.mailboxes" :key="mailbox.account_id">
        <span class="provider-tile"><AppIcon name="mail" :size="19" /></span>
        <span class="storage-name grow"><strong>{{ mailbox.email }}</strong><small v-if="note(mailbox.account_id)">{{ note(mailbox.account_id) }}</small></span>
        <dl class="storage-figures">
          <div><dt>{{ t('Messages') }}</dt><dd>{{ count(mailbox.messages) }}</dd></div>
          <div><dt>{{ t('Size') }}</dt><dd>{{ fileSize(mailbox.bytes) }}</dd></div>
        </dl>
      </li>
    </ul>
    <p v-else-if="usage" class="dim">{{ t('No mailboxes are connected yet.') }}</p>

    <div class="storage-notes">
      <p class="hint">{{ t('A message in several folders, such as a Gmail message with several labels, counts once in each. Its size is the one the mail server reports; this server keeps only each message’s details, never bodies or attachments.') }}</p>
      <p v-if="usage?.database_bytes !== undefined" class="hint">{{ t('The database is the file this server keeps everything in, for every person: the index, saved sign-ins, keys and the log of changes. Only owners see its size.') }}</p>
    </div>
  </div>
</template>

<style scoped>
.console-section { display: grid; gap: 18px; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.console-overview { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px; }
.console-overview.owner { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.console-overview article { padding: 20px; background: var(--bg-raised); border: 1px solid var(--console-border); border-radius: 12px; min-width: 0; }
.console-overview span { color: var(--text-dim); font-size: 12px; }
.console-overview strong { display: block; font-size: 26px; font-weight: 600; letter-spacing: -.8px; margin-top: 10px; line-height: 1.2; overflow-wrap: anywhere; }
.console-overview small { display: block; color: var(--text-dim); font-size: 11px; margin-top: 6px; }
.section-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 6px; flex-wrap: wrap; }
.section-title h2 { font-size: 15px; margin: 0; font-weight: 600; }
.section-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.section-actions button { display: inline-flex; align-items: center; gap: 6px; }
.storage-list { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; }
.storage-list li { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; padding: 16px 18px; border: 1px solid var(--console-border); border-radius: 12px; background: var(--bg-panel); min-width: 0; }
.storage-name { min-width: 0; flex: 1 1 220px; }
.storage-name strong { display: block; font-size: 14px; font-weight: 600; overflow-wrap: anywhere; }
.storage-name small { display: block; margin-top: 4px; font-size: 12px; color: var(--text-dim); line-height: 1.45; }
.storage-figures { display: flex; gap: 28px; margin: 0; }
.storage-figures div { min-width: 70px; }
.storage-figures dt { font-size: 11px; color: var(--text-dim); }
.storage-figures dd { margin: 4px 0 0; font-size: 14px; font-weight: 600; font-variant-numeric: tabular-nums; }
.storage-notes { display: grid; gap: 8px; }
.storage-notes p { margin: 0; }
@media (max-width: 1020px) {
  .console-overview.owner { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .console-overview article { padding: 16px; }
}
@media (max-width: 760px) {
  .console-overview, .console-overview.owner { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; }
  .console-overview article { padding: 13px 10px; }
  .console-overview strong { font-size: 21px; margin-top: 6px; }
  .console-overview small { display: none; }
  .storage-list li { padding: 14px; }
  .storage-figures { width: 100%; padding-left: 50px; }
}
@media (max-width: 420px) {
  .storage-figures { padding-left: 0; }
}
</style>
