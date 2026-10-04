-- Whether a key was made for chosen accounts, recorded on its own row.
--
-- The restriction rows (api_key_accounts) say what a key reaches, and no rows
-- means every account. A mailbox removed takes its rows with it (ON DELETE
-- CASCADE; 0005's trigger revokes a key left with none), so a key made for one
-- mailbox, once that mailbox was gone, was listed exactly like a key for every
-- mailbox: the record said the opposite of what the key could reach. The flag
-- is set when the key is issued and never changed. It names no account: a
-- removed mailbox's id is not kept anywhere, here included.
--
-- ALTER-only, like 0002 to 0005. A key issued before is restricted if it has
-- restriction rows now.

ALTER TABLE api_keys ADD COLUMN restricted INTEGER NOT NULL DEFAULT 0; -- 1: made for the accounts in api_key_accounts

UPDATE api_keys SET restricted = 1
 WHERE EXISTS (SELECT 1 FROM api_key_accounts WHERE key_prefix = api_keys.prefix);
