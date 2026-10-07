-- An API key belongs to its workspace (docs/workspaces.md, "API keys").
--
-- Until now a key either belonged to the instance (api_keys.user_id NULL),
-- reaching the operator workspace's mailboxes, or acted as a person, reaching
-- live what that person could. From here on every key belongs to one
-- workspace and acts as nobody:
--
--   - an operator key (workspace_id 'wsp_operator') is the instance key it
--     was: any scope, reaching the operator workspace's mailboxes, optionally
--     restricted to some of them in api_key_accounts, as before;
--   - a workspace key (workspace_id a personal workspace or a team) reaches
--     exactly the mailboxes of its workspace it holds in key_access, each with
--     its own read, act and send, given by the workspace's owners and admins:
--     read only by one who reads the mailbox then. What it holds stands on its
--     own, whatever becomes of who gave it or who created it, except that the
--     key is revoked when the person who created it (created_by) leaves the
--     workspace or is disabled or deleted on the instance. Its scope is read,
--     write or send; a key with no mailbox reaches nothing, and is not revoked
--     for that;
--   - a carried-over key (workspace_id NULL) is a person's key that reached
--     mailboxes of several workspaces when this migration ran. It keeps
--     exactly those, gains none, expires within 365 days, and is revoked when
--     its last one goes.
--
-- What becomes of a schema-11 database:
--
--   - Instance keys: workspace_id 'wsp_operator'. Nothing else changes; their
--     restriction rows stay.
--   - A person's key: user_id becomes NULL; created_by (the person, for a key
--     they made in the console) stays, as who answers for it. It reaches what
--     it reached: a key made for chosen mailboxes (origin 'person') those of
--     them its person reads now, and a live key made for every mailbox of
--     theirs (origin 'person-all') every mailbox its person reads now. Each
--     becomes a key_access row with read, with act where the key was write
--     and its person held act, never send, granted by 'migration': nothing a
--     key could do yesterday is widened. The key goes into the one workspace
--     those mailboxes are in, its person's personal workspace when there are
--     none, or none at all (carried over) when they span several. A revoked or
--     expired key keeps the mailboxes it was made for, and reaches nothing.
--   - A person's key nobody agreed to the key terms through (terms_version
--     '': made for them by an administrator, or before keys had terms) was
--     refused everywhere; it is revoked now, and kept for the record in its
--     person's personal workspace, holding nothing: a team never sees a key
--     made for one of its members, and the key goes with that workspace when
--     its person is deleted, as it went with them before. Nothing else tells
--     whose it was once user_id is gone.
--   - api_key_accounts holds only operator keys' restrictions.
--   - Sends are counted per key, for a key's daily limit.
--
-- An ordinary migration, in one transaction with foreign keys on: columns,
-- a table and indexes added, rows updated and moved, one trigger dropped and
-- created again as it was, triggers added.

ALTER TABLE api_keys ADD COLUMN workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE;
                                            -- 'wsp_operator', a personal workspace or a team; NULL: carried over
ALTER TABLE api_keys ADD COLUMN origin TEXT NOT NULL DEFAULT ''
  CHECK (origin IN ('', 'person', 'person-all')); -- a person's key this migration carried over, made for chosen
                                                  -- mailboxes or for every mailbox of theirs
CREATE INDEX api_keys_workspace ON api_keys(workspace_id);
CREATE INDEX api_keys_created_by ON api_keys(created_by);

-- What a workspace key holds on each mailbox of its workspace. A row with no
-- flag left is deleted, never stored.
CREATE TABLE key_access (
  key_prefix   TEXT NOT NULL REFERENCES api_keys(prefix) ON DELETE CASCADE,
  account_id   TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  read         INTEGER NOT NULL DEFAULT 0 CHECK (read IN (0, 1)),
  act          INTEGER NOT NULL DEFAULT 0 CHECK (act IN (0, 1)),
  send         INTEGER NOT NULL DEFAULT 0 CHECK (send IN (0, 1)),
  granted_by   TEXT NOT NULL DEFAULT '',  -- 'usr_…' or 'migration'; '' once that person is deleted
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (key_prefix, account_id),
  FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE,
  CHECK (act = 0 OR read = 1),            -- an action names messages the key reads
  CHECK (read + act + send > 0)
);
CREATE INDEX key_access_account ON key_access(account_id);

-- A key's sends of the last day, for its daily limit, and listed to its
-- workspace's owners and admins.
CREATE INDEX sends_created_by ON sends(created_by, created_at);

-- Instance keys.
UPDATE api_keys SET workspace_id = 'wsp_operator' WHERE user_id IS NULL;

-- What each person's key reaches today, as key_access rows. A mailbox its
-- person reads now is one they hold read on as an active member, active on
-- the instance; act where the key was write and they hold act. A key nobody
-- agreed to the key terms through reached nothing, and gets nothing: it goes
-- to its person's personal workspace, and is revoked, below. (A person's
-- key of the send or admin scope was made by an administrator, agreed to by
-- nobody, and is one of those.)
INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act, send, granted_by, created_at, updated_at)
SELECT k.prefix, a.id, a.workspace_id, 1,
       k.scope IN ('write', 'send') AND coalesce(g.act, 0) = 1
         AND EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id = m.user_id
                      WHERE m.workspace_id = a.workspace_id AND m.user_id = k.user_id
                        AND m.status = 'active' AND u.status = 'active'),
       0, 'migration', unixepoch(), unixepoch()
  FROM api_keys k
  JOIN accounts a ON a.workspace_id <> 'wsp_operator'
  LEFT JOIN mailbox_access g ON g.account_id = a.id AND g.user_id = k.user_id
 WHERE k.user_id IS NOT NULL AND k.terms_version <> ''
   AND (
         -- Made for chosen mailboxes: those still chosen. A live key's only
         -- where its person reads now, as it reached them yesterday; a dead
         -- key's as they were, for the record.
         (k.restricted = 1
          AND EXISTS (SELECT 1 FROM api_key_accounts r WHERE r.key_prefix = k.prefix AND r.account_id = a.id)
          AND ((k.revoked_at <> 0 OR (k.expires_at <> 0 AND k.expires_at <= unixepoch()))
               OR (coalesce(g.read, 0) = 1
                   AND EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id = m.user_id
                                WHERE m.workspace_id = a.workspace_id AND m.user_id = k.user_id
                                  AND m.status = 'active' AND u.status = 'active'))))
         -- Made for every mailbox of theirs, live: every one they read now.
      OR (k.restricted = 0 AND k.revoked_at = 0 AND (k.expires_at = 0 OR k.expires_at > unixepoch())
          AND coalesce(g.read, 0) = 1
          AND EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id = m.user_id
                       WHERE m.workspace_id = a.workspace_id AND m.user_id = k.user_id
                         AND m.status = 'active' AND u.status = 'active'))
       )
 ORDER BY k.rowid, a.rowid;

-- The workspace of each person's key: the one its mailboxes are in, its
-- person's personal workspace when it has none (a key nobody agreed to is
-- one), none when they span several.
UPDATE api_keys
   SET workspace_id = CASE (SELECT count(DISTINCT x.workspace_id) FROM key_access x WHERE x.key_prefix = api_keys.prefix)
         WHEN 0 THEN (SELECT w.id FROM workspaces w WHERE w.person_id = api_keys.user_id AND w.kind = 'personal')
         WHEN 1 THEN (SELECT x.workspace_id FROM key_access x WHERE x.key_prefix = api_keys.prefix LIMIT 1)
       END,
       origin = CASE restricted WHEN 1 THEN 'person' ELSE 'person-all' END
 WHERE user_id IS NOT NULL;

-- A carried-over key expires within 365 days.
UPDATE api_keys SET expires_at = unixepoch() + 31536000
 WHERE user_id IS NOT NULL AND workspace_id IS NULL
   AND (expires_at = 0 OR expires_at > unixepoch() + 31536000);

-- Keys nobody agreed to the key terms through, and keys left with no
-- workspace and no mailbox (a person without a personal workspace, which
-- only a binary older than 0008 makes): revoked, kept for the record.
UPDATE api_keys SET revoked_at = unixepoch()
 WHERE user_id IS NOT NULL AND revoked_at = 0
   AND (terms_version = ''
     OR (workspace_id IS NULL AND NOT EXISTS (SELECT 1 FROM key_access x WHERE x.key_prefix = api_keys.prefix)));

-- The person keys' restriction rows are in key_access now. 0005's trigger
-- would revoke a key losing its last one: it goes while they move, and comes
-- back exactly as it was, for the operator keys' rows.
DROP TRIGGER api_key_accounts_last;
DELETE FROM api_key_accounts
 WHERE key_prefix IN (SELECT prefix FROM api_keys WHERE user_id IS NOT NULL);
CREATE TRIGGER api_key_accounts_last AFTER DELETE ON api_key_accounts
WHEN NOT EXISTS (SELECT 1 FROM api_key_accounts WHERE key_prefix = OLD.key_prefix)
BEGIN
  UPDATE api_keys SET revoked_at = unixepoch() WHERE prefix = OLD.key_prefix AND revoked_at = 0;
END;

-- No key acts as a person any more. The column stays, NULL, since dropping
-- it would take a rebuild.
UPDATE api_keys SET user_id = NULL WHERE user_id IS NOT NULL;

-- The schema holds what the repository checks too.
CREATE TRIGGER api_keys_no_person_insert BEFORE INSERT ON api_keys
WHEN NEW.user_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'a key belongs to its workspace and acts as no person');
END;
CREATE TRIGGER api_keys_no_person_update BEFORE UPDATE OF user_id ON api_keys
WHEN NEW.user_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'a key belongs to its workspace and acts as no person');
END;
-- A new key belongs to a workspace: the operator's, at any scope, or another
-- at read, write or send. Only this migration carried keys over without one.
CREATE TRIGGER api_keys_workspace_new BEFORE INSERT ON api_keys
WHEN NEW.workspace_id IS NULL
  OR (NEW.workspace_id <> 'wsp_operator' AND NEW.scope NOT IN ('read', 'write', 'send'))
BEGIN
  SELECT RAISE(ABORT, 'a new key belongs to a workspace: the operator''s, or another at the read, write or send scope');
END;
CREATE TRIGGER api_keys_workspace_fixed BEFORE UPDATE OF workspace_id ON api_keys
WHEN NEW.workspace_id IS NOT OLD.workspace_id
BEGIN
  SELECT RAISE(ABORT, 'a key never moves to another workspace');
END;
-- Only an operator key is restricted to accounts.
CREATE TRIGGER api_key_accounts_operator BEFORE INSERT ON api_key_accounts
WHEN NOT EXISTS (SELECT 1 FROM api_keys WHERE prefix = NEW.key_prefix AND workspace_id = 'wsp_operator')
BEGIN
  SELECT RAISE(ABORT, 'only an operator key is restricted to accounts; a workspace key holds mailboxes in key_access');
END;

-- A key holds mailboxes of its own workspace only: an operator key none, and
-- a carried-over key none more.
CREATE TRIGGER key_access_own_workspace BEFORE INSERT ON key_access
WHEN NOT EXISTS (SELECT 1 FROM api_keys k
                  WHERE k.prefix = NEW.key_prefix AND k.workspace_id = NEW.workspace_id
                    AND k.workspace_id <> 'wsp_operator')
BEGIN
  SELECT RAISE(ABORT, 'a key holds mailboxes of its own workspace only');
END;
CREATE TRIGGER key_access_fixed BEFORE UPDATE OF key_prefix, account_id, workspace_id ON key_access
WHEN NEW.key_prefix IS NOT OLD.key_prefix OR NEW.account_id IS NOT OLD.account_id
  OR NEW.workspace_id IS NOT OLD.workspace_id
BEGIN
  SELECT RAISE(ABORT, 'what a key holds on a mailbox never moves to another key or mailbox');
END;
-- Act needs a key of the write scope or more, send one of the send scope.
CREATE TRIGGER key_access_scope_insert BEFORE INSERT ON key_access
WHEN (NEW.act = 1 AND (SELECT scope FROM api_keys WHERE prefix = NEW.key_prefix) NOT IN ('write', 'send'))
  OR (NEW.send = 1 AND (SELECT scope FROM api_keys WHERE prefix = NEW.key_prefix) <> 'send')
BEGIN
  SELECT RAISE(ABORT, 'act needs a key of the write or send scope, and send a key of the send scope');
END;
CREATE TRIGGER key_access_scope_update BEFORE UPDATE OF act, send ON key_access
WHEN (NEW.act = 1 AND (SELECT scope FROM api_keys WHERE prefix = NEW.key_prefix) NOT IN ('write', 'send'))
  OR (NEW.send = 1 AND (SELECT scope FROM api_keys WHERE prefix = NEW.key_prefix) <> 'send')
BEGIN
  SELECT RAISE(ABORT, 'act needs a key of the write or send scope, and send a key of the send scope');
END;
-- A carried-over key whose last mailbox goes is revoked: it gains none, so it
-- would reach nothing again.
CREATE TRIGGER key_access_carried_over_last AFTER DELETE ON key_access
WHEN EXISTS (SELECT 1 FROM api_keys WHERE prefix = OLD.key_prefix AND workspace_id IS NULL)
 AND NOT EXISTS (SELECT 1 FROM key_access WHERE key_prefix = OLD.key_prefix)
BEGIN
  UPDATE api_keys SET revoked_at = unixepoch() WHERE prefix = OLD.key_prefix AND revoked_at = 0;
END;
