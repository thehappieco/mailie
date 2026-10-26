-- migration: rebuild
--
-- One mailbox's sync paused, its index kept (docs/workspaces.md, "Pausing a mailbox's sync"); a
-- recovery's hold made the recovery's pause; and the journal of what was done to mailboxes
-- (migrations 0020 and 0021) rebuilt to record a pause and its end.
--
--   - accounts.sync_paused_at and sync_paused_by: when a mailbox's sync was paused and by whom
--     ('usr_…', or for an operator mailbox the operator's 'key:<prefix>' or 'cli'; for a
--     recovery's pause, whoever recovered it); 0 and '' when it is not paused, and the actor ''
--     once that person is deleted, or for a pause made below of a recovery's hold. A paused
--     mailbox syncs nothing (store.SyncEligible, the engine's rule) and keeps what it indexed,
--     maybe nothing, which is still read, searched and followed by actions (store.IndexFollows).
--     Only a live mailbox that nothing holds, whose consent stands, is paused (a team's or an
--     operator mailbox's own, held by the triggers below; a personal one's person's, by the code):
--     a removal, the operator's turn-off and every end of a consent end the pause first. It is
--     recorded whole and once, as a removal is: its moment never moves until it ends.
--   - A recovery pauses the mailbox where its consent stands instead of holding it
--     (account.recoverRemoved), and accounts.sync_held_at now holds only a personal mailbox the
--     operator turned off (store.StopSync), live or removed, until its person turns it on. A hold
--     is the act that set its moment. The holds that exist are told apart by what the code
--     allows, and else by the journal: a hold on a mailbox that names no person, or over an index,
--     is a recovery's (the operator's turn-off of a team's or an operator mailbox holds nothing,
--     and of a personal one deletes its index with the hold, and nothing writes a held mailbox's
--     index); so is a personal hold over no index whose moment a 'recovered' entry of the mailbox
--     bears, with no turn-off by the operator journaled after it. A recovery's hold becomes what a
--     recovery does now: on a live mailbox whose consent stands, its pause, at the hold's moment
--     and naming nobody; on a live one whose consent is off, or on a removed one, nothing (a
--     removed one's next recovery decides). Every other hold stays: the operator's, or a
--     recovery's the journal no longer shows, over no index either way. And a removed personal
--     mailbox the operator turned off while it was removed, which was not held then, is held at
--     the moment the journal gives that turn-off.
--   - mailbox_events gains two kinds, with no cause: 'paused', a team mailbox's sync paused by a
--     person, which tells its other readers (notices_to_readers), and 'resumed', the end of a team
--     mailbox's pause by a resume, which tells nobody. A turn-off stays 'sync_paused', the index
--     deleted; a recovery's pause is its 'recovered' entry.
--   - accounts_change_update (migration 0023) marks a workspace from another source changed when
--     a mailbox's pause starts or ends: a snapshot shows it. It is dropped before the holds are
--     converted and written again after, so the conversion marks nothing.
--
-- A CHECK constraint is widened only by rebuilding the table, so this follows migration 0021
-- exactly (store/migrate.go, applyRebuild): notices_to_readers names the journal in its body, so
-- it is dropped first and written again (now admitting 'paused'); the new table takes every row
-- with its seq and every column named; the AUTOINCREMENT high-water mark is carried over as it
-- was, a journal never written having none; mailbox_event_readers and notices name the journal by
-- name and refer to the new table once it carries it; the journal's own triggers
-- (mailbox_events_written_once, 0021, and mailbox_events_change, 0023) go with the old table and
-- are written again, byte for byte. Every row of every table keeps every value but the holds
-- converted below; every mailbox not converted is not paused.

ALTER TABLE accounts ADD COLUMN sync_paused_at INTEGER NOT NULL DEFAULT 0;  -- when its sync was paused; 0 when not
ALTER TABLE accounts ADD COLUMN sync_paused_by TEXT NOT NULL DEFAULT '';    -- 'usr_…', 'key:<prefix>' or 'cli'; '' when
                                                                            -- not paused, and once that person is deleted

DROP TRIGGER accounts_change_update;

-- A recovery's hold becomes what a recovery does now; every other hold stays. One statement: its
-- SET expressions read the row as it was. accounts_change_update is gone meanwhile (above), so
-- nothing is marked and applyRebuild's row counts stay equal; no trigger of this migration exists
-- yet, and the rows it leaves satisfy each. The journal is read before it is rebuilt below.
UPDATE accounts
   SET sync_paused_at = CASE
         WHEN removed_at = 0
          AND CASE WHEN owner_user_id IS NULL THEN sync_enabled_at <> 0
                   ELSE EXISTS (SELECT 1 FROM users u WHERE u.id = accounts.owner_user_id AND u.sync_consent_at <> 0) END
         THEN sync_held_at ELSE 0 END,
       sync_held_at = 0
 WHERE sync_held_at <> 0
   AND (owner_user_id IS NULL
        OR EXISTS (SELECT 1 FROM folders f WHERE f.account_id = accounts.id)
        OR EXISTS (SELECT 1 FROM messages m WHERE m.account_id = accounts.id)
        OR EXISTS (SELECT 1 FROM mailbox_events r
                    WHERE r.account_id = accounts.id AND r.kind = 'recovered' AND r.created_at = accounts.sync_held_at
                      AND NOT EXISTS (SELECT 1 FROM mailbox_events o
                                       WHERE o.account_id = accounts.id AND o.kind = 'sync_paused'
                                         AND o.actor = 'operator' AND o.seq > r.seq)));

-- A removed personal mailbox the operator turned off while it was removed: held, as StopSync now
-- holds one, at the moment of that turn-off (a removal is at most 30 days old: its entries are
-- all kept). One turned off with nothing to delete was not journaled, and is not held.
UPDATE accounts
   SET sync_held_at = (SELECT max(o.created_at) FROM mailbox_events o
                        WHERE o.account_id = accounts.id AND o.kind = 'sync_paused' AND o.actor = 'operator'
                          AND o.seq > (SELECT max(r.seq) FROM mailbox_events r
                                        WHERE r.account_id = accounts.id AND r.kind = 'removed'))
 WHERE removed_at <> 0 AND owner_user_id IS NOT NULL AND sync_held_at = 0
   AND EXISTS (SELECT 1 FROM mailbox_events o
                WHERE o.account_id = accounts.id AND o.kind = 'sync_paused' AND o.actor = 'operator'
                  AND o.seq > (SELECT max(r.seq) FROM mailbox_events r
                                WHERE r.account_id = accounts.id AND r.kind = 'removed'));

CREATE TRIGGER accounts_linked_unpaused BEFORE INSERT ON accounts
WHEN NEW.sync_paused_at <> 0 OR NEW.sync_paused_by <> ''
BEGIN
  SELECT RAISE(ABORT, 'a mailbox is linked with its sync not paused');
END;

CREATE TRIGGER accounts_pause_whole BEFORE UPDATE OF sync_paused_at, sync_paused_by ON accounts
WHEN NEW.sync_paused_at = 0 AND NEW.sync_paused_by <> ''
BEGIN
  SELECT RAISE(ABORT, 'a pause is recorded whole: a mailbox that is not paused names nobody who paused it');
END;

CREATE TRIGGER accounts_pause_fixed BEFORE UPDATE OF sync_paused_at, sync_paused_by ON accounts
WHEN OLD.sync_paused_at <> 0 AND NEW.sync_paused_at <> 0
 AND (NEW.sync_paused_at IS NOT OLD.sync_paused_at
      OR (NEW.sync_paused_by IS NOT OLD.sync_paused_by AND NEW.sync_paused_by <> ''))
BEGIN
  SELECT RAISE(ABORT, 'a pause is recorded once: it never moves until it ends');
END;

CREATE TRIGGER accounts_pause_live BEFORE UPDATE OF sync_paused_at, removed_at, sync_held_at ON accounts
WHEN NEW.sync_paused_at <> 0 AND (NEW.removed_at <> 0 OR NEW.sync_held_at <> 0)
BEGIN
  SELECT RAISE(ABORT, 'only a live mailbox whose sync nothing holds is paused: a removal or a hold ends the pause first');
END;

-- A team's or an operator mailbox is paused only while its consent stands: every turn-off ends the
-- pause first (store.SetMailboxSync, StopSync, StopBoundTx), and a recovery pauses only one whose
-- consent stands. A personal mailbox's consent is its person's, in another table: the code holds
-- it (store.PauseSync and a recovery ask it, store.WithdrawSyncConsent ends the pauses first).
CREATE TRIGGER accounts_pause_consent BEFORE UPDATE OF sync_paused_at, sync_enabled_at ON accounts
WHEN NEW.sync_paused_at <> 0 AND NEW.owner_user_id IS NULL AND NEW.sync_enabled_at = 0
BEGIN
  SELECT RAISE(ABORT, 'a team''s or an operator mailbox is paused only while its consent stands');
END;

-- Only the operator's turn-off of a personal mailbox holds its sync (store.StopSync); a recovery
-- pauses, and the operator's turn-off of any other mailbox withdraws its consent.
CREATE TRIGGER accounts_held_personal BEFORE UPDATE OF sync_held_at ON accounts
WHEN NEW.sync_held_at <> 0 AND NEW.owner_user_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'only a personal mailbox is held, by the operator''s turn-off');
END;

CREATE TRIGGER accounts_change_update AFTER UPDATE OF state, display_name, removed_at, purge_after, sync_enabled_at,
  sync_held_at, sync_enabled_via, sync_paused_at ON accounts
WHEN (NEW.state IS NOT OLD.state OR NEW.display_name IS NOT OLD.display_name OR NEW.removed_at IS NOT OLD.removed_at
      OR NEW.purge_after IS NOT OLD.purge_after OR NEW.sync_enabled_at IS NOT OLD.sync_enabled_at
      OR NEW.sync_held_at IS NOT OLD.sync_held_at OR NEW.sync_enabled_via IS NOT OLD.sync_enabled_via
      OR NEW.sync_paused_at IS NOT OLD.sync_paused_at)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;

DROP TRIGGER notices_to_readers;

CREATE TABLE mailbox_events_new (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused: notices and readers name it
  id           TEXT NOT NULL UNIQUE                -- 'mbe_' + 16 hex: what the console names
               CHECK (id GLOB 'mbe_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
  kind         TEXT NOT NULL CHECK (kind IN ('removed', 'sync_paused', 'recovered', 'purged', 'stranded',
                                             'paused', 'resumed')),
  workspace_id TEXT NOT NULL,                      -- no foreign keys: a row outlives the mailbox and its team
  account_id   TEXT NOT NULL,
  label        TEXT NOT NULL DEFAULT ''            -- the mailbox's name; '' once deleted for good, or when it
               CHECK (instr(label, '@') = 0),      -- held an address, which the journal never does
  hint         TEXT NOT NULL DEFAULT ''            -- 'a…@example.com'; '' once deleted for good and no notice needs it
               CHECK (hint = '' OR (substr(hint, 2, 2) = '…@' AND length(hint) > 3 AND instr(substr(hint, 4), '@') = 0)),
  actor        TEXT NOT NULL DEFAULT '',           -- 'usr_…', 'key:<prefix>', 'cli', 'sweep', 'directory' or
                                                   -- 'operator'; '' once that person is forgotten
  created_at   INTEGER NOT NULL,
  cause        TEXT NOT NULL DEFAULT ''            -- why: a stranding's always, a sync turned off's sometimes
               CHECK ((kind = 'stranded' AND cause IN ('membership_ended', 'forgotten', 'disabled', 'deleted', 'reset'))
                   OR (kind = 'sync_paused' AND cause IN ('', 'bound_stopped'))
                   OR (kind IN ('removed', 'recovered', 'purged', 'paused', 'resumed') AND cause = '')),
  lost_user    TEXT NOT NULL DEFAULT ''            -- the person a stranding lost; '' once forgotten
               CHECK (lost_user = '' OR kind = 'stranded'),
  purge_after  INTEGER NOT NULL DEFAULT 0          -- a removal's end, as it recorded it; 0 on every other kind
               CHECK ((kind = 'removed') = (purge_after <> 0))
);
INSERT INTO mailbox_events_new(seq, id, kind, workspace_id, account_id, label, hint, actor, created_at,
                               cause, lost_user, purge_after)
SELECT seq, id, kind, workspace_id, account_id, label, hint, actor, created_at, cause, lost_user, purge_after
  FROM mailbox_events ORDER BY seq;
-- The copy wrote the new table's high-water mark as the highest seq it copied; the journal's own
-- may be higher (rows swept since), or absent (never written). Either way it is the one that stays.
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

CREATE TRIGGER mailbox_events_change AFTER INSERT ON mailbox_events
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;

CREATE TRIGGER notices_to_readers BEFORE INSERT ON notices
WHEN NOT EXISTS (SELECT 1 FROM mailbox_event_readers r WHERE r.event_seq = NEW.event_seq AND r.user_id = NEW.user_id)
  OR NOT EXISTS (SELECT 1 FROM mailbox_events e
                  WHERE e.seq = NEW.event_seq AND e.kind IN ('removed', 'sync_paused', 'paused') AND e.actor IS NOT NEW.user_id)
BEGIN
  SELECT RAISE(ABORT, 'a notice goes to a reader of the mailbox, never to who acted');
END;
