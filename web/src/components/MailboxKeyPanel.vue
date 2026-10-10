<script setup lang="ts">
// A mailbox's key, in its sheet (docs/key-scheme.md sections 9 and 12.12 to
// 12.14), for the person who holds Read on it, owner, admin or member alike:
//
// - waiting for the key (Read without it): in a team, who can hand it over,
//   anyone who reads the mailbox; for a personal mailbox, a new key, which
//   its person writes (after a reset, the only way back to it);
// - reading it: the members who hold Read and wait for the key, each handed
//   it on request, sealed in this browser from the person's own; and, for a
//   personal mailbox whose own key does not open here, a new key;
// - reading a mailbox without a key: writing its first key, sealed to them
//   and to everyone else who reads it with an account key (the console
//   writes it by itself right after a sign-in).
//
// Each write asks for a step-up first (the password, or the edition's own)
// unless the last was within ten minutes (state/stepUp.ts). A team mailbox
// is never given a new key: one nobody can read keeps the card's "nobody can
// read" note, and is only removed. Nothing is drawn when there is nothing to
// say.
import { computed, onMounted, watch } from 'vue'
import { enrolled, type Account, type KeyRecipient } from '../api/types'
import { checkOwnGrant, loadMailboxKey, mailboxKeys, supplyKey, writeFirstKey, writeNewKey } from '../state/mailboxKeys'
import { session } from '../state/session'
import { stepUpHint } from '../state/stepUp'
import { workspaces } from '../state/workspaces'
import { accessOf, waitsForKey } from '../ui/access'
import { announce } from '../ui/announce'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ account: Account }>()

const view = computed(() => mailboxKeys.views[props.account.id])
const reads = computed(() => accessOf(props.account).read)
const waiting = computed(() => waitsForKey(props.account))
const keyed = computed(() => props.account.mailbox_key !== undefined)
/** A team's mailbox: never given a new key. A server without workspaces has the person's own only. */
const team = computed(() => workspaces.list.find(item => item.id === props.account.workspace_id)?.kind === 'team')
const withKey = computed(() => enrolled(session.user))
const busy = computed(() => view.value?.busy ?? '')
const suppliers = computed(() => view.value?.state?.suppliers ?? [])
const waitingPeople = computed(() => view.value?.state?.waiting ?? [])
const keylessReaders = computed(() => view.value?.state?.keyless_readers ?? [])
/** The person's own key of their personal mailbox does not open in this browser: a new key mends it. */
const unopened = computed(() => !team.value && reads.value && keyed.value && view.value?.opens === 'no')

/** Whether a write is offered here, each of which may ask for a step-up first. */
const offersWrite = computed(() => withKey.value && ((waiting.value && !team.value) || unopened.value
  || (reads.value && keyed.value && waitingPeople.value.length > 0 && session.keyed) || (reads.value && !keyed.value)))

/** Whether there is anything to say or do here. */
const shown = computed(() => {
  if (waiting.value) return true
  if (!withKey.value || !reads.value) return false
  if (!keyed.value) return true
  return unopened.value || waitingPeople.value.length > 0 || !!view.value?.failure || !!view.value?.problem
})

/**
 * Reads the key when the sheet opens, and again only when what the card says
 * of it changes: another mailbox, another epoch, Read or the key gained or
 * lost. The card is replaced whole whenever the mailbox syncs, and that
 * alone reads nothing again, nor opens the person's own key once more. A
 * personal mailbox's own key is tried here too. The console's own writes read
 * the key again themselves (state/mailboxKeys.ts).
 */
async function read() {
  if (!waiting.value && !reads.value) return
  await loadMailboxKey(props.account.id)
  if (!team.value && keyed.value && reads.value) await checkOwnGrant(props.account.id)
}
onMounted(read)
watch([() => props.account.id, () => props.account.mailbox_key?.epoch, reads, waiting], read)

const name = (person: { name: string; email: string }) => person.name || person.email

async function firstKey() {
  if (await writeFirstKey(props.account.id)) announce(t('The mailbox’s key was created.'))
}
async function newKey() {
  if (await writeNewKey(props.account)) announce(t('The mailbox has a new key.'))
}
async function supply(person: KeyRecipient) {
  if (await supplyKey(props.account.id, person)) announce(t('{name} was handed the key.', { name: name(person) }))
}
</script>

<template>
  <section v-if="shown" class="sheet-section key-section">
    <h3>{{ t('Mailbox key') }}</h3>
    <div v-if="view?.failure" class="alert with-action" role="alert">
      <span>{{ describe(view.failure) }}</span>
      <button class="ghost small" type="button" :disabled="view.loading" @click="read">{{ t('Try again') }}</button>
    </div>

    <!-- Read without the key. -->
    <template v-if="waiting">
      <p v-if="!withKey" class="dim">{{ describe({ op: 'load-mailbox-key', code: 'not_enrolled' }) }}</p>
      <template v-else-if="team">
        <p class="dim">{{ t('You hold Read on this mailbox, but not its key yet, so you do not read it.') }}</p>
        <template v-if="view?.loaded && suppliers.length">
          <p class="dim">{{ t('Anyone who reads it can hand the key to you:') }}</p>
          <ul class="key-people" :aria-label="t('Who can hand you the key')">
            <li v-for="person in suppliers" :key="person.user_id"><span class="who"><strong>{{ name(person) }}</strong><small v-if="person.name">{{ person.email }}</small></span></li>
          </ul>
        </template>
        <p v-else-if="view?.loaded" class="dim">{{ t('Nobody reads this mailbox now, so nobody can hand you its key.') }}</p>
      </template>
      <template v-else>
        <p class="dim">{{ t('This mailbox is waiting for its key: your account key changed after its key was created, so you no longer hold it. Create a new key to read it again.') }}</p>
        <button class="primary small" type="button" :disabled="!!busy" @click="newKey"><AppIcon name="key" :size="16" />{{ busy === 'new' ? t('Creating…') : t('Create a new key') }}</button>
      </template>
    </template>

    <!-- Read, with the key. -->
    <template v-else-if="keyed">
      <template v-if="unopened">
        <p class="dim">{{ t('Your key for this mailbox does not open in this browser. Create a new key to keep reading it.') }}</p>
        <button class="primary small" type="button" :disabled="!!busy" @click="newKey"><AppIcon name="key" :size="16" />{{ busy === 'new' ? t('Creating…') : t('Create a new key') }}</button>
      </template>
      <template v-if="waitingPeople.length">
        <p class="dim">{{ t('These people hold Read on this mailbox but not its key yet, so they do not read it. Handing it over seals the key to them in this browser.') }}</p>
        <p v-if="!session.keyed" class="hint">{{ describe({ op: 'supply-key', code: 'no_account_key' }) }}</p>
        <ul class="key-people" :aria-label="t('Waiting for the key')">
          <li v-for="person in waitingPeople" :key="person.user_id" :data-user="person.user_id">
            <span class="who"><strong>{{ name(person) }}</strong><small v-if="person.name">{{ person.email }}</small></span>
            <button v-if="session.keyed" class="ghost small" type="button" :disabled="!!busy" @click="supply(person)">{{ busy === person.user_id ? t('Handing over…') : t('Hand over the key') }}</button>
          </li>
        </ul>
      </template>
    </template>

    <!-- Read by the flag alone: no key yet. -->
    <template v-else>
      <p class="dim">{{ keylessReaders.length
        ? t('This mailbox has no key yet, so Read alone reads it. Creating its key seals it to you and to the people below, who also read it and have an account key; from then on, reading it takes Read and the key.')
        : t('This mailbox has no key yet, so Read alone reads it. Creating its key seals it to you; from then on, reading it takes Read and the key.') }}</p>
      <ul v-if="keylessReaders.length" class="key-people" :aria-label="t('Who else gets the key')">
        <li v-for="person in keylessReaders" :key="person.user_id"><span class="who"><strong>{{ name(person) }}</strong><small v-if="person.name">{{ person.email }}</small></span></li>
      </ul>
      <button class="primary small" type="button" :disabled="!!busy || !view?.loaded" @click="firstKey"><AppIcon name="key" :size="16" />{{ busy === 'first' ? t('Creating…') : t('Create its key') }}</button>
    </template>

    <p v-if="offersWrite" class="hint">{{ stepUpHint() }}</p>
    <p v-if="view?.problem" class="alert" role="alert">{{ describe(view.problem) }}</p>
  </section>
</template>

<style scoped>
.key-section { padding: 20px 0; border-bottom: 1px solid var(--line); display: grid; gap: 10px; }
.key-section h3 { margin: 0; font-size: 15px; }
.key-section p { margin: 0; line-height: 1.55; font-size: 13px; }
.key-section > button { justify-self: start; display: inline-flex; align-items: center; gap: 6px; }
.with-action { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.key-people { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
.key-people li { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 10px 14px; border-top: 1px solid var(--line); }
.key-people li:first-child { border-top: 0; }
.who { min-width: 0; }
.who strong { display: block; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
.who small { display: block; margin-top: 2px; font-size: 12px; color: var(--text-dim); overflow-wrap: anywhere; }
@media (max-width: 600px) {
  .key-people li { flex-wrap: wrap; }
  .key-people li button { flex: 1; }
}
</style>
