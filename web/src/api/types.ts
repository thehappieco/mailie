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

export interface User {
  id: string
  email: string
  name: string
  role: Role
  created_at: number
  /**
   * False for a person who signs in only through an edition's own sign-in
   * (an identity provider), with no password to change. A server older than
   * the field leaves it out: a password, as every person had one then.
   */
  has_password?: boolean
}
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
  /**
   * What the caller may do with the mailbox: their grant, as far as their
   * credential reaches, and manage, which an owner or an admin of a team
   * holds on every mailbox of it by their role (seeing its card without
   * reading it). Absent from a daemon older than workspaces; the server
   * decides either way.
   */
  access?: AccountAccess
}
/** What a caller may do with a mailbox: read its index, act on its messages, send from it, manage it. */
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
  /**
   * Sync is allowed: for a personal mailbox, its person agreed to it; for a
   * team's, the team's agreement stands and someone can read it; for the
   * operator's, the operator switched it on.
   */
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
 * What a workspace key may do at most (docs/workspaces.md, "API keys"): read
 * (search and read), write (read, and the actions on messages), or send
 * (write, and sending email). What it does on each mailbox is what it holds
 * there (KeyMailbox), within its scope.
 */
export type KeyScope = 'read' | 'write' | 'send'
/** How long a new key lives, in days: the three lifetimes a key may be created with. */
export type KeyLifetime = 30 | 90 | 365
/** What a key holds on one mailbox: read its index, act on its messages, send from it. act never comes without read. */
export interface KeyFlags { read: boolean; act: boolean; send: boolean }
/**
 * What a key holds on one mailbox, as its workspace lists it: who set it last
 * ("usr_…" or "migration"; absent once that person is deleted) and when.
 */
export interface KeyMailbox extends KeyFlags {
  account_id: string
  workspace_id: string
  granted_by?: string
  updated_at: number
}
/**
 * How a key came to be in its workspace: "person" for a person's key made for
 * mailboxes they chose, "person-all" for one made for every mailbox of
 * theirs, which the upgrade to workspace keys moved into their workspace
 * (the second with the mailboxes its person read then, and none linked
 * since). Absent for a key made as keys are now.
 */
export type KeyOrigin = 'person' | 'person-all'
/**
 * A key of a workspace, as its owners and admins list it (GET
 * /v1/workspaces/{id}/apikeys), and as the person who created it does (GET
 * /v1/me/apikeys). Never the secret: that exists once, in the answer to the
 * call that created the key.
 *
 * carried_over: a person's key the upgrade found reaching mailboxes of
 * several workspaces, with no workspace of its own: it keeps exactly those,
 * gains none, and is revoked with its last; a workspace lists it with its own
 * mailboxes and counts the others (other_workspaces) without naming them.
 * created_by: the person who created it, absent once they are deleted.
 * sends: whether it can send at all (the send scope, on a server whose keys
 * may send, under the current key terms). terms_version: the revision of the
 * key terms its creator agreed to.
 */
export interface WorkspaceKey {
  prefix: string
  name: string
  /** The running console reads a scope it does not know as its identifier. */
  scope: KeyScope | (string & {})
  workspace_id?: string
  carried_over?: boolean
  origin?: KeyOrigin | (string & {})
  mailboxes: KeyMailbox[]
  other_workspaces?: number
  created_by?: string
  created_at: number
  expires_at: number
  last_used_at?: number
  revoked_at?: number
  /** Neither revoked nor expired, by the server's clock. */
  live: boolean
  terms_version: string
  sends: boolean
}
/** The answer to creating a key: the key as listed, and its secret, "<prefix>.<secret>", this once. */
export interface CreatedKey extends WorkspaceKey { key: string }
/**
 * A live API key holding something on a mailbox, as the access directory
 * lists it to the workspace's owners and admins. Keys never count as readers.
 */
export interface MailboxKey extends KeyFlags {
  prefix: string
  name: string
  scope: KeyScope | (string & {})
  created_by?: string
  granted_by?: string
  updated_at: number
  carried_over?: boolean
}
/**
 * One of a key's sends, as its workspace's owners and admins list them (GET
 * /v1/workspaces/{id}/apikeys/{prefix}/sends): what Mailie keeps of a send for
 * 30 days. Never who it went to, its subject or its text: how many
 * recipients, how many attempts, and where the copy Mailie files in Sent
 * stands.
 */
export interface KeySend {
  account_id: string
  idempotency_key: string
  state: SendState | (string & {})
  message_id: string
  reason?: string
  attempts: number
  recipients: number
  sent_copy: string
  created_at: number
  updated_at: number
  sent_at?: number
}
/**
 * Where a send stands: sending, sent (the server accepted it), failed
 * (nothing was delivered), or unknown (the connection dropped after the
 * message was handed over: it may have been delivered, and it is never
 * sent again by itself).
 */
export type SendState = 'sending' | 'sent' | 'failed' | 'unknown'
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
 * (MAIL_MCP_HTTP), and whether its API keys may send email
 * (MAIL_KEYS_MAY_SEND). Off, the console shows no MCP address: /mcp answers
 * 404; keys_send false (or absent, from a server older than it), it offers
 * neither the send scope nor Send on a key's mailbox.
 */
export interface McpAccess { http: boolean; keys_send?: boolean }
/** What a key is given on one mailbox when it is created. */
export interface KeyMailboxRequest extends KeyFlags { account_id: string }
/** The body of POST /v1/workspaces/{id}/apikeys. mailboxes is left out for a key that reaches nothing yet. */
export interface CreateKeyRequest {
  name: string
  scope: KeyScope
  ttl_days: KeyLifetime
  terms_version: string
  mailboxes?: KeyMailboxRequest[]
}

/** The body of POST /v1/accounts. Unknown fields are refused by the daemon, so only these are ever sent. */
export interface AddAccountRequest {
  email: string
  display_name?: string
  provider: ProviderID
  /** The workspace to link it into: a team the caller owns or administers. Left out, the caller's personal workspace. */
  workspace_id?: string
  /**
   * Into a team only: the revision of the sync text the owner or admin was
   * shown, which gives the team's agreement to sync it with the link. Left
   * out, the mailbox is linked with sync off.
   */
  sync_consent_version?: string
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
/** A person's role in a workspace: owners and admins administer people and grants, and manage every mailbox, and read none by it. */
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
 * A membership of a workspace. last_owner and last_reader_of mark the
 * protections in advance: the last active owner is never demoted, disabled
 * or removed, and the last person who can read a mailbox of the team keeps
 * Read, and is neither disabled nor removed, while it is so.
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
  /** The team's mailboxes this person alone can read. Absent from a daemon older than the rule. */
  last_reader_of?: string[]
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
 * A mailbox's own agreement to sync, as its workspace's owners and admins
 * see it: a team mailbox's is the team's, given by one of them on its behalf;
 * an operator mailbox's is the operator's switch. enabled_by is who gave it
 * ("usr_…", "key:<prefix>" or "cli"; absent once that person is deleted) and
 * version the revision of the sync text it was given to, which current says
 * is the one the server asks about now. migrated: an agreement the upgrade
 * carried over from the person who linked the mailbox, still tied to them
 * until an owner or an admin gives it again for the team (with enabled false,
 * a mailbox kept stopped, with its index, because that person was disabled).
 */
export interface MailboxConsent {
  enabled: boolean
  enabled_at?: number
  enabled_by?: string
  version?: string
  migrated?: boolean
  current: boolean
}
/**
 * One mailbox of a workspace and who holds what on it: the access directory,
 * addresses and grants, never what the mailbox holds. linked_by says who
 * connected it, for the record only. readers counts the active members who
 * can read it (keys and roles never count), and no_reader marks a team
 * mailbox nobody can read: it syncs nothing, and only removing it gives
 * anyone Read on it again. sync is its own agreement to sync; absent for a
 * personal mailbox, which syncs under its person's. keys are the API keys
 * holding something on it, which never count as readers.
 */
export interface MailboxAccess {
  account_id: string
  email: string
  provider: ProviderID | (string & {})
  state: AccountState | (string & {})
  linked_by?: string
  sync?: MailboxConsent
  /** Absent from a daemon older than the last-reader rule. */
  readers?: number
  no_reader?: boolean
  grants: Grant[]
  /** The live API keys holding something on it. Absent from a daemon older than workspace keys. */
  keys?: MailboxKey[]
}
/** The body of PUT /v1/accounts/{id}/sync: a team's agreement on or off, on to the sync text revision shown. */
export interface MailboxSyncRequest { enabled: boolean; version?: string }
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
export const keyScopes: readonly KeyScope[] = ['read', 'write', 'send']
export const keyOrigins: readonly KeyOrigin[] = ['person', 'person-all']
export const sendStates: readonly SendState[] = ['sending', 'sent', 'failed', 'unknown']
export const sentCopyStates = ['n/a', 'pending', 'appended', 'failed'] as const
export const keyLifetimes: readonly KeyLifetime[] = [30, 90, 365]
/** internal/events: the closed vocabulary of the journal. */
export const eventTypes = ['message.new', 'message.flags', 'message.moved', 'message.deleted', 'folder.changed', 'account.state', 'send.finished', 'sync.progress'] as const
export const folderSyncStates = ['new', 'initial', 'live', 'resync', 'error', 'disabled'] as const

export function isUser(v: unknown, strict = false): v is User {
  return record(v) && known(v, ['id', 'email', 'name', 'role', 'created_at', 'has_password'], strict)
    && filled(v.id, 64) && filled(v.email, 320) && text(v.name, 1024) && oneOf(roles)(v.role) && seconds(v.created_at)
    // The daemon that writes the fixtures always says; an older one may not.
    && (strict ? flag(v.has_password) : optional(v.has_password, flag))
}

/** Whether the person signs in with a password here, which they may change: every person, but one who signs in only another way. */
export function hasPassword(user: User | null | undefined): boolean {
  return user?.has_password !== false
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
  return record(v) && known(v, ['id', 'email', 'display_name', 'provider', 'auth_kind', 'state', 'state_reason', 'sync_tier', 'save_sent_copy', 'last_ok_at', 'last_error', 'created_at', 'sync', 'actions', 'send', 'workspace_id', 'access'], strict)
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

const keyFields = ['prefix', 'name', 'scope', 'workspace_id', 'carried_over', 'origin', 'mailboxes', 'other_workspaces', 'created_by',
  'created_at', 'expires_at', 'last_used_at', 'revoked_at', 'live', 'terms_version', 'sends'] as const

/**
 * A key's prefix names it in a path (DELETE …/apikeys/{prefix}), so the
 * running console holds it to a short run without a separator; the contract
 * check, to the hex the daemon issues.
 */
const keyPrefix = (v: unknown, strict: boolean): v is string => filled(v, 64) && (strict ? /^[0-9a-f]+$/.test(v) : /^[^\s./\\?#]+$/.test(v))

export function isKeyMailbox(v: unknown, strict = false): v is KeyMailbox {
  return record(v) && known(v, ['account_id', 'workspace_id', 'read', 'act', 'send', 'granted_by', 'updated_at'], strict)
    && pathID(v.account_id) && pathID(v.workspace_id) && flag(v.read) && flag(v.act) && flag(v.send)
    && optional(v.granted_by, x => filled(x, 128)) && seconds(v.updated_at)
    // Never act without read, and never a hold of nothing: taking the last flag away takes the mailbox out.
    && (!strict || ((!v.act || v.read) && (v.read || v.act || v.send)))
}

function keyListed(v: Fields, strict: boolean): boolean {
  const mailboxes = v.mailboxes
  return keyPrefix(v.prefix, strict) && filled(v.name, 1024) && (strict ? oneOf(keyScopes)(v.scope) : filled(v.scope, 32))
    && optional(v.workspace_id, pathID) && optional(v.carried_over, flag)
    && optional(v.origin, x => strict ? oneOf(keyOrigins)(x) : filled(x, 32))
    && Array.isArray(mailboxes) && mailboxes.every(item => isKeyMailbox(item, strict))
    && optional(v.other_workspaces, counter) && optional(v.created_by, x => filled(x, 128))
    && seconds(v.created_at) && seconds(v.expires_at) && optional(v.last_used_at, seconds) && optional(v.revoked_at, seconds)
    && flag(v.live) && text(v.terms_version, 128) && flag(v.sends)
    // A key has its workspace, or was carried over without one; only a carried-over key counts other workspaces.
    && (!strict || ((v.workspace_id === undefined) === (v.carried_over === true) && (v.other_workspaces === undefined || v.carried_over === true)))
    // A key's mailboxes are its workspace's.
    && (!strict || v.workspace_id === undefined || (mailboxes as KeyMailbox[]).every(item => item.workspace_id === v.workspace_id))
}

export function isWorkspaceKey(v: unknown, strict = false): v is WorkspaceKey {
  return record(v) && known(v, keyFields, strict) && keyListed(v, strict)
}

export function isWorkspaceKeyList(v: unknown, strict = false): v is WorkspaceKey[] {
  return Array.isArray(v) && v.every(item => isWorkspaceKey(item, strict))
}

export function isKeySend(v: unknown, strict = false): v is KeySend {
  if (!record(v) || !known(v, ['account_id', 'idempotency_key', 'state', 'message_id', 'reason', 'attempts', 'recipients', 'sent_copy', 'created_at', 'updated_at', 'sent_at'], strict)) return false
  return filled(v.account_id, 64) && filled(v.idempotency_key, 128) && (strict ? oneOf(sendStates)(v.state) : filled(v.state, 32))
    && text(v.message_id, 1024) && optional(v.reason, x => filled(x, 64)) && counter(v.attempts) && counter(v.recipients)
    && (strict ? oneOf(sentCopyStates)(v.sent_copy) : text(v.sent_copy, 32)) && seconds(v.created_at) && seconds(v.updated_at)
    && optional(v.sent_at, seconds)
}

export function isKeySendList(v: unknown, strict = false): v is KeySend[] {
  return Array.isArray(v) && v.every(item => isKeySend(item, strict))
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
  return record(v) && known(v, ['http', 'keys_send'], strict) && flag(v.http)
    // The daemon that writes the fixtures always says; an older one may not.
    && (strict ? flag(v.keys_send) : optional(v.keys_send, flag))
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

/** Mailbox ids, each one a path may carry. */
const pathIDs = (v: unknown): v is string[] => Array.isArray(v) && v.every(pathID)

export function isMember(v: unknown, strict = false): v is Member {
  return record(v) && known(v, ['user_id', 'email', 'name', 'role', 'status', 'person_disabled', 'last_owner', 'last_reader_of', 'joined_at'], strict)
    && pathID(v.user_id) && filled(v.email, 320) && text(v.name, 1024) && word(workspaceRoles, strict)(v.role)
    && word(memberStatuses, strict)(v.status) && optional(v.person_disabled, flag) && flag(v.last_owner)
    // The daemon that writes the fixtures always says, [] for none; an older one may not.
    && (strict ? pathIDs(v.last_reader_of) : optional(v.last_reader_of, pathIDs))
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

/** Who did something, as the server records it: a person's id, a key's ("key:<prefix>"), or "cli". */
const actor = (v: unknown): v is string => filled(v, 128) && !/\s/.test(v)

export function isMailboxConsent(v: unknown, strict = false): v is MailboxConsent {
  return record(v) && known(v, ['enabled', 'enabled_at', 'enabled_by', 'version', 'migrated', 'current'], strict)
    && flag(v.enabled) && optional(v.enabled_at, seconds) && optional(v.enabled_by, actor) && optional(v.version, x => filled(x, 128))
    && optional(v.migrated, flag) && flag(v.current)
    // Current names a revision; an agreement that stands says when it was given.
    && (!strict || ((!v.current || v.version !== undefined) && (!v.enabled || v.enabled_at !== undefined)))
}

export function isMailboxKey(v: unknown, strict = false): v is MailboxKey {
  return record(v) && known(v, ['prefix', 'name', 'scope', 'read', 'act', 'send', 'created_by', 'granted_by', 'updated_at', 'carried_over'], strict)
    && keyPrefix(v.prefix, strict) && filled(v.name, 1024) && (strict ? oneOf(keyScopes)(v.scope) : filled(v.scope, 32))
    && flag(v.read) && flag(v.act) && flag(v.send) && optional(v.created_by, x => filled(x, 128)) && optional(v.granted_by, x => filled(x, 128))
    && seconds(v.updated_at) && optional(v.carried_over, flag)
    && (!strict || ((!v.act || v.read) && (v.read || v.act || v.send)))
}

export function isMailboxAccess(v: unknown, strict = false): v is MailboxAccess {
  return record(v) && known(v, ['account_id', 'email', 'provider', 'state', 'linked_by', 'sync', 'readers', 'no_reader', 'grants', 'keys'], strict)
    && pathID(v.account_id) && filled(v.email, 320) && word(providerIDs, strict)(v.provider) && word(accountStates, strict)(v.state)
    && optional(v.linked_by, actor) && optional(v.sync, x => isMailboxConsent(x, strict))
    // The daemon that writes the fixtures always says; an older one may not.
    && (strict ? counter(v.readers) && flag(v.no_reader) : optional(v.readers, counter) && optional(v.no_reader, flag))
    // Nobody can read a mailbox only when it has no reader.
    && (!strict || !v.no_reader || v.readers === 0)
    && Array.isArray(v.grants) && v.grants.every(grant => isGrant(grant, strict) && grant.account_id === v.account_id)
    // The daemon that writes the fixtures always lists them, [] for none; an older one may not.
    && (strict ? mailboxKeys(v.keys, strict) : optional(v.keys, x => mailboxKeys(x, false)))
}

const mailboxKeys = (v: unknown, strict: boolean): boolean => Array.isArray(v) && v.every(item => isMailboxKey(item, strict))

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
