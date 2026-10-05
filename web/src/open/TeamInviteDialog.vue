<script setup lang="ts">
// Inviting someone into the team shown: an address and the role they get,
// then the link to send them, shown this once, with a button that copies it.
// The link lives in this component alone and is let go of when the dialog
// closes; nothing about it reaches storage, the address bar or history, and
// the list of invitations never has it. While it is shown, only Done and the
// close button close the dialog: an Escape pressed by habit would lose a link
// that cannot be shown again (another invitation can be made instead).
//
// Who the link works for is said before it is made: someone with an account
// here accepts it signed in; someone without one can create an account with it
// only when an owner of this server made it.
import { computed, nextTick, onBeforeUnmount, ref, shallowRef } from 'vue'
import { invitationLink } from '../api/workspaces'
import type { TeamInvite, WorkspaceRole } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import ConsoleDialog from '../components/ConsoleDialog.vue'
import type { Failure } from '../state/failure'
import { session } from '../state/session'
import { inviteMember } from '../state/team'
import { currentWorkspace } from '../state/workspaces'
import { inviteRoles, workspaceName, workspaceRoleLabel } from '../ui/access'
import { announce } from '../ui/announce'
import { copyText } from '../ui/clipboard'
import { describe } from '../ui/errors'
import { dayStamp } from '../ui/format'
import { t } from '../ui/i18n'

defineProps<{ returnFocus?: () => HTMLElement | null }>()
const emit = defineEmits<{ close: [] }>()

const team = computed(currentWorkspace)
const teamName = computed(() => workspaceName(team.value))
const roles = computed(() => inviteRoles(team.value?.role))
const email = ref('')
const role = ref<WorkspaceRole>('member')
const busy = ref(false)
const problem = ref<Failure | null>(null)
/** The invitation just made, its link included. Here and nowhere else, until the dialog closes. */
const made = shallowRef<TeamInvite | null>(null)
const link = computed(() => invitationLink(made.value?.url))
const copied = ref(false)
const copyFailed = ref(false)
const copyButton = ref<HTMLButtonElement>()
let copiedTimer: ReturnType<typeof setTimeout> | undefined
/** An owner of this server: a link they make also creates an account for an address that has none. */
const serverOwner = computed(() => session.user?.role === 'owner')

async function submit() {
  if (busy.value || made.value || !email.value.trim()) return
  busy.value = true
  problem.value = null
  const outcome = await inviteMember(email.value.trim(), roles.value.includes(role.value) ? role.value : 'member')
  busy.value = false
  if ('failure' in outcome) { problem.value = outcome.failure; return }
  made.value = outcome.invite
  announce(t('Invitation made. Copy the link now: it is shown only this once.'))
  await nextTick()
  copyButton.value?.focus({ preventScroll: true })
}

async function copy() {
  if (!link.value) return
  const ok = await copyText(link.value)
  if (!made.value) return
  copyFailed.value = !ok
  copied.value = ok
  clearTimeout(copiedTimer)
  if (!ok) return
  announce(t('Link copied.'))
  copiedTimer = setTimeout(() => { copied.value = false }, 4_000)
}

/** Lets go of the link: what the dialog drew goes with it. */
function forget() {
  made.value = null
  copied.value = false
  clearTimeout(copiedTimer)
}

function close() {
  if (busy.value) return
  forget()
  emit('close')
}

onBeforeUnmount(forget)
</script>

<template>
  <ConsoleDialog :title="made ? t('Send this invitation') : t('Invite someone to {team}', { team: teamName })" :busy="busy" :persistent="!!made" :return-focus="returnFocus" @close="close">
    <div v-if="made" class="form-stack invite-made">
      <p class="note invite-warning"><strong>{{ t('Copy this link now.') }}</strong> {{ t('It is shown only this once. It works only for {email}, once, until {date}.', { email: made.email, date: dayStamp(made.expires_at) }) }}</p>
      <!-- translate="no": a page translator would send the link to its service. -->
      <code class="invite-link notranslate" translate="no" spellcheck="false">{{ link }}</code>
      <div class="copy-actions">
        <button ref="copyButton" class="primary" type="button" @click="copy"><AppIcon :name="copied ? 'check' : 'copy'" :size="17" />{{ copied ? t('Copied') : t('Copy link') }}</button>
      </div>
      <p v-if="copyFailed" class="alert" role="alert">{{ t('Your browser did not let Mailie copy. Select the link and copy it yourself.') }}</p>
      <p class="dim">{{ t('{email} joins {team} as {role} by opening the link signed in to this server. Joining gives no access to any mailbox: access to each one is given separately.', { email: made.email, team: teamName, role: workspaceRoleLabel(made.role) }) }}</p>
      <div class="dialog-actions"><button class="ghost" type="button" @click="close">{{ t('Done') }}</button></div>
    </div>

    <form v-else class="form-stack invite-form" name="mailie-team-invite" autocomplete="off" novalidate @submit.prevent="submit">
      <label>{{ t('Email address') }}<input v-model="email" name="invite-email" type="email" maxlength="320" required autocomplete="off" autocapitalize="off" spellcheck="false" inputmode="email" :disabled="busy" placeholder="name@example.com" /></label>
      <fieldset v-if="roles.length > 1" class="role-choice" :disabled="busy">
        <legend>{{ t('Role in the team') }}</legend>
        <label v-for="item in roles" :key="item" class="option">
          <input v-model="role" type="radio" name="invite-role" :value="item" />
          <span><strong>{{ workspaceRoleLabel(item) }}</strong><small>{{ item === 'owner' ? t('Runs the team: its people and their roles, and gives Manage on its mailboxes.') : item === 'admin' ? t('Invites and administers members, and gives Manage on its mailboxes.') : t('Uses the mailboxes they are given access to.') }}</small></span>
        </label>
      </fieldset>
      <p v-else class="hint">{{ t('As an admin, you invite members. Owners invite admins and owners.') }}</p>
      <p class="hint">{{ serverOwner
        ? t('Someone who already has an account here joins by opening the link signed in. Someone without one can create an account with it, because you are an owner of this server.')
        : t('Someone who already has an account here joins by opening the link signed in. Someone without one needs an invitation to this server first, from one of its owners.') }}</p>
      <p v-if="problem" class="alert" role="alert">{{ describe(problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="busy" @click="close">{{ t('Cancel') }}</button>
        <button class="primary" type="submit" :disabled="busy || !email.trim()">{{ busy ? t('Inviting…') : t('Make the invitation') }}</button>
      </div>
    </form>
  </ConsoleDialog>
</template>

<style scoped>
.invite-form > label { font-size: 13px; }
.role-choice { display: grid; gap: 8px; margin: 0; padding: 0; border: 0; min-width: 0; }
.role-choice legend { padding: 0; margin-bottom: 8px; font-size: 13px; }
.invite-form .option { display: flex; align-items: flex-start; gap: 10px; padding: 11px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--bg-raised); cursor: pointer; }
.invite-form .option:has(input:checked) { border-color: var(--accent); background: var(--accent-dim); }
.invite-form .option input { width: auto; min-height: 0; margin: 2px 0 0; accent-color: var(--accent); flex: none; }
.invite-form .option span { display: grid; gap: 3px; min-width: 0; }
.invite-form .option strong { font-size: 13px; font-weight: 600; }
.invite-form .option small { font-size: 12px; line-height: 1.45; color: var(--text-dim); }
.invite-form .hint, .invite-form .alert, .invite-made .dim, .invite-made .alert { margin: 0; }
.invite-warning { margin: 0; line-height: 1.55; }
.invite-link { display: block; padding: 14px; border: 1px dashed var(--console-border); border-radius: 10px; background: var(--bg-input); color: var(--text); font-family: var(--mono); font-size: 13px; line-height: 1.5; overflow-wrap: anywhere; user-select: all; }
.copy-actions { display: flex; flex-wrap: wrap; gap: 10px; }
.copy-actions button { display: inline-flex; align-items: center; gap: 8px; }
@media (max-width: 600px) { .copy-actions button { flex: 1 1 100%; justify-content: center; } }
</style>
