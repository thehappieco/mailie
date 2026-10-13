-- A workspace's kind, source and person never change (docs/workspaces.md,
-- "Workspaces" and "The workspace source").
--
-- An ordinary migration: one trigger is added, nothing else changes, and no
-- row of a schema-15 database does. Nothing here ever changed these three
-- columns (a team's name is the only thing a workspace's row changes, with
-- its updated_at); the trigger is what holds whatever writes the row.
--
--   - kind: personal, team or operator, as created. The rules that read it
--     (a personal workspace has its person as its only member, the operator
--     workspace has none, which workspace a key or a mailbox belongs to) are
--     each checked when a row is written, and would not be checked again by
--     a kind changed afterwards.
--   - source: local, created and changed here, or another source, whose
--     workspaces and memberships are sent from elsewhere and never changed
--     here. What deleting a person takes with them depends on it: a team
--     whose source is local and whose only member is the person goes with
--     them, mailboxes and all; a team from another source outlives every
--     person here (internal/workspace, GoesWithPersonSQL). A source changed
--     afterwards would move a team from one side of that rule to the other.
--   - person_id: the person of a personal workspace, whose mailboxes go with
--     them. A personal workspace never passes to anyone else.

CREATE TRIGGER workspaces_fixed BEFORE UPDATE OF kind, source, person_id ON workspaces
WHEN NEW.kind IS NOT OLD.kind OR NEW.source IS NOT OLD.source OR NEW.person_id IS NOT OLD.person_id
BEGIN
  SELECT RAISE(ABORT, 'a workspace''s kind, source and person never change');
END;
