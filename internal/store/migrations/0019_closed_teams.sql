-- A closed team (docs/workspaces.md, "Closed teams").
--
-- An ordinary migration: one column and its triggers are added, nothing else
-- changes, and no row of a schema-18 database does: closed_at is 0 for every
-- workspace, so every team is open.
--
-- A team closes when the operator closes it from the command line (a local
-- team), or when an extension of the daemon closes it, following a decision
-- taken elsewhere; only a team closes. While it is closed (closed_at <> 0,
-- the moment it closed) nothing in it is used: its mailboxes reach nobody,
-- its owners, its admins and its keys included, and sync nothing, with their
-- index, their consent to sync, their grants and the keys that hold them all
-- kept. Reopened, it is as it was.
--
-- Closing is a gate on use, never the reading rule: who reads a mailbox
-- (store.ReaderSQL) does not change, and the gate (store.OpenWorkspaceSQL) is
-- added beside it wherever a mailbox is reached and wherever it syncs. A team
-- mailbox someone reads keeps its reader while the team is closed; one
-- nobody reads is still marked so.
--
-- The triggers hold what the service and the repository refuse: nothing is
-- linked, granted, keyed or invited in a closed team. Each refuses a row
-- that would give the team something it did not have: a mailbox linked into
-- it; a grant on one of its mailboxes, a mailbox key or a grant sealed with
-- one; a key of it, or a mailbox of it given to a key; an invite into it; and
-- a flag a person or a key holds on one of its mailboxes going from 0 to 1.
-- An upsert runs the insert's trigger, so a grant is written in a closed team
-- by no insert at all. What takes something away is allowed — a flag taken,
-- a key's mailbox taken out of it, a key revoked, a mailbox removed, a member
-- removed — and so is every change to the team's memberships, so that
-- whatever keeps a team from another source in step with it applies them
-- while it is closed.

ALTER TABLE workspaces ADD COLUMN closed_at INTEGER NOT NULL DEFAULT 0
  CHECK (closed_at = 0 OR kind = 'team'); -- when the team closed; 0 while it is open, and for every
                                          -- workspace that is not a team, which never closes

CREATE TRIGGER accounts_closed_team BEFORE INSERT ON accounts
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER mailbox_access_closed_team BEFORE INSERT ON mailbox_access
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;
CREATE TRIGGER mailbox_access_closed_team_flags BEFORE UPDATE OF read, act, send, manage ON mailbox_access
WHEN ((NEW.read = 1 AND OLD.read = 0) OR (NEW.act = 1 AND OLD.act = 0)
      OR (NEW.send = 1 AND OLD.send = 0) OR (NEW.manage = 1 AND OLD.manage = 0))
 AND EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER mailbox_keys_closed_team BEFORE INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
              WHERE a.id = NEW.account_id AND w.closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER mailbox_grants_closed_team BEFORE INSERT ON mailbox_grants
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER api_keys_closed_team BEFORE INSERT ON api_keys
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER key_access_closed_team BEFORE INSERT ON key_access
WHEN EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;
CREATE TRIGGER key_access_closed_team_flags BEFORE UPDATE OF read, act, send ON key_access
WHEN ((NEW.read = 1 AND OLD.read = 0) OR (NEW.act = 1 AND OLD.act = 0) OR (NEW.send = 1 AND OLD.send = 0))
 AND EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;

CREATE TRIGGER invites_closed_team BEFORE INSERT ON invites
WHEN NEW.workspace_id IS NOT NULL
 AND EXISTS (SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND closed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is linked, granted, keyed or invited in a closed team');
END;
