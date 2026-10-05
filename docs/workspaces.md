# Workspaces

**Status: phase 2, approved by the owner on 2026-10-05, implemented on the server**: migration 0008
and the runner's rebuild procedure, `internal/workspace`, the service's authorization on workspaces
and grants for every path (REST, MCP, the event stream and the long poll), the routes, the command
line's commands and the contract fixtures. Not yet: the console's screens (step 3) and the
platform's workspace source, of which only the stub is here. [`console.md`](console.md) describes
the routes as the console reads them. Where this document had to choose between readings of the
plan, or found a rule that conflicts with the code or with another rule, it says so in
[Conflicts and resolutions](#conflicts-and-resolutions).

## In short

- Every mailbox belongs to a **workspace**: a person's **personal** workspace, a **team**
  workspace, or the one **operator** workspace, which has no members and is what instance keys and
  the command line act on.
- Seeing a mailbox takes two things: **active membership** in its workspace and a **grant** on it.
  A grant has four flags, `read`, `act`, `send` and `manage`. Whoever links a mailbox gets all four.
  Owners and admins of a team administer people and grants, but **get no automatic read**, and
  nobody can hand out a flag they do not hold.
- **Consents stay per person.** A mailbox syncs under the consent of the person who linked it; an
  action needs the actor's consent and the `act` flag; a send needs the sender's consent and the
  `send` flag, and goes out under the sender's name.
- The same address may be linked in several workspaces; each link is an independent mailbox with
  its own credentials, worker and index. Within one workspace an address is linked at most once.
- `users.role` (`owner`, `member`) becomes purely the **instance role** of a self-hosted server. It
  gives no mailbox visibility. The first owner comes only from `user invite --bootstrap`.
- Migration 0008 creates the workspaces and **rebuilds `accounts`**, through a new rebuild
  procedure in the migration runner that cannot cascade into credentials, folders or messages.
  Nothing of the index, credentials, folders, messages, events or sends changes. Migration 0009
  then sets right the two things 0008 could not change (see [Migration 0009](#migration-0009)).

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

Deleting a team workspace, and moving a mailbox from one workspace to another, are not part of this
phase.

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
- A personal workspace's single membership never changes; the operator workspace has none. A
  trigger refuses a membership in the operator workspace, and one in a personal workspace for
  anyone but its person, beside the repository's own check.
- Memberships cascade on the person (`ON DELETE CASCADE`), after the deletion has checked the
  protections below.

### Mailboxes

- `accounts.workspace_id` is required and never changes (a trigger refuses an update).
- `accounts.owner_user_id` keeps its name and now means **linked by**: the person who linked the
  mailbox, whose sync consent it syncs under. It is `NULL` exactly for the operator workspace's
  mailboxes; a trigger refuses any other combination. In prose, the API and Go comments it is
  "linked by"; the column keeps its name (see [conflict 12](#conflicts-and-resolutions)).
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

The plan does not say who links into a team; this document makes it an owner's or admin's act,
because a link puts a mailbox's index in a space the team shares. A member asks.

The linker gets a grant with all four flags in the transaction that creates the mailbox, which
re-checks their membership and role there (like `CheckOwnersWith` today). `POST /v1/accounts` takes
an optional `workspace_id`; without it a person links into their personal workspace and an instance
key into the operator workspace.

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
or a mailbox take the grants with it. A row with no flag left is deleted, never stored.

| Flag | Lets the holder | Requires |
|---|---|---|
| `read` | search and read messages, fetch originals and attachments, list folders, receive the mailbox's events, see its storage, ask for a sync pass (with the `write` scope) | |
| `act` | mark read or unread, star, archive, move, trash, undo a move | `read` (an action names messages the caller can read), and the actor's actions consent |
| `send` | send from the mailbox, read the records of their own sends | the sender's send consent; a reply or forward also needs `read` on the original |
| `manage` | re-authorize the mailbox, remove it, see and change who has access to it (within the [grant rules](#who-may-change-a-grant)) | |

Two levels of seeing follow from this:

- **Any grant** shows the mailbox in `GET /v1/accounts`, `GET /v1/accounts/{id}` and
  `GET /v1/accounts/{id}/sync`: its card (address, provider, state, sync status, `access`).
- **`read`** opens its index: folders, messages, events, storage.

Without any grant the mailbox does not exist for the caller (`404`). With a grant but without the
flag an operation needs, it is `403 not_authorized`.

### People on the instance

`users.role` stays `owner` or `member`, as the **instance role** of a self-hosted server:

- An instance `owner` may invite people to the server, disable and delete people, and is the only
  role that may change the server's instance-level settings (none is changed over the API yet).
- It gives **no mailbox visibility**. Today an owner also sees, acts on and sends from the mailboxes
  nobody owns; those become the operator workspace's, reached only by instance keys and the command
  line. No person ever gets operator powers through the operator workspace.
- The first owner comes only from `mailserver user invite --bootstrap --role owner`. The rule
  "whoever signs up first becomes an owner, whatever the invite says" (`auth.SignUp`) goes, with its
  warnings in the command line and the startup hint, and with the README's and
  `self-hosting.md`'s wording. See [conflict 6](#conflicts-and-resolutions) for the bootstrap
  default.

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
address with them.
Otherwise a duplicate invite made before they joined would let a removed person back in, with the
role it names, within its 7 days. An invite to another team stays: that team decided it.

### Consents

Consents stay per person, exactly as today (`users.*_consent_at` and `*_version`):

| Use | Whose consent | And |
|---|---|---|
| Sync (storing the index) | the mailbox's **linker** (`owner_user_id`), as `store/eligibility.go` reads it today: unchanged | an operator mailbox syncs when the operator switches it on, as today |
| Actions | the **actor**'s, at the current revision | the actor's `act` flag |
| Sending | the **sender**'s, at the current revision | the sender's `send` flag |
| `\Answered` after a reply | the sender's actions consent | the sender's `act` flag |

- The From name is the **sender's** profile name (`fromName` reads the caller, not the linker), so
  an account's `send.from_name` is per caller. An instance key sends under the address alone.
- Withdrawing the sync consent deletes, in the same transaction, the index of every mailbox the
  person **linked**, in every workspace, including team mailboxes other members read. The console
  has to say so before the person confirms; [taking over](#taking-over-a-link) a link first is how
  a team keeps it.
- Disabling a person on the instance stops the sync of every mailbox they linked (the eligibility
  rule requires the linker to be active); the index stays until they are enabled again or deleted.

## Who may do what

### Using a mailbox

| Operation | A person (session, or their key within its scope) | An instance key |
|---|---|---|
| list it, read its card | any grant | operator mailbox |
| folders, search, read, originals, attachments, events, storage | `read` | operator mailbox, `read` scope |
| ask for a sync pass | `read`, `write` scope | operator mailbox, `write` scope |
| switch sync on or off | never (their sync consent decides) | operator mailbox, unrestricted admin key |
| act | `act`, actions consent, `write` scope | operator mailbox, `write` scope |
| send, read own send records | `send`, send consent, a session (personal keys never have `send`) | operator mailbox, `send` scope |
| re-authorize, remove | `manage`, a session | operator mailbox, `admin` scope |
| restrict a new key to it | `read` | operator mailbox |

A person's key reaches only what that person can, at the moment of each request: the visibility
rule runs in SQL on every call, so **a key never reaches a mailbox its person lost access to**. A
key restricted to mailboxes reaches only those, as today.

### Administering a workspace

| | Team `owner` | Team `admin` | Team `member` | Operator (unrestricted instance admin key) |
|---|---|---|---|---|
| list my workspaces | yes | yes | yes | sees every workspace |
| rename the team | yes | yes | no | yes |
| list members | yes | yes | yes | yes |
| invite | any role | `member` only | no | any role |
| list and revoke pending invites | yes | yes, `member` invites | no | yes |
| change a role or status | anyone's | members' only, never to `admin` or `owner` | no | anyone's |
| remove a member | anyone | members only | only themselves (leave) | anyone |
| link a mailbox | yes | yes | no | no (operator mailboxes only) |
| see the access directory | every mailbox | every mailbox | mailboxes they `manage` | every mailbox |
| grant | within the grant rules | within the grant rules | if they hold `manage` on it, within the grant rules | `manage` only |
| revoke | yes | yes | if they hold `manage` on it; their own flags always | yes |

Any person may create a team, with a session, and becomes its owner; the operator creates one for
a named existing person. Personal workspaces have no administration: no invites, no other members,
no renaming. The
operator workspace has no members, grants or invites. Every row is subject to the
[protections](#protections). A person's key never administers anything: these routes take a
session or the operator's key.

### Who may change a grant

1. **Who.** An owner or admin of the mailbox's workspace, or a member holding `manage` on that
   mailbox. The operator may revoke, and grant `manage`.
2. **What.** `read`, `act` and `send` can only be granted by someone who holds that flag on the
   mailbox themselves; `manage` by an owner, an admin or a holder of `manage`. Access to mail passes
   only from someone who already has it: this is Mailie's form of Wappie's rule that read access
   needs a key grant from a client holding the key, and it is what makes "owners and admins get no
   automatic read" true rather than one click away ([conflict 2](#conflicts-and-resolutions)).
   Losing `manage` — the flag revoked, or the person's place in the workspace — ends, in the same
   transaction, the consent attempts they started on that mailbox (`oauth_pending`). Storing what a
   consent produced checks again, in its own transaction, that its starter still manages the
   mailbox, or for an attempt an instance key started that the mailbox is the operator's
   (`workspace.ManagesTx`, installed by the service with `account.Registry.CheckFlowsWith`): a
   loopback listener or a device poll the daemon finishes by itself, which no request re-checks,
   stores nothing for someone who stopped managing the mailbox while the person was signing in.
3. **To whom.** An active member of the mailbox's workspace. Nobody else, and nobody in a personal
   or the operator workspace but the person and the operator respectively.
4. **Revoking** is any of the above, or the person dropping their own flags.
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
(`last_owner`, `links`, `linked_by`) so a console can explain before anyone tries, as Wappie's
directory does; the mutation enforces them regardless.

| | Refused | The way out |
|---|---|---|
| **Last owner** | demoting, disabling or removing the last active owner of a team, or that owner leaving | promote another member first |
| **Linker** | removing or disabling the linker of a mailbox still linked in that workspace, or changing any flag of the linker's grant on it | the linker removes the mailbox, or another member [takes the link over](#taking-over-a-link) |
| **Last manager** | the last holder of `manage` on a linked mailbox losing it | always satisfied while the linker rule holds (the linker holds `manage`); kept as its own check |
| **Personal** | any change to a personal workspace's membership; an invite into one | |
| **Operator** | a membership, grant or invite in the operator workspace | |

On the instance, `user disable` and `user delete` also refuse, unless `force`, a person who is the
last active owner of a team with other active members, or the linker of a team mailbox that another
active member can read; the instance's own last-owner rule stays as it is. With `force`, a team can
be left without an owner (the operator assigns one with `mailserver member role`), and deleting a
linker removes every mailbox they linked, everywhere, with its index, because the consent it synced
under goes with them. A team whose only member is deleted is deleted with them, together with the
mailboxes in it, which they linked.

### Taking over a link

`POST /v1/accounts/{id}/take-over` (a session) makes the caller the mailbox's linker. The caller
must hold all four flags on it, be allowed to link into its workspace (owner or admin of a team),
and have agreed to sync; otherwise `409`. In one transaction `owner_user_id` becomes the caller;
the previous linker keeps their grant as an ordinary one, no longer protected. The index is kept:
from that moment it is stored under the new linker's consent, which already covers it. This is the
step that lets a team keep a mailbox when the person who linked it leaves (see
[conflict 1](#conflicts-and-resolutions)).

## Errors

The seven codes do not change.

| Situation | Code |
|---|---|
| a mailbox with no grant for the caller, another workspace's mailbox, a workspace the caller is not an active member of | `404 not_found`, never `403` |
| a mailbox the caller sees, without the flag the operation needs; a role that may not do this; a person's key on an administration route | `403 not_authorized` |
| a protection; a missing consent; a platform-sourced workspace changed locally | `409 conflict` |
| a personal or the operator workspace as the target of a member, grant or invite operation; an `act` without `read` | `400 bad_request` |

## Keys, tools and the operator

- **Instance keys** (no person) reach only the operator workspace's mailboxes, over REST and MCP
  alike. Today an instance key over REST sees every mailbox and can read a person's index; that ends
  ([conflict 3](#conflicts-and-resolutions)). A restriction to accounts keeps working, and may name
  only operator mailboxes, through the route and through `apikey create --bootstrap` alike.
- **Person keys** reach what their person can, live, within their scope (`read` or `write`) and
  their optional restriction. Losing `read` on a mailbox also deletes it from the restrictions of
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
- The console keeps the current workspace itself (per tab, remembered in local storage) and passes
  `?workspace=` so a view never fetches another workspace's data. This goes into `console.md` with
  the implementation.

## Events

The event gate (`internal/service/events.go`) today remembers per account whether the caller may
see it, assuming that never changes. With grants it does:

- The service keeps an **access epoch**, a counter it advances after every commit that can give or
  take `read` from someone: a grant set or revoked, a membership disabled or removed, a mailbox
  removed or taken over, a person disabled or deleted. One daemon writes the database (the lock),
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
| `GET /v1/workspaces/{id}/members` | member; operator | `[Member]` |
| `PATCH /v1/workspaces/{id}/members/{user_id}` | see the table; operator | `{role?, status?}` → `Member` |
| `DELETE /v1/workspaces/{id}/members/{user_id}` | see the table; the member themselves; operator | `204` |
| `GET /v1/workspaces/{id}/invites` | owner or admin; operator | `[Invite]`, pending only |
| `POST /v1/workspaces/{id}/invites` | owner or admin; operator | `{email, role}` → `Invite` with its `url` (201) |
| `DELETE /v1/workspaces/{id}/invites/{invite_id}` | owner or admin; operator | `204`; the unused invite is deleted |
| `POST /v1/auth/invites/accept` | session | `{invite}` → `Workspace`: joins with the invite's role |
| `GET /v1/workspaces/{id}/access` | owner or admin: every mailbox; a `manage` holder: theirs; operator | `[MailboxAccess]` |
| `PUT /v1/accounts/{id}/access/{user_id}` | see [the grant rules](#who-may-change-a-grant); session; operator for `manage` | `{read, act, send, manage}`, all four required, not all false → `Grant` |
| `DELETE /v1/accounts/{id}/access/{user_id}?flags=` | see the grant rules; the person themselves; operator | `204`; `flags` (comma-separated `read`, `act`, `send`, `manage`) names what goes, every flag without it |
| `POST /v1/accounts/{id}/take-over` | session | → `Account`; see [Taking over a link](#taking-over-a-link) |

Changed routes:

| Route | Change |
|---|---|
| `POST /v1/accounts` | optional `workspace_id`; who may link where, as [above](#mailboxes) |
| `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events`, `GET /v1/events/wait` | optional `?workspace=` |
| `DELETE /v1/accounts/{id}`, `POST /v1/accounts/{id}/oauth/start`, `POST /v1/accounts/oauth/callback` | `manage` (callback: the flow is still only its starter's) |
| `PUT /v1/accounts/{id}/sync` | unchanged: operator mailboxes, the operator |
| `POST /v1/auth/signup` | redeems an instance invite, or a team invite the operator or an instance owner made; the first sign-up is no owner unless its invite says so |
| `POST /v1/users/disable`, `POST /v1/users/delete` | also an instance owner's session; the team protections |
| `GET /v1/sends/{key}?account=` | only the caller's own sends ([conflict 10](#conflicts-and-resolutions)) |

Shapes:

```jsonc
// Workspace
{"id": "wsp_0a1b2c3d4e5f6a7b", "kind": "team", "source": "local", "name": "Support",
 "role": "admin", "status": "active", "created_at": 1790000000}
// the operator's listing has no role or status, and adds "members" and "mailboxes" counts

// Member ("person_disabled": true only for a person switched off on the instance)
{"user_id": "usr_…", "email": "bea@example.org", "name": "Bea Lima", "role": "member",
 "status": "active", "last_owner": false, "links": 1, "joined_at": 1790000000}

// MailboxAccess, whose grants are each a Grant
{"account_id": "acc_…", "email": "support@example.org", "provider": "gmail", "state": "active",
 "linked_by": "usr_…",
 "grants": [{"account_id": "acc_…", "user_id": "usr_…", "read": true, "act": true, "send": true,
             "manage": true, "granted_by": "migration", "updated_at": 1790000000}]}

// TeamInvite (role is the role in the team; url only in the answer that creates it). An instance
// invite keeps the shape POST /v1/users/invites always answered: {email, role, url, expires_at}.
{"id": "inv_…", "email": "bea@example.org", "workspace_id": "wsp_…", "role": "member",
 "url": "https://…/#invite=…&email=…", "created_by": "usr_…", "created_at": 1790000000,
 "expires_at": 1790604800}
```

`Account` gains:

```jsonc
{
  "workspace_id": "wsp_…",
  "linked_by": "usr_…",                     // absent for an operator mailbox
  "access": {"read": true, "act": true, "send": true, "manage": true},
  "send": {"available": false, "reason": "not_granted"}
}
```

`access` is the caller's grant; for an instance key, what its scope allows on an operator mailbox.
`send.reason` `not_owner` becomes `not_granted` (no `send` flag). The contract fixtures change only
through `go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`, and the
console's types and `web/test/contract.spec.ts` adapt to the new fields; the console's screens are
step 3.

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

- `access grant` grants `manage` only: `read`, `act` and `send` pass from a holder, in the console
  ([conflict 7](#conflicts-and-resolutions)).
- `account add|list|authorize|folders|remove|sync` and `apikey` keep working, on the operator
  workspace. `account list` no longer lists people's mailboxes.
- `user invite --bootstrap` takes `--workspace` too. Without `--role`, `--bootstrap` makes an
  `owner` invite while the instance has no active owner and no live owner invite, and a `member`
  invite otherwise ([conflict 6](#conflicts-and-resolutions)).
- `user disable|delete` take `--force` for the protections as for the last instance owner; without
  it the daemon's refusal, which the command prints, names the teams and mailboxes it is about.
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

## Going back

From this release on the runner refuses a database whose `user_version` is past the last migration
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
  `TestOwnersAndAdminsGetNoAutomaticRead`; `TestAnAdminCannotGrantReadTheyDoNotHold`;
  `TestAGrantOnlyGoesToAnActiveMemberOfTheMailboxesWorkspace`;
  `TestAnotherWorkspacesMailboxIsNotFoundNeverForbidden`.
- Links: `TestTheSameAddressInTwoWorkspacesIsTwoIndependentMailboxes`;
  `TestAnAddressIsLinkedOnceInAWorkspace`; `TestTheLinkerCannotBeRemovedWhileTheirMailboxIsLinked`;
  `TestATakeOverKeepsTheIndexAndMovesTheConsent`;
  `TestWithdrawingSyncDeletesTheIndexOfEveryMailboxThePersonLinked`.
- Members: `TestTheLastOwnerCannotLeaveOrBeDemoted`;
  `TestRemovingAMemberRemovesTheirGrantsAndStopsTheirEventStream`;
  `TestDisablingAMemberRemovesTheirGrantsAndEnablingDoesNotRestoreThem`;
  `TestAnAdminCannotChangeAnotherAdmin`.
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
  `TestADatabaseANewerBinaryMigratedIsRefused` the guard of [Going back](#going-back).

## Conflicts and resolutions

1. **The linker's consent outlives the linker's membership.** The plan lets a linker be removed once
   someone else holds `manage`, but keeps the mailbox syncing under the linker's consent: a person
   who left would answer for a team's index they cannot see, and their withdrawal would delete it.
   *Resolution:* the linker is protected while the link stands, and a member with all four flags
   who may link there **takes the link over** (`POST /v1/accounts/{id}/take-over`, one small
   transaction), after which the old linker is an ordinary member. The plan's "last holder of
   `manage`" rule stays, and is implied. Alternative, if the take-over is unwanted: the only way out
   is removing the mailbox.
2. **"Owners and admins grant" against "owners and admins get no automatic read".** If an admin
   could grant any flag to anyone, they could grant themselves `read`, and "no automatic read" would
   be one click. Wappie's barrier is cryptographic (a key grant from a holder); Mailie has no keys.
   *Resolution:* `read`, `act` and `send` pass only from someone who holds them; owners and admins
   grant `manage`, revoke anything, and grant the rest only when they hold it. A member holding
   `manage` on a mailbox may grant on it too, or a mailbox linked by a member could never be shared.
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
7. **`access grant` from the command line.** The operator holds no flag on anyone's mailbox, so under
   conflict 2's rule it cannot grant `read`, `act` or `send`; letting it would hand the operator
   people's mail by proxy, which the plan's own instance-key rule forbids. *Resolution:* the command
   grants `manage` only, and revokes anything.
8. **Withdrawing sync consent reaches team mailboxes.** A direct consequence of syncing under the
   linker's consent. *Resolution:* keep the consent rule; the console warns and names the team
   mailboxes before the person confirms (step 3); a take-over first keeps the team's index.
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
    which mailboxes exist in their team to grant and revoke. *Resolution:* the account routes keep
    the rule, and the access directory (`GET /v1/workspaces/{id}/access`) shows owners, admins and
    managers the addresses and grants, never the index, as Wappie's device directory does.
12. **The column name.** The plan keeps `owner_user_id` to avoid a rebuild, but `accounts` is
    rebuilt anyway, so renaming it to `linked_by` would cost no extra migration. *Resolution:* keep
    the name as the plan says, to keep the eligibility SQL, the earlier migrations' tests and
    `oauth_pending.owner_user_id` reading alike; documents and the API say "linked by".
13. **`act` without `read`** is a grant that can do nothing (an action names messages the caller
    reads). *Resolution:* `CHECK (act = 0 OR read = 1)`. `send` without `read` stays meaningful (a
    send-only member) and allowed.
14. **The consent texts.** The open console's sync text says what is stored for "your" mailboxes;
    with grants, other members read the index of a team mailbox. Whether the text, and so
    `MAIL_CONSENT_VERSION_SYNC`, needs a new revision when the console offers teams is the owner's
    call, in step 3.

## Not in this phase

- Deleting a team workspace; moving a mailbox between workspaces (link it again instead).
- A screen for any of this: the console is step 3. The platform source beyond its stub is a later
  step.
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
| `web/` | the types and contract spec for the changed fixtures only |
