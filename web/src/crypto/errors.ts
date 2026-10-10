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
// password_too_short, password_too_long, password_invalid: the platform's
// preparation refused a new password, before anything was derived.
// password_rejected: it refused a password being presented.
// recovery_code: not a recovery code (docs/key-scheme.md section 5.6).
// derive_failed: this browser could not run the derivation (memory, most
// likely).
// no_account_key: this browser does not hold the person's account key (they
// signed in elsewhere, or it refuses storage); signing in again here keeps it.
// It is what opening a grant needs (docs/key-scheme.md section 9.2).
// not_enrolled: the person has no account key at all yet (signed up before
// the key scheme, never enrolled, and still signed in from then): nothing can
// be sealed to them, so they link no mailbox until a reset link from the
// administrator enrols them. Nothing is sent.
export type CeremonyCode = 'security' | 'password_too_short' | 'password_too_long' | 'password_invalid'
  | 'password_rejected' | 'recovery_code' | 'derive_failed' | 'no_account_key' | 'not_enrolled'

export class CeremonyError extends Error {
  constructor(readonly code: CeremonyCode) {
    super(`The key scheme refused this (${code}).`)
    this.name = 'CeremonyError'
  }
}
