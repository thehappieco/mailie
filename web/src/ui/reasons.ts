// The few reasons the server gives for a failing account that the console
// explains in its own words.
//
// state_reason is English and written for logs, so the console never shows it.
// A handful are short fixed strings (internal/account), and for some of them
// the person can do something specific: allow access to the mailbox on the
// provider's consent screen, or pick the right account on its sign-in screen.
// Those are matched exactly here; any other reason falls back to the text for
// the state alone.

import type { Account } from '../api/types'
import { t } from './i18n'

/** A consent whose token does not cover the mailbox: a box left unticked on the provider's screen. */
export const REASON_NOT_GRANTED = 'the provider did not grant access to the mailbox'
/** A consent whose grant the mail server would not log in with: usually another account chosen at the provider. */
export const REASON_MAILBOX_REFUSED = 'the mail server refused this authorization for the mailbox'
/**
 * A working account whose token the mail server stopped taking, a freshly
 * refreshed one too. The fix is the same as for a refused consent: consent
 * again, as the right account.
 */
export const REASON_TOKEN_REJECTED = "the mail server rejected the account's access token"

export type ReasonFacts = Pick<Account, 'provider' | 'email' | 'state' | 'state_reason'>

/**
 * reasonText says what went wrong and what to do about it, for an account
 * that is failing for a reason the console knows; otherwise undefined.
 * The provider names the screen the person has to look for.
 */
export function reasonText(account: ReasonFacts): string | undefined {
  if (account.state !== 'error' && account.state !== 'needs_reauth') return undefined
  switch (account.state_reason) {
    case REASON_NOT_GRANTED:
      switch (account.provider) {
        case 'gmail': return t('Google did not give Mailie access to this mailbox. Try again and, on Google’s screen, allow access to Gmail (tick the box if one is shown).')
        case 'microsoft': return t('Microsoft did not give Mailie access to this mailbox. Try again and accept every permission on Microsoft’s screen. For a work or school account, an administrator may have to allow Mailie first.')
        default: return t('The provider did not give Mailie access to this mailbox. Try again and allow access to the mailbox on the provider’s screen.')
      }
    case REASON_MAILBOX_REFUSED:
    case REASON_TOKEN_REJECTED:
      switch (account.provider) {
        case 'gmail': return t('The mail server refused this authorization. Try again and choose the account {email} on Google’s sign-in screen.', { email: account.email })
        case 'microsoft': return t('The mail server refused this authorization. Try again and choose the account {email} on Microsoft’s sign-in screen. If you already did, check that IMAP is turned on for this mailbox.', { email: account.email })
        default: return t('The mail server refused this authorization. Try again and choose the account {email} on the provider’s sign-in screen.', { email: account.email })
      }
    default:
      return undefined
  }
}
