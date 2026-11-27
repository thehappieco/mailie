-- Each mailbox's bytes under usage rule 1 (store.Usage, store.RuleVersion), kept by UTC day as its
-- rows change, and the index every delete of a message looks its copies up by (docs/workspaces.md,
-- "Migration 0030").
--
-- An ordinary migration: one index, two tables and the triggers that keep them; no row of a
-- schema-29 database changes, and both tables start empty. Migrations run SQL alone and the rule's
-- terms are built in Go (usageTables), so store.ReconcileUsageTotals fills them, from every
-- mailbox's rows, at the daemon's first start after this migration and in `mailserver migrate`,
-- before anything reads them; it checks them again daily and repairs a mailbox whose totals drifted.
--
--   - messages_dup_of: what each delete of a message runs ON DELETE SET NULL by, the copies whose
--     dup_of names it. Without it every delete scans every message, and deleting a mailbox's index
--     (a turn-off, a purge) takes a time quadratic in its messages. Index overhead counts nothing
--     under rule 1.
--   - usage_days: per mailbox and UTC day of internal_date (floor division: a date before 1970
--     falls on its own day), the bytes of its message rows still in their folder (vanished_at = 0)
--     and of their parts, counted as rule 1 counts every value.
--   - usage_fixed: per mailbox, the bytes of its folders rows and open drafts, which take no date.
--
-- Neither table has a foreign key: an upsert into a table bound to accounts, run by a trigger of a
-- row the account's delete cascades to, fails that delete, and every deletion for good with it.
-- usage_accounts_delete takes a mailbox's rows instead, after every trigger of the cascade. No
-- CHECK either, and bytes may be NULL, read as 0: a constraint a delta could break would refuse the
-- write that moved it, a sync batch, an action, a turn-off or a purge. A total that drifted is found
-- and repaired outside the write path (store.CheckUsageTotals, store.RepairUsageTotals).
--
-- Each trigger adds or takes away a row's bytes, its expression written out over exactly the
-- columns rule 1 counts, in their order (usageTables; TestTheUsageTriggersCountEveryColumnTheRuleCounts
-- holds this text to them). No trigger marks a workspace or a person: what a sync writes is
-- measured, never marked (0027).
--
-- Never INSERT OR REPLACE into messages, parts, folders or drafts: it fires the insert's trigger and
-- not the delete's, and the totals drift. Any rebuild of those four tables, or of accounts, must
-- create its usage_* triggers again after its copy: dropping a table takes its triggers with it,
-- and fires none (TestTheUsageTriggersAreOnEveryTableTheRuleCounts).

CREATE INDEX messages_dup_of ON messages(dup_of) WHERE dup_of IS NOT NULL;

CREATE TABLE usage_days (
  account_id TEXT    NOT NULL,             -- no foreign key: usage_accounts_delete takes its rows
  day        INTEGER NOT NULL,             -- unix days, UTC: floor(internal_date / 86400)
  bytes      INTEGER DEFAULT 0,            -- NULL reads as 0; no constraint a delta could break
  PRIMARY KEY (account_id, day)
) WITHOUT ROWID;

CREATE TABLE usage_fixed (
  account_id TEXT    PRIMARY KEY,          -- no foreign key: usage_accounts_delete takes its row
  bytes      INTEGER DEFAULT 0             -- NULL reads as 0; no constraint a delta could break
) WITHOUT ROWID;

-- A message row still in its folder counts on its UTC day, from its insert; its parts count
-- while it is live (usage_parts_insert).
CREATE TRIGGER usage_messages_insert AFTER INSERT ON messages
WHEN NEW.vanished_at = 0
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (NEW.account_id, ((NEW.internal_date - (((NEW.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.folder_id) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uid) END
      + CASE typeof(NEW.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.modseq) END
      + CASE typeof(NEW.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id) END
      + CASE typeof(NEW.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.group_key) END
      + CASE typeof(NEW.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.dup_of) END
      + CASE typeof(NEW.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.in_reply_to) END
      + CASE typeof(NEW.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.references_json) END
      + CASE typeof(NEW.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.subject) END
      + CASE typeof(NEW.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_json) END
      + CASE typeof(NEW.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_json) END
      + CASE typeof(NEW.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.cc_json) END
      + CASE typeof(NEW.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.bcc_json) END
      + CASE typeof(NEW.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.reply_to_json) END
      + CASE typeof(NEW.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_addr) END
      + CASE typeof(NEW.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_text) END
      + CASE typeof(NEW.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_text) END
      + CASE typeof(NEW.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.date) END
      + CASE typeof(NEW.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.internal_date) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flags_json) END
      + CASE typeof(NEW.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.seen) END
      + CASE typeof(NEW.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flagged) END
      + CASE typeof(NEW.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.answered) END
      + CASE typeof(NEW.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.draft) END
      + CASE typeof(NEW.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.deleted) END
      + CASE typeof(NEW.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.has_attachments) END
      + CASE typeof(NEW.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.snippet) END
      + CASE typeof(NEW.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_fetched_at) END
      + CASE typeof(NEW.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.vanished_at) END
      + CASE typeof(NEW.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.stale) END
      + CASE typeof(NEW.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.first_seen_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_text) END))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- Before a live row goes, while its parts are still there to sum: inside a cascade the parts'
-- own delete no longer finds their message, and an AFTER DELETE on messages finds no parts.
CREATE TRIGGER usage_messages_delete BEFORE DELETE ON messages
WHEN OLD.vanished_at = 0
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (OLD.account_id, ((OLD.internal_date - (((OLD.internal_date % 86400) + 86400) % 86400)) / 86400),
    -((CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.folder_id) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uid) END
      + CASE typeof(OLD.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.modseq) END
      + CASE typeof(OLD.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id) END
      + CASE typeof(OLD.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.group_key) END
      + CASE typeof(OLD.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.dup_of) END
      + CASE typeof(OLD.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.in_reply_to) END
      + CASE typeof(OLD.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.references_json) END
      + CASE typeof(OLD.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.subject) END
      + CASE typeof(OLD.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_json) END
      + CASE typeof(OLD.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_json) END
      + CASE typeof(OLD.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.cc_json) END
      + CASE typeof(OLD.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.bcc_json) END
      + CASE typeof(OLD.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.reply_to_json) END
      + CASE typeof(OLD.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_addr) END
      + CASE typeof(OLD.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_text) END
      + CASE typeof(OLD.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_text) END
      + CASE typeof(OLD.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.date) END
      + CASE typeof(OLD.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.internal_date) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flags_json) END
      + CASE typeof(OLD.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.seen) END
      + CASE typeof(OLD.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flagged) END
      + CASE typeof(OLD.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.answered) END
      + CASE typeof(OLD.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.draft) END
      + CASE typeof(OLD.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.deleted) END
      + CASE typeof(OLD.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.has_attachments) END
      + CASE typeof(OLD.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.snippet) END
      + CASE typeof(OLD.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_fetched_at) END
      + CASE typeof(OLD.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.vanished_at) END
      + CASE typeof(OLD.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.stale) END
      + CASE typeof(OLD.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.first_seen_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_text) END)
     + (SELECT coalesce(sum((CASE typeof(p.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.msg_id) END
           + CASE typeof(p.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.path) END
           + CASE typeof(p.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.mime_type) END
           + CASE typeof(p.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.charset) END
           + CASE typeof(p.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.encoding) END
           + CASE typeof(p.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.disposition) END
           + CASE typeof(p.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.filename) END
           + CASE typeof(p.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.content_id) END
           + CASE typeof(p.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.size) END
           + CASE typeof(p.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_body) END
           + CASE typeof(p.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_attachment) END
           + CASE typeof(p.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.sha256) END)), 0)
        FROM parts p WHERE p.msg_id = OLD.id)))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- Liveness counts on its flip, never on every write of vanished_at: a refresh writes 0 again on
-- a row that is live.
CREATE TRIGGER usage_messages_revive AFTER UPDATE OF vanished_at ON messages
WHEN OLD.vanished_at <> 0 AND NEW.vanished_at = 0
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (NEW.account_id, ((NEW.internal_date - (((NEW.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.folder_id) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uid) END
      + CASE typeof(NEW.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.modseq) END
      + CASE typeof(NEW.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id) END
      + CASE typeof(NEW.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.group_key) END
      + CASE typeof(NEW.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.dup_of) END
      + CASE typeof(NEW.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.in_reply_to) END
      + CASE typeof(NEW.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.references_json) END
      + CASE typeof(NEW.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.subject) END
      + CASE typeof(NEW.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_json) END
      + CASE typeof(NEW.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_json) END
      + CASE typeof(NEW.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.cc_json) END
      + CASE typeof(NEW.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.bcc_json) END
      + CASE typeof(NEW.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.reply_to_json) END
      + CASE typeof(NEW.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_addr) END
      + CASE typeof(NEW.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_text) END
      + CASE typeof(NEW.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_text) END
      + CASE typeof(NEW.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.date) END
      + CASE typeof(NEW.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.internal_date) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flags_json) END
      + CASE typeof(NEW.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.seen) END
      + CASE typeof(NEW.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flagged) END
      + CASE typeof(NEW.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.answered) END
      + CASE typeof(NEW.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.draft) END
      + CASE typeof(NEW.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.deleted) END
      + CASE typeof(NEW.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.has_attachments) END
      + CASE typeof(NEW.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.snippet) END
      + CASE typeof(NEW.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_fetched_at) END
      + CASE typeof(NEW.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.vanished_at) END
      + CASE typeof(NEW.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.stale) END
      + CASE typeof(NEW.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.first_seen_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_text) END)
    + (SELECT coalesce(sum((CASE typeof(p.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.msg_id) END
          + CASE typeof(p.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.path) END
          + CASE typeof(p.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.mime_type) END
          + CASE typeof(p.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.charset) END
          + CASE typeof(p.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.encoding) END
          + CASE typeof(p.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.disposition) END
          + CASE typeof(p.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.filename) END
          + CASE typeof(p.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.content_id) END
          + CASE typeof(p.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.size) END
          + CASE typeof(p.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_body) END
          + CASE typeof(p.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_attachment) END
          + CASE typeof(p.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.sha256) END)), 0)
       FROM parts p WHERE p.msg_id = NEW.id))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_messages_vanish AFTER UPDATE OF vanished_at ON messages
WHEN OLD.vanished_at = 0 AND NEW.vanished_at <> 0
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (OLD.account_id, ((OLD.internal_date - (((OLD.internal_date % 86400) + 86400) % 86400)) / 86400),
    -((CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.folder_id) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uid) END
      + CASE typeof(OLD.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.modseq) END
      + CASE typeof(OLD.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id) END
      + CASE typeof(OLD.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.group_key) END
      + CASE typeof(OLD.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.dup_of) END
      + CASE typeof(OLD.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.in_reply_to) END
      + CASE typeof(OLD.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.references_json) END
      + CASE typeof(OLD.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.subject) END
      + CASE typeof(OLD.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_json) END
      + CASE typeof(OLD.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_json) END
      + CASE typeof(OLD.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.cc_json) END
      + CASE typeof(OLD.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.bcc_json) END
      + CASE typeof(OLD.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.reply_to_json) END
      + CASE typeof(OLD.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_addr) END
      + CASE typeof(OLD.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_text) END
      + CASE typeof(OLD.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_text) END
      + CASE typeof(OLD.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.date) END
      + CASE typeof(OLD.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.internal_date) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flags_json) END
      + CASE typeof(OLD.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.seen) END
      + CASE typeof(OLD.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flagged) END
      + CASE typeof(OLD.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.answered) END
      + CASE typeof(OLD.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.draft) END
      + CASE typeof(OLD.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.deleted) END
      + CASE typeof(OLD.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.has_attachments) END
      + CASE typeof(OLD.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.snippet) END
      + CASE typeof(OLD.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_fetched_at) END
      + CASE typeof(OLD.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.vanished_at) END
      + CASE typeof(OLD.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.stale) END
      + CASE typeof(OLD.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.first_seen_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_text) END)
     + (SELECT coalesce(sum((CASE typeof(p.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.msg_id) END
           + CASE typeof(p.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.path) END
           + CASE typeof(p.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.mime_type) END
           + CASE typeof(p.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.charset) END
           + CASE typeof(p.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.encoding) END
           + CASE typeof(p.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.disposition) END
           + CASE typeof(p.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.filename) END
           + CASE typeof(p.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.content_id) END
           + CASE typeof(p.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.size) END
           + CASE typeof(p.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_body) END
           + CASE typeof(p.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_attachment) END
           + CASE typeof(p.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.sha256) END)), 0)
        FROM parts p WHERE p.msg_id = OLD.id)))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- A live row whose values change, on its day: the difference of its bytes. Every TEXT column
-- but account_id, and dup_of, which ON DELETE SET NULL changes from 8 bytes to none; an INTEGER
-- column NOT NULL counts 8 whatever it holds. A value written again as it was counts nothing.
CREATE TRIGGER usage_messages_resize AFTER UPDATE OF
  message_id, group_key, dup_of, in_reply_to, references_json, subject,
  from_json, to_json, cc_json, bcc_json, reply_to_json, from_addr,
  from_text, to_text, flags_json, snippet, body_text ON messages
WHEN OLD.vanished_at = 0 AND NEW.vanished_at = 0
 AND NEW.account_id IS OLD.account_id AND NEW.internal_date IS OLD.internal_date
 AND (NEW.message_id IS NOT OLD.message_id OR NEW.group_key IS NOT OLD.group_key OR NEW.dup_of IS NOT OLD.dup_of
      OR NEW.in_reply_to IS NOT OLD.in_reply_to OR NEW.references_json IS NOT OLD.references_json OR NEW.subject IS NOT OLD.subject
      OR NEW.from_json IS NOT OLD.from_json OR NEW.to_json IS NOT OLD.to_json OR NEW.cc_json IS NOT OLD.cc_json
      OR NEW.bcc_json IS NOT OLD.bcc_json OR NEW.reply_to_json IS NOT OLD.reply_to_json OR NEW.from_addr IS NOT OLD.from_addr
      OR NEW.from_text IS NOT OLD.from_text OR NEW.to_text IS NOT OLD.to_text OR NEW.flags_json IS NOT OLD.flags_json
      OR NEW.snippet IS NOT OLD.snippet OR NEW.body_text IS NOT OLD.body_text)
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (NEW.account_id, ((NEW.internal_date - (((NEW.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.folder_id) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uid) END
      + CASE typeof(NEW.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.modseq) END
      + CASE typeof(NEW.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id) END
      + CASE typeof(NEW.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.group_key) END
      + CASE typeof(NEW.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.dup_of) END
      + CASE typeof(NEW.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.in_reply_to) END
      + CASE typeof(NEW.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.references_json) END
      + CASE typeof(NEW.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.subject) END
      + CASE typeof(NEW.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_json) END
      + CASE typeof(NEW.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_json) END
      + CASE typeof(NEW.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.cc_json) END
      + CASE typeof(NEW.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.bcc_json) END
      + CASE typeof(NEW.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.reply_to_json) END
      + CASE typeof(NEW.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_addr) END
      + CASE typeof(NEW.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_text) END
      + CASE typeof(NEW.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_text) END
      + CASE typeof(NEW.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.date) END
      + CASE typeof(NEW.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.internal_date) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flags_json) END
      + CASE typeof(NEW.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.seen) END
      + CASE typeof(NEW.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flagged) END
      + CASE typeof(NEW.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.answered) END
      + CASE typeof(NEW.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.draft) END
      + CASE typeof(NEW.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.deleted) END
      + CASE typeof(NEW.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.has_attachments) END
      + CASE typeof(NEW.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.snippet) END
      + CASE typeof(NEW.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_fetched_at) END
      + CASE typeof(NEW.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.vanished_at) END
      + CASE typeof(NEW.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.stale) END
      + CASE typeof(NEW.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.first_seen_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_text) END)
    - (CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.folder_id) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uid) END
      + CASE typeof(OLD.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.modseq) END
      + CASE typeof(OLD.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id) END
      + CASE typeof(OLD.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.group_key) END
      + CASE typeof(OLD.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.dup_of) END
      + CASE typeof(OLD.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.in_reply_to) END
      + CASE typeof(OLD.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.references_json) END
      + CASE typeof(OLD.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.subject) END
      + CASE typeof(OLD.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_json) END
      + CASE typeof(OLD.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_json) END
      + CASE typeof(OLD.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.cc_json) END
      + CASE typeof(OLD.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.bcc_json) END
      + CASE typeof(OLD.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.reply_to_json) END
      + CASE typeof(OLD.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_addr) END
      + CASE typeof(OLD.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_text) END
      + CASE typeof(OLD.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_text) END
      + CASE typeof(OLD.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.date) END
      + CASE typeof(OLD.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.internal_date) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flags_json) END
      + CASE typeof(OLD.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.seen) END
      + CASE typeof(OLD.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flagged) END
      + CASE typeof(OLD.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.answered) END
      + CASE typeof(OLD.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.draft) END
      + CASE typeof(OLD.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.deleted) END
      + CASE typeof(OLD.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.has_attachments) END
      + CASE typeof(OLD.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.snippet) END
      + CASE typeof(OLD.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_fetched_at) END
      + CASE typeof(OLD.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.vanished_at) END
      + CASE typeof(OLD.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.stale) END
      + CASE typeof(OLD.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.first_seen_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_text) END))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- A live row moved to another mailbox or day, with its parts.
CREATE TRIGGER usage_messages_move AFTER UPDATE OF account_id, internal_date ON messages
WHEN OLD.vanished_at = 0 AND NEW.vanished_at = 0
 AND (NEW.account_id IS NOT OLD.account_id OR NEW.internal_date IS NOT OLD.internal_date)
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (OLD.account_id, ((OLD.internal_date - (((OLD.internal_date % 86400) + 86400) % 86400)) / 86400),
    -((CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.folder_id) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uid) END
      + CASE typeof(OLD.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.modseq) END
      + CASE typeof(OLD.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id) END
      + CASE typeof(OLD.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.group_key) END
      + CASE typeof(OLD.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.dup_of) END
      + CASE typeof(OLD.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.in_reply_to) END
      + CASE typeof(OLD.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.references_json) END
      + CASE typeof(OLD.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.subject) END
      + CASE typeof(OLD.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_json) END
      + CASE typeof(OLD.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_json) END
      + CASE typeof(OLD.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.cc_json) END
      + CASE typeof(OLD.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.bcc_json) END
      + CASE typeof(OLD.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.reply_to_json) END
      + CASE typeof(OLD.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_addr) END
      + CASE typeof(OLD.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.from_text) END
      + CASE typeof(OLD.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.to_text) END
      + CASE typeof(OLD.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.date) END
      + CASE typeof(OLD.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.internal_date) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flags_json) END
      + CASE typeof(OLD.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.seen) END
      + CASE typeof(OLD.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.flagged) END
      + CASE typeof(OLD.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.answered) END
      + CASE typeof(OLD.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.draft) END
      + CASE typeof(OLD.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.deleted) END
      + CASE typeof(OLD.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.has_attachments) END
      + CASE typeof(OLD.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.snippet) END
      + CASE typeof(OLD.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_fetched_at) END
      + CASE typeof(OLD.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.vanished_at) END
      + CASE typeof(OLD.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.stale) END
      + CASE typeof(OLD.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.first_seen_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.body_text) END)
     + (SELECT coalesce(sum((CASE typeof(p.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.msg_id) END
           + CASE typeof(p.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.path) END
           + CASE typeof(p.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.mime_type) END
           + CASE typeof(p.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.charset) END
           + CASE typeof(p.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.encoding) END
           + CASE typeof(p.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.disposition) END
           + CASE typeof(p.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.filename) END
           + CASE typeof(p.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.content_id) END
           + CASE typeof(p.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.size) END
           + CASE typeof(p.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_body) END
           + CASE typeof(p.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_attachment) END
           + CASE typeof(p.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.sha256) END)), 0)
        FROM parts p WHERE p.msg_id = OLD.id)))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
  INSERT INTO usage_days(account_id, day, bytes)
  VALUES (NEW.account_id, ((NEW.internal_date - (((NEW.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.folder_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.folder_id) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uid) END
      + CASE typeof(NEW.modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.modseq) END
      + CASE typeof(NEW.message_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id) END
      + CASE typeof(NEW.group_key) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.group_key) END
      + CASE typeof(NEW.dup_of) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.dup_of) END
      + CASE typeof(NEW.in_reply_to) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.in_reply_to) END
      + CASE typeof(NEW.references_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.references_json) END
      + CASE typeof(NEW.subject) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.subject) END
      + CASE typeof(NEW.from_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_json) END
      + CASE typeof(NEW.to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_json) END
      + CASE typeof(NEW.cc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.cc_json) END
      + CASE typeof(NEW.bcc_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.bcc_json) END
      + CASE typeof(NEW.reply_to_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.reply_to_json) END
      + CASE typeof(NEW.from_addr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_addr) END
      + CASE typeof(NEW.from_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.from_text) END
      + CASE typeof(NEW.to_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.to_text) END
      + CASE typeof(NEW.date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.date) END
      + CASE typeof(NEW.internal_date) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.internal_date) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.flags_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flags_json) END
      + CASE typeof(NEW.seen) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.seen) END
      + CASE typeof(NEW.flagged) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.flagged) END
      + CASE typeof(NEW.answered) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.answered) END
      + CASE typeof(NEW.draft) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.draft) END
      + CASE typeof(NEW.deleted) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.deleted) END
      + CASE typeof(NEW.has_attachments) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.has_attachments) END
      + CASE typeof(NEW.snippet) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.snippet) END
      + CASE typeof(NEW.body_fetched_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_fetched_at) END
      + CASE typeof(NEW.vanished_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.vanished_at) END
      + CASE typeof(NEW.stale) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.stale) END
      + CASE typeof(NEW.first_seen_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.first_seen_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.body_text) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.body_text) END)
    + (SELECT coalesce(sum((CASE typeof(p.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.msg_id) END
          + CASE typeof(p.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.path) END
          + CASE typeof(p.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.mime_type) END
          + CASE typeof(p.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.charset) END
          + CASE typeof(p.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.encoding) END
          + CASE typeof(p.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.disposition) END
          + CASE typeof(p.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.filename) END
          + CASE typeof(p.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.content_id) END
          + CASE typeof(p.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.size) END
          + CASE typeof(p.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_body) END
          + CASE typeof(p.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.is_attachment) END
          + CASE typeof(p.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(p.sha256) END)), 0)
       FROM parts p WHERE p.msg_id = NEW.id))
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- A part counts on its message's day while its message exists and is live: a cascade from a
-- message never counts it twice.
CREATE TRIGGER usage_parts_insert AFTER INSERT ON parts
WHEN EXISTS (SELECT 1 FROM messages m WHERE m.id = NEW.msg_id AND m.vanished_at = 0)
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  SELECT m.account_id, ((m.internal_date - (((m.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.msg_id) END
      + CASE typeof(NEW.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.path) END
      + CASE typeof(NEW.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.mime_type) END
      + CASE typeof(NEW.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.charset) END
      + CASE typeof(NEW.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.encoding) END
      + CASE typeof(NEW.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.disposition) END
      + CASE typeof(NEW.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.filename) END
      + CASE typeof(NEW.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.content_id) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.is_body) END
      + CASE typeof(NEW.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.is_attachment) END
      + CASE typeof(NEW.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sha256) END)
  FROM messages m WHERE m.id = NEW.msg_id AND m.vanished_at = 0
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_parts_delete AFTER DELETE ON parts
WHEN EXISTS (SELECT 1 FROM messages m WHERE m.id = OLD.msg_id AND m.vanished_at = 0)
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  SELECT m.account_id, ((m.internal_date - (((m.internal_date % 86400) + 86400) % 86400)) / 86400),
    -(CASE typeof(OLD.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.msg_id) END
      + CASE typeof(OLD.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.path) END
      + CASE typeof(OLD.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.mime_type) END
      + CASE typeof(OLD.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.charset) END
      + CASE typeof(OLD.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.encoding) END
      + CASE typeof(OLD.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.disposition) END
      + CASE typeof(OLD.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.filename) END
      + CASE typeof(OLD.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.content_id) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.is_body) END
      + CASE typeof(OLD.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.is_attachment) END
      + CASE typeof(OLD.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sha256) END)
  FROM messages m WHERE m.id = OLD.msg_id AND m.vanished_at = 0
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_parts_update AFTER UPDATE ON parts
BEGIN
  INSERT INTO usage_days(account_id, day, bytes)
  SELECT m.account_id, ((m.internal_date - (((m.internal_date % 86400) + 86400) % 86400)) / 86400),
    -(CASE typeof(OLD.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.msg_id) END
      + CASE typeof(OLD.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.path) END
      + CASE typeof(OLD.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.mime_type) END
      + CASE typeof(OLD.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.charset) END
      + CASE typeof(OLD.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.encoding) END
      + CASE typeof(OLD.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.disposition) END
      + CASE typeof(OLD.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.filename) END
      + CASE typeof(OLD.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.content_id) END
      + CASE typeof(OLD.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.size) END
      + CASE typeof(OLD.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.is_body) END
      + CASE typeof(OLD.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.is_attachment) END
      + CASE typeof(OLD.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sha256) END)
  FROM messages m WHERE m.id = OLD.msg_id AND m.vanished_at = 0
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
  INSERT INTO usage_days(account_id, day, bytes)
  SELECT m.account_id, ((m.internal_date - (((m.internal_date % 86400) + 86400) % 86400)) / 86400),
    (CASE typeof(NEW.msg_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.msg_id) END
      + CASE typeof(NEW.path) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.path) END
      + CASE typeof(NEW.mime_type) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.mime_type) END
      + CASE typeof(NEW.charset) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.charset) END
      + CASE typeof(NEW.encoding) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.encoding) END
      + CASE typeof(NEW.disposition) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.disposition) END
      + CASE typeof(NEW.filename) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.filename) END
      + CASE typeof(NEW.content_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.content_id) END
      + CASE typeof(NEW.size) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.size) END
      + CASE typeof(NEW.is_body) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.is_body) END
      + CASE typeof(NEW.is_attachment) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.is_attachment) END
      + CASE typeof(NEW.sha256) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sha256) END)
  FROM messages m WHERE m.id = NEW.msg_id AND m.vanished_at = 0
  ON CONFLICT (account_id, day) DO UPDATE SET bytes = coalesce(usage_days.bytes, 0) + excluded.bytes;
END;

-- Every folders row counts, whatever its state. Its TEXT columns but account_id, and never its
-- counters, which the 0001 triggers move at every message's insert, delete and change of seen.
CREATE TRIGGER usage_folders_insert AFTER INSERT ON folders
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (NEW.account_id,
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.name) END
      + CASE typeof(NEW.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.display_name) END
      + CASE typeof(NEW.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.delim) END
      + CASE typeof(NEW.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.attrs_json) END
      + CASE typeof(NEW.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role) END
      + CASE typeof(NEW.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role_source) END
      + CASE typeof(NEW.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.selectable) END
      + CASE typeof(NEW.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.synced) END
      + CASE typeof(NEW.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.passive) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidnext) END
      + CASE typeof(NEW.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.max_seen_uid) END
      + CASE typeof(NEW.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.highest_modseq) END
      + CASE typeof(NEW.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_floor) END
      + CASE typeof(NEW.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_cursor) END
      + CASE typeof(NEW.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_full_uid_scan_at) END
      + CASE typeof(NEW.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_flag_scan_at) END
      + CASE typeof(NEW.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.server_count) END
      + CASE typeof(NEW.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.local_count) END
      + CASE typeof(NEW.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.unseen_count) END
      + CASE typeof(NEW.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_state) END
      + CASE typeof(NEW.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_error) END
      + CASE typeof(NEW.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_synced_at) END
      + CASE typeof(NEW.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.missing_since) END
      + CASE typeof(NEW.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_total) END
      + CASE typeof(NEW.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_fetched) END
      + CASE typeof(NEW.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_since) END
      + CASE typeof(NEW.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.resync_from) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_folders_delete AFTER DELETE ON folders
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (OLD.account_id,
    -(CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.name) END
      + CASE typeof(OLD.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.display_name) END
      + CASE typeof(OLD.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.delim) END
      + CASE typeof(OLD.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.attrs_json) END
      + CASE typeof(OLD.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role) END
      + CASE typeof(OLD.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role_source) END
      + CASE typeof(OLD.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.selectable) END
      + CASE typeof(OLD.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.synced) END
      + CASE typeof(OLD.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.passive) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidnext) END
      + CASE typeof(OLD.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.max_seen_uid) END
      + CASE typeof(OLD.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.highest_modseq) END
      + CASE typeof(OLD.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_floor) END
      + CASE typeof(OLD.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_cursor) END
      + CASE typeof(OLD.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_full_uid_scan_at) END
      + CASE typeof(OLD.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_flag_scan_at) END
      + CASE typeof(OLD.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.server_count) END
      + CASE typeof(OLD.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.local_count) END
      + CASE typeof(OLD.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.unseen_count) END
      + CASE typeof(OLD.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_state) END
      + CASE typeof(OLD.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_error) END
      + CASE typeof(OLD.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_synced_at) END
      + CASE typeof(OLD.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.missing_since) END
      + CASE typeof(OLD.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_total) END
      + CASE typeof(OLD.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_fetched) END
      + CASE typeof(OLD.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_since) END
      + CASE typeof(OLD.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.resync_from) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_folders_resize AFTER UPDATE OF
  name, display_name, delim, attrs_json, role, role_source,
  sync_state, sync_error, resync_from ON folders
WHEN NEW.account_id IS OLD.account_id
 AND (NEW.name IS NOT OLD.name OR NEW.display_name IS NOT OLD.display_name OR NEW.delim IS NOT OLD.delim
      OR NEW.attrs_json IS NOT OLD.attrs_json OR NEW.role IS NOT OLD.role OR NEW.role_source IS NOT OLD.role_source
      OR NEW.sync_state IS NOT OLD.sync_state OR NEW.sync_error IS NOT OLD.sync_error OR NEW.resync_from IS NOT OLD.resync_from)
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (NEW.account_id,
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.name) END
      + CASE typeof(NEW.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.display_name) END
      + CASE typeof(NEW.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.delim) END
      + CASE typeof(NEW.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.attrs_json) END
      + CASE typeof(NEW.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role) END
      + CASE typeof(NEW.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role_source) END
      + CASE typeof(NEW.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.selectable) END
      + CASE typeof(NEW.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.synced) END
      + CASE typeof(NEW.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.passive) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidnext) END
      + CASE typeof(NEW.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.max_seen_uid) END
      + CASE typeof(NEW.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.highest_modseq) END
      + CASE typeof(NEW.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_floor) END
      + CASE typeof(NEW.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_cursor) END
      + CASE typeof(NEW.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_full_uid_scan_at) END
      + CASE typeof(NEW.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_flag_scan_at) END
      + CASE typeof(NEW.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.server_count) END
      + CASE typeof(NEW.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.local_count) END
      + CASE typeof(NEW.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.unseen_count) END
      + CASE typeof(NEW.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_state) END
      + CASE typeof(NEW.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_error) END
      + CASE typeof(NEW.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_synced_at) END
      + CASE typeof(NEW.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.missing_since) END
      + CASE typeof(NEW.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_total) END
      + CASE typeof(NEW.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_fetched) END
      + CASE typeof(NEW.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_since) END
      + CASE typeof(NEW.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.resync_from) END)
    - (CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.name) END
      + CASE typeof(OLD.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.display_name) END
      + CASE typeof(OLD.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.delim) END
      + CASE typeof(OLD.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.attrs_json) END
      + CASE typeof(OLD.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role) END
      + CASE typeof(OLD.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role_source) END
      + CASE typeof(OLD.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.selectable) END
      + CASE typeof(OLD.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.synced) END
      + CASE typeof(OLD.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.passive) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidnext) END
      + CASE typeof(OLD.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.max_seen_uid) END
      + CASE typeof(OLD.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.highest_modseq) END
      + CASE typeof(OLD.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_floor) END
      + CASE typeof(OLD.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_cursor) END
      + CASE typeof(OLD.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_full_uid_scan_at) END
      + CASE typeof(OLD.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_flag_scan_at) END
      + CASE typeof(OLD.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.server_count) END
      + CASE typeof(OLD.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.local_count) END
      + CASE typeof(OLD.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.unseen_count) END
      + CASE typeof(OLD.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_state) END
      + CASE typeof(OLD.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_error) END
      + CASE typeof(OLD.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_synced_at) END
      + CASE typeof(OLD.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.missing_since) END
      + CASE typeof(OLD.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_total) END
      + CASE typeof(OLD.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_fetched) END
      + CASE typeof(OLD.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_since) END
      + CASE typeof(OLD.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.resync_from) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_folders_move AFTER UPDATE OF account_id ON folders
WHEN NEW.account_id IS NOT OLD.account_id
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (OLD.account_id,
    -(CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.name) END
      + CASE typeof(OLD.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.display_name) END
      + CASE typeof(OLD.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.delim) END
      + CASE typeof(OLD.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.attrs_json) END
      + CASE typeof(OLD.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role) END
      + CASE typeof(OLD.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.role_source) END
      + CASE typeof(OLD.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.selectable) END
      + CASE typeof(OLD.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.synced) END
      + CASE typeof(OLD.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.passive) END
      + CASE typeof(OLD.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidvalidity) END
      + CASE typeof(OLD.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.uidnext) END
      + CASE typeof(OLD.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.max_seen_uid) END
      + CASE typeof(OLD.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.highest_modseq) END
      + CASE typeof(OLD.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_floor) END
      + CASE typeof(OLD.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.backfill_cursor) END
      + CASE typeof(OLD.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_full_uid_scan_at) END
      + CASE typeof(OLD.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_flag_scan_at) END
      + CASE typeof(OLD.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.server_count) END
      + CASE typeof(OLD.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.local_count) END
      + CASE typeof(OLD.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.unseen_count) END
      + CASE typeof(OLD.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_state) END
      + CASE typeof(OLD.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sync_error) END
      + CASE typeof(OLD.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.last_synced_at) END
      + CASE typeof(OLD.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.missing_since) END
      + CASE typeof(OLD.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_total) END
      + CASE typeof(OLD.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_fetched) END
      + CASE typeof(OLD.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.initial_since) END
      + CASE typeof(OLD.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.resync_from) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (NEW.account_id,
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.name) END
      + CASE typeof(NEW.display_name) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.display_name) END
      + CASE typeof(NEW.delim) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.delim) END
      + CASE typeof(NEW.attrs_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.attrs_json) END
      + CASE typeof(NEW.role) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role) END
      + CASE typeof(NEW.role_source) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.role_source) END
      + CASE typeof(NEW.selectable) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.selectable) END
      + CASE typeof(NEW.synced) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.synced) END
      + CASE typeof(NEW.passive) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.passive) END
      + CASE typeof(NEW.uidvalidity) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidvalidity) END
      + CASE typeof(NEW.uidnext) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.uidnext) END
      + CASE typeof(NEW.max_seen_uid) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.max_seen_uid) END
      + CASE typeof(NEW.highest_modseq) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.highest_modseq) END
      + CASE typeof(NEW.backfill_floor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_floor) END
      + CASE typeof(NEW.backfill_cursor) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.backfill_cursor) END
      + CASE typeof(NEW.last_full_uid_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_full_uid_scan_at) END
      + CASE typeof(NEW.last_flag_scan_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_flag_scan_at) END
      + CASE typeof(NEW.server_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.server_count) END
      + CASE typeof(NEW.local_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.local_count) END
      + CASE typeof(NEW.unseen_count) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.unseen_count) END
      + CASE typeof(NEW.sync_state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_state) END
      + CASE typeof(NEW.sync_error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sync_error) END
      + CASE typeof(NEW.last_synced_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.last_synced_at) END
      + CASE typeof(NEW.missing_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.missing_since) END
      + CASE typeof(NEW.initial_total) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_total) END
      + CASE typeof(NEW.initial_fetched) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_fetched) END
      + CASE typeof(NEW.initial_since) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.initial_since) END
      + CASE typeof(NEW.resync_from) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.resync_from) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;

-- An open draft counts; one sending, sent or failed does not. Nothing writes a draft yet.
CREATE TRIGGER usage_drafts_insert AFTER INSERT ON drafts
WHEN NEW.state = 'open'
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (NEW.account_id,
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.compose_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.compose_json) END
      + CASE typeof(NEW.compose_hash) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.compose_hash) END
      + CASE typeof(NEW.message_id_hdr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id_hdr) END
      + CASE typeof(NEW.state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.state) END
      + CASE typeof(NEW.error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.error) END
      + CASE typeof(NEW.created_by) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.created_by) END
      + CASE typeof(NEW.created_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.created_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.sent_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sent_at) END
      + CASE typeof(NEW.expires_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.expires_at) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_drafts_delete AFTER DELETE ON drafts
WHEN OLD.state = 'open'
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  VALUES (OLD.account_id,
    -(CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.compose_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.compose_json) END
      + CASE typeof(OLD.compose_hash) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.compose_hash) END
      + CASE typeof(OLD.message_id_hdr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id_hdr) END
      + CASE typeof(OLD.state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.state) END
      + CASE typeof(OLD.error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.error) END
      + CASE typeof(OLD.created_by) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.created_by) END
      + CASE typeof(OLD.created_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.created_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.sent_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sent_at) END
      + CASE typeof(OLD.expires_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.expires_at) END))
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;
CREATE TRIGGER usage_drafts_update AFTER UPDATE ON drafts
WHEN OLD.state = 'open' OR NEW.state = 'open'
BEGIN
  INSERT INTO usage_fixed(account_id, bytes)
  SELECT OLD.account_id,
    -(CASE typeof(OLD.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.id) END
      + CASE typeof(OLD.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.account_id) END
      + CASE typeof(OLD.compose_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.compose_json) END
      + CASE typeof(OLD.compose_hash) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.compose_hash) END
      + CASE typeof(OLD.message_id_hdr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.message_id_hdr) END
      + CASE typeof(OLD.state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.state) END
      + CASE typeof(OLD.error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.error) END
      + CASE typeof(OLD.created_by) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.created_by) END
      + CASE typeof(OLD.created_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.created_at) END
      + CASE typeof(OLD.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.updated_at) END
      + CASE typeof(OLD.sent_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.sent_at) END
      + CASE typeof(OLD.expires_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(OLD.expires_at) END)
  WHERE OLD.state = 'open'
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
  INSERT INTO usage_fixed(account_id, bytes)
  SELECT NEW.account_id,
    (CASE typeof(NEW.id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.id) END
      + CASE typeof(NEW.account_id) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.account_id) END
      + CASE typeof(NEW.compose_json) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.compose_json) END
      + CASE typeof(NEW.compose_hash) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.compose_hash) END
      + CASE typeof(NEW.message_id_hdr) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.message_id_hdr) END
      + CASE typeof(NEW.state) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.state) END
      + CASE typeof(NEW.error) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.error) END
      + CASE typeof(NEW.created_by) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.created_by) END
      + CASE typeof(NEW.created_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.created_at) END
      + CASE typeof(NEW.updated_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.updated_at) END
      + CASE typeof(NEW.sent_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.sent_at) END
      + CASE typeof(NEW.expires_at) WHEN 'integer' THEN 8 WHEN 'real' THEN 8 WHEN 'null' THEN 0 ELSE octet_length(NEW.expires_at) END)
  WHERE NEW.state = 'open'
  ON CONFLICT (account_id) DO UPDATE SET bytes = coalesce(usage_fixed.bytes, 0) + excluded.bytes;
END;

-- A mailbox deleted for good takes its totals with it. AFTER DELETE ON accounts fires after
-- every trigger of the rows its delete cascades to, so what they wrote goes too.
CREATE TRIGGER usage_accounts_delete AFTER DELETE ON accounts
BEGIN
  DELETE FROM usage_days WHERE account_id = OLD.id;
  DELETE FROM usage_fixed WHERE account_id = OLD.id;
END;
