-- Mailbox keys and the grants sealed to people (docs/key-scheme.md, sections
-- 3.2, 3.3, 8, 9 and 12.11 to 12.15).
--
-- An ordinary migration: two tables and their triggers are added, nothing
-- else changes. Every mailbox that exists now stays without a key, and is
-- read as before, by the flag alone (section 12.14); a person who reads one
-- and has enrolled writes its first key from the console.
--
--   - mailbox_keys: a mailbox's key pair at one epoch, as the linker's or the
--     writer's browser made it: the public key and the namespace, never the
--     private key, which only grants carry, sealed. Written once: a row is
--     never changed, but for its writer's name, blanked when that person is
--     deleted (created_by, attribution only). The first row of a mailbox is
--     epoch 1, and each next one the epoch after the highest (section 3.3);
--     the mailbox's current epoch is its highest, held nowhere else. Every
--     epoch of a mailbox has the same namespace, which no other mailbox uses
--     (section 3.2). An operator mailbox never gets one (section 12.15): there
--     is no person to hold its key. Older epochs' rows stay; only the grants
--     at older epochs go (section 12.12).
--   - mailbox_grants: a mailbox's private key at one epoch, sealed to one
--     person's account public key: 88 bytes the server stores, hands back to
--     that person and cannot open (section 9.3, whose full shape the code
--     checks; the schema holds the length and the magic). Written once, and
--     for a member of the mailbox's own workspace only: two foreign keys make
--     that a property of the schema, as mailbox_access's do, so the operator
--     workspace, which has no members, can hold none. Removing the member,
--     the mailbox or the key row takes the grant with it; taking "read"
--     without removing the membership (a revoke, a membership disabled, the
--     person disabled or reset) deletes it in the code's transaction that
--     takes the flag (section 12.13). Who gave it is attribution only,
--     blanked when that person is deleted.
--
-- Who reads a mailbox is decided from both tables with mailbox_access, by one
-- rule the code holds in one place (store.ReaderSQL): the read flag, as an
-- active member active on the instance, and, on a mailbox that has a key, a
-- grant at its current epoch. No view carries it: the rebuild runner refuses
-- a schema with one.

CREATE TABLE mailbox_keys (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  epoch      INTEGER NOT NULL CHECK (epoch BETWEEN 1 AND 65535),
  public_key BLOB NOT NULL CHECK (typeof(public_key) = 'blob' AND length(public_key) = 32),
  namespace  TEXT NOT NULL CHECK (namespace GLOB
    '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-4[0-9a-f][0-9a-f][0-9a-f]-[89ab][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
  created_by TEXT NOT NULL DEFAULT '',  -- the writer's 'usr_…'; '' once that person is deleted
  created_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, epoch)
);
CREATE INDEX mailbox_keys_namespace ON mailbox_keys(namespace);

-- Written once. The one change is the attribution's, which a person's
-- deletion blanks, as it blanks who granted and who linked.
CREATE TRIGGER mailbox_keys_written_once BEFORE UPDATE ON mailbox_keys
WHEN NEW.account_id IS NOT OLD.account_id OR NEW.epoch IS NOT OLD.epoch OR NEW.public_key IS NOT OLD.public_key
  OR NEW.namespace IS NOT OLD.namespace OR NEW.created_at IS NOT OLD.created_at
  OR (NEW.created_by IS NOT OLD.created_by AND NEW.created_by <> '')
BEGIN
  SELECT RAISE(ABORT, 'a mailbox key is written once');
END;
-- Epoch 1 first, then each the one after the highest: never one skipped,
-- never one written twice.
CREATE TRIGGER mailbox_keys_next_epoch BEFORE INSERT ON mailbox_keys
WHEN NEW.epoch IS NOT (SELECT coalesce(max(k.epoch), 0) + 1 FROM mailbox_keys k WHERE k.account_id = NEW.account_id)
BEGIN
  SELECT RAISE(ABORT, 'a mailbox key''s epoch is the one after its current, 1 for its first');
END;
-- One namespace per mailbox, the same for every epoch...
CREATE TRIGGER mailbox_keys_one_namespace BEFORE INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM mailbox_keys k WHERE k.account_id = NEW.account_id AND k.namespace <> NEW.namespace)
BEGIN
  SELECT RAISE(ABORT, 'every epoch of a mailbox key has the mailbox''s one namespace');
END;
-- ...that no other mailbox uses.
CREATE TRIGGER mailbox_keys_own_namespace BEFORE INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM mailbox_keys k WHERE k.namespace = NEW.namespace AND k.account_id <> NEW.account_id)
BEGIN
  SELECT RAISE(ABORT, 'a mailbox''s namespace is used by no other mailbox');
END;
-- An operator mailbox has no key in phase 3: there is no person to hold one.
CREATE TRIGGER mailbox_keys_not_operator BEFORE INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM accounts a WHERE a.id = NEW.account_id AND a.workspace_id = 'wsp_operator')
BEGIN
  SELECT RAISE(ABORT, 'an operator mailbox has no key');
END;

CREATE TABLE mailbox_grants (
  account_id   TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  epoch        INTEGER NOT NULL,
  grant        BLOB NOT NULL CHECK (typeof(grant) = 'blob' AND length(grant) = 88 AND substr(grant, 1, 2) = x'4d4c'),
  granted_by   TEXT NOT NULL DEFAULT '',  -- 'usr_…'; '' once that person is deleted
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (account_id, user_id, epoch),
  FOREIGN KEY (account_id, epoch) REFERENCES mailbox_keys(account_id, epoch) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE,
  FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX mailbox_grants_user ON mailbox_grants(user_id);
CREATE INDEX mailbox_grants_member ON mailbox_grants(workspace_id, user_id);
CREATE INDEX mailbox_grants_key ON mailbox_grants(account_id, epoch);

-- Written once, as a key is: giving "read" again after taking it writes a
-- new grant. The one change is the attribution's.
CREATE TRIGGER mailbox_grants_written_once BEFORE UPDATE ON mailbox_grants
WHEN NEW.account_id IS NOT OLD.account_id OR NEW.workspace_id IS NOT OLD.workspace_id
  OR NEW.user_id IS NOT OLD.user_id OR NEW.epoch IS NOT OLD.epoch OR NEW.grant IS NOT OLD.grant
  OR NEW.created_at IS NOT OLD.created_at
  OR (NEW.granted_by IS NOT OLD.granted_by AND NEW.granted_by <> '')
BEGIN
  SELECT RAISE(ABORT, 'a grant is written once');
END;
