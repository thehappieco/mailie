-- A removed mailbox, kept for 30 days and then deleted for good, and the
-- journal of what was done to mailboxes, with the notices it gives their
-- readers (docs/workspaces.md, "Removed mailboxes").
--
-- An ordinary migration: four columns, three tables and their triggers are
-- added, nothing else changes, and no row of a schema-19 database does:
-- every mailbox is live (removed_at, purge_after and sync_held_at are 0 and
-- removed_by is ''), and the journal and the notices start empty.
--
-- Removing a mailbox no longer deletes it. Its row stays, with its
-- credentials, folders, messages, drafts, sends, key, grants and consent,
-- for 30 days at least (purge_after), and is used by nothing meanwhile: no
-- person or key reaches it, it syncs nothing, it has no reader and protects
-- none (store.LiveSQL). Within those days it may be recovered, which clears
-- the removal and holds its sync (sync_held_at) until someone who may turns
-- it on; after them, the hourly sweep deletes it for good. Who may remove,
-- recover or delete for good, and when, is the service's.
--
--   - accounts.removed_at, removed_by and purge_after record a removal,
--     whole: all three while it is removed (the moment, who removed it, and
--     when it may be deleted for good, 30 days or more after), none while it
--     is live, and its moment and its end never move until it is recovered.
--     removed_by is "usr_…", "key:<prefix>" or "cli", '' once that person
--     is deleted. A mailbox is always linked live.
--   - accounts.sync_held_at is when a recovery held the mailbox's sync; 0
--     when nothing holds it. A held mailbox syncs nothing until it is
--     turned on again, which clears it.
--   - mailbox_events: what was done to a mailbox — removed, sync_paused
--     (sync turned off, or a consent bound to a person stopped by them),
--     recovered, purged — when, in which workspace and by whom ("usr_…",
--     "key:<prefix>", "cli" or "sweep"; '' once that person is deleted),
--     with the mailbox's name (label) and a masked hint of its address
--     ("a…@example.com"), both '' once it is deleted for good, and never
--     its address: no foreign keys, so a row outlives the mailbox and its
--     team. Written once: only the label, the hint and the actor change,
--     and only to ''.
--   - mailbox_event_readers: who read the mailbox when it happened.
--   - notices: what a person is told in the console, a row per reader of a
--     removal or a sync turned off, never for the person who did it, until
--     they dismiss it.
--
-- The triggers hold what the service and the repository refuse: nothing is
-- given on a removed mailbox. Each refuses a row that would give it
-- something it did not have: a grant on it, its next key or a grant sealed
-- with one, a key restricted to it or given it, and a flag a person or a key
-- holds on it going from 0 to 1. An upsert runs the insert's trigger, so a
-- grant is written on a removed mailbox by no insert at all. What takes
-- something away is allowed. The credentials are not held: a token refresh
-- already under way when the mailbox was removed still records the refresh
-- token the provider rotated, which a recovery needs.

ALTER TABLE accounts ADD COLUMN removed_at   INTEGER NOT NULL DEFAULT 0;   -- when it was removed; 0 while live
ALTER TABLE accounts ADD COLUMN removed_by   TEXT NOT NULL DEFAULT '';    -- 'usr_…', 'key:<prefix>' or 'cli'; '' while live,
                                                                          -- and once that person is deleted
ALTER TABLE accounts ADD COLUMN purge_after  INTEGER NOT NULL DEFAULT 0;   -- when it may be deleted for good; 0 while live
ALTER TABLE accounts ADD COLUMN sync_held_at INTEGER NOT NULL DEFAULT 0;   -- when a recovery held its sync; 0 when not held

-- What the hourly sweep reads: the removed mailboxes, by when they go.
CREATE INDEX accounts_purge ON accounts(purge_after) WHERE removed_at <> 0;

CREATE TRIGGER accounts_linked_live BEFORE INSERT ON accounts
WHEN NEW.removed_at <> 0 OR NEW.removed_by <> '' OR NEW.purge_after <> 0 OR NEW.sync_held_at <> 0
BEGIN
  SELECT RAISE(ABORT, 'a mailbox is linked live');
END;

-- 2592000 seconds are 30 days.
CREATE TRIGGER accounts_removal_whole BEFORE UPDATE OF removed_at, removed_by, purge_after ON accounts
WHEN (NEW.removed_at = 0) <> (NEW.purge_after = 0)
  OR (NEW.removed_at = 0 AND NEW.removed_by <> '')
  OR (NEW.removed_at <> 0 AND NEW.purge_after - NEW.removed_at < 2592000)
BEGIN
  SELECT RAISE(ABORT, 'a removed mailbox is kept for 30 days, recorded whole');
END;
CREATE TRIGGER accounts_removal_fixed BEFORE UPDATE OF removed_at, removed_by, purge_after ON accounts
WHEN OLD.removed_at <> 0 AND NEW.removed_at <> 0
 AND (NEW.removed_at IS NOT OLD.removed_at OR NEW.purge_after IS NOT OLD.purge_after
      OR (NEW.removed_by IS NOT OLD.removed_by AND NEW.removed_by <> ''))
BEGIN
  SELECT RAISE(ABORT, 'a removal is recorded once: it never moves until the mailbox is recovered');
END;

CREATE TRIGGER mailbox_access_removed BEFORE INSERT ON mailbox_access
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;
CREATE TRIGGER mailbox_access_removed_flags BEFORE UPDATE OF read, act, send, manage ON mailbox_access
WHEN ((NEW.read = 1 AND OLD.read = 0) OR (NEW.act = 1 AND OLD.act = 0)
      OR (NEW.send = 1 AND OLD.send = 0) OR (NEW.manage = 1 AND OLD.manage = 0))
 AND EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;

CREATE TRIGGER mailbox_keys_removed BEFORE INSERT ON mailbox_keys
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;

CREATE TRIGGER mailbox_grants_removed BEFORE INSERT ON mailbox_grants
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;

CREATE TRIGGER api_key_accounts_removed BEFORE INSERT ON api_key_accounts
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;

CREATE TRIGGER key_access_removed BEFORE INSERT ON key_access
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;
CREATE TRIGGER key_access_removed_flags BEFORE UPDATE OF read, act, send ON key_access
WHEN ((NEW.read = 1 AND OLD.read = 0) OR (NEW.act = 1 AND OLD.act = 0) OR (NEW.send = 1 AND OLD.send = 0))
 AND EXISTS (SELECT 1 FROM accounts WHERE id = NEW.account_id AND removed_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'nothing is granted, keyed or given on a removed mailbox');
END;

CREATE TABLE mailbox_events (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused: notices and readers name it
  id           TEXT NOT NULL UNIQUE                -- 'mbe_' + 16 hex: what the console names
               CHECK (id GLOB 'mbe_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
  kind         TEXT NOT NULL CHECK (kind IN ('removed', 'sync_paused', 'recovered', 'purged')),
  workspace_id TEXT NOT NULL,                      -- no foreign keys: a row outlives the mailbox and its team
  account_id   TEXT NOT NULL,
  label        TEXT NOT NULL DEFAULT ''            -- the mailbox's name; '' once deleted for good, or when it
               CHECK (instr(label, '@') = 0),      -- held an address, which the journal never does
  hint         TEXT NOT NULL DEFAULT ''            -- 'a…@example.com'; '' once deleted for good
               CHECK (hint = '' OR (substr(hint, 2, 2) = '…@' AND length(hint) > 3 AND instr(substr(hint, 4), '@') = 0)),
  actor        TEXT NOT NULL DEFAULT '',           -- 'usr_…', 'key:<prefix>', 'cli' or 'sweep'; '' once that person is deleted
  created_at   INTEGER NOT NULL
);
CREATE INDEX mailbox_events_account ON mailbox_events(account_id);
CREATE INDEX mailbox_events_created ON mailbox_events(created_at);

CREATE TRIGGER mailbox_events_written_once BEFORE UPDATE ON mailbox_events
WHEN NEW.seq IS NOT OLD.seq OR NEW.id IS NOT OLD.id OR NEW.kind IS NOT OLD.kind
  OR NEW.workspace_id IS NOT OLD.workspace_id OR NEW.account_id IS NOT OLD.account_id
  OR NEW.created_at IS NOT OLD.created_at
  OR (NEW.label IS NOT OLD.label AND NEW.label <> '')
  OR (NEW.hint IS NOT OLD.hint AND NEW.hint <> '')
  OR (NEW.actor IS NOT OLD.actor AND NEW.actor <> '')
BEGIN
  SELECT RAISE(ABORT, 'a mailbox event is written once');
END;

CREATE TABLE mailbox_event_readers (
  event_seq INTEGER NOT NULL REFERENCES mailbox_events(seq) ON DELETE CASCADE,
  user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (event_seq, user_id)
);
CREATE INDEX mailbox_event_readers_user ON mailbox_event_readers(user_id);

CREATE TRIGGER mailbox_event_readers_fixed BEFORE UPDATE ON mailbox_event_readers
BEGIN
  SELECT RAISE(ABORT, 'who read a mailbox when something happened to it is written once');
END;

CREATE TABLE notices (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  event_seq  INTEGER NOT NULL REFERENCES mailbox_events(seq) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, event_seq)
);
CREATE INDEX notices_event ON notices(event_seq);

-- A notice goes to someone who read the mailbox when it was removed or its
-- sync was turned off, and never to the person who did it.
CREATE TRIGGER notices_to_readers BEFORE INSERT ON notices
WHEN NOT EXISTS (SELECT 1 FROM mailbox_event_readers r WHERE r.event_seq = NEW.event_seq AND r.user_id = NEW.user_id)
  OR NOT EXISTS (SELECT 1 FROM mailbox_events e
                  WHERE e.seq = NEW.event_seq AND e.kind IN ('removed', 'sync_paused') AND e.actor IS NOT NEW.user_id)
BEGIN
  SELECT RAISE(ABORT, 'a notice goes to a reader of the mailbox, never to who acted');
END;
CREATE TRIGGER notices_fixed BEFORE UPDATE ON notices
BEGIN
  SELECT RAISE(ABORT, 'a notice is written once');
END;
