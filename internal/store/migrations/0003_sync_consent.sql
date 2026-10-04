-- Phase 2: the sync engine starts storing message metadata, and only with
-- consent.
--
-- The privacy policy promises that nothing about a person's messages is kept
-- until they agree to it in the console. Consent is per person and covers
-- every mailbox they own; a mailbox nobody owns (created by an instance key)
-- syncs only once the operator switches it on, and who did that is recorded.
-- The engine reads one rule, in store/eligibility.go:
--   state = 'active' AND (owner active and consented OR, when unowned, sync_enabled_at <> 0)
--
-- ALTER-only, like 0002: rebuilding accounts or users would be a DROP TABLE,
-- and with foreign keys on that is a DELETE cascading into credentials,
-- folders, messages and sessions.

ALTER TABLE users ADD COLUMN sync_consent_at INTEGER NOT NULL DEFAULT 0;       -- 0: not consented (or withdrawn)
ALTER TABLE users ADD COLUMN sync_consent_version TEXT NOT NULL DEFAULT '';   -- the policy revision agreed to

ALTER TABLE accounts ADD COLUMN sync_enabled_at INTEGER NOT NULL DEFAULT 0;   -- instance-owned accounts: 0 = off
ALTER TABLE accounts ADD COLUMN sync_enabled_by TEXT NOT NULL DEFAULT '';     -- 'key:<prefix>' or 'cli': who switched it on

-- Initial sync progress per folder: how many UIDs the initial window found,
-- and how many of them are indexed so far. A percentage needs both, and
-- local_count alone also counts mail that arrived meanwhile.
ALTER TABLE folders ADD COLUMN initial_total   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE folders ADD COLUMN initial_fetched INTEGER NOT NULL DEFAULT 0;

-- Where a folder's initial window starts, fixed when its initial sync starts
-- (0: everything). A person agreed to "the last 90 days" on the day they
-- turned sync on; a window recomputed from the clock at every pass would
-- slide, and whatever a later pass looked at would depend on when it ran.
ALTER TABLE folders ADD COLUMN initial_since INTEGER NOT NULL DEFAULT 0;
-- A UIDVALIDITY resync in progress, and what the folder was when it began:
-- 'live', or 'initial' for one whose initial sync had not finished. Kept
-- until the resync ends, so a resync resumed after a dropped connection
-- still ends the way the first attempt would have: resync_done or
-- initial_done.
ALTER TABLE folders ADD COLUMN resync_from TEXT NOT NULL DEFAULT '' CHECK (resync_from IN ('', 'live', 'initial'));

-- The end of a UIDVALIDITY resync deletes what no new UID claimed.
CREATE INDEX messages_folder_stale ON messages(folder_id) WHERE stale = 1;

-- A deletion from the full-text index removes the terms from its segments
-- then and there, instead of recording a marker and keeping them until some
-- later merge. Without this, a subject or an address deleted from messages —
-- by an expunge, by turning sync off, by removing a mailbox — stays in the
-- file inside an older segment, and secure_delete and the WAL checkpoint
-- cannot reach it, because as far as SQLite is concerned that page is live.
-- Persistent: FTS5 keeps it in messages_fts_config.
INSERT INTO messages_fts(messages_fts, rank) VALUES ('secure-delete', 1);
