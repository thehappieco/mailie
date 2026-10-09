-- Every person's account key, and a password the server never receives
-- (docs/key-scheme.md, sections 3 to 5, 11 and 12).
--
-- ALTER-only, as every migration of users before it: a rebuild of users would
-- be a DROP TABLE, which with foreign keys on deletes everything that refers
-- to a person. What is added:
--
--   - users.seal_id: the UUIDv4 every wrap and grant binds the person by
--     (section 3.1). Drawn here for every person who exists, by the code for
--     everyone created later (keyscheme.NewSealID), and by the trigger below
--     for a row written without one. Unique, and never changed.
--   - users.public_key: the account public key, 32 bytes, written once at the
--     person's own enrolment (section 4). Only the reset invitation (section
--     12.6) replaces it, in the same statement that moves key_replaced_at
--     forward; the trigger below refuses any other change. The reset deletes
--     every grant sealed to the old key and every platform wrap of the
--     person in the same transaction (internal/auth, CompleteReset): neither
--     exists yet, since mailbox keys come with migration 0014.
--   - users.auth_verifier and users.recovery_verifier: Argon2id PHC strings
--     of the auth key's and the recovery proof's text (section 5.7), at an
--     API key's cost.
--   - users.kdf_salt and users.kdf_m, kdf_t, kdf_p: the salt and the Argon2id
--     parameters the person's browser derives with, always the account's
--     target (section 5.3): the address's salt under the server's salt key
--     (meta.kdf_salt_key) and the server's default.
--   - users.password_wrap and users.recovery_wrap: the account key wrapped
--     under the password and the recovery code, 61 bytes starting with 0x02
--     (section 5.5).
--   - users.zk_enrolled_at: when the person enrolled in this scheme. One way:
--     once set it never changes, and an enrolled person has every column
--     above and no server-side password hash (section 12.7), which the CHECK
--     holds. users.password_hash is from then on '', which no password
--     matches: the server refuses a password in clear for them.
--   - sessions.authenticated_at: the session's step-up time (section 11), set
--     when a sign-in or a step-up verifies the person's own secret; 0 is
--     none. Sessions open now have none.
--   - sessions.stepup_mark_at: the hosted service's step-up mark (section
--     11): when the page started a step-up on this session, valid ten
--     minutes and used once; 0 is none.
--   - auth_tickets: the single-use tickets of the ceremonies (section 12),
--     ten minutes each, stored as SHA-256, bound to the person and, for a
--     password change and a sign-in's re-derivation, to the session and to
--     the auth key verified when it was issued (SHA-256 of it); each carries
--     the target the browser derives under.
--   - reset_invites: the reset invitations `user password --bootstrap`
--     prints (section 12.6), seven days, stored as SHA-256, used once.
--   - invites.seal_id: the seal id of the person an invitation signs up,
--     drawn by the server once, when the person's browser first opens the
--     invitation (section 12.1), so that the wraps it sends with the sign-up
--     are bound to the seal id the person gets; '' until then.

ALTER TABLE users ADD COLUMN seal_id TEXT NOT NULL DEFAULT '';
UPDATE users SET seal_id =
  lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) ||
  '-' || substr('89ab', 1 + (random() & 3), 1) || substr(lower(hex(randomblob(2))), 2) || '-' ||
  lower(hex(randomblob(6)))
WHERE seal_id = '';
CREATE UNIQUE INDEX users_seal_id ON users(seal_id);

ALTER TABLE users ADD COLUMN public_key BLOB
  CHECK (public_key IS NULL OR (typeof(public_key) = 'blob' AND length(public_key) = 32));
ALTER TABLE users ADD COLUMN key_replaced_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN auth_verifier TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN kdf_salt BLOB
  CHECK (kdf_salt IS NULL OR (typeof(kdf_salt) = 'blob' AND length(kdf_salt) = 16));
ALTER TABLE users ADD COLUMN kdf_m INTEGER NOT NULL DEFAULT 0 CHECK (kdf_m = 0 OR kdf_m BETWEEN 65536 AND 262144);
ALTER TABLE users ADD COLUMN kdf_t INTEGER NOT NULL DEFAULT 0 CHECK (kdf_t = 0 OR kdf_t BETWEEN 3 AND 10);
ALTER TABLE users ADD COLUMN kdf_p INTEGER NOT NULL DEFAULT 0 CHECK (kdf_p = 0 OR kdf_p BETWEEN 1 AND 4);
ALTER TABLE users ADD COLUMN password_wrap BLOB
  CHECK (password_wrap IS NULL OR (typeof(password_wrap) = 'blob' AND length(password_wrap) = 61
                                   AND substr(password_wrap, 1, 1) = x'02'));
ALTER TABLE users ADD COLUMN recovery_wrap BLOB
  CHECK (recovery_wrap IS NULL OR (typeof(recovery_wrap) = 'blob' AND length(recovery_wrap) = 61
                                   AND substr(recovery_wrap, 1, 1) = x'02'));
ALTER TABLE users ADD COLUMN recovery_verifier TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN zk_enrolled_at INTEGER NOT NULL DEFAULT 0
  CHECK (zk_enrolled_at = 0 OR (public_key IS NOT NULL AND auth_verifier <> '' AND kdf_salt IS NOT NULL
                                AND kdf_m <> 0 AND kdf_t <> 0 AND kdf_p <> 0 AND kdf_m * kdf_t <= 1048576
                                AND password_wrap IS NOT NULL AND recovery_wrap IS NOT NULL
                                AND recovery_verifier <> '' AND password_hash = ''));

-- A seal id has one spelling, a lowercase UUIDv4, and never changes. A row
-- written without one gets one drawn here, as the people who existed did.
CREATE TRIGGER users_seal_id_spelled BEFORE INSERT ON users
WHEN NEW.seal_id <> '' AND NEW.seal_id NOT GLOB
  '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-4[0-9a-f][0-9a-f][0-9a-f]-[89ab][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
BEGIN
  SELECT RAISE(ABORT, 'a seal id is a lowercase UUIDv4');
END;
CREATE TRIGGER users_seal_id_drawn AFTER INSERT ON users
WHEN NEW.seal_id = ''
BEGIN
  UPDATE users SET seal_id =
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) ||
    '-' || substr('89ab', 1 + (random() & 3), 1) || substr(lower(hex(randomblob(2))), 2) || '-' ||
    lower(hex(randomblob(6)))
  WHERE rowid = NEW.rowid;
END;
CREATE TRIGGER users_seal_id_fixed BEFORE UPDATE OF seal_id ON users
WHEN NEW.seal_id IS NOT OLD.seal_id AND (OLD.seal_id <> '' OR NEW.seal_id NOT GLOB
  '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-4[0-9a-f][0-9a-f][0-9a-f]-[89ab][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]')
BEGIN
  SELECT RAISE(ABORT, 'a seal id never changes');
END;

-- The account public key is written once. The reset invitation is the one
-- replacement, and it moves key_replaced_at forward in the same statement;
-- nothing else changes the key, nor takes it away. This stops the server's
-- own code from replacing it by mistake, and nothing more: whoever can write
-- the database can replace it (the threat model's section 5.3).
CREATE TRIGGER users_public_key_once BEFORE UPDATE OF public_key ON users
WHEN OLD.public_key IS NOT NULL AND NEW.public_key IS NOT OLD.public_key
 AND NEW.key_replaced_at <= OLD.key_replaced_at
BEGIN
  SELECT RAISE(ABORT, 'an account public key is written once; only a reset replaces it');
END;

-- Enrolment is one way: once set, zk_enrolled_at never changes, so nothing
-- takes a person back to a password the server checks.
CREATE TRIGGER users_enrolled_once BEFORE UPDATE OF zk_enrolled_at ON users
WHEN OLD.zk_enrolled_at <> 0 AND NEW.zk_enrolled_at IS NOT OLD.zk_enrolled_at
BEGIN
  SELECT RAISE(ABORT, 'enrolment in the key scheme is one way');
END;

ALTER TABLE invites ADD COLUMN seal_id TEXT NOT NULL DEFAULT '' CHECK (seal_id = '' OR seal_id GLOB
  '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-4[0-9a-f][0-9a-f][0-9a-f]-[89ab][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]');

ALTER TABLE sessions ADD COLUMN authenticated_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN stepup_mark_at INTEGER NOT NULL DEFAULT 0;

CREATE TABLE auth_tickets (
  hash       BLOB PRIMARY KEY CHECK (typeof(hash) = 'blob' AND length(hash) = 32),  -- sha256 of 32 random bytes
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- The session a password change or a sign-in's re-derivation belongs to;
  -- NULL for a recovery and the upgrade's enrolment, which have none yet.
  session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
  purpose    TEXT NOT NULL CHECK (purpose IN ('password','rederive','recover','enrol')),
  -- The target the browser derives under, fixed when the ticket is made.
  kdf_salt   BLOB NOT NULL CHECK (typeof(kdf_salt) = 'blob' AND length(kdf_salt) = 16),
  kdf_m      INTEGER NOT NULL,
  kdf_t      INTEGER NOT NULL,
  kdf_p      INTEGER NOT NULL,
  -- SHA-256 of the auth key a password change's first step or a sign-in
  -- verified when it issued the ticket: finishing presents that key again,
  -- so whoever saw only the answer that carried the ticket sets nothing.
  -- NULL for a recovery and the upgrade's enrolment.
  proof      BLOB CHECK (proof IS NULL OR (typeof(proof) = 'blob' AND length(proof) = 32)),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,                                         -- created_at + 10 minutes
  CHECK ((purpose IN ('password','rederive')) = (session_id IS NOT NULL)),
  CHECK ((purpose IN ('password','rederive')) = (proof IS NOT NULL))
);
CREATE INDEX auth_tickets_user ON auth_tickets(user_id);
CREATE INDEX auth_tickets_session ON auth_tickets(session_id);

CREATE TABLE reset_invites (
  code_hash  BLOB PRIMARY KEY CHECK (typeof(code_hash) = 'blob' AND length(code_hash) = 32),  -- sha256 of the code
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- Issued with --force: completing it does not ask again whether the person
  -- is the last reader of a team mailbox.
  forced     INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1)),
  created_by TEXT NOT NULL DEFAULT 'cli',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL                                          -- created_at + 7 days
);
CREATE INDEX reset_invites_user ON reset_invites(user_id);
