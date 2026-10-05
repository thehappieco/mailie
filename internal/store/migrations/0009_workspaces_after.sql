-- After 0008: what a schema-7 database may hold that 0008 could not set right,
-- since a rebuild adds rows and never removes or changes one that was there
-- (store/migrate.go, sameRows). An ordinary migration, in one transaction with
-- foreign keys on, which drops nothing.

-- 1. A person's key restricted to a mailbox its person cannot read.
--
-- Under schema 7 an owner saw the mailboxes nobody owned, and could restrict a
-- key of theirs to one. Those are the operator workspace's now, which no person
-- reaches, so such a restriction row reaches nothing while the key's listing
-- says it does. It goes, as at run time losing read takes a mailbox out of the
-- person's keys (workspace.forgetInKeysTx); a key left with none is revoked by
-- 0005's trigger, never widened to every mailbox the person has. An instance
-- key restricted to a person's mailbox keeps its rows: it reaches nothing, and
-- `mailserver apikey list` shows it for the operator to revoke.
DELETE FROM api_key_accounts
 WHERE EXISTS (SELECT 1 FROM api_keys k
                WHERE k.prefix = api_key_accounts.key_prefix AND k.user_id IS NOT NULL
                  AND NOT EXISTS (SELECT 1 FROM mailbox_access g
                                   WHERE g.account_id = api_key_accounts.account_id
                                     AND g.user_id = k.user_id AND g.read = 1));

-- 2. The first owner of a server nobody has signed up to yet.
--
-- Under schema 7 the first person to sign up became an owner whatever their
-- invite said, and `user invite --bootstrap` without --role made a member
-- invite that counted on it. Since 0008 the invite decides (docs/workspaces.md,
-- conflict 6). A server upgraded between that invite and its sign-up, with
-- nobody on it and no owner invite waiting, would be left without an owner:
-- the oldest invite still waiting becomes an owner's, the one the old quick
-- start had the operator make for themselves. Under schema 7 whoever signed up
-- first would have been the owner; this is the one invite the operator made
-- first.
UPDATE invites SET role = 'owner'
 WHERE rowid = (SELECT rowid FROM invites
                 WHERE workspace_id IS NULL AND used_at = 0 AND expires_at > unixepoch()
                 ORDER BY created_at, rowid LIMIT 1)
   AND NOT EXISTS (SELECT 1 FROM users)
   AND NOT EXISTS (SELECT 1 FROM invites
                    WHERE workspace_id IS NULL AND role = 'owner' AND used_at = 0 AND expires_at > unixepoch());
