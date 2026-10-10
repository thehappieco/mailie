// What a ceremony of the key scheme refuses in this browser, by code: the
// console translates the code (ui/errors.ts), never a message, which names
// no key, password, code or address in any case.
//
// security: the server answered something this browser will not use, such
// as a wrap that does not open for the auth key it just accepted, a seal id
// or public key outside their spelling, KDF parameters or a salt outside the
// platform's bounds, a grant that does not open as the person's grant of the
// mailbox's key, or a recipient's key no grant may be sealed to. Nothing more is sent, and the person is told to
// contact whoever runs the server.
// upgrade_refused: the server asked for the password in clear (the upgrade,
// docs/key-scheme.md section 12.7) of an address this browser saw enrol.
// Nothing is sent. A server put back from a copy older than the key scheme
// does this honestly too, and the person's way back is then a reset link.
// password_too_short, password_too_long, password_invalid: the platform's
// preparation refused a new password, before anything was derived.
// password_rejected: it refused a password being presented.
// recovery_code: not a recovery code (docs/key-scheme.md section 5.6).
// derive_failed: this browser could not run the derivation (memory, most
// likely).
// no_account_key: this browser does not hold the person's account key (they
// signed in elsewhere, or it refuses storage); signing in again here keeps it.
// It is what opening a grant needs (docs/key-scheme.md section 9.2).
// not_enrolled: the person has no account key at all yet (signed in from
// before the upgrade): nothing can be sealed to them, so they link no
// mailbox until they sign in again, which enrols them. Nothing is sent.
export type CeremonyCode = 'security' | 'upgrade_refused' | 'password_too_short' | 'password_too_long' | 'password_invalid'
  | 'password_rejected' | 'recovery_code' | 'derive_failed' | 'no_account_key' | 'not_enrolled'

export class CeremonyError extends Error {
  constructor(readonly code: CeremonyCode) {
    super(`The key scheme refused this (${code}).`)
    this.name = 'CeremonyError'
  }
}
