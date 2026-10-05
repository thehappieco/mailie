-- migration: rebuild
--
-- Phase 2: every mailbox belongs to a workspace, and seeing one takes active
-- membership in its workspace and a grant on it (docs/workspaces.md).
--
--   workspaces        a person's personal workspace, teams, and the one
--                     operator workspace, which has no members and is what
--                     instance keys and the command line act on
--   workspace_members who belongs to a workspace, as owner, admin or member
--   mailbox_access    per mailbox and person: read, act, send, manage
--
-- accounts.email was UNIQUE across the daemon (0001); the same address may
-- now be linked once per workspace. SQLite drops a constraint only by
-- rebuilding the table, so this migration is the first rebuild, run by the
-- runner's rebuild procedure (store/migrate.go, applyRebuild): foreign keys
-- off outside the transaction, so dropping the old accounts cascades into
-- nothing; every row copied with its rowid and every column named; foreign
-- keys and row counts checked before the commit. credentials, oauth_pending,
-- folders, messages, drafts, sends and api_key_accounts name accounts by
-- name, and refer to the new table once it carries it.
--
-- accounts.owner_user_id keeps its name and now means "linked by": the
-- person who linked the mailbox, whose consent to sync it syncs under. It is
-- NULL exactly for the operator workspace's mailboxes.
--
-- What becomes of a schema-7 database: each person gets a personal workspace
-- and is its owner; each mailbox they own goes into it, with a grant of all
-- four flags to them; each mailbox nobody owns goes into the operator
-- workspace, without grants. Invites gain an id and, for a team invite, a
-- workspace and a role. Nothing else changes.

CREATE TABLE workspaces (
  id         TEXT PRIMARY KEY,                -- 'wsp_' + 16 hex here; the platform's UUID; 'wsp_operator'
  kind       TEXT NOT NULL CHECK (kind IN ('personal','team','operator')),
  source     TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local','platform')),
  name       TEXT NOT NULL DEFAULT '',        -- a team's, 1 to 80 characters; '' for the others, which a client names
  person_id  TEXT UNIQUE REFERENCES users(id), -- the person of a personal workspace. No ON DELETE: deleting a
                                              -- person deletes it explicitly, after the mailboxes in it.
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  CHECK ((kind = 'personal') = (person_id IS NOT NULL)),
  CHECK ((kind = 'operator') = (id = 'wsp_operator')),
  CHECK (kind <> 'operator' OR source = 'local'),
  CHECK (CASE WHEN kind = 'team' THEN length(name) BETWEEN 1 AND 80 ELSE name = '' END)
);
-- Exactly one operator workspace.
CREATE UNIQUE INDEX workspaces_one_operator ON workspaces(kind) WHERE kind = 'operator';

CREATE TABLE workspace_members (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role         TEXT NOT NULL CHECK (role IN ('owner','admin','member')),
  status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
                                              -- disabled: listed, with no access; its grants were deleted
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX workspace_members_user ON workspace_members(user_id);

-- The repository checks these too; the triggers are what holds whatever
-- writes the row.
CREATE TRIGGER workspace_members_not_operator BEFORE INSERT ON workspace_members
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'operator')
BEGIN
  SELECT RAISE(ABORT, 'the operator workspace has no members');
END;
CREATE TRIGGER workspace_members_personal BEFORE INSERT ON workspace_members
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'personal'
               AND (person_id IS NOT NEW.user_id OR NEW.role <> 'owner' OR NEW.status <> 'active'))
BEGIN
  SELECT RAISE(ABORT, 'a personal workspace has one member, its person, as an active owner');
END;
CREATE TRIGGER workspace_members_personal_fixed BEFORE UPDATE OF role, status ON workspace_members
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'personal')
     AND (NEW.role <> 'owner' OR NEW.status <> 'active')
BEGIN
  SELECT RAISE(ABORT, 'a personal workspace has one member, its person, as an active owner');
END;
CREATE TRIGGER workspace_members_fixed BEFORE UPDATE OF workspace_id, user_id ON workspace_members
WHEN NEW.workspace_id IS NOT OLD.workspace_id OR NEW.user_id IS NOT OLD.user_id
BEGIN
  SELECT RAISE(ABORT, 'a membership never moves to another workspace or person');
END;

INSERT INTO workspaces(id, kind, source, name, person_id, created_at, updated_at)
VALUES ('wsp_operator', 'operator', 'local', '', NULL, unixepoch(), unixepoch());

INSERT INTO workspaces(id, kind, source, name, person_id, created_at, updated_at)
SELECT 'wsp_' || lower(hex(randomblob(8))), 'personal', 'local', '', id, created_at, created_at
  FROM users ORDER BY rowid;

INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
SELECT id, person_id, 'owner', 'active', created_at, created_at
  FROM workspaces WHERE kind = 'personal' ORDER BY rowid;

-- Every column of accounts as sqlite_schema holds it after 0007, with its
-- type, default and CHECK, in the same order, and workspace_id after id.
CREATE TABLE accounts_new (
  id               TEXT PRIMARY KEY,                       -- 'acc_' + 16 hex, never reused
  workspace_id     TEXT NOT NULL REFERENCES workspaces(id), -- never changes (accounts_workspace_fixed)
  email            TEXT NOT NULL COLLATE NOCASE,           -- unique per workspace, below
  display_name     TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL CHECK (provider IN ('gmail','microsoft','imap')),
  auth_kind        TEXT NOT NULL CHECK (auth_kind IN ('oauth2','password')),
  imap_host        TEXT NOT NULL, imap_port INTEGER NOT NULL,
  smtp_host        TEXT NOT NULL, smtp_port INTEGER NOT NULL,
  smtp_tls         TEXT NOT NULL CHECK (smtp_tls IN ('implicit','starttls')),  -- implicit=465 WithSSL, starttls=587 TLSMandatory
  login_user       TEXT NOT NULL,                          -- SASL identity; differs from email on some generic IMAP servers
  oauth_tenant     TEXT NOT NULL DEFAULT '',               -- microsoft: 'common' covers work and personal accounts
  sync_tier        TEXT NOT NULL DEFAULT 'auto' CHECK (sync_tier IN ('auto','condstore','uidpoll')),
  sync_tier_resolved TEXT NOT NULL DEFAULT '',             -- what Caps() actually said at first login; an operator needs to see this
  save_sent_copy   INTEGER NOT NULL,                       -- 0 for gmail/microsoft (they file the copy themselves), 1 for generic imap
  initial_days     INTEGER NOT NULL DEFAULT 90,            -- initial sync window (SEARCH SINCE); 0 means everything
  folder_overrides TEXT NOT NULL DEFAULT '{}',             -- {"sent":"Itens Enviados"} beats SPECIAL-USE and the localised table
  state            TEXT NOT NULL CHECK (state IN ('pending_auth','active','needs_reauth','disabled','error')),
  state_reason     TEXT NOT NULL DEFAULT '',               -- 'AADSTS70008 refresh token expired', redacted
  state_changed_at INTEGER NOT NULL,
  last_ok_at            INTEGER NOT NULL DEFAULT 0,
  last_error            TEXT NOT NULL DEFAULT '',
  consecutive_failures  INTEGER NOT NULL DEFAULT 0,
  next_retry_at         INTEGER NOT NULL DEFAULT 0,
  last_idle_event_at    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  owner_user_id    TEXT REFERENCES users(id),              -- linked by; NULL exactly in the operator workspace
  oauth_client     TEXT NOT NULL DEFAULT 'installed' CHECK (oauth_client IN ('installed','web')),
  sync_enabled_at  INTEGER NOT NULL DEFAULT 0,             -- the operator's mailboxes: 0 = off
  sync_enabled_by  TEXT NOT NULL DEFAULT '',               -- 'key:<prefix>' or 'cli': who switched it on
  UNIQUE (workspace_id, email),                            -- an address once per workspace, without regard to case
  UNIQUE (id, workspace_id)                                -- the parent key of mailbox_access
);

INSERT INTO accounts_new(rowid, id, workspace_id, email, display_name, provider, auth_kind,
  imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, oauth_tenant,
  sync_tier, sync_tier_resolved, save_sent_copy, initial_days, folder_overrides,
  state, state_reason, state_changed_at, last_ok_at, last_error, consecutive_failures,
  next_retry_at, last_idle_event_at, created_at, updated_at,
  owner_user_id, oauth_client, sync_enabled_at, sync_enabled_by)
SELECT a.rowid, a.id,
       coalesce((SELECT w.id FROM workspaces w WHERE w.person_id = a.owner_user_id), 'wsp_operator'),
       a.email, a.display_name, a.provider, a.auth_kind,
       a.imap_host, a.imap_port, a.smtp_host, a.smtp_port, a.smtp_tls, a.login_user, a.oauth_tenant,
       a.sync_tier, a.sync_tier_resolved, a.save_sent_copy, a.initial_days, a.folder_overrides,
       a.state, a.state_reason, a.state_changed_at, a.last_ok_at, a.last_error, a.consecutive_failures,
       a.next_retry_at, a.last_idle_event_at, a.created_at, a.updated_at,
       a.owner_user_id, a.oauth_client, a.sync_enabled_at, a.sync_enabled_by
  FROM accounts a ORDER BY a.rowid;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
CREATE INDEX accounts_owner ON accounts(owner_user_id);

CREATE TRIGGER accounts_workspace_fixed BEFORE UPDATE OF workspace_id ON accounts
WHEN NEW.workspace_id IS NOT OLD.workspace_id
BEGIN
  SELECT RAISE(ABORT, 'a mailbox never moves to another workspace');
END;
CREATE TRIGGER accounts_linker_on_insert BEFORE INSERT ON accounts
WHEN (NEW.owner_user_id IS NULL) <> EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'operator')
BEGIN
  SELECT RAISE(ABORT, 'a mailbox has no linker exactly when it is in the operator workspace');
END;
CREATE TRIGGER accounts_linker_on_update BEFORE UPDATE OF owner_user_id ON accounts
WHEN (NEW.owner_user_id IS NULL) <> EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'operator')
BEGIN
  SELECT RAISE(ABORT, 'a mailbox has no linker exactly when it is in the operator workspace');
END;

-- A grant carries its workspace so that two foreign keys make "only for a
-- member of the mailbox's own workspace" a property of the schema, and so
-- that removing the member or the mailbox takes the grant with it. A row
-- with no flag left is deleted, never stored.
CREATE TABLE mailbox_access (
  account_id   TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  read         INTEGER NOT NULL DEFAULT 0 CHECK (read IN (0, 1)),
  act          INTEGER NOT NULL DEFAULT 0 CHECK (act IN (0, 1)),
  send         INTEGER NOT NULL DEFAULT 0 CHECK (send IN (0, 1)),
  manage       INTEGER NOT NULL DEFAULT 0 CHECK (manage IN (0, 1)),
  granted_by   TEXT NOT NULL DEFAULT '',  -- 'usr_…', 'key:<prefix>', 'cli' or 'migration'
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, user_id),
  FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE,
  CHECK (act = 0 OR read = 1),            -- an action names messages the actor reads
  CHECK (read + act + send + manage > 0)
);
CREATE INDEX mailbox_access_user ON mailbox_access(user_id);
CREATE INDEX mailbox_access_member ON mailbox_access(workspace_id, user_id);

INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
SELECT id, workspace_id, owner_user_id, 1, 1, 1, 1, 'migration', created_at, created_at
  FROM accounts WHERE owner_user_id IS NOT NULL ORDER BY rowid;

-- An invite is an instance invite (no workspace: a new person, with the
-- instance role in role) or a team invite (a workspace and the role in it; a
-- new person who signs up with it is an instance member). Each now has an id,
-- by which its team lists and revokes it.
ALTER TABLE invites ADD COLUMN id TEXT NOT NULL DEFAULT '';
UPDATE invites SET id = 'inv_' || lower(hex(randomblob(8)));
CREATE UNIQUE INDEX invites_id ON invites(id);
ALTER TABLE invites ADD COLUMN workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL;
ALTER TABLE invites ADD COLUMN workspace_role TEXT NOT NULL DEFAULT ''
  CHECK (workspace_role IN ('', 'owner', 'admin', 'member'));
CREATE INDEX invites_workspace ON invites(workspace_id) WHERE workspace_id IS NOT NULL;

CREATE TRIGGER invites_team BEFORE INSERT ON invites
WHEN (NEW.workspace_id IS NULL) <> (NEW.workspace_role = '')
  OR (NEW.workspace_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND kind = 'team'))
BEGIN
  SELECT RAISE(ABORT, 'a team invite names a team and a role in it; an instance invite names neither');
END;

-- A team that goes takes the invites still waiting to join it, which could
-- otherwise outlive it as instance invites. A used one stays, its workspace
-- set to NULL and its workspace_role kept, as the record of how its person
-- arrived: it goes when that person does, like every used invite.
CREATE TRIGGER workspaces_unused_invites BEFORE DELETE ON workspaces
BEGIN
  DELETE FROM invites WHERE workspace_id = OLD.id AND used_at = 0;
END;
