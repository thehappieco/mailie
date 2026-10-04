-- Phase 3b: Mailie changes a person's messages on their mail server — marks
-- them read or unread, stars them, archives, moves them, puts them in the
-- trash — only when the person asks, and only once they have allowed it.
--
-- Changing a mailbox is a use of it the sync consent does not cover, so it
-- has a consent of its own, per person, naming the policy revision they
-- agreed to. Withdrawing sets both back; nothing else goes with it, because
-- acting stores nothing beyond what the index already holds.
--
-- ALTER-only, like 0002 and 0003: rebuilding users would be a DROP TABLE, and
-- with foreign keys on that is a DELETE cascading into sessions and accounts.

ALTER TABLE users ADD COLUMN actions_consent_at INTEGER NOT NULL DEFAULT 0;     -- 0: not allowed (or withdrawn)
ALTER TABLE users ADD COLUMN actions_consent_version TEXT NOT NULL DEFAULT ''; -- the policy revision agreed to
