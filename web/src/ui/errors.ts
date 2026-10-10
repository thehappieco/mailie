// What a failure says to a person.
//
// The server's own `message` is never shown: it is English, it can carry
// upstream detail, and it is written for logs. The console says what the
// failure means for what the person was doing, in their language. A failure
// that left an account failing for a reason the console knows says that
// reason; a code with nothing specific to say for an operation falls through
// to its general text. An edition says what its own operations' failures
// mean (addDescriber); the core's are here.

import type { Account } from '../api/types'
import { MIN_PASSWORD } from '../api/auth'
import { edition } from '../edition'
import { MAX_LIVE_KEYS } from './apikeys'
import { count } from './format'
import { t } from './i18n'
import { reasonText } from './reasons'
import type { Failure } from '../state/failure'

/** Words for a failure, or undefined to leave it to the core. */
export type Describer = (failure: Failure) => string | undefined

const describers: Describer[] = []

/** Adds an edition's words for failures: asked before the core's, so it may also say a core one its own way. */
export function addDescriber(describer: Describer): void {
  describers.push(describer)
}

function specific(failure: Failure): string | undefined {
  const { op, code } = failure
  switch (op) {
    case 'sign-in':
      if (code === 'unauthorized') return t('The email or password is incorrect.')
      if (code === 'bad_request') return t('Enter a valid email address and your password.')
      if (code === 'rate_limited') return t('Too many sign-in attempts. Wait a minute and try again.')
      // The upgrade's ticket, or a derivation the server's default moved under: both are a new start.
      if (code === 'not_authorized' || code === 'conflict') return t('This took too long, or something changed on the server. Sign in again.')
      return undefined
    case 'sign-up':
      // Also an invitation into a team made by someone who may not bring people onto the server: it is accepted signed in,
      // where teams are made here. Elsewhere an invitation only ever creates an account.
      if (code === 'not_authorized') {
        return edition().teams
          ? t('This invitation cannot create an account for this address. It may have expired or been used, or it invites you to a team: sign in to the account you already have, and open the link again.')
          : t('This invitation cannot create an account for this address. It may have expired or been used: ask for a new one.')
      }
      if (code === 'bad_request') return t('Check your name and the address, and try again.')
      if (code === 'conflict') return t('An account with this email already exists. Sign in instead.')
      if (code === 'rate_limited') return t('Too many sign-in attempts. Wait a minute and try again.')
      return undefined
    case 'recover':
      if (code === 'unauthorized') return t('The email or recovery code is incorrect.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      if (code === 'not_authorized' || code === 'conflict') return t('This took too long, or something changed on the server. Start again.')
      return undefined
    case 'reset':
      if (code === 'not_authorized') return t('This reset link is not valid: it may have expired or been used, or be meant for another address. Ask the administrator of this server for a new one.')
      if (code === 'conflict') return t('This reset would leave a team mailbox that nobody can read. Ask the administrator of this server to give someone else Read on it first.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      return undefined
    case 'step-up':
      if (code === 'not_authorized') return t('The password is incorrect.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      return undefined
    case 'recovery-code':
      if (code === 'not_authorized') return t('The password is incorrect.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      return undefined
    case 'restore':
      return t('Could not restore your session right now. Check your connection and try again.')
    case 'sign-out':
      return t('Could not sign out of the other browsers. Check your connection and try again.')
    case 'profile':
      if (code === 'bad_request') return t('Use a name of up to 120 characters.')
      return undefined
    case 'password':
      if (code === 'not_authorized') return t('The current password is incorrect.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      if (code === 'conflict') return t('This took too long, or something changed on the server. Start again.')
      return undefined
    case 'load-accounts':
      if (code === 'unavailable' || code === 'internal') return t('Could not load your email accounts. Try again in a moment.')
      return undefined
    case 'load-providers':
      return t('Could not check which providers this server supports. You can still try to connect.')
    case 'add-account':
      if (code === 'conflict') return t('This address is already connected in this workspace. If it is waiting for authorization, finish it from its card.')
      if (code === 'bad_request') return t('The server did not accept these details. Check the address and try again.')
      if (code === 'not_authorized') return t('Your account is not allowed to connect mailboxes here. In a team, only its owners and admins connect them.')
      if (code === 'not_found') return t('This workspace is no longer yours. Choose another and try again.')
      if (code === 'flow_unsupported') return t('This server offered a way to sign in that this page cannot finish.')
      return undefined
    case 'test-login':
      if (code === 'bad_request') return t('The mail server refused the sign-in or could not be reached. Check the address, the password and the server names.')
      if (code === 'conflict') return t('This address is already connected in this workspace.')
      if (code === 'not_authorized' || code === 'not_found') return specific({ op: 'add-account', code })
      return undefined
    case 'test-login-icloud':
      // The usual cause by far: the Apple Account password, which iCloud refuses over IMAP.
      if (code === 'bad_request') return t('iCloud refused the sign-in. Check the address and that you used an app-specific password, not your Apple Account password.')
      return specific({ op: 'test-login', code })
    case 'start-auth':
      if (code === 'not_authorized') return t('Only someone who manages this mailbox can authorize it again.')
      if (code === 'bad_request') return t('This server cannot start the sign-in for this provider. It may not be configured for it.')
      if (code === 'conflict') return t('This account cannot be authorized right now.')
      if (code === 'not_found') return t('This account no longer exists.')
      if (code === 'flow_unsupported') return t('This server offered a way to sign in that this page cannot finish.')
      return undefined
    case 'wait-auth':
      if (code === 'not_found') return t('The account was removed while waiting for authorization.')
      if (code === 'flow_expired') return t('The sign-in window closed before it was completed. Try again.')
      if (code === 'flow_failed') return t('The provider did not complete the authorization. Try again.')
      return undefined
    case 'complete-auth':
      if (code === 'bad_request') return t('The provider did not grant access. You can try again from the account.')
      if (code === 'not_found') return t('This authorization expired or was started by another sign-in. Start again.')
      if (code === 'return_invalid') return t('The provider sent back an incomplete answer. Start again.')
      if (code === 'return_expired') return t('This authorization took too long and expired. Start again.')
      return undefined
    case 'remove-account':
      if (code === 'conflict') return t('This account cannot be removed right now. Try again in a moment.')
      if (code === 'not_authorized') return t('Only the owners and admins of a team remove its mailboxes.')
      // The id the removal repeats did not match: nothing was removed.
      if (code === 'bad_request') return t('The server removed nothing: the mailbox to remove was not confirmed. Close this and try again.')
      return undefined
    case 'folders':
      if (code === 'not_authorized') return t('You do not have read access to this mailbox. It comes only from an owner or an admin of the team who reads it.')
      if (code === 'conflict') return t('This account needs to be authorized again before its folders can be listed.')
      if (code === 'unavailable' || code === 'internal') return t('Could not list the folders. The mail server may be slow or unreachable.')
      return undefined
    case 'sync-consent':
      return t('Could not check whether sync is on. Try again in a moment.')
    case 'grant-sync':
      // The console asked about an older text than the server's.
      if (code === 'bad_request') return edition().sync.changedWhileOpen()
      return undefined
    case 'withdraw-sync':
      // An answer that never came says nothing about whether the deletion
      // happened; asking again is safe, because withdrawing twice deletes
      // whatever the first one missed.
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that sync was turned off. Try again: doing it twice is safe.')
      return undefined
    case 'sync-now':
      if (code === 'conflict') return t('Mailie cannot sync this mailbox right now. Try again in a moment.')
      if (code === 'not_authorized') return specific({ op: 'folders', code })
      return undefined
    case 'actions-consent':
      return t('Could not check whether actions are allowed. Try again in a moment.')
    case 'grant-actions':
      // The console asked about an older text than the server's.
      if (code === 'bad_request') return edition().actions.changedWhileOpen()
      return undefined
    case 'withdraw-actions':
      // Turning actions off twice is harmless, so asking again is the answer to not knowing.
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that actions were turned off. Try again: doing it twice is safe.')
      return undefined
    case 'load-keys':
      if (code === 'unavailable' || code === 'internal') return t('Could not load the API keys. Try again in a moment.')
      if (code === 'not_authorized') return t('Only the owners and admins of a team see its API keys.')
      return undefined
    case 'create-key':
      // A mailbox removed meanwhile, or one of another workspace.
      if (code === 'not_found') return t('A chosen mailbox is no longer in this workspace. Choose again.')
      if (code === 'bad_request') return t('The server did not accept this key. Check its name, and that what it holds on each mailbox fits what it may do: Act needs Read.')
      // A member, a role lost meanwhile, Read on a mailbox the person does not read, or Send where keys do not send.
      if (code === 'not_authorized') return t('The server refused this key: only owners and admins create keys, Read is given only on a mailbox you read yourself, and Send only where this server’s keys may send.')
      // No answer, or one the console cannot read, says nothing sure about whether the key was made;
      // the list, read again, shows it if it was. Trying again as if it were not leaves a key nobody saw.
      if (code === 'unavailable' || code === 'internal' || code === 'invalid_response') return t('Mailie could not confirm that the key was created. If it is in the list, revoke it and create another: its secret cannot be shown again.')
      return undefined
    case 'revoke-key':
      if (code === 'not_found') return t('This key no longer exists.')
      if (code === 'not_authorized') return t('Only the owners and admins of a team revoke its API keys.')
      // Revoking twice is harmless, so asking again is the answer to not knowing.
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that the key was revoked. Try again: doing it twice is safe.')
      return undefined
    case 'change-key-access':
      if (code === 'not_authorized') return t('The server refused this: you can give a key Read only on a mailbox you read yourself, and Send only where this server’s keys may send.')
      if (code === 'bad_request') return t('The server did not accept this: Act needs Read and a key that can act, Send a key that can send, and a key made before keys belonged to workspaces gains nothing.')
      if (code === 'conflict') return t('This key no longer works: it was revoked, or it expired.')
      if (code === 'not_found') return t('This key, or this mailbox, is no longer in the workspace.')
      return undefined
    case 'load-key-sends':
      if (code === 'unavailable' || code === 'internal') return t('Could not read this key’s sends. Try again in a moment.')
      return undefined
    case 'load-my-keys':
      if (code === 'unavailable' || code === 'internal') return t('Could not load the API keys you created. Try again in a moment.')
      return undefined
    case 'load-storage':
      if (code === 'unavailable' || code === 'internal') return t('Could not read what your mailboxes take up. Try again in a moment.')
      return undefined
    case 'load-workspaces':
      // Every list waits for them (state/workspaces.ts), whatever kept them from loading.
      if (code === 'unauthorized') return undefined
      return t('Could not load your workspaces. Your mailboxes and what they take up are shown once they load: Mailie tries again by itself, or you can try now.')
    case 'create-team':
    case 'rename-team':
      if (code === 'bad_request') return t('Use a name of 1 to 80 characters.')
      if (code === 'not_authorized') return op === 'create-team' ? t('Your account is not allowed to create teams.') : t('Only owners and admins of the team can rename it.')
      if (code === 'conflict') return t('Teams are not changed here: they come from elsewhere on this server.')
      return undefined
    case 'load-members':
      if (code === 'unavailable' || code === 'internal') return t('Could not load the members. Try again in a moment.')
      if (code === 'not_found') return t('You are no longer a member of this team.')
      if (code === 'not_authorized') return t('Only the owners and admins of the team see its members.')
      return undefined
    case 'change-member':
    case 'remove-member':
      if (code === 'conflict') return t('The team’s protections refuse this: it keeps an active owner, and a mailbox someone reads keeps someone who can read it. Make another member an owner, or give someone else Read, first.')
      if (code === 'not_authorized') return t('Your role in the team does not allow this. Admins change and remove members only, and make nobody an admin or an owner.')
      if (code === 'not_found') return t('This person is no longer a member of the team.')
      return undefined
    case 'leave-team':
      if (code === 'conflict') return t('You cannot leave yet: you are the team’s only owner, or the only person who can read one of its mailboxes. Make another member an owner, or give someone else Read, first.')
      if (code === 'not_authorized') return t('Only an owner leaves a team, while another owner remains.')
      return undefined
    case 'load-invites':
      if (code === 'unavailable' || code === 'internal') return t('Could not load the invitations. Try again in a moment.')
      if (code === 'not_authorized') return t('Only owners and admins of the team see its invitations.')
      return undefined
    case 'create-invite':
      if (code === 'bad_request') return t('Enter a valid email address.')
      if (code === 'conflict') return t('This address is already a member of the team, or this server cannot make invitation links now: its public address is not set.')
      if (code === 'not_authorized') return t('Your role does not allow this invitation. Admins invite members only.')
      if (code === 'unavailable' || code === 'internal' || code === 'invalid_response') return t('Mailie could not confirm that the invitation was made. If it is in the list, revoke it and make another: its link cannot be shown again.')
      return undefined
    case 'revoke-invite':
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that the invitation was revoked. Try again: doing it twice is safe.')
      return undefined
    case 'accept-invite':
      if (code === 'not_authorized') return t('This invitation does not work for your account: it may have expired or been used, or be for another address. An invitation to create an account here is used signed out.')
      if (code === 'conflict') return t('You are already a member of this team.')
      return undefined
    case 'load-access':
      if (code === 'unavailable' || code === 'internal') return t('Could not read who has access. Try again in a moment.')
      if (code === 'not_authorized') return t('Only the owners and admins of the team see who has access to its mailboxes.')
      return undefined
    case 'change-access':
      if (code === 'not_authorized') return t('Only the owners and admins of the team change who has access, and they give Read only on a mailbox they read themselves.')
      if (code === 'conflict') return t('This is the only person who can read this mailbox: give someone else Read on it first.')
      if (code === 'bad_request') return t('The server did not accept this access: Act needs Read, Manage is given to members only, and only active members of the team can be given access.')
      if (code === 'not_found') return t('This person, or this mailbox, is no longer in the team.')
      return undefined
    case 'give-read':
      if (code === 'not_authorized') return t('Only an owner or an admin of the team who reads this mailbox gives Read on it.')
      if (code === 'conflict') return t('Meanwhile, this mailbox’s key, this person’s account key, or what they hold on it changed. What is shown was read again: check it and try again.')
      if (code === 'security') return t('Your key for this mailbox does not open in this browser, so Mailie cannot hand it to anyone. Nothing was sent.')
      return specific({ op: 'change-access', code })
    case 'load-mailbox-key':
      if (code === 'unavailable' || code === 'internal') return t('Could not read this mailbox’s key. Try again in a moment.')
      if (code === 'not_authorized') return t('You do not hold Read on this mailbox, so its key is not shown to you.')
      if (code === 'not_found') return t('This mailbox no longer exists.')
      return undefined
    case 'supply-key':
      if (code === 'not_authorized') return t('Only someone who reads this mailbox hands its key on.')
      if (code === 'conflict') return t('Meanwhile, this person was handed the key, lost Read on the mailbox, or their account key or the mailbox’s key changed. What is shown was read again.')
      if (code === 'not_found') return t('This person is no longer a member of the team, or the mailbox was removed.')
      if (code === 'security') return specific({ op: 'give-read', code })
      return undefined
    case 'first-key':
      if (code === 'not_authorized') return t('Only someone who reads this mailbox creates its key.')
      if (code === 'conflict') return t('This mailbox got its key, or who reads it changed, meanwhile. What is shown was read again.')
      if (code === 'not_found') return t('This mailbox no longer exists.')
      return undefined
    case 'new-key':
      if (code === 'not_authorized') return t('Only the person whose mailbox it is gives it a new key, and a team mailbox never gets one.')
      if (code === 'conflict') return t('This mailbox’s key changed meanwhile. What is shown was read again.')
      if (code === 'not_found') return t('This mailbox no longer exists.')
      return undefined
    case 'team-sync-on':
      // The console asked about an older text than the server's.
      if (code === 'bad_request') return edition().sync.changedWhileOpen()
      if (code === 'not_authorized') return t('Only the owners and admins of the team turn its mailboxes’ sync on or off.')
      if (code === 'not_found') return t('This mailbox no longer exists.')
      // Nobody reads it any more, nor can be given Read on it.
      if (code === 'conflict') return t('Nobody in the team can read this mailbox, so its sync cannot be turned on. Remove it, and connect it again, to use it.')
      return undefined
    case 'team-sync-off':
      if (code === 'not_authorized') return specific({ op: 'team-sync-on', code })
      if (code === 'not_found') return t('This mailbox no longer exists.')
      // Turning it off twice deletes whatever the first one missed.
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that sync was turned off. Try again: doing it twice is safe.')
      return undefined
    default:
      return undefined
  }
}

function general(failure: Failure): string {
  switch (failure.code) {
    case 'unavailable': return t('Could not reach the server. Check your connection and try again.')
    case 'rate_limited': return t('Too many requests. Wait a moment and try again.')
    case 'unauthorized': return t('Your session ended. Sign in again.')
    case 'not_authorized': return t('Your account is not allowed to do this.')
    case 'not_found': return t('This item no longer exists.')
    case 'conflict': return t('This conflicts with the current state. Reload and try again.')
    case 'bad_request': return t('The server did not accept this request.')
    case 'invalid_response': return t('The server sent an answer this page cannot read. Reload the page and try again.')
    case 'flow_expired': return t('The sign-in window closed before it was completed. Try again.')
    case 'flow_failed': return t('The provider did not complete the authorization. Try again.')
    case 'flow_unsupported': return t('This server offered a way to sign in that this page cannot finish.')
    case 'return_invalid': return t('The provider sent back an incomplete answer. Start again.')
    case 'return_expired': return t('This authorization took too long and expired. Start again.')
    case 'key_limit': return t('This workspace has {count} active keys, the most it can have. Revoke one to create another.', { count: count(MAX_LIVE_KEYS) })
    case 'terms_changed': return t('The terms for API keys changed while this page was open. Reload the page to read the current text.')
    case 'security': return t('This server answered something Mailie does not trust, so nothing more was sent. Tell the administrator of this server.')
    case 'upgrade_refused': return t('This browser already set up this account so that your password never leaves it, but the server asked for it, so nothing was sent. Tell the administrator of this server: if they restored it from an older copy, they can send you a reset link.')
    case 'password_too_short': return t('The new password needs at least {count} characters.', { count: MIN_PASSWORD })
    case 'password_too_long': return t('The password can have at most 256 characters.')
    case 'password_invalid':
    case 'password_rejected':
      return t('The password has characters that cannot be used, such as control characters.')
    case 'recovery_code': return t('That is not a recovery code. It has 30 letters and digits, in six groups of five.')
    case 'derive_failed': return t('This browser could not process the password. Close other tabs and try again.')
    case 'no_account_key': return t('This browser does not hold your account key. Sign out, sign in again here, and try again.')
    case 'not_enrolled': return t('Your account has no account key yet. Sign out and sign in again with your password to set it up, then try again.')
    case 'aborted':
    case 'internal':
      return t('Something went wrong on the server. Try again in a moment.')
  }
}

function editionWords(failure: Failure): string | undefined {
  for (const describer of describers) {
    const words = describer(failure)
    if (words) return words
  }
  return undefined
}

export function describe(failure: Failure): string {
  return (failure.account && reasonText(failure.account)) || editionWords(failure) || (specific(failure) ?? general(failure))
}

/**
 * describeFolders says why listing an account's folders failed. The server
 * answers 409 for several causes it does not tell apart (the account is
 * pending, the provider refused the grant, the mail server refused the saved
 * password or could not be reached), so the account the sheet already has
 * decides which of them to name. A refused grant moves the account to
 * needs_reauth as it is refused, and the store asks for the account again
 * before it reports the 409, so the sheet has the new state by then.
 */
export function describeFolders(failure: Failure, account: Pick<Account, 'state' | 'auth_kind' | 'provider' | 'email' | 'state_reason'>): string {
  if (failure.code !== 'conflict') return describe(failure)
  if (account.state === 'pending_auth') return t('Finish the authorization before listing folders.')
  if (account.auth_kind !== 'oauth2') return t('The mail server did not accept the saved password, or cannot be reached from this server. To change the password, remove the account and connect it again.')
  // Beside the "Finish authorization" button the sheet shows for it, and
  // below the status, which already says the reason when there is one.
  if (account.state === 'needs_reauth' || reasonText(account)) return describe(failure)
  return t('The mail server would not open this mailbox. Check that IMAP is enabled for it, or authorize the account again.')
}

