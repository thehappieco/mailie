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
// A page whose address names the workspace it shows asks for that one
// (requestWorkspace), for itself only: it is shown once the list holds it,
// and nothing is remembered of it. Such a page may also turn remembering off
// altogether (rememberWorkspaceChoice), so that nothing of a workspace is
// written to storage while it is open: what a reload shows is its address.
//
// Until the list has been read the lists wait: reading them without
// ?workspace= would show every workspace's mailboxes together, a team's as if
// they were the person's own. Only a server without workspaces (404) lists
// everything. While the console is shown (retryWorkspaces), a first read that
// failed is tried again by itself.
//
// A read narrowed to the workspace shown that answers not_found says the
// workspace is no longer the person's under a session still live: a
// directory their workspaces come from replaced their personal one, or ended
// their membership (workspaceVanished). The list is read again, once for
// that workspace, and the one shown chosen again: the one remembered if it
// is still there, else the person's own; the one gone is kept for the notice
// that says so.
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
  /**
   * The workspace the page asked to show (requestWorkspace), until the list
   * holds it: then it is the one shown, and this is empty again. Never
   * remembered.
   */
  requested: string
  /**
   * The list was read and does not hold the workspace requested: it is not
   * one of the person's (or the server has none). The one shown was chosen as
   * ever meanwhile; the page says so rather than show it in its place. Set
   * only by a request the list answered, never when a workspace shown goes
   * away (lost says that).
   */
  requestUnknown: boolean
}

const fresh = (): WorkspacesState => ({
  list: [], loaded: false, loading: false, supported: false, failure: null, currentID: '', lost: null, requested: '', requestUnknown: false,
})

export const workspaces = reactive<WorkspacesState>(fresh())

let generation = 0
let pending: Promise<void> | null = null
/** The workspaces a narrowed read found gone, each read again for once. */
const vanished = new Set<string>()
/** The console is shown: a first read that failed is tried again. */
let retrying = false
/** Whether choosing a workspace remembers it for the person's next visit (rememberWorkspaceChoice). */
let remembering = true
let retryTimer: ReturnType<typeof setTimeout> | undefined
let retries = 0

watch(identity, () => {
  generation++
  pending = null
  clearTimeout(retryTimer)
  retryTimer = undefined
  retries = 0
  vanished.clear()
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
  workspaces.currentID = chooseWorkspace(list, [workspaces.requested, workspaces.currentID, readPersonPreference('workspace', identity()) ?? ''])
  if (before && before.id !== workspaces.currentID && !list.some(item => item.id === before.id)) workspaces.lost = before
  answerRequest()
}

/**
 * A read list answers the workspace requested: listed, it is the one shown
 * (adopt chose it first) and the request is done; else it is not the
 * person's, and stays asked for in case a later read holds it.
 */
function answerRequest(): void {
  const wanted = workspaces.requested
  if (!wanted) return
  if (workspaces.list.some(item => item.id === wanted)) {
    if (workspaces.currentID !== wanted) {
      workspaces.lost = null
      workspaces.currentID = wanted
    }
    workspaces.requested = ''
    workspaces.requestUnknown = false
  } else {
    workspaces.requestUnknown = true
  }
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
        // A server older than workspaces: nothing to narrow to, and nothing a page asks for is one.
        Object.assign(workspaces, { list: [], loaded: true, supported: false, currentID: '' })
        answerRequest()
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

/**
 * A read narrowed to workspace id answered not_found: the workspace may be
 * gone from under the session. When it is the one shown, and was not asked
 * about before, the list is read again and the one shown chosen again
 * (chooseWorkspace: the person's own when nothing remembered is left), the
 * one gone kept in lost. Answers whether another workspace is shown now,
 * which its lists are read for; false leaves the failure to the caller.
 */
export async function workspaceVanished(id: string): Promise<boolean> {
  if (!id || id !== workspaces.currentID || vanished.has(id)) return false
  vanished.add(id)
  const at = generation
  if (pending) await pending
  if (at !== generation || id !== workspaces.currentID) return at === generation
  await loadWorkspaces()
  return at === generation && workspaces.currentID !== id
}

/** The workspace shown, as listed. */
export function currentWorkspace(): Workspace | undefined {
  return workspaces.list.find(item => item.id === workspaces.currentID)
}

/**
 * Shows another of the person's workspaces, and remembers it for their next
 * visit, unless remembering is off (rememberWorkspaceChoice). A workspace a
 * page asked for (requestWorkspace) is no longer waited for: this one is
 * chosen instead.
 */
export function selectWorkspace(id: string): void {
  if (!workspaces.list.some(item => item.id === id)) return
  workspaces.lost = null
  workspaces.requested = ''
  workspaces.requestUnknown = false
  if (remembering) writePersonPreference('workspace', identity(), id)
  workspaces.currentID = id
}

/**
 * Shows the workspace the page's address names, for this page only: nothing
 * is remembered, and the next visit opens where it would have. Asked once the
 * person's workspaces are read, it is shown at once if they hold it; asked
 * before, once their list is read, and every list is read for it first. A
 * workspace the list does not hold is not the person's: the one shown is
 * chosen as ever meanwhile, requestUnknown says so, and it stays asked for (a
 * later read that holds it shows it). An empty id or null withdraws the
 * request, and so do choosing a workspace (selectWorkspace) and a change of
 * person: ask while signed in.
 */
export function requestWorkspace(id: string | null): void {
  workspaces.requested = id ?? ''
  workspaces.requestUnknown = false
  if (workspaces.loaded) answerRequest()
}

/**
 * Whether choosing a workspace (selectWorkspace, the person's choice or the
 * core's own: the workspace a mailbox was just linked into, the person's own
 * shown) remembers it for the person's next visit. On unless turned off. A
 * page whose address names the workspace it shows turns it off, so that
 * nothing of a workspace is written to storage while it is open; the one
 * remembered before is still read, to choose one when nothing else does.
 */
export function rememberWorkspaceChoice(remember: boolean): void {
  remembering = remember
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
