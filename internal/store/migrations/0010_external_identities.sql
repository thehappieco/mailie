-- People who sign in through an extension of the daemon (internal/app's
-- Options.Extensions, with service.SignInExternal) rather than with a
-- password: an identity provider's (issuer, subject) linked to a person, and
-- the public keys pinned for each identity at its first sign-in.
--
-- An ordinary migration, in one transaction with foreign keys on: two new
-- tables and their triggers, and two triggers on sessions. Nothing that
-- exists changes. A person who signs in only this way has no password: their
-- users.password_hash is '', which no password check accepts (internal/auth,
-- verifyPassword), and their password_changed_at is 0.

-- Which person an identity signs in. The subject is the provider's own stable
-- id for the person, never an address: the address may change at the
-- provider, the identity does not. A person has at most one subject per
-- issuer. Deleting the person deletes their identities (internal/auth,
-- DeleteTx, does it explicitly first, to take the pins with them).
CREATE TABLE user_identities (
  issuer     TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 300),  -- an origin, as a browser writes it
  subject    TEXT NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (issuer, subject),
  UNIQUE (user_id, issuer)
);

-- The public keys an identity is known by, pinned the first time each is
-- seen and never replaced: an extension that finds a different key under a
-- pinned key id refuses it. Insert only. No foreign key on the identity, since
-- a key is pinned before the sign-in that links its identity commits. A pin
-- whose identity never came to be (the sign-in was refused) signs nobody in,
-- and the hourly sweep deletes it once it is ten minutes old
-- (auth.SweepUnlinkedPins), which the trigger below allows.
CREATE TABLE identity_key_pins (
  issuer    TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 300),
  subject   TEXT NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  key_id    TEXT NOT NULL CHECK (length(key_id) BETWEEN 1 AND 255),
  key       BLOB NOT NULL CHECK (typeof(key) = 'blob' AND length(key) BETWEEN 1 AND 4096),
  pinned_at INTEGER NOT NULL,
  PRIMARY KEY (issuer, subject, key_id)
);

-- The repository never updates or deletes a pin but with its person; the
-- triggers are what holds whatever writes the row. An insert under a key id
-- already pinned changes nothing, whatever its conflict clause says: REPLACE
-- would delete the pin without firing the DELETE trigger below (recursive
-- triggers are off), and IGNORE keeps the ON CONFLICT DO NOTHING the
-- repository pins with.
CREATE TRIGGER identity_key_pins_once BEFORE INSERT ON identity_key_pins
WHEN EXISTS (SELECT 1 FROM identity_key_pins
              WHERE issuer = NEW.issuer AND subject = NEW.subject AND key_id = NEW.key_id)
BEGIN
  SELECT RAISE(IGNORE);
END;
CREATE TRIGGER identity_key_pins_fixed BEFORE UPDATE ON identity_key_pins
BEGIN
  SELECT RAISE(ABORT, 'a pinned key is never replaced');
END;
CREATE TRIGGER identity_key_pins_kept BEFORE DELETE ON identity_key_pins
WHEN EXISTS (SELECT 1 FROM user_identities WHERE issuer = OLD.issuer AND subject = OLD.subject)
BEGIN
  SELECT RAISE(ABORT, 'a pinned key goes only with the person it identifies');
END;

-- A session's lifetime is absolute (0002), and an extension may start one
-- for less than a password's: no statement may move a session's expiry
-- later, so none outlives what it was started for. An update that would is
-- refused, an upsert's DO UPDATE included. So is an insert under an id or a
-- token hash a session already has: INSERT OR REPLACE would delete that row
-- and write its own, which no UPDATE trigger sees. A session deleted and
-- then written again from nothing is a new session, as the code only ever
-- writes one (auth.startSessionTx), under a new id and token.
CREATE TRIGGER sessions_expiry_fixed BEFORE UPDATE OF expires_at ON sessions
WHEN NEW.expires_at > OLD.expires_at
BEGIN
  SELECT RAISE(ABORT, 'a session never lasts longer than it was started for');
END;
CREATE TRIGGER sessions_started_once BEFORE INSERT ON sessions
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = NEW.id OR token_hash = NEW.token_hash)
BEGIN
  SELECT RAISE(ABORT, 'a session never lasts longer than it was started for');
END;
