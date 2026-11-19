-- Keys an issuer makes elsewhere, mirrored here (docs/workspaces.md, "Keys an
-- issuer makes").
--
-- An extension of the daemon may follow an issuer of API keys: keys made
-- somewhere else, for people of a directory it follows, which this server
-- checks and enforces as its own workspace keys. Each one is a row of
-- api_keys (with its issuer, never this server's own) holding mailboxes in
-- key_access, so every rule a workspace key obeys holds for it unchanged;
-- what such a key needs and this server's own keys do not lives in the
-- tables below. On a server that follows no issuer, which is every
-- self-hosted one, they stay empty and api_keys.issuer stays ''.
--
-- An ordinary migration, in one transaction with foreign keys on: one column
-- added to api_keys, seven tables, their indexes and triggers, and no row of
-- a schema-27 database changes.
--
--   - api_keys.issuer: '' for this server's own keys; for a key an issuer
--     made, that issuer (the directory's issuer it follows). Such a key
--     always expires (0 is "never" for an own key), is never the operator's,
--     has no name (the issuer's name for it is never kept), no origin, the
--     write scope and no last use (no time of use is kept), and its issuer,
--     hash, creator, creation and workspace never change; once revoked it is
--     never live again, and its expiry never moves later.
--   - issued_keys: per issued key, what its issuer said of it: its id there
--     and the version of its record applied (0 after a directory reset until
--     the reset's pass lists it again, listed), its creation at the issuer in
--     microseconds, whether its key terms were current when it was first
--     stored (judged once), when it ended and how (its own revocation or
--     expiry, or a pass that did not list it), and the last day it was
--     accepted.
--   - issued_key_stops: this server's own stops of an issued key, each
--     independent: the operator's hold, and its creator being stopped here.
--     A key with any is refused; each lift deletes only its own.
--   - issued_key_grants: per issued key, mailbox and use, who gave it there
--     (their subject at the issuer) and when, and what became of it here:
--     held, waiting for its giver's document, refused with its cause, or a
--     removed tombstone (its mailbox removed, or an old grant a reset's pass
--     did not keep).
--   - issued_key_uses: per issued key and day, the uses refused and how
--     many, until reported (at most 30 days, and gone with the key).
--   - issued_key_tell: what is owed to the issuer: a grant refused, or a key
--     held narrower than a reset's pass listed it (one row per key). No
--     foreign key: a record never stored is refused too. A row goes once the
--     issuer accepted it, with its key's row, and 30 days after it arrived.
--   - issued_key_ends: what stays of an issued key once its row goes: its
--     selector, how it ended (revoked, held here, or expired) and when, until
--     a year after its creation (forget_at). Written by the delete trigger on
--     every path, never changed, never written again for a selector it holds:
--     a selector it holds is never stored again.
--   - directory_keys: per issuer that issues keys, the directory reset its
--     feed counts under, where the feed was read up to, up to when it said it
--     published everything, the highest of that trusted before the last
--     reset, whether the reset's pass is open, and when a page was last
--     applied. Its cursor goes back only with a reset, and what it trusted
--     never falls.
--
-- What a workspace from another source reports of its mailboxes now includes
-- the issued keys on them: their workspace is marked (workspace_changes) when
-- what a key holds changes, when an issued key is revoked, its expiry moves
-- or it ends, and when one of its stops is put or lifted. A mailbox removed
-- turns its issued grants still held or waiting into removed tombstones: a
-- recovery gives nothing back.

ALTER TABLE api_keys ADD COLUMN issuer TEXT NOT NULL DEFAULT ''
  CHECK (length(issuer) <= 300 AND (issuer = '' OR expires_at > 0)); -- '' for this server's own keys
CREATE INDEX api_keys_issuer ON api_keys(issuer) WHERE issuer <> '';

-- An issued key's shape, as written.
CREATE TRIGGER api_keys_issued_insert BEFORE INSERT ON api_keys
WHEN NEW.issuer <> '' AND (NEW.workspace_id IS NULL OR NEW.workspace_id = 'wsp_operator')
BEGIN
  SELECT RAISE(ABORT, 'an issued key belongs to a workspace, never the operator''s');
END;
CREATE TRIGGER api_keys_issued_shape_insert BEFORE INSERT ON api_keys
WHEN NEW.issuer <> '' AND (NEW.name <> '' OR NEW.origin <> '' OR NEW.scope <> 'write' OR NEW.last_used_at <> 0
  OR NEW.restricted <> 0 OR NEW.created_by NOT GLOB 'usr_*' OR length(NEW.prefix) NOT BETWEEN 1 AND 64)
BEGIN
  SELECT RAISE(ABORT, 'an issued key has no name, origin or last use, the write scope, a person who created it and a selector');
END;
CREATE TRIGGER api_keys_issued_shape_update BEFORE UPDATE OF name, origin, scope, last_used_at, restricted ON api_keys
WHEN OLD.issuer <> '' AND (NEW.name <> '' OR NEW.origin <> '' OR NEW.scope <> 'write' OR NEW.last_used_at <> 0
  OR NEW.restricted <> 0)
BEGIN
  SELECT RAISE(ABORT, 'an issued key has no name, origin or last use, the write scope, a person who created it and a selector');
END;
CREATE TRIGGER api_keys_issuer_fixed BEFORE UPDATE OF issuer ON api_keys
WHEN NEW.issuer IS NOT OLD.issuer
BEGIN
  SELECT RAISE(ABORT, 'a key''s issuer never changes');
END;
CREATE TRIGGER api_keys_issued_fixed BEFORE UPDATE OF hash, created_by, created_at, workspace_id ON api_keys
WHEN OLD.issuer <> '' AND (NEW.hash IS NOT OLD.hash OR NEW.created_by IS NOT OLD.created_by
  OR NEW.created_at IS NOT OLD.created_at OR NEW.workspace_id IS NOT OLD.workspace_id)
BEGIN
  SELECT RAISE(ABORT, 'an issued key''s hash, creator, creation and workspace never change');
END;
CREATE TRIGGER api_keys_issued_revocation_kept BEFORE UPDATE OF revoked_at ON api_keys
WHEN OLD.issuer <> '' AND OLD.revoked_at <> 0 AND NEW.revoked_at = 0
BEGIN
  SELECT RAISE(ABORT, 'a revoked issued key is never live again');
END;
CREATE TRIGGER api_keys_issued_expiry BEFORE UPDATE OF expires_at ON api_keys
WHEN OLD.issuer <> '' AND (NEW.expires_at = 0 OR NEW.expires_at > OLD.expires_at)
BEGIN
  SELECT RAISE(ABORT, 'an issued key''s expiry never moves later, and an issued key always expires');
END;

CREATE TABLE issued_keys (
  prefix        TEXT NOT NULL PRIMARY KEY REFERENCES api_keys(prefix) ON DELETE CASCADE,
  issuer        TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 300),
  external_id   TEXT NOT NULL CHECK (length(external_id) BETWEEN 1 AND 64),  -- the key's id at its issuer
  version       INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),            -- 0 after a reset until its pass lists it
  listed        INTEGER NOT NULL DEFAULT 0 CHECK (listed IN (0, 1)),        -- seen in the current reset's pass
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),                 -- its creation at the issuer
  terms_ok      INTEGER NOT NULL CHECK (terms_ok IN (0, 1)),                -- its key terms current when first stored
  ended_at      INTEGER NOT NULL DEFAULT 0 CHECK (ended_at >= 0),           -- 0 while live
  end_kind      TEXT NOT NULL DEFAULT '' CHECK (end_kind IN ('', 'revoked', 'expired', 'unlisted')),
  used_day      TEXT NOT NULL DEFAULT ''                                    -- the last UTC day it was accepted
    CHECK (used_day = '' OR used_day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  UNIQUE (issuer, external_id),
  CHECK ((ended_at = 0) = (end_kind = ''))
);
CREATE TRIGGER issued_keys_of_their_key BEFORE INSERT ON issued_keys
WHEN NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.prefix = NEW.prefix AND k.issuer = NEW.issuer)
BEGIN
  SELECT RAISE(ABORT, 'an issued key''s record names a key of its issuer');
END;
CREATE TRIGGER issued_keys_fixed BEFORE UPDATE OF prefix, issuer, external_id, created_at_us, terms_ok ON issued_keys
WHEN NEW.prefix IS NOT OLD.prefix OR NEW.issuer IS NOT OLD.issuer OR NEW.external_id IS NOT OLD.external_id
  OR NEW.created_at_us IS NOT OLD.created_at_us OR NEW.terms_ok IS NOT OLD.terms_ok
BEGIN
  SELECT RAISE(ABORT, 'an issued key''s id, creation and judged terms never change');
END;
-- A version only rises, but for a reset's, which sets it to 0.
CREATE TRIGGER issued_keys_version BEFORE UPDATE OF version ON issued_keys
WHEN NEW.version < OLD.version AND NEW.version <> 0
BEGIN
  SELECT RAISE(ABORT, 'an issued key''s version falls only to 0, at a reset');
END;
CREATE TRIGGER issued_keys_end_kept BEFORE UPDATE OF ended_at, end_kind ON issued_keys
WHEN OLD.ended_at <> 0 AND NEW.ended_at = 0
BEGIN
  SELECT RAISE(ABORT, 'an issued key that ended never lives again');
END;

-- This server's own stops of an issued key, each lifted on its own.
CREATE TABLE issued_key_stops (
  prefix TEXT NOT NULL REFERENCES issued_keys(prefix) ON DELETE CASCADE,
  cause  TEXT NOT NULL CHECK (cause IN ('operator', 'creator')),
  at     INTEGER NOT NULL CHECK (at > 0),
  PRIMARY KEY (prefix, cause)
);

-- Per issued key, mailbox and use: who gave it at the issuer, and what became
-- of it here. Gone with the mailbox.
CREATE TABLE issued_key_grants (
  prefix      TEXT NOT NULL REFERENCES issued_keys(prefix) ON DELETE CASCADE,
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  use         TEXT NOT NULL CHECK (use IN ('read', 'act', 'send')),
  given_by    TEXT NOT NULL CHECK (length(given_by) BETWEEN 1 AND 255),  -- the giver's subject at the issuer
  given_at_us INTEGER NOT NULL CHECK (given_at_us >= 0),
  state       TEXT NOT NULL CHECK (state IN ('held', 'waiting', 'refused', 'removed')),
  cause       TEXT NOT NULL DEFAULT '',
  version     INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),            -- the record that last listed it
  PRIMARY KEY (prefix, account_id, use),
  CHECK (CASE state
           WHEN 'refused' THEN cause IN ('giver_not_reader', 'mailbox_unknown', 'not_offered', 'team_withheld',
                                         'terms_not_current', 'invalid')
           WHEN 'removed' THEN cause IN ('mailbox_removed', 'reset')
           ELSE cause = '' END)
);
CREATE INDEX issued_key_grants_account ON issued_key_grants(account_id);
CREATE INDEX issued_key_grants_waiting ON issued_key_grants(given_by) WHERE state = 'waiting';

-- A mailbox removed loses what issued keys held or waited for on it, and a
-- recovery gives none of it back.
CREATE TRIGGER accounts_removed_issued_grants AFTER UPDATE OF removed_at ON accounts
WHEN OLD.removed_at = 0 AND NEW.removed_at <> 0
BEGIN
  UPDATE issued_key_grants SET state = 'removed', cause = 'mailbox_removed'
   WHERE account_id = NEW.id AND state IN ('held', 'waiting');
END;

-- The uses an issued key had refused, per day, until reported.
CREATE TABLE issued_key_uses (
  prefix     TEXT NOT NULL REFERENCES issued_keys(prefix) ON DELETE CASCADE,
  day        TEXT NOT NULL CHECK (day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  account_id TEXT NOT NULL DEFAULT '',  -- '' for a refusal of the key itself
  use        TEXT NOT NULL DEFAULT '' CHECK (use IN ('', 'read', 'act', 'send')),
  refused    TEXT NOT NULL CHECK (refused IN ('no_flag', 'creator_consent', 'not_offered', 'workspace_closed',
                                              'revoked', 'expired', 'held')),
  count      INTEGER NOT NULL CHECK (count >= 1),
  reported   INTEGER NOT NULL DEFAULT 0 CHECK (reported IN (0, 1)),
  PRIMARY KEY (prefix, day, account_id, use, refused),
  CHECK ((account_id = '') = (use = '') AND (account_id = '') = (refused IN ('revoked', 'expired', 'held')))
);

-- What is owed to the issuer, until it accepted it.
CREATE TABLE issued_key_tell (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused: an acceptance deletes what it names
  issuer      TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 300),
  kind        TEXT NOT NULL CHECK (kind IN ('refusal', 'held')),
  external_id TEXT NOT NULL CHECK (length(external_id) BETWEEN 1 AND 64),
  prefix      TEXT NOT NULL DEFAULT '',           -- the stored key's; '' for a record never stored
  version     INTEGER NOT NULL DEFAULT 0,
  account_id  TEXT NOT NULL DEFAULT '',
  use         TEXT NOT NULL DEFAULT '',
  cause       TEXT NOT NULL DEFAULT '',
  revoked     INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
  expires_at  INTEGER NOT NULL DEFAULT 0 CHECK (expires_at >= 0),
  removed     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(removed) AND json_type(removed) = 'array'),
  received_at INTEGER NOT NULL CHECK (received_at > 0),
  CHECK (CASE kind
           WHEN 'refusal' THEN version >= 1 AND account_id <> '' AND use IN ('read', 'act', 'send')
                               AND cause IN ('giver_not_reader', 'mailbox_unknown', 'not_offered', 'team_withheld',
                                             'terms_not_current', 'invalid')
                               AND revoked = 0 AND expires_at = 0 AND removed = '[]'
           ELSE version = 0 AND account_id = '' AND use = '' AND cause = '' END)
);
CREATE UNIQUE INDEX issued_key_tell_held ON issued_key_tell(issuer, external_id) WHERE kind = 'held';
CREATE INDEX issued_key_tell_issuer ON issued_key_tell(issuer, id);
CREATE INDEX issued_key_tell_prefix ON issued_key_tell(prefix) WHERE prefix <> '';

-- What stays of an issued key once its row goes, until a year after its
-- creation.
CREATE TABLE issued_key_ends (
  prefix    TEXT NOT NULL PRIMARY KEY CHECK (length(prefix) BETWEEN 1 AND 64),
  how       TEXT NOT NULL CHECK (how IN ('revoked', 'held', 'expired')),
  ended_at  INTEGER NOT NULL CHECK (ended_at > 0),
  forget_at INTEGER NOT NULL CHECK (forget_at > 0)   -- its creation + 365 days
);
CREATE TRIGGER issued_key_ends_fixed BEFORE UPDATE ON issued_key_ends
BEGIN
  SELECT RAISE(ABORT, 'what is left of an issued key never changes');
END;
-- A selector this server forgot an issued key under is never stored again.
CREATE TRIGGER api_keys_issued_forgotten BEFORE INSERT ON api_keys
WHEN NEW.issuer <> '' AND EXISTS (SELECT 1 FROM issued_key_ends e WHERE e.prefix = NEW.prefix)
BEGIN
  SELECT RAISE(ABORT, 'an issued key this server forgot is never stored again');
END;

-- An issued key's row leaves its end on every path that deletes it, the
-- cascade from its workspace included, while its stops are still there to
-- read, and takes what is owed of it with it. Never INSERT OR REPLACE: a
-- trigger's statement takes the conflict clause of the statement that fired
-- it.
CREATE TRIGGER issued_keys_leave_their_end BEFORE DELETE ON issued_keys
BEGIN
  INSERT INTO issued_key_ends(prefix, how, ended_at, forget_at)
  SELECT OLD.prefix,
         CASE WHEN OLD.end_kind <> 'expired' THEN 'revoked'
              WHEN EXISTS (SELECT 1 FROM issued_key_stops s WHERE s.prefix = OLD.prefix) THEN 'held'
              ELSE 'expired' END,
         CASE OLD.ended_at WHEN 0 THEN unixepoch() ELSE OLD.ended_at END,
         OLD.created_at_us / 1000000 + 31536000
   WHERE NOT EXISTS (SELECT 1 FROM issued_key_ends e WHERE e.prefix = OLD.prefix);
  DELETE FROM issued_key_tell WHERE prefix = OLD.prefix;
END;

-- Per issuer that issues keys, its feed's place, counted under its
-- directory's reset.
CREATE TABLE directory_keys (
  issuer               TEXT NOT NULL PRIMARY KEY REFERENCES directories(issuer),
  reset                INTEGER NOT NULL CHECK (reset >= 0),
  cursor               INTEGER NOT NULL DEFAULT 0 CHECK (cursor >= 0),
  published_through_us INTEGER NOT NULL DEFAULT 0 CHECK (published_through_us >= 0),
  trusted_through_us   INTEGER NOT NULL DEFAULT 0 CHECK (trusted_through_us >= 0),
  pass_open            INTEGER NOT NULL DEFAULT 0 CHECK (pass_open IN (0, 1)),
  applied_at_us        INTEGER NOT NULL DEFAULT 0 CHECK (applied_at_us >= 0)  -- when a page was last applied
);
CREATE TRIGGER directory_keys_written_at_the_reset BEFORE INSERT ON directory_keys
WHEN NEW.reset IS NOT (SELECT d.reset FROM directories d WHERE d.issuer = NEW.issuer)
BEGIN
  SELECT RAISE(ABORT, 'an issuer''s keys are written at its directory''s reset');
END;
CREATE TRIGGER directory_keys_rises BEFORE UPDATE ON directory_keys
WHEN NEW.issuer IS NOT OLD.issuer OR NEW.reset < OLD.reset
  OR (NEW.reset IS NOT OLD.reset AND NEW.reset IS NOT (SELECT d.reset FROM directories d WHERE d.issuer = NEW.issuer))
  OR (NEW.reset = OLD.reset AND (NEW.cursor < OLD.cursor OR NEW.published_through_us < OLD.published_through_us))
  OR NEW.trusted_through_us < OLD.trusted_through_us
BEGIN
  SELECT RAISE(ABORT, 'an issuer''s cursor goes back only with a reset, and what it trusted never falls');
END;
CREATE TRIGGER directory_keys_kept BEFORE DELETE ON directory_keys
BEGIN
  SELECT RAISE(ABORT, 'an issuer followed is never forgotten');
END;

-- The issued keys on a workspace's mailboxes are part of what is reported of
-- it: 0023's marks, for what a key holds, an issued key's revocation, expiry
-- and end, and its stops.
CREATE TRIGGER key_access_change_insert AFTER INSERT ON key_access
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER key_access_change_update AFTER UPDATE OF read, act, send ON key_access
WHEN (NEW.read IS NOT OLD.read OR NEW.act IS NOT OLD.act OR NEW.send IS NOT OLD.send)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER key_access_change_delete AFTER DELETE ON key_access
WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = OLD.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = OLD.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (OLD.workspace_id);
END;
CREATE TRIGGER api_keys_issued_change AFTER UPDATE OF revoked_at, expires_at ON api_keys
WHEN NEW.issuer <> '' AND (NEW.revoked_at IS NOT OLD.revoked_at OR NEW.expires_at IS NOT OLD.expires_at)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;
CREATE TRIGGER issued_keys_change AFTER UPDATE OF ended_at ON issued_keys
WHEN NEW.ended_at IS NOT OLD.ended_at
 AND EXISTS (SELECT 1 FROM api_keys k JOIN workspaces w ON w.id = k.workspace_id
              WHERE k.prefix = NEW.prefix AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = (SELECT k.workspace_id FROM api_keys k WHERE k.prefix = NEW.prefix);
  INSERT INTO workspace_changes(workspace_id) SELECT k.workspace_id FROM api_keys k WHERE k.prefix = NEW.prefix;
END;
CREATE TRIGGER issued_key_stops_change_insert AFTER INSERT ON issued_key_stops
WHEN EXISTS (SELECT 1 FROM api_keys k JOIN workspaces w ON w.id = k.workspace_id
              WHERE k.prefix = NEW.prefix AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = (SELECT k.workspace_id FROM api_keys k WHERE k.prefix = NEW.prefix);
  INSERT INTO workspace_changes(workspace_id) SELECT k.workspace_id FROM api_keys k WHERE k.prefix = NEW.prefix;
END;
CREATE TRIGGER issued_key_stops_change_delete AFTER DELETE ON issued_key_stops
WHEN EXISTS (SELECT 1 FROM api_keys k JOIN workspaces w ON w.id = k.workspace_id
              WHERE k.prefix = OLD.prefix AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = (SELECT k.workspace_id FROM api_keys k WHERE k.prefix = OLD.prefix);
  INSERT INTO workspace_changes(workspace_id) SELECT k.workspace_id FROM api_keys k WHERE k.prefix = OLD.prefix;
END;
