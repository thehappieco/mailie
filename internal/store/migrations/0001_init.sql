-- Timestamps are int64 unix seconds; booleans are INTEGER 0/1; JSON columns
-- hold canonical JSON produced by Go and are never edited by SQL.
--
-- Identity, in two layers, because IMAP gives no stable message id:
--   physical: (folder_id, uidvalidity, uid) — what the server can address
--   logical:  group_key + dup_of           — what a human would call one message
-- Gmail exposes a labelled message once per label, and Exchange Online hands
-- the same message a new UID after a mailbox move. messages.id survives both,
-- which is why it is the id the API hands out.

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- rows: instance_id (random, quoted in webhook payloads), created_at,
-- credential_keyid_active (mirrors the environment; rewrap compares against it)

CREATE TABLE accounts (
  id               TEXT PRIMARY KEY,                       -- 'acc_' + 16 hex, never reused
  email            TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name     TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL CHECK (provider IN ('gmail','microsoft','imap')),
  auth_kind        TEXT NOT NULL CHECK (auth_kind IN ('oauth2','password')),
  imap_host        TEXT NOT NULL, imap_port INTEGER NOT NULL,
  smtp_host        TEXT NOT NULL, smtp_port INTEGER NOT NULL,
  smtp_tls         TEXT NOT NULL CHECK (smtp_tls IN ('implicit','starttls')),  -- implicit=465 WithSSL, starttls=587 TLSMandatory
  login_user       TEXT NOT NULL,                          -- SASL identity; differs from email on some generic IMAP servers
  oauth_tenant     TEXT NOT NULL DEFAULT '',               -- microsoft: 'common' covers work and personal accounts
  sync_tier        TEXT NOT NULL DEFAULT 'auto' CHECK (sync_tier IN ('auto','condstore','uidpoll')),
  sync_tier_resolved TEXT NOT NULL DEFAULT '',             -- what Caps() actually said at first login; an operator needs to see this
  save_sent_copy   INTEGER NOT NULL,                       -- 0 for gmail/microsoft (they file the copy themselves), 1 for generic imap
  initial_days     INTEGER NOT NULL DEFAULT 90,            -- initial sync window (SEARCH SINCE); 0 means everything
  folder_overrides TEXT NOT NULL DEFAULT '{}',             -- {"sent":"Itens Enviados"} beats SPECIAL-USE and the localised table
  state            TEXT NOT NULL CHECK (state IN ('pending_auth','active','needs_reauth','disabled','error')),
  state_reason     TEXT NOT NULL DEFAULT '',               -- 'AADSTS70008 refresh token expired', redacted
  state_changed_at INTEGER NOT NULL,
  last_ok_at            INTEGER NOT NULL DEFAULT 0,
  last_error            TEXT NOT NULL DEFAULT '',
  consecutive_failures  INTEGER NOT NULL DEFAULT 0,
  next_retry_at         INTEGER NOT NULL DEFAULT 0,
  last_idle_event_at    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE credentials (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  field      TEXT NOT NULL CHECK (field IN ('password','oauth_token')),
  -- oauth_token is the JSON of *oauth2.Token, rewritten whenever the token
  -- source returns a new pointer: Microsoft rotates the refresh token on every
  -- use, and a daemon that forgets to persist the new one dies after 90 days.
  -- OAuth client ids come from the environment, not from here.
  keyid      INTEGER NOT NULL,        -- copy of envelope byte[1], so a rewrap can select rows without decrypting
  ciphertext BLOB NOT NULL,           -- v1(1)||keyid(1)||nonce(12)||ct||tag(16)
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, field)
);
CREATE INDEX credentials_keyid ON credentials(keyid);

CREATE TABLE oauth_pending (
  state         TEXT PRIMARY KEY,     -- 32 random bytes base64url; doubles as the CSRF state
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  flow          TEXT NOT NULL CHECK (flow IN ('loopback','pasted','device')),
  pkce_verifier TEXT NOT NULL DEFAULT '',
  redirect_uri  TEXT NOT NULL,        -- exactly what was handed to the identity provider
  device_code   TEXT NOT NULL DEFAULT '',
  expires_at    INTEGER NOT NULL      -- swept every minute; a pasted callback after this is not_found
);

CREATE TABLE folders (
  id            INTEGER PRIMARY KEY,
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,                     -- as go-imap returns it (already UTF-8; it handles modified UTF-7)
  display_name  TEXT NOT NULL,
  delim         TEXT NOT NULL DEFAULT '/',
  attrs_json    TEXT NOT NULL DEFAULT '[]',        -- raw LIST attributes, for debugging a role that resolved oddly
  role          TEXT NOT NULL DEFAULT '' CHECK (role IN ('','inbox','all','sent','drafts','trash','junk','archive','flagged','important')),
  role_source   TEXT NOT NULL DEFAULT '' CHECK (role_source IN ('','inbox','special-use','name-table','override')),
  selectable    INTEGER NOT NULL DEFAULT 1,
  synced        INTEGER NOT NULL DEFAULT 1,        -- 0 for Gmail's \All (archive target only), \Flagged/\Important (flag views), \Noselect
  passive       INTEGER NOT NULL DEFAULT 0,        -- never synced, but holds rows we moved here ourselves, so archiving is not a deletion
  uidvalidity   INTEGER NOT NULL DEFAULT 0,
  uidnext       INTEGER NOT NULL DEFAULT 0,        -- UIDNEXT at the last SELECT (informational)
  max_seen_uid  INTEGER NOT NULL DEFAULT 0,        -- highest UID ever indexed under this uidvalidity; the NEW step starts here.
                                                   -- max(uid) would drop when the newest message is deleted and replay old mail as new.
  highest_modseq INTEGER NOT NULL DEFAULT 0,       -- condstore tier only
  backfill_floor  INTEGER NOT NULL DEFAULT 0,      -- lowest UID we intend to hold; the expunge diff ignores anything below
  backfill_cursor INTEGER NOT NULL DEFAULT 0,      -- resumable initial sync: lowest UID fetched so far (0 = done)
  last_full_uid_scan_at INTEGER NOT NULL DEFAULT 0,
  last_flag_scan_at     INTEGER NOT NULL DEFAULT 0,
  server_count  INTEGER NOT NULL DEFAULT 0,        -- EXISTS at the last SELECT; only ever a hint that something changed
  local_count   INTEGER NOT NULL DEFAULT 0, unseen_count INTEGER NOT NULL DEFAULT 0,
  sync_state    TEXT NOT NULL DEFAULT 'new' CHECK (sync_state IN ('new','initial','live','resync','error','disabled')),
  sync_error    TEXT NOT NULL DEFAULT '', last_synced_at INTEGER NOT NULL DEFAULT 0,
  missing_since INTEGER NOT NULL DEFAULT 0,        -- absent from LIST once; deleted only on a second consecutive miss
  UNIQUE (account_id, name)
);
CREATE INDEX folders_role ON folders(account_id, role);
-- One folder per role, so a localised-name table that matches twice is a loud
-- failure rather than a mailbox that silently files sent mail in two places.
-- 'archive' is exempt: several servers expose more than one archive folder.
CREATE UNIQUE INDEX folders_role_unique ON folders(account_id, role) WHERE role <> '' AND role <> 'archive';

CREATE TABLE messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT, -- ids are event cursors and cache keys: never reused
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_id     INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  uidvalidity   INTEGER NOT NULL,
  uid           INTEGER NOT NULL,
  modseq        INTEGER NOT NULL DEFAULT 0,
  message_id    TEXT NOT NULL DEFAULT '',          -- RFC 5322 header, angle brackets stripped; '' when absent
  group_key     TEXT NOT NULL,                     -- 'mid:'||message_id when it has an '@', else 'h:'||sha256(subject|from_addr|internal_date)
  dup_of        INTEGER REFERENCES messages(id) ON DELETE SET NULL, -- NULL marks the group's primary row; set on Gmail label copies
  in_reply_to   TEXT NOT NULL DEFAULT '', references_json TEXT NOT NULL DEFAULT '[]',
  subject       TEXT NOT NULL DEFAULT '',
  from_json     TEXT NOT NULL DEFAULT '[]', to_json TEXT NOT NULL DEFAULT '[]', cc_json TEXT NOT NULL DEFAULT '[]',
  bcc_json      TEXT NOT NULL DEFAULT '[]', reply_to_json TEXT NOT NULL DEFAULT '[]',
  from_addr     TEXT NOT NULL DEFAULT '',          -- first From address lowercased (indexed filter)
  from_text     TEXT NOT NULL DEFAULT '', to_text TEXT NOT NULL DEFAULT '',   -- FTS column sources
  date          INTEGER NOT NULL DEFAULT 0,        -- Date header; 0 when unparsable
  internal_date INTEGER NOT NULL,                  -- INTERNALDATE: ordering, pagination and the dedupe key
  size          INTEGER NOT NULL DEFAULT 0,        -- RFC822.SIZE
  flags_json    TEXT NOT NULL DEFAULT '[]',        -- raw flags, lowercased (imapmemserver lowercases; so do we, to compare)
  seen INTEGER NOT NULL DEFAULT 0, flagged INTEGER NOT NULL DEFAULT 0, answered INTEGER NOT NULL DEFAULT 0,
  draft INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0,
  has_attachments INTEGER NOT NULL DEFAULT 0,      -- from the BODYSTRUCTURE walk, with nothing downloaded
  snippet       TEXT NOT NULL DEFAULT '',          -- filled when the body is fetched, not during header sync
  body_fetched_at INTEGER NOT NULL DEFAULT 0,
  vanished_at   INTEGER NOT NULL DEFAULT 0,        -- missing from the server once; deleted on the second consecutive miss
  stale         INTEGER NOT NULL DEFAULT 0,        -- set during a UIDVALIDITY resync until a new UID claims the row
  first_seen_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  -- Last column on purpose: a list query reads the leading columns and would
  -- otherwise pull overflow pages it has no use for. Never evicted, because it
  -- is the FTS content.
  body_text     TEXT NOT NULL DEFAULT '',
  UNIQUE (folder_id, uidvalidity, uid)
);
CREATE INDEX messages_primary_date  ON messages(account_id, internal_date DESC, id DESC) WHERE dup_of IS NULL AND vanished_at = 0;
CREATE INDEX messages_folder_date   ON messages(folder_id, internal_date DESC, id DESC) WHERE vanished_at = 0;
CREATE INDEX messages_group         ON messages(account_id, group_key);
CREATE INDEX messages_folder_live   ON messages(folder_id, uidvalidity, uid) WHERE vanished_at = 0;
CREATE INDEX messages_reclaim       ON messages(folder_id, message_id, internal_date, size) WHERE message_id <> '';
CREATE INDEX messages_folder_unseen ON messages(folder_id, seen, internal_date DESC) WHERE seen = 0 AND vanished_at = 0;
CREATE INDEX messages_from          ON messages(account_id, from_addr);

CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, from_text, to_text, body_text,
  content='messages', content_rowid='id', tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, from_text, to_text, body_text)
  VALUES (new.id, new.subject, new.from_text, new.to_text, new.body_text);
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_text, to_text, body_text)
  VALUES ('delete', old.id, old.subject, old.from_text, old.to_text, old.body_text);
END;
-- Restricted to the indexed columns: marking a message read is the hot path and
-- must not rewrite a full-text row.
CREATE TRIGGER messages_au AFTER UPDATE OF subject, from_text, to_text, body_text ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_text, to_text, body_text)
  VALUES ('delete', old.id, old.subject, old.from_text, old.to_text, old.body_text);
  INSERT INTO messages_fts(rowid, subject, from_text, to_text, body_text)
  VALUES (new.id, new.subject, new.from_text, new.to_text, new.body_text);
END;

-- Folder counters are local truth. The server's EXISTS is a hint that
-- something changed, never the number we report.
CREATE TRIGGER messages_count_ai AFTER INSERT ON messages BEGIN
  UPDATE folders SET local_count = local_count + (new.vanished_at = 0),
                     unseen_count = unseen_count + (new.vanished_at = 0 AND new.seen = 0)
   WHERE id = new.folder_id;
END;
CREATE TRIGGER messages_count_ad AFTER DELETE ON messages BEGIN
  UPDATE folders SET local_count = local_count - (old.vanished_at = 0),
                     unseen_count = unseen_count - (old.vanished_at = 0 AND old.seen = 0)
   WHERE id = old.folder_id;
END;
CREATE TRIGGER messages_count_au AFTER UPDATE OF seen, folder_id, vanished_at ON messages BEGIN
  UPDATE folders SET local_count = local_count - (old.vanished_at = 0),
                     unseen_count = unseen_count - (old.vanished_at = 0 AND old.seen = 0)
   WHERE id = old.folder_id;
  UPDATE folders SET local_count = local_count + (new.vanished_at = 0),
                     unseen_count = unseen_count + (new.vanished_at = 0 AND new.seen = 0)
   WHERE id = new.folder_id;
END;

CREATE TABLE parts (                               -- every BODYSTRUCTURE leaf, flattened by the provider; nothing downloaded
  msg_id       INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,  -- local row id; messages.message_id is the RFC header
  path         TEXT NOT NULL,                      -- IMAP section, 1-based: '1', '1.2', '2'
  mime_type    TEXT NOT NULL,
  charset      TEXT NOT NULL DEFAULT '', encoding TEXT NOT NULL DEFAULT '',  -- needed to decode a part fetched on its own
  disposition  TEXT NOT NULL DEFAULT '', filename TEXT NOT NULL DEFAULT '', content_id TEXT NOT NULL DEFAULT '',
  size         INTEGER NOT NULL DEFAULT 0,         -- encoded octets as advertised
  is_body      INTEGER NOT NULL DEFAULT 0,
  is_attachment INTEGER NOT NULL DEFAULT 0,
  sha256       TEXT,                               -- NULL until downloaded; then <data>/att/<aa>/<sha256>
  PRIMARY KEY (msg_id, path)
);
CREATE INDEX parts_sha256 ON parts(sha256) WHERE sha256 IS NOT NULL;

CREATE TABLE bodies (                              -- what does not fit in messages.body_text; LRU bounded
  msg_id         INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  headers_raw    BLOB NOT NULL,                    -- BODY.PEEK[HEADER]
  text_html      TEXT,                             -- decoded, not sanitised (the API flags it html_unsafe)
  size           INTEGER NOT NULL, fetched_at INTEGER NOT NULL,
  last_access_at INTEGER NOT NULL                  -- written at most once an hour per row: a read must not be a write
);
CREATE INDEX bodies_lru ON bodies(last_access_at);

CREATE TABLE attachment_blobs (                    -- index of the files on disk; LRU bounded
  sha256 TEXT PRIMARY KEY, size INTEGER NOT NULL, last_access_at INTEGER NOT NULL
);
CREATE INDEX attachment_blobs_lru ON attachment_blobs(last_access_at);

CREATE TABLE drafts (                              -- local compose requests; not mirrored to the server's \Drafts in v1
  id             TEXT PRIMARY KEY,                 -- 'drf_' + 16 hex
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  compose_json   TEXT NOT NULL,
  compose_hash   TEXT NOT NULL,                    -- sha256(compose_json); send_draft(expected_hash) loses an edit race loudly
  message_id_hdr TEXT NOT NULL,                    -- 'uuid@sender-domain' WITHOUT angle brackets, fixed at creation.
                                                   -- go-mail's SetMessageIDWithValue adds the brackets itself.
  state          TEXT NOT NULL CHECK (state IN ('open','sending','sent','failed')),
  error          TEXT NOT NULL DEFAULT '',
  created_by     TEXT NOT NULL DEFAULT '',         -- api key prefix, for the audit trail
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, sent_at INTEGER NOT NULL DEFAULT 0,
  expires_at     INTEGER NOT NULL                  -- created_at + 7d; swept unless state='sending'
);
CREATE INDEX drafts_account ON drafts(account_id, state);

CREATE TABLE sends (                               -- the send state machine, idempotency reservation and audit trail
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,                   -- the client's; else 'draft:'||draft_id; else sha256(compose)||'/'||minute,
                                                   -- so a model that retries a timed-out send does not send twice
  compose_hash    TEXT NOT NULL,                   -- same key, same body: replay. Same key, different body: conflict.
  draft_id        TEXT NOT NULL DEFAULT '',
  message_id_hdr  TEXT NOT NULL,                   -- bare id generated before the first attempt and reused on every retry
  state           TEXT NOT NULL CHECK (state IN ('sending','sent','unknown','failed')),
                                                   -- 'unknown' is an error after DATA: Exchange delivers both copies of a
                                                   -- retry, so this is reconciled from the Sent folder, never resent.
  attempts        INTEGER NOT NULL DEFAULT 0,
  smtp_response   TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  sent_copy_state TEXT NOT NULL DEFAULT 'n/a' CHECK (sent_copy_state IN ('n/a','pending','appended','failed')),
  created_by      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, sent_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, idempotency_key)
);
CREATE INDEX sends_message_id ON sends(account_id, message_id_hdr);

CREATE TABLE api_keys (
  prefix       TEXT PRIMARY KEY,                   -- 8 hex characters: a lookup selector, not a secret
  hash         TEXT NOT NULL,                      -- Argon2id PHC string
  name         TEXT NOT NULL,
  scope        TEXT NOT NULL CHECK (scope IN ('read','write','send','admin')),
  created_at   INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  revoked_at   INTEGER NOT NULL DEFAULT 0,         -- revoked keys stay listed: part of the answer to "what could have read this"
  last_used_at INTEGER NOT NULL DEFAULT 0          -- written by a detached goroutine, at most once a minute per key
);
CREATE TABLE api_key_accounts (                    -- no rows for a key means every account
  key_prefix TEXT NOT NULL REFERENCES api_keys(prefix) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  PRIMARY KEY (key_prefix, account_id)
);

CREATE TABLE events (                              -- the journal behind the bus; seq is the SSE id and the wait cursor
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,
  type         TEXT NOT NULL,
  account_id   TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX events_account_seq ON events(account_id, seq);

CREATE TABLE webhooks (
  id                TEXT PRIMARY KEY,              -- 'whk_' + 16 hex
  url               TEXT NOT NULL,
  secret_ciphertext BLOB NOT NULL, keyid INTEGER NOT NULL,   -- same envelope, AAD field 'webhook_secret' with the webhook id
  events_json       TEXT NOT NULL DEFAULT '["message.new"]',
  accounts_json     TEXT NOT NULL DEFAULT '[]',    -- [] means every account the creating key could see, snapshotted
  enabled           INTEGER NOT NULL DEFAULT 1, disabled_reason TEXT NOT NULL DEFAULT '',
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE webhook_deliveries (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  webhook_id      TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  event_seq       INTEGER NOT NULL,                -- no foreign key: event retention must not delete a pending delivery
  attempt         INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('pending','delivered','dead')),
  last_status     INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE (webhook_id, event_seq)                   -- exactly one delivery row per (hook, event): a retry never duplicates
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at) WHERE status = 'pending';
