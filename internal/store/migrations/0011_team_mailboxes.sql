-- A team mailbox belongs to its workspace (docs/workspaces.md).
--
-- Until now accounts.owner_user_id meant "linked by": the person who linked a
-- mailbox, whose own consent to sync it synced under, and who could not leave
-- the team while it was linked. From here on it means what its name says
-- again: the person whose mailbox it is, set exactly for a mailbox of that
-- person's personal workspace. A team mailbox names no person:
--
--   - it syncs under its workspace's consent, which an owner or an admin of
--     the team gives on the team's behalf, to a revision of the sync text, and
--     which any of them may withdraw, deleting its index for every reader;
--     recorded on the mailbox in sync_enabled_at, sync_enabled_by (who gave
--     it, an id and nothing else) and sync_consent_version (the revision);
--   - who linked it is attribution only, in linked_by ('usr_…',
--     'key:<prefix>' or 'cli'), blanked when that person is deleted;
--   - owners and admins manage every mailbox of their workspace by their
--     role, so manage is stored for members only.
--
-- An operator mailbox keeps its switch in the same columns, as before, with
-- no revision.
--
-- What becomes of a schema-10 database:
--
--   - A personal mailbox: nothing. Its person syncs it under their own
--     consent, as before; the trigger below proves owner_user_id names the
--     person of its workspace on every row.
--   - A team mailbox whose linker is active and consented: their consent is
--     copied to the mailbox, with its date and revision, so it keeps syncing.
--     It stays bound to that person (sync_enabled_via = 'migration',
--     sync_enabled_by = them) until an owner or an admin confirms the team's
--     consent at the current revision: while bound, the person withdrawing
--     their consent, or being disabled or deleted on the instance, stops the
--     mailbox and deletes its index, as the text they agreed to promised.
--   - A team mailbox whose linker consented and is disabled on the instance:
--     it was not syncing, and its index was kept. Nothing is copied: it stays
--     stopped with its index, bound to that person in the same way, until an
--     owner or an admin turns its sync on at the current revision (or off,
--     or removes it); deleting that person deletes the index. One that only
--     that person read can never be read again, nor turned on: it can only be
--     removed, or turned off to delete its index. The daemon lists both at
--     start.
--   - A team mailbox whose linker never consented, or withdrew: no consent,
--     no index; sync stays off until an owner or an admin turns it on.
--   - Grants: the linker keeps theirs as an ordinary grant. Owners and admins
--     lose a stored manage, which their role gives them now, and a grant of
--     manage alone goes; a member's manage stays.
--   - A pending invite whose creator could not make it now (deleted, no
--     longer active, no longer an instance owner, no longer an owner or an
--     admin of the team allowed that role) is expired; the hourly sweep
--     deletes it 30 days on.
--
-- An ordinary migration, in one transaction with foreign keys on: columns
-- added, rows updated, triggers replaced, nothing dropped but two triggers.

ALTER TABLE accounts ADD COLUMN sync_consent_version TEXT NOT NULL DEFAULT '';  -- the sync text revision the workspace agreed to; '' for an operator mailbox
ALTER TABLE accounts ADD COLUMN sync_enabled_via TEXT NOT NULL DEFAULT ''        -- 'migration': copied from its linker's own consent, and bound to them
  CHECK (sync_enabled_via IN ('', 'migration'));
ALTER TABLE accounts ADD COLUMN linked_by TEXT NOT NULL DEFAULT '';             -- attribution only: 'usr_…', 'key:<prefix>' or 'cli'; '' once that person is deleted

UPDATE accounts SET linked_by = owner_user_id WHERE owner_user_id IS NOT NULL;

-- A team mailbox's linker's consent becomes the workspace's, as it was given,
-- where the linker is active; bound to them until the team confirms it.
UPDATE accounts
   SET sync_enabled_at      = (SELECT u.sync_consent_at FROM users u WHERE u.id = accounts.owner_user_id),
       sync_consent_version = (SELECT u.sync_consent_version FROM users u WHERE u.id = accounts.owner_user_id),
       sync_enabled_by      = accounts.owner_user_id,
       sync_enabled_via     = 'migration'
 WHERE workspace_id IN (SELECT id FROM workspaces WHERE kind = 'team')
   AND EXISTS (SELECT 1 FROM users u WHERE u.id = accounts.owner_user_id
                 AND u.status = 'active' AND u.sync_consent_at <> 0);

-- A disabled linker's: stopped, its index kept, bound to them all the same.
UPDATE accounts
   SET sync_enabled_by  = accounts.owner_user_id,
       sync_enabled_via = 'migration'
 WHERE workspace_id IN (SELECT id FROM workspaces WHERE kind = 'team')
   AND EXISTS (SELECT 1 FROM users u WHERE u.id = accounts.owner_user_id
                 AND u.status <> 'active' AND u.sync_consent_at <> 0);

DROP TRIGGER accounts_linker_on_insert;
DROP TRIGGER accounts_linker_on_update;

UPDATE accounts SET owner_user_id = NULL WHERE workspace_id IN (SELECT id FROM workspaces WHERE kind = 'team');

-- A mailbox names a person exactly when it is in that person's personal
-- workspace: NULL in a team's and the operator's (whose person_id is NULL).
CREATE TRIGGER accounts_person_on_insert BEFORE INSERT ON accounts
WHEN NEW.owner_user_id IS NOT (SELECT person_id FROM workspaces WHERE id = NEW.workspace_id)
BEGIN
  SELECT RAISE(ABORT, 'a mailbox names a person exactly when it is in that person''s personal workspace');
END;
CREATE TRIGGER accounts_person_on_update BEFORE UPDATE OF owner_user_id ON accounts
WHEN NEW.owner_user_id IS NOT (SELECT person_id FROM workspaces WHERE id = NEW.workspace_id)
BEGIN
  SELECT RAISE(ABORT, 'a mailbox names a person exactly when it is in that person''s personal workspace');
END;
-- Every row through the new rule: one that breaks it fails the migration,
-- which then changes nothing.
UPDATE accounts SET owner_user_id = owner_user_id;

-- Owners and admins manage every mailbox of their workspace by their role:
-- manage is stored for members only.
DELETE FROM mailbox_access
 WHERE read = 0 AND act = 0 AND send = 0
   AND EXISTS (SELECT 1 FROM workspace_members m
                WHERE m.workspace_id = mailbox_access.workspace_id AND m.user_id = mailbox_access.user_id
                  AND m.role IN ('owner', 'admin'));
UPDATE mailbox_access SET manage = 0
 WHERE manage = 1
   AND EXISTS (SELECT 1 FROM workspace_members m
                WHERE m.workspace_id = mailbox_access.workspace_id AND m.user_id = mailbox_access.user_id
                  AND m.role IN ('owner', 'admin'));

CREATE TRIGGER mailbox_access_manage_insert BEFORE INSERT ON mailbox_access
WHEN NEW.manage = 1
 AND EXISTS (SELECT 1 FROM workspace_members m
              WHERE m.workspace_id = NEW.workspace_id AND m.user_id = NEW.user_id AND m.role IN ('owner', 'admin'))
BEGIN
  SELECT RAISE(ABORT, 'owners and admins manage every mailbox of their workspace by their role; manage is stored for members only');
END;
CREATE TRIGGER mailbox_access_manage_update BEFORE UPDATE OF manage ON mailbox_access
WHEN NEW.manage = 1
 AND EXISTS (SELECT 1 FROM workspace_members m
              WHERE m.workspace_id = NEW.workspace_id AND m.user_id = NEW.user_id AND m.role IN ('owner', 'admin'))
BEGIN
  SELECT RAISE(ABORT, 'owners and admins manage every mailbox of their workspace by their role; manage is stored for members only');
END;
-- A member made an owner or an admin manages by the role from then on: a
-- stored manage goes, and a grant that held nothing else with it.
CREATE TRIGGER workspace_members_promoted AFTER UPDATE OF role ON workspace_members
WHEN NEW.role IN ('owner', 'admin')
BEGIN
  DELETE FROM mailbox_access
   WHERE workspace_id = NEW.workspace_id AND user_id = NEW.user_id AND read = 0 AND act = 0 AND send = 0;
  UPDATE mailbox_access SET manage = 0
   WHERE workspace_id = NEW.workspace_id AND user_id = NEW.user_id AND manage = 1;
END;

-- A pending invite whose creator could not make it today: one deleted since
-- (schema 10 blanked created_by and left the invite pending; nothing ever
-- writes '' otherwise), one no longer active on the instance, an instance
-- invite from someone no longer an instance owner, a team invite from someone
-- no longer an active owner of the team, or an admin of it inviting a member.
-- The operator's ('cli', 'key:<prefix>') are untouched.
UPDATE invites SET expires_at = unixepoch()
 WHERE used_at = 0 AND expires_at > unixepoch()
   AND (created_by = ''
     OR (created_by LIKE 'usr!_%' ESCAPE '!'
         AND (NOT EXISTS (SELECT 1 FROM users u WHERE u.id = invites.created_by AND u.status = 'active')
           OR (invites.workspace_id IS NULL
               AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = invites.created_by AND u.role = 'owner'))
           OR (invites.workspace_id IS NOT NULL
               AND NOT EXISTS (SELECT 1 FROM workspace_members m
                                WHERE m.workspace_id = invites.workspace_id AND m.user_id = invites.created_by
                                  AND m.status = 'active'
                                  AND (m.role = 'owner' OR (m.role = 'admin' AND invites.workspace_role = 'member')))))));
