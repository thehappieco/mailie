// The reset link the page was opened with (an <origin>/#reset=…&email=…
// link, ui/signupLink.ts), until it is used or set aside. In memory only: its
// code never reaches storage, the address bar or history. It is the
// operator's way back for a person who lost both their password and their
// recovery code (docs/key-scheme.md section 12.6).

import { reactive } from 'vue'
import type { ResetLink } from '../ui/signupLink'

export const resetLink = reactive<{ pending: ResetLink | null }>({ pending: null })

/** Holds the reset link the page was opened with. */
export function holdReset(value: ResetLink | null): void {
  resetLink.pending = value
}

/** Lets go of it: used, or set aside by the person. */
export function dropReset(): void {
  resetLink.pending = null
}
