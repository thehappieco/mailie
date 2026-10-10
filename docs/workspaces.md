# Workspaces

**Status:** phase 2 (approved by the owner on 2026-10-05) is implemented on the server and in the
console: migrations 0008 and 0009 and the runner's rebuild procedure, `internal/workspace`, the
service's authorization on workspaces and grants for every path (REST, MCP, the event stream and
the long poll), the routes, the command line and the contract fixtures. **The workspace model of
2026-10-06** — Mailie follows Wappie's: a team's mailbox belongs to the team, its owners and admins
manage it by their role, the last reader is protected, and every API key belongs to a workspace —
was approved by the owner on 2026-10-06. Its first step (migration 0011, team mailboxes) and its
third (**migration 0012, the workspace's API keys**: `key_access`, the routes, sending by key, the
command line, the contract fixtures and the open key terms' revision) are implemented on the
server; the console follows them. Phase 3 of the key scheme adds **mailbox keys and sealed
grants** (step 4, migration 0014, [`key-scheme.md`](key-scheme.md) sections 8, 9 and 12.11 to
12.15): on a mailbox that has a key, reading takes the flag and the person's grant at its current
epoch ([Mailbox keys and sealed grants](#mailbox-keys-and-sealed-grants)); the server's half is
implemented, the console's sealing follows. Not yet: the platform's workspace source, of which only
the stub is here. [`console.md`](console.md) describes the routes as the console reads them, and its
screens. Where this document had to choose between readings of the plan, or found a rule that
conflicts with the code or with another rule, it says so in
[Conflicts and resolutions](#conflicts-and-resolutions).

## In short

- Every mailbox belongs to a **workspace**: a person's **personal** workspace, a **team**
  workspace, or the one **operator** workspace, which has no members and is what instance keys and
  the command line act on. A personal mailbox is its person's; a team's mailbox is the **team's**,
  whoever linked it; who linked it is attribution only.
- Using a mailbox takes **active membership** in its workspace and a **grant** on it: `read`,
  `act` and `send` each open one use. On a mailbox that has a **key**, `read` also takes the
  person's **sealed grant** at its current epoch, the mailbox's private key sealed to them by the
  browser of someone who reads it; a member who holds the flag without one **waits for the key**,
  sees the card and reads nothing ([Mailbox keys](#mailbox-keys-and-sealed-grants)). **Owners and admins manage every mailbox of their workspace
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
- **API keys belong to their workspace**, as Wappie's: only its owners and admins, signed in,
  create, list and revoke them (in a personal workspace, its person). A key acts as nobody and
  reaches exactly the mailboxes it holds `read`, `act` or `send` on (`key_access`): `read` only from
  an owner or an admin who reads the mailbox then, `act` and `send` from any of them. What a key
  holds stands on its own, whoever gave it; keys never count as readers and never pass `read`. A key
  may send, with the `send` scope and flag and `confirm: true`, unless the server says keys may not
  (`MAIL_KEYS_MAY_SEND=false`). It stops when its creator leaves the workspace or is disabled or
  deleted on the instance. See [API keys](#api-keys).
- The same address may be linked in several workspaces; each link is an independent mailbox with
  its own credentials, worker and index. Within one workspace an address is linked at most once.
- `users.role` (`owner`, `member`) is purely the **instance role** of a self-hosted server. It
  gives no mailbox visibility. The first owner comes only from `user invite --bootstrap`.
- Migration 0008 created the workspaces and rebuilt `accounts`; 0009 set right what 0008 could not
  change; **0011** makes team mailboxes their team's (see [Migration 0011](#migration-0011));
  **0012** moves every key into its workspace (see [Migration 0012](#migration-0012)).

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
- **`read`** opens its index: folders, messages, events, storage. On a mailbox that has a key, the
  flag opens it only beside the person's grant at its current epoch (the one rule below).

Without any grant or role the mailbox does not exist for the caller (`404`). With the card but
without the flag an operation needs, it is `403 not_authorized`.

### Mailbox keys and sealed grants

Phase 3 of the key scheme, step 4 ([`key-scheme.md`](key-scheme.md) sections 8, 9 and 12.11 to
12.15; migration 0014). A mailbox's **key pair** is made by a browser, never by the server: an
X25519 key pair, a **namespace** (a lowercase UUIDv4, the same at every epoch and used by no other
mailbox) and an **epoch** (1 first, each new key the one after). The server stores the public half
and the namespace, and **grants**: the private key sealed to one person's account public key, 88
bytes it checks only for their shape and cannot open. The private key never reaches it.

```sql
mailbox_keys(account_id, epoch, public_key, namespace, created_by, created_at)
  PRIMARY KEY (account_id, epoch)                      -- written once; epoch 1, then max + 1
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
mailbox_grants(account_id, workspace_id, user_id, epoch, grant, granted_by, created_at)
  PRIMARY KEY (account_id, user_id, epoch)             -- written once
  FOREIGN KEY (account_id, epoch) REFERENCES mailbox_keys(account_id, epoch) ON DELETE CASCADE
  FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
  FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE
```

The current epoch is the highest row of `mailbox_keys`; older rows stay, and only their grants go.
Triggers refuse every update but the blanking of `created_by` and `granted_by` when that person is
deleted, an epoch skipped or written twice, a second namespace for a mailbox or one another uses,
and a key for an operator mailbox, which has none in phase 3 (nobody holds it). A grant is 88 bytes
starting with `ML`, for a member of the mailbox's own workspace only.

**Who reads** (section 12.13), one rule in one place (`store.ReaderSQL`, SQL fragments every
reader of access builds on: `account.Visibility`, `workspace.Access`, the last reader, the
directory's `readers`, sync eligibility and the team's consent): the `read` flag, as an active
member active on the instance, and, **on a mailbox that has a key, a grant at its current epoch**.
On a mailbox without one, the flag alone, as before. `act` counts only where its holder reads. A
member who holds the flag without the grant **waits for the key**: their card stays, `access.read`
is false and `access.waiting_key` true. Keys never count, and roles never read.

**Writing keys and grants**, each by a person signed in, with a **fresh step-up** (a sign-in or a
step-up within ten minutes, section 11), checked before anything slow and again in the transaction
that writes:

| What | Who | Route |
|---|---|---|
| a mailbox's first key, with the linker's grant (section 12.11) | the person who links it; an instance key's link carries none | `POST /v1/accounts` with `{public_key, namespace, grant}` |
| `read` with the recipient's grant (section 12.13) | an owner or an admin who reads the mailbox now; the step-up when the recipient is someone else | `PUT /v1/accounts/{id}/access/{user}` with `grant` |
| the key, to a member who holds the flag and has no grant at the current epoch (section 12.13) | **anyone who reads the mailbox now**: it gives nobody `read` who was not given it | `PUT /v1/accounts/{id}/grants/{user}` |
| the first key of a mailbox without one, with a grant for everyone who holds `read` and has an account key, exactly (section 12.14) | a person who reads it now, by the flag, and has an account key | `POST /v1/accounts/{id}/mailbox-key` |
| a personal mailbox's next key, deleting every grant of an older epoch (section 12.12) | its person only; a team mailbox never gets one | `PUT /v1/accounts/{id}/mailbox-key` |

To a member without an account key (a person who signs in elsewhere and has none yet), and on a
mailbox without a key, `read` is the flag alone, with no step-up and no grant, as before; on a
keyed mailbox they wait for the key, which a reader supplies once they enrol. A grant is refused
with anything but the `read` it gives.

**Taking `read` takes the grants** (section 12.13), every epoch's, in the transaction that takes
the flag: a revoke, a `PUT` without `read`, a membership disabled or removed, the person disabled
on the instance (their flags stay, and count for nothing while they are off), their deletion, and
the **reset** of their account key, which keeps their flags: on every mailbox that has a key they
then wait for it, from a reader for a team's, by a new key of their own for a personal one, which
keeps syncing meanwhile under their consent.

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
| A workspace key's actions and sends | **nobody's**: the key terms its creator agreed to, recorded on the key (`terms_version`), say what it may do | the key's own `act` or `send` flag on the mailbox, its scope, and for sending `MAIL_KEYS_MAY_SEND`; with `MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT` every workspace key acts only while its creator allows actions; a person's key carried over by 0012 always does, and never sends |

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
- **An index that stays while its mailbox does not sync is read, never acted on**: kept stopped, or
  read by nobody but a key that holds it. No row of it can change, so an action there would change
  the server and leave the index contradicting it; it is refused with `409` before the server is
  touched, when it is accepted and again on the connection before each command. Sending writes
  nothing to the index and still works, but a reply leaves the message it answers without
  `\Answered`.
- The From name is the **sender's** profile name (`fromName` reads the caller), so an account's
  `send.from_name` is per caller. A key, instance or workspace, sends under the address alone.

## Who may do what

### Using a mailbox

| Operation | A person (session) | A workspace key | An instance key |
|---|---|---|---|
| list it, read its card | any grant; or owner or admin of its workspace, by the role | it holds anything on it | operator mailbox |
| folders, search, read, originals, attachments, events, storage | `read` | `read` | operator mailbox, `read` scope |
| ask for a sync pass | `read`, `write` scope | `read`, `write` scope | operator mailbox, `write` scope |
| switch sync on or off | a team mailbox: its owners and admins, signed in (the team's consent); a personal one: never (its person's own consent decides) | never | operator mailbox, unrestricted admin key |
| act | `act`, actions consent, the mailbox syncing | `act`, `write` scope (under its key terms; its creator's actions consent only as the Consents table says), the mailbox syncing | operator mailbox, `write` scope, the mailbox syncing |
| send, read own send records | `send`, send consent | `send`, `send` scope, `MAIL_KEYS_MAY_SEND` (under its key terms; no person's consent) | operator mailbox, `send` scope |
| re-authorize | manage (owner or admin, or a member holding `manage`) | never | operator mailbox, `admin` scope |
| remove (repeating its id) | a team's: its owners and admins; a personal one: its person | never | operator mailbox, `admin` scope |
| restrict a new instance key to it | | | operator mailbox |

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
| create a key; list the workspace's keys; revoke any of them | yes, signed in | yes, signed in | no | lists and revokes every key (`/v1/apikeys`, with `MAIL_ADMIN_API=true`) |
| give a key `read` on a mailbox | while they read it themselves, now | likewise | no | no |
| give a key `act` (where it reads) or `send`; take anything out of a key | yes | yes | no | no |
| list a key's sends | yes | yes | no | no |

Any person may create a team, with a session, and becomes its owner; the operator creates one for
a named existing person. A personal workspace's person is its owner, and holds every power above
in it, with `read`, `act` and `send` on each of its mailboxes; personal workspaces have no invites,
other members or renaming. The operator workspace has no members, grants or invites. Every row is
subject to the [protections](#protections). A workspace key never administers anything, keys
included: these routes take a session or the operator's key.

### Who may change a grant

1. **Who.** An active owner or admin of the mailbox's workspace, re-read inside the transaction; or
   the operator, who grants `manage` to members and revokes. A member who holds a grant on it sees
   the mailbox and is `403`; anyone else is `404`.
2. **What.** `read` that the person did not hold passes only from an owner or an admin who **reads
   the mailbox themselves, now** (the flag, and on a mailbox that has a key their grant at its
   current epoch), checked in the same transaction: this stands in for Wappie's sealed grant,
   which only a holder of the device key can pass on, and it is what makes "owners and admins read
   nothing by their role" true rather than one click away. On a mailbox that has a key it comes
   with the recipient's grant, which the giver's browser seals from its own, after a fresh step-up
   when the recipient is someone else; to a member without an account key it is the flag alone
   ([Mailbox keys](#mailbox-keys-and-sealed-grants)). `act` needs `read` after
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
5. The grant records `granted_by`: `usr_…` for a person signed in (the linker's own grant
   included), `key:<prefix>` for the operator, whose command line goes through the daemon with an
   instance admin key, or `migration` for a grant migration 0008 made; empty once the person who
   granted it is deleted. Nothing writes `cli`, which 0008's column comment still lists.

### Instance administration

| | Instance `owner` session | Instance `member` session | Unrestricted instance admin key |
|---|---|---|---|
| instance invite (`POST /v1/users/invites`) | yes | no | yes |
| disable a person (`POST /v1/users/disable`) | yes, not themselves | no | yes |
| delete a person (`POST /v1/users/delete`) | yes, not themselves | no | yes |
| `database_bytes` in storage | yes | no | no |
| every key, instance and workspace keys (`/v1/apikeys`, with `MAIL_ADMIN_API=true`): list, revoke; issue instance keys | no | no | yes |

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
holding `read`, and on a mailbox that has a key a grant at its current epoch; keys, disabled
members, a member waiting for the key and a role on its own never count ("ownership is not a
recovery path"), and taking a waiting member's flag leaves the readers as they were. The reset of a
person's account key (`mailserver user password --bootstrap`) deletes their grants and keeps their
flags, so it refuses, without `--force`, only where they are the last reader of a team mailbox
**that has a key**; on one without, they keep reading by the flag. After a forced reset, a team
mailbox left with no reader stops syncing. Removing the mailbox is not guarded, nor is a role change. The database has one
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
| a mailbox the caller neither holds a grant on nor manages by their role, another workspace's mailbox, a workspace the caller is not an active member of; a grant's recipient who is not an active member of the mailbox's workspace | `404 not_found`, never `403` |
| a mailbox the caller sees, without the flag the operation needs (a member waiting for the key reads nothing); a role that may not do this (a member listing members, leaving, granting, creating a key); a key on an administration, key or mailbox-key route; a key's send where keys may not send; a giver of `read` or of the key, or a first key's writer, who does not read the mailbox now; a step-up more than ten minutes old; a team mailbox's new key | `403 not_authorized` |
| a protection; a platform-sourced workspace changed locally; a missing actions or send consent; a grant at another epoch than the mailbox key's current one, or a new key at another than the next; a grant that already exists; a first key for a mailbox that has one, or whose grants are not exactly its readers' with an account key; a namespace another mailbox uses; a grant for a person without an account key, or the key for one who holds no `read`; a person without an account key linking a mailbox | `409 conflict` |
| a personal or the operator workspace as the target of a member, grant or invite operation; an `act` without `read`; `manage` for an owner or an admin; a removal without its id repeated; a consent to another revision than the current one; a key's flag its scope does not allow; `POST /v1/me/apikeys`; a public key, namespace, grant or epoch outside its shape, or a grant at odds with the epoch its request names; a person's link without its key, or an instance key's with one; `read` on a mailbox that has a key, to a person with an account key, without their grant; a grant with a change that gives no `read` | `400 bad_request` |

## API keys

Every API key belongs to one workspace, as Wappie's do (migration 0012):

- **An operator key** (`workspace_id = 'wsp_operator'`, formerly "instance key") is what the
  command line holds. It reaches the operator workspace's mailboxes, over REST and MCP alike
  ([conflict 3](#conflicts-and-resolutions)), with every flag its scope (`read` to `admin`) allows;
  an optional restriction to some of them (`api_key_accounts`) names only operator mailboxes,
  through the route and through `apikey create --bootstrap` alike, and a key whose last restriction
  goes is revoked rather than widened. Nothing about it changed.
- **A workspace key** belongs to a personal workspace or a team, and acts as nobody: its
  `api_keys.user_id` is `NULL` (a trigger refuses anything else) and its creator is
  `created_by` (`usr_…`). Its scope is `read`, `write` or `send`, never `admin`. It reaches exactly
  the mailboxes of its workspace it holds something on:

  ```sql
  key_access(key_prefix, account_id, workspace_id, read, act, send, granted_by, created_at, updated_at)
    PRIMARY KEY (key_prefix, account_id)
    FOREIGN KEY (account_id, workspace_id) REFERENCES accounts(id, workspace_id) ON DELETE CASCADE
    CHECK (act = 0 OR read = 1), CHECK (read + act + send > 0)
  ```

  Triggers hold that a row is in the key's own workspace (never the operator's), never moves, and
  that `act` needs the `write` scope or more and `send` the `send` scope. A key with no row
  reaches nothing, and is not revoked for that: an owner or an admin may give it a mailbox later.
- **A carried-over key** (`workspace_id NULL`) is a person's key 0012 found reaching mailboxes of
  several workspaces. It keeps exactly what it held, gains nothing (the same trigger), expires
  within 365 days, and is revoked when its last mailbox goes. Each workspace it reaches lists it,
  with that workspace's mailboxes and a count of the others (`other_workspaces`); revoking it there
  takes that workspace's mailboxes out of it. The daemon says at start how many are live.

**Who.** Only an active owner or admin of the workspace, signed in, creates a key, lists the
workspace's keys (revoked and expired ones too, live first, newest first in each group) and revokes
any of them, whoever created it; in a personal workspace that is its person. A member creates
none. A key never mints, lists or changes a key, so a leaked one cannot keep access after it is
revoked; the operator lists and revokes every key on `/v1/apikeys`. A person lists the keys they
created, in every workspace, on `GET /v1/me/apikeys`, and revokes any of them.

**What a key holds.** `read` on a mailbox is given only by an owner or an admin who **reads that
mailbox themselves, then**, checked in the transaction that writes `key_access`: a role reads
nothing, and minting a key never lets an owner or an admin read mail they hold no grant on. `act`
(where the key reads, with the `write` scope or more) and `send` (with the `send` scope) any owner
or admin gives. What a key holds **stands on its own**: whoever gave it, and its creator, may lose
their own access, demotion included, and the key keeps its mailboxes until an owner or an admin
takes them out or revokes it; so every owner and admin sees every key of the workspace, and each
mailbox's keys in the access directory. Keys never count as readers ([Protections](#protections))
and never pass `read`.

**Creating one** (`POST /v1/workspaces/{id}/apikeys`): a name (up to 120 characters), the scope,
`ttl_days` (30, 90 — the default — or 365; not bounded by what is left of the session it is created
in), `terms_version`, which must be the current key terms (`MAIL_CONSENT_VERSION_KEYS`, `409`
otherwise), and the mailboxes with their flags. At most 20 live keys per workspace made as keys are
now, counted in the transaction that issues (`409`); the persons' keys migration 0012 moved in are
not counted, since a team may hold more than 20 of them, and they expire on their own. The secret is in the answer and nowhere else; only its Argon2id
hash is stored. `POST /v1/me/apikeys` answers `400` ("API keys are created in a workspace by its
owners and admins").

**What a key does.** The key terms its creator agreed to, recorded on the key, cover what a tool
holding it does with what it holds: no person's consent to actions or sending is asked for a key's.
Before a mail server is touched the service asks again whether the key is live and not expired,
still holds the flag and has the scope, and, for sending, whether keys may send. A person's key
0012 carried over was created under terms that let it act only while its person allowed actions
and never send: it goes on that way. The actions text says so too (`2026-10-open-actions-3`):
turning actions off stops the person's own and those of such a key of theirs, never one created
since, which taking `act` away or revoking it stops ([conflict 24](#conflicts-and-resolutions)).

**Sending by key** takes the one send path: `confirm: true` on every send, the idempotency key
reserved before anything is dialed, an outcome after `DATA` `unknown` (never retried) or `failed`.
Only from a mailbox the key holds `send` on, with the `send` scope; at most `DailyKeySendLimit`
(100) sends a day per key; the message goes out under the address alone, no person's name; the
send record's `created_by` and the `send.finished` notice's `sent_by` name the key
(`key:<prefix>`), and only that key reads them (`GET /v1/sends/{key}`, the event stream). The
workspace's owners and admins list a key's sends
(`GET /v1/workspaces/{id}/apikeys/{prefix}/sends`). A key-less send's idempotency key is a keyed
hash of the message and the key, and the minute: the same key retrying is answered from its record,
another key sending the same message from the same mailbox sends its own
([conflict 10](#conflicts-and-resolutions)). `MAIL_KEYS_MAY_SEND=false` is for an edition whose key
terms do not cover sending: the `send` scope and flag are refused when a key is created or given a
mailbox, and every send by a workspace key is refused; operator keys are not affected. It defaults
to `true` only under the open key terms: a daemon whose `MAIL_CONSENT_VERSION_KEYS` names other
terms refuses to start until it is set ([conflict 25](#conflicts-and-resolutions)).

**Revoked automatically**, in the same transaction, the keys a person created: when they are
removed from the workspace (a carried-over key loses that workspace's mailboxes), and when they
are disabled or deleted on the instance. Demoting them or disabling their membership keeps the
keys, as Wappie does. Deleting a person deletes the keys of their personal workspace and of the
teams they were alone in, with those workspaces, and blanks their name on the keys they created in
a team that stays, which stay revoked as the team's record.

**Held principals.** A workspace key's principal names no mailbox: every use reads what the key
holds now, so the stdio MCP session, a resource subscription, an event stream and a long poll see
a mailbox given or taken at once (the stream says so with `event: access`), and a key revoked or
expired stops at the next re-check, before every answer. `ErrKeyNarrowed` (`409`) is left only for
an operator key whose restriction lost a mailbox.

**MCP.** Tools and resources call the same service methods and see exactly what the key holds;
`AuthenticateTool` keeps refusing sessions on `/mcp`, and a key nobody agreed to the key terms
through, which migration 0012 revoked, everywhere.

## Choosing the workspace in a request

There is no "current workspace" on the server for a session, which reaches the person's mailboxes
in every workspace they belong to; a workspace key belongs to its workspace and reaches only what it
holds there. Instead:

- Every mailbox-bearing answer carries `workspace_id` (accounts, storage).
- `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events` and
  `GET /v1/events/wait` accept `?workspace=ID`, which narrows them to that workspace's mailboxes. A
  workspace the caller is not an active member of is `404` (for a key, any workspace but its own,
  or for a carried-over key one it holds no mailbox in); with `account` as well, the account must
  be in that workspace.
- The console keeps the current workspace itself (per tab, remembered for each person in local
  storage) and passes `?workspace=` so a view never fetches another workspace's data
  ([`console.md`](console.md), "Workspaces in the console").

## Events

The event gate (`internal/service/events.go`) today remembers per account whether the caller may
see it, assuming that never changes. With grants it does:

- The service keeps an **access epoch**, a counter it advances after every commit that can give or
  take `read` from someone: a grant set or revoked, a membership disabled, enabled again or
  removed, a mailbox linked or removed, a person disabled or deleted, a key created, given or taken
  a mailbox, or revoked. A role never gives `read`, so a role change
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
  by `?account=`, or every mailbox of a restricted instance key, is gone, or the caller is no longer an
  active member of the workspace `?workspace=` named. Do not reconnect with the same filter then. A
  stream of everything the caller may read stays open however little that is — none since the last
  grant was revoked, or none yet — because one opened now would be the same: a mailbox granted or
  linked later appears on it without reconnecting.
- **`send.finished`** is not the mailbox's but one sender's record (see
  [conflict 10](#conflicts-and-resolutions)): its payload names the sender — `user_id` for a
  person, `sent_by` (`key:<prefix>`) for a key — and it goes only to whoever may read that record
  with `GET /v1/sends/{key}`: the person who sent it, with the `send` flag on the mailbox at
  delivery, `read` or not; a workspace key's send, to that key while it may send from the mailbox;
  an instance key's send, to the instance keys with the `send` scope. A member who reads a shared mailbox never hears
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
so it is not a workspace's whole usage. An instance key answers for the operator workspace, a
workspace key for the mailboxes it reads.

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
| `DELETE /v1/accounts/{id}/access/{user_id}?flags=` | owner or admin, their own flags included; operator | `204`; `flags` (comma-separated `read`, `act`, `send`, `manage`) names what goes, every flag without it; taking `read` deletes the person's grants on the mailbox |
| `GET /v1/accounts/{id}/mailbox-key` | session holding `read` on it (a person who sees only its card: `403`) | `MailboxKeyState`: the key pair, the caller's own grant, and whom they may seal to or who may seal to them |
| `POST /v1/accounts/{id}/mailbox-key` | session reading it by the flag, with an account key; fresh step-up | `{public_key, namespace, grants: [{user_id, grant}]}` → `MailboxKeyPair` (201), the first key of a mailbox without one |
| `PUT /v1/accounts/{id}/mailbox-key` | session, the personal mailbox's person; fresh step-up | `{epoch, public_key, grant}` → `MailboxKeyPair`, the next key; older grants deleted |
| `PUT /v1/accounts/{id}/grants/{user_id}` | session reading it; fresh step-up | `{epoch, grant}` → `SealedGrant`: the key, to a member who holds `read` without it |
| `GET /v1/workspaces/{id}/apikeys` | owner or admin, signed in | `[WorkspaceKey]`: every key of the workspace and the carried-over keys holding one of its mailboxes, revoked and expired ones too, live first |
| `POST /v1/workspaces/{id}/apikeys` | owner or admin, signed in | `{name, scope, ttl_days, terms_version, mailboxes: [{account_id, read, act, send}]}` → `{key, …WorkspaceKey}` (201), the secret shown once |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}` | owner or admin, signed in | `204`; a carried-over key loses the workspace's mailboxes instead |
| `PUT /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | owner or admin, signed in; `read` only from one who reads the mailbox | `{read, act, send}`, all three required, not all false → `KeyMailbox` |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | owner or admin, signed in | `204`: the mailbox is out of the key |
| `GET /v1/workspaces/{id}/apikeys/{prefix}/sends` | owner or admin, signed in | `[SendStatus]`, the key's sends from the workspace's mailboxes, newest first (at most 200) |
| `GET /v1/me/apikeys` | session | `[WorkspaceKey]`: the keys the person created, in every workspace |
| `DELETE /v1/me/apikeys/{prefix}` | session | `204`: revokes a key the person created |
| `POST /v1/me/apikeys` | session | `400`: keys are created in a workspace |

Gone: `POST /v1/accounts/{id}/take-over` (`404`; see [Taking over a link](#taking-over-a-link)).

Changed routes:

| Route | Change |
|---|---|
| `POST /v1/accounts` | optional `workspace_id`; who may link where, as [above](#mailboxes); into a team, optional `sync_consent_version` (the current sync text: the team's consent, given with the link); from a person, `public_key`, `namespace` and `grant` (the mailbox's first key, phase 3), with a fresh step-up |
| `PUT /v1/accounts/{id}/access/{user_id}` | optional `grant`: required with `read` given on a mailbox that has a key to a person with an account key, refused otherwise (phase 3) |
| `GET /v1/accounts`, `GET /v1/accounts/{id}`, `GET /v1/accounts/{id}/sync` | a team's owners and admins see the card of each of its mailboxes |
| `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events`, `GET /v1/events/wait` | optional `?workspace=` |
| `DELETE /v1/accounts/{id}?confirm=<id>` | the id repeated, or `400` and nothing removed; a team's owners and admins, a personal mailbox's person, the operator for its own |
| `POST /v1/accounts/{id}/oauth/start`, `POST /v1/accounts/oauth/callback` | manage (callback: the flow is still only its starter's) |
| `PUT /v1/accounts/{id}/sync` | `{enabled, version}`: a team mailbox's consent, from its owners and admins signed in (on: `version` the current sync text; off: deletes its index for everyone); an operator mailbox's switch, from the operator, with no `version`; `400` for a personal mailbox; `409` to turn on a team mailbox nobody can read. Answers `AccountSync` |
| `POST /v1/auth/signup` | redeems an instance invite, or a team invite the operator or an instance owner made; the first sign-up is no owner unless its invite says so |
| `POST /v1/users/disable`, `POST /v1/users/delete` | also an instance owner's session; the team protections (last owner, last reader of a team that outlives them, a consent still bound to them that someone else reads); `team_syncs_stopped` lists the team mailboxes a closure stopped |
| `GET /v1/sends/{key}?account=` | only the caller's own sends ([conflict 10](#conflicts-and-resolutions)); a key's are that key's |
| `POST /v1/messages/send` | a workspace key holding `send`, with the `send` scope, where keys may send |
| `GET /v1/workspaces/{id}/access` | each mailbox lists its live keys (`keys`) |
| `GET /v1/apikeys` (operator) | each key names its `workspace_id`; a workspace key's `account_ids` are the mailboxes it holds |

Shapes:

```jsonc
// Workspace
{"id": "wsp_0a1b2c3d4e5f6a7b", "kind": "team", "source": "local", "name": "Support",
 "role": "admin", "status": "active", "created_at": 1790000000}
// the operator's listing has no role or status, and adds "members" and "mailboxes" counts

// Member ("person_disabled": true only for a person switched off on the instance; last_reader_of
// lists the team mailboxes they alone read, always present; seal_id and public_key are what a
// grant to them is bound by and sealed to, public_key absent until they enrol)
{"user_id": "usr_…", "email": "bea@example.org", "name": "Bea Lima", "role": "member",
 "status": "active", "last_owner": false, "last_reader_of": ["acc_…"],
 "seal_id": "b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4", "public_key": "…", "joined_at": 1790000000}

// MailboxAccess, whose grants are each a Grant. linked_by is attribution only (absent once that
// person is deleted); readers counts who reads it now, by the one rule; no_reader marks a team
// mailbox nobody can read; epoch is its key's current one (absent without a key); sync is the
// mailbox's own consent (absent for a personal mailbox): migrated marks one the upgrade copied
// from its linker and nobody confirmed yet, current whether version is the revision asked for
// now. A grant's sealed says its person holds the key at the current epoch.
{"account_id": "acc_…", "email": "support@example.org", "provider": "gmail", "state": "active",
 "linked_by": "usr_…", "readers": 2, "no_reader": false, "epoch": 1,
 "sync": {"enabled": true, "enabled_at": 1790000000, "enabled_by": "usr_…",
          "version": "2026-10-open-sync-3", "current": true},
 "grants": [{"account_id": "acc_…", "user_id": "usr_…", "read": true, "act": true, "send": true,
             "manage": false, "granted_by": "migration", "updated_at": 1790000000, "sealed": true}]}
// a Grant's manage is the stored flag, which only a member holds; owners and admins manage by
// their role, which the members list says

// MailboxAccess.keys: the live keys holding something on the mailbox
{"prefix": "0a1b2c3d", "name": "Support bot", "scope": "write", "read": true, "act": true,
 "send": false, "created_by": "usr_…", "granted_by": "usr_…", "updated_at": 1790000000}
// "carried_over": true for a key 0012 carried over

// WorkspaceKey (the secret is never here; created_by absent once that person is deleted; origin
// "person" or "person-all" for a person's key 0012 moved into its workspace; carried_over and
// other_workspaces for one it carried over; sends: the key can send at all, its scope and the
// server allowing)
{"prefix": "0a1b2c3d", "name": "Claude Code", "scope": "read", "workspace_id": "wsp_…",
 "mailboxes": [{"account_id": "acc_…", "workspace_id": "wsp_…", "read": true, "act": false,
                "send": false, "granted_by": "usr_…", "updated_at": 1790000000}],
 "created_by": "usr_…", "created_at": 1790000000, "expires_at": 1797776000,
 "last_used_at": 1790003600, "live": true, "terms_version": "2026-10-open-api-keys-2",
 "sends": false}

// MailboxKeyPair: a mailbox's key at one epoch, as Account.mailbox_key, a first key and a new key
// answer it
{"epoch": 1, "public_key": "…", "namespace": "9d035f2b-81d0-420e-90e2-bb16e950497b"}

// MailboxKeyState (GET /v1/accounts/{id}/mailbox-key): for a reader, the key pair, their grant and
// who waits; for a member waiting, the key pair and who may supply it (suppliers: user_id, email,
// name); on a mailbox without a key, keyless_readers, whom its first key is sealed to
{"epoch": 1, "public_key": "…", "namespace": "9d035f2b-…", "grant": "TUwBAQEAAQ…",
 "waiting": [{"user_id": "usr_…", "email": "carol@example.org", "name": "", "seal_id": "…",
              "public_key": "…"}],
 "suppliers": [], "keyless_readers": []}

// SealedGrant (PUT /v1/accounts/{id}/grants/{user_id})
{"account_id": "acc_…", "user_id": "usr_…", "epoch": 1, "grant": "TUwBAQEAAQ…",
 "granted_by": "usr_…", "created_at": 1790000000}

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
instance key, what its scope allows on an operator mailbox. Since 0014, `access.read` is reading
now, by the one rule; for a person signed in, `access.waiting_key` marks the flag held on a
mailbox that has a key without a grant, and `mailbox_key` is the mailbox's key pair at its current
epoch (absent without a key, and for every API key).
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
  (`?confirm=`). `apikey create` issues operator keys; `apikey list` lists every key with its
  workspace (`several` for a carried-over one) and, for a workspace key, the mailboxes it holds
  (`none` when it holds none); `apikey revoke` revokes any key. A workspace's keys are created in
  the console only.
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

## Migration 0012

`0012_workspace_keys.sql`, an ordinary migration in one transaction with foreign keys on: two
columns added to `api_keys` (`workspace_id`, `origin`), the table `key_access`, three indexes (keys
by workspace and by creator, sends by who sent them), rows updated and moved, 0005's trigger on
`api_key_accounts` dropped and created again as it was around the move, and the triggers of
[API keys](#api-keys) added. What becomes of a schema-11 database:

| Before | After |
|---|---|
| an instance key (`user_id NULL`) | `workspace_id = 'wsp_operator'`; nothing else changes, its restriction rows included (one restricted to a person's mailbox still reaches nothing) |
| a person's key made for chosen mailboxes, live | `origin = 'person'`; a `key_access` row for each chosen mailbox its person reads now (an active grant with `read`, as an active member, active on the instance) with `read`, `act` where the key was `write` and its person held `act`, never `send`, `granted_by = 'migration'`; a chosen mailbox its person no longer reads is left out: nothing it could do yesterday is widened |
| a person's key made for every mailbox of theirs, live | `origin = 'person-all'`; a row for every mailbox its person reads now, the same way. Mailboxes linked later are not added: an owner or an admin who reads one adds it |
| a revoked or expired person's key | mapped the same way, with the mailboxes it was made for, for the record; it reaches nothing |
| its workspace | the one its mailboxes are in; its person's personal workspace when it has none; `NULL` (carried over) when they span several, with `expires_at` brought within 365 days. A team may end up with more than 20 live keys this way (each person could hold 20): they do not count toward its limit |
| a person's key nobody agreed to the key terms through (`terms_version ''`: made for them by an administrator, or before keys had terms), refused everywhere since phase 4 | revoked, holding nothing, in its person's personal workspace whatever it reached: a team never sees a key made for one of its members, and the key goes with that workspace when its person is deleted, as it went with them before ([conflict 26](#conflicts-and-resolutions)) |
| `api_keys.user_id` | `NULL` everywhere; the column stays, since dropping it takes a rebuild, and a trigger refuses anything else. `created_by` keeps who created each key |
| `api_key_accounts` | only the operator keys' rows |
| everything else | untouched, value for value |

A person's key in a team where its person is a plain member becomes that team's key all the same:
listed to its owners and admins, revocable by them; only creating new ones is closed to members.
The hosted service's shape — one person, two personal mailboxes, one key whichever it is — keeps
the key working on what it reached: the person's key goes into their personal workspace with both
mailboxes, an instance key stays the operator's, a revoked one stays revoked. The tests in
`internal/store/migrate_twelve_test.go` dump every table before and after, on that shape (each kind
of key) and on a self-hosted one with teams and keys of every kind, and compare them value by
value except the columns named.

## Migration 0014

`0014_mailbox_keys.sql`, an ordinary migration: the two tables of
[Mailbox keys and sealed grants](#mailbox-keys-and-sealed-grants), their triggers and indexes, and
nothing else. No row of a schema-13 database changes, and **every mailbox stays without a key**,
read by the flag as before, until a person who reads it and has enrolled writes its first key from
the console (section 12.14): right after their next sign-in, while it counts as a step-up, or
from the mailbox behind a step-up, never on reopening an older session. From that first key on, a
member who held the flag without an account key (one who has not upgraded) waits for the key until
a reader supplies it once they enrol. Operator mailboxes stay without a key. The tests in
`internal/store/migrate_fourteen_test.go` hold every table's rows and columns as they were, both
new tables empty, and each refusal of the schema.

## Going back

Schemas 10 to 14 are refused by an older binary like any newer one: going back past 0012 is the
backup taken before the upgrade, restored with the binary of its time. Going back past 0013 or 0014
is the same, and loses what they hold: past 0014 every mailbox key and grant written since (the
mailboxes are read by the flag again, and the console writes their first keys again after a
sign-in); past 0013 every account key, so everyone who enrolled signs in with a reset invitation.

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

- Visibility: `TestAMemberWithoutAGrantCannotSeeTheMailbox` over REST, the event stream, the long
  poll and storage, and over MCP, which takes keys only,
  `TestAKeyHoldingNothingReachesNothingUntilAReaderGivesItAMailbox`;
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
  `TestOnlyAnOwnerOrAdminSignedInCreatesAWorkspacesKeys` (and over REST
  `TestOnlyAnOwnerOrAdminSignedInManagesAWorkspacesKeys`); `TestAKeyNeverMintsListsOrChangesAKey`;
  `TestAKeyReadsOnlyMailboxesAReaderGaveItAndAnAdminWhoReadsNothingCannotReadThroughOne` (the
  service, every path: search, message, folders, storage, the stream, the long poll, a
  subscription), over REST `TestAnAdminWhoReadsNothingCannotReadThroughAKeyOverREST` and over MCP
  `TestAnOwnerOrAdminReadsNoTeamMailboxThroughTheirKey`;
  `TestAKeyKeepsItsMailboxWhenItsCreatorLosesReadAndEveryOwnerOrAdminSeesAndRevokesIt`;
  `TestAKeyActsOnlyWithActAndTheWriteScopeUnderItsKeyTerms`;
  `TestAPersonsKeyCarriedOverActsOnlyWhileItsCreatorAllowsActionsAndNeverSends`;
  `TestAKeySendsOnlyWithTheSendFlagTheSendScopeAndConfirmAndUnderTheAddressAlone`;
  `TestAKeysSendThatMayHaveBeenDeliveredIsNeverRetried`; `TestNoKeySendsWhereTheServersKeysMayNotSend`
  (and `TestKeysSendByDefaultOnlyUnderTheOpenKeyTermsAndAnotherEditionSaysWhetherTheyMay` in
  `internal/config`); `TestTwoKeysSendingTheSameMessageWithoutAKeyInOneMinuteEachSendTheirs`;
  `TestAKeySendsAtMostItsDailyLimit`; `TestAKeyNeverReachesAnotherWorkspace`;
  `TestRemovingDisablingOrDeletingTheCreatorRevokesTheirKeysButADemotionDoesNot` (and in
  `internal/workspace` and `internal/auth`);
  `TestAWorkspaceHoldsAtMostTwentyLiveKeys`,
  `TestThePersonsKeysTheUpgradeMovedIntoATeamDoNotCountTowardItsTwentyLiveKeys`;
  `TestAHeldKeySeesWhatItHoldsChangeAtOnce`,
  `TestAHeldSessionSeesAMailboxTakenOutOfItsKeyAtOnce` (MCP) and
  `TestAKeysStreamStopsCarryingAMailboxTakenOutOfIt` (SSE);
  `TestACarriedOverKeyGainsNothingAndIsRevokedWithItsLastMailbox`.
- Events: `TestAStreamFollowsAccessAsItChanges`;
  `TestAStreamOfEverythingOutlivesTheLossOfEveryMailboxItRead`;
  `TestASendFinishedReachesOnlyItsSender`.
- Subscriptions and storage: `TestSubscribingToAnInboxNeedsReadAccessToIt`;
  `TestASubscriptionToAnInboxTheKeyCanNoLongerReadIsDropped`;
  `TestStorageCountsOnlyTheMailboxesTheCallerMayRead`.
- Consent attempts: `TestAConsentFinishingAfterItsStarterStoppedManagingTheMailboxStoresNoGrant`;
  `TestLosingManageOfAMailboxEndsTheConsentAttemptsStartedOnIt`.
- Use: `TestActingNeedsTheActorsConsentAndTheActFlag`;
  `TestAMailboxKeptStoppedWithItsIndexIsNotChangedByARefusedAction`;
  `TestAnActionCutShortWhenItsMailboxStopsSyncingNeverSaysTheServerIsUnchanged`;
  `TestAWorkspaceKeyNeverChangesAMailboxWhoseIndexCannotFollowIt`;
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
  `TestMigrationElevenRefusesWhatItsSchemaForbids`. 0012:
  `TestMigrationTwelveMovesEachPersonKeyIntoTheWorkspaceItReaches` (the self-hosted shape with
  every kind of key, every table compared value by value, an administrator's key for a team member
  left in that member's personal workspace),
  `TestMigrationTwelveKeepsTheSmallestShapesKey` (the hosted shape, with each kind of key),
  `TestMigrationTwelveNeverWidensAKeysActions`,
  `TestMigrationTwelveGivesAnUnrestrictedKeyWhatItReachedAndNoMore`,
  `TestMigrationTwelveCarriesOverAKeySpanningWorkspacesFrozen`,
  `TestMigrationTwelveLeavesOperatorKeysAndTheirRestrictionsAlone` and
  `TestMigrationTwelveRefusesWhatItsSchemaForbids`.
- Mailbox keys (phase 3, step 4): the one rule,
  `TestAKeylessMailboxIsReadByTheFlagAlone`, `TestAMemberWithTheFlagButNoGrantDoesNotReadAKeyedMailbox`,
  `TestTheLastReaderOfAKeyedTeamMailboxCountsOnlyGrantHolders`,
  `TestATeamMailboxWithAKeySyncsOnlyWhileSomeoneHoldsItsGrant`,
  `TestAKeyedMailboxShowsAMemberWaitingForTheKeyItsCardAndNothingToRead` and
  `TestAMemberWaitingForTheKeyIsRefusedTheAccessRoutesNotToldTheMailboxIsMissing`; writing,
  `TestAPersonLinksAMailboxWithItsKeyOnThePasswordAndTheOAuthPaths`,
  `TestALinkWithoutItsKeyOrWithAMalformedOneStoresNothing`,
  `TestAPersonWithoutAnAccountKeyCannotLinkAMailbox`, `TestALinkStoresNothingForAPersonNoLongerActive`,
  `TestReadOnAKeyedMailboxIsGivenWithTheRecipientsGrantAndRefusedWithout`,
  `TestReadIsGivenByTheFlagAloneToAMemberWithoutAnAccountKeyWhoThenWaitsForTheKey`,
  `TestOnlySomeoneWhoReadsAKeyedMailboxGivesReadOnIt`, `TestOnlySomeoneWhoReadsAKeyedMailboxGivesAKeyReadOnIt`,
  `TestAnyReaderSuppliesTheKeyToAMemberWhoWaitsForIt`,
  `TestTheFirstKeyOfAMailboxComesWithAGrantForEveryoneWhoReadsItWithAnAccountKey`,
  `TestOnlyAPersonalMailboxsPersonWritesItANewKeyAndItsOlderGrantsGo` and
  `TestAMailboxKeyIsWrittenOnce`; taking, `TestTakingReadDeletesTheGrantsInTheSameTransaction`,
  `TestDisablingAPersonDeletesTheirGrantsAndKeepsTheirFlags`,
  `TestAResetDeletesEveryGrantOfThePersonAndKeepsTheirFlags` and
  `TestAResetPersonWaitsForTheKeysOfTheirMailboxesUntilANewKeyOrAReaderGivesThemBack` (their personal
  mailbox keeps syncing meanwhile); the event stream, `TestAStreamHearsAFirstKeyTakeReadFromAReaderWithoutAnAccountKey`
  and `TestAStreamHearsTheSuppliedKeyGiveRead`; the step-up,
  `TestEveryKeyWriteNeedsAStepUpWithinTenMinutes`, `TestALinkWhoseStepUpEndedDuringItsLoginStoresNothing`
  and `TestChangingFlagsWithoutAGrantNeedsNoStepUp`; and the boundary,
  `TestNoMailboxPrivateKeyReachesTheDatabase`, `TestAKeySeesNoMailboxKeyAndWaitsForNone`,
  `TestAKeysListAccountsCarriesNoMailboxKey` (MCP), `TestTheMailboxKeyRoutesAreThinOverTheService` and
  `TestNoRouteTakesAMailboxPrivateKey` (REST, no route taking a private key).

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
    deleted) and reaches only who may read the record (see [Events](#events)). *Then (2026-10-06,
    with workspace keys):* keys are how tools send, many of them from one shared mailbox, and a
    key-less send's derived key made of the message alone refused a second key's identical alert in
    the same minute as a reused key — sending nothing, and telling it that another sender had just
    sent exactly that. A derived key is now a keyed hash of the message and its sender, and the
    minute: the same sender retrying is still answered from its record, and another sends its own.
    An `Idempotency-Key` a caller chose stays per mailbox, as above.
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
    key text kept its revision until step 3, which gave it `2026-10-open-api-keys-2`
    ([conflict 20](#conflicts-and-resolutions)).
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
18. **Keys of the workspace.** Phase 2 let each person create keys acting as them, reaching live
    what they could. *Resolution (the owner, 2026-10-06):* keys belong to their workspace, as
    Wappie's: only its owners and admins create, list and revoke them, a key never mints a key, and
    each holds its own mailboxes (`key_access`). See [API keys](#api-keys).
19. **What a key holds stands on its own.** The design of 2026-10-06 capped each key live by the
    person who answered for it (losing `read` took the mailbox out of their keys).
    *Resolution (the owner, 2026-10-06, replacing that):* no cap, as in Wappie: a key keeps a
    mailbox when its creator, or whoever gave it, loses `read` or is demoted; every owner and admin
    sees every key of the workspace, and each mailbox's keys in the access directory, and revokes
    them. What keeps "no role reads" true is where `read` comes from: only an owner or an admin who
    reads the mailbox then gives a key `read`, and keys never count as readers nor pass `read`. The
    key stops when its creator leaves the workspace or is disabled or deleted on the instance —
    never on a demotion or a disabled membership, as Wappie revokes only on removal.
20. **Keys may send.** Phase 2's keys never sent (`read` or `write`). *Resolution (the owner,
    2026-10-06):* keys send, as Wappie's do: the `send` scope and the `send` flag on the mailbox,
    `confirm: true` on every send, the one send path (idempotency reserved before dialing, `unknown`
    never retried), a daily limit per key (100), the address alone in the From, records and notices
    naming the key. What a key does is covered by the key terms its creator agreed to, on the key,
    not by anybody's own consent to actions or sending. An edition whose key terms do not cover
    sending turns it off with `MAIL_KEYS_MAY_SEND=false`, which refuses the `send` scope and every
    send by a workspace key. The open key terms say so at `2026-10-open-api-keys-2`; keys created
    under `2026-10-open-api-keys` keep working under it and never send (their scope is at most
    `write`), and, as people's keys from before, act only while their creator allows actions, as
    that text said.
21. **Carried-over keys.** A person's key that reached mailboxes of several workspaces has no one
    workspace to go into. *Resolution:* migration 0012 carries it over with no workspace: exactly
    what it held, nothing more ever (a trigger), within a year, revoked with its last mailbox, and
    listed by each workspace it reaches, whose owners and admins take their mailboxes out of it. A
    later migration drops the case once none is left; the daemon says at start how many are live.
22. **Members create no keys.** A member of a self-hosted team could create keys for the mailboxes
    they read; now an owner or an admin creates one for them, holding what they decide.
    `POST /v1/me/apikeys` answers `400` and says where keys are made. The upgrade notes in
    [`self-hosting.md`](self-hosting.md) say so.
23. **A key's lifetime and the session it was created in.** A key lasts what it was created for
    (30, 90 or 365 days), whatever is left of the session it was created in, as before: a session an
    extension started for less than a password's (`SignInExternal`) bounds the session, not the
    key, and disabling the person is what revokes the keys they created sooner.
24. **The actions text and keys that act under their own terms.** The open actions text
    `2026-10-open-actions-2` counted "a tool with an API key you created that can act" among those
    acting under the person's agreement, and said that turning actions off stops them at once; a
    key created since 0012 acts under the key terms, whatever anyone chooses about actions, and a
    key another owner or admin created acts on a team's mailboxes too. *Resolution:* a new revision,
    `2026-10-open-actions-3` (`ActionsText.vue`, `versions.ts`, `config.DefaultActionsConsentVersion`):
    the server changes a mailbox when you ask, under this agreement, anyone else allowed to act
    there, under their own, or a tool whose key is given Act there, under the key terms; turning
    actions off stops yours and those of a key you created before keys belonged to workspaces,
    never a key created since, which taking Act away or revoking it stops. The key terms
    (`-2`) say the same of the key, the open console's dialog for turning actions off says what
    stops and what does not, and its notices no longer say the mailboxes stop changing. As for any
    revision, everyone is asked again before acting.
25. **The sending switch and an edition's own key terms.** `MAIL_KEYS_MAY_SEND` defaults to `true`
    because the open key terms say a key may send. An edition with its own key terms, which may say
    a key cannot send, inherited that default with nothing to catch it before its next release. *Resolution:* the default stands only under the open key terms; a
    daemon whose `MAIL_CONSENT_VERSION_KEYS` names other terms refuses to start until
    `MAIL_KEYS_MAY_SEND` is set, `true` or `false`. Whether those terms also say that a key acts
    under them rather than under its creator's actions consent is that edition's text to settle.
26. **Keys made for a person by an administrator.** Before keys had terms, an administrator could
    make a key acting as a person; 0012 found such keys (`terms_version ''`) and would have put
    one into the team whose mailboxes it reached, listed there for good, with its name and last use,
    and kept after its person's deletion, with nothing left that said whose it had been.
    *Resolution:* such a key is revoked holding nothing, in its person's personal workspace, and
    goes with that workspace when the person is deleted, as it went with them before.
27. **A team over its key limit from day one.** Each person could hold 20 live keys before 0012, so
    a team whose members' keys 0012 moved in may hold many more than 20. *Resolution:* the limit
    counts the keys made as keys are now (`origin = ''`); the ones moved in expire on their own,
    and the team's owners and admins revoke those they no longer want.

## Not in this phase

- A send tool over MCP (the planned draft and confirmed send); keys send over REST.
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
| `internal/store/migrations/0012_workspace_keys.sql` | keys of the workspace, `key_access`, the carried-over keys |
| `internal/workspace/keys.go`, `internal/auth/keys.go` | what a key holds; keys issued, listed and revoked per workspace; a principal naming its workspace, `IsInstance` meaning the operator workspace's |
| `internal/service/workspacekeys.go` (replacing `mykeys.go`) | who creates, lists, changes and revokes keys; sending and acting by key (`send.go`, `actions.go`); `MAIL_KEYS_MAY_SEND` |
| `cmd/mailserver` | `workspace`, `member`, `access`; `user invite --workspace`; the bootstrap default; the startup hint |
| `.golangci.yml` | `internal/workspace` joins the packages transports may not import |
| docs | `console.md` (ownership becomes workspaces, routes, storage, events, the current workspace), `architecture.md` and `CLAUDE.md` (the ownership rule), `mcp.md` (instance keys), `self-hosting.md` and the README (the first owner) |
| `web/` | the types and the contract spec; the workspace switcher, every list narrowed with `?workspace=`, the `access` events, each mailbox's grant and access panel, accepting a team invitation signed in, and the open edition's Members section ([`console.md`](console.md), "Workspaces in the console") |
| 0014 (phase 3, step 4) | `internal/store/migrations/0014_mailbox_keys.sql` and `migrate_fourteen_test.go`; `store/readers.go` (the one rule as SQL fragments); `workspace/sealed.go` (keys and grants written and read), `grants.go`, `reader.go`, `members.go` and `people.go` (the rule, grants taken with `read`); `account/store.go` (`Visibility` by the rule; the link's key in its transaction); `auth` (a disable and a reset take the grants; the reset's guard counts mailboxes with a key); `service/mailboxkeys.go` (the use cases and their step-up), `accounts.go`, `workspaces.go`; `api/mailboxkeys.go`; the contract fixtures and the console's types; the console's half (`web/src/crypto/mailbox.ts`, `state/mailboxKeys.ts`, `state/grants.ts`, `state/stepUp.ts`, the link's key, Read with a grant, the sheet's key section; [`console.md`](console.md), "Mailbox keys") |
| 0011 (2026-10-06, step 1) | `internal/store/migrations/0011_team_mailboxes.sql` and `migrate_eleven_test.go`; `store/eligibility.go` (a team mailbox's consent and a reader), `store/consent.go` (`SetMailboxSync`, `StopBoundTx`, a person's withdrawal personal only); `workspace/reader.go` (the last reader, `HasReaderTx`), `grants.go` (effective manage, the directory), `members.go` (invites expire, no linker), `people.go` (blocks: last owner, last reader of a team that outlives them, a bound consent someone else reads; attribution blanked); `account/store.go` (the person of a personal mailbox, `linked_by`, the team's consent at the link, `Visibility` by role) and `registry.go` (`LinkerID`, `RemoveChecked`, a sole member's teams removed with them); `auth/closure.go` (a disable changes nothing for someone already disabled); the service's grant rules, `RemoveAccount` with `confirm`, `SetMailboxSync` (never on for a mailbox nobody reads), closure (`team_syncs_stopped`); the API's routes and fixtures; MCP's `list_accounts` text (read access marked); the command line; the open texts `-3` and `-2` |
