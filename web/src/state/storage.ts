// What the signed-in person's mailboxes take up in this server's index, as
// the Storage section shows it. Read when the section is first shown, on
// Refresh, and when what it counts changed; it resets when the person
// changes, and a slow answer for one person is never drawn in front of the
// next. Asked again while a read is in flight, it reads once more when that
// one ends: the answer in flight may predate the change.

import { reactive, watch } from 'vue'
import { getStorage } from '../api/storage'
import type { Storage } from '../api/types'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

interface StorageState {
  loaded: boolean
  loading: boolean
  failure: Failure | null
  /** The last answer; null until there is one. */
  usage: Storage | null
}

const fresh = (): StorageState => ({ loaded: false, loading: false, failure: null, usage: null })

export const storage = reactive<StorageState>(fresh())

let generation = 0
/** Asked for while a read was in flight: read again once it ends. */
let again = false
watch(identity, () => {
  generation++
  again = false
  Object.assign(storage, fresh())
}, { flush: 'sync' })

export async function loadStorage(): Promise<void> {
  if (storage.loading) { again = true; return }
  const at = generation
  storage.loading = true
  storage.failure = null
  try {
    const usage = await authorized(token => getStorage(token))
    if (at !== generation) return
    storage.usage = usage
    storage.loaded = true
  } catch (error) {
    if (at === generation) storage.failure = failure('load-storage', error)
  } finally {
    if (at === generation) {
      storage.loading = false
      if (again) { again = false; void loadStorage() }
    }
  }
}
