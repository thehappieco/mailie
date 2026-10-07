<script setup lang="ts">
// One key of the workspace shown, for its owners and admins: what it is,
// what it holds on each of the workspace's mailboxes, and changing that, and
// its sends. Each mailbox's flags are ticked and then saved; what the caller
// may not give is shown as such beforehand (ui/apikeys.ts keyAccessRules):
// Read only on a mailbox they read themselves, Act where the key reads and
// can act, Send where it can send, nothing to a key carried over from before
// keys belonged to workspaces; anything may be taken away, and taking every
// flag takes the mailbox out of the key. A key that no longer works is shown
// as it was, and changes no more. Its sends are the record the server keeps
// of each, for 30 days: never who a message went to, its subject or its text.
// The server decides every change.
import { computed, onMounted, ref } from 'vue'
import type { Account, KeyFlags, KeySend } from '../api/types'
import { accounts } from '../state/accounts'
import { apiKeys, loadKeySends, setKeyMailbox } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { keysMaySend } from '../state/mcp'
import { session } from '../state/session'
import { personName } from '../state/team'
import { accessOf } from '../ui/access'
import { announce } from '../ui/announce'
import {
  keyAccessRules, keyFlagEditable, keyFlagHint, keyFlagLabel, keyFlagNames, keyFlagsOf, keyFlagsSummary, keyOriginNote, keyPerson, keyStanding,
  holdsAnyKeyFlag, sameKeyFlags, scopeActs, scopeLabel, sendStateLabel, standingLabel, toggleKeyFlag, type KeyFlag,
} from '../ui/apikeys'
import { describe } from '../ui/errors'
import { count, dayStamp, since, stamp } from '../ui/format'
import { t } from '../ui/i18n'
import { providerName } from '../ui/labels'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const props = defineProps<{ keyPrefix: string; returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ close: []; revoke: [] }>()

const key = computed(() => apiKeys.list.find(item => item.prefix === props.keyPrefix) ?? null)
const standing = computed(() => key.value ? keyStanding(key.value) : 'revoked')
const live = computed(() => standing.value === 'live')
const me = computed(() => session.user?.id ?? '')
const sendOffered = computed(keysMaySend)

interface Row { id: string; email: string; provider: Account['provider'] | ''; reads: boolean; held: KeyFlags; grantedBy: string; updatedAt: number }

/**
 * The workspace's mailboxes, and any the key holds that this page does not
 * list: a key that works may be given any of the first; one that no longer
 * works shows only what it held.
 */
const rows = computed<Row[]>(() => {
  const held = key.value?.mailboxes ?? []
  const listed = live.value && !key.value?.carried_over ? accounts.list : accounts.list.filter(item => held.some(entry => entry.account_id === item.id))
  const out: Row[] = listed.map(item => {
    const entry = held.find(h => h.account_id === item.id)
    return { id: item.id, email: item.email, provider: item.provider, reads: accessOf(item).read, held: keyFlagsOf(entry), grantedBy: entry?.granted_by ?? '', updatedAt: entry?.updated_at ?? 0 }
  })
  for (const entry of held) {
    if (out.some(row => row.id === entry.account_id)) continue
    out.push({ id: entry.account_id, email: t('A removed mailbox'), provider: '', reads: false, held: keyFlagsOf(entry), grantedBy: entry.granted_by ?? '', updatedAt: entry.updated_at })
  }
  // The mailboxes it holds something on first.
  return out.sort((a, b) => Number(holdsAnyKeyFlag(b.held)) - Number(holdsAnyKeyFlag(a.held)) || a.email.localeCompare(b.email))
})

const rulesFor = (row: Row) => keyAccessRules({
  key: { scope: key.value?.scope ?? 'read', sends: !!key.value?.sends, carried_over: key.value?.carried_over, origin: key.value?.origin, live: live.value },
  viewerReads: row.reads,
  keysSend: sendOffered.value,
})
/** The flags shown on a row: those the key's scope lets it hold, and any it holds. */
const flagsFor = (row: Row): KeyFlag[] => keyFlagNames.filter(flag => row.held[flag] || flag === 'read'
  || (flag === 'act' ? scopeActs(key.value?.scope ?? '') : key.value?.sends && sendOffered.value))

/** What the caller ticked on each mailbox, until it is saved or set back. */
const drafts = ref<Record<string, KeyFlags>>({})
const saving = ref('')
const problems = ref<Record<string, Failure>>({})
const shown = (row: Row): KeyFlags => drafts.value[row.id] ?? row.held
const changed = (row: Row): boolean => !sameKeyFlags(shown(row), row.held)
const editable = (row: Row, flag: KeyFlag) => !saving.value && keyFlagEditable(rulesFor(row), row.held, shown(row), flag)

function toggle(row: Row, flag: KeyFlag, event: Event) {
  const next = toggleKeyFlag(shown(row), flag, (event.target as HTMLInputElement).checked)
  delete problems.value[row.id]
  if (sameKeyFlags(next, row.held)) delete drafts.value[row.id]
  else drafts.value[row.id] = next
}
function setBack(row: Row) {
  delete drafts.value[row.id]
  delete problems.value[row.id]
}
async function save(row: Row) {
  const draft = drafts.value[row.id]
  if (!draft || saving.value || !key.value) return
  saving.value = row.id
  const problem = await setKeyMailbox(key.value.prefix, row.id, row.held, draft)
  saving.value = ''
  if (problem) { problems.value[row.id] = problem; return }
  delete drafts.value[row.id]
  announce(holdsAnyKeyFlag(draft) ? t('Saved: what the key holds on {email} changed.', { email: row.email }) : t('{email} was taken out of the key.', { email: row.email }))
}

/** Who gave it what it holds there, and when; the upgrade, for a key it moved. */
function givenBy(row: Row): string {
  if (!holdsAnyKeyFlag(row.held) || !row.updatedAt) return ''
  const date = dayStamp(row.updatedAt)
  if (row.grantedBy === me.value) return t('Given by you, {date}', { date })
  const who = keyPerson(row.grantedBy, me.value, personName)
  return who ? t('Given by {name}, {date}', { name: who, date }) : t('Given on {date}', { date })
}

/** Why Read cannot be given here: said once for every row. */
const readHint = computed(() => live.value && !key.value?.carried_over && rows.value.some(row => !row.reads && !row.held.read)
  ? t('You can give the key Read only on a mailbox you read yourself.') : '')

// Its sends: shown for a key that can send, or holds Send somewhere.
const sendsShown = computed(() => !!key.value && (key.value.scope === 'send' || key.value.mailboxes.some(item => item.send)))
const sends = ref<KeySend[] | null>(null)
const sendsProblem = ref<Failure | null>(null)
const sendsLoading = ref(false)
async function readSends() {
  if (!key.value || sendsLoading.value) return
  sendsLoading.value = true
  sendsProblem.value = null
  const answer = await loadKeySends(key.value.prefix)
  sendsLoading.value = false
  if ('failure' in answer) {
    if (answer.failure.code !== 'aborted') sendsProblem.value = answer.failure
    return
  }
  sends.value = answer.list
}
const emailOf = (accountID: string) => accounts.list.find(item => item.id === accountID)?.email ?? t('A removed mailbox')

const close = () => { if (!saving.value) emit('close') }
onMounted(() => { if (sendsShown.value) void readSends() })
</script>

<template>
  <ConsoleDialog v-if="key" :title="key.name" :subtitle="`${key.prefix}…`" wide :busy="!!saving" :return-focus="returnFocus" @close="close">
    <div class="key-sheet">
      <dl class="key-facts">
        <div><dt>{{ t('State') }}</dt><dd>{{ standingLabel(standing) }}</dd></div>
        <div><dt>{{ t('Access') }}</dt><dd>{{ scopeLabel(key.scope) }}</dd></div>
        <div v-if="keyPerson(key.created_by, me, personName)"><dt>{{ t('Created by') }}</dt><dd>{{ keyPerson(key.created_by, me, personName) }}</dd></div>
        <div><dt>{{ t('Created') }}</dt><dd>{{ dayStamp(key.created_at) }}</dd></div>
        <div v-if="standing === 'revoked'"><dt>{{ t('Revoked on') }}</dt><dd>{{ dayStamp(key.revoked_at) }}</dd></div>
        <div v-else><dt>{{ standing === 'expired' ? t('Expired on') : t('Expires') }}</dt><dd>{{ dayStamp(key.expires_at) }}</dd></div>
        <div><dt>{{ t('Last used') }}</dt><dd>{{ key.last_used_at ? since(key.last_used_at) : t('Never') }}</dd></div>
      </dl>
      <p v-if="keyOriginNote(key)" class="note">{{ keyOriginNote(key) }}</p>
      <p v-else-if="key.scope === 'send' && !key.sends" class="note">{{ t('This server’s API keys do not send email: this key sends nothing while that is so.') }}</p>

      <section class="sheet-section" :aria-label="t('Mailboxes')">
        <h3>{{ t('Mailboxes') }}</h3>
        <p v-if="!live" class="hint">{{ t('This key no longer works: what it held is shown as it was, and changes no more.') }}</p>
        <p v-else-if="readHint" class="hint">{{ readHint }}</p>
        <p v-if="!rows.length" class="dim">{{ t('This key holds nothing on any mailbox.') }}</p>
        <ul v-else class="key-rows">
          <li v-for="row in rows" :key="row.id" class="key-row" :data-account="row.id">
            <div class="who">
              <strong>{{ row.email }}</strong>
              <small>{{ row.provider ? providerName(row.provider) : '' }}<template v-if="row.provider && !row.reads"> · {{ t('You do not read it') }}</template><template v-if="givenBy(row)"> · {{ givenBy(row) }}</template></small>
            </div>
            <fieldset class="flags" :disabled="!live">
              <legend class="visually-hidden">{{ t('What the key holds on {email}', { email: row.email }) }}</legend>
              <label v-for="flag in flagsFor(row)" :key="flag" class="flag" :title="keyFlagHint(flag)">
                <input type="checkbox" :name="`${row.id}-${flag}`" :value="flag" :checked="shown(row)[flag]" :disabled="!live || !editable(row, flag)" @change="toggle(row, flag, $event)" />
                <span>{{ keyFlagLabel(flag) }}</span>
              </label>
            </fieldset>
            <p v-if="problems[row.id]" class="alert" role="alert">{{ describe(problems[row.id]!) }}</p>
            <div v-if="changed(row)" class="row-save">
              <span class="dim">{{ t('Now: {flags}', { flags: keyFlagsSummary(row.held) }) }}</span>
              <button class="ghost small" type="button" :disabled="!!saving" @click="setBack(row)">{{ t('Cancel') }}</button>
              <button class="primary small" type="button" :disabled="!!saving" @click="save(row)">{{ saving === row.id ? t('Saving…') : holdsAnyKeyFlag(shown(row)) ? t('Save access') : t('Take out of the key') }}</button>
            </div>
          </li>
        </ul>
        <dl class="flag-legend">
          <div v-for="flag in keyFlagNames" :key="flag"><dt>{{ keyFlagLabel(flag) }}</dt><dd>{{ keyFlagHint(flag) }}</dd></div>
        </dl>
      </section>

      <section v-if="sendsShown" class="sheet-section" :aria-label="t('Sends')">
        <div class="section-head">
          <h3>{{ t('Sends') }}</h3>
          <button class="ghost small" type="button" :disabled="sendsLoading" @click="readSends"><AppIcon name="refresh" :size="15" />{{ t('Refresh') }}</button>
        </div>
        <p class="hint">{{ t('What this server keeps of each message the key sent, for 30 days: never who it went to, its subject or its text.') }}</p>
        <div v-if="sendsProblem" class="alert with-action" role="alert"><span>{{ describe(sendsProblem) }}</span><button class="ghost small" type="button" @click="readSends">{{ t('Try again') }}</button></div>
        <p v-else-if="!sends" class="dim" role="status">{{ t('Reading the sends…') }}</p>
        <p v-else-if="!sends.length" class="dim">{{ t('This key sent nothing in the last 30 days.') }}</p>
        <ul v-else class="key-rows sends">
          <li v-for="item in sends" :key="item.idempotency_key + item.account_id" class="key-row send-row" :data-state="item.state">
            <div class="who"><strong>{{ sendStateLabel(item.state) }}</strong><small>{{ emailOf(item.account_id) }} · {{ stamp(item.sent_at || item.created_at) }} · {{ t('Recipients: {count}', { count: count(item.recipients) }) }}</small></div>
          </li>
        </ul>
      </section>

      <div class="dialog-actions">
        <button v-if="live" class="ghost revoke" type="button" :disabled="!!saving" @click="emit('revoke')">{{ t('Revoke…') }}</button>
        <button class="primary" type="button" :disabled="!!saving" @click="close">{{ t('Done') }}</button>
      </div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.key-sheet { display: grid; gap: 16px; }
.key-sheet p { margin: 0; }
.key-facts { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin: 0; }
.key-facts div { min-width: 0; }
.key-facts dt { font-size: 11px; color: var(--text-dim); }
.key-facts dd { margin: 4px 0 0; font-size: 13px; overflow-wrap: anywhere; }
.sheet-section { display: grid; gap: 10px; padding-top: 14px; border-top: 1px solid var(--line); }
.sheet-section h3 { margin: 0; font-size: 14px; }
.section-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.section-head button { display: inline-flex; align-items: center; gap: 6px; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.key-rows { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
.key-row { display: grid; gap: 8px; padding: 12px 14px; border-top: 1px solid var(--line); }
.key-row:first-child { border-top: 0; }
.who { min-width: 0; }
.who strong { display: block; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
.who small { display: block; margin-top: 2px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; }
.flags { display: flex; flex-wrap: wrap; gap: 8px; margin: 0; padding: 0; border: 0; min-width: 0; }
.flag { display: inline-flex; align-items: center; gap: 7px; min-height: 36px; padding: 6px 12px; border: 1px solid var(--line); border-radius: 999px; background: var(--bg-raised); font-size: 12.5px; cursor: pointer; }
.flag:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); font-weight: 600; }
.flag:has(input:disabled) { cursor: default; opacity: .65; }
.flag input { width: auto; min-height: 0; margin: 0; accent-color: var(--accent); }
.row-save { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; }
.row-save .dim { margin-right: auto; font-size: 12px; }
.flag-legend { display: grid; gap: 6px; margin: 0; font-size: 12px; line-height: 1.45; }
.flag-legend div { display: grid; grid-template-columns: 70px minmax(0, 1fr); gap: 10px; }
.flag-legend dt { font-weight: 600; }
.flag-legend dd { margin: 0; color: var(--text-dim); }
.send-row[data-state='unknown'] strong, .send-row[data-state='failed'] strong { color: var(--danger); }
.revoke { color: var(--danger); margin-right: auto; }
@media (max-width: 600px) {
  .key-facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .flag { flex: 1 1 calc(33% - 8px); justify-content: center; }
  .row-save button { flex: 1; }
}
</style>
