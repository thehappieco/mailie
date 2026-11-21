-- Requests an extension carries for a person, run once each, and when a mailbox access row last
-- gained a flag (docs/workspaces.md, "Acts an extension runs for a person").
--
-- An ordinary migration: one table and its index are added, and one column with its default; no
-- row of a schema-28 database changes, and no trigger is added or written again.
--
--   - extension_requests: a person's request that an extension carries here with no session
--     (service.ActFor), recorded by the write that decides it, in its own transaction, whatever
--     it did (a request whose effect held already included): the extension's name and the
--     request's id, which is its idempotency key, the person, the act (the core's closed list),
--     what it did (a JSON object of counts and flags, never a name, an address or what was asked)
--     and when, in microseconds. A second run of the same request finds its row and answers what
--     the first did, changing nothing. A request refused writes nothing here: its transaction is
--     rolled back. Kept until keep_until (unix seconds, at most 31 days after it ran, as its
--     extension says), then deleted by the hourly housekeeping; no foreign key to its person, so
--     that the record of what they asked outlives their deletion by those days, as the record of
--     changes it belongs to does.
--   - mailbox_access.raised_at: when a flag was last added to the row (unix seconds), by a change
--     that adds one (a grant given or raised, a mailbox linked); never by one that only takes flags
--     away, nor by a key supplied. A request that takes flags away is refused when the row gained
--     one at or after the second it was first asked: it never undoes a newer widening, and never
--     blocks another narrowing. 0 for every row before this migration, read as raised before any
--     request was asked.
--
-- workspace_changes and person_changes (0023, 0027) already mark every write that carries a new
-- raised_at or a widening's moment, which change flags or consents those triggers watch; neither
-- watches this table, which no report reads.

CREATE TABLE extension_requests (
  extension  TEXT    NOT NULL CHECK (length(extension) BETWEEN 1 AND 64),
  id         TEXT    NOT NULL CHECK (length(id) BETWEEN 1 AND 64),
  user_id    TEXT    NOT NULL,                   -- no foreign key: kept its days after a deletion
  act        TEXT    NOT NULL CHECK (length(act) BETWEEN 1 AND 64),
  outcome    TEXT    NOT NULL CHECK (json_valid(outcome) AND json_type(outcome) = 'object'),
  done_at    INTEGER NOT NULL CHECK (done_at > 0),  -- unix microseconds
  keep_until INTEGER NOT NULL CHECK (keep_until > 0), -- unix seconds
  PRIMARY KEY (extension, id)
) WITHOUT ROWID;
CREATE INDEX extension_requests_keep ON extension_requests(keep_until);

ALTER TABLE mailbox_access ADD COLUMN raised_at INTEGER NOT NULL DEFAULT 0 CHECK (raised_at >= 0);
