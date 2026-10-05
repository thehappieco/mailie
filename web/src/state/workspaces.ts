// The workspaces the signed-in person belongs to, and the one the console
// shows (docs/workspaces.md).
//
// The server has no current workspace: a session reaches the person's
// mailboxes in every workspace they belong to. The console keeps one itself,
// in memory, for this tab, and every list it reads (mailboxes, storage, the
// event stream) narrows to it with ?workspace=, so a view never reads another
// workspace's data. The last one chosen is remembered as a preference of this
// host and of this person (ui/preferences.ts, keyed by their user id: someone
// else signing in on the same browser does not open on it), checked against
// the list the server answers: a workspace the person is no longer in is
// never asked for.
//
// Until the list has been read the lists wait: reading them without
// ?workspace= would show every workspace's mailboxes together, a team's as if
// they were the person's own. Only a server without workspaces (404) lists
// everything. While the console is shown (retryWorkspaces), a first read that
// failed is tried again by itself.
//
// Everything resets when the person changes, as in the other stores.

import { reactive, watch } from 'vue'
import { ApiError } from '../api/http'
import * as api from '../api/workspaces'
import type { Workspace } from '../api/types'
import { readPersonPreference, writePersonPreference } from '../ui/preferences'
import { failure, type Failure } from './failure'
import { authorized, identity } from './session'

/** How long a first read that failed waits before it is tried again, doubling up to WORKSPACES_RETRY_MAX_MS. */
export const WORKSPACES_RETRY_MS = 2_000
export const WORKSPACES_RETRY_MAX_MS = 30_000

interface WorkspacesState {
  list: Workspace[]
  /** The list was read, or the server turned out to have no workspaces (supported false). */
  loaded: boolean
  loading: boolean
  /** The server has workspaces. One older than them answers 404, and every list is then the person's whole. */
  supported: boolean
  failure: Failure | null
  /** The workspace the console shows. Empty until it is known, and on a server without workspaces. */
  currentID: string
  /** The workspace shown last, gone from the list since (the person left it, or was removed): for the notice that says so. */
  lost: Workspace | null
}

const fresh = (): WorkspacesState => ({ list: [], loaded: false, loading: false, supported: false, failure: null, currentID: '', lost: null })

export const workspaces = reactive<WorkspacesState>(fresh())

let generation = 0
let pending: Promise<void> | null = null
/** The console is shown: a first read that failed is tried again. */
let retrying = false
let retryTimer: ReturnType<typeof setTimeout> | undefined
let retries = 0

watch(identity, () => {
  generation++
  pending = null
  clearTimeout(retryTimer)
  retryTimer = undefined
  retries = 0
  Object.assign(workspaces, fresh())
}, { flush: 'sync' })

/**
 * Why a list waits: the person's workspaces could not be read, so the one to
 * narrow it to is not known. failure is the read's, which says so.
 */
export class WorkspacesUnknown extends Error {
  constructor(readonly failure: Failure) {
    super('workspaces: not read yet')
    this.name = 'WorkspacesUnknown'
  }
}

/**
 * The one to show from a list: the one shown now if it is still there, else
 * the one remembered, else the person's own, else the first.
 */
export function chooseWorkspace(list: readonly Workspace[], preferred: readonly string[]): string {
  for (const id of preferred) if (id && list.some(item => item.id === id)) return id
  return list.find(item => item.kind === 'personal')?.id ?? list[0]?.id ?? ''
}

function adopt(list: Workspace[]): void {
  clearTimeout(retryTimer)
  retryTimer = undefined
  retries = 0
  const before = workspaces.list.find(item => item.id === workspaces.currentID)
  workspaces.list = list
  workspaces.loaded = true
  workspaces.supported = true
  workspaces.currentID = chooseWorkspace(list, [workspaces.currentID, readPersonPreference('workspace', identity()) ?? ''])
  if (before && before.id !== workspaces.currentID && !list.some(item => item.id === before.id)) workspaces.lost = before
}

/**
 * Reads the person's workspaces. One read at a time: a caller arriving while
 * one is in flight waits for it. A failure leaves the workspace shown as it
 * was (none, the first time, and the lists wait), and says so where the
 * switcher is.
 */
export function loadWorkspaces(): Promise<void> {
  if (pending) return pending
  const at = generation
  workspaces.loading = true
  workspaces.failure = null
  const read = (async () => {
    try {
      const list = await authorized(token => api.listWorkspaces(token))
      if (at === generation) adopt(list)
    } catch (error) {
      if (at !== generation) return
      if (error instanceof ApiError && error.code === 'not_found') {
        // A server older than workspaces: nothing to narrow to.
        Object.assign(workspaces, { list: [], loaded: true, supported: false, currentID: '' })
        return
      }
      workspaces.failure = failure('load-workspaces', error)
      if (!workspaces.loaded) retryLater(at, error)
    } finally {
      if (at === generation) {
        workspaces.loading = false
        pending = null
      }
    }
  })()
  pending = read
  return read
}

/** Codes worth asking again for: no answer, a failure on the server's side, or a rate limit. */
const passing = new Set<string>(['unavailable', 'internal', 'rate_limited', 'invalid_response'])

/** Schedules the next attempt at a first read that failed, while the console is shown. */
function retryLater(at: number, error: unknown): void {
  const code = error instanceof ApiError ? error.code : 'internal'
  if (!retrying || !passing.has(code)) return
  clearTimeout(retryTimer)
  const asked = error instanceof ApiError && error.code === 'rate_limited' ? (error.retryAfter ?? 0) * 1000 : 0
  const delay = Math.max(asked, Math.min(WORKSPACES_RETRY_MAX_MS, WORKSPACES_RETRY_MS * 2 ** Math.min(retries++, 10)))
  retryTimer = setTimeout(() => {
    retryTimer = undefined
    if (at === generation && retrying && !workspaces.loaded) void loadWorkspaces()
  }, delay)
}

/**
 * While the console is shown: a first read of the workspaces that failed is
 * tried again by itself, backing off (2 s doubling to 30 s, or as long as a
 * rate limit asks), since every list waits for it. Returns what stops it.
 */
export function retryWorkspaces(): () => void {
  retrying = true
  if (!workspaces.loaded && !workspaces.loading && workspaces.failure) void loadWorkspaces()
  return () => {
    retrying = false
    clearTimeout(retryTimer)
    retryTimer = undefined
  }
}

/**
 * The workspace every list narrows to, once it is known: the list is read
 * first when it has not been. Empty on a server without workspaces. When the
 * list could not be read it throws WorkspacesUnknown: a list read without
 * ?workspace= would show every workspace's mailboxes together, so the lists
 * wait, and are read once the workspace shown is known.
 */
export async function settleWorkspace(): Promise<string> {
  if (!workspaces.loaded) await loadWorkspaces()
  if (!workspaces.loaded) throw new WorkspacesUnknown(workspaces.failure ?? { op: 'load-workspaces', code: 'internal' })
  return workspaces.currentID
}

/** The workspace shown, as listed. */
export function currentWorkspace(): Workspace | undefined {
  return workspaces.list.find(item => item.id === workspaces.currentID)
}

/** Shows another of the person's workspaces, and remembers it for their next visit. */
export function selectWorkspace(id: string): void {
  if (!workspaces.list.some(item => item.id === id)) return
  workspaces.lost = null
  writePersonPreference('workspace', identity(), id)
  workspaces.currentID = id
}

/** Says nothing more about a workspace that went away. */
export function dismissLost(): void {
  workspaces.lost = null
}

/**
 * The caller's own role and status in a workspace, as another answer named
 * them (their own row in its members): what the console offers there follows
 * it at once.
 */
export function adoptOwnPlace(id: string, role: string, status: string): void {
  const index = workspaces.list.findIndex(item => item.id === id)
  const known = workspaces.list[index]
  if (!known || (known.role === role && known.status === status)) return
  workspaces.list.splice(index, 1, { ...known, role, status })
}

/** A workspace the server answered for (created, renamed, joined): the list holds its latest word. */
export function upsertWorkspace(workspace: Workspace): void {
  const index = workspaces.list.findIndex(item => item.id === workspace.id)
  if (index >= 0) workspaces.list.splice(index, 1, workspace)
  else workspaces.list.push(workspace)
}

/** Creates a team, whose owner the person becomes, and shows it. */
export async function createTeam(name: string): Promise<Failure | null> {
  const at = generation
  try {
    const team = await authorized(token => api.createWorkspace(token, name))
    if (at !== generation) return null
    upsertWorkspace(team)
    selectWorkspace(team.id)
    return null
  } catch (error) {
    return failure('create-team', error)
  }
}

/** Renames a team. */
export async function renameTeam(id: string, name: string): Promise<Failure | null> {
  const at = generation
  try {
    const team = await authorized(token => api.renameWorkspace(token, id, name))
    if (at === generation) upsertWorkspace(team)
    return null
  } catch (error) {
    const found = failure('rename-team', error)
    // The person's role changed elsewhere: what they may do here follows it.
    if (at === generation && found.code === 'not_authorized') void loadWorkspaces()
    return found
  }
}
