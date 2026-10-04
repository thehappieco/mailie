-- The web console: people who sign in, their browser sessions, the invites
-- that let them sign up, and which of them owns each mailbox.
--
-- ALTER-only for accounts and api_keys. Rebuilding either would be a DROP
-- TABLE, and with foreign keys on — which every connection here has — that is
-- an implicit DELETE cascading into credentials, folders, messages and the key
-- restrictions. oauth_pending is the exception: an ephemeral child table whose
-- rows are ten-minute consent attempts, so it is dropped and recreated to widen
-- its CHECK and bind each attempt to whoever started it.

CREATE TABLE users (
  id                  TEXT PRIMARY KEY,                -- 'usr_' + 16 hex, never reused
  email               TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name                TEXT NOT NULL DEFAULT '',
  password_hash       TEXT NOT NULL,                   -- Argon2id PHC; the parameters are read back from it
  role                TEXT NOT NULL CHECK (role IN ('owner','member')),
                                                       -- owner: sees instance-owned accounts and may invite
  status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  password_changed_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,                       -- 'ses_' + 16 hex; safe to log, unlike the token
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- sha256 of the token. Fast on purpose: the token is 32 random bytes, so
  -- there is no dictionary to slow an attacker down through, unlike a password.
  token_hash   BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
  user_agent   TEXT NOT NULL DEFAULT '',               -- at most 200 characters, for "where am I signed in"
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,                       -- bumped at most once a minute: a read must not be a write
  expires_at   INTEGER NOT NULL,                       -- created_at + 14 days, absolute; nothing extends it
  revoked_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE invites (
  code_hash  BLOB PRIMARY KEY CHECK (length(code_hash) = 32),  -- sha256 of the code; the code is 32 random bytes
  email      TEXT NOT NULL COLLATE NOCASE,             -- the invite is for this address and no other
  role       TEXT NOT NULL CHECK (role IN ('owner','member')),
  created_by TEXT NOT NULL DEFAULT '',                 -- 'usr_…', 'key:<prefix>' or 'cli'
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,                         -- created_at + 7 days
  used_at    INTEGER NOT NULL DEFAULT 0,
  used_by    TEXT NOT NULL DEFAULT ''                  -- the 'usr_…' it became
);

-- NULL is an account created by an instance key (the CLI), which the owner role
-- sees. No ON DELETE action: users are disabled, not deleted, and a deletion
-- that quietly turned someone's mailboxes into instance-owned ones would hand
-- them to every owner.
ALTER TABLE accounts ADD COLUMN owner_user_id TEXT REFERENCES users(id);
-- Which OAuth registration issued the stored grant. A refresh token belongs to
-- the client that issued it, so the CLI's installed client and the console's
-- web client cannot refresh each other's tokens.
ALTER TABLE accounts ADD COLUMN oauth_client TEXT NOT NULL DEFAULT 'installed'
  CHECK (oauth_client IN ('installed','web'));
CREATE INDEX accounts_owner ON accounts(owner_user_id);

-- NULL is an instance key: today's behaviour, every account. A key with a user
-- acts as that person, within its own scope and account restriction.
ALTER TABLE api_keys ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE;

DROP TABLE oauth_pending;
CREATE TABLE oauth_pending (
  state         TEXT PRIMARY KEY,     -- 32 random bytes base64url; doubles as the CSRF state
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  owner_user_id TEXT,                 -- who started it; NULL for an instance key. Checked before a code is exchanged.
  flow          TEXT NOT NULL CHECK (flow IN ('loopback','pasted','device','web')),
  pkce_verifier TEXT NOT NULL DEFAULT '',
  redirect_uri  TEXT NOT NULL,        -- exactly what was handed to the identity provider
  device_code   TEXT NOT NULL DEFAULT '',
  expires_at    INTEGER NOT NULL      -- swept every minute; a pasted callback after this is not_found
);
