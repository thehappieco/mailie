# Workspaces

**Status:** phase 2 (approved by the owner on 2026-10-05) is implemented on the server and in the
console: migrations 0008 and 0009 and the runner's rebuild procedure, `internal/workspace`, the
service's authorization on workspaces and grants for every path (REST, MCP, the event stream and
the long poll), the routes, the command line and the contract fixtures. **The workspace model of
2026-10-06** — Mailie follows Wappie's: a team's mailbox belongs to the team, its owners and admins
manage it by their role, the last reader is protected — was approved by the owner on 2026-10-06,
and its first step is implemented on the server (migration 0011, the service, the routes, the
command line, the contract fixtures and the open texts' revisions); the console follows it. Its
**API keys of the workspace (step 3) are not built yet**: until then a person's keys work as
[Keys, tools and the operator](#keys-tools-and-the-operator) says. Not yet either: the platform's
workspace source, of which only the stub is here. [`console.md`](console.md) describes the routes
as the console reads them, and its screens. Where this document had to choose between readings of
the plan, or found a rule that conflicts with the code or with another rule, it says so in
[Conflicts and resolutions](#conflicts-and-resolutions).

## In short

- Every mailbox belongs to a **workspace**: a person's **personal** workspace, a **team**
  workspace, or the one **operator** workspace, which has no members and is what instance keys and
  the command line act on. A personal mailbox is its person's; a team's mailbox is the **team's**,
  whoever linked it; who linked it is attribution only.
- Using a mailbox takes **active membership** in its workspace and a **grant** on it: `read`,
  `act` and `send` each open one use. **Owners and admins manage every mailbox of their workspace
  by their role** — its card, re-authorizing it, removing it, who holds what — and **read none of
  them by being one**. `read` passes only from an owner or an admin who reads the mailbox now;
  `act` (to someone who reads) and `send` from any owner or admin. `manage` is stored only for
  members, where it means the card and re-authorizing.
- **Consents.** A personal mailbox syncs under its person's consent; a team mailbox under its
  **workspace's**, which an owner or an admin gives on the team's behalf, to the current sync text,
  and any of them withdraws, deleting its index for everyone. A team mailbox also syncs only while
  someone can read it. An action needs the actor's own consent and the `act` flag; a send the
  sender's consent and the `send` flag, and goes out under the sender's name.
- **Protections:** a team keeps an active owner, and a team mailbox someone reads keeps a
  **reader**. Nobody is protected for having linked a mailbox; there is no taking a link over.
- Members see only the mailboxes they hold a grant on; only owners and admins (and the operator)
  see the team's members and who holds what, invite, and remove; a member or an admin does not
  leave by themselves; an owner leaves while another owner remains.
- The same address may be linked in several workspaces; each link is an independent mailbox with
  its own credentials, worker and index. Within one workspace an address is linked at most once.
- `users.role` (`owner`, `member`) is purely the **instance role** of a self-hosted server. It
  gives no mailbox visibility. The first owner comes only from `user invite --bootstrap`.
- Migration 0008 created the workspaces and rebuilt `accounts`; 0009 set right what 0008 could not
  change; **0011** makes team mailboxes their team's (see [Migration 0011](#migration-0011)).

## The model

### Workspaces

| Kind | How many | Members | Created by |
|---|---|---|---|
| `personal` | one per person | exactly that person, as `owner`; nobody else, ever | the person's creation, in the same transaction |
| `team` | any number | people with role `owner`, `admin` or `member`; always at least one active `owner` | a person in the console (who becomes its owner), or the operator for a named owner |
| `operator` | exactly one | none | migration 0008 |

```sql
workspaces(id, kind, source, name, person_id, created_at, updated_at)
```

- `id` is `wsp_` and 16 hex characters for a workspace created here, the platform's own id for
  one the platform source sends, and the fixed `wsp_operator` for the operator's. Ids are never
  reused.
- `source` is `local` (created and changed from the console and the command line) or `platform`
  (mirrored from the platform; see [The workspace source](#the-workspace-source)). The operator
  workspace is always `local`.
- `name` is 1–80 characters for a team; empty for the personal and operator workspaces, which a
  client names itself ("Personal", "Operator").
- `person_id` names the person of a personal workspace and is `NULL` otherwise (`UNIQUE`, and a
  `CHECK` ties it to the kind). It has no `ON DELETE` action: deleting a person deletes their
  personal workspace explicitly, in the deletion's transaction, as their mailboxes are today.
- A unique partial index on `kind` where `kind = 'operator'` keeps the operator workspace single.

A team is deleted only with its last member (`user delete`), with its mailboxes; moving a mailbox
from one workspace to another is not part of this phase.

### Members

```sql
workspace_members(workspace_id, user_id, role, status, created_at, updated_at)  -- PK (workspace_id, user_id)
```

- `role` is `owner`, `admin` or `member`; `status` is `active` or `disabled`.
- An **active member** is a membership with `status = 'active'` whose person is active on the
  instance (`users.status = 'active'`). Every rule below that says "member" means an active one.
- A disabled membership stays listed but carries no access: disabling deletes the person's grants
  in that workspace in the same transaction, and enabling again restores the membership only, never
  the grants (as Wappie does).
- Any real change of a member's role or status **expires the invites they created** in that team
  that are still waiting: each was made under a standing they may no longer have. Being made an
  owner or an admin takes a stored `manage` away (the role gives it; a schema trigger does it, and
  a grant that held nothing else goes); losing that role ends the consent attempts they started on
  the team's mailboxes.
- A personal workspace's single membership never changes; the operator workspace has none. A
  trigger refuses a membership in the operator workspace, and one in a personal workspace for
  anyone but its person, beside the repository's own check.
- Memberships cascade on the person (`ON DELETE CASCADE`), after the deletion has checked the
  protections below.

### Mailboxes

- `accounts.workspace_id` is required and never changes (a trigger refuses an update).
- `accounts.owner_user_id` means what its name says: **the person whose mailbox it is**. It is set
  exactly for a mailbox of a personal workspace, to that workspace's person, and `NULL` for a team's
  and for the operator's; a trigger refuses any other combination (0011 replaced 0008's "linker"
  triggers). A team's mailbox belongs to the team.
- `accounts.linked_by` is **attribution only**: who linked the mailbox (`usr_…`, `key:<prefix>` or
  `cli`). It decides nothing, is shown only in the access directory, and is blanked when that
  person is deleted.
- Addresses are unique per workspace, `UNIQUE (workspace_id, email)` with `email COLLATE NOCASE`,
  instead of across the daemon. A second link of an address already linked in the same workspace is
  `409` with the generic "that address is already connected".
- Each link is a mailbox of its own: its own id, credentials and OAuth grant, sync worker with its
  three connections, index, events and send records. Nothing is shared between two links of one
  address, and nothing says to anyone that an address is linked in another workspace.
- Linking the same address in several workspaces multiplies the connections against the provider's
  per-user limit (three per link; Gmail allows 15 for all of a user's apps). The engine already
  backs off on `too many connections`; there is no cap, because refusing a link "because the address
  is linked elsewhere" would tell a stranger where an address is linked.

Who may link a mailbox:

| Into | Who | With |
|---|---|---|
| a personal workspace | its person | a session |
| a team workspace | an `owner` or `admin` of it | a session |
| the operator workspace | the operator | an unrestricted instance admin key |

The person who links a mailbox gets `read`, `act` and `send` on it in the transaction that creates
it, which re-checks their membership and role there (like `CheckOwnersWith`); they manage it by
their role. They also start, and own, its OAuth consent flow: only they finish it, whoever else
manages the mailbox. `POST /v1/accounts` takes an optional `workspace_id`; without it a person links
into their personal workspace and an instance key into the operator workspace. Into a team it also
takes `sync_consent_version`: naming the current revision of the sync text gives the team's consent
to sync it, on the team's behalf, in the same transaction; left out, the mailbox is linked with sync
off, until an owner or an admin turns it on. A personal or operator link refuses it.

Removing a mailbox repeats its id (`DELETE /v1/accounts/{id}?confirm=<id>`), or nothing is removed
(`400`). A team's mailbox is its owners' and admins' to remove, a personal one its person's, an
operator one the operator's.

### Grants

```sql
mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
  PRIMARY KEY (account_id, user_id)
  FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE
  FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
  CHECK (act = 0 OR read = 1)
  CHECK (read + act + send + manage > 0)
```

`workspace_id` is carried on the grant so that the two composite foreign keys make "a grant only
for a member of the mailbox's own workspace" a property of the schema, and make removing a member
or a mailbox take the grants with it. A row with no flag left is deleted, never stored. Since 0011
a trigger refuses a stored `manage` for an owner or an admin of the grant's workspace.

| Flag | Lets the holder | Requires |
|---|---|---|
| `read` | search and read messages, fetch originals and attachments, list folders, receive the mailbox's events, see its storage, ask for a sync pass (with the `write` scope) | |
| `act` | mark read or unread, star, archive, move, trash, undo a move | `read` (an action names messages the caller can read), and the actor's actions consent |
| `send` | send from the mailbox, read the records of their own sends | the sender's send consent; a reply or forward also needs `read` on the original |
| `manage` | see the mailbox's card and re-authorize it | held by owners and admins through their role, on every mailbox of their workspace, who also remove it and change who holds what on it; stored only for members |

**Effective manage** is one SQL expression, as Wappie's `role IN ('owner','admin') OR can_manage`:
the role, or a stored `manage` of a member. It never opens the index.

Two levels of seeing follow from this:

- **Any grant, or managing it by the role**, shows the mailbox in `GET /v1/accounts`,
  `GET /v1/accounts/{id}` and `GET /v1/accounts/{id}/sync`: its card — address, provider, state,
  sync counters, `access` — and nothing it holds: no subject, no correspondent, no folder name.
- **`read`** opens its index: folders, messages, events, storage.

Without any grant or role the mailbox does not exist for the caller (`404`). With the card but
without the flag an operation needs, it is `403 not_authorized`.

### People on the instance

`users.role` stays `owner` or `member`, as the **instance role** of a self-hosted server:

- An instance `owner` may invite people to the server, disable and delete people, and is the only
  role that may change the server's instance-level settings (none is changed over the API yet).
- It gives **no mailbox visibility**. Mailboxes nobody's are the operator workspace's, reached only
  by instance keys and the command line. No person ever gets operator powers through the operator
  workspace.
- The first owner comes only from `mailserver user invite --bootstrap --role owner`. The rule
  "whoever signs up first becomes an owner, whatever the invite says" (`auth.SignUp`) is gone (see
  [conflict 6](#conflicts-and-resolutions) for the bootstrap default).

Invitations stay single use, bound to one address, valid for 7 days, and their code travels in the
link's fragment. There are two kinds, in the same `invites` table:

| Kind | Created by | Accepted by | Gives |
|---|---|---|---|
| instance invite (`workspace_id` empty) | an instance owner's session, or an unrestricted instance admin key | sign-up | a new person with the invite's instance role, and their personal workspace |
| team invite | a team `owner` (any role) or `admin` (role `member` only), or the operator | a signed-in person with that address (`POST /v1/auth/invites/accept`); or sign-up, for an address without an account, **only when the operator or an instance owner made it** | membership in the team with the invite's role; a new person also gets instance role `member` and their personal workspace |

A team invite for someone already a member of that workspace is refused when created (`409`). An
acceptance that is refused (already a member, wrong address, expired) does not spend the invite.
Accepting never grants access to a mailbox.

Any person may create a team and invite into it, so a team invite signs a new person up only when
its creator may bring people onto the server, **as they stand when it is redeemed**: the operator
(`created_by` `cli`, or an instance key's `key:<prefix>`) or an instance owner still active. Were
any team invite enough, any member could create an account for any address that has none, with a
password of their choosing. Any other team invite offered at sign-up is `403 not_authorized`
("sign in with that address and accept it"), whether or not the address has an account, so the
link does not tell its holder which addresses do; the invite is not spent, and its person accepts it
once they have an account (an invitation to the server first, then the team's).

Redeeming an invite spends, in the same transaction, the other unused invites to the same team for
the same address, and a sign-up also the instance invites still waiting for that address, which can
never sign it up again (and would count as an owner on the way); only an instance owner's or the
operator's team invite signs anyone up, the same people who make instance invites. A member leaving
a team — removed, or their membership disabled — takes the team's invites still waiting for their
address with them. Otherwise a duplicate invite made before they joined would let a removed person
back in, with the role it names, within its 7 days. An invite to another team stays: that team
decided it.

An invite also **expires with its creator's standing** (Wappie's rule): when their role or status
in the team changes, when they are removed from it, and, for every invite they made, instance and
team alike, when they are disabled or deleted on the instance. The record stays, and the hourly
sweep deletes it 30 days on; deleting the person then blanks `created_by`. Migration 0011 expired
the invites whose creator could no longer make them.

### Consents

| Use | Whose consent | And |
|---|---|---|
| Sync of a personal mailbox (storing the index) | its **person**'s (`users.sync_consent_*`), as `store/eligibility.go` reads it | |
| Sync of a team mailbox | its **workspace**'s, on the mailbox: `sync_enabled_at`, `sync_enabled_by` (who gave it, an id), `sync_consent_version` (the revision of the sync text) | someone reads it: an active member, active on the instance, holding `read` |
| Sync of an operator mailbox | the operator's switch, in the same columns, with no revision | |
| Actions | the **actor**'s, at the current revision | the actor's `act` flag |
| Sending | the **sender**'s, at the current revision | the sender's `send` flag |
| `\Answered` after a reply | the sender's actions consent | the sender's `act` flag |

- An owner or an admin gives a team mailbox's consent on the team's behalf, signed in, to the
  current revision of the sync text (`MAIL_CONSENT_VERSION_SYNC`, the same text that covers
  personal mailboxes): with the link (`sync_consent_version`) or later
  (`PUT /v1/accounts/{id}/sync` `{"enabled": true, "version": "…"}`). It is recorded separately,
  per mailbox, even when the person also agreed for themselves. A consent to an earlier revision
  keeps syncing, as a person's does; agreeing again takes the current one.
- Any owner or admin turns it off (`{"enabled": false}`): the same transaction deletes the
  mailbox's index for every reader — its messages, folders and events — and the service compacts
  and scrubs.
- A person's withdrawal of their own sync consent deletes the index of **their personal
  mailboxes only**; it never reaches a team's (but for a migrated consent still bound to them,
  below).
- Disabling, removing or closing a person does not stop a team mailbox of a team others are in,
  nor remove it: it is the team's — but for a migrated consent still bound to them, below, and a
  mailbox only they read, which no reader is left for. Disabling a person stops their personal
  mailboxes (the eligibility rule requires the person active); deleting them removes those, and
  every team whose only member they were, with its mailboxes. The open sync text says both.
- A team mailbox **nobody can read** syncs nothing more; its consent stays, and its owners and
  admins see it marked (`no_reader`). Read passes only from someone who reads it, so its sync is
  never turned on again (`409`), and the way out is removing it, or removing it and linking it
  again (the linker gets `read`); turning it off deletes what is still indexed.
- **A migrated consent stays bound to its linker.** Migration 0011 copied each team mailbox's
  linker's own consent (when the linker was active and had agreed) to the mailbox, with its date
  and revision, and marked it (`sync_enabled_via = 'migration'`, `sync_enabled_by` = the linker).
  The text they agreed to promised that turning their sync off deletes the index of every mailbox
  they linked; until an owner or an admin confirms the team's consent at the current revision —
  which replaces the mark — that promise holds: the linker withdrawing their consent, or being
  disabled or deleted on the instance, stops the mailbox and deletes its index in the same
  transaction (`store.StopBoundTx`). While someone else reads such a mailbox, disabling or
  deleting its linker on the instance is refused without `force`, naming it (`Blocks.BoundTo`),
  since it deletes that reader's index; with `force`, the answer lists what stopped
  (`team_syncs_stopped`). A team mailbox whose linker was disabled at the migration kept its index
  and stays stopped, bound the same way (its consent off), until an owner or an admin turns it on
  for the team, or off, or removes it; deleting the linker deletes that index, and disabling them
  again changes nothing. The daemon lists these at start; one only the disabled linker read is
  listed as read by nobody instead, and can only be removed or turned off.
- The From name is the **sender's** profile name (`fromName` reads the caller), so an account's
  `send.from_name` is per caller. An instance key sends under the address alone.

## Who may do what

### Using a mailbox

| Operation | A person (session, or their key within its scope) | An instance key |
|---|---|---|
| list it, read its card | any grant; or owner or admin of its workspace, by the role | operator mailbox |
| folders, search, read, originals, attachments, events, storage | `read` | operator mailbox, `read` scope |
| ask for a sync pass | `read`, `write` scope | operator mailbox, `write` scope |
| switch sync on or off | a team mailbox: its owners and admins, signed in (the team's consent); a personal one: never (its person's own consent decides) | operator mailbox, unrestricted admin key |
| act | `act`, actions consent, `write` scope | operator mailbox, `write` scope |
| send, read own send records | `send`, send consent, a session (personal keys never have `send`) | operator mailbox, `send` scope |
| re-authorize | manage (owner or admin, or a member holding `manage`), a session | operator mailbox, `admin` scope |
| remove (repeating its id) | a team's: its owners and admins; a personal one: its person; a session | operator mailbox, `admin` scope |
| restrict a new key to it | `read` | operator mailbox |

### Administering a workspace

| | Team `owner` | Team `admin` | Team `member` | Operator (unrestricted instance admin key) |
|---|---|---|---|---|
| list my workspaces | yes | yes | yes | sees every workspace |
| rename the team | yes | yes | no | yes |
| list members | yes | yes | no (`403`) | yes |
| see the access directory | every mailbox | every mailbox | no (`403`) | every mailbox |
| see each team mailbox's card | every one, by the role | every one, by the role | those they hold a grant on | operator mailboxes only |
| invite | any role | `member` only | no | any role |
| list and revoke pending invites | yes | yes, `member` invites | no | yes |
| change a role or status | anyone's | members' only, never to `admin` or `owner` | no | anyone's |
| remove a member | anyone, themselves included while another active owner remains | members only, never themselves | no one, not themselves | anyone |
| link a mailbox; give or withdraw the team's consent to sync it | yes | yes | no | no (operator mailboxes only) |
| remove a mailbox | yes | yes | no | operator mailboxes only |
| re-authorize a mailbox | yes | yes | with a stored `manage` on it | operator mailboxes only |
| grant `read` | on a mailbox they read themselves, now | likewise | no | no |
| grant `act` | to someone who reads after the change, themselves included | likewise | no | no |
| grant `send` | to any active member, themselves included | likewise | no | no |
| grant `manage` | to members | to members | no | to members |
| revoke | any flag, their own included | any flag, their own included | no, not their own | any flag |

Any person may create a team, with a session, and becomes its owner; the operator creates one for
a named existing person. A personal workspace's person is its owner, and holds every power above
in it, with `read`, `act` and `send` on each of its mailboxes; personal workspaces have no invites,
other members or renaming. The operator workspace has no members, grants or invites. Every row is
subject to the [protections](#protections). A person's key never administers anything: these routes
take a session or the operator's key.

### Who may change a grant

1. **Who.** An active owner or admin of the mailbox's workspace, re-read inside the transaction; or
   the operator, who grants `manage` to members and revokes. A member who holds a grant on it sees
   the mailbox and is `403`; anyone else is `404`.
2. **What.** `read` that the person did not hold passes only from an owner or an admin who **reads
   the mailbox themselves, now**, checked in the same transaction: this stands in for Wappie's
   sealed grant, which only a holder of the device key can pass on, and it is what makes "owners
   and admins read nothing by their role" true rather than one click away. `act` needs `read` after
   the change (`CHECK act = 0 OR read = 1`), and `send` nothing: any owner or admin gives them, to
   anyone, themselves included, without holding them. `manage` is stored for members only; for an
   owner or an admin it is `400` ("owners and admins manage every team mailbox by their role").
   Losing `manage` ends, in the same transaction, the consent attempts the person started on that
   mailbox (`oauth_pending`). Storing what a consent produced checks again, in its own transaction,
   that its starter still manages the mailbox, or for an attempt an instance key started that the
   mailbox is the operator's (`workspace.ManagesTx`, installed by the service with
   `account.Registry.CheckFlowsWith`).
3. **To whom.** An active member of the mailbox's workspace. Nobody else, and nobody in a personal
   or the operator workspace.
4. **Revoking** any flag is an owner's, an admin's or the operator's, under the
   [last-reader rule](#protections); an owner or an admin drops their own flags, a member does not.
5. The grant records `granted_by` (`usr_…`, `key:<prefix>`, `cli`, or `migration`).

### Instance administration

| | Instance `owner` session | Instance `member` session | Unrestricted instance admin key |
|---|---|---|---|
| instance invite (`POST /v1/users/invites`) | yes | no | yes |
| disable a person (`POST /v1/users/disable`) | yes, not themselves | no | yes |
| delete a person (`POST /v1/users/delete`) | yes, not themselves | no | yes |
| `database_bytes` in storage | yes | no | no |
| instance keys (`/v1/apikeys`, with `MAIL_ADMIN_API=true`) | no | no | yes |

## Protections

Each is checked inside the transaction that would break it, re-reading the actor's own authority
there, and refused with `409 conflict`. The member and access listings mark them in advance
(`last_owner`, `last_reader_of`, `readers`, `no_reader`) so a console can explain before anyone
tries, as Wappie's directory does; the mutation enforces them regardless.

| | Refused | The way out |
|---|---|---|
| **Last owner** | demoting, disabling or removing the last active owner of a team, or that owner leaving | promote another member first |
| **Last reader** | taking `read` from the last person who can read a team mailbox — revoking it, disabling or removing their membership, leaving, closing their account without `force` | give another member `read` first (an owner or an admin who reads it can), or remove the mailbox |
| **Personal** | any change to a personal workspace's membership; an invite into one; a grant change on one of its mailboxes | |
| **Operator** | a membership, grant or invite in the operator workspace | |

The **last-reader rule** is Mailie's form of Wappie's `requireRemainingReader`: a team mailbox with
at least one reader is never left with none. A reader is an active member, active on the instance,
holding `read`; keys, disabled members and a role on its own never count ("ownership is not a
recovery path"). Removing the mailbox is not guarded, nor is a role change. The database has one
writer, so two changes that would each leave one reader are ordered, and the second is refused.
The refusal says: "that is the last person who can read a mailbox of the workspace; give another
member read on it first, or remove the mailbox".

On the instance, `user disable` and `user delete` also refuse, unless `force`, a person who is the
last active owner of a team with other active members; the last reader of a team mailbox of a team
that outlives them — any other member remains, whatever their status, since a team goes only with
its last member and nobody could read the mailbox again; or the linker a migrated team consent is
still bound to while someone else reads that mailbox. The instance's own last-owner rule stays as
it is. With `force`, a team can be left without an owner (the operator assigns one with `mailserver
member role`), a team mailbox without a reader, which then syncs nothing and is marked
`no_reader`, and a bound mailbox stopped with its index deleted.
Deleting a person removes the mailboxes of their personal workspace and every team whose only member
they were, with its mailboxes, through the account registry's usual clean-up (workers, cached
credentials, consent attempts, compaction); a team mailbox of a team others are in stays.

### Taking over a link

Gone with the linker. Phase 2 kept a team mailbox syncing under its linker's consent, so the linker
was protected and `POST /v1/accounts/{id}/take-over` moved the link to someone else; a team mailbox
is the team's now, and that route answers `404`.

## Errors

The seven codes do not change.

| Situation | Code |
|---|---|
| a mailbox the caller neither holds a grant on nor manages by their role, another workspace's mailbox, a workspace the caller is not an active member of | `404 not_found`, never `403` |
| a mailbox the caller sees, without the flag the operation needs; a role that may not do this (a member listing members, leaving, granting); a person's key on an administration route | `403 not_authorized` |
| a protection; a platform-sourced workspace changed locally; a missing actions or send consent | `409 conflict` |
| a personal or the operator workspace as the target of a member, grant or invite operation; an `act` without `read`; `manage` for an owner or an admin; a removal without its id repeated; a consent to another revision than the current one | `400 bad_request` |

## Keys, tools and the operator

The approved model makes every API key its workspace's, created by an owner or an admin, with
mailboxes given by someone who reads them (step 3 of the plan of 2026-10-06, **not built yet**).
Until it is, keys work as phase 2 made them:

- **Instance keys** (no person) reach only the operator workspace's mailboxes, over REST and MCP
  alike ([conflict 3](#conflicts-and-resolutions)). A restriction to accounts keeps working, and may
  name only operator mailboxes, through the route and through `apikey create --bootstrap` alike.
- **Person keys** reach what their person can, live, within their scope (`read` or `write`) and
  their optional restriction: the cards of what their person manages by a role included, and
  never the index of a mailbox their person does not read. Losing `read` on a mailbox also deletes it from the restrictions of
  that person's keys in the same transaction, so a key made for that mailbox alone is revoked by
  0005's trigger, as when a mailbox is removed today, rather than waking up again if the person is
  granted access later. A key with other mailboxes left keeps working for those. What a caller
  holds for long — the stdio MCP session, a resource subscription, an event stream — authenticated
  once, with the restriction as it was; the re-check before each answer (`auth.Keys.Recheck`)
  refuses a principal that still names a mailbox its key no longer does, so it never reaches the
  lost mailbox again, even once `read` comes back. A mailbox removed since is no difference: its
  id is never reused. The refusal is `409 conflict` ("this key no longer reaches a mailbox it was
  authenticated with"), not the `401` of a key that no longer works: the key does, and the caller
  authenticates again — reconnects the stream, restarts the stdio session — and goes on with what
  the key still names.
- **MCP** tools and resources call the same service methods and see exactly what the key may; the
  account they list carries `workspace_id` and `access`. `Principal.Tool` no longer changes what an
  instance key sees, since REST and MCP now agree; it keeps refusing sessions on `/mcp`.

## Choosing the workspace in a request

There is no "current workspace" on the server: sessions and keys are not bound to a workspace, so a
person's key reaches their mailboxes in every workspace they belong to, as the plan says. Instead:

- Every mailbox-bearing answer carries `workspace_id` (accounts, storage).
- `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events` and
  `GET /v1/events/wait` accept `?workspace=ID`, which narrows them to that workspace's mailboxes. A
  workspace the caller is not an active member of is `404`; with `account` as well, the account
  must be in that workspace.
- The console keeps the current workspace itself (per tab, remembered for each person in local
  storage) and passes `?workspace=` so a view never fetches another workspace's data
  ([`console.md`](console.md), "Workspaces in the console").

## Events

The event gate (`internal/service/events.go`) today remembers per account whether the caller may
see it, assuming that never changes. With grants it does:

- The service keeps an **access epoch**, a counter it advances after every commit that can give or
  take `read` from someone: a grant set or revoked, a membership disabled or removed, a mailbox
  linked or removed, a person disabled or deleted. A role never gives `read`, so a role change
  moves nothing a stream carries; an owner's or an admin's stream carries a team mailbox's events
  only while they hold `read` on it. Switching a team mailbox's sync off deletes its events with
  its index; nothing journals the switch itself. One daemon writes the database (the lock),
  so a counter in memory sees every change; the `--bootstrap` commands run with the daemon stopped.
- Each subscription keeps the set of mailboxes the caller can read and the epoch it read it at.
  Before deciding on an event, if the epoch moved, it reads the set again (one query). Events are
  decided at delivery with current access: a mailbox's events stop the moment its revocation
  commits, and resuming from an old cursor never yields events of a mailbox the caller cannot read
  now. A new grant brings that mailbox's journal from the cursor on, which is history the index
  already shows them.
- **SSE.** Before each batch and at each ping (15 s) the handler asks the stream to check the
  credential, as now, **and** access. When a mailbox leaves the set the stream writes
  `event: access` with `{"account_id": "…", "read": false}`, and `"read": true` when one joins;
  like `lagged`, it has no `id`, since it is not a journal entry. A stream ends with `event: error`
  and `{"code": "not_found", …}` only when its filter can match nothing again: every mailbox named
  by `?account=`, or every mailbox of a restricted key, is gone, or the caller is no longer an
  active member of the workspace `?workspace=` named. Do not reconnect with the same filter then. A
  stream of everything the caller may read stays open however little that is — none since the last
  grant was revoked, or none yet — because one opened now would be the same: a mailbox granted or
  linked later appears on it without reconnecting.
- **`send.finished`** is not the mailbox's but one sender's record (see
  [conflict 10](#conflicts-and-resolutions)): its payload names the sender (`user_id`), and it goes
  only to whoever may read that record with `GET /v1/sends/{key}` — the person who sent it, with
  the `send` scope and the `send` flag on the mailbox at delivery, `read` or not; an instance key's
  send, to the instance keys with the `send` scope. A member who reads a shared mailbox never hears
  of another member's sends, and a member who may send without reading hears of their own, on a
  stream without `?account=` (which names mailboxes the caller reads).
- **Long poll.** The same gate: events of a mailbox lost during the wait are left out, and
  `?account=` naming a mailbox the caller does not see is `404`, and one they see without `read`
  is `403`, when the call starts.
- **MCP.** `wait_for_new_mail` uses the same service method. Subscribing to an inbox needs `read`
  on it (`service.MayFollow`): a mailbox the key does not see is `resource not found`, one it sees
  without `read` is refused. After every wait the watcher asks again for each inbox subscribed to,
  so one the key can no longer read stops being notified, also when a lagged wait would mark every
  inbox as changed, and is dropped from the watcher; subscribing again is refused the same way.

The transport stays thin: the stream reports why it ended through a service error the handler
renders, as it renders a credential failure today.

## Storage

`GET /v1/me/storage` answers for the mailboxes the caller can **read**:

```jsonc
{
  "mailboxes": [{"account_id": "acc_…", "workspace_id": "wsp_…", "email": "…", "messages": 2, "bytes": 1037}],
  "workspaces": [{"workspace_id": "wsp_…", "mailboxes": 1, "messages": 2, "bytes": 1037}],
  "total": {"messages": 2, "bytes": 1037},
  "database_bytes": 1048576            // an instance owner's session only, as today
}
```

`workspaces` sums the caller's readable mailboxes per workspace, never a mailbox they cannot read,
so it is not a workspace's whole usage. An instance key answers for the operator workspace.

## REST

New routes. A session is a person signed in; "operator" is an unrestricted instance admin key.

| Route | Who | What |
|---|---|---|
| `GET /v1/workspaces` | read | `[Workspace]`: a person's active memberships; an instance key the operator workspace; the operator every workspace |
| `POST /v1/workspaces` | session; operator | `{name}` (the operator adds `owner_email`) → `Workspace` (201), a team, `local` source only |
| `PATCH /v1/workspaces/{id}` | team owner or admin; operator | `{name}` → `Workspace` |
| `GET /v1/workspaces/{id}/members` | owner or admin; operator (a member: `403`) | `[Member]` |
| `PATCH /v1/workspaces/{id}/members/{user_id}` | see the table; operator | `{role?, status?}` → `Member` |
| `DELETE /v1/workspaces/{id}/members/{user_id}` | see the table (an owner themselves while another owner remains); operator | `204` |
| `GET /v1/workspaces/{id}/invites` | owner or admin; operator | `[Invite]`, pending only |
| `POST /v1/workspaces/{id}/invites` | owner or admin; operator | `{email, role}` → `Invite` with its `url` (201) |
| `DELETE /v1/workspaces/{id}/invites/{invite_id}` | owner or admin; operator | `204`; the unused invite is deleted |
| `POST /v1/auth/invites/accept` | session | `{invite}` → `Workspace`: joins with the invite's role |
| `GET /v1/workspaces/{id}/access` | owner or admin; operator (a member: `403`) | `[MailboxAccess]`, every mailbox of the workspace |
| `PUT /v1/accounts/{id}/access/{user_id}` | see [the grant rules](#who-may-change-a-grant); session; operator for `manage` | `{read, act, send, manage}`, all four required, not all false → `Grant` |
| `DELETE /v1/accounts/{id}/access/{user_id}?flags=` | owner or admin, their own flags included; operator | `204`; `flags` (comma-separated `read`, `act`, `send`, `manage`) names what goes, every flag without it |

Gone: `POST /v1/accounts/{id}/take-over` (`404`; see [Taking over a link](#taking-over-a-link)).

Changed routes:

| Route | Change |
|---|---|
| `POST /v1/accounts` | optional `workspace_id`; who may link where, as [above](#mailboxes); into a team, optional `sync_consent_version` (the current sync text: the team's consent, given with the link) |
| `GET /v1/accounts`, `GET /v1/accounts/{id}`, `GET /v1/accounts/{id}/sync` | a team's owners and admins see the card of each of its mailboxes |
| `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events`, `GET /v1/events/wait` | optional `?workspace=` |
| `DELETE /v1/accounts/{id}?confirm=<id>` | the id repeated, or `400` and nothing removed; a team's owners and admins, a personal mailbox's person, the operator for its own |
| `POST /v1/accounts/{id}/oauth/start`, `POST /v1/accounts/oauth/callback` | manage (callback: the flow is still only its starter's) |
| `PUT /v1/accounts/{id}/sync` | `{enabled, version}`: a team mailbox's consent, from its owners and admins signed in (on: `version` the current sync text; off: deletes its index for everyone); an operator mailbox's switch, from the operator, with no `version`; `400` for a personal mailbox; `409` to turn on a team mailbox nobody can read. Answers `AccountSync` |
| `POST /v1/auth/signup` | redeems an instance invite, or a team invite the operator or an instance owner made; the first sign-up is no owner unless its invite says so |
| `POST /v1/users/disable`, `POST /v1/users/delete` | also an instance owner's session; the team protections (last owner, last reader of a team that outlives them, a consent still bound to them that someone else reads); `team_syncs_stopped` lists the team mailboxes a closure stopped |
| `GET /v1/sends/{key}?account=` | only the caller's own sends ([conflict 10](#conflicts-and-resolutions)) |

Shapes:

```jsonc
// Workspace
{"id": "wsp_0a1b2c3d4e5f6a7b", "kind": "team", "source": "local", "name": "Support",
 "role": "admin", "status": "active", "created_at": 1790000000}
// the operator's listing has no role or status, and adds "members" and "mailboxes" counts

// Member ("person_disabled": true only for a person switched off on the instance; last_reader_of
// lists the team mailboxes they alone read, always present)
{"user_id": "usr_…", "email": "bea@example.org", "name": "Bea Lima", "role": "member",
 "status": "active", "last_owner": false, "last_reader_of": ["acc_…"], "joined_at": 1790000000}

// MailboxAccess, whose grants are each a Grant. linked_by is attribution only (absent once that
// person is deleted); readers counts who can read it; no_reader marks a team mailbox nobody can
// read; sync is the mailbox's own consent (absent for a personal mailbox): migrated marks one the
// upgrade copied from its linker and nobody confirmed yet, current whether version is the
// revision asked for now.
{"account_id": "acc_…", "email": "support@example.org", "provider": "gmail", "state": "active",
 "linked_by": "usr_…", "readers": 2, "no_reader": false,
 "sync": {"enabled": true, "enabled_at": 1790000000, "enabled_by": "usr_…",
          "version": "2026-10-open-sync-3", "current": true},
 "grants": [{"account_id": "acc_…", "user_id": "usr_…", "read": true, "act": true, "send": true,
             "manage": false, "granted_by": "migration", "updated_at": 1790000000}]}
// a Grant's manage is the stored flag, which only a member holds; owners and admins manage by
// their role, which the members list says

// TeamInvite (role is the role in the team; url only in the answer that creates it). An instance
// invite keeps the shape POST /v1/users/invites always answered: {email, role, url, expires_at}.
{"id": "inv_…", "email": "bea@example.org", "workspace_id": "wsp_…", "role": "member",
 "url": "https://…/#invite=…&email=…", "created_by": "usr_…", "created_at": 1790000000,
 "expires_at": 1790604800}
```

`Account` gains (and, since 0011, no longer carries `linked_by`):

```jsonc
{
  "workspace_id": "wsp_…",
  "access": {"read": true, "act": true, "send": true, "manage": true},
  "send": {"available": false, "reason": "not_granted"}
}
```

`access` is what the caller may do: their grant, with `manage` from it or from their role; for an
instance key, what its scope allows on an operator mailbox.
`send.reason` `not_owner` becomes `not_granted` (no `send` flag). The contract fixtures change only
through `go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`, and the
console's types and `web/test/contract.spec.ts` hold them to the new fields.

## Command line

The command line stays a client of the daemon with `MAIL_ADMIN_KEY`, the operator. Addresses take
`--email -` as elsewhere.

```sh
mailserver workspace list
mailserver workspace create --name NAME --owner EMAIL
mailserver workspace rename --workspace ID --name NAME
mailserver member list   --workspace ID
mailserver member role   --workspace ID --email EMAIL --role owner|admin|member
mailserver member remove --workspace ID --email EMAIL
mailserver access list   --workspace ID
mailserver access grant  --account ID --email EMAIL --manage
mailserver access revoke --account ID --email EMAIL [--read] [--act] [--send] [--manage]   # none: every flag
mailserver user invite --email EMAIL [--role owner|member]                      # an instance invite
mailserver user invite --email EMAIL --workspace ID [--role owner|admin|member] # a team invite
```

- `access grant` grants `manage` only, to a member: `read` passes from an owner or an admin who
  reads the mailbox, and `act` and `send` from an owner or an admin, in the console
  ([conflict 7](#conflicts-and-resolutions)). `access list` shows each mailbox's own consent to sync
  and marks one nobody can read.
- `account add|list|authorize|folders|remove|sync` and `apikey` keep working, on the operator
  workspace. `account list` no longer lists people's mailboxes; `account remove` repeats the id
  (`?confirm=`).
- `user invite --bootstrap` takes `--workspace` too. Without `--role`, `--bootstrap` makes an
  `owner` invite while the instance has no active owner and no live owner invite, and a `member`
  invite otherwise ([conflict 6](#conflicts-and-resolutions)).
- `user disable|delete` take `--force` for the protections (the last owner of a team, the last
  reader of a team mailbox) as for the last instance owner; without it the daemon's refusal, which
  the command prints, names the teams and mailboxes it is about. With `--bootstrap` they run the
  very same service method against the database.
  `user delete` also prints how many teams went with the person.

## The workspace source

Where workspaces and memberships come from is an internal interface, in a new package
`internal/workspace` (which also holds the repository for workspaces, members and grants, and
which the transports may not import: it joins `store`, `sync`, `provider` and `account` in
depguard's list):

```go
// Source is where workspaces and their memberships come from.
type Source interface {
	// Name is "local" or "platform".
	Name() string
	// Changeable reports whether workspaces and memberships may be created and changed here,
	// from the console and the command line: nil for the local source; ErrManagedElsewhere
	// for the platform source, which mirrors what the platform sends and creates nothing.
	Changeable() error
	// PersonCreatedTx runs inside the transaction that creates a person. The local source
	// creates their personal workspace and its membership there; the platform source does
	// nothing, since the platform's document creates it.
	PersonCreatedTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error
}
```

- **Local** implements everything in this document.
- **Platform** is a stub in this phase: `Changeable` refuses (the service answers `409`, "workspaces
  are managed in the account console"), and `PersonCreatedTx` creates nothing. A row's own `source`
  is checked too: a `platform` workspace is never changed locally, whatever the configured source.
- **Selection** is `app.Options.WorkspaceSource`, set only by a binary that embeds `internal/app`;
  `nil` is local. There is no environment variable, and `serve` never sets it: nothing hosted is in
  the core.
- Unchanged in both: the operator workspace, linking mailboxes, grants, keys and consents, which
  are Mailie's whatever the source.
- At start the daemon gives a personal workspace to any person without one, as old as the person
  (`workspace.Repository.RepairPersonal`), under the local source only. Every person gets theirs in
  the transaction that creates them; only a binary from before 0008, run on a migrated database,
  creates one without (see [Going back](#going-back)).

## Migration 0008

What becomes of a schema-7 database:

| Before | After |
|---|---|
| each person | a personal workspace (`wsp_` + random hex, `created_at` the person's) and their `owner` membership, `active` |
| a mailbox with `owner_user_id` = P | in P's personal workspace, with a grant to P of all four flags, `granted_by = 'migration'` |
| a mailbox nobody owns | in the operator workspace (`wsp_operator`), without grants |
| an instance key | unchanged; reaches the operator workspace's mailboxes, over REST too |
| a person's key, its restriction rows | unchanged by 0008; reaches the mailboxes of its person's it reached (its person holds a full grant on each). A restriction to a mailbox nobody owned, which an owner could choose before, reaches nothing now: [0009](#migration-0009) takes it out |
| invites | gain `id` (`inv_` + random hex, unique), `workspace_id` (`NULL`; `ON DELETE SET NULL`) and `workspace_role` (`''`) |
| `users.role` | unchanged; read as the instance role |
| `meta`, `credentials`, `oauth_pending`, `folders`, `messages`, `parts`, `bodies`, `messages_fts`, `attachment_blobs`, `drafts`, `sends`, `api_keys`, `api_key_accounts`, `events`, `webhooks`, `webhook_deliveries`, `sessions` | untouched, row for row and value for value |

The smallest real shape (one owner, the mailboxes they connected, no keys) ends with one personal
workspace, the same mailboxes in it and a full grant on each; the owner's console shows what it
showed. A server with more people also sees two changes of behaviour, both intended: an owner no
longer sees the CLI's mailboxes in the console, and an instance key no longer reaches a person's
mailbox over REST (one restricted only to people's mailboxes now reaches nothing, and is listed by
`apikey list` for the operator to revoke).

The file, in outline (the real `accounts_new` repeats every column of today's `accounts` with its
type, default and `CHECK`, as `sqlite_schema` holds it after 0007):

```sql
-- migration: rebuild
CREATE TABLE workspaces (
  id         TEXT PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('personal','team','operator')),
  source     TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local','platform')),
  name       TEXT NOT NULL DEFAULT '',
  person_id  TEXT UNIQUE REFERENCES users(id),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  CHECK ((kind = 'personal') = (person_id IS NOT NULL))
);
CREATE UNIQUE INDEX workspaces_one_operator ON workspaces(kind) WHERE kind = 'operator';
CREATE TABLE workspace_members ( … );                 -- as above
INSERT INTO workspaces(id, kind, created_at, updated_at) VALUES ('wsp_operator', 'operator', unixepoch(), unixepoch());
INSERT INTO workspaces(id, kind, person_id, created_at, updated_at)
  SELECT 'wsp_' || lower(hex(randomblob(8))), 'personal', id, created_at, created_at FROM users;
INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
  SELECT id, person_id, 'owner', 'active', created_at, created_at FROM workspaces WHERE kind = 'personal';

CREATE TABLE accounts_new (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  email TEXT NOT NULL COLLATE NOCASE,                  -- no longer UNIQUE on its own
  …every other column of accounts, unchanged…,
  UNIQUE (workspace_id, email),
  UNIQUE (id, workspace_id)                            -- the parent key of mailbox_access
);
INSERT INTO accounts_new(rowid, id, workspace_id, email, …)
  SELECT a.rowid, a.id,
         coalesce((SELECT w.id FROM workspaces w WHERE w.person_id = a.owner_user_id), 'wsp_operator'),
         a.email, …
    FROM accounts a;                                   -- every column named, never SELECT *
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
CREATE INDEX accounts_owner ON accounts(owner_user_id);
-- triggers: workspace_id never changes; owner_user_id IS NULL exactly in the operator workspace

CREATE TABLE mailbox_access ( … );                     -- as above
CREATE INDEX mailbox_access_user ON mailbox_access(user_id);
CREATE INDEX mailbox_access_member ON mailbox_access(workspace_id, user_id);
INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
  SELECT id, workspace_id, owner_user_id, 1, 1, 1, 1, 'migration', created_at, created_at
    FROM accounts WHERE owner_user_id IS NOT NULL;

ALTER TABLE invites ADD COLUMN id TEXT NOT NULL DEFAULT '';
UPDATE invites SET id = 'inv_' || lower(hex(randomblob(8)));
CREATE UNIQUE INDEX invites_id ON invites(id);
ALTER TABLE invites ADD COLUMN workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL;
ALTER TABLE invites ADD COLUMN workspace_role TEXT NOT NULL DEFAULT ''
  CHECK (workspace_role IN ('', 'owner', 'admin', 'member'));
-- a team that goes takes its invites still waiting; a used one stays, without its team
CREATE TRIGGER workspaces_unused_invites BEFORE DELETE ON workspaces
BEGIN DELETE FROM invites WHERE workspace_id = OLD.id AND used_at = 0; END;
```

A team is deleted only with its last member (`user delete`). Its used invites are other people's
records of how they arrived, and stay, detached from it (`workspace_id` `NULL`, `workspace_role`
kept), until those people go; an unused one would read as an instance invite, so it goes first,
in `workspace.DeletePersonTx` and in the trigger alike.

`rowid` is copied so the rows come out of the rebuild exactly as they went in. The children of
`accounts` (`credentials`, `oauth_pending`, `folders`, `messages`, `drafts`, `sends`,
`api_key_accounts`) name it as `REFERENCES accounts(id)`, by name, so they refer to the new table
once it carries the name. No trigger or view mentions `accounts`; the only index is
`accounts_owner`, recreated, and the `UNIQUE` constraints bring their own.

## Migration 0009

A rebuild adds tables and rows and never removes or changes a row that was there (the runner's own
guard), so what a schema-7 database may hold that 0008 cannot carry over as it is goes into an
ordinary migration after it, `0009_workspaces_after.sql`, applied in the same upgrade:

| Before | After |
|---|---|
| a person's key restricted to a mailbox nobody owned (an owner could choose one under schema 7; it is the operator's now, which no person reaches) | that restriction row deleted, as losing `read` takes a mailbox out of a person's keys at run time; a key left with none is revoked by 0005's trigger, never widened to every mailbox. A key with a mailbox of its person's left keeps it |
| an instance key restricted to a person's mailbox | unchanged: it reaches nothing, and `apikey list` shows it for the operator to revoke |
| a server nobody has signed up to, with instance invites waiting and none of them an owner's | the oldest of them becomes an `owner` invite |

The last row is the quick start of earlier releases: `user invite --bootstrap` without `--role`
made a `member` invite, and the first sign-up became an owner whatever it said. That rule is gone
([conflict 6](#conflicts-and-resolutions)); a server upgraded between that invite and its first
sign-up would otherwise be left without an owner. Under schema 7 whoever signed up first would have
been the owner; the migration picks the invite the operator made first, which the old quick start
made for themselves. A server with people and no active owner (the last one deleted with `--force`,
say) is not changed: the daemon says so at start, and the operator invites one, with an address
that has no account yet.

## Migration 0011

`0011_team_mailboxes.sql`, an ordinary migration in one transaction with foreign keys on: three
columns added to `accounts` (`sync_consent_version`, `sync_enabled_via`, `linked_by`), rows
updated, two triggers replaced, three added, nothing dropped but 0008's two linker triggers. What
becomes of a schema-10 database:

| Before | After |
|---|---|
| a personal mailbox | unchanged; `linked_by` = its person. The new rule on `owner_user_id` runs over every row (`UPDATE accounts SET owner_user_id = owner_user_id`): one naming another person than its workspace's fails the migration, which then changes nothing |
| a team mailbox whose linker is active and agreed to sync | its linker's consent becomes the team's, with its date and revision (`sync_enabled_at`, `sync_consent_version`), `sync_enabled_by` = the linker and `sync_enabled_via = 'migration'`: bound to them until an owner or an admin confirms it ([Consents](#consents)); `owner_user_id` `NULL`, `linked_by` = the linker. It keeps syncing |
| a team mailbox whose linker agreed and is **disabled** | no consent copied: stopped, its index kept as it was, bound to them (`sync_enabled_by`, `migration`), so deleting them deletes it; listed by the daemon at start, as one to turn on again (or off, or remove) while someone else reads it, and otherwise as one nobody can read, which can only be removed or turned off |
| a team mailbox whose linker never agreed, or withdrew | no consent; sync stays off, with the empty index the withdrawal left |
| an operator mailbox | unchanged |
| the linker's grant | kept, as an ordinary grant |
| an owner's or an admin's grant | `manage` taken away (the role gives it); a grant of `manage` alone deleted |
| a member's grant | unchanged, `manage` included |
| an invite waiting whose creator could not make it now: deleted (schema 10 blanked `created_by` and left the invite pending); a `usr_` creator no longer active; an instance invite from someone no longer an instance owner; a team invite from someone no longer an active owner of the team, or an admin of it for a role other than member | expired (`expires_at` = the migration's time); the sweep deletes it 30 days on. `cli` and `key:` creators untouched |
| sessions, keys, consents of people, everything else | untouched, value for value |

New triggers: a mailbox names a person exactly when it is in that person's personal workspace
(`accounts_person_on_insert`, `…_on_update`); a stored `manage` is refused for an owner or an admin
(`mailbox_access_manage_insert`, `…_update`); a member made an owner or an admin loses a stored
`manage`, and a grant that held nothing else (`workspace_members_promoted`).

The hosted service's shape — one person, two personal mailboxes, no team — changes no team row:
the two grants keep `read`, `act` and `send`, with `manage` from the role, and the consents stay as
they were. The tests in `internal/store/migrate_eleven_test.go` dump every table before and after,
on that shape and on a self-hosted one with teams, and compare them value by value except the
columns named.

## Going back

Schemas 10 and 11 are refused by an older binary like any newer one: going back past 0011 is the
backup taken before the upgrade, restored with the binary of its time.

From the release of 0008 on the runner refuses a database whose `user_version` is past the last migration
the binary embeds (`store.ErrSchemaTooNew`): `Open`, `mailserver migrate` and
`mailserver migrate --dry-run` say which schema the database is at and which the binary knows, and
stop. A binary rolled back alone would read and write a schema it does not know.

Binaries from before 0008 have no such guard. One started on a migrated database opens it, creates
people without a personal workspace and fails to link any mailbox (`accounts.workspace_id` is
required). Never roll the binary back alone past 0008: going back is the backup taken before the
upgrade, restored with the binary of its time. Should it have happened, the next start of a
current binary gives those people their personal workspace (see
[The workspace source](#the-workspace-source)) and says so in the log.

## Rebuilding a table safely

`accounts.email` is `UNIQUE` inline (0001), and SQLite drops a constraint only by rebuilding the
table. Every migration so far was `ALTER`-only for this reason: migrations run inside a transaction
on a connection with `foreign_keys` on, where `DROP TABLE accounts` is an implicit `DELETE` that
cascades into credentials, folders and messages. `PRAGMA foreign_keys` cannot be changed inside a
transaction (it is silently ignored there), so the runner gains a second procedure, SQLite's own
for "other kinds of table schema changes", for a migration whose first line is
`-- migration: rebuild`:

1. Take the writer's one connection for the whole procedure (`Writer().Conn`), so the pragmas and
   the transaction run on the same connection. At `Open` and in `mailserver migrate` nothing else
   is connected: the reader pool opens after migrating, and the lock keeps a daemon away.
2. Outside any transaction, `PRAGMA foreign_keys = OFF`, and read it back: anything but `0` stops
   here. `PRAGMA legacy_alter_table` is read back too and must be `0`.
3. `BEGIN IMMEDIATE`.
4. Count the rows of every table in `sqlite_schema` (FTS5's shadow tables included).
5. Run the migration: create the new table, copy with every column named, drop the old one, rename
   the new one into its place, recreate indexes and triggers (and views, were there any; the runner
   refuses a rebuild while a view names the table).
6. `PRAGMA foreign_key_check`: any row fails the migration.
7. Count again: every table that existed before must hold exactly as many rows. A rebuild adds
   tables and rows; it never loses one.
8. `PRAGMA user_version = N`, in the same transaction.
9. `COMMIT`; on any failure before it, `ROLLBACK`, and the database is as it was.
10. Whatever happened, `PRAGMA foreign_keys = ON`, read back, before the connection goes back to
    the pool. A process that dies midway loses nothing: the transaction was never committed, and a
    new connection gets `foreign_keys(1)` from the DSN.

Never the other shape, renaming the old table aside first: with `legacy_alter_table` off (the
default), `ALTER TABLE accounts RENAME TO accounts_old` rewrites every child's foreign key to point
at `accounts_old`, and dropping that afterwards orphans them all.

`mailserver migrate --dry-run` says which pending migration rebuilds a table. A backup before
upgrading to a release with one is the operator's ordinary precaution (`mailserver backup`, or a
copy of the data directory with the daemon stopped).

Guards against a future mistake: a test reads every migration and fails when one without the
rebuild line drops a table that another table references (the 0002 drop of `oauth_pending`, a child
with no children of its own, is the one exception, named in the test).

## Tests

Test names state the guarantee. At least:

- Visibility: `TestAMemberWithoutAGrantCannotSeeTheMailbox` over REST, MCP, the event stream, the
  long poll and storage (one test per transport, the same fixture);
  `TestAnOwnerOrAdminManagesEveryTeamMailboxAndReadsNone` (the service, every path) with
  `TestAnOwnerOrAdminSeesATeamMailboxsCardAndReadsNoneOfItOverREST` and
  `TestAnOwnerOrAdminReadsNoTeamMailboxThroughTheirKey` (MCP);
  `TestOwnersAndAdminsSeeEveryTeamMailboxsCardAndReadNone` (the SQL rule);
  `TestOnlyAnOwnerOrAdminWhoReadsPassesRead`; `TestAnAdminTurnsOnSendForAnyoneAndActForAReader`;
  `TestManageIsStoredForMembersOnlyAndOwnersAndAdminsManageByRole`;
  `TestAGrantOnlyGoesToAnActiveMemberOfTheMailboxesWorkspace`;
  `TestAnotherWorkspacesMailboxIsNotFoundNeverForbidden`;
  `TestOnlyOwnersAndAdminsSeeMembersAndTheDirectory`.
- Links and team mailboxes: `TestTheSameAddressInTwoWorkspacesIsTwoIndependentMailboxes`;
  `TestAnAddressIsLinkedOnceInAWorkspace`;
  `TestATeamMailboxNamesNoPersonAndItsLinkerHoldsReadActAndSend`;
  `TestAnOwnerLinksAnOAuthTeamMailboxCompletesItAndOnlyTheyCanComplete`;
  `TestRemovingAMailboxNeedsItsIdRepeated`.
- Consents: `TestATeamMailboxSyncsUnderItsWorkspacesConsent`;
  `TestATeamMailboxsConsentIsGivenOnlyToTheCurrentSyncTextAndOutlivesItsChange`;
  `TestSwitchingATeamMailboxsSyncOffDeletesItsIndexForEveryone`;
  `TestWithdrawingAPersonsSyncNeverTouchesATeamMailbox`;
  `TestATeamMailboxWithNoReaderStopsSyncing`;
  `TestAMigratedTeamConsentStopsWhenItsLinkerWithdrawsUntilTheTeamConfirmsIt` (withdrawing,
  disabled, deleted: refused without force while someone else reads it);
  `TestConfirmingAMigratedTeamConsentDetachesItFromTheLinker`;
  `TestADisabledLinkersTeamMailboxIsKeptStoppedWithItsIndex`;
  `TestDisablingSomeoneAlreadyDisabledKeepsTheIndexKeptForThem`;
  `TestATeamMailboxNobodyCanReadCannotBeTurnedOnAndStaysBound`;
  `TestMigrationElevenKeepsADisabledLinkersOwnMailboxBoundAndListsItAsReadByNobody`.
- Members and protections: `TestTheLastOwnerCannotLeaveOrBeDemoted`;
  `TestTheLastReaderCannotLoseReadBeDisabledOrRemoved`; `TestConcurrentRemovalsAlwaysKeepAReader`;
  `TestAMemberOrAdminCannotLeave`;
  `TestARoleOrStatusChangeExpiresTheInvitesThePersonCreated`;
  `TestRemovingAMemberRemovesTheirGrantsAndStopsTheirEventStream`;
  `TestDisablingAMemberRemovesTheirGrantsAndEnablingDoesNotRestoreThem`;
  `TestAnAdminCannotChangeAnotherAdmin`.
- Closure: `TestClosingAPersonNeverStopsOrRemovesATeamMailbox`;
  `TestDisablingOrDeletingTheLastReaderOnTheInstanceIsRefusedWithoutForce`, over REST
  (`TestDisablingOrDeletingTheLastReaderOverRESTNeedsForce`) and `--bootstrap`
  (`TestClosingTheLastReaderOfATeamMailboxWithBootstrapNeedsForce`), and its concurrent variant
  `TestConcurrentClosureAndRevocationAlwaysKeepAReader`;
  `TestClosingTheLastReaderOfATeamThatOutlivesThemIsRefusedWithoutForce` (its other members
  disabled), over REST and `--bootstrap`, and its concurrent variant
  `TestConcurrentlyDisablingTheOtherMembersNeverLetsTheLastReaderGoWithoutForce`;
  `TestClosingTheOnlyMemberOfATeamIsNotRefusedForItsMailbox`;
  `TestAForcedClosureLeavesAMailboxNobodyCanReadMarked`;
  `TestDeletingTheOnlyMemberOfATeamStopsItsMailboxesWorkersAndAttempts`;
  `TestDeletingAPersonLeavesNoRowThatNamesThemOrTheirMailboxes` (every form of attribution).
- Keys: `TestAnInstanceKeyReachesOnlyOperatorMailboxes` (REST and MCP);
  `TestAPersonKeyLosesAMailboxWhenItsPersonLosesTheGrant`;
  `TestAKeyMadeForALostMailboxIsRevoked`;
  `TestAHeldPrincipalNeverReachesAMailboxItsKeyLostWhenItsPersonLostRead`;
  `TestAHeldSessionNeverReachesAMailboxItsKeyLostEvenOnceReadComesBack` (MCP);
  `TestAStreamWhoseKeyLostAMailboxEndsWithAConflictAndReconnects` (SSE).
- Events: `TestAStreamFollowsAccessAsItChanges`;
  `TestAStreamOfEverythingOutlivesTheLossOfEveryMailboxItRead`;
  `TestASendFinishedReachesOnlyItsSender`.
- Subscriptions and storage: `TestSubscribingToAnInboxNeedsReadAccessToIt`;
  `TestASubscriptionToAnInboxTheKeyCanNoLongerReadIsDropped`;
  `TestStorageCountsOnlyTheMailboxesTheCallerMayRead`.
- Consent attempts: `TestAConsentFinishingAfterItsStarterStoppedManagingTheMailboxStoresNoGrant`;
  `TestLosingManageOfAMailboxEndsTheConsentAttemptsStartedOnIt`.
- Use: `TestActingNeedsTheActorsConsentAndTheActFlag`;
  `TestSendingNeedsTheSendersConsentAndTheSendFlag`;
  `TestAMessageFromASharedMailboxGoesOutUnderTheSendersName`;
  `TestASendKeyIsNeverReplayedToAnotherPerson`;
  `TestALinkRefusedInsideItsTransactionKeepsTheRefusalsCode`.
- People: `TestTheFirstSignUpIsNoOwnerUnlessInvitedAsOne`;
  `TestATeamInviteMakesANewPersonWithTheirPersonalWorkspace`;
  `TestATeamInviteSignsUpANewPersonOnlyWhenTheOperatorOrAnInstanceOwnerMadeIt`;
  `TestAMemberCannotMintAnAccountThroughATeamInvite`;
  `TestAnotherPersonsUsedTeamInviteOutlivesTheTeamDeletedWithItsLastMember`;
  `TestATeamDeletedByAnyWayTakesTheInvitesStillWaitingToJoinIt`;
  `TestAnExistingPersonJoinsByAcceptingATeamInvite`;
  `TestARefusedAcceptanceDoesNotSpendTheInvite`;
  `TestARemovedMemberCannotRejoinWithALeftoverInvite`;
  `TestLeavingATeamDeletesItsInvitesStillWaitingForThePerson`;
  `TestSigningUpSpendsTheOtherInvitesWaitingForTheAddress`.
- Source: `TestThePlatformSourceRefusesLocalChanges`;
  `TestAPersonWithoutAPersonalWorkspaceGetsOneAtStart`.
- Migration, in `internal/store`: `TestMigrationEightKeepsEveryRowAndGivesEachPersonTheirMailboxes`
  runs every migration on a database with rows in **every** table, in the smallest shape (one
  owner with two mailboxes, no keys) and in a larger self-hosted shape (several people, one
  disabled; owned and unowned mailboxes; instance keys unrestricted, restricted to an unowned and
  to an owned mailbox; person keys, restricted and not; invites used and unused; sessions; consent
  attempts on owned and unowned mailboxes; sends with and without a person; events; webhooks and
  deliveries; drafts), dumps every pre-existing table before and after ordered by `rowid`, and
  compares them value by value (for `accounts`, every column but the new one); then checks the FTS
  index (`integrity-check` and the same `MATCH` answers),
  `PRAGMA integrity_check` and `foreign_key_check`, and that `foreign_keys` is back on: removing a
  mailbox after the rebuild still takes its credentials, folders and messages with it.
  `TestARebuildThatBreaksAForeignKeyChangesNothing` and
  `TestARebuildThatLosesARowChangesNothing` feed the runner a broken rebuild and find the database,
  and `foreign_keys`, as they were. `TestOnlyARebuildMayDropAReferencedTable` is the guard above.
  `TestMigrationNineTakesTheMailboxesTheirPersonCannotReadOutOfPersonKeys` and
  `TestMigrationNineGivesAServerNobodySignedUpToItsFirstOwner` cover 0009, and
  `TestADatabaseANewerBinaryMigratedIsRefused` the guard of [Going back](#going-back). 0011:
  `TestMigrationElevenMakesTheLinkersConsentTheWorkspacesAndLeavesPersonalMailboxesAlone` (both
  shapes, every table compared value by value), `TestMigrationElevenLeavesATeamMailboxOfADisabledLinkerStoppedWithItsIndex`,
  `TestMigrationElevenRefusesAPersonalMailboxThatNamesAnotherPerson`,
  `TestMigrationElevenTakesStoredManageFromOwnersAndAdminsOnly`,
  `TestMigrationElevenExpiresTheInvitesTheirCreatorCouldNotMakeNow` and
  `TestMigrationElevenRefusesWhatItsSchemaForbids`.

## Conflicts and resolutions

1. **The linker's consent outlived the linker's membership.** Phase 2 kept a team mailbox syncing
   under its linker's consent, protected the linker while the link stood and let an owner or an
   admin take the link over. *Resolution (the owner, 2026-10-06):* the mailbox is the team's, as a
   number is in Wappie: it syncs under the team's consent, nobody is protected for having linked it,
   and the take-over is gone. The last-reader rule keeps someone able to read it.
2. **"Owners and admins grant" against "owners and admins read nothing by their role".** If an
   owner or an admin could grant `read` to anyone, themselves included, "no automatic read" would be
   one click. Wappie's barrier is cryptographic (a sealed grant from a holder of the device key);
   Mailie holds plaintext. *Resolution (the owner, 2026-10-06):* `read` passes only from an owner
   or an admin who reads the mailbox now — not from a member who reads it, as in Wappie only owners
   and admins add grants; `act` and `send` are any owner's or admin's to give, to anyone,
   themselves included (`act` to someone who reads), since neither reads mail and each still needs
   the actor's own consent; owners and admins manage by their role, and `manage` is stored for
   members only.
3. **Instance keys over REST see every mailbox today**, a person's index included (`visibility`
   gives a non-tool instance key `All`). The plan restricts them to the operator workspace. Intended,
   and a change for self-hosted operators: `account list` shows only operator mailboxes, and an
   instance key restricted to a person's mailbox reaches nothing after the migration.
4. **An instance owner loses today's powers over the mailboxes nobody owns** (seeing them, acting,
   sending, from the console). Intended by the plan; the command line keeps them.
5. **Closing accounts.** The code keeps `user disable|delete` for the operator's key on purpose ("a
   request that arrives by email and has to be verified first"); the plan gives instance owners
   disable and delete. *Resolution:* follow the plan, with every protection, never on oneself, and
   keep the operator's routes and commands as they are.
6. **The quick start breaks without the first-sign-up rule.** The README, `CLAUDE.md` and
   `self-hosting.md` run `user invite --bootstrap --email …` without `--role`, which today ends as an
   owner only thanks to the rule the plan removes. *Resolution:* the invite decides, at creation,
   never the sign-up: `--bootstrap` without `--role` makes an `owner` invite while the instance has
   no active owner and no live owner invite, `member` otherwise; the REST default stays `member`.
   The docs say `--role owner` explicitly. An invite an earlier release made, still waiting when the
   server is upgraded with nobody on it, is the owner's it was meant to be:
   [0009](#migration-0009) makes the oldest one an owner invite. And the daemon warns at start when
   people exist and nobody administers the server.
7. **`access grant` from the command line.** The operator holds no flag on anyone's mailbox and
   reads none, so it cannot grant `read`; letting it grant `act` or `send` would hand the operator
   people's mail by proxy, which the plan's own instance-key rule forbids. *Resolution:* the command
   grants `manage` only, to members, and revokes anything.
8. **Withdrawing sync consent reached team mailboxes.** Phase 2's consequence of syncing under the
   linker's consent, warned about in the console. *Resolution (2026-10-06):* reversed: a person's
   withdrawal deletes their personal mailboxes' index only. A team mailbox's index goes when an owner
   or an admin turns the team's sync off, for everyone. The one exception is transitional: a
   consent migration 0011 copied from a linker stays bound to them until the team confirms it,
   because the text they agreed to promised exactly that ([conflict 15](#conflicts-and-resolutions)).
9. **Team invites bring new people onto a self-hosted server**, which today only an instance owner
   can. Any person may create a team and invite into it, so if every team invite could sign a new
   person up, any member could create an account for any address without one, with a password of
   their choosing, and spend the instance invites waiting for that address on the way. *Resolution:*
   a team invite signs a new person up only when the operator or an instance owner (still active
   when it is redeemed) made it, which keeps the plan's reading for those who may bring people onto
   the server anyway. Anyone else's team invite adds an existing account to the team, accepted
   signed in. Refusing to create such an invite for an address without an account would tell any
   team admin which addresses have accounts here, so it is created whatever the address, and the
   sign-up refuses it the same way whether or not the address has an account. A new person arrives
   as an instance `member` with only their personal workspace and that team.
10. **One send key space for several senders.** `sends` is keyed by `(account_id, idempotency_key)`,
    and a derived key is `<compose hash>/<minute>`, so two people sending the same text from one
    shared mailbox within a minute would collapse into one send, and one could read the other's
    record. *Resolution:* no schema change: a reserved key whose record belongs to another person is
    `409` (as a key reused for another message is), never a replay, and `GET /v1/sends/{key}`
    answers only the caller's own sends (the operator's for instance keys). The journal's
    `send.finished`, which names a send's key and outcome, follows the same rule rather than the
    mailbox's: it carries its sender (`user_id`, absent for an instance key and once that person is
    deleted) and reaches only who may read the record (see [Events](#events)).
11. **"Seeing a mailbox requires a grant" against administering it.** Owners and admins must see
    which mailboxes exist in their team to grant, revoke, re-authorize and remove. *Resolution
    (2026-10-06):* they see each team mailbox's card by their role — address, state, sync counters,
    nothing it holds — and the access directory (`GET /v1/workspaces/{id}/access`) shows them the
    addresses and grants, never the index, as Wappie's device directory does.
12. **The column name.** Phase 2 kept `owner_user_id` and read it as "linked by". *Resolution
    (2026-10-06):* it means the person whose mailbox it is again — set exactly for a personal
    workspace's mailbox, as the eligibility SQL always read it — and who linked a mailbox is a
    column of its own, `linked_by`, attribution only. `oauth_pending.owner_user_id` is who started a
    consent flow, as before.
13. **`act` without `read`** is a grant that can do nothing (an action names messages the caller
    reads). *Resolution:* `CHECK (act = 0 OR read = 1)`. `send` without `read` stays meaningful (a
    send-only member) and allowed.
14. **The consent texts.** The open console's sync text said, at revision `2026-10-open-sync-2`,
    that a mailbox syncs under the agreement of whoever connected it and that turning sync off
    deletes the index of every mailbox they connected, team ones included, unless another member
    took the link over; the actions text, at `2026-10-open-actions`, that the server changes "your
    mailboxes" only when you or your tool ask. Neither is true of a team's mailbox now.
    *Resolution (the owner, 2026-10-06):* new revisions, `2026-10-open-sync-3` and
    `2026-10-open-actions-2` (`web/src/open/SyncText.vue`, `ActionsText.vue`, `versions.ts`, and
    `config.DefaultSyncConsentVersion` / `DefaultActionsConsentVersion`). The sync text says that a
    personal mailbox syncs under the person's agreement and turning it off deletes only theirs; a
    team mailbox under the team's agreement, given by an owner or an admin to this same text, which
    any of them turns off for everyone; who reads a team mailbox's index (the members given read,
    which only an owner or an admin who has it gives, and the tools they give a key); that owners
    and admins read nothing by their role, and see a team mailbox's address, state and sync
    counters; that closing an account does not remove the mailboxes of a team others are in, while
    a team whose only member closes their account goes with it; that a team mailbox still syncing
    under its linker's own agreement since the upgrade stops with it until the team agrees
    ([conflict 15](#conflicts-and-resolutions)); and that whoever runs the server can read its
    database. The actions text says the server changes a mailbox only when
    someone allowed to act on it asks, under their own agreement. As for any revision, everyone is
    asked again; sync keeps running for whoever agreed before, and actions are refused until they
    agree again. Linking into a team with the team's consent, or turning it on, needs `-3`. The API
    key text keeps its revision until step 3.
15. **A migrated consent and the promise it was given under.** Migration 0011 turns each team
    mailbox's linker's own consent into the team's, which would otherwise be a consent its person
    can no longer withdraw, recorded under a revision whose text promised the opposite.
    *Resolution:* the copy stays bound to the linker (`sync_enabled_via = 'migration'`) until an
    owner or an admin confirms the team's consent at the current revision; until then the linker's
    withdrawal, disable or deletion stops the mailbox and deletes its index in the same
    transaction. Because that deletes the index of whoever else reads it, closing such a linker on
    the instance is refused without `force` while someone else does, as Phase 2 refused closing a
    linker whose mailbox others read. A linker found disabled by the migration gets nothing copied:
    the mailbox stays stopped with its index, as it was, still bound to them (deleting them
    deletes it; disabling them again changes nothing), and the daemon lists it at start for the
    team to turn on, off or remove — or, when only that linker read it, as one nobody can read,
    which is never turned on again. The open sync text says so, as its one exception.
16. **Closing the last reader.** Closing an account must never be blockable for good, and a team
    mailbox read by nobody can never be read again (read passes only from a reader). *Resolution:*
    `user disable|delete` refuse the last reader of a team mailbox, in a team that outlives them
    (any other member remains, whatever their status: the same test as whether the team goes with
    them), unless `force`, as for the last owner; forced, the mailbox keeps its consent, syncs
    nothing more (the eligibility rule needs a reader), cannot be turned on, and is marked
    `no_reader` for the team's owners and admins, who remove it, or remove it and link it again.
17. **Members do not leave by themselves.** Phase 2 let any member leave. Wappie has no leave route,
    and an admin removes members only. *Resolution (the owner, 2026-10-06):* only an owner leaves,
    while another owner remains; a member or an admin asks an owner (or, for a member, an admin) to
    remove them.
18. **Keys of the workspace.** The owner decided that API keys belong to their workspace, made by
    its owners and admins, may send, and hold their own mailboxes. That is step 3, not built yet;
    until it is, a person's key reaches what its person can, as phase 2 made it, and a member of a
    self-hosted team still creates keys for the mailboxes they read.

## Not in this phase

- API keys of the workspace (step 3 of the plan of 2026-10-06).
- Deleting a team workspace but with its last member; moving a mailbox between workspaces (link it
  again instead).
- The platform source beyond its stub, a later step; with it, an edition's own screens for teams
  (the open edition's Members section is for the local source only).
- Selecting a workspace per MCP session; an MCP tool for workspaces.
- Whole-workspace usage for owners.

## What changes where

| Where | What |
|---|---|
| `internal/store/migrations/0008_workspaces.sql`, `0009_workspaces_after.sql`, `internal/store/migrate.go` | the schema above; the rebuild procedure and its guards; the refusal of a newer schema |
| `internal/workspace` (new) | workspaces, members and grants repository; `Source`, local and platform stub; protections inside transactions |
| `internal/account` | `Visibility` becomes the workspace and grant clause, one for listing and fetching; linking with the workspace and the linker's grant in one transaction; duplicates per workspace |
| `internal/auth` | invites with ids and teams; sign-up without the first-owner rule, creating through the source's hook; deletion that removes the personal workspace and checks team protections |
| `internal/service` | `authorizeAccount` takes the flag it needs; `mayAct`, `maySend`, `fromName`, `sendOf` per caller; the event gate's epoch and `access` events; storage; new `workspaces.go`, `members.go`, `access.go`; invites; closure |
| `internal/api`, `internal/mcp` | the routes above, as thin adapters; `?workspace=`; the SSE handler renders the stream's end |
| `internal/app` | `Options.WorkspaceSource` |
| `cmd/mailserver` | `workspace`, `member`, `access`; `user invite --workspace`; the bootstrap default; the startup hint |
| `.golangci.yml` | `internal/workspace` joins the packages transports may not import |
| docs | `console.md` (ownership becomes workspaces, routes, storage, events, the current workspace), `architecture.md` and `CLAUDE.md` (the ownership rule), `mcp.md` (instance keys), `self-hosting.md` and the README (the first owner) |
| `web/` | the types and the contract spec; the workspace switcher, every list narrowed with `?workspace=`, the `access` events, each mailbox's grant and access panel, accepting a team invitation signed in, and the open edition's Members section ([`console.md`](console.md), "Workspaces in the console") |
| 0011 (2026-10-06, step 1) | `internal/store/migrations/0011_team_mailboxes.sql` and `migrate_eleven_test.go`; `store/eligibility.go` (a team mailbox's consent and a reader), `store/consent.go` (`SetMailboxSync`, `StopBoundTx`, a person's withdrawal personal only); `workspace/reader.go` (the last reader, `HasReaderTx`), `grants.go` (effective manage, the directory), `members.go` (invites expire, no linker), `people.go` (blocks: last owner, last reader of a team that outlives them, a bound consent someone else reads; attribution blanked); `account/store.go` (the person of a personal mailbox, `linked_by`, the team's consent at the link, `Visibility` by role) and `registry.go` (`LinkerID`, `RemoveChecked`, a sole member's teams removed with them); `auth/closure.go` (a disable changes nothing for someone already disabled); the service's grant rules, `RemoveAccount` with `confirm`, `SetMailboxSync` (never on for a mailbox nobody reads), closure (`team_syncs_stopped`); the API's routes and fixtures; MCP's `list_accounts` text (read access marked); the command line; the open texts `-3` and `-2` |
