<script setup lang="ts">
// An invitation link opened by someone signed in: on a server whose teams are
// made here, it may invite them into a team, which they join by accepting it.
// It works only for the address it names, so for another address this says
// so, and signing out is how to use it (to create an account, or to sign in
// as that address). Joining gives access to no mailbox, and the dialog says
// that before anyone agrees. Set aside, the invitation is forgotten: it was
// never stored.
import { computed } from 'vue'
import { acceptInvitation, dropInvitation, invitation, invitationFits } from '../state/invitation'
import { session, signOut } from '../state/session'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import ConsoleDialog from './ConsoleDialog.vue'

const fits = computed(() => invitationFits(session.user?.email))
const invited = computed(() => invitation.pending?.email || session.user?.email || '')
/** Refused for this person: an invitation to create an account is used signed out. */
const refused = computed(() => invitation.problem?.code === 'not_authorized')

function close() { if (!invitation.busy) dropInvitation() }
async function join() { await acceptInvitation() }
async function leave() {
  if (invitation.busy) return
  // The invitation stays held: signed out, the page offers to use it.
  await signOut()
}
</script>

<template>
  <ConsoleDialog v-if="invitation.pending" :title="t('Join a team?')" :busy="invitation.busy" @close="close">
    <div class="form-stack invitation">
      <template v-if="fits">
        <p class="dim">{{ t('This invitation is for {email}. Accepting it makes you a member of a team on this server, with the role the invitation gives.', { email: invited }) }}</p>
        <p class="dim">{{ t('Joining gives you access to no mailbox: access to each one is given separately.') }}</p>
      </template>
      <p v-else class="dim">{{ t('This invitation is for {invited}, and you are signed in as {email}. It works only for its own address: sign out to use it.', { invited, email: session.user?.email ?? '' }) }}</p>
      <p v-if="invitation.problem" class="alert" role="alert">{{ describe(invitation.problem) }}</p>
      <div class="dialog-actions">
        <button class="ghost" type="button" :disabled="invitation.busy" @click="close">{{ t('Not now') }}</button>
        <button v-if="!fits || refused" :class="fits ? 'ghost' : 'primary'" type="button" :disabled="invitation.busy" @click="leave">{{ t('Sign out') }}</button>
        <button v-if="fits" class="primary" type="button" :disabled="invitation.busy" @click="join">{{ invitation.busy ? t('Joining…') : t('Join the team') }}</button>
      </div>
    </div>
  </ConsoleDialog>
</template>

<style scoped>
.invitation p { margin: 0; }
</style>
