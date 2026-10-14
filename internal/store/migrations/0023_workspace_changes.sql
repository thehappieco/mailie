-- Which workspaces from another source changed what an extension reports of
-- them (docs/workspaces.md, "Following an external directory").
--
-- An ordinary migration: one table and the triggers that write it, and no
-- row of a schema-22 database changes. workspace_changes holds one row per
-- workspace whose source is not the local one, with the seq of the last
-- change to anything an access snapshot of it shows: its mailboxes (linked,
-- deleted, their state, name, removal, sync consent and hold), who holds
-- what on them (flags, sealed grants, mailbox keys), its members (their
-- role, status and hold) and those members' own state (status, suspension,
-- account key, sync consent), its name and closure, and its journal. An
-- extension reads the marks above the last seq it saw (store.WorkspaceChanges)
-- and builds those workspaces' snapshots again. A seq is never reused
-- (AUTOINCREMENT), and a workspace's mark moves only up: every change deletes
-- its row and writes it again. Never INSERT OR REPLACE: a trigger's statement
-- takes the conflict clause of the outer statement that fired it, and an
-- outer INSERT OR IGNORE would then leave the mark where it was. No foreign
-- key: a deleted workspace's last mark stays, which tells an extension it is
-- gone. A local workspace is never marked, so on a server that follows no
-- directory the table stays empty.
--
-- An update marks only when a value it watches changes: a mailbox's state
-- written again as it was, its sync bookkeeping (last_ok_at,
-- consecutive_failures, next_retry_at, state_reason, last_idle_event_at)
-- and every column no snapshot shows mark nothing.

CREATE TABLE workspace_changes (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused
  workspace_id TEXT NOT NULL UNIQUE                -- no foreign key: a deleted workspace's last mark stays
);

CREATE TRIGGER accounts_change_insert AFTER INSERT ON accounts
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER accounts_change_update AFTER UPDATE OF state, display_name, removed_at, purge_after, sync_enabled_at,
  sync_held_at, sync_enabled_via ON accounts
WHEN (NEW.state IS NOT OLD.state OR NEW.display_name IS NOT OLD.display_name OR NEW.removed_at IS NOT OLD.removed_at
      OR NEW.purge_after IS NOT OLD.purge_after OR NEW.sync_enabled_at IS NOT OLD.sync_enabled_at
      OR NEW.sync_held_at IS NOT OLD.sync_held_at OR NEW.sync_enabled_via IS NOT OLD.sync_enabled_via)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER accounts_change_delete AFTER DELETE ON accounts
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = OLD.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.workspace_id);
END;

CREATE TRIGGER mailbox_access_change_insert AFTER INSERT ON mailbox_access
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER mailbox_access_change_update AFTER UPDATE OF read, act, send, manage ON mailbox_access
WHEN (NEW.read IS NOT OLD.read OR NEW.act IS NOT OLD.act OR NEW.send IS NOT OLD.send OR NEW.manage IS NOT OLD.manage)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER mailbox_access_change_delete AFTER DELETE ON mailbox_access
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = OLD.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.workspace_id);
END;

CREATE TRIGGER mailbox_grants_change_insert AFTER INSERT ON mailbox_grants
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER mailbox_grants_change_delete AFTER DELETE ON mailbox_grants
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = OLD.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.workspace_id);
END;

-- A mailbox key names its mailbox alone. Deleted with its mailbox, it finds
-- the mailbox gone, and the mailbox's own deletion marks the workspace.
CREATE TRIGGER mailbox_keys_change_insert AFTER INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
              WHERE a.id = NEW.account_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = (SELECT a.workspace_id FROM accounts a WHERE a.id = NEW.account_id);
  INSERT INTO workspace_changes(workspace_id) SELECT a.workspace_id FROM accounts a WHERE a.id = NEW.account_id;
END;
CREATE TRIGGER mailbox_keys_change_delete AFTER DELETE ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
              WHERE a.id = OLD.account_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = (SELECT a.workspace_id FROM accounts a WHERE a.id = OLD.account_id);
  INSERT INTO workspace_changes(workspace_id) SELECT a.workspace_id FROM accounts a WHERE a.id = OLD.account_id;
END;

CREATE TRIGGER workspace_members_change_insert AFTER INSERT ON workspace_members
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER workspace_members_change_update AFTER UPDATE OF role, status, held_at ON workspace_members
WHEN (NEW.role IS NOT OLD.role OR NEW.status IS NOT OLD.status OR NEW.held_at IS NOT OLD.held_at)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER workspace_members_change_delete AFTER DELETE ON workspace_members
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = OLD.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.workspace_id);
END;

CREATE TRIGGER workspaces_change_insert AFTER INSERT ON workspaces
WHEN NEW.source <> 'local'
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.id);
END;
CREATE TRIGGER workspaces_change_update AFTER UPDATE OF name, closed_at ON workspaces
WHEN (NEW.name IS NOT OLD.name OR NEW.closed_at IS NOT OLD.closed_at) AND NEW.source <> 'local'
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.id);
END;
CREATE TRIGGER workspaces_change_delete AFTER DELETE ON workspaces
WHEN OLD.source <> 'local'
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.id);
END;

-- A person's own state shows in every workspace they are a member of.
CREATE TRIGGER users_change AFTER UPDATE OF status, suspended_at, public_key, sync_consent_at ON users
WHEN NEW.status IS NOT OLD.status OR NEW.suspended_at IS NOT OLD.suspended_at
  OR NEW.public_key IS NOT OLD.public_key OR NEW.sync_consent_at IS NOT OLD.sync_consent_at
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id IN
    (SELECT m.workspace_id FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
      WHERE m.user_id = NEW.id AND w.source <> 'local');
  INSERT INTO workspace_changes(workspace_id)
    SELECT m.workspace_id FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
     WHERE m.user_id = NEW.id AND w.source <> 'local' ORDER BY m.workspace_id;
END;

CREATE TRIGGER mailbox_events_change AFTER INSERT ON mailbox_events
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
