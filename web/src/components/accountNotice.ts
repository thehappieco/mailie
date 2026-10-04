// The one success line at the top of the account section. AccountPanel shows
// it; the permission rows an edition puts in the section say through it what
// they just did, so a change of name, password or permission all land in the
// same place.

import type { InjectionKey } from 'vue'

export interface AccountNotice {
  /** Shows the section's success line, in place of the last one: words read in the language of the moment. */
  show(words: () => string): void
  /** Takes the line away: something else is being changed now. */
  clear(): void
}

export const accountNotice: InjectionKey<AccountNotice> = Symbol('account-notice')

/** Outside an account section there is nowhere to say it. */
export const noAccountNotice: AccountNotice = { show() {}, clear() {} }
