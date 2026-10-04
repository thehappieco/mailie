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
      return undefined
    case 'sign-up':
      if (code === 'not_authorized') return t('This invitation is not valid for this address. It may have expired or already been used.')
      if (code === 'bad_request') return t('Check the details. The password needs at least 10 characters.')
      if (code === 'conflict') return t('An account with this email already exists. Sign in instead.')
      if (code === 'rate_limited') return t('Too many sign-in attempts. Wait a minute and try again.')
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
      if (code === 'bad_request') return t('The new password needs at least 10 characters.')
      if (code === 'rate_limited') return t('Too many attempts. Wait a minute and try again.')
      return undefined
    case 'load-accounts':
      if (code === 'unavailable' || code === 'internal') return t('Could not load your email accounts. Try again in a moment.')
      return undefined
    case 'load-providers':
      return t('Could not check which providers this server supports. You can still try to connect.')
    case 'add-account':
      if (code === 'conflict') return t('This address is already connected. If it is waiting for authorization, finish it from its card.')
      if (code === 'bad_request') return t('The server did not accept these details. Check the address and try again.')
      if (code === 'not_authorized') return t('Your account is not allowed to connect mailboxes.')
      if (code === 'flow_unsupported') return t('This server offered a way to sign in that this console cannot finish.')
      return undefined
    case 'test-login':
      if (code === 'bad_request') return t('The mail server refused the sign-in or could not be reached. Check the address, the password and the server names.')
      if (code === 'conflict') return t('This address is already connected.')
      if (code === 'not_authorized') return t('Your account is not allowed to connect mailboxes.')
      return undefined
    case 'test-login-icloud':
      // The usual cause by far: the Apple Account password, which iCloud refuses over IMAP.
      if (code === 'bad_request') return t('iCloud refused the sign-in. Check the address and that you used an app-specific password, not your Apple Account password.')
      return specific({ op: 'test-login', code })
    case 'start-auth':
      if (code === 'bad_request') return t('This server cannot start the sign-in for this provider. It may not be configured for it.')
      if (code === 'conflict') return t('This account cannot be authorized right now.')
      if (code === 'not_found') return t('This account no longer exists.')
      if (code === 'flow_unsupported') return t('This server offered a way to sign in that this console cannot finish.')
      return undefined
    case 'wait-auth':
      if (code === 'not_found') return t('The account was removed while waiting for authorization.')
      if (code === 'flow_expired') return t('The sign-in window closed before it was completed. Try again.')
      if (code === 'flow_failed') return t('The provider did not complete the authorization. Try again.')
      return undefined
    case 'complete-auth':
      if (code === 'bad_request') return t('The provider did not grant access. You can try again from the account.')
      if (code === 'not_found') return t('This authorization expired or was started by another sign-in. Start again from the console.')
      if (code === 'return_invalid') return t('The provider sent back an incomplete answer. Start again from the console.')
      if (code === 'return_expired') return t('This authorization took too long and expired. Start again from the console.')
      return undefined
    case 'remove-account':
      if (code === 'conflict') return t('This account cannot be removed right now. Try again in a moment.')
      return undefined
    case 'folders':
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
      if (code === 'unavailable' || code === 'internal') return t('Could not load your API keys. Try again in a moment.')
      return undefined
    case 'create-key':
      // A mailbox removed meanwhile, or one the person sees without owning it (an owner sees the instance's own).
      if (code === 'not_found') return t('A chosen mailbox cannot be given to a key: it was removed, or it is not one you connected. Choose again.')
      if (code === 'bad_request') return t('The server did not accept this key. Check its name and the mailboxes, and try again.')
      if (code === 'not_authorized') return t('Your account is not allowed to create API keys.')
      // No answer, or one the console cannot read, says nothing sure about whether the key was made;
      // the list, read again, shows it if it was. Trying again as if it were not leaves a key nobody saw.
      if (code === 'unavailable' || code === 'internal' || code === 'invalid_response') return t('Mailie could not confirm that the key was created. If it is in the list, revoke it and create another: its secret cannot be shown again.')
      return undefined
    case 'revoke-key':
      if (code === 'not_found') return t('This key no longer exists.')
      // Revoking twice is harmless, so asking again is the answer to not knowing.
      if (code === 'unavailable' || code === 'internal') return t('Could not confirm that the key was revoked. Try again: doing it twice is safe.')
      return undefined
    case 'load-storage':
      if (code === 'unavailable' || code === 'internal') return t('Could not read what your mailboxes take up. Try again in a moment.')
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
    case 'invalid_response': return t('The server sent an answer this console cannot read. Reload the page and try again.')
    case 'flow_expired': return t('The sign-in window closed before it was completed. Try again.')
    case 'flow_failed': return t('The provider did not complete the authorization. Try again.')
    case 'flow_unsupported': return t('This server offered a way to sign in that this console cannot finish.')
    case 'return_invalid': return t('The provider sent back an incomplete answer. Start again from the console.')
    case 'return_expired': return t('This authorization took too long and expired. Start again from the console.')
    case 'key_limit': return t('You have {count} active keys, the most you can have. Revoke one to create another.', { count: count(MAX_LIVE_KEYS) })
    case 'terms_changed': return t('The terms for API keys changed while this page was open. Reload the page to read the current text.')
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

