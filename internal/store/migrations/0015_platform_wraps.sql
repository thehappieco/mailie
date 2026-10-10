-- The account key of a person who signs in through an identity provider
-- (docs/key-scheme.md sections 6.3, 12.8 and 12.10): its wrap under the
-- product key the provider delivers to the person's page, and the tickets of
-- the first sign-in that writes it.
--
-- An ordinary migration: two tables and their triggers are added, nothing
-- else changes. Neither names a provider. An extension of the daemon
-- (internal/app's Options.Extensions) signs people in through one, and the
-- service writes and reads these rows for it (service.SignInExternal,
-- service.EnrolExternal); a server without such an extension leaves both
-- empty.
--
--   - platform_wraps: the person's account key wrapped under a key derived
--     from the product key, one per product key id, as the person's browser
--     made it: 61 bytes starting with 0x03 (the kit's platformwrap, under
--     Mailie's profile), which the server checks the shape of and never
--     opens. The product key id is Mailie's product and an epoch,
--     "mailie:<epoch>", the epoch 1 to 2^31 - 1 in decimal without a leading
--     zero, exactly as the provider names it. Written once, at the person's
--     first sign-in (section 12.10): never changed, and deleted only with
--     the person, or by the reset invitation (section 12.6), which replaces
--     their account key and deletes every platform wrap of the old one in
--     its transaction (internal/auth, CompleteReset), as migration 0013's
--     header said it would.
--   - external_enrolments: the single-use tickets a sign-in through an
--     identity provider answers a person who has no account key yet, in
--     place of a session (section 12.10): ten minutes each, stored as
--     SHA-256, bound to the person, the identity that signed in and the
--     product key id pinned for it, and carrying the sign-in's
--     authentication time and the session length the extension asked for,
--     which the session the enrolment opens gets. auth_tickets cannot hold
--     them: its target columns are NOT NULL, and such a person has no
--     password to derive.

CREATE TABLE platform_wraps (
  user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_key_id TEXT NOT NULL CHECK (length(product_key_id) BETWEEN 8 AND 17
                                      AND product_key_id GLOB 'mailie:[1-9]*'
                                      AND substr(product_key_id, 8) NOT GLOB '*[^0-9]*'
                                      AND CAST(substr(product_key_id, 8) AS INTEGER) <= 2147483647),
  wrap           BLOB NOT NULL CHECK (typeof(wrap) = 'blob' AND length(wrap) = 61 AND substr(wrap, 1, 1) = x'03'),
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (user_id, product_key_id)
);

-- Insert only. An update is refused, and so is an insert under a person and
-- a product key id that have a wrap already, whatever its conflict clause
-- says: REPLACE would delete the wrap without any UPDATE trigger seeing it.
CREATE TRIGGER platform_wraps_once BEFORE INSERT ON platform_wraps
WHEN EXISTS (SELECT 1 FROM platform_wraps
              WHERE user_id = NEW.user_id AND product_key_id = NEW.product_key_id)
BEGIN
  SELECT RAISE(ABORT, 'a platform wrap is written once');
END;
CREATE TRIGGER platform_wraps_fixed BEFORE UPDATE ON platform_wraps
BEGIN
  SELECT RAISE(ABORT, 'a platform wrap is written once');
END;

CREATE TABLE external_enrolments (
  hash           BLOB PRIMARY KEY CHECK (typeof(hash) = 'blob' AND length(hash) = 32),  -- sha256 of 32 random bytes
  user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- The identity whose sign-in issued it, which must still sign the person
  -- in when it is used.
  issuer         TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 300),
  subject        TEXT NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  -- The product key id the sign-in was delivered, pinned for that identity:
  -- the one the platform wrap is bound to and stored under.
  product_key_id TEXT NOT NULL CHECK (length(product_key_id) BETWEEN 8 AND 17
                                      AND product_key_id GLOB 'mailie:[1-9]*'
                                      AND substr(product_key_id, 8) NOT GLOB '*[^0-9]*'
                                      AND CAST(substr(product_key_id, 8) AS INTEGER) <= 2147483647),
  -- The sign-in's authentication time (unix seconds, 0 for none), at most
  -- its now: the step-up time of the session the enrolment opens.
  auth_time      INTEGER NOT NULL,
  -- How long that session lasts, in seconds: what the sign-in asked for.
  session_ttl    INTEGER NOT NULL CHECK (session_ttl BETWEEN 1 AND 1209600),
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL                                        -- created_at + 10 minutes
);
CREATE INDEX external_enrolments_user ON external_enrolments(user_id);
