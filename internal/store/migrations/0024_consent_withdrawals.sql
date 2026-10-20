-- When a person last took their consent to actions or to sending back
-- (docs/console.md, "Consent to actions" and "Consent to sending").
--
-- An ordinary migration: two columns are added, nothing else changes, and no
-- row of a schema-23 database does: both are 0 for everyone. A withdrawal
-- leaves actions_consent_at and send_consent_at at 0 with an empty version,
-- as never having agreed does, so a console could not tell a person who
-- turned a permission off from one who was never asked. Each withdrawal now
-- writes the moment it happened, in the same UPDATE that clears the consent,
-- and each grant sets it back to 0 in the UPDATE that records the consent: it
-- is never beside a consent. An edition that asks for a permission only the
-- first time reads it to leave a person who said no alone. A withdrawal made
-- before this migration is not known, and reads as never asked.
--
-- ALTER-only, like 0002 to 0007: rebuilding users would be a DROP TABLE, and
-- with foreign keys on that is a DELETE cascading into sessions and accounts.

ALTER TABLE users ADD COLUMN actions_consent_withdrawn_at INTEGER NOT NULL DEFAULT 0; -- 0: never withdrawn, or given since
ALTER TABLE users ADD COLUMN send_consent_withdrawn_at INTEGER NOT NULL DEFAULT 0;    -- 0: never withdrawn, or given since
