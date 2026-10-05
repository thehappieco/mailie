// Every mailbox the signed-in person holds a grant on, in every workspace:
// what the person's own settings name, which are not any one workspace's.
// An API key acts as its person wherever they are a member, and may be made
// for mailboxes of several workspaces; the person's consent to sync covers
// every mailbox they linked, team mailboxes included, which turning it off
// deletes the index of. The lists a workspace shows are state/accounts.ts's.
//
// Read when one of those asks (the API keys section, the dialog that turns
// sync off), and reset when the person changes.

import { reactive, watch } from 'vue'
import { listAccounts } from '../api/accounts'
import type { Account } from '../api/types'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

interface EveryMailboxState {
  list: Account[]
  loaded: boolean
  loading: boolean
  failure: Failure | null
}

const fresh = (): EveryMailboxState => ({ list: [], loaded: false, loading: false, failure: null })

export const everyMailbox = reactive<EveryMailboxState>(fresh())

let generation = 0
watch(identity, () => {
  generation++
  Object.assign(everyMailbox, fresh())
}, { flush: 'sync' })

export async function loadEveryMailbox(): Promise<void> {
  const at = generation
  everyMailbox.loading = true
  everyMailbox.failure = null
  try {
    const list = await authorized(token => listAccounts(token))
    if (at !== generation) return
    everyMailbox.list = list
    everyMailbox.loaded = true
  } catch (error) {
    if (at === generation) everyMailbox.failure = failure('load-accounts', error)
  } finally {
    if (at === generation) everyMailbox.loading = false
  }
}
