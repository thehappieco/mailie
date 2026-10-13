-- Why an API key stopped (docs/workspaces.md, "API keys").
--
-- A key stops when someone revokes it, or when what it stands on goes: the
-- person who created it leaves its workspace or is closed, or the last
-- mailbox it could reach is removed. Until now only the moment was kept, and
-- a listing of revoked keys could not tell a key someone revoked from one
-- that stopped with its creator. From here on a key says why it stopped, in
-- the same write that stops it, once:
--
--   - manual: revoked by the operator, by an owner or an admin of its
--     workspace, or by the person who created it;
--   - creator_left: the person who created it left its workspace, or was
--     removed from it;
--   - creator_disabled: the person who created it was disabled on this
--     server;
--   - creator_suspended: the person who created it was suspended by an
--     extension of the daemon;
--   - creator_deleted: the person who created it was deleted;
--   - account_recovered: the account of the person who created it was
--     recovered, which stops the keys they had created before;
--   - restriction_emptied: the last mailbox it could reach was removed: an
--     operator key restricted to mailboxes that are all gone, or a key
--     carried over from a person's (migration 0012) losing its last one;
--   - migration: reserved for a migration that stops keys.
--
-- An ordinary migration: a column and two triggers are added, and two
-- triggers are dropped and created again, each exactly as it was but for the
-- reason it now writes (api_key_accounts_last as migration 0012 left it, and
-- key_access_carried_over_last as 0012 made it). No row of a schema-16
-- database changes: a key revoked before this migration keeps '' and says no
-- reason, since nothing recorded one. Every write that revokes a key names
-- its reason, the schema's triggers included: a key that stops without one
-- is refused, and a reason, once written, never changes.

ALTER TABLE api_keys ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''
  CHECK (stop_reason IN ('', 'manual', 'creator_left', 'creator_disabled', 'creator_suspended',
                         'creator_deleted', 'account_recovered', 'restriction_emptied', 'migration')
         AND (stop_reason = '' OR revoked_at <> 0)); -- why the key stopped; '' while live, and for a key
                                                     -- revoked before this migration

CREATE TRIGGER api_keys_stop_says_why BEFORE UPDATE OF revoked_at ON api_keys
WHEN OLD.revoked_at = 0 AND NEW.revoked_at <> 0 AND NEW.stop_reason = ''
BEGIN
  SELECT RAISE(ABORT, 'a key that stops says why');
END;
CREATE TRIGGER api_keys_stop_reason_fixed BEFORE UPDATE OF stop_reason ON api_keys
WHEN OLD.stop_reason <> '' AND NEW.stop_reason IS NOT OLD.stop_reason
BEGIN
  SELECT RAISE(ABORT, 'why a key stopped never changes');
END;

-- An operator key restricted to mailboxes that are all gone reaches nothing:
-- revoked, rather than read as a key for every mailbox.
DROP TRIGGER api_key_accounts_last;
CREATE TRIGGER api_key_accounts_last AFTER DELETE ON api_key_accounts
WHEN NOT EXISTS (SELECT 1 FROM api_key_accounts WHERE key_prefix = OLD.key_prefix)
BEGIN
  UPDATE api_keys SET revoked_at = unixepoch(), stop_reason = 'restriction_emptied'
   WHERE prefix = OLD.key_prefix AND revoked_at = 0;
END;

-- A carried-over key whose last mailbox goes is revoked: it gains none, so it
-- would reach nothing again.
DROP TRIGGER key_access_carried_over_last;
CREATE TRIGGER key_access_carried_over_last AFTER DELETE ON key_access
WHEN EXISTS (SELECT 1 FROM api_keys WHERE prefix = OLD.key_prefix AND workspace_id IS NULL)
 AND NOT EXISTS (SELECT 1 FROM key_access WHERE key_prefix = OLD.key_prefix)
BEGIN
  UPDATE api_keys SET revoked_at = unixepoch(), stop_reason = 'restriction_emptied'
   WHERE prefix = OLD.key_prefix AND revoked_at = 0;
END;
