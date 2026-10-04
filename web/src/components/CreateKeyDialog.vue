<script setup lang="ts">
// Creating an API key: a name, what the key may do, which mailboxes it
// reaches and how long it lives, then the edition's text of what that
// authorizes above the one button that agrees to it. Read and act is offered
// only while the person allows actions on their messages. Then the key
// itself, shown this once, with a button that copies it and, where the
// edition shows how to reach the MCP server and the server answers at /mcp,
// one that copies the Claude Code command with the key in it: to the
// clipboard only, never drawn. The secret
// lives in this component alone and is let go of when the dialog closes;
// nothing about it reaches storage, the address bar or history. While it is
// shown, only Done and the close button close the dialog: an Escape pressed
// by habit, or a tap beside it, would lose a key that cannot be shown again.
import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue'
import { keyLifetimes, type CreatedKey, type KeyLifetime, type KeyScope } from '../api/types'
import { edition } from '../edition'
import { accounts } from '../state/accounts'
import { actionsAllowed } from '../state/actionsConsent'
import { atKeyLimit, createKey } from '../state/apikeys'
import type { Failure } from '../state/failure'
import { mcpOffered } from '../state/mcp'
import { announce } from '../ui/announce'
import { DEFAULT_LIFETIME, claudeCommand, keyMailboxes, lifetimeLabel, mcpEndpoint, scopeLabel } from '../ui/apikeys'
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
/** Every mailbox of the person's, the ones connected later too; else only those chosen. */
const every = ref(true)
const chosen = ref<string[]>([])
const lifetime = ref<KeyLifetime>(DEFAULT_LIFETIME)
const busy = ref(false)
const problem = ref<Failure | null>(null)
const noneChosen = ref(false)
/** The key just made, its secret included. Here and nowhere else, until the dialog closes. */
const created = shallowRef<CreatedKey | null>(null)
const copied = ref<'' | 'key' | 'command'>('')
const copyFailed = ref(false)
const copyButton = ref<HTMLButtonElement>()
let copiedTimer: ReturnType<typeof setTimeout> | undefined

// Actions turned off (here or elsewhere) while the dialog is open take the choice with them.
const writeOffered = computed(actionsAllowed)
watch(writeOffered, offered => { if (!offered) scope.value = 'read' })
const expiresOn = computed(() => dayStamp(Math.floor(Date.now() / 1000) + lifetime.value * 86_400))
const termsChanged = computed(() => problem.value?.code === 'terms_changed')
const { keyTerms, copy: words } = edition()
const mcp = computed(mcpOffered)
const accountSection = computed(() => words.accountSection())

async function submit() {
  if (busy.value || created.value || !name.value.trim()) return
  // Chosen from the list as it is now: a mailbox removed meanwhile is not asked for.
  const accountIDs = every.value ? null : accounts.list.filter(item => chosen.value.includes(item.id)).map(item => item.id)
  noneChosen.value = accountIDs !== null && accountIDs.length === 0
  if (noneChosen.value) return
  busy.value = true
  problem.value = null
  const outcome = await createKey({ name: name.value.trim(), scope: writeOffered.value ? scope.value : 'read', accountIDs, lifetime: lifetime.value })
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
        <div><dt>{{ t('Mailboxes') }}</dt><dd>{{ keyMailboxes(created, accounts.list).join(', ') }}</dd></div>
        <div><dt>{{ t('Expires') }}</dt><dd>{{ dayStamp(created.expires_at) }}</dd></div>
      </dl>
      <div class="dialog-actions"><button class="ghost" type="button" @click="close">{{ t('Done') }}</button></div>
    </div>

    <form v-else class="form-stack key-form" name="mailie-new-key" autocomplete="off" novalidate @submit.prevent="submit">
      <label>{{ t('Name') }}<input v-model="name" name="key-name" maxlength="120" required autocomplete="off" spellcheck="false" :placeholder="t('For example, Claude Code on my laptop')" :disabled="busy" /></label>
      <fieldset class="key-choice" :disabled="busy">
        <legend>{{ t('Access') }}</legend>
        <label class="option"><input v-model="scope" type="radio" name="scope" value="read" /><span><strong>{{ t('Read') }}</strong><small>{{ t('Search and read messages.') }}</small></span></label>
        <label v-if="writeOffered" class="option"><input v-model="scope" type="radio" name="scope" value="write" /><span><strong>{{ t('Read and act') }}</strong><small>{{ t('Also mark as read or unread, star, archive, move and move to the trash, while actions on your messages are allowed in {section}.', { section: accountSection }) }}</small></span></label>
        <p v-else class="hint">{{ t('To create a key that can also change messages, first allow actions on your messages in {section}.', { section: accountSection }) }}</p>
      </fieldset>
      <fieldset class="key-choice" :disabled="busy">
        <legend>{{ t('Mailboxes') }}</legend>
        <label class="option"><input v-model="every" type="radio" name="mailboxes" :value="true" /><span><strong>{{ t('All my mailboxes') }}</strong><small>{{ t('Including the ones you connect later.') }}</small></span></label>
        <label v-if="accounts.list.length" class="option"><input v-model="every" type="radio" name="mailboxes" :value="false" /><span><strong>{{ t('Only the ones I choose') }}</strong></span></label>
        <div v-if="!every" class="mailbox-choices" role="group" :aria-label="t('Mailboxes this key reaches')">
          <label v-for="account in accounts.list" :key="account.id" class="choice"><input v-model="chosen" type="checkbox" name="account" :value="account.id" /><span class="grow">{{ account.email }}</span><small>{{ providerName(account.provider) }}</small></label>
        </div>
        <p v-if="noneChosen && !every" class="alert" role="alert">{{ t('Choose at least one mailbox.') }}</p>
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
        <component :is="keyTerms.component" :write="writeOffered && scope === 'write'" />
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
.mailbox-choices { display: grid; gap: 2px; padding: 2px 0 0 12px; }
.key-form .mailbox-choices .choice { display: flex; align-items: center; gap: 10px; min-height: 38px; font-size: 13px; cursor: pointer; }
.key-form .mailbox-choices .choice input { flex: none; margin: 0; }
.mailbox-choices .choice span { min-width: 0; overflow-wrap: anywhere; }
.mailbox-choices small { color: var(--text-dim); font-size: 12px; white-space: nowrap; }
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
  .mailbox-choices { padding-left: 4px; }
  .key-form .mailbox-choices .choice { flex-wrap: wrap; row-gap: 0; }
}
</style>
