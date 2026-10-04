-- Phase 5: Mailie sends mail from a person's mailbox only when they click
-- Send in the console, and only once they have allowed it.
--
-- Sending is a use of the mailbox neither the sync consent nor the actions
-- consent covers, so it has a consent of its own, per person, naming the
-- policy revision they agreed to. Withdrawing sets both back; nothing else
-- goes with it, because a send keeps nothing of the message.
--
-- A send keeps a short record (sends, from 0001): which mailbox, the
-- idempotency key, a hash of what was composed, the Message-ID, the state, a
-- classified reason, and when. Two columns join it here: how many recipients
-- the message had (never who), and the person who asked, so a person's daily
-- limit counts every session and key of theirs. No subject, no address, no
-- text. Rows go 30 days after their last change.
--
-- ALTER-only, like 0002 to 0006: rebuilding users would be a DROP TABLE, and
-- with foreign keys on that is a DELETE cascading into sessions and accounts.

ALTER TABLE users ADD COLUMN send_consent_at INTEGER NOT NULL DEFAULT 0;     -- 0: not allowed (or withdrawn)
ALTER TABLE users ADD COLUMN send_consent_version TEXT NOT NULL DEFAULT ''; -- the policy revision agreed to

ALTER TABLE sends ADD COLUMN recipients INTEGER NOT NULL DEFAULT 0; -- to + cc + bcc, distinct; never the addresses
ALTER TABLE sends ADD COLUMN user_id TEXT NOT NULL DEFAULT '';      -- the person who asked; '' for an instance key

CREATE INDEX sends_user ON sends(user_id, created_at);  -- a person's sends of the last day
CREATE INDEX sends_updated ON sends(updated_at);        -- the 30-day sweep
