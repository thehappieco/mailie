// The HTTP contract (SPEC §4), as types and as the checks that hold a reply to
// them. The checks run on every reply the console reads, and test/contract
// runs them in strict mode over the JSON the Go handlers actually produce, so
// a field renamed on one side fails a test instead of a screen.
//
// Strict mode also refuses keys this file does not know; the running console
// tolerates them, because a daemon adding a field is not a daemon breaking one.

import { counter, filled, flag, headerText, known, oneOf, optional, record, rowID, seconds, text, type Fields } from './checks'
import { serverCodes, type ServerCode } from './http'

export type Role = 'owner' | 'member'
/**
 * icloud is stored by the daemon as an IMAP account with Apple's servers and
 * presented as its own provider; the console never sees the difference.
 */
export type ProviderID = 'gmail' | 'microsoft' | 'icloud' | 'imap'
export type AuthKind = 'oauth2' | 'password'
export type AccountState = 'pending_auth' | 'active' | 'needs_reauth' | 'disabled' | 'error'
export type FlowKind = 'web' | 'loopback' | 'pasted' | 'device'
export type SMTPSecurity = 'implicit' | 'starttls'

export interface User { id: string; email: string; name: string; role: Role; created_at: number }
/** What sign-in, sign-up and a password change answer: a fresh bearer token. */
export interface SessionReply { token: string; expires_at: number; user: User }
export interface SessionInfo { id: string; created_at: number; expires_at: number }
export interface Me { user: User; session: SessionInfo }
export interface Account {
  id: string
  email: string
  display_name?: string
  provider: ProviderID
  auth_kind: AuthKind
  state: AccountState
  state_reason?: string
  sync_tier?: string
  save_sent_copy: boolean
  last_ok_at?: number
  last_error?: string
  created_at: number
  /** Where the account's sync stands; the same block GET /v1/accounts/{id}/sync answers. */
  sync: AccountSync
  /**
   * Which moves the mailbox has somewhere to make. Absent from a daemon older
   * than actions, which then offers neither.
   */
  actions?: AccountActions
  /**
   * Whether Mailie can send from the mailbox, when the daemon says. Absent,
   * a mailbox that works can send; the server decides either way.
   */
  send?: AccountSend
  /** The workspace the mailbox belongs to. Absent from a daemon older than workspaces. */
  workspace_id?: string
  /** The person who linked it, under whose consent to sync it syncs; absent for the operator's mailboxes. */
  linked_by?: string
  /**
   * What the caller may do with the mailbox: their grant, as far as their
   * credential reaches. Absent from a daemon older than workspaces; the
   * server decides either way.
   */
  access?: AccountAccess
}
/** A caller's grant on a mailbox: read its index, act on its messages, send from it, manage it. */
export interface AccountAccess { read: boolean; act: boolean; send: boolean; manage: boolean }
/**
 * available false: the mailbox cannot send now, for a short reason (such as
 * needs_reauth, or no SMTP server) the console never shows as it is.
 */
export interface AccountSend {
  available: boolean
  reason?: string
  /** The name its messages go out under: the owner's profile name. Absent: the address alone. */
  from_name?: string
}
/**
 * archive: the account has an archive target (Gmail's All Mail, or a folder
 * marked Archive); trash: a Trash folder. archive_reason: why archive is
 * false, when it is something the person can change; absent otherwise, and
 * from a daemon older than it.
 */
export interface AccountActions { archive: boolean; trash: boolean; archive_reason?: ArchiveReason | (string & {}) }
/**
 * all_mail_hidden: the mailbox archives to Gmail's All Mail, and its Show in
 * IMAP setting hides it. The running console reads any other reason as none;
 * the contract check holds the daemon to these.
 */
export type ArchiveReason = 'all_mail_hidden'
/**
 * The sync states the daemon reports: off (not permitted, account not active,
 * or no engine), initial (the first pass over the last 90 days), live,
 * backoff (waiting to retry) and stopped. The running console reads any other
 * as text; the contract check holds the daemon to these.
 */
export type SyncState = 'off' | 'initial' | 'live' | 'backoff' | 'stopped'
export interface AccountSync {
  /** The owner agreed to sync (or, for a mailbox nobody owns, the operator switched it on). */
  enabled: boolean
  running: boolean
  state: SyncState | (string & {})
  tier?: string
  folders_synced: number
  folders_total: number
  messages: number
  /** 0..100 while the initial sync runs, 100 after. */
  initial_progress: number
  last_synced_at?: number
  /** A short fixed class (needs_reauth, rate_limited, …), never the server's words. */
  error_class?: string
  next_retry_at?: number
}
/** A person's answer about sync, and the revision of the text the server asks about. */
export interface SyncConsent { consented: boolean; version?: string; consented_at?: number; current_version: string }
/** A person's answer about actions on their messages: the same shape, its own revision. */
export type ActionsConsent = SyncConsent
/** One journal entry, as the event stream's data lines and the long poll both carry it. */
export interface ServerEvent { seq: number; type: string; account_id: string; at: number; payload: Record<string, unknown> }
export interface WaitResult { timed_out: boolean; lagged: boolean; events: ServerEvent[]; next_cursor: number }
/** message.new's payload. from is null when the header had no sender. */
export interface MessageNew {
  account_id: string
  message_id: number
  folder_id: number
  folder_role: string
  subject: string
  from: { name?: string; email: string } | null
  internal_date: number
  first_copy: boolean
  first_inbox_copy: boolean
}
export interface AuthFlow {
  flow: FlowKind
  auth_url?: string
  state?: string
  user_code?: string
  verification_uri?: string
  expires_at?: number
}
export interface AddAccountResult { account: Account; auth?: AuthFlow }
export interface Provider { id: ProviderID; oauth: boolean; password: boolean; flows: FlowKind[] }
export interface Folder {
  /**
   * The folder's id in Mailie's index: what GET /v1/messages filters by.
   * Present only when the listing comes from the index; a live listing has
   * nothing indexed to filter.
   */
  id?: number
  name: string
  display_name: string
  role?: string
  role_source?: string
  selectable: boolean
  synced: boolean
  messages?: number
  unseen?: number
  /** Present only when the listing comes from the index: new, initial, live, resync, error or disabled. */
  sync_state?: string
}
export interface Invite { email: string; role: Role; url: string; expires_at: number }
export interface ErrorBody { code: ServerCode; message: string }
/**
 * What a person may give one of their own API keys: read (search and read),
 * or write (read, and the actions on messages their consent allows).
 */
export type KeyScope = 'read' | 'write'
/** How long a new key lives, in days: the three lifetimes POST /v1/me/apikeys takes. */
export type KeyLifetime = 30 | 90 | 365
/**
 * One of the person's API keys, as GET /v1/me/apikeys lists it. Never the
 * secret: that exists once, in the answer to the call that created the key.
 * account_ids empty: every mailbox of the person, including the ones
 * connected later, unless restricted says the key was made for chosen
 * mailboxes, all removed since (the server revoked it with the last).
 * terms_version: the revision of the text the person agreed to when they
 * created it; empty for a key an administrator made for them, which the
 * list includes, as it does revoked and expired keys.
 */
export interface PersonalKey {
  prefix: string
  name: string
  /** A person's keys are read or write; the running console reads another scope as its identifier. */
  scope: KeyScope | (string & {})
  account_ids?: string[]
  /** Made for the mailboxes chosen, not for all of them. Left out, account_ids alone says. */
  restricted?: boolean
  created_at: number
  expires_at: number
  last_used_at?: number
  revoked_at?: number
  terms_version: string
}
/** The answer to creating a key: the key as listed, and its secret, "<prefix>.<secret>", this once. */
export interface CreatedKey extends PersonalKey { key: string }
/**
 * What one of the caller's mailboxes takes up in the index, as GET
 * /v1/me/storage reports it: the messages indexed (a copy in each folder it
 * is in counts once in each) and their size on the mail server, not what the
 * index keeps of them. Zeros for a mailbox that does not sync.
 */
export interface MailboxStorage {
  account_id: string
  /** The workspace the mailbox belongs to. Absent from a daemon older than workspaces. */
  workspace_id?: string
  email: string
  messages: number
  bytes: number
}
/** The caller's readable mailboxes of one workspace, summed: never a workspace's whole usage. */
export interface WorkspaceStorage { workspace_id: string; mailboxes: number; messages: number; bytes: number }
/**
 * GET /v1/me/storage: the caller's own mailboxes and their sum, and the size
 * of the database file on disk (everyone's, the write-ahead log included),
 * which only an owner signed in to the console is told.
 */
export interface Storage {
  mailboxes: MailboxStorage[]
  /** mailboxes summed per workspace. Absent from a daemon older than workspaces. */
  workspaces?: WorkspaceStorage[]
  total: { messages: number; bytes: number }
  database_bytes?: number
}
/**
 * GET /v1/me/mcp: whether this server answers MCP over HTTP at /mcp
 * (MAIL_MCP_HTTP). Off, the console shows no MCP address: /mcp answers 404.
 */
export interface McpAccess { http: boolean }
/** The body of POST /v1/me/apikeys. account_ids is left out for every mailbox. */
export interface CreateKeyRequest {
  name: string
  scope: KeyScope
  account_ids?: string[]
  ttl_days: KeyLifetime
  terms_version: string
}

/** The body of POST /v1/accounts. Unknown fields are refused by the daemon, so only these are ever sent. */
export interface AddAccountRequest {
  email: string
  display_name?: string
  provider: ProviderID
  /** The workspace to link it into: a team the caller owns or administers. Left out, the caller's personal workspace. */
  workspace_id?: string
  password?: string
  imap_host?: string
  imap_port?: number
  smtp_host?: string
  smtp_port?: number
  smtp_tls?: SMTPSecurity
  login_user?: string
  flow?: FlowKind
}

/**
 * Where a mailbox belongs (docs/workspaces.md): a person's own (personal),
 * one shared by people (team), or the one the operator's instance keys reach
 * (operator), which no person is ever a member of.
 */
export type WorkspaceKind = 'personal' | 'team' | 'operator'
/**
 * Where a workspace and its memberships come from: created and changed here
 * (local), or mirrored from elsewhere and never changed here (platform).
 */
export type WorkspaceSource = 'local' | 'platform'
/** A person's role in a workspace: owners and admins administer people and grants, and get no read by it. */
export type WorkspaceRole = 'owner' | 'admin' | 'member'
/** A membership that is disabled is listed and reaches nothing. */
export type MemberStatus = 'active' | 'disabled'

/**
 * A workspace as the caller sees it (GET /v1/workspaces). role and status
 * are the caller's own membership; name is a team's, and empty for a
 * personal workspace, which the console names itself.
 */
export interface Workspace {
  id: string
  kind: WorkspaceKind | (string & {})
  source: WorkspaceSource | (string & {})
  name: string
  role?: WorkspaceRole | (string & {})
  status?: MemberStatus | (string & {})
  /** Counts, in the operator's listing only. */
  members?: number
  mailboxes?: number
  created_at: number
}
/**
 * A membership of a workspace. last_owner and links mark the protections in
 * advance: the last active owner is never demoted, disabled or removed, and
 * a person who linked mailboxes there stays while any of them is linked.
 */
export interface Member {
  user_id: string
  email: string
  name: string
  role: WorkspaceRole | (string & {})
  status: MemberStatus | (string & {})
  /** Switched off on the server: the membership counts for nothing until they are back. */
  person_disabled?: boolean
  last_owner: boolean
  links: number
  joined_at: number
}
/** What one person holds on one mailbox. act never comes without read. */
export interface Grant {
  account_id: string
  user_id: string
  read: boolean
  act: boolean
  send: boolean
  manage: boolean
  /** Who set it last: a person's id, a key's, or the migration's; absent once that person is deleted. */
  granted_by?: string
  updated_at: number
}
/**
 * One mailbox of a workspace and who holds what on it: the access directory,
 * addresses and grants, never what the mailbox holds. linked_by is the person
 * it syncs under, whose grant nobody changes while it is linked.
 */
export interface MailboxAccess {
  account_id: string
  email: string
  provider: ProviderID | (string & {})
  state: AccountState | (string & {})
  linked_by?: string
  grants: Grant[]
}
/**
 * An invitation into a team, as its owners and admins see it. url, the link
 * to send, is in the answer that creates it and nowhere else.
 */
export interface TeamInvite {
  id: string
  email: string
  workspace_id: string
  role: WorkspaceRole | (string & {})
  url?: string
  created_by?: string
  created_at: number
  expires_at: number
}
/** The four flags of a grant, as a request sets them: exactly these. */
export interface GrantFlags { read: boolean; act: boolean; send: boolean; manage: boolean }
/** A change to a membership: a field left out stays as it is. */
export interface MemberChange { role?: WorkspaceRole; status?: MemberStatus }

export const roles: readonly Role[] = ['owner', 'member']
export const workspaceKinds: readonly WorkspaceKind[] = ['personal', 'team', 'operator']
export const workspaceSources: readonly WorkspaceSource[] = ['local', 'platform']
export const workspaceRoles: readonly WorkspaceRole[] = ['owner', 'admin', 'member']
export const memberStatuses: readonly MemberStatus[] = ['active', 'disabled']
export const providerIDs: readonly ProviderID[] = ['gmail', 'microsoft', 'icloud', 'imap']
export const authKinds: readonly AuthKind[] = ['oauth2', 'password']
export const accountStates: readonly AccountState[] = ['pending_auth', 'active', 'needs_reauth', 'disabled', 'error']
export const flowKinds: readonly FlowKind[] = ['web', 'loopback', 'pasted', 'device']
export const syncStates: readonly SyncState[] = ['off', 'initial', 'live', 'backoff', 'stopped']
export const archiveReasons: readonly ArchiveReason[] = ['all_mail_hidden']
export const keyScopes: readonly KeyScope[] = ['read', 'write']
export const keyLifetimes: readonly KeyLifetime[] = [30, 90, 365]
/** internal/events: the closed vocabulary of the journal. */
export const eventTypes = ['message.new', 'message.flags', 'message.moved', 'message.deleted', 'folder.changed', 'account.state', 'send.finished', 'sync.progress'] as const
export const folderSyncStates = ['new', 'initial', 'live', 'resync', 'error', 'disabled'] as const

export function isUser(v: unknown, strict = false): v is User {
  return record(v) && known(v, ['id', 'email', 'name', 'role', 'created_at'], strict)
    && filled(v.id, 64) && filled(v.email, 320) && text(v.name, 1024) && oneOf(roles)(v.role) && seconds(v.created_at)
}

/** Bearer tokens are opaque here; the bounds only keep a corrupt value out of a header. */
export function isToken(v: unknown): v is string {
  return filled(v, 4096) && !/\s/.test(v)
}

export function isSessionReply(v: unknown, strict = false): v is SessionReply {
  return record(v) && known(v, ['token', 'expires_at', 'user'], strict)
    && isToken(v.token) && seconds(v.expires_at) && isUser(v.user, strict)
}

export function isMe(v: unknown, strict = false): v is Me {
  if (!record(v) || !known(v, ['user', 'session'], strict) || !isUser(v.user, strict)) return false
  const s = v.session
  return record(s) && known(s, ['id', 'created_at', 'expires_at'], strict)
    && filled(s.id, 64) && seconds(s.created_at) && seconds(s.expires_at)
}

export function isAccount(v: unknown, strict = false): v is Account {
  return record(v) && known(v, ['id', 'email', 'display_name', 'provider', 'auth_kind', 'state', 'state_reason', 'sync_tier', 'save_sent_copy', 'last_ok_at', 'last_error', 'created_at', 'sync', 'actions', 'send', 'workspace_id', 'linked_by', 'access'], strict)
    && filled(v.id, 64) && filled(v.email, 320) && optional(v.display_name, x => text(x, 1024))
    && oneOf(providerIDs)(v.provider) && oneOf(authKinds)(v.auth_kind) && oneOf(accountStates)(v.state)
    && optional(v.state_reason, text) && optional(v.sync_tier, x => text(x, 64)) && flag(v.save_sent_copy)
    && optional(v.last_ok_at, seconds) && optional(v.last_error, text) && seconds(v.created_at)
    && isAccountSync(v.sync, strict)
    // The daemon that writes the fixtures always says; an older one may not.
    && (strict ? isAccountActions(v.actions, strict) : optional(v.actions, x => isAccountActions(x)))
    && optional(v.send, x => isAccountSend(x, strict))
    && (strict ? filled(v.workspace_id, 64) && isAccountAccess(v.access, strict)
      : optional(v.workspace_id, x => filled(x, 64)) && optional(v.access, x => isAccountAccess(x)))
    && optional(v.linked_by, x => filled(x, 64))
}

export function isAccountAccess(v: unknown, strict = false): v is AccountAccess {
  return record(v) && known(v, ['read', 'act', 'send', 'manage'], strict)
    && flag(v.read) && flag(v.act) && flag(v.send) && flag(v.manage)
}

export function isAccountSend(v: unknown, strict = false): v is AccountSend {
  // A reason is only ever why a mailbox cannot send.
  return record(v) && known(v, ['available', 'reason', 'from_name'], strict) && flag(v.available)
    && optional(v.reason, x => filled(x, 64)) && (!strict || v.reason === undefined || v.available === false)
    && optional(v.from_name, x => filled(x, 256))
}

export function isAccountActions(v: unknown, strict = false): v is AccountActions {
  return record(v) && known(v, ['archive', 'trash', 'archive_reason'], strict) && flag(v.archive) && flag(v.trash)
    // A reason is only ever why there is no archive.
    && (strict ? optional(v.archive_reason, oneOf(archiveReasons)) && (v.archive_reason === undefined || v.archive === false)
      : optional(v.archive_reason, x => filled(x, 64)))
}

export function isAccountSync(v: unknown, strict = false): v is AccountSync {
  return record(v) && known(v, ['enabled', 'running', 'state', 'tier', 'folders_synced', 'folders_total', 'messages', 'initial_progress', 'last_synced_at', 'error_class', 'next_retry_at'], strict)
    && flag(v.enabled) && flag(v.running) && (strict ? oneOf(syncStates)(v.state) : filled(v.state, 32))
    && optional(v.tier, x => text(x, 32)) && counter(v.folders_synced) && counter(v.folders_total) && counter(v.messages)
    && counter(v.initial_progress) && v.initial_progress <= 100
    && optional(v.last_synced_at, seconds) && optional(v.error_class, x => text(x, 64)) && optional(v.next_retry_at, seconds)
}

export function isSyncConsent(v: unknown, strict = false): v is SyncConsent {
  return record(v) && known(v, ['consented', 'version', 'consented_at', 'current_version'], strict)
    && flag(v.consented) && optional(v.version, x => text(x, 128)) && optional(v.consented_at, seconds) && filled(v.current_version, 128)
}

export function isServerEvent(v: unknown, strict = false): v is ServerEvent {
  return record(v) && known(v, ['seq', 'type', 'account_id', 'at', 'payload'], strict)
    && counter(v.seq) && v.seq > 0 && (strict ? oneOf(eventTypes)(v.type) : filled(v.type, 64))
    && filled(v.account_id, 64) && seconds(v.at) && record(v.payload)
}

export function isWaitResult(v: unknown, strict = false): v is WaitResult {
  return record(v) && known(v, ['timed_out', 'lagged', 'events', 'next_cursor'], strict)
    && flag(v.timed_out) && flag(v.lagged) && counter(v.next_cursor)
    && Array.isArray(v.events) && v.events.every(item => isServerEvent(item, strict))
}

export function isMessageNew(v: unknown, strict = false): v is MessageNew {
  if (!record(v) || !known(v, ['account_id', 'message_id', 'folder_id', 'folder_role', 'subject', 'from', 'internal_date', 'first_copy', 'first_inbox_copy'], strict)) return false
  const from = v.from
  const sender = from === null || (record(from) && known(from, ['name', 'email'], strict) && optional(from.name, headerText) && headerText(from.email))
  return filled(v.account_id, 64) && counter(v.message_id) && counter(v.folder_id) && text(v.folder_role, 32) && headerText(v.subject)
    && sender && seconds(v.internal_date) && flag(v.first_copy) && flag(v.first_inbox_copy)
}

export function isAccountList(v: unknown): v is Account[] {
  return Array.isArray(v) && v.every(item => isAccount(item))
}

export function isAuthFlow(v: unknown, strict = false): v is AuthFlow {
  return record(v) && known(v, ['flow', 'auth_url', 'state', 'user_code', 'verification_uri', 'expires_at'], strict)
    && oneOf(flowKinds)(v.flow) && optional(v.auth_url, x => filled(x, 8192)) && optional(v.state, x => filled(x, 512))
    && optional(v.user_code, x => filled(x, 64)) && optional(v.verification_uri, x => filled(x, 2048)) && optional(v.expires_at, seconds)
}

export function isAddAccountResult(v: unknown, strict = false): v is AddAccountResult {
  return record(v) && known(v, ['account', 'auth'], strict) && isAccount(v.account, strict) && optional(v.auth, x => isAuthFlow(x, strict))
}

export function isProvider(v: unknown, strict = false): v is Provider {
  return record(v) && known(v, ['id', 'oauth', 'password', 'flows'], strict)
    && oneOf(providerIDs)(v.id) && flag(v.oauth) && flag(v.password)
    && Array.isArray(v.flows) && v.flows.every(oneOf(flowKinds))
}

export function isProviderList(v: unknown, strict = false): v is Provider[] {
  return Array.isArray(v) && v.every(item => isProvider(item, strict))
}

export function isFolder(v: unknown, strict = false): v is Folder {
  return record(v) && known(v, ['id', 'name', 'display_name', 'role', 'role_source', 'selectable', 'synced', 'messages', 'unseen', 'sync_state'], strict)
    && optional(v.id, rowID) && filled(v.name, 2048) && text(v.display_name, 2048) && optional(v.role, x => text(x, 64)) && optional(v.role_source, x => text(x, 64))
    && flag(v.selectable) && flag(v.synced) && optional(v.messages, counter) && optional(v.unseen, counter)
    && optional(v.sync_state, x => strict ? oneOf(folderSyncStates)(x) : text(x, 32))
}

export function isFolderList(v: unknown): v is Folder[] {
  return Array.isArray(v) && v.every(item => isFolder(item))
}

export function isInvite(v: unknown, strict = false): v is Invite {
  return record(v) && known(v, ['email', 'role', 'url', 'expires_at'], strict)
    && filled(v.email, 320) && oneOf(roles)(v.role) && filled(v.url, 4096) && seconds(v.expires_at)
}

const keyFields = ['prefix', 'name', 'scope', 'account_ids', 'restricted', 'created_at', 'expires_at', 'last_used_at', 'revoked_at', 'terms_version'] as const

/**
 * A key's prefix names it in a path (DELETE /v1/me/apikeys/{prefix}), so the
 * running console holds it to a short run without a separator; the contract
 * check, to the hex the daemon issues.
 */
const keyPrefix = (v: unknown, strict: boolean): v is string => filled(v, 64) && (strict ? /^[0-9a-f]+$/.test(v) : /^[^\s./\\]+$/.test(v))

const accountIDs = (v: unknown): v is string[] => Array.isArray(v) && v.every(id => filled(id, 64))

function keyListed(v: Fields, strict: boolean): boolean {
  return keyPrefix(v.prefix, strict) && filled(v.name, 1024) && (strict ? oneOf(keyScopes)(v.scope) : filled(v.scope, 32))
    // The daemon always names them, [] for every mailbox; an older answer may leave them out.
    && (strict ? accountIDs(v.account_ids) : optional(v.account_ids, accountIDs)) && optional(v.restricted, flag)
    && seconds(v.created_at) && seconds(v.expires_at) && optional(v.last_used_at, seconds) && optional(v.revoked_at, seconds)
    && text(v.terms_version, 128)
}

export function isPersonalKey(v: unknown, strict = false): v is PersonalKey {
  return record(v) && known(v, keyFields, strict) && keyListed(v, strict)
}

export function isPersonalKeyList(v: unknown, strict = false): v is PersonalKey[] {
  return Array.isArray(v) && v.every(item => isPersonalKey(item, strict))
}

export function isMailboxStorage(v: unknown, strict = false): v is MailboxStorage {
  return record(v) && known(v, ['account_id', 'workspace_id', 'email', 'messages', 'bytes'], strict)
    && filled(v.account_id, 64) && filled(v.email, 320) && counter(v.messages) && counter(v.bytes)
    // The daemon that writes the fixtures always says; an older one may not.
    && (strict ? filled(v.workspace_id, 64) : optional(v.workspace_id, x => filled(x, 64)))
}

export function isWorkspaceStorage(v: unknown, strict = false): v is WorkspaceStorage {
  return record(v) && known(v, ['workspace_id', 'mailboxes', 'messages', 'bytes'], strict)
    && filled(v.workspace_id, 64) && counter(v.mailboxes) && counter(v.messages) && counter(v.bytes)
}

export function isStorage(v: unknown, strict = false): v is Storage {
  const workspaces = (x: unknown) => Array.isArray(x) && x.every(item => isWorkspaceStorage(item, strict))
  return record(v) && known(v, ['mailboxes', 'workspaces', 'total', 'database_bytes'], strict)
    && Array.isArray(v.mailboxes) && v.mailboxes.every(item => isMailboxStorage(item, strict))
    && (strict ? workspaces(v.workspaces) : optional(v.workspaces, workspaces))
    && record(v.total) && known(v.total, ['messages', 'bytes'], strict) && counter(v.total.messages) && counter(v.total.bytes)
    && optional(v.database_bytes, counter)
}

export function isMcpAccess(v: unknown, strict = false): v is McpAccess {
  return record(v) && known(v, ['http'], strict) && flag(v.http)
}

/** The one answer that carries a secret: "<prefix>.<secret>", under the prefix it is listed by. */
export function isCreatedKey(v: unknown, strict = false): v is CreatedKey {
  return record(v) && known(v, [...keyFields, 'key'], strict) && keyListed(v, strict)
    && isToken(v.key) && v.key.length <= 512 && v.key.startsWith(`${v.prefix as string}.`) && v.key.length > (v.prefix as string).length + 1
    && (!strict || /^[0-9a-f]+\.[A-Za-z0-9_-]+$/.test(v.key))
}

/** An identifier the console puts in a path: bounded, and never a separator. */
const pathID = (v: unknown): v is string => filled(v, 64) && !/[\s/\\?#]/.test(v)
/** A closed vocabulary: the contract check holds the daemon to it; the running console reads another word as text. */
const word = <T extends string>(options: readonly T[], strict: boolean) => (v: unknown): boolean => strict ? oneOf(options)(v) : filled(v, 32)

export function isWorkspace(v: unknown, strict = false): v is Workspace {
  return record(v) && known(v, ['id', 'kind', 'source', 'name', 'role', 'status', 'members', 'mailboxes', 'created_at'], strict)
    && pathID(v.id) && word(workspaceKinds, strict)(v.kind) && word(workspaceSources, strict)(v.source) && text(v.name, 1024)
    && optional(v.role, word(workspaceRoles, strict)) && optional(v.status, word(memberStatuses, strict))
    && optional(v.members, counter) && optional(v.mailboxes, counter) && seconds(v.created_at)
    // A team has a name; the others are named by the console.
    && (!strict || (v.kind === 'team') === (v.name !== ''))
}

export function isWorkspaceList(v: unknown, strict = false): v is Workspace[] {
  return Array.isArray(v) && v.every(item => isWorkspace(item, strict))
}

export function isMember(v: unknown, strict = false): v is Member {
  return record(v) && known(v, ['user_id', 'email', 'name', 'role', 'status', 'person_disabled', 'last_owner', 'links', 'joined_at'], strict)
    && pathID(v.user_id) && filled(v.email, 320) && text(v.name, 1024) && word(workspaceRoles, strict)(v.role)
    && word(memberStatuses, strict)(v.status) && optional(v.person_disabled, flag) && flag(v.last_owner) && counter(v.links)
    && seconds(v.joined_at)
}

export function isMemberList(v: unknown, strict = false): v is Member[] {
  return Array.isArray(v) && v.every(item => isMember(item, strict))
}

export function isGrant(v: unknown, strict = false): v is Grant {
  return record(v) && known(v, ['account_id', 'user_id', 'read', 'act', 'send', 'manage', 'granted_by', 'updated_at'], strict)
    && pathID(v.account_id) && pathID(v.user_id) && flag(v.read) && flag(v.act) && flag(v.send) && flag(v.manage)
    && optional(v.granted_by, x => filled(x, 128)) && seconds(v.updated_at)
    // Never act without read, and never a grant of nothing: the server deletes those.
    && (!strict || ((!v.act || v.read) && (v.read || v.act || v.send || v.manage)))
}

export function isMailboxAccess(v: unknown, strict = false): v is MailboxAccess {
  return record(v) && known(v, ['account_id', 'email', 'provider', 'state', 'linked_by', 'grants'], strict)
    && pathID(v.account_id) && filled(v.email, 320) && word(providerIDs, strict)(v.provider) && word(accountStates, strict)(v.state)
    && optional(v.linked_by, pathID) && Array.isArray(v.grants) && v.grants.every(grant => isGrant(grant, strict) && grant.account_id === v.account_id)
}

export function isMailboxAccessList(v: unknown, strict = false): v is MailboxAccess[] {
  return Array.isArray(v) && v.every(item => isMailboxAccess(item, strict))
}

export function isTeamInvite(v: unknown, strict = false): v is TeamInvite {
  return record(v) && known(v, ['id', 'email', 'workspace_id', 'role', 'url', 'created_by', 'created_at', 'expires_at'], strict)
    && pathID(v.id) && filled(v.email, 320) && pathID(v.workspace_id) && word(workspaceRoles, strict)(v.role)
    && optional(v.url, x => filled(x, 4096)) && optional(v.created_by, x => filled(x, 128))
    && seconds(v.created_at) && seconds(v.expires_at)
}

export function isTeamInviteList(v: unknown, strict = false): v is TeamInvite[] {
  return Array.isArray(v) && v.every(item => isTeamInvite(item, strict))
}

export function isErrorBody(v: unknown, strict = false): v is ErrorBody {
  return record(v) && known(v, ['code', 'message'], strict) && oneOf(serverCodes)(v.code) && text(v.message)
}
