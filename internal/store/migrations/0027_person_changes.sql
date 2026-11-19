-- What an extension reports of a followed workspace's settings, and of a person's own
-- (docs/workspaces.md, "What an extension reads, and the freshness of an act"): three facts the
-- schema did not keep, the marks of a person whose own settings changed, and the workspace marks
-- widened to every column those settings show.
--
-- An ordinary migration: three columns, one table and its triggers are added, one trigger is
-- written again watching more columns, and one backfill sets a column it adds. Nothing is
-- dropped but that trigger, and no other value of a schema-26 database changes.
--
--   - users.signed_out_everywhere_at: when the person last signed out of every session
--     (auth.Users.EndAllSessions, POST /v1/auth/logout with everywhere), in the transaction
--     that does it; 0 when they never did, or did before this migration, which is not known (as
--     0024 left the withdrawals before it). A password change, a reset, a recovery and an
--     external directory's revocation end sessions too, and are not recorded as one.
--   - accounts.connected_at: when the mailbox first stored something to sign in with (a
--     credentials row), kept through a re-authorization, a token's rotation and a rewrap; 0 while
--     it never connected (account.Account.Connected says the same, with no moment). Set by
--     credentials_connected below from now on. The backfill takes created_at only where it is
--     exact: a password mailbox, whose row is written only once a login with its password
--     worked, its password in the same write (and seconds later before that write was one). An
--     OAuth mailbox connected before this migration stays 0, not known: its row was written
--     waiting for consent, which may have come days later, or failed and been started again, and
--     credentials.updated_at moves with every refresh, so nothing kept its first moment.
--   - accounts.server_kind: what the last login recorded since this migration found the IMAP
--     server to be: 'gmail' when it is one of Google's IMAP hosts or advertised Gmail's
--     extension (provider.IsGmailServer), 'other' otherwise, '' while no login recorded it. A
--     password mailbox's login check records it when the mailbox is added, and every sync login
--     when it changes. Never backfilled: no capability was ever stored.
--   - person_changes: one row per person an external identity signs in (user_identities), with
--     the seq of the last change to their own settings as an extension reports them: their
--     consents (sync, actions, sending: moment, revision, withdrawal), their account key, their
--     last sign-out everywhere, and their notices — given, dismissed, withdrawn, swept with their
--     journal entry, and changed by what they show of their mailbox (its removal and its end, its
--     deletion for good, its label, hint and actor, and whether that actor is still a member of
--     the notice's workspace). A first sign-in, which links the identity, marks the new person.
--     As workspace_changes (0023): a seq is never reused (AUTOINCREMENT), every write deletes the
--     person's row and writes it again (never INSERT OR REPLACE, whose clause an outer INSERT OR
--     IGNORE would override), and a person no identity signs in is never marked, so on a server
--     that follows no directory the table stays empty. Unlike a deleted workspace's, a deleted
--     person's mark goes with them, as everything that names them does: with their identity,
--     which goes first (auth.DeleteTx) or by the cascade, in whichever order it runs; nothing of
--     them is reported any more. No foreign key, which would refuse a mark a cascade of their own
--     deletion writes before it is done. A trigger that marks the holders of a set of notices
--     selects each person once (DISTINCT): one person may hold several notices of one mailbox or
--     by one actor, and a second copy of their id would break the table's UNIQUE and abort the
--     write that fired it.
--   - accounts_change_update (0023, last written by 0025) watches, beside its eight columns,
--     every other column of a mailbox an extension reports: its state's reason, who linked it,
--     who agreed to its team's sync and to which text, who paused it, whether it saves a copy of
--     what is sent and on which host, when it first connected and what its server is. Its sync
--     bookkeeping (last_ok_at and the rest), its folders and its messages still mark nothing:
--     how much of it is indexed, and when it last synced, are measured, never watched.
--
-- The backfill runs before any trigger here exists, and accounts_change_update watches named
-- columns only, so the migration marks nothing.

ALTER TABLE users ADD COLUMN signed_out_everywhere_at INTEGER NOT NULL DEFAULT 0   -- 0: never, or not known
  CHECK (signed_out_everywhere_at >= 0);
ALTER TABLE accounts ADD COLUMN connected_at INTEGER NOT NULL DEFAULT 0            -- 0: never connected, or not known
  CHECK (connected_at >= 0);
ALTER TABLE accounts ADD COLUMN server_kind TEXT NOT NULL DEFAULT ''               -- '': no login recorded it yet
  CHECK (server_kind IN ('', 'gmail', 'other'));

UPDATE accounts SET connected_at = created_at
 WHERE auth_kind = 'password' AND EXISTS (SELECT 1 FROM credentials c WHERE c.account_id = accounts.id);

-- The first credential a mailbox stores is when it connected. An upsert that updates a credential
-- inserts nothing, and fires no INSERT trigger; the guard keeps the first moment however often it
-- is replaced.
CREATE TRIGGER credentials_connected AFTER INSERT ON credentials
BEGIN
  UPDATE accounts SET connected_at = NEW.updated_at WHERE id = NEW.account_id AND connected_at = 0;
END;

DROP TRIGGER accounts_change_update;
CREATE TRIGGER accounts_change_update AFTER UPDATE OF state, display_name, removed_at, purge_after, sync_enabled_at,
  sync_held_at, sync_enabled_via, sync_paused_at, state_reason, linked_by, sync_enabled_by, sync_consent_version,
  sync_paused_by, save_sent_copy, imap_host, connected_at, server_kind ON accounts
WHEN (NEW.state IS NOT OLD.state OR NEW.display_name IS NOT OLD.display_name OR NEW.removed_at IS NOT OLD.removed_at
      OR NEW.purge_after IS NOT OLD.purge_after OR NEW.sync_enabled_at IS NOT OLD.sync_enabled_at
      OR NEW.sync_held_at IS NOT OLD.sync_held_at OR NEW.sync_enabled_via IS NOT OLD.sync_enabled_via
      OR NEW.sync_paused_at IS NOT OLD.sync_paused_at OR NEW.state_reason IS NOT OLD.state_reason
      OR NEW.linked_by IS NOT OLD.linked_by OR NEW.sync_enabled_by IS NOT OLD.sync_enabled_by
      OR NEW.sync_consent_version IS NOT OLD.sync_consent_version OR NEW.sync_paused_by IS NOT OLD.sync_paused_by
      OR NEW.save_sent_copy IS NOT OLD.save_sent_copy OR NEW.imap_host IS NOT OLD.imap_host
      OR NEW.connected_at IS NOT OLD.connected_at OR NEW.server_kind IS NOT OLD.server_kind)
 AND EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id AND w.source <> 'local')
BEGIN
  DELETE FROM workspace_changes WHERE workspace_id = NEW.workspace_id;
  INSERT INTO workspace_changes(workspace_id) VALUES (NEW.workspace_id);
END;

CREATE TABLE person_changes (
  seq     INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused
  user_id TEXT NOT NULL UNIQUE                -- no foreign key: deleted with the person's identity, below
);

-- A person no identity signs in any more takes their mark with them: their identities go with
-- them (first, by auth.DeleteTx, or by the cascade), and a mark is written only while one is
-- there, so whichever order a deletion's cascades run in, none is left.
CREATE TRIGGER user_identities_person_delete AFTER DELETE ON user_identities
WHEN NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = OLD.user_id)
BEGIN
  DELETE FROM person_changes WHERE user_id = OLD.user_id;
END;

-- A person's own consents, account key and last sign-out everywhere.
CREATE TRIGGER users_person_change AFTER UPDATE OF sync_consent_at, sync_consent_version, actions_consent_at,
  actions_consent_version, actions_consent_withdrawn_at, send_consent_at, send_consent_version,
  send_consent_withdrawn_at, public_key, signed_out_everywhere_at ON users
WHEN (NEW.sync_consent_at IS NOT OLD.sync_consent_at OR NEW.sync_consent_version IS NOT OLD.sync_consent_version
      OR NEW.actions_consent_at IS NOT OLD.actions_consent_at
      OR NEW.actions_consent_version IS NOT OLD.actions_consent_version
      OR NEW.actions_consent_withdrawn_at IS NOT OLD.actions_consent_withdrawn_at
      OR NEW.send_consent_at IS NOT OLD.send_consent_at OR NEW.send_consent_version IS NOT OLD.send_consent_version
      OR NEW.send_consent_withdrawn_at IS NOT OLD.send_consent_withdrawn_at OR NEW.public_key IS NOT OLD.public_key
      OR NEW.signed_out_everywhere_at IS NOT OLD.signed_out_everywhere_at)
 AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = NEW.id)
BEGIN
  DELETE FROM person_changes WHERE user_id = NEW.id;
  INSERT INTO person_changes(user_id) VALUES (NEW.id);
END;

-- A first sign-in through an identity: the person exists, with nothing reported of them yet.
CREATE TRIGGER user_identities_person_change AFTER INSERT ON user_identities
BEGIN
  DELETE FROM person_changes WHERE user_id = NEW.user_id;
  INSERT INTO person_changes(user_id) VALUES (NEW.user_id);
END;

-- A notice given, and one gone: dismissed, withdrawn by a recovery or a pause's end, or with its
-- journal entry (the sweep, a person closed, a link abandoned), whose cascade fires this too.
CREATE TRIGGER notices_person_insert AFTER INSERT ON notices
WHEN EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = NEW.user_id)
BEGIN
  DELETE FROM person_changes WHERE user_id = NEW.user_id;
  INSERT INTO person_changes(user_id) VALUES (NEW.user_id);
END;
CREATE TRIGGER notices_person_delete AFTER DELETE ON notices
WHEN EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = OLD.user_id)
BEGIN
  DELETE FROM person_changes WHERE user_id = OLD.user_id;
  INSERT INTO person_changes(user_id) VALUES (OLD.user_id);
END;

-- What a notice shows of its journal entry: the mailbox's label and hint, forgotten at its
-- deletion for good, and the actor, forgotten with that person.
CREATE TRIGGER mailbox_events_person_change AFTER UPDATE OF label, hint, actor ON mailbox_events
WHEN NEW.label IS NOT OLD.label OR NEW.hint IS NOT OLD.hint OR NEW.actor IS NOT OLD.actor
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n WHERE n.event_seq = NEW.seq
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n WHERE n.event_seq = NEW.seq
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;

-- What a notice shows of its mailbox now: removed until when, and deleted for good.
CREATE TRIGGER accounts_person_change AFTER UPDATE OF removed_at, purge_after ON accounts
WHEN NEW.removed_at IS NOT OLD.removed_at OR NEW.purge_after IS NOT OLD.purge_after
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.account_id = NEW.id
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.account_id = NEW.id
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;
CREATE TRIGGER accounts_person_delete AFTER DELETE ON accounts
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.account_id = OLD.id
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.account_id = OLD.id
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;

-- Whether a notice's actor is still a member of its workspace, and whether that workspace is
-- still there and a team.
CREATE TRIGGER workspace_members_person_insert AFTER INSERT ON workspace_members
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq
      WHERE e.workspace_id = NEW.workspace_id AND e.actor = NEW.user_id
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq
     WHERE e.workspace_id = NEW.workspace_id AND e.actor = NEW.user_id
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;
CREATE TRIGGER workspace_members_person_delete AFTER DELETE ON workspace_members
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq
      WHERE e.workspace_id = OLD.workspace_id AND e.actor = OLD.user_id
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq
     WHERE e.workspace_id = OLD.workspace_id AND e.actor = OLD.user_id
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;
CREATE TRIGGER workspaces_person_delete AFTER DELETE ON workspaces
BEGIN
  DELETE FROM person_changes WHERE user_id IN
    (SELECT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.workspace_id = OLD.id
        AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id));
  INSERT INTO person_changes(user_id)
    SELECT DISTINCT n.user_id FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq WHERE e.workspace_id = OLD.id
       AND EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = n.user_id) ORDER BY n.user_id;
END;
