import { ApiError, type ErrorCode } from '../api/http'
import type { ReasonFacts } from '../ui/reasons'

/**
 * What the console was doing when something failed. The same code means
 * different things to a person depending on this — a 403 while changing a
 * password is a wrong current password; while signing up it is an invitation
 * that does not fit — so a failure is always the pair, and ui/errors.ts turns
 * the pair into words.
 *
 * These are the core's. An edition names its own by augmenting the interface
 * (declare module '@core/state/failure' { interface Operations { … } }) and
 * says what they mean with ui/errors.ts addDescriber.
 */
export interface Operations {
  'sign-in': true; 'sign-up': true; 'restore': true; 'sign-out': true; 'profile': true; 'password': true
  'load-accounts': true; 'load-providers': true; 'add-account': true; 'test-login': true; 'test-login-icloud': true
  'start-auth': true; 'wait-auth': true; 'complete-auth': true; 'remove-account': true; 'folders': true
  'sync-consent': true; 'grant-sync': true; 'withdraw-sync': true; 'sync-now': true
  'actions-consent': true; 'grant-actions': true; 'withdraw-actions': true
  'load-keys': true; 'create-key': true; 'revoke-key': true; 'change-key-access': true; 'load-key-sends': true; 'load-my-keys': true
  'load-storage': true
  'load-workspaces': true; 'create-team': true; 'rename-team': true; 'accept-invite': true
  'load-members': true; 'change-member': true; 'remove-member': true; 'leave-team': true
  'load-invites': true; 'create-invite': true; 'revoke-invite': true
  'load-access': true; 'change-access': true; 'team-sync-on': true; 'team-sync-off': true
}
export type Operation = keyof Operations

/**
 * Beyond the transport's codes, the ones only the console can know: the
 * consent window closed, the account left pending for a failed state, the
 * server offered a flow a browser cannot finish, or the provider's redirect
 * came back malformed or too late; a new key refused because the workspace
 * holds as many as it may, or because the text they were shown is no
 * longer the one the server asks about (a 409 on creating a key is either,
 * and state/apikeys.ts reads the list again to tell which).
 */
export type FailureCode = ErrorCode | 'flow_expired' | 'flow_failed' | 'flow_unsupported' | 'return_invalid' | 'return_expired' | 'key_limit' | 'terms_changed'

/**
 * account is set when the failure left an account failing (a consent that
 * ended in error, a grant the mail server refused): the server records why
 * on the account, and a reason the console knows (ui/reasons.ts) says more
 * to the person than the operation's own text.
 */
export interface Failure { op: Operation; code: FailureCode; account?: ReasonFacts; partial?: PartialChange }

/**
 * An action on more messages than one request takes is sent in parts; when a
 * later part fails, the earlier ones were still made. done is how many
 * messages the server confirmed; left, how many it did not.
 */
export interface PartialChange { done: number; left: number }

export function failure(op: Operation, error: unknown): Failure {
  return { op, code: error instanceof ApiError ? error.code : 'internal' }
}
