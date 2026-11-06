-- A link at the provider: a Google or Microsoft mailbox connected through the
-- web flow without its address being typed here. The provider's account
-- chooser picks the account, its answer names the address, and the mailbox,
-- its key and its linker's grant are created only at the return, in one
-- transaction, once one login with that address worked (internal/account,
-- links.go).
--
-- An ordinary migration: one table and its indexes are added, nothing else
-- changes, and no row of a schema-25 database does. No mailbox exists before
-- the return, so accounts keeps its rules (an address, linked once per
-- workspace) and nothing that reads an address changes.
--
--   - oauth_links: one row per link, from its start until 10 minutes after it
--     expires (about 20 minutes, until the sweep that runs every minute
--     deletes it): its flow's state, who started it, the team it links into
--     (NULL for the starter's personal workspace, which the transaction that
--     creates the mailbox resolves), the provider, the PKCE verifier and the
--     redirect the provider was told, and what the starter asked for (a
--     name, the team's sync text). It is taken once, by
--     its starter, in the statement that sets taken_at, before its code is
--     exchanged, and finished once: connected, with the mailbox it created,
--     or failed, with a fixed reason and the address the provider named, if
--     it named one. A finished link keeps neither its verifier nor what was
--     asked for, a connected one never an address, and a mailbox deleted
--     for good leaves its link naming none.
--
-- Nothing sealed waits here, and no address while the link waits.

CREATE TABLE oauth_links (
  id                   TEXT PRIMARY KEY CHECK (id LIKE 'lnk_%'),
  state                TEXT NOT NULL UNIQUE,                                -- the flow's state
  owner_user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id         TEXT REFERENCES workspaces(id) ON DELETE CASCADE,  -- NULL: the starter's personal workspace
  provider             TEXT NOT NULL CHECK (provider IN ('gmail', 'microsoft')),
  oauth_client         TEXT NOT NULL CHECK (oauth_client = 'web'),
  pkce_verifier        TEXT NOT NULL DEFAULT '',                            -- cleared at the finish
  redirect_uri         TEXT NOT NULL,
  display_name         TEXT NOT NULL DEFAULT '',                            -- cleared at the finish
  sync_consent_version TEXT NOT NULL DEFAULT '',                            -- cleared at the finish
  created_at           INTEGER NOT NULL,
  expires_at           INTEGER NOT NULL,
  taken_at             INTEGER NOT NULL DEFAULT 0,                          -- 0: not taken
  finished_at          INTEGER NOT NULL DEFAULT 0,                          -- 0: not finished
  outcome              TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'connected', 'failed')),
  account_id           TEXT REFERENCES accounts(id) ON DELETE SET NULL,     -- connected only
  email                TEXT NOT NULL DEFAULT '',                            -- failed only, once the provider named it
  reason               TEXT NOT NULL DEFAULT '',                            -- failed only: account.Reason*
  CHECK ((outcome = '') = (finished_at = 0)),
  CHECK (outcome <> 'connected' OR taken_at <> 0),
  CHECK (outcome = 'connected' OR account_id IS NULL),
  CHECK (outcome = 'failed' OR (email = '' AND reason = '')),
  CHECK (outcome <> 'failed' OR reason <> ''),
  CHECK (finished_at = 0 OR (pkce_verifier = '' AND display_name = '' AND sync_consent_version = ''))
);
CREATE INDEX oauth_links_owner  ON oauth_links(owner_user_id);
CREATE INDEX oauth_links_expiry ON oauth_links(expires_at);
