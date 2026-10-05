// What the mailboxes the signed-in person may read take up in this server's
// index, in the workspace the console shows (?workspace=), as the Storage
// section shows it. Read when the section is first shown, on Refresh, and
// when what it counts changed; it resets when the person changes, and is read
// again for another workspace. A slow answer for one person, or one
// workspace, is never drawn in front of the next. Asked again while a read is
// in flight, it reads once more when that one ends: the answer in flight may
// predate the change.

import { reactive, watch } from 'vue'
import { getStorage } from '../api/storage'
import type { Storage } from '../api/types'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'
import { settleWorkspace, workspaces, WorkspacesUnknown } from './workspaces'

interface StorageState {
  /** The workspace the figures were read for; empty for every workspace (a server without them). */
  workspace: string
  loaded: boolean
  loading: boolean
  failure: Failure | null
  /** The last answer; null until there is one. */
  usage: Storage | null
}

const fresh = (): StorageState => ({ workspace: '', loaded: false, loading: false, failure: null, usage: null })

export const storage = reactive<StorageState>(fresh())

let generation = 0
/** Asked for while a read was in flight: read again once it ends. */
let again = false
watch(identity, () => {
  generation++
  again = false
  Object.assign(storage, fresh())
}, { flush: 'sync' })

/** The workspace the read in flight asks for; null while it is still learning which. */
let asking: string | null = null

// Another workspace shown: figures read are the one before's, and are read
// again for the new one, as are figures that failed (waiting for the
// workspaces too); never asked for, they are left for whoever asks, and a
// first read still learning which workspace to ask for asks for this one.
watch(() => workspaces.currentID, id => {
  if (storage.loaded && storage.workspace === id) return
  if (!storage.loaded && storage.loading && asking === null) return
  const wanted = storage.loaded || storage.loading || storage.failure !== null
  generation++
  again = false
  Object.assign(storage, fresh())
  if (wanted) void loadStorage()
}, { flush: 'sync' })

export async function loadStorage(): Promise<void> {
  if (storage.loading) { again = true; return }
  const at = generation
  storage.loading = true
  storage.failure = null
  asking = null
  try {
    const workspace = await settleWorkspace()
    if (at !== generation) return
    asking = workspace
    const usage = await authorized(token => getStorage(token, workspace))
    if (at !== generation) return
    storage.usage = usage
    storage.workspace = workspace
    storage.loaded = true
  } catch (error) {
    if (at === generation) storage.failure = error instanceof WorkspacesUnknown ? error.failure : failure('load-storage', error)
  } finally {
    if (at === generation) {
      storage.loading = false
      if (again) { again = false; void loadStorage() }
    }
  }
}
