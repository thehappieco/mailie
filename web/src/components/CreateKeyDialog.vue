<script setup lang="ts">
// Creating an API key of the workspace shown: a name, what the key may do at
// most (its scope), what it holds on each of the workspace's mailboxes, and
// how long it lives, then the edition's text of what that authorizes above
// the one button that agrees to it. What it may be given is said before
// anyone tries (ui/apikeys.ts keyAccessRules): Read only on a mailbox the
// person creating it reads; Act where it reads, with a scope that acts; Send
// with the send scope, which is offered only where the server says its keys
// may send. A key given no mailbox reaches nothing until one is given to it.
//
// Then the key itself, shown this once, with a button that copies it and,
// where the edition shows how to reach the MCP server and the server answers
// at /mcp, one that copies the Claude Code command with the key in it: to the
// clipboard only, never drawn. The secret lives in this component alone and
// is let go of when the dialog closes; nothing about it reaches storage, the
// address bar or history. While it is shown, only Done and the close button
// close the dialog: an Escape pressed by habit, or a tap beside it, would
// lose a key that cannot be shown again.
import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue'
import { keyLifetimes, type CreatedKey, type KeyFlags, type KeyLifetime, type KeyScope } from '../api/types'
import { edition } from '../edition'
import { accounts } from '../state/accounts'
import { atKeyLimit, createKey } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { keysMaySend, mcpOffered } from '../state/mcp'
import { currentWorkspace } from '../state/workspaces'
import { accessOf, workspaceName } from '../ui/access'
import { announce } from '../ui/announce'
import {
  DEFAULT_LIFETIME, NO_KEY_ACCESS, claudeCommand, holdsAnyKeyFlag, keyAccessRules, keyFlagEditable, keyFlagLabel, keyFlagNames, keyFlagsSummary,
  lifetimeLabel, mcpEndpoint, scopeActs, scopeLabel, toggleKeyFlag, type KeyFlag,
} from '../ui/apikeys'
import { copyText } from '../ui/clipboard'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'
import { providerName } from '../ui/labels'
import AppIcon from './AppIcon.vue'
import ConsoleDialog from './ConsoleDialog.vue'

defineProps<{ returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ close: [] }>()

const name = ref('')
const scope = ref<KeyScope>('read')
/** What the key is given on each mailbox, by its id; a mailbox with nothing ticked is not given. */
const given = ref<Record<string, KeyFlags>>({})
const lifetime = ref<KeyLifetime>(DEFAULT_LIFETIME)
const busy = ref(false)
const problem = ref<Failure | null>(null)
/** The key just made, its secret included. Here and nowhere else, until the dialog closes. */
const created = shallowRef<CreatedKey | null>(null)
const copied = ref<'' | 'key' | 'command'>('')
const copyFailed = ref(false)
const copyButton = ref<HTMLButtonElement>()
let copiedTimer: ReturnType<typeof setTimeout> | undefined

const workspace = computed(currentWorkspace)
/** The team's name, which the key terms name; empty in a personal workspace. */
const team = computed(() => workspace.value?.kind === 'team' ? workspaceName(workspace.value) : '')
const sendOffered = computed(keysMaySend)
const scopes = computed<KeyScope[]>(() => sendOffered.value ? ['read', 'write', 'send'] : ['read', 'write'])
// The server said meanwhile that its keys do not send: the choice goes with it.
watch(sendOffered, offered => { if (!offered && scope.value === 'send') scope.value = 'write' })
/** The workspace's mailboxes: a key is given only its own workspace's. */
const mailboxes = computed(() => accounts.list.filter(item => !workspace.value || item.workspace_id === undefined || item.workspace_id === workspace.value.id))
const rulesFor = (accountID: string) => keyAccessRules({
  key: { scope: scope.value, sends: scope.value === 'send', live: true },
  viewerReads: accessOf(accounts.list.find(item => item.id === accountID)).read,
  keysSend: sendOffered.value,
})
const flagsOf = (accountID: string): KeyFlags => given.value[accountID] ?? NO_KEY_ACCESS
const editable = (accountID: string, flag: KeyFlag) => !busy.value && keyFlagEditable(rulesFor(accountID), NO_KEY_ACCESS, flagsOf(accountID), flag)
/** The flags the scope chosen lets a key hold: Act with one that acts, Send with send. */
const flagsShown = computed(() => keyFlagNames.filter(flag => flag === 'read' || (flag === 'act' ? scopeActs(scope.value) : scope.value === 'send')))

function toggle(accountID: string, flag: KeyFlag, event: Event) {
  const next = toggleKeyFlag(flagsOf(accountID), flag, (event.target as HTMLInputElement).checked)
  // Act ticks Read only where the person may give it; otherwise it stays as it was.
  if (next.read && !flagsOf(accountID).read && !rulesFor(accountID).canAdd.read) return
  given.value = { ...given.value, [accountID]: next }
}
// A narrower scope takes the flags it does not cover with it.
watch(scope, value => {
  const next: Record<string, KeyFlags> = {}
  for (const [id, flags] of Object.entries(given.value)) next[id] = { read: flags.read, act: flags.act && scopeActs(value), send: flags.send && value === 'send' }
  given.value = next
})
/** Chosen from the list as it is now: a mailbox removed meanwhile is not asked for. */
const chosen = computed(() => mailboxes.value.map(item => ({ account_id: item.id, ...flagsOf(item.id) })).filter(holdsAnyKeyFlag))
const readsNone = computed(() => mailboxes.value.length > 0 && !mailboxes.value.some(item => accessOf(item).read))

const expiresOn = computed(() => dayStamp(Math.floor(Date.now() / 1000) + lifetime.value * 86_400))
const termsChanged = computed(() => problem.value?.code === 'terms_changed')
const { keyTerms } = edition()
const mcp = computed(mcpOffered)
/** The mailboxes a key holds something on, by address, with what it holds. */
const createdMailboxes = computed(() => (created.value?.mailboxes ?? []).map(item => `${accounts.list.find(account => account.id === item.account_id)?.email ?? t('A removed mailbox')} (${keyFlagsSummary(item)})`))

async function submit() {
  if (busy.value || created.value || !name.value.trim()) return
  busy.value = true
  problem.value = null
  const outcome = await createKey({ name: name.value.trim(), scope: scope.value, mailboxes: chosen.value, lifetime: lifetime.value })
  busy.value = false
  if ('failure' in outcome) {
    problem.value = outcome.failure
    return
  }
  created.value = outcome.created
  announce(t('Key created. Copy it now: it is shown only this once.'))
  await nextTick()
  // The form was scrolled down to its button; the key and its warning start at the top.
  copyButton.value?.closest('.dialog-content')?.scrollTo({ top: 0 })
  copyButton.value?.focus({ preventScroll: true })
}

async function copy(what: 'key' | 'command') {
  const key = created.value?.key
  if (!key || (what === 'command' && !mcp.value)) return
  const ok = await copyText(what === 'key' ? key : claudeCommand(mcpEndpoint(location.origin), key))
  // The dialog may have closed while the browser was copying.
  if (!created.value) return
  copyFailed.value = !ok
  copied.value = ok ? what : ''
  clearTimeout(copiedTimer)
  if (!ok) return
  announce(what === 'key' ? t('Key copied.') : t('Command copied.'))
  copiedTimer = setTimeout(() => { copied.value = '' }, 4_000)
}

/** Lets go of the secret: what the dialog drew goes with it. */
function forget() {
  created.value = null
  copied.value = ''
  clearTimeout(copiedTimer)
}

function close() {
  if (busy.value) return
  forget()
  emit('close')
}

function reload() { location.reload() }

onBeforeUnmount(forget)
</script>

<template>
  <ConsoleDialog :title="created ? t('Your new API key') : t('Create an API key')" :busy="busy" :persistent="!!created" wide :return-focus="returnFocus" @close="close">
    <div v-if="created" class="form-stack key-created">
      <p class="note key-warning"><strong>{{ t('Copy this key now.') }}</strong> {{ t('Mailie shows it only this once and cannot show it again. If you lose it, revoke it and create another.') }}</p>
      <!-- translate="no": a page translator would send the key to its service. -->
      <code class="key-secret notranslate" translate="no" spellcheck="false">{{ created.key }}</code>
      <div class="copy-actions">
        <button ref="copyButton" class="primary" type="button" @click="copy('key')"><AppIcon :name="copied === 'key' ? 'check' : 'copy'" :size="17" />{{ copied === 'key' ? t('Copied') : t('Copy key') }}</button>
        <button v-if="mcp" class="ghost" type="button" aria-describedby="key-command-note" @click="copy('command')"><AppIcon :name="copied === 'command' ? 'check' : 'copy'" :size="17" />{{ copied === 'command' ? t('Command copied') : t('Copy the Claude Code command with this key') }}</button>
      </div>
      <p v-if="mcp" id="key-command-note" class="dim">{{ t('The command connects Claude Code to Mailie with this key. It is copied, not shown: paste it in a terminal.') }}</p>
      <p v-if="copyFailed" class="alert" role="alert">{{ t('Your browser did not let Mailie copy. Select the key and copy it yourself.') }}</p>
      <dl class="key-facts">
        <div><dt>{{ t('Name') }}</dt><dd>{{ created.name }}</dd></div>
        <div><dt>{{ t('Access') }}</dt><dd>{{ scopeLabel(created.scope) }}</dd></div>
        <div><dt>{{ t('Mailboxes') }}</dt><dd>{{ createdMailboxes.length ? createdMailboxes.join(', ') : t('None yet') }}</dd></div>
        <div><dt>{{ t('Expires') }}</dt><dd>{{ dayStamp(created.expires_at) }}</dd></div>
      </dl>
      <div class="dialog-actions"><button class="ghost" type="button" @click="close">{{ t('Done') }}</button></div>
    </div>

    <form v-else class="form-stack key-form" name="mailie-new-key" autocomplete="off" novalidate @submit.prevent="submit">
      <label>{{ t('Name') }}<input v-model="name" name="key-name" maxlength="120" required autocomplete="off" spellcheck="false" :placeholder="t('For example, Claude Code on my laptop')" :disabled="busy" /></label>
      <fieldset class="key-choice" :disabled="busy">
        <legend>{{ t('What the key may do') }}</legend>
        <label v-for="option in scopes" :key="option" class="option"><input v-model="scope" type="radio" name="scope" :value="option" /><span><strong>{{ scopeLabel(option) }}</strong>
          <small v-if="option === 'read'">{{ t('Search and read the messages of the mailboxes it is given Read on.') }}</small>
          <small v-else-if="option === 'write'">{{ t('Also mark as read or unread, star, archive and move messages, on the mailboxes it is given Act on.') }}</small>
          <small v-else>{{ t('Also send email from the mailboxes it is given Send on, each message confirmed by the tool.') }}</small>
        </span></label>
      </fieldset>
      <fieldset class="key-choice" :disabled="busy">
        <legend>{{ t('Mailboxes') }}</legend>
        <p v-if="!mailboxes.length" class="hint">{{ t('This workspace has no mailbox yet. The key reaches nothing until one is connected and given to it.') }}</p>
        <ul v-else class="mailbox-flags" :aria-label="t('Mailboxes this key reaches')">
          <li v-for="account in mailboxes" :key="account.id" class="mailbox-flag-row">
            <div class="who"><strong>{{ account.email }}</strong><small>{{ providerName(account.provider) }}<template v-if="!accessOf(account).read"> · {{ t('You do not read it') }}</template></small></div>
            <div class="flags" role="group" :aria-label="t('What the key holds on {email}', { email: account.email })">
              <label v-for="flag in flagsShown" :key="flag" class="flag">
                <input type="checkbox" :name="`${account.id}-${flag}`" :value="flag" :checked="flagsOf(account.id)[flag]" :disabled="!editable(account.id, flag)" @change="toggle(account.id, flag, $event)" />
                <span>{{ keyFlagLabel(flag) }}</span>
              </label>
            </div>
          </li>
        </ul>
        <p v-if="readsNone" class="hint">{{ t('You read none of these mailboxes, so you cannot give the key Read on any: an owner or an admin who reads one can give it later.') }}</p>
        <p v-else-if="mailboxes.length" class="hint">{{ t('Read is given only on a mailbox you read yourself. With no mailbox ticked, the key reaches nothing until one is given to it.') }}</p>
      </fieldset>
      <fieldset class="key-choice" :disabled="busy">
        <legend>{{ t('Expires after') }}</legend>
        <div class="lifetimes">
          <label v-for="days in keyLifetimes" :key="days" class="lifetime"><input v-model="lifetime" type="radio" name="lifetime" :value="days" /><span>{{ lifetimeLabel(days) }}</span></label>
        </div>
        <p class="hint">{{ t('The key stops working on {date}.', { date: expiresOn }) }}</p>
      </fieldset>
      <!-- Not once the server asks about another text: this one is no longer what a key would be created under. -->
      <section v-if="!termsChanged" class="key-terms" aria-labelledby="key-terms-title">
        <h3 id="key-terms-title">{{ t('What creating this key allows') }}</h3>
        <component :is="keyTerms.component" :write="scopeActs(scope)" :send="scope === 'send'" :team="team" />
      </section>
      <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
        <button v-if="termsChanged" class="primary" type="button" @click="reload">{{ t('Reload page') }}</button>
        <button v-else class="primary" type="submit" :disabled="busy || atKeyLimit() || !name.trim()">{{ busy ? t('Creating…') : t('Create key') }}</button>
      </div>
    </form>
  </ConsoleDialog>
</template>

<style scoped>
/* Scoped under .key-form: the dialog lays every label of a .form-stack out as a grid. */
.key-form > label { font-size: 13px; }
.key-choice { display: grid; gap: 8px; margin: 0; padding: 0; border: 0; min-width: 0; }
.key-choice legend { padding: 0; margin-bottom: 8px; font-size: 13px; }
.key-form .option { display: flex; align-items: flex-start; gap: 10px; padding: 11px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--bg-raised); cursor: pointer; }
.key-form .option:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); }
.key-form .option input { width: auto; min-height: 0; margin: 2px 0 0; accent-color: var(--accent); flex: none; }
.key-form .option span { display: grid; gap: 3px; min-width: 0; }
.key-form .option strong { font-size: 13px; font-weight: 600; }
.key-form .option small { font-size: 12px; line-height: 1.45; color: var(--text-dim); }
.mailbox-flags { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
.mailbox-flag-row { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 14px; padding: 10px 12px; border-top: 1px solid var(--line); }
.mailbox-flag-row:first-child { border-top: 0; }
.mailbox-flag-row .who { flex: 1 1 200px; min-width: 0; }
.mailbox-flag-row .who strong { display: block; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
.mailbox-flag-row .who small { display: block; margin-top: 2px; font-size: 12px; color: var(--text-dim); }
.flags { display: flex; flex-wrap: wrap; gap: 8px; }
.key-form .flag { display: inline-flex; align-items: center; gap: 7px; min-height: 36px; padding: 6px 12px; border: 1px solid var(--line); border-radius: 999px; background: var(--bg-raised); font-size: 12.5px; cursor: pointer; }
.key-form .flag:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); font-weight: 600; }
.key-form .flag:has(input:disabled) { cursor: default; opacity: .65; }
.key-form .flag input { width: auto; min-height: 0; margin: 0; accent-color: var(--accent); }
.lifetimes { display: flex; flex-wrap: wrap; gap: 8px; }
.key-form .lifetime { display: inline-flex; align-items: center; gap: 8px; min-height: 40px; padding: 8px 14px; border: 1px solid var(--line); border-radius: 999px; background: var(--bg-raised); font-size: 13px; cursor: pointer; }
.key-form .lifetime:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); font-weight: 600; }
.key-form .lifetime input { width: auto; min-height: 0; margin: 0; accent-color: var(--accent); flex: none; }
.key-choice .hint, .key-choice .alert { margin: 0; }
.key-terms { display: grid; gap: 10px; padding: 16px; border: 1px solid var(--line); border-left: 3px solid var(--accent); border-radius: 12px; background: var(--bg-raised); }
.key-terms h3 { margin: 0; font-size: 14px; }
.key-warning { margin: 0; line-height: 1.55; }
.key-secret { display: block; padding: 14px; border: 1px dashed var(--console-border); border-radius: 10px; background: var(--bg-input); color: var(--text); font-family: var(--mono); font-size: 13px; line-height: 1.5; overflow-wrap: anywhere; user-select: all; }
.copy-actions { display: flex; flex-wrap: wrap; gap: 10px; }
.copy-actions button { display: inline-flex; align-items: center; gap: 8px; }
.key-created .dim, .key-created .alert { margin: 0; }
.key-facts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; margin: 0; padding-top: 14px; border-top: 1px solid var(--line); }
.key-facts div { min-width: 0; }
.key-facts dt { font-size: 11px; color: var(--text-dim); }
.key-facts dd { margin: 4px 0 0; font-size: 13px; overflow-wrap: anywhere; }
@media (max-width: 600px) {
  .copy-actions button { flex: 1 1 100%; justify-content: center; }
  .key-facts { grid-template-columns: 1fr; }
  .key-form .flag { flex: 1 1 calc(33% - 8px); justify-content: center; }
}
</style>
