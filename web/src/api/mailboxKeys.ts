// A mailbox's key, for a person signed in (docs/key-scheme.md sections 8, 9
// and 12.11 to 12.15; docs/console.md, "Mailbox keys"): what this console
// reads to seal and open grants, a mailbox's first key, a personal mailbox's
// next one, and the key handed to a member who holds Read without it. The
// browser makes the keys and the grants; the server stores the public half,
// the namespace and the grants, and decides who may. No route takes a
// private key.

import { segment } from './endpoint'
import { checked, request } from './http'
import {
  isMailboxKeyPair, isMailboxKeyState, isSealedGrant,
  type FirstKeyRequest, type MailboxKeyPair, type MailboxKeyState, type NewKeyRequest, type SealedGrant, type SupplyKeyRequest,
} from './types'

/** GET /v1/accounts/{id}/mailbox-key: the key, the caller's own grant, and to whom, or from whom, it may go. */
export async function getMailboxKey(token: string, accountID: string, signal?: AbortSignal): Promise<MailboxKeyState> {
  return checked(await request(`/v1/accounts/${segment(accountID)}/mailbox-key`, { token, signal }), isMailboxKeyState)
}

/** POST /v1/accounts/{id}/mailbox-key: a mailbox's first key, with a grant for everyone who holds Read with an account key. */
export async function writeFirstKey(token: string, accountID: string, body: FirstKeyRequest): Promise<MailboxKeyPair> {
  return checked(await request(`/v1/accounts/${segment(accountID)}/mailbox-key`, { token, body }), isMailboxKeyPair)
}

/** PUT /v1/accounts/{id}/mailbox-key: a personal mailbox's next key, at the epoch after its current one. */
export async function writeNewKey(token: string, accountID: string, body: NewKeyRequest): Promise<MailboxKeyPair> {
  return checked(await request(`/v1/accounts/${segment(accountID)}/mailbox-key`, { token, method: 'PUT', body }), isMailboxKeyPair)
}

/** PUT /v1/accounts/{id}/grants/{user}: the key, to a member who holds Read without it. */
export async function supplyKey(token: string, accountID: string, userID: string, body: SupplyKeyRequest): Promise<SealedGrant> {
  return checked(await request(`/v1/accounts/${segment(accountID)}/grants/${segment(userID)}`, { token, method: 'PUT', body }), isSealedGrant)
}
