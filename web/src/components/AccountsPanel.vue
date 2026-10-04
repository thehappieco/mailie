<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { Account } from '../api/types'
import { edition } from '../edition'
import { accounts, dismissNotice, loadAccounts, openDetail } from '../state/accounts'
import { consent, loadConsent, needsConsent } from '../state/sync'
import { describe } from '../ui/errors'
import { count, since } from '../ui/format'
import { t } from '../ui/i18n'
import { needsAuthorization, providerIcon, providerName, stateDetail, stateTone } from '../ui/labels'
import { noticeText } from '../ui/notices'
import AccountSheet from './AccountSheet.vue'
import AddAccountDialog from './AddAccountDialog.vue'
import AppIcon from './AppIcon.vue'
import StatusChip from './StatusChip.vue'
import SyncConsentCard from './SyncConsentCard.vue'
import SyncStatus from './SyncStatus.vue'

// The dialog is either adding a new mailbox or finishing an existing one's
// authorization; one instance at a time, so two flows never poll at once.
const dialog = ref<{ resume: Account | null } | null>(null)
// Active means the server accepted the sign-in; whether mail is syncing is
// each card's to say, since that also depends on the person's consent.
const active = computed(() => accounts.list.filter(item => item.state === 'active').length)
const attention = computed(() => accounts.list.filter(item => item.state !== 'active').length)
const detail = computed(() => accounts.list.find(item => item.id === accounts.detailID) ?? null)
/** Asked once there is a mailbox to sync, and until the person answers (or says "not now"). */
const askConsent = computed(() => accounts.loaded && accounts.list.length > 0 && needsConsent() && !consent.dismissed)
/** Syncing: active, and its owner turned sync on. */
const syncing = (account: Account) => account.state === 'active' && account.sync.enabled
/**
 * Refresh reads the consent again too: it may have changed in another tab or
 * on another device, and a first read that failed left this page without its
 * question.
 */
function refresh() {
  void loadAccounts()
  if (!consent.loading && !consent.busy) void loadConsent()
}
/** Each notice is a new element, so a failure's role="alert" is inserted with its text and read. */
const noticeKey = ref(0)
watch(() => accounts.notice, () => { noticeKey.value++ })
/**
 * When a dialog closes and the control that opened it is gone (the empty
 * state's button once the first account exists, a card's Details button once
 * its account is removed), focus returns to the section's heading.
 */
const heading = ref<HTMLElement | null>(null)
const headingTarget = () => heading.value

function add() { dismissNotice(); dialog.value = { resume: null } }
function authorize(account: Account) { dismissNotice(); dialog.value = { resume: account } }
</script>

<template>
  <div class="console-section">
    <!-- Good news is said by the live region (state/accounts.ts); a failure is an alert. -->
    <div v-if="accounts.notice" :key="noticeKey" :class="accounts.notice.kind === 'failed' ? 'alert notice' : 'success notice'" :role="accounts.notice.kind === 'failed' ? 'alert' : undefined">
      <AppIcon :name="accounts.notice.kind === 'failed' ? 'alert' : 'check'" :size="18" />
      <span class="grow">{{ noticeText(accounts.notice) }}</span>
      <button class="icon-btn notice-close" type="button" :aria-label="t('Dismiss')" @click="dismissNotice"><AppIcon name="close" :size="17" /></button>
    </div>

    <SyncConsentCard v-if="askConsent" />

    <div class="console-overview">
      <article><span>{{ t('Email accounts') }}</span><strong>{{ accounts.loaded ? count(accounts.list.length) : '—' }}</strong><small>{{ t('added to Mailie') }}</small></article>
      <article><span>{{ t('Active accounts') }}</span><strong>{{ accounts.loaded ? count(active) : '—' }}</strong><small>{{ t('sign-in accepted') }}</small></article>
      <article><span>{{ t('Need attention') }}</span><strong>{{ accounts.loaded ? count(attention) : '—' }}<i v-if="attention > 0" class="summary-status warn" aria-hidden="true" /></strong><small>{{ t('waiting for you') }}</small></article>
    </div>

    <div class="section-title">
      <h2 ref="heading" tabindex="-1">{{ t('Your mailboxes') }}</h2>
      <div class="section-actions">
        <button class="ghost small" type="button" :disabled="accounts.loading" @click="refresh"><AppIcon name="refresh" :size="16" />{{ t('Refresh') }}</button>
        <button v-if="!accounts.loaded || accounts.list.length" class="primary small" type="button" @click="add"><AppIcon name="plus" :size="16" />{{ t('Connect an email account') }}</button>
      </div>
    </div>

    <div v-if="accounts.failure" class="alert with-action" role="alert"><span>{{ describe(accounts.failure) }}</span><button class="ghost small" type="button" @click="loadAccounts">{{ t('Try again') }}</button></div>
    <p v-if="accounts.loading && !accounts.loaded" class="dim" role="status">{{ t('Loading your email accounts…') }}</p>

    <section class="cards" :aria-label="t('Email accounts')" :aria-busy="accounts.loading">
      <article v-for="account in accounts.list" :key="account.id" class="account-card">
        <!-- Two rows: an address is long, and beside a status it would break mid-word. -->
        <div class="head">
          <span class="provider-tile"><AppIcon :name="providerIcon(account.provider)" :size="21" /></span>
          <StatusChip :state="account.state" />
        </div>
        <div class="identity"><div class="name">{{ account.display_name || account.email }}</div><div class="sub">{{ account.display_name ? account.email : providerName(account.provider) }}</div></div>
        <dl class="counts">
          <div><dt>{{ t('Provider') }}</dt><dd>{{ providerName(account.provider) }}</dd></div>
          <div><dt>{{ t('Sign-in') }}</dt><dd>{{ account.auth_kind === 'oauth2' ? 'OAuth' : t('Password') }}</dd></div>
          <!-- What the index holds: only for a mailbox that syncs, or there is nothing true to put here. -->
          <div v-if="syncing(account)"><dt>{{ t('Messages indexed') }}</dt><dd>{{ count(account.sync.messages) }}</dd></div>
          <div v-if="syncing(account) && account.sync.last_synced_at"><dt>{{ t('Last sync') }}</dt><dd>{{ since(account.sync.last_synced_at) }}</dd></div>
        </dl>
        <SyncStatus v-if="syncing(account)" :account="account" />
        <p v-else class="account-note" :class="stateTone(account.state)"><AppIcon name="info" :size="15" />{{ stateDetail(account) }}</p>
        <div class="row-actions">
          <button class="ghost" type="button" @click="openDetail(account.id)">{{ t('Details') }}</button>
          <button v-if="needsAuthorization(account)" class="ghost authorize" type="button" @click="authorize(account)"><AppIcon name="shield" :size="16" />{{ t('Finish authorization') }}</button>
        </div>
      </article>
      <div v-if="accounts.loaded && !accounts.list.length" class="empty-card">
        <span class="empty-icon"><AppIcon name="mail" :size="30" /></span>
        <h3>{{ t('Connect your first email account') }}</h3>
        <p>{{ edition().copy.mailboxesIntro() }}</p>
        <button class="primary" type="button" @click="add"><AppIcon name="plus" :size="18" />{{ t('Connect an email account') }}</button>
      </div>
    </section>

    <AddAccountDialog v-if="dialog" :resume="dialog.resume" :return-focus="headingTarget" @close="dialog = null" />
    <AccountSheet v-if="detail" :account="detail" :return-focus="headingTarget" @authorize="authorize" />
  </div>
</template>

<style scoped>
.console-section { display: grid; gap: 18px; }
.notice { display: flex; align-items: center; gap: 10px; margin: 0; }
.notice > span { overflow-wrap: anywhere; }
.notice-close { min-width: 32px; min-height: 32px; padding: 4px; margin: -6px -6px -6px 0; color: inherit; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin: 0; }
.console-overview { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px; }
.console-overview article { padding: 20px; background: var(--bg-raised); border: 1px solid var(--console-border); border-radius: 12px; min-width: 0; }
.console-overview span { color: var(--text-dim); font-size: 12px; }
.console-overview strong { display: flex; align-items: center; gap: 10px; font-size: 30px; font-weight: 600; letter-spacing: -1px; margin-top: 10px; line-height: 1.2; }
.console-overview small { display: block; color: var(--text-dim); font-size: 11px; margin-top: 6px; }
.summary-status { width: 8px; height: 8px; border-radius: 50%; background: var(--text-faint); flex-shrink: 0; }
.summary-status.warn { background: var(--warn); }
.section-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 6px; flex-wrap: wrap; }
.section-title h2 { font-size: 15px; margin: 0; font-weight: 600; }
/* A place to return focus to, not a control: no ring. */
.section-title h2:focus { outline: none; }
.section-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.section-actions button { display: inline-flex; align-items: center; gap: 6px; }
.account-note { display: flex; align-items: flex-start; gap: 7px; color: var(--text-dim); font-size: 12px; line-height: 1.5; margin: 0; }
.account-note .app-icon { margin-top: 1px; }
.account-note.warn { color: var(--warn-text); }
.account-note.bad { color: var(--danger); }
.row-actions { margin-top: auto; }
.row-actions button { font-size: 12px; padding: 8px 12px; }
.authorize { color: var(--console-accent); }
.empty-icon { width: 56px; height: 56px; border-radius: 16px; display: grid; place-items: center; background: var(--accent-dim); color: var(--accent); }
.empty-card .primary { margin-top: 4px; }
@media (max-width: 1020px) {
  .console-overview article { padding: 16px; }
}
@media (max-width: 760px) {
  .console-overview { gap: 8px; }
  .console-overview article { padding: 13px 10px; }
  .console-overview span { font-size: 11px; display: block; line-height: 1.35; min-height: 30px; }
  .console-overview strong { font-size: 23px; gap: 6px; margin-top: 6px; }
  .console-overview small { display: none; }
  .account-card { padding: 17px; }
  .section-actions { width: 100%; }
  .section-actions .primary { flex: 1; }
}
</style>
