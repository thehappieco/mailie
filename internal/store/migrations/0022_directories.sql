-- Following an external directory: the people, workspaces and memberships an
-- extension of the daemon mirrors from a directory it follows, under a
-- workspace source that is not the local one (docs/workspaces.md,
-- "Following an external directory").
--
-- An ordinary migration: three tables, three columns and their triggers are
-- added, and no row of a schema-21 database changes. On a server that follows
-- no directory, which is every self-hosted one, the tables stay empty and the
-- columns 0.
--
--   - directories: one row per directory followed, by its issuer: the
--     generation of its data the documents applied belong to, whether the
--     directory confirmed it, how many times it was reset here (a new
--     generation, or the directory forgetting where its feed stood), where
--     its feed was read up to (cursor), and up to when it says it published
--     every change (published_through_us, microseconds). One row, so a
--     generation and its cursor move together; they go back only with a
--     reset, which never goes back.
--   - directory_people: per directory and person, what it last said of them:
--     their subject there, the generation and version of the document
--     applied and the reset that version counts under, their status, and the
--     moments it disabled them, revoked their sessions and revoked their
--     credentials, in microseconds, kept as the highest ever seen. Keyed by
--     the person, not by the subject: the row outlives the person's deletion
--     (gone_at) until an extension releases it, and the same subject signing
--     in again meanwhile is a new person with a row of their own, so the
--     subject is unique only among the rows of people still here.
--   - workspaces.source_version: a workspace from another source, the
--     directory's version of its name and closure last applied; and
--     source_reset, the sum of every directory's resets when that version
--     was last zeroed or the workspace mirrored. A local workspace has
--     neither.
--   - workspace_members.held_at: a membership a directory ended while its
--     generation was not confirmed, and whose end would leave a mailbox with
--     nobody who reads it: disabled, reading nothing, its flags and sealed
--     grants kept until a document lists it again or the generation is
--     confirmed. 0008's word on a disabled membership ("its grants were
--     deleted") holds for one that is not held. Only a team's from another
--     source is held, and never when written.
--   - extension_state: an extension's own small state, by extension and key,
--     a value of a mebibyte at most, rolled back with the database it
--     describes.
--
-- What a directory revoked, and a version, only rise: a version falls only to
-- 0 and only with its reset rising to the one its directory recorded, and a
-- workspace's source version only with its source_reset rising to the sum of
-- every directory's (no directory's reset ever falls, and no directory is
-- ever deleted, so that sum only rises). A stray zeroing is refused unless a
-- reset was recorded first.

CREATE TABLE directories (
  issuer               TEXT PRIMARY KEY CHECK (length(issuer) BETWEEN 1 AND 300),
  generation           TEXT NOT NULL CHECK (length(generation) BETWEEN 1 AND 64),
  confirmed            INTEGER NOT NULL DEFAULT 0 CHECK (confirmed IN (0, 1)),
  reset                INTEGER NOT NULL DEFAULT 0 CHECK (reset >= 0),
  cursor               INTEGER NOT NULL DEFAULT 0 CHECK (cursor >= 0),
  published_through_us INTEGER NOT NULL DEFAULT 0 CHECK (published_through_us >= 0),
  changed_at           INTEGER NOT NULL
);
CREATE TRIGGER directories_reset BEFORE UPDATE ON directories
WHEN NEW.issuer IS NOT OLD.issuer OR NEW.reset < OLD.reset
  OR (NEW.reset = OLD.reset AND (NEW.generation IS NOT OLD.generation OR NEW.cursor < OLD.cursor
      OR (OLD.confirmed = 1 AND NEW.confirmed = 0) OR NEW.published_through_us < OLD.published_through_us))
BEGIN
  SELECT RAISE(ABORT, 'a directory''s generation, cursor and confirmation go back only with a reset');
END;
CREATE TRIGGER directories_kept BEFORE DELETE ON directories
BEGIN
  SELECT RAISE(ABORT, 'a directory followed is never forgotten');
END;

CREATE TABLE directory_people (
  issuer                    TEXT NOT NULL REFERENCES directories(issuer),
  user_id                   TEXT NOT NULL,          -- no foreign key: the row outlives the person (gone_at)
  subject                   TEXT NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  generation                TEXT NOT NULL,
  version                   INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
  reset                     INTEGER NOT NULL DEFAULT 0 CHECK (reset >= 0),  -- directories.reset its version counts under
  status                    TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'closed')),
  disabled_at_us            INTEGER NOT NULL DEFAULT 0 CHECK (disabled_at_us >= 0),
  sessions_revoked_at_us    INTEGER NOT NULL DEFAULT 0 CHECK (sessions_revoked_at_us >= 0),
  credentials_revoked_at_us INTEGER NOT NULL DEFAULT 0 CHECK (credentials_revoked_at_us >= 0),
  applied_at                INTEGER NOT NULL,
  gone_at                   INTEGER NOT NULL DEFAULT 0 CHECK (gone_at >= 0),
  PRIMARY KEY (issuer, user_id)
);
-- A subject is held by one row of a person still here; the rows of people
-- gone, whose subject signed in again, stay beside it.
CREATE UNIQUE INDEX directory_people_subject ON directory_people(issuer, subject) WHERE gone_at = 0;
CREATE INDEX directory_people_user ON directory_people(user_id);
CREATE TRIGGER directory_people_written_at_the_reset BEFORE INSERT ON directory_people
WHEN NEW.reset IS NOT (SELECT d.reset FROM directories d WHERE d.issuer = NEW.issuer)
BEGIN
  SELECT RAISE(ABORT, 'a directory''s person is written at its directory''s reset');
END;
CREATE TRIGGER directory_people_rises BEFORE UPDATE ON directory_people
WHEN NEW.user_id IS NOT OLD.user_id OR NEW.issuer IS NOT OLD.issuer OR NEW.subject IS NOT OLD.subject
  OR NEW.disabled_at_us < OLD.disabled_at_us OR NEW.sessions_revoked_at_us < OLD.sessions_revoked_at_us
  OR NEW.credentials_revoked_at_us < OLD.credentials_revoked_at_us
  OR NEW.reset < OLD.reset
  OR (NEW.reset IS NOT OLD.reset
      AND NEW.reset IS NOT (SELECT d.reset FROM directories d WHERE d.issuer = NEW.issuer))
  -- A version falls only to 0, and only with its reset rising to the
  -- directory's current one: only a reset recorded in directories, whose own
  -- trigger never lowers it, lets a version fall.
  OR (NEW.version < OLD.version AND (NEW.version <> 0 OR NEW.reset <= OLD.reset))
  OR (OLD.gone_at <> 0 AND NEW.gone_at IS NOT OLD.gone_at)
BEGIN
  SELECT RAISE(ABORT, 'what a directory revoked, and its version, only rise');
END;

ALTER TABLE workspaces ADD COLUMN source_version INTEGER NOT NULL DEFAULT 0
  CHECK (source_version >= 0 AND (source_version = 0 OR source <> 'local'));
-- The sum of every directory's reset when this workspace's source_version
-- was last zeroed, or the workspace mirrored.
ALTER TABLE workspaces ADD COLUMN source_reset INTEGER NOT NULL DEFAULT 0
  CHECK (source_reset >= 0 AND (source_reset = 0 OR source <> 'local'));
CREATE TRIGGER workspaces_source_reset_insert BEFORE INSERT ON workspaces
WHEN NEW.source <> 'local' AND NEW.source_reset IS NOT (SELECT coalesce(sum(d.reset), 0) FROM directories d)
BEGIN
  SELECT RAISE(ABORT, 'a workspace from another source is written at the directories'' reset');
END;
CREATE TRIGGER workspaces_source_version_rises BEFORE UPDATE OF source_version, source_reset ON workspaces
WHEN NEW.source_reset < OLD.source_reset
  OR (NEW.source_reset IS NOT OLD.source_reset
      AND NEW.source_reset IS NOT (SELECT coalesce(sum(d.reset), 0) FROM directories d))
  OR (NEW.source_version < OLD.source_version AND (NEW.source_version <> 0 OR NEW.source_reset <= OLD.source_reset))
BEGIN
  SELECT RAISE(ABORT, 'a workspace''s source version falls only to 0, with a reset');
END;

ALTER TABLE workspace_members ADD COLUMN held_at INTEGER NOT NULL DEFAULT 0
  CHECK (held_at >= 0 AND (held_at = 0 OR status = 'disabled'));
CREATE TRIGGER workspace_members_held_insert BEFORE INSERT ON workspace_members
WHEN NEW.held_at <> 0
BEGIN
  SELECT RAISE(ABORT, 'a membership is never held when written');
END;
CREATE TRIGGER workspace_members_held BEFORE UPDATE OF held_at ON workspace_members
WHEN NEW.held_at <> 0 AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = NEW.workspace_id
                                       AND w.kind = 'team' AND w.source <> 'local')
BEGIN
  SELECT RAISE(ABORT, 'only a membership of a team from another source is held');
END;

CREATE TABLE extension_state (
  extension  TEXT NOT NULL CHECK (length(extension) BETWEEN 1 AND 64),
  key        TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 256),
  value      BLOB NOT NULL CHECK (typeof(value) = 'blob' AND length(value) <= 1048576),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (extension, key)
);
