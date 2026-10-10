// A mailbox's key as this console shows and changes it (docs/key-scheme.md
// sections 8, 9 and 12.12 to 12.14; docs/console.md, "Mailbox keys"): what
// the sheet of a mailbox reads (GET /v1/accounts/{id}/mailbox-key: the key,
// the person's own grant, the members waiting for it, who may supply it, and
// whom a first key is sealed to), handing the key to a member who holds
// Read without it, a mailbox's first key, and a personal mailbox's new one.
// Each write needs a fresh step-up (state/stepUp.ts), asked before the call;
// a mailbox's first keys are also written right after a sign-in, while that
// sign-in counts as one, and never on the page load of an older session
// (firstKeysAfterSignIn).
//
// The browser makes every key and seals every grant (crypto/mailbox.ts);
// each private key is zeroed as soon as its grants are sealed. After its own
// writes the console reads the key, the mailbox's card and the team's
// directory again: the server's event stream tells only the person whose
// reading changed.

import { reactive, watch } from 'vue'
import { listAccounts } from '../api/accounts'
import { ApiError } from '../api/http'
import * as api from '../api/mailboxKeys'
import { enrolled, type Account, type KeyRecipient, type MailboxKeyState, type User } from '../api/types'
import { CeremonyError } from '../crypto/errors'
import { newMailboxKey, sealAll } from '../crypto/mailbox'
import { accounts, accountsSettled, refreshAccount } from './accounts'
import { failure, type Failure } from './failure'
import { onceMore, requireAccountKeyHere, sealFromOwn, withOwnMailboxKey } from './grants'
import { authorized, freshStepUp, identity, onSignIn, session } from './session'
import { StepUpCancelled, withStepUp } from './stepUp'
import { refreshTeam } from './team'

/**
 * The operator workspace's fixed id (docs/workspaces.md): its mailboxes have
 * no key in phase 3 (docs/key-scheme.md section 12.15), and a person never
 * lists one; one listed all the same is never keyed from here.
 */
const OPERATOR_WORKSPACE = 'wsp_operator'

/** What the console knows of one mailbox's key. */
export interface KeyView {
  loading: boolean
  loaded: boolean
  /** GET /v1/accounts/{id}/mailbox-key's answer, once read. */
  state: MailboxKeyState | null
  failure: Failure | null
  /**
   * Whether the person's own grant opens in this browser, to the key the
   * server holds (docs/key-scheme.md section 9.2): asked of a personal
   * mailbox's sheet, whose person may write it a new key when it does not.
   * unknown until tried, or when this browser holds no account key.
   */
  opens: 'unknown' | 'yes' | 'no'
  /** What is being written: '' for nothing, 'first', 'new', or the id of the member being handed the key. */
  busy: string
  /** The last write's failure, said beside it. */
  problem: Failure | null
}

const freshView = (): KeyView => ({ loading: false, loaded: false, state: null, failure: null, opens: 'unknown', busy: '', problem: null })

export const mailboxKeys = reactive<{ views: Record<string, KeyView> }>({ views: {} })

let generation = 0
watch(identity, () => {
  generation++
  mailboxKeys.views = {}
}, { flush: 'sync' })

function current(): () => boolean {
  const at = generation
  return () => at === generation
}

/** The view of a mailbox's key, made on first use. */
export function keyView(accountID: string): KeyView {
  return mailboxKeys.views[accountID] ??= freshView()
}

/** Reads a mailbox's key again, for a person who holds Read on it. */
export async function loadMailboxKey(accountID: string): Promise<void> {
  const ok = current()
  const view = keyView(accountID)
  view.loading = true
  view.failure = null
  try {
    const state = await authorized(token => api.getMailboxKey(token, accountID))
    if (!ok()) return
    const now = keyView(accountID)
    // Another epoch, or another grant: whether it opens is asked again.
    if (now.state?.epoch !== state.epoch || now.state?.grant !== state.grant) now.opens = 'unknown'
    now.state = state
    now.loaded = true
  } catch (error) {
    if (ok()) keyView(accountID).failure = failure('load-mailbox-key', error)
  } finally {
    if (ok()) keyView(accountID).loading = false
  }
}

/**
 * Tries the person's own grant of a mailbox in this browser, as read last:
 * opens yes, or no when it does not open as their grant of the mailbox's
 * key (section 9.2), which for a personal mailbox its person mends with a new
 * key. Without the account key here nothing is tried.
 */
export async function checkOwnGrant(accountID: string): Promise<void> {
  const ok = current()
  const view = keyView(accountID)
  const state = view.state
  if (!state?.grant || !session.keyed) return
  try {
    await withOwnMailboxKey(state, async () => undefined)
    if (ok() && keyView(accountID).state === state) keyView(accountID).opens = 'yes'
  } catch (error) {
    if (ok() && keyView(accountID).state === state && error instanceof CeremonyError && error.code === 'security') keyView(accountID).opens = 'no'
  }
}

/** The person signed in, as someone a grant is sealed to. */
function self(user: User): KeyRecipient {
  return { user_id: user.id, email: user.email, name: user.name, seal_id: user.seal_id!, public_key: user.public_key! }
}

/**
 * firstKey writes a mailbox's first key (section 12.14): a key pair and a
 * namespace made here, sealed to the person and to everyone else the server
 * lists as holding Read with an account key (keyless_readers), in one
 * request. A conflict (someone enrolled, or another tab keyed it,
 * meanwhile) reads the list again and tries once more. Answers false when
 * the mailbox has a key already.
 */
async function firstKey(accountID: string): Promise<boolean> {
  const user = session.user
  if (!user || !enrolled(user)) throw new CeremonyError('not_enrolled')
  for (let attempt = 0; ; attempt++) {
    const state = await authorized(token => api.getMailboxKey(token, accountID))
    if (state.epoch !== undefined) return false
    const recipients: KeyRecipient[] = [self(user), ...state.keyless_readers.filter(reader => reader.user_id !== user.id)]
    const made = await newMailboxKey()
    const grants = await sealAll(made, recipients)
    try {
      await authorized(token => api.writeFirstKey(token, accountID, {
        public_key: made.publicKey, namespace: made.namespace,
        grants: recipients.map((recipient, i) => ({ user_id: recipient.user_id, grant: grants[i]!, public_key: recipient.public_key })),
      }))
      return true
    } catch (error) {
      if (attempt === 0 && error instanceof ApiError && error.code === 'conflict') continue
      throw error
    }
  }
}

/** After a write of the console's own: the key, the mailbox's card and the team's directory are read again. */
async function readAgain(accountID: string): Promise<void> {
  refreshTeam()
  await Promise.all([loadMailboxKey(accountID), refreshAccount(accountID)])
}

/**
 * write runs one write of a mailbox's key from its sheet: after a fresh
 * step-up, marked busy meanwhile, its failure said beside it, and what it
 * changed read again. Closing the step-up stops it without a word. A
 * conflict, another write first, reads everything again too. One that opens
 * the person's own grant (opens) is refused before the step-up is asked
 * when this browser does not hold their account key.
 */
async function write(accountID: string, busy: string, op: Failure['op'], call: () => Promise<unknown>, opens = false): Promise<boolean> {
  const ok = current()
  const view = keyView(accountID)
  if (view.busy) return false
  view.busy = busy
  view.problem = null
  try {
    if (opens) requireAccountKeyHere()
    await withStepUp(call)
  } catch (error) {
    if (!ok()) return false
    keyView(accountID).busy = ''
    if (error instanceof StepUpCancelled) return false
    const found = failure(op, error)
    keyView(accountID).problem = found
    if (found.code === 'conflict' || found.code === 'not_found') await readAgain(accountID)
    return false
  }
  if (!ok()) return false
  await readAgain(accountID)
  keyView(accountID).busy = ''
  return true
}

/**
 * supplyKey hands a mailbox's key to a member who holds Read on it without
 * it (section 12.13): the person's own grant opened here, the key sealed at
 * its current epoch to the member as the answer read in the same call lists
 * them among those waiting, never as the sheet showed them (a reset since
 * gave them another account key). A conflict, the server finding the key it
 * names is not theirs now, reads them again and tries once more.
 */
export function supplyKey(accountID: string, recipient: Pick<KeyRecipient, 'user_id'>): Promise<boolean> {
  return write(accountID, recipient.user_id, 'supply-key', () => onceMore(async () => {
    const sealed = await sealFromOwn(accountID, state => state.waiting.find(person => person.user_id === recipient.user_id))
    return authorized(token => api.supplyKey(token, accountID, recipient.user_id, sealed))
  }), true)
}

/** writeFirstKey writes the first key of a mailbox without one, from its sheet (section 12.14). */
export function writeFirstKey(accountID: string): Promise<boolean> {
  return write(accountID, 'first', 'first-key', async () => {
    // Keyed meanwhile (another tab, another reader): there is nothing to write, and what is shown is read again.
    if (!await firstKey(accountID)) throw new ApiError('conflict')
  })
}

/**
 * writeNewKey gives a personal mailbox its next key (section 12.12): a new
 * pair at the epoch after the one the server holds now, in the mailbox's own
 * namespace, sealed to its person alone. Every grant of an older epoch goes.
 */
export function writeNewKey(account: Account): Promise<boolean> {
  return write(account.id, 'new', 'new-key', async () => {
    const user = session.user
    if (!user || !enrolled(user)) throw new CeremonyError('not_enrolled')
    const state = await authorized(token => api.getMailboxKey(token, account.id))
    // A mailbox without a key gets a first one instead.
    if (state.epoch === undefined || !state.namespace) throw new ApiError('conflict')
    const made = await newMailboxKey(state.namespace, state.epoch + 1)
    const [grant] = await sealAll(made, [self(user)])
    return authorized(token => api.writeNewKey(token, account.id, { epoch: made.epoch, public_key: made.publicKey, grant: grant! }))
  })
}

/**
 * writeFirstKeys writes the first key of every mailbox the person reads
 * without one, but an operator's (section 12.14), right after a sign-in:
 * while its step-up time counts and this browser keeps the person's account
 * key, one request per mailbox, without asking for the password again. A
 * mailbox it could not key is left as it is: its sheet offers the key,
 * behind a step-up. Answers the mailboxes it keyed.
 */
export async function writeFirstKeys(): Promise<string[]> {
  const user = session.user
  if (!user || !enrolled(user) || !session.keyed || !freshStepUp()) return []
  const ok = current()
  let list: Account[]
  try {
    list = await authorized(token => listAccounts(token))
  } catch {
    return []
  }
  const keyed: string[] = []
  for (const account of list) {
    if (!ok() || !freshStepUp()) break
    if (!account.access?.read || account.mailbox_key || account.workspace_id === OPERATOR_WORKSPACE) continue
    try {
      if (await firstKey(account.id)) keyed.push(account.id)
    } catch { /* Left for its sheet. */ }
  }
  if (!ok() || !keyed.length) return keyed
  refreshTeam()
  // The cards say what the server holds now, once the list in flight, if any, has landed.
  await accountsSettled()
  if (ok() && accounts.loaded) await Promise.all(keyed.map(id => refreshAccount(id)))
  return keyed
}

/** firstKeysAfterSignIn writes first keys after every sign-in in this page (state/session.ts onSignIn), and answers what stops it. */
export function firstKeysAfterSignIn(): () => void {
  return onSignIn(() => { void writeFirstKeys() })
}
