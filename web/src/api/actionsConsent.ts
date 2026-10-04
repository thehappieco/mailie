// The person's consent to actions on their messages: Mailie changing their
// mailboxes when they, or a tool holding one of their keys, ask. Who may act
// is decided by the server (internal/service); this file only asks.

import { checked, request } from './http'
import { isSyncConsent, type ActionsConsent } from './types'

export async function getActionsConsent(token: string, signal?: AbortSignal): Promise<ActionsConsent> {
  return checked(await request('/v1/me/actions-consent', { token, signal }), isSyncConsent)
}

/** version names the revision of the text the person was shown; the server refuses any other. */
export async function grantActionsConsent(token: string, version: string): Promise<ActionsConsent> {
  return checked(await request('/v1/me/actions-consent', { token, body: { version } }), isSyncConsent)
}

/** Nothing is stored by actions, so withdrawing only stops them. */
export async function withdrawActionsConsent(token: string): Promise<ActionsConsent> {
  return checked(await request('/v1/me/actions-consent', { token, method: 'DELETE' }), isSyncConsent)
}
