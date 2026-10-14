-- migration: rebuild
--
-- The journal of what was done to mailboxes (migration 0020) gains a fifth
-- kind of entry, a stranding, and three columns, and is read per workspace
-- from a cursor (docs/workspaces.md, "Removed mailboxes" and "Following an
-- external directory").
--
--   - kind 'stranded': a team mailbox left with nobody who reads it, or would
--     read it once resumed or but for a hold, because the one person who did
--     lost it: an external directory ended their membership or forgot them,
--     or they were disabled, deleted or had their account key reset with
--     force. Nobody is told of it in the console (notices_to_readers admits
--     only a removal and a sync turned off); an extension reads it.
--   - cause: why it was written. A stranding's is 'membership_ended',
--     'forgotten', 'disabled', 'deleted' or 'reset'; a sync turned off by a
--     person's consent bound to it stopping with them (store.StopBoundTx) is
--     'bound_stopped', and any other sync turned off ''; every other kind has
--     none.
--   - lost_user: the person a stranding lost, while they exist and until an
--     extension that follows them has delivered it; '' on every other kind,
--     and once forgotten.
--   - purge_after: when a removed mailbox may be deleted for good, as its
--     removal recorded it; 0 on every other kind. A removal recorded before
--     this migration gets its moment and 30 days, exactly what the removal
--     wrote then (account.Repository.remove writes both from one moment).
--   - mailbox_events_workspace: the journal of one workspace, in order.
--
-- A CHECK constraint is added only by rebuilding a table, so this is a
-- rebuild (store/migrate.go, applyRebuild): foreign keys off outside the
-- transaction, so dropping the old journal cascades into nothing; every row
-- copied with its seq and every column named; mailbox_event_readers and
-- notices name the journal by name and refer to the new table once it
-- carries it. notices_to_readers names the journal in its body, which a
-- rename refuses while the table it names is gone, so it is dropped first and
-- written again, byte for byte. The journal's AUTOINCREMENT high-water mark
-- (sqlite_sequence) is carried over as it was, a journal never written having
-- none, so no seq is ever handed out twice, even one swept before the
-- rebuild. Written once, as before: only the label, the hint, the actor and
-- now the lost person change, and only to ''.
--
-- Every row keeps every value; no other table changes.

DROP TRIGGER notices_to_readers;

CREATE TABLE mailbox_events_new (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused: notices and readers name it
  id           TEXT NOT NULL UNIQUE                -- 'mbe_' + 16 hex: what the console names
               CHECK (id GLOB 'mbe_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
  kind         TEXT NOT NULL CHECK (kind IN ('removed', 'sync_paused', 'recovered', 'purged', 'stranded')),
  workspace_id TEXT NOT NULL,                      -- no foreign keys: a row outlives the mailbox and its team
  account_id   TEXT NOT NULL,
  label        TEXT NOT NULL DEFAULT ''            -- the mailbox's name; '' once deleted for good, or when it
               CHECK (instr(label, '@') = 0),      -- held an address, which the journal never does
  hint         TEXT NOT NULL DEFAULT ''            -- 'a…@example.com'; '' once deleted for good and no notice needs it
               CHECK (hint = '' OR (substr(hint, 2, 2) = '…@' AND length(hint) > 3 AND instr(substr(hint, 4), '@') = 0)),
  actor        TEXT NOT NULL DEFAULT '',           -- 'usr_…', 'key:<prefix>', 'cli', 'sweep' or 'directory';
                                                   -- '' once that person is forgotten
  created_at   INTEGER NOT NULL,
  cause        TEXT NOT NULL DEFAULT ''            -- why: a stranding's always, a sync turned off's sometimes
               CHECK ((kind = 'stranded' AND cause IN ('membership_ended', 'forgotten', 'disabled', 'deleted', 'reset'))
                   OR (kind = 'sync_paused' AND cause IN ('', 'bound_stopped'))
                   OR (kind IN ('removed', 'recovered', 'purged') AND cause = '')),
  lost_user    TEXT NOT NULL DEFAULT ''            -- the person a stranding lost; '' once forgotten
               CHECK (lost_user = '' OR kind = 'stranded'),
  purge_after  INTEGER NOT NULL DEFAULT 0          -- a removal's end, as it recorded it; 0 on every other kind
               CHECK ((kind = 'removed') = (purge_after <> 0))
);
INSERT INTO mailbox_events_new(seq, id, kind, workspace_id, account_id, label, hint, actor, created_at,
                               cause, lost_user, purge_after)
SELECT seq, id, kind, workspace_id, account_id, label, hint, actor, created_at, '', '',
       CASE kind WHEN 'removed' THEN created_at + 2592000 ELSE 0 END
  FROM mailbox_events ORDER BY seq;
-- The copy wrote the new table's high-water mark as the highest seq it
-- copied; the journal's own may be higher (rows swept since), or absent
-- (never written). Either way it is the one that stays.
DELETE FROM sqlite_sequence WHERE name = 'mailbox_events_new';
INSERT INTO sqlite_sequence(name, seq)
  SELECT 'mailbox_events_new', seq FROM sqlite_sequence WHERE name = 'mailbox_events';
DROP TABLE mailbox_events;
ALTER TABLE mailbox_events_new RENAME TO mailbox_events;

CREATE INDEX mailbox_events_account ON mailbox_events(account_id);
CREATE INDEX mailbox_events_created ON mailbox_events(created_at);
CREATE INDEX mailbox_events_workspace ON mailbox_events(workspace_id, seq);

CREATE TRIGGER mailbox_events_written_once BEFORE UPDATE ON mailbox_events
WHEN NEW.seq IS NOT OLD.seq OR NEW.id IS NOT OLD.id OR NEW.kind IS NOT OLD.kind
  OR NEW.workspace_id IS NOT OLD.workspace_id OR NEW.account_id IS NOT OLD.account_id
  OR NEW.created_at IS NOT OLD.created_at OR NEW.cause IS NOT OLD.cause OR NEW.purge_after IS NOT OLD.purge_after
  OR (NEW.label IS NOT OLD.label AND NEW.label <> '')
  OR (NEW.hint IS NOT OLD.hint AND NEW.hint <> '')
  OR (NEW.actor IS NOT OLD.actor AND NEW.actor <> '')
  OR (NEW.lost_user IS NOT OLD.lost_user AND NEW.lost_user <> '')
BEGIN
  SELECT RAISE(ABORT, 'a mailbox event is written once');
END;

CREATE TRIGGER notices_to_readers BEFORE INSERT ON notices
WHEN NOT EXISTS (SELECT 1 FROM mailbox_event_readers r WHERE r.event_seq = NEW.event_seq AND r.user_id = NEW.user_id)
  OR NOT EXISTS (SELECT 1 FROM mailbox_events e
                  WHERE e.seq = NEW.event_seq AND e.kind IN ('removed', 'sync_paused') AND e.actor IS NOT NEW.user_id)
BEGIN
  SELECT RAISE(ABORT, 'a notice goes to a reader of the mailbox, never to who acted');
END;
