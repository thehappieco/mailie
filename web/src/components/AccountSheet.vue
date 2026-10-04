<script setup lang="ts">
// One account, in detail: what the server knows about it, its sync, its
// folders (from the index once sync has listed them, else asked of the mail
// server live), and removing it. Removal is typed, never a confirm(): the
// person writes the address, which also makes them read it.
import { computed, onBeforeUnmount, onMounted, ref, useId, watch } from 'vue'
import type { Account } from '../api/types'
import { edition } from '../edition'
import { accounts, closeDetail, loadFolders, removeAccount } from '../state/accounts'
import { consent, reviewConsent, syncNow, syncRequests } from '../state/sync'
import { announce } from '../ui/announce'
import { describe, describeFolders } from '../ui/errors'
import { count, since, stamp } from '../ui/format'
import { t } from '../ui/i18n'
import { authKindLabel, folderRoleIcon, folderRoleLabel, needsAuthorization, providerIcon, providerName, sortFolders, stateDetail, syncTierLabel } from '../ui/labels'
import { folderBadge, fromIndex } from '../ui/sync'
import AppIcon from './AppIcon.vue'
import LiveRegion from './LiveRegion.vue'
import StatusChip from './StatusChip.vue'
import SyncStatus from './SyncStatus.vue'

// returnFocus: where focus goes on close when the control that opened the sheet is gone (its card, once removed).
const props = defineProps<{ account: Account; returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ authorize: [Account] }>()

const dialog = ref<HTMLDialogElement | null>(null)
const closeButton = ref<HTMLButtonElement | null>(null)
const titleID = useId()
const confirmID = useId()
const confirming = ref(false)
const typed = ref('')
let origin: HTMLElement | null = null
let disposed = false
let backdropPressed = false

const view = computed(() => accounts.folders[props.account.id])
const indexed = computed(() => fromIndex(view.value?.list ?? []))
const sync = computed(() => props.account.sync)
/** Syncing: active, and its owner turned sync on. */
const syncing = computed(() => props.account.state === 'active' && sync.value.enabled)
const request = computed(() => syncRequests[props.account.id])
const armed = computed(() => typed.value.trim().toLowerCase() === props.account.email.toLowerCase())
const folders = computed(() => sortFolders(view.value?.list ?? []))
/** The role badge, unless the folder's own name already says it. */
function roleBadge(folder: { role?: string; display_name: string; name: string }): string {
  const label = folderRoleLabel(folder.role)
  return label && label.toLowerCase() !== (folder.display_name || folder.name).toLowerCase() ? label : ''
}

watch(() => props.account.id, () => { confirming.value = false; typed.value = '' })

onMounted(() => {
  origin = document.activeElement instanceof HTMLElement ? document.activeElement : null
  dialog.value?.showModal()
  closeButton.value?.focus({ preventScroll: true })
})
onBeforeUnmount(() => {
  disposed = true
  dialog.value?.close()
  const target = origin?.isConnected ? origin : props.returnFocus?.() ?? document.getElementById('console-content')
  target?.focus({ preventScroll: true })
})

function closed() { if (!disposed) closeDetail() }
function dismiss() {
  if (accounts.removing) return
  if (dialog.value?.open) dialog.value.close()
  else closeDetail()
}
function pointerDown(event: PointerEvent) { backdropPressed = event.target === event.currentTarget }
function backdropClick(event: MouseEvent) {
  if (backdropPressed && event.target === event.currentTarget) dismiss()
  backdropPressed = false
}
function cancelRemoval() { confirming.value = false; typed.value = '' }
function showFolders() {
  announce(t('Listing folders…'))
  void loadFolders(props.account.id)
}
async function destroy() {
  if (!armed.value || accounts.removing) return
  await removeAccount(props.account.id)
}
function requestSync() { void syncNow(props.account.id) }
/** Back to the accounts page, where the consent card is, with the focus on it. */
function turnOn() {
  closeDetail()
  reviewConsent()
}
/** The sheet steps aside first, so the dialog that opens next returns focus to the card, not to a sheet that is gone. */
function authorize() {
  const account = props.account
  dismiss()
  emit('authorize', account)
}
</script>

<template>
  <dialog ref="dialog" class="sheet-backdrop" :aria-labelledby="titleID"
    @cancel.prevent="dismiss" @close="closed"
    @pointerdown="pointerDown" @click="backdropClick">
    <section class="sheet">
      <LiveRegion />
      <header class="sheet-head">
        <span class="provider-tile"><AppIcon :name="providerIcon(account.provider)" :size="21" /></span>
        <div class="grow"><h2 :id="titleID">{{ account.display_name || account.email }}</h2><p class="dim">{{ account.display_name ? account.email : providerName(account.provider) }}</p></div>
        <button ref="closeButton" class="icon-btn" type="button" :disabled="accounts.removing" :aria-label="t('Close')" @click="dismiss"><AppIcon name="close" :size="21" /></button>
      </header>
      <div class="sheet-scroll">
        <section class="sheet-section">
          <dl class="facts">
            <div><dt>{{ t('Status') }}</dt><dd><StatusChip :state="account.state" /><span class="fact-note">{{ stateDetail(account) }}</span></dd></div>
            <div><dt>{{ t('Address') }}</dt><dd>{{ account.email }}</dd></div>
            <div v-if="account.display_name"><dt>{{ t('Display name') }}</dt><dd>{{ account.display_name }}</dd></div>
            <div><dt>{{ t('Provider') }}</dt><dd>{{ providerName(account.provider) }}</dd></div>
            <div><dt>{{ t('Sign-in method') }}</dt><dd>{{ authKindLabel(account.auth_kind) }}</dd></div>
            <div><dt>{{ t('Connected on') }}</dt><dd>{{ stamp(account.created_at) }}</dd></div>
            <div v-if="account.provider === 'imap' || account.provider === 'icloud'"><dt>{{ t('Copy of sent mail') }}</dt><dd>{{ account.save_sent_copy ? t('Saved in the Sent folder') : t('Not saved by Mailie') }}</dd></div>
          </dl>
          <div v-if="needsAuthorization(account)" class="connection-action">
            <button class="primary" type="button" @click="authorize"><AppIcon name="shield" :size="17" />{{ t('Finish authorization') }}</button>
          </div>
        </section>

        <section class="sheet-section sync-section">
          <div class="section-head">
            <h3>{{ t('Sync') }}</h3>
            <button v-if="syncing" class="ghost small" type="button" :disabled="request?.busy" @click="requestSync">
              <AppIcon name="refresh" :size="16" />{{ request?.busy ? t('Requesting…') : t('Sync now') }}
            </button>
          </div>
          <template v-if="syncing">
            <SyncStatus :account="account" />
            <dl class="facts sync-facts">
              <div><dt>{{ t('Messages indexed') }}</dt><dd>{{ count(sync.messages) }}</dd></div>
              <div v-if="sync.state === 'initial' && sync.folders_total"><dt>{{ t('Folders finished') }}</dt><dd>{{ t('{done} of {total}', { done: count(sync.folders_synced), total: count(sync.folders_total) }) }}</dd></div>
              <div v-if="sync.last_synced_at"><dt>{{ t('Last sync') }}</dt><dd>{{ stamp(sync.last_synced_at) }}<span class="dim"> · {{ since(sync.last_synced_at) }}</span></dd></div>
              <div v-if="sync.tier"><dt>{{ t('Sync mode') }}</dt><dd>{{ syncTierLabel(sync.tier) }}</dd></div>
            </dl>
            <p v-if="request?.failure" class="alert" role="alert">{{ describe(request.failure) }}</p>
            <p v-else-if="request?.requested" class="hint">{{ t('Sync requested. The counts update as it runs.') }}</p>
            <p class="hint">{{ edition().copy.indexHint() }}</p>
          </template>
          <template v-else-if="!sync.enabled">
            <!-- Consented, and this mailbox still may not sync: nobody owns it (an owner sees those), and only the operator
                 switches it on. Not said while the accounts are being read again, as they are just after consent. -->
            <p v-if="consent.consented && !accounts.loading" class="dim">{{ t('Sync is off for this mailbox. It is not linked to your Mailie account, so the server’s administrator decides whether it syncs.') }}</p>
            <template v-else>
              <p class="dim">{{ t('Sync is off. Nothing from this mailbox is stored.') }}</p>
              <button v-if="consent.loaded && !consent.consented" class="ghost small" type="button" @click="turnOn">{{ t('Turn on sync…') }}</button>
            </template>
          </template>
          <p v-else class="dim">{{ t('Sync is on for this mailbox, and resumes once the account works again.') }}</p>
        </section>

        <section class="sheet-section">
          <div class="section-head">
            <h3>{{ t('Folders') }}</h3>
            <button class="ghost small" type="button" :disabled="view?.loading" @click="showFolders">
              <AppIcon :name="view?.loaded ? 'refresh' : 'folder'" :size="16" />{{ view?.loaded ? t('Refresh') : t('Show folders') }}
            </button>
          </div>
          <p v-if="!view?.loaded && !view?.loading" class="hint">{{ sync.enabled ? t('Once sync has listed the folders, the list and its counts come from Mailie’s index. Until then Mailie asks the mail server, which can take up to a minute.') : t('Mailie asks the mail server for the list directly, which can take up to a minute.') }}</p>
          <p v-if="view?.loading" class="dim loading-line"><span class="loading-spinner inline" aria-hidden="true" />{{ t('Listing folders…') }}</p>
          <p v-if="view?.failure" class="alert" role="alert">{{ describeFolders(view.failure, account) }}</p>
          <ul v-if="view?.loaded && folders.length" class="folder-list" :aria-label="t('Folders')">
            <li v-for="folder in folders" :key="folder.name">
              <AppIcon :name="folderRoleIcon(folder.role)" :size="18" />
              <span class="folder-name grow"><strong>{{ folder.display_name || folder.name }}</strong><small v-if="folder.display_name && folder.display_name !== folder.name" class="mono">{{ folder.name }}</small></span>
              <span v-if="roleBadge(folder)" class="pill">{{ roleBadge(folder) }}</span>
              <span v-if="folderBadge(folder, indexed)" class="pill" :class="folderBadge(folder, indexed)!.tone" :title="folderBadge(folder, indexed)!.title">{{ folderBadge(folder, indexed)!.text }}</span>
              <span v-if="folder.messages !== undefined" class="folder-count" :title="indexed ? t('Unread / indexed messages') : t('Unread / total messages')">{{ folder.unseen ? `${count(folder.unseen)} / ` : '' }}{{ count(folder.messages) }}</span>
            </li>
          </ul>
          <p v-else-if="view?.loaded" class="dim">{{ t('The server reported no folders.') }}</p>
          <p v-if="view?.loaded && indexed" class="hint folder-note">{{ t('From Mailie’s index: the counts are what Mailie has indexed, not what the mail server holds.') }}</p>
        </section>

        <section class="sheet-section danger-zone">
          <h3>{{ t('Remove account') }}</h3>
          <p>{{ t('Removes this mailbox from Mailie and deletes the saved sign-in and everything else Mailie keeps for it. Nothing is deleted at the provider.') }}</p>
          <p v-if="accounts.removeFailure" class="alert" role="alert">{{ describe(accounts.removeFailure) }}</p>
          <button v-if="!confirming" class="danger" type="button" @click="confirming = true"><AppIcon name="trash" :size="17" />{{ t('Remove account…') }}</button>
          <div v-else class="field">
            <label :for="confirmID">{{ t('Type {address} to confirm', { address: account.email }) }}</label>
            <input :id="confirmID" v-model="typed" autocomplete="off" :spellcheck="false" autocapitalize="off" inputmode="email" :placeholder="account.email" :disabled="accounts.removing" />
            <p class="hint">{{ t('This cannot be undone. To use the mailbox again, connect it again.') }}</p>
          </div>
        </section>
      </div>
      <footer v-if="confirming" class="sheet-footer">
        <button class="ghost" type="button" :disabled="accounts.removing" @click="cancelRemoval">{{ t('Cancel') }}</button>
        <button class="danger" type="button" :disabled="!armed || accounts.removing" @click="destroy">{{ accounts.removing ? t('Removing…') : t('Remove permanently') }}</button>
      </footer>
    </section>
  </dialog>
</template>

<style scoped>
.sheet-backdrop { position: fixed; inset: 0; z-index: 50; background: transparent; display: none; place-items: center; padding: 20px; margin: 0; width: 100%; height: 100dvh; max-width: none; max-height: none; border: 0; overflow: hidden; box-sizing: border-box; }
.sheet-backdrop[open] { display: grid; }
.sheet-backdrop::backdrop { background: #0009; backdrop-filter: blur(5px); }
.sheet { width: min(680px, 100%); max-height: min(90dvh, 950px); display: flex; flex-direction: column; min-height: 0; border: 1px solid var(--line); border-radius: 20px; background: var(--bg-panel); color: var(--text); box-shadow: 0 24px 90px #0005; overflow: hidden; }
.sheet-head { padding: 20px; display: flex; align-items: center; gap: 12px; border-bottom: 1px solid var(--line); }
.sheet-head h2 { margin: 0; font-size: 20px; overflow-wrap: anywhere; }
.sheet-head p { margin: 4px 0 0; overflow-wrap: anywhere; }
.sheet-scroll { min-height: 0; overflow-y: auto; overscroll-behavior: contain; padding: 0 22px 20px; }
.sheet-section { padding: 20px 0; border-bottom: 1px solid var(--line); }
.sheet-section:last-child { border: 0; }
.sheet-section h3 { margin: 0 0 10px; font-size: 15px; }
.sheet-section p { line-height: 1.55; font-size: 13px; }
.facts { margin: 0; display: grid; gap: 12px; font-size: 13px; }
.facts div { display: grid; grid-template-columns: 170px minmax(0, 1fr); gap: 12px; }
.facts dt { color: var(--text-dim); }
.facts dd { margin: 0; overflow-wrap: anywhere; }
.fact-note { display: block; margin-top: 6px; color: var(--text-dim); line-height: 1.5; }
.connection-action { margin-top: 18px; }
.section-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 8px; }
.section-head h3 { margin: 0; }
.loading-line { display: flex; align-items: center; gap: 8px; }
.folder-list { list-style: none; margin: 8px 0 0; padding: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
.folder-list li { display: flex; align-items: center; gap: 10px; padding: 11px 14px; border-top: 1px solid var(--line); font-size: 13px; color: var(--text-dim); }
.folder-list li:first-child { border-top: 0; }
.folder-name strong { display: block; font-weight: 550; color: var(--text); overflow-wrap: anywhere; }
.folder-name small { display: block; margin-top: 2px; color: var(--text-dim); overflow-wrap: anywhere; }
/* Planned, not done: a dashed outline rather than the green of something that works. */
.pill.planned { border-style: dashed; }
.pill.live { color: var(--ok-text); border-color: color-mix(in srgb, var(--ok) 45%, transparent); }
.pill.busy { color: var(--console-accent); border-color: color-mix(in srgb, var(--accent) 45%, transparent); }
.pill.bad { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 45%, transparent); }
.sync-facts { margin-top: 14px; }
.sync-section .hint, .folder-note { margin-top: 10px; }
.sync-section > .ghost { margin-top: 10px; }
.folder-count { font-variant-numeric: tabular-nums; font-size: 12px; white-space: nowrap; }
.danger-zone h3 { color: var(--danger); }
.danger-zone .field { margin: 14px 0 0; }
.danger-zone input { color: var(--text); background: var(--bg-input); border: 1px solid var(--line); border-radius: 10px; padding: 11px 12px; width: 100%; }
.sheet-footer { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 10px; padding: 16px 22px; border-top: 1px solid var(--line); background: var(--bg-raised); flex-shrink: 0; }
@media (max-width: 600px) {
  .sheet-backdrop { padding: 0; align-items: end; }
  .sheet { max-height: 96dvh; border-radius: 20px 20px 0 0; }
  .sheet-head { padding: 16px; }
  .sheet-scroll { padding: 0 16px 16px; }
  .facts div { grid-template-columns: 1fr; gap: 4px; }
  .folder-list li { flex-wrap: wrap; row-gap: 6px; }
  /* The name keeps a readable width; the badges and the count wrap below it instead of squeezing it letter by letter. */
  .folder-name { min-width: min(100%, 150px); }
  .sheet-footer { padding-bottom: max(16px, env(safe-area-inset-bottom)); }
  .sheet-footer button { flex: 1; }
  .danger-zone input { font-size: 16px; }
}
</style>
