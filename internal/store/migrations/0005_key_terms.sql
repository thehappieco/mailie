-- Phase 4: people create their own API keys in the console, for a tool — an
-- AI assistant through the MCP server, say — to read their mailboxes and, with
-- the write scope, act on them.
--
-- Creating a key is the person's agreement to what a tool holding it can do,
-- so the key records which text they were shown when they agreed, and who
-- created it: 'usr_…' for a person in the console, 'key:<prefix>' for an
-- administrator's key, 'cli' for the bootstrap. Keys from before this
-- migration carry '' for both: nobody agreed to anything through them.
--
-- ALTER-only, like 0002 to 0004: rebuilding api_keys would be a DROP TABLE,
-- and with foreign keys on that is a DELETE cascading into the restrictions.

ALTER TABLE api_keys ADD COLUMN terms_version TEXT NOT NULL DEFAULT ''; -- the key terms revision agreed to
ALTER TABLE api_keys ADD COLUMN created_by TEXT NOT NULL DEFAULT '';    -- 'usr_…', 'key:<prefix>' or 'cli'

-- A person's keys are listed and counted by person.
CREATE INDEX api_keys_user ON api_keys(user_id);

-- No restriction rows means every account. A mailbox removed takes its
-- restriction rows with it (ON DELETE CASCADE), so a key restricted to that
-- one mailbox would quietly become a key for every mailbox its owner has.
-- Revoked instead, when the last of its mailboxes goes: a key that was for
-- nothing but that mailbox is for nothing now.
CREATE TRIGGER api_key_accounts_last AFTER DELETE ON api_key_accounts
WHEN NOT EXISTS (SELECT 1 FROM api_key_accounts WHERE key_prefix = OLD.key_prefix)
BEGIN
  UPDATE api_keys SET revoked_at = unixepoch() WHERE prefix = OLD.key_prefix AND revoked_at = 0;
END;
