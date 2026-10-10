// Opening the person's own grant of a mailbox, to seal the mailbox's key to
// someone else (docs/key-scheme.md sections 9.2 and 12.13): giving Read with
// a grant, and supplying the key to a member who holds Read without it. The
// mailbox's key, its namespace and epoch, and the person's own grant are
// read from the server (GET /v1/accounts/{id}/mailbox-key); the account key
// that opens the grant from this browser's vault (state/accountVault.ts), as
// the kit's non-extractable private key, never the raw bytes. The mailbox key
// it opens is lent to the sealing and zeroed after it; one at a time.

import * as api from '../api/mailboxKeys'
import { ApiError } from '../api/http'
import { enrolled, type MailboxKeyState } from '../api/types'
import { CeremonyError } from '../crypto/errors'
import { openOwnGrant, sealTo, type GrantRecipient } from '../crypto/mailbox'
import type { Bytes } from '../crypto/mailie'
import { accountPrivateKeyOf } from './accountVault'
import { authorized, session } from './session'

/**
 * withOwnMailboxKey opens the person's own grant in state with the account
 * key this browser keeps for them, lends the mailbox's private key to use,
 * and zeroes it after, whatever happens. Without an account key at all it is
 * not_enrolled; without the key in this browser (another browser's sign-in,
 * storage refused), no_account_key, which signing in again here mends; a
 * grant that does not open, or opens to another key than the mailbox's
 * public one, is security.
 */
export async function withOwnMailboxKey<T>(state: MailboxKeyState, use: (mailboxKey: Bytes) => Promise<T>): Promise<T> {
  const user = session.user
  if (!user || !enrolled(user)) throw new CeremonyError('not_enrolled')
  if (!state.grant) throw new ApiError('not_authorized')
  const account = session.keyed ? await accountPrivateKeyOf(user.seal_id!, user.public_key!) : null
  if (!account) {
    session.keyed = false
    throw new CeremonyError('no_account_key')
  }
  const mailboxKey = await openOwnGrant(account, user.seal_id!, state, state.grant)
  try {
    return await use(mailboxKey)
  } finally {
    mailboxKey.fill(0)
  }
}

/** A grant sealed for someone, at the epoch it binds: what PUT …/access/{user} and PUT …/grants/{user} send. */
export interface SealedFor { epoch: number; grant: string }

/**
 * sealFromOwn seals a mailbox's key to someone from the person's own grant:
 * the key as the server holds it now, the person's grant opened, the key
 * sealed to the recipient's public key and seal id at its current epoch. A
 * mailbox without a key has nothing to seal (conflict: the console reads it
 * again); a person who waits for the key themselves has nothing to give
 * (not_authorized, as the server would answer).
 */
export async function sealFromOwn(accountID: string, recipient: GrantRecipient): Promise<SealedFor> {
  const state = await authorized(token => api.getMailboxKey(token, accountID))
  if (state.epoch === undefined || !state.namespace) throw new ApiError('conflict')
  const epoch = state.epoch
  const namespace = state.namespace
  const grant = await withOwnMailboxKey(state, key => sealTo(recipient, namespace, epoch, key))
  return { epoch, grant }
}
